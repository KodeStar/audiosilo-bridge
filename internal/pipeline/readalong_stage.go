package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/asr"
	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/qa"
	"github.com/kodestar/audiosilo-bridge/internal/readalong"
	"github.com/kodestar/audiosilo-bridge/internal/repair"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
)

// --- readalong_aligning (Phase 3c, the ASR lane, MECHANICAL) ---
//
// A read-along map lines the book's ebook up with its narration, sentence by
// sentence (internal/readalong). The stage reads the word timings of
// transcripts-json/ (never transcripts-repaired/, which has none), aligns them with
// the ebook's text, transcribes again (NoContext) every stretch left unaligned
// between aligned neighbours and every clip-repaired loop, aligns once more with
// those words spliced in, moves each sentence's start to its speech onset and
// writes readalong/map.json for the readalong.align job to save into the library.
// The chapter FLACs a purge took are cut again first: the heal and the onset snap
// need the 16 kHz audio.

// ReadalongFile is one of the book's audio files as the server lists them: its
// path relative to the book (a single-file book: its name) and the server's length.
type ReadalongFile struct {
	Path     string  `json:"path"`
	Duration float64 `json:"duration"`
}

// ReadalongRequest is what a readalong.align job asks of its engine book's stage
// (readalong/request.json): the ebook to line up (the Bridge's path, through the
// checked mapping), the server's files in its order (whole-book time is theirs,
// decision 10) and the Bridge's version for the map's generator.
type ReadalongRequest struct {
	Ebook   string          `json:"ebook"`
	Files   []ReadalongFile `json:"files"`
	Version string          `json:"version"`
}

// WriteReadalongRequest writes the job's request into the book's work dir.
func WriteReadalongRequest(workDir string, req ReadalongRequest) error {
	if err := os.MkdirAll(filepath.Join(workDir, readalong.WorkDir), 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, readalong.RequestPath), append(raw, '\n'), 0o644)
}

func readReadalongRequest(workDir string) (ReadalongRequest, error) {
	var req ReadalongRequest
	raw, err := os.ReadFile(filepath.Join(workDir, readalong.RequestPath)) //nolint:gosec // the book's work dir
	if err != nil {
		return req, err
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, err
	}
	if req.Ebook == "" || len(req.Files) == 0 {
		return req, errors.New("the request names no ebook or no audio files")
	}
	return req, nil
}

// ResetReadalongRepair forgets the mechanical loop-repair round, so a run continued
// from its transcripts again (sanitizing) gets its one round (decision 4).
func ResetReadalongRepair(workDir string) error {
	err := os.Remove(filepath.Join(workDir, readalong.RepairedPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// readalongRepaired reports whether the book's one mechanical repair round ran.
func readalongRepaired(workDir string) bool {
	return fsutil.IsFile(filepath.Join(workDir, readalong.RepairedPath))
}

func markReadalongRepaired(workDir string) error {
	if err := os.MkdirAll(filepath.Join(workDir, readalong.WorkDir), 0o750); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, readalong.RepairedPath), []byte("1\n"), 0o644)
}

// readalongSweep is qa_sweep's routing for goal readalong (decision 4): a report
// that isn't clean gets ONE mechanical repair round (qa.MechanicalPlan, no agent)
// and goes to retranscribing; after that round, or with nothing a re-transcription
// could fix, the book goes on to readalong_aligning clean or not (the gap heal
// covers what is left), the stage note naming the chapters left.
func readalongSweep(workDir string, rep *qa.Report, r scheduler.StageReport) (bool, error) {
	if rep.Clean() {
		return false, nil
	}
	if !readalongRepaired(workDir) {
		if plan := qa.MechanicalPlan(rep); plan != nil && plan.RetranscribeNeeded() {
			if err := qa.WritePlan(workDir, plan); err != nil {
				return false, fmt.Errorf("qa_sweep: write the mechanical plan: %w", err)
			}
			if r.Note != nil {
				r.Note(fmt.Sprintf("read-along: transcribing %s again without context (loop repair, no agent)",
					countNoun(len(plan.NonAcceptEntries()), "chapter")))
			}
			return true, nil
		}
	}
	if r.Note != nil {
		if left := qa.FlaggedChapters(rep); len(left) > 0 {
			noun := "chapters "
			if len(left) == 1 {
				noun = "chapter "
			}
			r.Note("read-along: " + noun + intsCSV(left) + " still flagged by the transcript check; the alignment heals what it can")
		}
	}
	return false, nil
}

// ReadalongReport is readalong/align_report.json: the stage's figures (no text).
type ReadalongReport struct {
	Coverage          readalong.Coverage  `json:"coverage"`
	FirstPassCoverage float64             `json:"first_pass_coverage"`
	Sentences         int                 `json:"sentences"`
	AlignedSentences  int                 `json:"aligned_sentences"`
	Sections          int                 `json:"sections"`
	Spans             int                 `json:"spans"`
	AudioGaps         int                 `json:"audio_gaps"`
	HealWindows       int                 `json:"heal_windows"`
	HealSeconds       float64             `json:"heal_seconds"`
	HealLoopWindows   int                 `json:"heal_loop_windows"`
	Snap              readalong.SnapStats `json:"snap"`
	EbookSHA256       string              `json:"ebook_sha256"`
	Resplit           int                 `json:"resplit"`
}

// ReadalongMap reads the book's map as the stage wrote it: the file's bytes (what
// the job writes into the library) and the map they decode to.
func ReadalongMap(workDir string) ([]byte, readalong.Map, error) {
	raw, err := os.ReadFile(filepath.Join(workDir, readalong.MapPath)) //nolint:gosec // the book's work dir
	if err != nil {
		return nil, readalong.Map{}, err
	}
	var m readalong.Map
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, readalong.Map{}, fmt.Errorf("the read-along map can't be read: %w", err)
	}
	if m.Format != readalong.Format {
		return nil, readalong.Map{}, fmt.Errorf("the read-along map's format is %q", m.Format)
	}
	return raw, m, nil
}

// ReadReadalongReport reads the stage's figures.
func ReadReadalongReport(workDir string) (ReadalongReport, error) {
	var rep ReadalongReport
	raw, err := os.ReadFile(filepath.Join(workDir, readalong.ReportPath)) //nolint:gosec // the book's work dir
	if err != nil {
		return rep, err
	}
	return rep, json.Unmarshal(raw, &rep)
}

// readalongTimeline places the manifest's chapters on the server's files: a files
// manifest's chapter by its file (relative to the book folder, the manifest's
// source), the single-file styles' by their start in the one file.
func readalongTimeline(m audio.Manifest, files []ReadalongFile) (readalong.Timeline, error) {
	var style string
	switch m.Style {
	case audio.StyleFiles:
		style = readalong.StyleFiles
	case audio.StyleMarkers:
		style = readalong.StyleMarkers
	case audio.StyleWindows:
		style = readalong.StyleWindows
	default:
		return readalong.Timeline{}, fmt.Errorf("a %q book has no audio to line up", m.Style)
	}
	chs := make([]readalong.ManifestChapter, 0, len(m.Chapters))
	for _, c := range m.Chapters {
		mc := readalong.ManifestChapter{Chapter: c.Chapter, Start: c.Start}
		if style == readalong.StyleFiles {
			rel, err := filepath.Rel(m.Source, c.FilePath)
			if err != nil || strings.HasPrefix(rel, "..") {
				return readalong.Timeline{}, fmt.Errorf("chapter %d's file isn't in the book's folder", c.Chapter)
			}
			mc.FilePath = filepath.ToSlash(rel)
		}
		chs = append(chs, mc)
	}
	af := make([]readalong.AudioFile, len(files))
	for k, f := range files {
		af[k] = readalong.AudioFile{Path: f.Path, Duration: f.Duration}
	}
	return readalong.Compose(style, chs, af)
}

// readalongAlign is the readalong_aligning stage.
func (e *Executor) readalongAlign(ctx context.Context, book store.Book, r scheduler.StageReport) (scheduler.StageResult, error) {
	ffmpeg, _ := e.ensureTools()
	if ffmpeg == "" {
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkMediaToolsUnavailable, MediaToolsUnavailableMsg)
	}
	req, err := readReadalongRequest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: read the job's request: %w", err)
	}
	manifest, err := audio.ReadManifest(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: read manifest: %w", err)
	}
	tl, err := readalongTimeline(manifest, req.Files)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: %w", err)
	}
	start := time.Now()
	rep := ReadalongReport{}

	// The FLACs a purge took (auto-purge at done) are cut again: the heal and the
	// snap read them.
	if n, err := e.resplitMissing(ctx, book, manifest, ffmpeg, r); err != nil {
		return scheduler.StageResult{}, err
	} else {
		rep.Resplit = n
	}

	words := map[int][]readalong.Word{}
	for _, c := range manifest.Chapters {
		tr, err := transcript.ReadNormalized(filepath.Join(book.WorkDir, transcript.JSONDir), c.Chapter)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a chapter the asr stage accepted as empty
		}
		if err != nil {
			return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: read chapter %d's transcript: %w", c.Chapter, err)
		}
		words[c.Chapter] = transcriptWords(tr, 0)
	}

	epub, err := readalong.OpenEPUB(req.Ebook)
	if err != nil {
		return scheduler.StageResult{}, readalongEbookErr(err)
	}
	text, err := readalong.LoadText(epub)
	if err != nil {
		return scheduler.StageResult{}, readalongEbookErr(err)
	}
	rep.EbookSHA256 = epub.SHA256
	sections := text.Sections()

	first := readalong.Align(sections, tl.BookWords(words, nil))
	rep.FirstPassCoverage = readalong.BodyCoverage(first)

	verdicts, err := repair.TailVerdictsByChapter(book.WorkDir)
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: read the tail verdicts: %w", err)
	}
	var loops []readalong.Window
	for ch, v := range verdicts {
		if w, ok := tl.ChapterWindow(ch, v.ClipStart, v.ClipEnd); ok {
			loops = append(loops, w)
		}
	}
	wins := tl.HealWindows(first, loops)
	rep.HealLoopWindows = len(loops)
	patches, err := e.healWindows(ctx, book, r, tl, wins, ffmpeg)
	if err != nil {
		return scheduler.StageResult{}, err
	}
	rep.HealWindows = len(wins)
	for _, w := range wins {
		rep.HealSeconds += w.End - w.Start
	}
	rep.HealSeconds = math.Round(rep.HealSeconds*10) / 10

	bookWords := tl.BookWords(words, patches)
	res := readalong.Align(sections, bookWords)
	if err := writeReadalongWords(book.WorkDir, bookWords); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: write the words: %w", err)
	}

	err = runWithStageHeartbeat(ctx, r, func(elapsed time.Duration) string {
		return fmt.Sprintf("readalong_aligning: still finding the sentence starts in the audio (%s elapsed)", humanDuration(elapsed))
	}, func() error {
		stats, serr := readalong.Snap(&res, tl, func(chapter int) ([]float64, error) {
			flac := filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(chapter))
			return chapterEnergies(ctx, ffmpeg, flac)
		})
		rep.Snap = stats
		return serr
	})
	if err != nil {
		if ctx.Err() != nil {
			return scheduler.StageResult{}, ctx.Err()
		}
		return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: onset snap: %w", err)
	}

	asrName := readASRProvenance(book.WorkDir).Model
	m, err := readalong.Build(res, text, tl, req.Ebook, readalong.Generator{
		Version: req.Version, ASR: path.Base(filepath.ToSlash(asrName)), Now: time.Now(),
	})
	if err != nil {
		return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: compose the map: %w", err)
	}
	raw, err := readalong.Encode(m)
	if err != nil {
		return scheduler.StageResult{}, err
	}
	rep.Coverage = m.Coverage
	rep.Sentences, rep.AlignedSentences = res.Sentences, res.AlignedSentences
	rep.Sections, rep.Spans, rep.AudioGaps = len(m.Sections), m.Spans(), len(m.AudioGaps)
	if err := fsutil.WriteFileAtomic(filepath.Join(book.WorkDir, readalong.MapPath), raw, 0o644); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: write the map: %w", err)
	}
	repRaw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return scheduler.StageResult{}, err
	}
	if err := fsutil.WriteFileAtomic(filepath.Join(book.WorkDir, readalong.ReportPath), append(repRaw, '\n'), 0o644); err != nil {
		return scheduler.StageResult{}, fmt.Errorf("readalong_aligning: write the report: %w", err)
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("read-along: %.1f%% of the text lined up (%.1f%% before the heal of %s), %s",
			100*m.Coverage.Text, 100*rep.FirstPassCoverage, countNoun(rep.HealWindows, "window"), countNoun(rep.Spans, "sentence")))
	}
	e.accountScratch(ctx, book)
	result := scheduler.StageResult{
		Metrics: metrics(map[string]any{
			"coverage_text":       m.Coverage.Text,
			"coverage_audio":      m.Coverage.Audio,
			"first_pass_coverage": rep.FirstPassCoverage,
			"sentences":           rep.Sentences,
			"aligned_sentences":   rep.AlignedSentences,
			"heal_windows":        rep.HealWindows,
			"heal_seconds":        rep.HealSeconds,
			"resplit":             rep.Resplit,
			"map_bytes":           len(raw),
		}),
		RateSample: rateSample(1, time.Since(start).Seconds()),
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.ReadalongAligning), result); err != nil {
		return scheduler.StageResult{}, err
	}
	return result, nil
}

// readalongEbookErr words an ebook the stage can't read for the job's console:
// the readalong package's own sentences as they are.
func readalongEbookErr(err error) error {
	switch {
	case errors.Is(err, readalong.ErrDRM), errors.Is(err, readalong.ErrNotEPUB), errors.Is(err, readalong.ErrEbookTooBig):
		return err
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("the book's ebook isn't where the job found it; run it again")
	}
	return fmt.Errorf("the book's ebook can't be read: %w", err)
}

// transcriptWords are a transcript's words, offset seconds later.
func transcriptWords(tr transcript.Transcript, offset float64) []readalong.Word {
	var out []readalong.Word
	for _, s := range tr.Segments {
		for _, w := range s.Words {
			out = append(out, readalong.Word{W: w.W, Start: offset + w.Start, End: offset + w.End})
		}
	}
	return out
}

// resplitMissing cuts again every chapter FLAC that isn't there (audio.Split skips
// the ones that are) and reports how many it cut.
func (e *Executor) resplitMissing(ctx context.Context, book store.Book, m audio.Manifest, ffmpeg string, r scheduler.StageReport) (int, error) {
	missing := 0
	for _, c := range m.Chapters {
		if !fsutil.IsFile(filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(c.Chapter))) {
			missing++
		}
	}
	if missing == 0 {
		return 0, nil
	}
	if r.Note != nil {
		r.Note(fmt.Sprintf("read-along: cutting %s of audio again (the work files were cleared after the last run)", countNoun(missing, "chapter")))
	}
	err := runWithStageHeartbeat(ctx, r, func(elapsed time.Duration) string {
		return fmt.Sprintf("readalong_aligning: still cutting the chapters again (%s elapsed)", humanDuration(elapsed))
	}, func() error {
		_, serr := splitWithRetries(ctx, m, book.WorkDir, ffmpeg, r, nil)
		return serr
	})
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("readalong_aligning: cut the chapters again: %w", err)
	}
	return missing, nil
}

// healKey names a heal window's clip and raw: dot-free (an ASR tool's file stem
// must round-trip) and the window's own, so a resumed stage reuses a finished raw.
func healKey(w readalong.Window) string {
	return fmt.Sprintf("f%03d_%09d_%09d", w.File, int64(math.Round(w.Start*1000)), int64(math.Round(w.End*1000)))
}

// healWindows transcribes each heal window again (NoContext, through the engine's
// ASR backend: a worker when one is connected) from a clip cut across its
// chapters' FLACs, and returns the words as patches in in-file time. A finished
// raw is reused; each clip is deleted once transcribed.
func (e *Executor) healWindows(ctx context.Context, book store.Book, r scheduler.StageReport, tl readalong.Timeline, wins []readalong.Window, ffmpeg string) ([]readalong.Patch, error) {
	if r.Progress != nil {
		r.Progress(0, len(wins)+1)
	}
	if len(wins) == 0 {
		return nil, nil
	}
	setup, err := e.readyASR(ctx)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(book.WorkDir, readalong.HealPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	var patches []readalong.Patch
	for k, w := range wins {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := healKey(w)
		clip := filepath.Join(dir, key+".flac")
		rawPath := filepath.Join(dir, key+".json")
		dur := w.End - w.Start
		if !rawComplete(rawPath) {
			var parts []audio.SpanPart
			for _, p := range tl.Parts(w) {
				flac := filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(p.Chapter))
				parts = append(parts, audio.SpanPart{File: flac, Start: p.From, Dur: p.To - p.From})
			}
			if err := audio.CutSpan(ctx, ffmpeg, parts, clip); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, fmt.Errorf("readalong_aligning: cut heal window %d: %w", k+1, err)
			}
			job := asr.Job{Audio: clip, OutDir: dir, Language: setup.Language, NoContext: true,
				Start: tl.FileStart[w.File] + w.Start, Duration: dur}
			terr := runWithStageHeartbeat(ctx, r, func(elapsed time.Duration) string {
				return fmt.Sprintf("readalong_aligning: heal window %d of %d still transcribing (%s elapsed)", k+1, len(wins), humanDuration(elapsed))
			}, func() error { return setup.Backend.Transcribe(ctx, job) })
			if terr != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, fmt.Errorf("readalong_aligning: transcribe heal window %d: %w", k+1, terr)
			}
		}
		_ = os.Remove(clip)
		raw, err := os.ReadFile(rawPath) //nolint:gosec // the book's work dir
		if err != nil {
			return nil, fmt.Errorf("readalong_aligning: read heal window %d: %w", k+1, err)
		}
		tr, err := transcript.Normalize(raw, transcript.Meta{Backend: setup.Backend.ID(), Model: setup.Model, Language: setup.Language})
		if err != nil {
			return nil, fmt.Errorf("readalong_aligning: heal window %d: %w", k+1, err)
		}
		var pw []readalong.Word
		for _, wd := range transcriptWords(tr, 0) {
			if wd.Start < 0 || wd.Start >= dur {
				continue // decoded past the clip's ends
			}
			pw = append(pw, readalong.Word{W: wd.W, Start: w.Start + wd.Start, End: w.Start + math.Min(wd.End, dur)})
		}
		// A window heard as nothing keeps the words it had: an empty patch would
		// only remove them.
		if len(pw) > 0 {
			patches = append(patches, readalong.Patch{File: w.File, Start: w.Start, End: w.End, Words: pw})
		}
		if r.Progress != nil {
			r.Progress(k+1, len(wins)+1)
		}
	}
	return patches, nil
}

// writeReadalongWords writes the words the map was aligned from (book time).
func writeReadalongWords(workDir string, words []readalong.Word) error {
	type word struct {
		W     string  `json:"w"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
	}
	out := make([]word, len(words))
	for k, w := range words {
		out[k] = word{w.W, math.Round(w.Start*1000) / 1000, math.Round(w.End*1000) / 1000}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, readalong.WordsPath), append(raw, '\n'), 0o644)
}

// chapterEnergies decodes a chapter FLAC to 16 kHz mono float32 (an ffmpeg pipe)
// and keeps only its 10 ms frame energies, never the samples (decision 9).
func chapterEnergies(ctx context.Context, ffmpeg, flac string) ([]float64, error) {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", //nolint:gosec // ffmpeg is operator-resolved; flac is a work-dir file
		"-i", flac, "-f", "f32le", "-ac", "1", "-ar", fmt.Sprint(readalong.SampleRate), "-")
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 2000}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	energies, rerr := frameEnergies(bufio.NewReaderSize(out, 1<<16))
	if werr := cmd.Wait(); werr != nil {
		return nil, fmt.Errorf("decode %s: %w: %s", filepath.Base(flac), werr, strings.TrimSpace(stderr.String()))
	}
	return energies, rerr
}

// frameEnergies reads little-endian float32 samples and returns each whole
// frame's energy.
func frameEnergies(r io.Reader) ([]float64, error) {
	buf := make([]byte, readalong.FrameSamples*4)
	frame := make([]float32, readalong.FrameSamples)
	var out []float64
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return out, nil
			}
			return out, err
		}
		for k := range frame {
			frame[k] = math.Float32frombits(binary.LittleEndian.Uint32(buf[4*k:]))
		}
		out = append(out, readalong.Energy(frame))
	}
}

// limitedWriter keeps the first n bytes written to it.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// AudioCurrent reports whether the audio at source (where the book is now; "" for
// the manifest's) is still what the book in workDir was inspected from: a single
// file the size probe.json states, modified no later than probe.json was written
// (give or take sourceClockSlack); a folder of files the same files by name, in
// the manifest's order, none modified since. It fails closed: an error when the
// audio can't be looked at now.
func AudioCurrent(workDir, source string) (bool, error) {
	m, err := audio.ReadManifest(workDir)
	if err != nil {
		return false, err
	}
	if source == "" {
		source = m.Source
	}
	src, err := audio.ResolveSource(source)
	if err != nil {
		if _, serr := os.Stat(source); serr != nil {
			return false, serr
		}
		return false, nil
	}
	switch m.Style {
	case audio.StyleMarkers, audio.StyleWindows:
		if src.Style != audio.StyleMarkers {
			return false, nil
		}
		info, err := os.Stat(src.BookFile)
		if err != nil {
			return false, err
		}
		return inspectedFile(workDir, info), nil
	case audio.StyleFiles:
		if src.Style != audio.StyleFiles || len(src.Files) != len(m.Chapters) {
			return false, nil
		}
		for k, f := range src.Files {
			if filepath.Base(f) != filepath.Base(m.Chapters[k].FilePath) {
				return false, nil
			}
			info, err := os.Stat(f)
			if err != nil {
				return false, err
			}
			if !modifiedBeforeProbe(workDir, info) {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}
