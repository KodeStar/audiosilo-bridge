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
//     transcripts, the detection, and the window FLACs into windows/chapters/);
//   - manifest.json becomes the DETECTED manifest (StyleMarkers, Detected);
//   - transcripts-raw/ gets one synthesized raw per detected chapter, re-timed
//     from the windows' words (frozen 0444 like a real one), and asr.json the new
//     manifest's fingerprint, so a later asr finds every chapter complete;
//   - chapters/ is cut on the detected chapters from the window FLACs (the whole
//     book, decoded and local already; the source only for what they can't give),
//     and the window FLACs go;
//   - chapters_detected.json is the player's chapter list (DetectedChapters).
//
// A detection that isn't confident parks chapters_not_detected with everything in
// place (the silences heard kept beside the window FLACs), so a Retry runs it
// again without decoding. Once the layer is moved it is the source of truth: every
// step after that is redone safely after a crash, from windows/.

// Names in the book's work dir.
const (
	detectedChaptersName = "chapters_detected.json"
	windowsDetectionName = "detection.json"
	// detectedPositionsName is the classification chapters_detected.json was grouped
	// by (see detectedClassification). classifyBookEdges takes it for a detected
	// manifest instead of classifying again from the text layer the later stages
	// leave (QA can re-transcribe a chapter's opening, or shrink a looped one past the
	// edge rule), so the list's entry k and the sidecars' logical position k come from
	// one classification by construction. It quotes no transcript, so it stays with
	// the durables.
	detectedPositionsName = "positions_detected.json"
)

// silenceDetectFloor / silenceDetectPerAudioSec bound the silence pass over one
// window's FLAC (only when split didn't keep its silences): a local 16 kHz mono file
// decodes hundreds of times faster than real time, so a bound hit is a stall, not a
// slow machine.
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
// detected chapters an entry groups; rankPositions gives them its ordinal).
type DetectedChapters struct {
	// ManifestSHA is the fingerprint of the detected manifest the list was made
	// from: a list for another manifest (the book was inspected again) is stale.
	ManifestSHA string `json:"manifest_sha"`
	// SourceSize is the audio file's size then: a replaced file makes it stale too
	// (0 when it couldn't be looked at then: unknown, never compared).
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

// DetectedStamp identifies the book's chapters_detected.json and the manifest it is
// checked against as they are now (sizes and modification times, from local stats),
// so a caller that looks often reads and checks the list again only once either
// changed; ok is false while there is no list.
func DetectedStamp(workDir string) (string, bool) {
	list, err := os.Stat(DetectedChaptersPath(workDir))
	if err != nil {
		return "", false
	}
	stamp := fmt.Sprintf("%s|%d|%d", workDir, list.Size(), list.ModTime().UnixNano())
	if m, err := os.Stat(filepath.Join(workDir, audio.ManifestName)); err == nil {
		stamp += fmt.Sprintf("|%d|%d", m.Size(), m.ModTime().UnixNano())
	}
	return stamp, true
}

// CurrentFor reports whether d still describes the book in workDir: its manifest is
// the one d was made from, and its audio file (when it can be looked at, and was
// when d was written) has the same size.
func (d DetectedChapters) CurrentFor(workDir string) bool {
	fp, err := manifestFingerprint(workDir)
	if err != nil || fp != d.ManifestSHA {
		return false
	}
	m, err := audio.ReadManifest(workDir)
	if err != nil {
		return false
	}
	if info, err := os.Stat(m.Source); err == nil && d.SourceSize > 0 && info.Size() != d.SourceSize {
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
	// One unit, as every book stage (the ETA charges a unit its learned per-book
	// time): the cut after the detection says what it is doing on the heartbeat.
	if r.Progress != nil {
		r.Progress(0, 1)
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
	docs := tl.split(manifest.Chapters)
	fp, err := e.remapTranscripts(book.WorkDir, manifest, docs)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: %w", err)
	}
	if e.db != nil {
		_ = e.db.SetBookChapters(context.WithoutCancel(ctx), book.ID, manifest.ChapterCount)
	}
	err = runWithStageHeartbeat(ctx, r, func(elapsed time.Duration) string {
		return fmt.Sprintf("chapter_detecting: ffmpeg still cutting the chapters (%s elapsed)", humanDuration(elapsed))
	}, func() error {
		return cutDetectedChapters(ctx, book.WorkDir, manifest, layer.manifest, ffmpeg, r)
	})
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: cut the chapters: %w", err)
	}
	list, err := writeDetectedChapters(book, manifest, docs, headingNumbers(det.Chapters), fp)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("chapter_detecting: %w", err)
	}
	e.accountScratch(ctx, book)
	if r.Progress != nil {
		r.Progress(1, 1)
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
// one for these windows (windows/detection.json), else a fresh one over the
// windows' silences (audio.WindowSilences: split heard them, else the window FLACs
// are listened to once), which is kept there when confident.
func (e *Executor) detection(ctx context.Context, workDir string, layer windowLayer, tl windowTimeline, ffmpeg string) (chapterdetect.Result, error) {
	if kept, ok := keptDetection(workDir, layer.manifest); ok {
		return kept, nil
	}
	if layer.moved {
		return chapterdetect.Result{}, errors.New("the windows were moved aside without their detection; cancel the job and generate the book again")
	}
	var sil []audio.Silence
	for _, w := range layer.manifest.Chapters {
		bound := max(silenceDetectFloor, time.Duration(silenceDetectPerAudioSec*w.Duration*float64(time.Second)))
		got, err := audio.WindowSilences(ctx, ffmpeg, filepath.Join(workDir, audio.ChaptersDir), w, bound)
		if err != nil {
			return chapterdetect.Result{}, fmt.Errorf("window %d silences: %w", w.Chapter, err)
		}
		sil = append(sil, got...)
	}
	det := chapterdetect.Detect(tl.words(), sil, layer.manifest.Duration)
	if !det.Confident {
		return det, nil
	}
	out, err := json.MarshalIndent(windowDetection{WindowsSHA: windowsSHA(layer.manifest), Result: det}, "", "  ")
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

// keptDetection is the confident detection kept for the windows of m
// (windows/detection.json), ok false when there is none for them.
func keptDetection(workDir string, m audio.Manifest) (chapterdetect.Result, bool) {
	var kept windowDetection
	raw, err := os.ReadFile(filepath.Join(workDir, audio.WindowsDir, windowsDetectionName)) //nolint:gosec // the book's work dir
	if err != nil || json.Unmarshal(raw, &kept) != nil || kept.WindowsSHA != windowsSHA(m) || !kept.Result.Confident {
		return chapterdetect.Result{}, false
	}
	return kept.Result, true
}

// DetectionRerunnable reports whether a book stopped at chapter_detecting (parked,
// cancelled or failed there) can run the stage again from what it kept, its audio
// never transcribed again: the window layer, either in place with every window's
// silences to be had (kept, or its FLAC there to listen to) or moved aside with the
// confident detection the move was made from; every window's raw transcript
// complete; and the audio file the one inspected (the size probe.json states, which
// must be known). A generate job continues such a chapters-only book.
func DetectionRerunnable(workDir string) bool {
	layer, err := readWindowLayer(workDir)
	if err != nil || len(layer.manifest.Chapters) == 0 {
		return false
	}
	if layer.moved {
		if _, ok := keptDetection(workDir, layer.manifest); !ok {
			return false
		}
	}
	for _, w := range layer.manifest.Chapters {
		if !rawComplete(filepath.Join(layer.rawDir, transcript.RawName(w.Chapter))) {
			return false
		}
		if !layer.moved && !audio.WindowHeard(filepath.Join(workDir, audio.ChaptersDir), w) {
			return false
		}
	}
	size := audio.ProbedSize(workDir)
	info, err := os.Stat(layer.manifest.Source)
	return err == nil && size > 0 && info.Size() == size
}

// moveWindowLayer moves the window layer aside (a no-op once moved): chapters/ (the
// window FLACs, whose names are the detected chapters' too, so split would take them
// for cut chapters) becomes windows/chapters/, then transcripts-raw/ is renamed to
// windows/transcripts-raw/, the step after which the move counts as done.
func moveWindowLayer(workDir string, layer *windowLayer) error {
	if layer.moved {
		return nil
	}
	if chapters := filepath.Join(workDir, audio.ChaptersDir); isDir(chapters) {
		audioDir := filepath.Join(workDir, audio.WindowAudioDir)
		if err := os.RemoveAll(audioDir); err != nil { // a leftover: chapters/ is the newer
			return err
		}
		if err := os.Rename(chapters, audioDir); err != nil {
			return err
		}
	}
	moved := filepath.Join(workDir, audio.WindowsDir, transcript.RawDir)
	if err := os.Rename(layer.rawDir, moved); err != nil {
		return err
	}
	layer.rawDir, layer.moved = moved, true
	return nil
}

// cutDetectedChapters cuts the detected manifest's chapter FLACs from the window
// FLACs in windows/chapters/, splits any they can't give from the source (a crash
// after the window FLACs went), then lets the window FLACs go.
func cutDetectedChapters(ctx context.Context, workDir string, m, windows audio.Manifest, ffmpeg string, r scheduler.StageReport) error {
	audioDir := filepath.Join(workDir, audio.WindowAudioDir)
	left, err := audio.CutFromWindows(ctx, m, windows.Chapters, audioDir, workDir, ffmpeg)
	if err != nil {
		return err
	}
	if left > 0 {
		if r.Note != nil {
			r.Note(fmt.Sprintf("the window audio is gone: splitting %s from the source", countNoun(left, "chapter")))
		}
		if _, err := splitWithRetries(ctx, m, workDir, ffmpeg, r, nil); err != nil {
			return err
		}
	}
	return os.RemoveAll(audioDir)
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
// detected chapter (docs, tl.split's) into transcripts-raw/ (a complete one already
// there is the same and is kept, never encoded again), and asr.json with the new
// manifest's fingerprint and its empty chapters, so the asr stage after a continue
// finds every chapter done. It returns the fingerprint.
func (e *Executor) remapTranscripts(workDir string, m audio.Manifest, docs []transcript.Transcript) (string, error) {
	if err := audio.WriteManifest(workDir, m); err != nil {
		return "", fmt.Errorf("write the detected manifest: %w", err)
	}
	rawDir := filepath.Join(workDir, transcript.RawDir)
	if err := os.MkdirAll(rawDir, 0o750); err != nil {
		return "", err
	}
	var empty []int
	for i, doc := range docs {
		ch := m.Chapters[i].Chapter
		if len(doc.Segments) == 0 {
			empty = append(empty, ch)
		}
		path := filepath.Join(rawDir, transcript.RawName(ch))
		if rawComplete(path) {
			continue
		}
		raw, err := transcript.EncodeOpenAI(doc)
		if err != nil {
			return "", err
		}
		_ = os.Remove(path)
		if err := fsutil.WriteFileAtomic(path, raw, 0o644); err != nil {
			return "", fmt.Errorf("write chapter %d transcript: %w", ch, err)
		}
		// Frozen like a real raw: durable evidence the later stages only read.
		if err := os.Chmod(path, 0o444); err != nil { //nolint:gosec // immutability guard on a non-secret artifact
			e.log.Warn("chapter_detecting: could not freeze a transcript", "path", path, "err", err)
		}
	}
	fp, err := manifestFingerprint(workDir)
	if err != nil {
		return "", err
	}
	prov := readASRProvenance(workDir)
	prov.ManifestSHA, prov.EmptyChapters = fp, empty
	return fp, writeASRProvenance(workDir, prov)
}

// headingNumbers is the number each detected chapter's "chapter" heading says (0
// for none), in the detected manifest's order.
func headingNumbers(chs []chapterdetect.Chapter) []int {
	out := make([]int, len(chs))
	for i, c := range chs {
		out[i] = c.Number
	}
	return out
}

// writeDetectedChapters writes chapters_detected.json for the detected manifest
// (its fingerprint fp, its chapters' transcripts docs and heading numbers said),
// grouped by logical position (see detectedClassification), and, first, the
// classification it was grouped by (positions_detected.json), which the later
// stages take for the book.
func writeDetectedChapters(book store.Book, m audio.Manifest, docs []transcript.Transcript, said []int, fp string) (DetectedChapters, error) {
	class, list := detectedClassification(m, docs, said)
	pos, err := json.Marshal(detectedPositions{ManifestSHA: fp, Classification: class})
	if err != nil {
		return DetectedChapters{}, err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(book.WorkDir, detectedPositionsName), append(pos, '\n'), 0o644); err != nil {
		return DetectedChapters{}, err
	}
	d := DetectedChapters{ManifestSHA: fp, DurationMs: ms(m.Duration), Chapters: list}
	if info, err := os.Stat(m.Source); err == nil {
		d.SourceSize = info.Size()
	}
	out, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return DetectedChapters{}, err
	}
	return d, fsutil.WriteFileAtomic(DetectedChaptersPath(book.WorkDir), append(out, '\n'), 0o644)
}

// detectedPositions is positions_detected.json: a detected manifest's
// classification (see detectedPositionsName), with the manifest it was made from.
type detectedPositions struct {
	ManifestSHA    string             `json:"manifest_sha"`
	Classification edgeClassification `json:"classification"`
}

// loadDetectedPositions is the classification chapters_detected.json was grouped by,
// when it was made from the manifest fingerprinted fp.
func loadDetectedPositions(workDir, fp string) (edgeClassification, bool) {
	raw, err := os.ReadFile(filepath.Join(workDir, detectedPositionsName)) //nolint:gosec // the book's work dir
	if err != nil {
		return edgeClassification{}, false
	}
	var p detectedPositions
	if json.Unmarshal(raw, &p) != nil || p.ManifestSHA != fp {
		return edgeClassification{}, false
	}
	return p.Classification, true
}

// groupByPosition is the player's list for the detected manifest m (see
// detectedClassification).
func groupByPosition(m audio.Manifest, docs []transcript.Transcript, said []int) []DetectedChapter {
	_, list := detectedClassification(m, docs, said)
	return list
}

// detectedClassification is the detected manifest m's classification and the
// player's list grouped by it, from its chapters' synthesized transcripts in memory
// (docs) through the same edgeChapters probe-window rule classifyBookEdges uses.
// The list's entry k IS logical position k (the server and the player gate spoilers
// by the list's ordinal), and the classification is made so (rankPositions): the
// later stages take it for the book (positions_detected.json) rather than classify
// it again.
//
// said is the number each chapter's "chapter" heading says (0 for none; nil when
// not known). The classifier reads a chapter's number from its opening only when it
// is announced bare ("Chapter Four."), and a numbered chapter it can't read ("Chapter
// Four, The Storm.") would join the next one's entry as an unnumbered section; the
// detector read the heading whatever follows it, so that chapter opens with its
// number announced.
func detectedClassification(m audio.Manifest, docs []transcript.Transcript, said []int) (edgeClassification, []DetectedChapter) {
	text := make(map[int]string, len(m.Chapters))
	number := make(map[int]int, len(m.Chapters))
	for i, ch := range m.Chapters {
		text[ch.Chapter] = transcript.PlainText(docs[i])
		if i < len(said) {
			number[ch.Chapter] = said[i]
		}
	}
	edge, _ := edgeChapters(m.Chapters, func(ch audio.Chapter) (string, error) {
		if n := number[ch.Chapter]; n > 0 {
			return fmt.Sprintf("Chapter %d.", n), nil
		}
		return clipBytes(text[ch.Chapter], chapterOpeningBytes), nil
	}, func(ch audio.Chapter) (int, bool, error) {
		return len(strings.Fields(text[ch.Chapter])), true, nil
	})
	class := classifyEdgeChapters(edge)
	list := groupEntries(m, class)
	return rankPositions(class, list), list
}

// rankPositions makes class's positions the ordinals of the list grouped by it
// (groupEntries). The list opens an entry at each position above the last, so its
// ordinals count the positions; where the narration's numbering skips one (a heading
// not heard, an announcement that doesn't read as a number) entry k would otherwise
// hold position k+1 from there on, every later reveal and recap opening a chapter
// late and the last never. Front matter keeps position 0, and a file with no position
// (an excluded edge, end matter) none. A classification by file number (no narrated
// numbering) counts the list already and is returned as it is, as is one whose
// positions are the ordinals.
func rankPositions(class edgeClassification, list []DetectedChapter) edgeClassification {
	if len(class.Positions) == 0 {
		return class
	}
	positions := make(map[int]int, len(class.Positions))
	logical, changed := 0, false
	for k, e := range list {
		for _, ch := range e.Parts {
			pos, ok := class.Positions[ch]
			if !ok {
				continue
			}
			if pos > 0 {
				changed = changed || pos != k+1
				pos = k + 1
				logical = max(logical, pos)
			}
			positions[ch] = pos
		}
	}
	if !changed {
		return class
	}
	out := class
	out.Positions, out.LogicalCount = positions, logical
	for _, e := range list {
		found := false
		for _, ch := range e.Parts {
			if pos := positions[ch]; pos > 0 {
				out.ChapterOffset, found = ch-pos, true
				break
			}
		}
		if found {
			break
		}
	}
	out.EdgeNote = composeEdgeNote(out.LogicalCount, out.ExcludedLeading, out.ExcludedTrailing, out.FrontMatter, out.EndMatter)
	out.ChunkNote = composeChunkNote(out.ExcludedLeading, out.ExcludedTrailing)
	out.AssembleNote = composeAssembleNote(out)
	return out
}

// groupEntries is the player's list: the detected chapters grouped by the LOGICAL
// position class gives them. Front matter and excluded leading files join the first
// story chapter; an interior unnumbered section joins the chapter whose number it
// takes; excluded trailing files (credits) join the entry before them; end matter
// past the last numbered chapter stays separate, after it. A merged entry's titles
// are joined with " / ".
func groupEntries(m audio.Manifest, class edgeClassification) []DetectedChapter {
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
	if len(t) > chapterdetect.MaxTitleBytes {
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

// timedSegment is one transcript segment on the whole-book timeline. synthetic says
// its one word is its text, made for the detection (whisper gave it no words): the
// synthesized raw keeps it wordless, as whisper wrote it.
type timedSegment struct {
	start, end float64
	text       string
	words      []timedWord
	synthetic  bool
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
// space continues the word before it, its probability the lowest of them;
// mlx-whisper's words all start with one), and a segment without words contributes
// its text as one word spanning it. Nothing is timed past the window's own audio: a
// segment or word whisper put there (a hallucination after the window's last words)
// is dropped, and an end there is clamped, so a window's words never interleave with
// the next window's (a stray "!" between "Day" and "85." loses the heading).
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
		limit := w.Duration
		if limit <= 0 {
			limit = w.End - w.Start
		}
		within := func(t float64) bool { return limit <= 0 || t < limit }
		clamp := func(t float64) float64 {
			if limit > 0 && t > limit {
				return limit
			}
			return t
		}
		for _, s := range tr.Segments {
			if !within(s.Start) {
				continue
			}
			seg := timedSegment{start: w.Start + s.Start, end: w.Start + clamp(s.End), text: s.Text}
			for _, wd := range s.Words {
				if !within(wd.Start) {
					continue
				}
				if n := len(seg.words); n > 0 && !strings.HasPrefix(wd.W, " ") {
					last := &seg.words[n-1]
					last.text += wd.W
					last.end = w.Start + clamp(wd.End)
					if wd.P != nil && (last.p == nil || *wd.P < *last.p) {
						last.p = wd.P
					}
					continue
				}
				seg.words = append(seg.words, timedWord{text: wd.W, start: w.Start + wd.Start, end: w.Start + clamp(wd.End), p: wd.P})
			}
			if len(seg.words) == 0 && strings.TrimSpace(s.Text) != "" {
				seg.words, seg.synthetic = []timedWord{{text: s.Text, start: seg.start, end: seg.end}}, true
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

// split re-maps the timeline onto chs: one transcript per chapter (its raw is
// transcript.EncodeOpenAI's), times relative to its start. A segment goes to the
// chapter that holds its first word, and a boundary inside a segment splits it at
// the word (each part keeps the words on its side, its text theirs); a segment with
// no words goes by its start.
func (tl windowTimeline) split(chs []audio.Chapter) []transcript.Transcript {
	docs := make([]transcript.Transcript, len(chs))
	for i := range docs {
		docs[i] = transcript.Transcript{Language: tl.language, Segments: []transcript.Segment{}}
	}
	at := func(t float64) int {
		i := sort.Search(len(chs), func(i int) bool { return chs[i].Start > t }) - 1
		return max(i, 0)
	}
	add := func(c int, s transcript.Segment) {
		s.ID = len(docs[c].Segments)
		docs[c].Segments = append(docs[c].Segments, s)
	}
	rel := func(c int, t float64) float64 { return math.Max(0, t-chs[c].Start) }
	for _, s := range tl.segments {
		if len(s.words) == 0 || s.synthetic {
			c := at(s.start)
			add(c, transcript.Segment{Start: rel(c, s.start), End: rel(c, s.end), Text: s.text, Words: []transcript.Word{}})
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
			seg := transcript.Segment{Text: s.text, Start: s.start, End: s.end}
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
				seg.Words = append(seg.Words, transcript.Word{W: w.text, Start: rel(c, w.start), End: rel(c, w.end), P: w.p})
			}
			add(c, seg)
		}
	}
	return docs
}
