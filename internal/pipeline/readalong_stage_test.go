package pipeline

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/asr"
	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/qa"
	"github.com/kodestar/audiosilo-bridge/internal/readalong"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
)

// The read-along fixture: two audio files (one chapter each), an EPUB of two
// sections of four sentences, one per file. Each sentence is 8 words, 0.3 s each,
// read from 0.3 s into a 3 s slot; the second file's second sentence is missing
// from its transcript (a gap the heal transcribes again).
const (
	raFileSecs    = 12.0
	raSentences   = 4
	raWords       = 8
	raWordSecs    = 0.3
	raSlotSecs    = 3.0
	raLeadSecs    = 0.3
	raGapSection  = 1
	raGapSentence = 1
)

func raWord(sec, sent, k int) string {
	return fmt.Sprintf("Word%sx%s", strings.Repeat("a", sec+1), strings.Repeat("b", sent*raWords+k+1))
}

func raSentence(sec, sent int) string {
	var w []string
	for k := range raWords {
		w = append(w, raWord(sec, sent, k))
	}
	return strings.Join(w, " ") + "."
}

// raEPUB writes the fixture EPUB.
func raEPUB(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	put := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	put("mimetype", "application/epub+zip")
	put("META-INF/container.xml", `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`)
	put("OEBPS/content.opf", `<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata/><manifest>`+
		`<item id="s1" href="s1.xhtml" media-type="application/xhtml+xml"/><item id="s2" href="s2.xhtml" media-type="application/xhtml+xml"/>`+
		`</manifest><spine><itemref idref="s1"/><itemref idref="s2"/></spine></package>`)
	for sec := range 2 {
		var body strings.Builder
		for s := range raSentences {
			body.WriteString("<p>" + raSentence(sec, s) + "</p>")
		}
		put(fmt.Sprintf("OEBPS/s%d.xhtml", sec+1), `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><head><title>x</title></head><body>`+body.String()+`</body></html>`)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// raSentenceWords are a sentence's words, in seconds from base.
func raSentenceWords(sec, sent int, base float64) []transcript.Word {
	var out []transcript.Word
	for k := range raWords {
		at := base + float64(k)*raWordSecs
		out = append(out, transcript.Word{W: " " + raWord(sec, sent, k), Start: at, End: at + raWordSecs*0.9})
	}
	return out
}

// raChapterTranscript is file sec's transcript, the gap sentence left out.
func raChapterTranscript(sec int) transcript.Transcript {
	tr := transcript.Transcript{Schema: transcript.Schema, Chapter: sec + 1, Backend: "fake", Model: "fake", Language: "en"}
	for s := range raSentences {
		if sec == raGapSection && s == raGapSentence {
			continue
		}
		start := float64(s)*raSlotSecs + raLeadSecs
		ws := raSentenceWords(sec, s, start)
		tr.Segments = append(tr.Segments, transcript.Segment{ID: s, Start: start, End: ws[len(ws)-1].End, Text: raSentence(sec, s), Words: ws})
	}
	return tr
}

// healBackend is an ASR backend for the heal: it reads the gap sentence at its
// place in the clip (the clip's book start is the job's Start).
type healBackend struct {
	mu   sync.Mutex
	jobs []asr.Job
}

func (h *healBackend) ID() string { return "fake" }
func (h *healBackend) Detect(context.Context) (asr.Capability, error) {
	return asr.Capability{Backend: "fake", Available: true}, nil
}
func (h *healBackend) EnsureReady(context.Context) error { return nil }
func (h *healBackend) Transcribe(_ context.Context, job asr.Job) error {
	h.mu.Lock()
	h.jobs = append(h.jobs, job)
	h.mu.Unlock()
	if !fileExists(job.Audio) {
		return fmt.Errorf("no clip at %s", job.Audio)
	}
	// The gap sentence's book time, made clip-relative.
	at := raFileSecs*float64(raGapSection) + float64(raGapSentence)*raSlotSecs + raLeadSecs - job.Start
	type w struct {
		Word  string  `json:"word"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		P     float64 `json:"probability"`
	}
	var words []w
	for _, x := range raSentenceWords(raGapSection, raGapSentence, at) {
		words = append(words, w{x.W, x.Start, x.End, 0.9})
	}
	raw, _ := json.Marshal(map[string]any{"text": raSentence(raGapSection, raGapSentence), "language": "en",
		"segments": []map[string]any{{"id": 0, "start": at, "end": words[len(words)-1].End, "text": raSentence(raGapSection, raGapSentence), "words": words}}})
	stem := strings.TrimSuffix(filepath.Base(job.Audio), filepath.Ext(job.Audio))
	return os.WriteFile(filepath.Join(job.OutDir, stem+".json"), raw, 0o644) //nolint:gosec // test artifact
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// raFixture builds the book: the two audio files (tone, then quiet, in each
// sentence's slot), the inspected manifest and probe, the transcripts, the
// request. No chapter FLACs: a purge took them.
func raFixture(t *testing.T, ffmpeg string) (store.Book, string) {
	t.Helper()
	dir := t.TempDir()
	bookDir := filepath.Join(dir, "Book")
	work := filepath.Join(dir, "work")
	for _, d := range []string{bookDir, work} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	var chapters []audio.Chapter
	for sec := range 2 {
		f := filepath.Join(bookDir, fmt.Sprintf("%02d.mp3", sec+1))
		// Speech-like bursts: 2.4 s of tone from 0.3 s into each 3 s slot.
		expr := fmt.Sprintf("if(between(mod(t\\,%g)\\,%g\\,%g)\\,0.5*sin(2*PI*220*t)\\,0)", raSlotSecs, raLeadSecs, raLeadSecs+raWords*raWordSecs)
		cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", //nolint:gosec // test
			fmt.Sprintf("aevalsrc=%s:s=16000:d=%g", expr, raFileSecs), "-ac", "1", f)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
		chapters = append(chapters, audio.Chapter{Chapter: sec + 1, Start: float64(sec) * raFileSecs, End: float64(sec+1) * raFileSecs,
			Duration: raFileSecs, FilePath: f})
		seedNormalized(t, work, raChapterTranscript(sec))
	}
	writeManifestStruct(t, work, audio.Manifest{Source: bookDir, Style: audio.StyleFiles, Duration: 2 * raFileSecs,
		ChapterCount: 2, Chapters: chapters})
	writeProbeJSON(t, work, `{"format":{"duration":"24"}}`)
	if err := writeASRProvenance(work, asrProvenance{Backend: "mlx-whisper", Model: "mlx-community/whisper-large-v3-turbo", Language: "en"}); err != nil {
		t.Fatal(err)
	}
	ebook := filepath.Join(bookDir, "Book.epub")
	raEPUB(t, ebook)
	if err := WriteReadalongRequest(work, ReadalongRequest{Ebook: ebook, Version: "0.9.0", Files: []ReadalongFile{
		{Path: "01.mp3", Duration: raFileSecs}, {Path: "02.mp3", Duration: raFileSecs}}}); err != nil {
		t.Fatal(err)
	}
	return store.Book{ID: 1, WorkDir: work, SourcePath: bookDir, Goal: state.GoalReadalong, State: string(state.ReadalongAligning)}, ebook
}

// TestReadalongAligningStage runs the stage on a purged book: the chapter FLACs
// are cut again, the gap between aligned sentences is transcribed again
// (NoContext, a clip in the second file) and spliced in, and the map lines up
// every sentence, book-relative and with the original model's name. A second run
// reuses the heal's raw (no ASR) and writes the same spans.
func TestReadalongAligningStage(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	book, _ := raFixture(t, ffmpeg)
	hb := &healBackend{}
	exe := NewExecutor(Config{DataDir: t.TempDir(), FFmpeg: ffmpeg, ASR: fakeASR(hb), Fallback: scheduler.NewStubExecutor(0, 0)})
	var notes []string
	r := scheduler.StageReport{Note: func(s string) { notes = append(notes, s) }}
	if _, err := exe.Execute(context.Background(), book, state.ReadalongAligning, r); err != nil {
		t.Fatal(err)
	}
	for ch := 1; ch <= 2; ch++ {
		if !fileExists(filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(ch))) {
			t.Fatalf("chapter %d's FLAC wasn't cut again", ch)
		}
	}
	if len(hb.jobs) != 1 || !hb.jobs[0].NoContext {
		t.Fatalf("heal jobs %+v, want one NoContext window", hb.jobs)
	}
	if j := hb.jobs[0]; j.Start < raFileSecs || j.Start+j.Duration > 2*raFileSecs {
		t.Fatalf("the heal window [%g, %g] isn't in the second file", j.Start, j.Start+j.Duration)
	}
	if fileExists(hb.jobs[0].Audio) {
		t.Fatal("the heal clip was kept")
	}
	raw, m, err := ReadalongMap(book.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Spans() != 2*raSentences {
		t.Fatalf("%d spans, want every sentence (%d): %s", m.Spans(), 2*raSentences, raw)
	}
	if m.Coverage.Text != 1 || m.Generator.ASR != "whisper-large-v3-turbo" || m.Generator.Version != "0.9.0" ||
		m.Ebook.File != "Book.epub" || len(m.Audio.Files) != 2 || m.Audio.Files[1].Path != "02.mp3" || m.Audio.Duration != 24 {
		t.Fatalf("map head: %+v %+v %+v %+v", m.Coverage, m.Generator, m.Ebook, m.Audio)
	}
	gap := m.Sections[raGapSection].Spans[raGapSentence]
	want := raFileSecs + float64(raGapSentence)*raSlotSecs + raLeadSecs - 0.1
	if gap.Start < want-0.15 || gap.Start > want+0.15 {
		t.Fatalf("the healed sentence starts at %g, want about %g (onset less the lead)", gap.Start, want)
	}
	rep, err := ReadReadalongReport(book.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HealWindows != 1 || rep.Resplit != 2 || rep.FirstPassCoverage >= 1 || rep.Coverage.Text != 1 || rep.EbookSHA256 != m.Ebook.SHA256 {
		t.Fatalf("report %+v", rep)
	}
	if !scheduler.SentinelExists(book.WorkDir, string(state.ReadalongAligning)) {
		t.Fatal("no sentinel")
	}
	if !fileExists(filepath.Join(book.WorkDir, readalong.WordsPath)) {
		t.Fatal("no words.json")
	}
	// Resume: the heal's raw is reused, so no ASR runs again.
	if err := os.Remove(scheduler.SentinelPath(book.WorkDir, string(state.ReadalongAligning))); err != nil {
		t.Fatal(err)
	}
	if _, err := exe.Execute(context.Background(), book, state.ReadalongAligning, scheduler.StageReport{}); err != nil {
		t.Fatal(err)
	}
	if len(hb.jobs) != 1 {
		t.Fatalf("a resumed stage transcribed again: %d jobs", len(hb.jobs))
	}
	_, again, err := ReadalongMap(book.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(again.Sections) != fmt.Sprint(m.Sections) {
		t.Fatal("a resumed stage wrote other spans")
	}
}

// TestReadalongAligningRefusesTheEbook: a DRM-protected or missing ebook fails the
// stage with its sentence; a server file list the manifest's files aren't in
// fails before any work.
func TestReadalongAligningRefusesTheEbook(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	book, ebook := raFixture(t, ffmpeg)
	exe := NewExecutor(Config{DataDir: t.TempDir(), FFmpeg: ffmpeg, ASR: fakeASR(&healBackend{}), Fallback: scheduler.NewStubExecutor(0, 0)})
	f, err := os.Create(ebook) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("META-INF/rights.xml")
	_, _ = w.Write([]byte("<rights/>"))
	_ = zw.Close()
	_ = f.Close()
	if _, err := exe.Execute(context.Background(), book, state.ReadalongAligning, scheduler.StageReport{}); err == nil || err.Error() != readalong.ErrDRM.Error() {
		t.Fatalf("DRM: %v", err)
	}
	_ = os.Remove(ebook)
	if _, err := exe.Execute(context.Background(), book, state.ReadalongAligning, scheduler.StageReport{}); err == nil || !strings.Contains(err.Error(), "isn't where") {
		t.Fatalf("missing: %v", err)
	}
	if err := WriteReadalongRequest(book.WorkDir, ReadalongRequest{Ebook: ebook, Files: []ReadalongFile{{Path: "other.mp3", Duration: 24}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := exe.Execute(context.Background(), book, state.ReadalongAligning, scheduler.StageReport{}); err == nil || !strings.Contains(err.Error(), "isn't one of the server's files") {
		t.Fatalf("files: %v", err)
	}
}

// TestReadalongSweepOneRound: a dirty sweep of a read-along book plans one
// mechanical round (no agent) and goes to retranscribing; once that round ran it
// goes on to aligning, clean or not; a clean one goes straight on.
func TestReadalongSweepOneRound(t *testing.T) {
	work := t.TempDir()
	dirty := &qa.Report{RetranscribeQueue: []int{2}, RepeatedRuns: []qa.RepeatedRun{{Chapter: 3}}}
	var notes []string
	r := scheduler.StageReport{Note: func(s string) { notes = append(notes, s) }}
	got, err := readalongSweep(work, dirty, r)
	if err != nil || !got {
		t.Fatalf("first sweep: %v %v", got, err)
	}
	plan, err := qa.LoadPlan(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.NonAcceptEntries()) != 2 {
		t.Fatalf("plan %+v", plan.Entries)
	}
	if err := markReadalongRepaired(work); err != nil {
		t.Fatal(err)
	}
	if got, err := readalongSweep(work, dirty, r); err != nil || got {
		t.Fatalf("second sweep: %v %v", got, err)
	}
	if last := notes[len(notes)-1]; !strings.Contains(last, "chapters 2,3") && !strings.Contains(last, "chapters 2, 3") {
		t.Fatalf("note %q doesn't name the chapters left", last)
	}
	if err := ResetReadalongRepair(work); err != nil {
		t.Fatal(err)
	}
	if got, _ := readalongSweep(work, &qa.Report{}, r); got {
		t.Fatal("a clean sweep was repaired")
	}
	// Nothing a re-transcription could fix (a cross-segment hit only): straight on.
	if got, _ := readalongSweep(work, &qa.Report{CrossSegment: []qa.CrossSegmentHit{{Chapter: 4}}}, r); got {
		t.Fatal("an accept-only plan went to retranscribing")
	}
}

// TestReadalongRetranscribeMarksTheRound: a read-along book's retranscribing
// marks its round, and cuts a FLAC a purge took again before transcribing it.
func TestReadalongRetranscribeMarksTheRound(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	book, _ := raFixture(t, ffmpeg)
	book.State = string(state.Retranscribing)
	seedPlan(t, book.WorkDir, qa.PlanEntry{Chapter: 2, Action: qa.ActionRetranscribe, Reason: "loop"}, qa.PlanEntry{Chapter: 1, Action: qa.ActionAccept, Reason: "fine"})
	fake := newFakeBackend()
	exe := NewExecutor(Config{DataDir: t.TempDir(), FFmpeg: ffmpeg, ASR: fakeASR(fake), Fallback: scheduler.NewStubExecutor(0, 0)})
	if _, err := exe.Execute(context.Background(), book, state.Retranscribing, scheduler.StageReport{}); err != nil {
		t.Fatal(err)
	}
	if fake.count(2) != 1 || !fake.sawNoContext(2) {
		t.Fatalf("chapter 2 transcribed %d times", fake.count(2))
	}
	if !fileExists(filepath.Join(book.WorkDir, audio.ChaptersDir, audio.ChapterFileName(2))) {
		t.Fatal("chapter 2's FLAC wasn't cut again")
	}
	if !readalongRepaired(book.WorkDir) {
		t.Fatal("the round wasn't marked")
	}
}

// TestAudioCurrent: a folder book's files as inspected are current; a file
// added, renamed or touched after the probe isn't; a folder gone is an error.
func TestAudioCurrent(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	book, _ := raFixture(t, ffmpeg)
	if ok, err := AudioCurrent(book.WorkDir, ""); err != nil || !ok {
		t.Fatalf("as inspected: %v %v", ok, err)
	}
	f := filepath.Join(book.SourcePath, "02.mp3")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(f, future, future); err != nil {
		t.Fatal(err)
	}
	if ok, _ := AudioCurrent(book.WorkDir, ""); ok {
		t.Fatal("a file touched since is current")
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(f, past, past)
	extra := filepath.Join(book.SourcePath, "03.mp3")
	if err := os.WriteFile(extra, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := AudioCurrent(book.WorkDir, ""); ok {
		t.Fatal("a folder with a file added is current")
	}
	_ = os.Remove(extra)
	if ok, err := AudioCurrent(book.WorkDir, filepath.Join(filepath.Dir(book.SourcePath), "gone")); err == nil || ok {
		t.Fatalf("a folder gone: %v %v", ok, err)
	}
	// The same files where the book is now (moved): current.
	moved := filepath.Join(filepath.Dir(book.SourcePath), "Moved")
	if err := os.Rename(book.SourcePath, moved); err != nil {
		t.Fatal(err)
	}
	if ok, err := AudioCurrent(book.WorkDir, moved); err != nil || !ok {
		t.Fatalf("moved: %v %v", ok, err)
	}
}

// TestTranscriptionContinuableChapters: a files book stopped at asr goes on from
// splitting while its audio is as inspected and every chapter has its raw or its
// FLAC; a file touched since is denied.
func TestTranscriptionContinuableChapters(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	book, _ := raFixture(t, ffmpeg)
	if !TranscriptionContinuable(book.WorkDir) {
		t.Fatal("a files book before splitting's sentinel isn't continuable")
	}
	if err := scheduler.WriteSentinel(book.WorkDir, string(state.Splitting), scheduler.StageResult{}); err != nil {
		t.Fatal(err)
	}
	if TranscriptionContinuable(book.WorkDir) {
		t.Fatal("continuable with neither a raw nor a FLAC for its chapters")
	}
	seedFLACs(t, book.WorkDir, 2)
	if !TranscriptionContinuable(book.WorkDir) {
		t.Fatal("not continuable with every FLAC there")
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(book.SourcePath, "01.mp3"), future, future)
	if TranscriptionContinuable(book.WorkDir) {
		t.Fatal("continuable with a file touched since it was inspected")
	}
}

// TestReadalongInspectWindowsAnUnusableTable: a read-along book whose marker table
// isn't usable (chapter 2 missing) is transcribed in windows, never sent to the
// agent's markers_normalizing; a generate book's goes there as before.
func TestReadalongInspectWindowsAnUnusableTable(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	var meta strings.Builder
	meta.WriteString(";FFMETADATA1\ntitle=Gappy\n")
	for i, title := range []string{"Chapter 1", "Chapter 3", "Chapter 4"} {
		fmt.Fprintf(&meta, "[CHAPTER]\nTIMEBASE=1/1000\nSTART=%d\nEND=%d\ntitle=%s\n", i*2000, (i+1)*2000, title)
	}
	metaPath := filepath.Join(dir, "meta.txt")
	if err := os.WriteFile(metaPath, []byte(meta.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "gappy.m4b")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=220:duration=6", //nolint:gosec // test
		"-i", metaPath, "-map", "0:a", "-map_metadata", "1", "-c:a", "aac", src).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	exe := NewExecutor(Config{FFmpeg: ffmpeg, FFprobe: ffprobe, Fallback: scheduler.NewStubExecutor(0, 0)})
	for _, tc := range []struct {
		goal  string
		style string
	}{{state.GoalReadalong, audio.StyleWindows}, {"", audio.StyleMarkers}} {
		work := filepath.Join(dir, "work-"+tc.goal)
		if err := os.MkdirAll(work, 0o750); err != nil {
			t.Fatal(err)
		}
		res, err := exe.Execute(context.Background(), store.Book{SourcePath: src, WorkDir: work, Goal: tc.goal}, state.Inspecting, scheduler.StageReport{})
		if err != nil {
			t.Fatalf("goal %q: %v", tc.goal, err)
		}
		m, err := audio.ReadManifest(work)
		if err != nil || m.Style != tc.style {
			t.Fatalf("goal %q: manifest %q, want %q (%v)", tc.goal, m.Style, tc.style, err)
		}
		next, _, err := state.NextState(state.KindAudio, state.Inspecting, state.Outcome{Goal: tc.goal, MarkersContiguous: res.MarkersContiguous})
		if want := map[string]state.State{state.GoalReadalong: state.Splitting, "": state.MarkersNormalizing}[tc.goal]; err != nil || next != want {
			t.Fatalf("goal %q: next %q, want %q (%v)", tc.goal, next, want, err)
		}
	}
}

// The transcript check's located loops become heal windows (a re-align reuses the
// kept transcripts, loops and all, and the mechanical round writes no tail
// verdicts); an unlocated one, or one over 600 s, doesn't.
func TestReadalongLoopWindows(t *testing.T) {
	tl, err := readalong.Compose(readalong.StyleFiles, []readalong.ManifestChapter{{Chapter: 1, FilePath: "01.mp3"}, {Chapter: 4, FilePath: "04.mp3"}},
		[]readalong.AudioFile{{Path: "01.mp3", Duration: 1510.89}, {Path: "04.mp3", Duration: 1400}})
	if err != nil {
		t.Fatal(err)
	}
	sec := func(v float64) *float64 { return &v }
	rep := &qa.Report{CrossSegment: []qa.CrossSegmentHit{
		{Chapter: 1, Count: 2641, FirstSec: sec(1277), LastSec: sec(1507)},
		{Chapter: 4, Count: 4194, FirstSec: sec(957), LastSec: sec(1331)},
		{Chapter: 4, Count: 9, Pos: -1},                               // not located
		{Chapter: 1, Count: 6, FirstSec: sec(10), LastSec: sec(1200)}, // a phrase the book repeats
	}}
	got := readalongLoopWindows(rep, tl)
	want := []readalong.Window{{File: 0, Start: 1276.5, End: 1507.5}, {File: 1, Start: 956.5, End: 1331.5}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("windows %+v, want %+v", got, want)
	}
}
