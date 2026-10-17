package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/chapterdetect"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
)

// chapter_detecting (Phase 1d): a single file with no chapter markers was
// transcribed in duration windows (audio.StyleWindows). This stage finds the
// chapters in the narration (internal/chapterdetect), then re-maps the book onto
// them, so every later stage sees an ordinary chaptered book and never learns it
// had windows:
//
//   - the window layer is moved aside into windows/ (its manifest, its raw
//     transcripts, the detection), the window FLACs deleted;
//   - manifest.json becomes the DETECTED manifest (StyleMarkers, Detected);
//   - transcripts-raw/ gets one synthesized raw per detected chapter, re-timed
//     from the windows' words (frozen 0444 like a real one), and asr.json the new
//     manifest's fingerprint, so a later asr finds every chapter complete;
//   - chapters/ is split again on the detected chapters;
//   - chapters_detected.json is the player's chapter list (DetectedChapters).
//
// A detection that isn't confident parks chapters_not_detected with everything in
// place, so a Retry runs it again. Once the layer is moved it is the source of
// truth: every step after that is redone safely after a crash, from windows/.

// Names in the book's work dir.
const (
	detectedChaptersName = "chapters_detected.json"
	windowsDetectionName = "detection.json"
)

// silenceDetectFloor / silenceDetectPerAudioSec bound the silence pass over one
// window's FLAC: a local 16 kHz mono file decodes hundreds of times faster than
// real time, so a bound hit is a stall, not a slow machine.
var (
	silenceDetectFloor       = 2 * time.Minute
	silenceDetectPerAudioSec = 0.1
)

// ChaptersNotDetectedPrefix starts the park reason of a detection that isn't
// confident; chapterdetect's own reason follows.
const ChaptersNotDetectedPrefix = "couldn't find the chapters in the narration with confidence"

// DetectedChaptersPath is the book's detected chapter list (chapters_detected.json),
// written once chapter_detecting has found the chapters.
func DetectedChaptersPath(workDir string) string { return filepath.Join(workDir, detectedChaptersName) }

// DetectedChapters is chapters_detected.json: the chapters a player shows for a
// book whose chapters were found in its narration, on the whole-book timeline, in
// milliseconds, contiguous from 0 to the end. Each entry is one LOGICAL chapter
// position (what the server and the player gate spoilers by: entry k is position
// k), so a front-matter section shares the first story chapter's entry and an
// unnumbered interior section the entry of the chapter it precedes (Parts names the
// detected chapters an entry groups).
type DetectedChapters struct {
	// ManifestSHA is the fingerprint of the detected manifest the list was made
	// from: a list for another manifest (the book was inspected again) is stale.
	ManifestSHA string `json:"manifest_sha"`
	// SourceSize is the audio file's size then: a replaced file makes it stale too.
	SourceSize int64             `json:"source_size"`
	DurationMs int64             `json:"duration_ms"`
	Chapters   []DetectedChapter `json:"chapters"`
}

// DetectedChapter is one entry of DetectedChapters.
type DetectedChapter struct {
	Title    string `json:"title"`
	StartMs  int64  `json:"start_ms"`
	LengthMs int64  `json:"length_ms"`
	Parts    []int  `json:"parts"`
}

// ReadDetectedChapters reads a book's chapters_detected.json.
func ReadDetectedChapters(workDir string) (DetectedChapters, error) {
	var d DetectedChapters
	raw, err := os.ReadFile(DetectedChaptersPath(workDir)) //nolint:gosec // the book's own work dir
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return DetectedChapters{}, fmt.Errorf("parse %s: %w", detectedChaptersName, err)
	}
	return d, nil
}

// CurrentFor reports whether d still describes the book in workDir: its manifest is
// the one d was made from, and its audio file (when it can be looked at) has the
// same size.
func (d DetectedChapters) CurrentFor(workDir string) bool {
	fp, err := manifestFingerprint(workDir)
	if err != nil || fp != d.ManifestSHA {
		return false
	}
	m, err := audio.ReadManifest(workDir)
	if err != nil {
		return false
	}
	if info, err := os.Stat(m.Source); err == nil && info.Size() != d.SourceSize {
		return false
	}
	return true
}

// windowLayer is where a book's windows are: in place (manifest.json and
// transcripts-raw/), or moved aside into windows/.
type windowLayer struct {
	manifest audio.Manifest
	rawDir   string
	moved    bool
}

// readWindowLayer finds the window layer. Once windows/transcripts-raw/ exists the
// move happened (windows/manifest.json and the detection were written before it);
// before that, manifest.json must be the windows manifest.
func readWindowLayer(workDir string) (windowLayer, error) {
	wdir := filepath.Join(workDir, audio.WindowsDir)
	if info, err := os.Stat(filepath.Join(wdir, transcript.RawDir)); err == nil && info.IsDir() {
		m, err := audio.ReadManifest(wdir)
		if err != nil {
			return windowLayer{}, fmt.Errorf("read the moved windows manifest: %w", err)
		}
		return windowLayer{manifest: m, rawDir: filepath.Join(wdir, transcript.RawDir), moved: true}, nil
	}
	m, err := audio.ReadManifest(workDir)
	if err != nil {
		return windowLayer{}, fmt.Errorf("read manifest (inspect must run first): %w", err)
	}
	if m.Style != audio.StyleWindows {
		return windowLayer{}, fmt.Errorf("manifest is %q, not a windows manifest: only a book with no chapter markers is transcribed in windows", m.Style)
	}
	return windowLayer{manifest: m, rawDir: filepath.Join(workDir, transcript.RawDir)}, nil
}

// windowDetection is windows/detection.json: the detection a move was made from,
// kept so a crash after the window FLACs went (they give the silences) never needs
// them again.
type windowDetection struct {
	WindowsSHA string               `json:"windows_sha"`
	Result     chapterdetect.Result `json:"result"`
}

// windowsSHA fingerprints a windows manifest as marshalled.
func windowsSHA(m audio.Manifest) string {
	raw, _ := json.Marshal(m)
	return hexSHA256(raw)
}

// chapterDetect is the chapter_detecting stage.
func (e *Executor) chapterDetect(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	ffmpeg, _ := e.ensureTools()
	if ffmpeg == "" {
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkMediaToolsUnavailable, MediaToolsUnavailableMsg)
	}
	start := time.Now()
	layer, err := readWindowLayer(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: %w", err)
	}
	tl, err := loadWindowTimeline(layer.manifest.Chapters, func(w audio.Chapter) ([]byte, error) {
		return os.ReadFile(filepath.Join(layer.rawDir, transcript.RawName(w.Chapter))) //nolint:gosec // the book's work dir
	})
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: %w", err)
	}
	if r.Progress != nil {
		r.Progress(0, 2)
	}
	var det chapterdetect.Result
	err = runWithStageHeartbeat(ctx, r, func(elapsed time.Duration) string {
		return fmt.Sprintf("chapter_detecting: still listening for the chapters (%s elapsed)", humanDuration(elapsed))
	}, func() error {
		var derr error
		det, derr = e.detection(ctx, book.WorkDir, layer, tl, ffmpeg)
		return derr
	})
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: %w", err)
	}
	if !det.Confident {
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkChaptersNotDetected, ChaptersNotDetectedPrefix+": "+det.Reason)
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("found %s in the narration (%d headings heard)", countNoun(len(det.Chapters), "chapter"), det.Headings))
	}
	if err := moveWindowLayer(book.WorkDir, &layer); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: move the windows aside: %w", err)
	}
	manifest := detectedManifest(layer.manifest, det.Chapters)
	if err := e.remapTranscripts(book.WorkDir, manifest, tl); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: %w", err)
	}
	if e.db != nil {
		_ = e.db.SetBookChapters(context.WithoutCancel(ctx), book.ID, manifest.ChapterCount)
	}
	if r.Progress != nil {
		r.Progress(1, 2)
	}
	err = runWithStageHeartbeat(ctx, r, func(elapsed time.Duration) string {
		return fmt.Sprintf("chapter_detecting: ffmpeg still splitting the chapters (%s elapsed)", humanDuration(elapsed))
	}, func() error {
		_, serr := splitWithRetries(ctx, manifest, book.WorkDir, ffmpeg, r, nil)
		return serr
	})
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: split the chapters: %w", err)
	}
	list, err := writeDetectedChapters(book, manifest, tl)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: %w", err)
	}
	e.accountScratch(ctx, book)
	if r.Progress != nil {
		r.Progress(2, 2)
	}
	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"windows":       len(layer.manifest.Chapters),
			"chapter_count": manifest.ChapterCount,
			"headings":      det.Headings,
			"entries":       len(list.Chapters),
		}),
		RateSample: rateSample(1, time.Since(start).Seconds()),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.ChapterDetecting), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// detection is the book's detection: the one a move was made from when there is
// one for these windows (windows/detection.json), else a fresh one over the window
// FLACs' silences, which is kept there when confident.
func (e *Executor) detection(ctx context.Context, workDir string, layer windowLayer, tl windowTimeline, ffmpeg string) (chapterdetect.Result, error) {
	sha := windowsSHA(layer.manifest)
	var kept windowDetection
	if raw, err := os.ReadFile(filepath.Join(workDir, audio.WindowsDir, windowsDetectionName)); err == nil && //nolint:gosec // the book's work dir
		json.Unmarshal(raw, &kept) == nil && kept.WindowsSHA == sha && kept.Result.Confident {
		return kept.Result, nil
	}
	if layer.moved {
		return chapterdetect.Result{}, errors.New("the windows were moved aside without their detection; cancel the job and generate the book again")
	}
	var sil []chapterdetect.Silence
	for _, w := range layer.manifest.Chapters {
		flac := filepath.Join(workDir, audio.ChaptersDir, audio.ChapterFileName(w.Chapter))
		if !fsutil.IsFile(flac) {
			return chapterdetect.Result{}, fmt.Errorf("window %d FLAC missing (%s); split must run first", w.Chapter, flac)
		}
		bound := max(silenceDetectFloor, time.Duration(silenceDetectPerAudioSec*w.Duration*float64(time.Second)))
		got, err := audio.DetectSilences(ctx, ffmpeg, flac, 0, 0, bound)
		if err != nil {
			return chapterdetect.Result{}, fmt.Errorf("window %d silences: %w", w.Chapter, err)
		}
		for _, s := range got {
			sil = append(sil, chapterdetect.Silence{Start: w.Start + s.Start, End: w.Start + s.End})
		}
	}
	det := chapterdetect.Detect(tl.words(), sil, layer.manifest.Duration)
	if !det.Confident {
		return det, nil
	}
	out, err := json.MarshalIndent(windowDetection{WindowsSHA: sha, Result: det}, "", "  ")
	if err != nil {
		return chapterdetect.Result{}, err
	}
	wdir := filepath.Join(workDir, audio.WindowsDir)
	if err := os.MkdirAll(wdir, 0o750); err != nil {
		return chapterdetect.Result{}, err
	}
	// The windows manifest goes with it, before anything is moved: once the raws
	// are in windows/, readWindowLayer reads the manifest from there.
	if err := audio.WriteManifest(wdir, layer.manifest); err != nil {
		return chapterdetect.Result{}, err
	}
	return det, fsutil.WriteFileAtomic(filepath.Join(wdir, windowsDetectionName), append(out, '\n'), 0o644)
}

// moveWindowLayer moves the window layer aside (a no-op once moved): the window
// FLACs go (their names are the detected chapters' too, and split would take them
// for cut chapters), then transcripts-raw/ is renamed to windows/transcripts-raw/,
// the step after which the move counts as done.
func moveWindowLayer(workDir string, layer *windowLayer) error {
	if layer.moved {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(workDir, audio.ChaptersDir)); err != nil {
		return err
	}
	moved := filepath.Join(workDir, audio.WindowsDir, transcript.RawDir)
	if err := os.Rename(layer.rawDir, moved); err != nil {
		return err
	}
	layer.rawDir, layer.moved = moved, true
	return nil
}

// detectedManifest is the windows manifest re-cut on the detected chapters: a
// markers-style manifest of the same file, marked Detected.
func detectedManifest(windows audio.Manifest, chs []chapterdetect.Chapter) audio.Manifest {
	out := make([]audio.Chapter, len(chs))
	for i, c := range chs {
		out[i] = audio.Chapter{Chapter: i + 1, Title: c.Title, Start: c.Start, End: c.End, Duration: c.End - c.Start}
	}
	return audio.Manifest{Source: windows.Source, Title: windows.Title, Style: audio.StyleMarkers, Duration: windows.Duration,
		ChapterCount: len(out), Chapters: out, Detected: true}
}

// remapTranscripts writes the detected manifest, one synthesized raw transcript per
// detected chapter into transcripts-raw/ (a complete one already there is the same
// and is kept), and asr.json with the new manifest's fingerprint and its empty
// chapters, so the asr stage after a continue finds every chapter done.
func (e *Executor) remapTranscripts(workDir string, m audio.Manifest, tl windowTimeline) error {
	if err := audio.WriteManifest(workDir, m); err != nil {
		return fmt.Errorf("write the detected manifest: %w", err)
	}
	rawDir := filepath.Join(workDir, transcript.RawDir)
	if err := os.MkdirAll(rawDir, 0o750); err != nil {
		return err
	}
	var empty []int
	for i, doc := range tl.split(m.Chapters) {
		ch := m.Chapters[i].Chapter
		if len(doc.Segments) == 0 {
			empty = append(empty, ch)
		}
		path := filepath.Join(rawDir, transcript.RawName(ch))
		if rawComplete(path) {
			continue
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		_ = os.Remove(path)
		if err := fsutil.WriteFileAtomic(path, raw, 0o644); err != nil {
			return fmt.Errorf("write chapter %d transcript: %w", ch, err)
		}
		// Frozen like a real raw: durable evidence the later stages only read.
		if err := os.Chmod(path, 0o444); err != nil { //nolint:gosec // immutability guard on a non-secret artifact
			e.log.Warn("chapter_detecting: could not freeze a transcript", "path", path, "err", err)
		}
	}
	fp, err := manifestFingerprint(workDir)
	if err != nil {
		return err
	}
	prov := readASRProvenance(workDir)
	prov.ManifestSHA, prov.EmptyChapters = fp, empty
	return writeASRProvenance(workDir, prov)
}

// writeDetectedChapters writes chapters_detected.json for the detected manifest,
// grouped by logical position (see groupByPosition).
func writeDetectedChapters(book store.Book, m audio.Manifest, tl windowTimeline) (DetectedChapters, error) {
	fp, err := manifestFingerprint(book.WorkDir)
	if err != nil {
		return DetectedChapters{}, err
	}
	d := DetectedChapters{ManifestSHA: fp, DurationMs: ms(m.Duration), Chapters: groupByPosition(m, tl)}
	if info, err := os.Stat(m.Source); err == nil {
		d.SourceSize = info.Size()
	}
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return DetectedChapters{}, err
	}
	return d, fsutil.WriteFileAtomic(DetectedChaptersPath(book.WorkDir), append(out, '\n'), 0o644)
}

// groupByPosition is the player's list: the detected chapters grouped by the
// LOGICAL position classifyEdgeChapters gives them (the classification the later
// stages make from the same transcripts, computed here from the synthesized ones in
// memory), so that entry k IS logical position k (the server and the player gate
// spoilers by the list's ordinal). Front matter and excluded leading files join the
// first story chapter; an interior unnumbered section joins the chapter whose
// number it takes; excluded trailing files (credits) join the entry before them;
// end matter past the last numbered chapter stays separate, after it. A merged
// entry's titles are joined with " / ".
func groupByPosition(m audio.Manifest, tl windowTimeline) []DetectedChapter {
	docs := tl.split(m.Chapters)
	n := len(m.Chapters)
	edge := make([]edgeChapter, n)
	for i, ch := range m.Chapters {
		text := strings.TrimSpace(docs[i].Text)
		edge[i] = edgeChapter{Chapter: ch.Chapter, Opening: clipBytes(text, chapterOpeningBytes)}
		// classifyBookEdges' probe window, so the two classify alike.
		if n <= 2*edgeProbeDepth || i < edgeProbeDepth || i >= n-edgeProbeDepth {
			edge[i].Words, edge[i].HasTranscript, edge[i].Probed, edge[i].DurationSec = len(strings.Fields(text)), true, true, ch.Duration
		}
	}
	class := classifyEdgeChapters(edge)
	set := func(chs []int) map[int]bool {
		out := make(map[int]bool, len(chs))
		for _, c := range chs {
			out[c] = true
		}
		return out
	}
	leading, trailing, endMatter := set(class.ExcludedLeading), set(class.ExcludedTrailing), set(class.EndMatter)
	var out []DetectedChapter
	lastPos := 0
	for _, ch := range m.Chapters {
		pos, ok := class.PositionOf(ch.Chapter)
		join := false
		switch {
		case len(out) == 0:
		case leading[ch.Chapter], trailing[ch.Chapter]:
			join = true // an intro joins the first story chapter, credits the entry before them
		case endMatter[ch.Chapter] || !ok:
		case pos <= max(lastPos, 1):
			join = true // front matter (0, or a leading exclusion's), or a section sharing its chapter's number
		}
		if ok && pos > lastPos && !endMatter[ch.Chapter] {
			lastPos = pos
		}
		if join {
			e := &out[len(out)-1]
			if t := strings.TrimSpace(ch.Title); t != "" {
				e.Title = joinTitle(e.Title, t)
			}
			e.Parts = append(e.Parts, ch.Chapter)
			continue
		}
		out = append(out, DetectedChapter{Title: strings.TrimSpace(ch.Title), StartMs: ms(ch.Start), Parts: []int{ch.Chapter}})
	}
	total := ms(m.Duration)
	for i := range out {
		end := total
		if i+1 < len(out) {
			end = out[i+1].StartMs
		}
		out[i].LengthMs = end - out[i].StartMs
	}
	return out
}

// joinTitle joins a merged entry's titles ("Prologue / Chapter 1"), bounded like a
// detected title.
func joinTitle(a, b string) string {
	if a == "" {
		return b
	}
	t := a + " / " + b
	if len(t) > 200 {
		return a
	}
	return t
}

// ms is seconds as whole milliseconds.
func ms(sec float64) int64 { return int64(math.Round(sec * 1000)) }

// clipBytes is s cut to at most n bytes (at a byte, as chapterOpening reads them).
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// --- the windows' words on the whole-book timeline ---

// timedWord is one word on the whole-book timeline, with whisper's probability.
type timedWord struct {
	text       string
	start, end float64
	p          *float64
}

// timedSegment is one transcript segment on the whole-book timeline.
type timedSegment struct {
	start, end float64
	text       string
	words      []timedWord
}

// windowTimeline is the book's transcribed segments on the whole-book timeline, in
// window order, and the language the windows were heard in.
type windowTimeline struct {
	segments []timedSegment
	language string
}

// loadWindowTimeline reads each window's raw transcript (read) and puts its
// segments on the whole-book timeline (the window's start + whisper's times).
// whisper.cpp's sub-word tokens are glued into words (a token not starting with a
// space continues the word before it; mlx-whisper's words all start with one), and a
// segment without words contributes its text as one word spanning it.
func loadWindowTimeline(windows []audio.Chapter, read func(audio.Chapter) ([]byte, error)) (windowTimeline, error) {
	var tl windowTimeline
	for _, w := range windows {
		raw, err := read(w)
		if err != nil {
			return windowTimeline{}, fmt.Errorf("read window %d transcript (asr must run first): %w", w.Chapter, err)
		}
		tr, err := transcript.Normalize(raw, transcript.Meta{Chapter: w.Chapter})
		if err != nil {
			return windowTimeline{}, fmt.Errorf("window %d transcript: %w", w.Chapter, err)
		}
		if tl.language == "" {
			tl.language = tr.Language
		}
		for _, s := range tr.Segments {
			seg := timedSegment{start: w.Start + s.Start, end: w.Start + s.End, text: s.Text}
			for _, wd := range s.Words {
				if n := len(seg.words); n > 0 && !strings.HasPrefix(wd.W, " ") {
					seg.words[n-1].text += wd.W
					seg.words[n-1].end = w.Start + wd.End
					continue
				}
				seg.words = append(seg.words, timedWord{text: wd.W, start: w.Start + wd.Start, end: w.Start + wd.End, p: wd.P})
			}
			if len(seg.words) == 0 && strings.TrimSpace(s.Text) != "" {
				seg.words = []timedWord{{text: s.Text, start: seg.start, end: seg.end}}
			}
			tl.segments = append(tl.segments, seg)
		}
	}
	return tl, nil
}

// words is every word in time order, for chapterdetect.
func (tl windowTimeline) words() []chapterdetect.Word {
	var out []chapterdetect.Word
	for _, s := range tl.segments {
		for _, w := range s.words {
			out = append(out, chapterdetect.Word{Text: w.text, Start: w.start, End: w.end})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// owRaw is a synthesized raw transcript in the openai-whisper shape the transcript
// adapters read (what mlx-whisper writes).
type owRaw struct {
	Text     string      `json:"text"`
	Language string      `json:"language"`
	Segments []owRawSegm `json:"segments"`
}

type owRawSegm struct {
	ID    int         `json:"id"`
	Start float64     `json:"start"`
	End   float64     `json:"end"`
	Text  string      `json:"text"`
	Words []owRawWord `json:"words"`
}

type owRawWord struct {
	Word        string   `json:"word"`
	Start       float64  `json:"start"`
	End         float64  `json:"end"`
	Probability *float64 `json:"probability"`
}

// split re-maps the timeline onto chs: one raw document per chapter, times
// relative to its start. A segment goes to the chapter that holds its first word,
// and a boundary inside a segment splits it at the word (each part keeps the words
// on its side, its text theirs); a segment with no words goes by its start.
func (tl windowTimeline) split(chs []audio.Chapter) []owRaw {
	docs := make([]owRaw, len(chs))
	for i := range docs {
		docs[i] = owRaw{Language: tl.language, Segments: []owRawSegm{}}
	}
	at := func(t float64) int {
		i := sort.Search(len(chs), func(i int) bool { return chs[i].Start > t }) - 1
		return max(i, 0)
	}
	add := func(c int, s owRawSegm) {
		s.ID = len(docs[c].Segments)
		docs[c].Segments = append(docs[c].Segments, s)
	}
	rel := func(c int, t float64) float64 { return math.Max(0, t-chs[c].Start) }
	for _, s := range tl.segments {
		if len(s.words) == 0 {
			c := at(s.start)
			add(c, owRawSegm{Start: rel(c, s.start), End: rel(c, s.end), Text: s.text, Words: []owRawWord{}})
			continue
		}
		// Runs of consecutive words in one chapter; one run is the whole segment.
		var runs [][]timedWord
		var runCh []int
		for _, w := range s.words {
			c := at(w.start)
			if n := len(runs); n > 0 && runCh[n-1] == c {
				runs[n-1] = append(runs[n-1], w)
				continue
			}
			runs, runCh = append(runs, []timedWord{w}), append(runCh, c)
		}
		for k, run := range runs {
			c := runCh[k]
			seg := owRawSegm{Text: s.text, Start: s.start, End: s.end}
			if len(runs) > 1 {
				parts := make([]string, len(run))
				for i, w := range run {
					parts[i] = strings.TrimSpace(w.text)
				}
				seg.Text = " " + strings.Join(parts, " ")
				seg.Start, seg.End = run[0].start, run[len(run)-1].end
				if k == 0 {
					seg.Start = math.Min(s.start, seg.Start)
				}
				if k == len(runs)-1 {
					seg.End = math.Max(s.end, seg.End)
				}
			}
			seg.Start, seg.End = rel(c, seg.Start), rel(c, seg.End)
			for _, w := range run {
				seg.Words = append(seg.Words, owRawWord{Word: w.text, Start: rel(c, w.start), End: rel(c, w.end), Probability: w.p})
			}
			add(c, seg)
		}
	}
	for i := range docs {
		var b strings.Builder
		for _, s := range docs[i].Segments {
			b.WriteString(s.Text)
		}
		docs[i].Text = b.String()
	}
	return docs
}
