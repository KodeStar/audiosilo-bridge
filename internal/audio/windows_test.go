package audio

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkerless(t *testing.T) {
	for _, tc := range []struct {
		name    string
		markers []Marker
		want    bool
	}{
		{"no markers", nil, true},
		{"one marker over the whole file", []Marker{{Title: "The Black Gate", Start: 0, End: 990}}, true},
		{"one marker over part of it", []Marker{{Title: "Chapter 1", Start: 0, End: 500}}, false},
		{"a table", []Marker{{Start: 0, End: 500}, {Start: 500, End: 1000}}, false},
	} {
		if got := markerless(tc.markers, 1000); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Without ffmpeg (or a silence) each cut falls on its target; a short file is one
// window and a remainder under five minutes stays with the last.
func TestWindowsManifestCuts(t *testing.T) {
	for _, tc := range []struct {
		duration float64
		want     []float64 // window starts
	}{
		{1000, []float64{0}},
		{1800, []float64{0}},
		{1801, []float64{0, 1500}},
		{3000, []float64{0, 1500}},
		{3290, []float64{0, 1500}},
		{3300, []float64{0, 1500}},
		{3301, []float64{0, 1500, 3000}},
		{3801, []float64{0, 1500, 3000}},
	} {
		m, err := windowsManifest(context.Background(), "/book.mp3", "T", tc.duration, "")
		if err != nil {
			t.Fatal(err)
		}
		var starts []float64
		for i, c := range m.Chapters {
			starts = append(starts, c.Start)
			if c.Chapter != i+1 || c.Duration != c.End-c.Start {
				t.Errorf("%v: window %+v", tc.duration, c)
			}
		}
		if len(starts) != len(tc.want) || m.Chapters[len(m.Chapters)-1].End != tc.duration || m.Style != StyleWindows {
			t.Errorf("%v: starts %v, want %v (%+v)", tc.duration, starts, tc.want, m)
			continue
		}
		for i := range starts {
			if starts[i] != tc.want[i] {
				t.Errorf("%v: starts %v, want %v", tc.duration, starts, tc.want)
			}
		}
	}
}

func TestParseSilences(t *testing.T) {
	log := []byte("[silencedetect @ 0x1] silence_start: -0.01\n[silencedetect @ 0x1] silence_end: 1.5 | silence_duration: 1.5\n" +
		"size=N/A\n[silencedetect @ 0x1] silence_start: 40\n")
	got := parseSilences(log, 100, 60)
	if len(got) != 2 || got[0] != (Silence{100, 101.5}) || got[1] != (Silence{140, 160}) {
		t.Fatalf("silences = %+v", got)
	}
}

func TestDetectSilencesAndWindowedInspect(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	// 4 s of tone, 2 s of silence, 4 s of tone; no chapter markers.
	book := filepath.Join(dir, "book.m4a")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=4", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono:d=2",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=4",
		"-filter_complex", "[0][1][2]concat=n=3:v=0:a=1", "-c:a", "aac", book).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v: %s", err, out)
	}
	sil, err := DetectSilences(context.Background(), ffmpeg, book, 2, 6, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sil) != 1 || math.Abs(sil[0].Start-4) > 0.2 || math.Abs(sil[0].End-6) > 0.2 {
		t.Fatalf("silences on the file's timeline = %+v, want about 4-6 s", sil)
	}
	m, stats, err := Inspect(context.Background(), book, filepath.Join(dir, "work"), ffprobe, ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	if m.Style != StyleWindows || m.ChapterCount != 1 || !stats.Usable() || stats.Seen != 0 {
		t.Fatalf("manifest %+v, stats %+v: want one window", m, stats)
	}
	// One marker over the whole file is a title, not a chapter table.
	titled := genChapteredM4B(t, ffmpeg, t.TempDir(), []string{"The Book"}, 5)
	m, stats, err = Inspect(context.Background(), titled, filepath.Join(dir, "work2"), ffprobe, ffmpeg)
	if err != nil || m.Style != StyleWindows || !stats.Usable() || stats.Seen != 1 {
		t.Fatalf("one full marker: %+v %+v %v", m, stats, err)
	}
}

// flacSeconds is a FLAC's duration as ffprobe reads it.
func flacSeconds(t *testing.T, ffprobe, path string) float64 {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return parseFloat(strings.TrimSpace(string(out)))
}

// Split hears a windows manifest's silences in the same decode and keeps them beside
// each window FLAC (on the file's timeline, for that window only), and the detected
// chapters are cut from the window FLACs, within a window or across one, to the
// millisecond; a chapter whose window is gone is left for the source split.
func TestWindowSilencesAndCutFromWindows(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	// 4 s tone, 2 s silence, 8 s tone, 1 s silence, 5 s tone: 20 s, no markers.
	src := filepath.Join(dir, "book.wav")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=4", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono:d=2",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=8", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono:d=1",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=5",
		"-filter_complex", "[0][1][2][3][4]concat=n=5:v=0:a=1", src).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v: %s", err, out)
	}
	windows := []Chapter{{Chapter: 1, Start: 0, End: 10, Duration: 10}, {Chapter: 2, Start: 10, End: 20, Duration: 10}}
	work := filepath.Join(dir, "work")
	m := Manifest{Source: src, Style: StyleWindows, Duration: 20, ChapterCount: 2, Chapters: windows}
	if err := Split(context.Background(), m, work, ffmpeg, nil); err != nil {
		t.Fatal(err)
	}
	chapters := filepath.Join(work, ChaptersDir)
	near := func(a, b, tol float64) bool { return math.Abs(a-b) <= tol }
	want := [][]Silence{{{4, 6}}, {{14, 15}}}
	for i, w := range windows {
		// No ffmpeg: only the kept silences can answer.
		got, err := WindowSilences(context.Background(), "", chapters, w, 0)
		if err != nil || len(got) != 1 || !near(got[0].Start, want[i][0].Start, 0.05) || !near(got[0].End, want[i][0].End, 0.05) {
			t.Fatalf("window %d kept silences = %+v, %v; want about %+v", w.Chapter, got, err, want[i])
		}
		// The ones the FLAC itself gives.
		flac, err := DetectSilences(context.Background(), ffmpeg, filepath.Join(chapters, ChapterFileName(w.Chapter)), 0, 0, 0)
		if err != nil || len(flac) != 1 || !near(flac[0].Start+w.Start, got[0].Start, 0.01) || !near(flac[0].End+w.Start, got[0].End, 0.01) {
			t.Fatalf("window %d: the FLAC's silences %+v, split heard %+v", w.Chapter, flac, got)
		}
	}
	// Denied: silences kept for another cut of the book aren't this window's (and
	// with no ffmpeg nothing else can answer).
	if _, err := WindowSilences(context.Background(), "", chapters, Chapter{Chapter: 1, Start: 0, End: 9, Duration: 9}, 0); err == nil {
		t.Fatal("another window's kept silences were used")
	}

	// The window FLACs move aside, as chapter_detecting moves them, and the detected
	// chapters are cut from them: one inside window 1, one across the edge, one
	// inside window 2.
	audioDir := filepath.Join(work, WindowAudioDir)
	if err := os.MkdirAll(filepath.Dir(audioDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(chapters, audioDir); err != nil {
		t.Fatal(err)
	}
	detected := Manifest{Source: src, Style: StyleMarkers, Detected: true, Duration: 20, ChapterCount: 3, Chapters: []Chapter{
		{Chapter: 1, Start: 0, End: 7.5, Duration: 7.5}, {Chapter: 2, Start: 7.5, End: 13.25, Duration: 5.75},
		{Chapter: 3, Start: 13.25, End: 20, Duration: 6.75}}}
	left, err := CutFromWindows(context.Background(), detected, windows, audioDir, work, ffmpeg)
	if err != nil || left != 0 {
		t.Fatalf("cut = %d left, %v", left, err)
	}
	for _, ch := range detected.Chapters {
		p := filepath.Join(work, ChaptersDir, ChapterFileName(ch.Chapter))
		got := flacSeconds(t, ffprobe, p)
		if !near(got, ch.Duration, 0.003) {
			t.Errorf("chapter %d runs %.4f s, want %.4f s", ch.Chapter, got, ch.Duration)
		}
		if codec, channels, rate := probeFlac(t, ffprobe, p); codec != "flac" || channels != 1 || rate != 16000 {
			t.Errorf("chapter %d is %s/%d/%d", ch.Chapter, codec, channels, rate)
		}
	}
	// The silence at 14-15 s sits 0.75-1.75 s into chapter 3: the cut is the same audio.
	sil, err := DetectSilences(context.Background(), ffmpeg, filepath.Join(work, ChaptersDir, ChapterFileName(3)), 0, 0, 0)
	if err != nil || len(sil) != 1 || !near(sil[0].Start, 0.75, 0.05) || !near(sil[0].End, 1.75, 0.05) {
		t.Fatalf("chapter 3 silences = %+v, %v", sil, err)
	}
	// A window gone: the chapter that needs it is left for the source split; the
	// cut ones are kept.
	if err := os.Remove(filepath.Join(audioDir, ChapterFileName(2))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(work, ChaptersDir, ChapterFileName(3))); err != nil {
		t.Fatal(err)
	}
	if left, err := CutFromWindows(context.Background(), detected, windows, audioDir, work, ffmpeg); err != nil || left != 1 {
		t.Fatalf("with window 2 gone: %d left, %v; want 1", left, err)
	}
}

// A probe that fails (its time bound ran out on a wedged mount) ends the probing:
// the rest of a long book is cut at the targets, not probed once per cut.
func TestWindowCutsStopProbingAfterAFailure(t *testing.T) {
	calls := 0
	probe := func(t float64) (float64, bool, error) {
		calls++
		return 0, false, context.DeadlineExceeded
	}
	cuts, err := windowCuts(context.Background(), 40*3600, probe)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("probed %d times after a failure, want once", calls)
	}
	for i, c := range cuts[:len(cuts)-1] {
		if c != float64(i)*windowTarget {
			t.Fatalf("cut %d at %v, want its target", i, c)
		}
	}
	// A probe that answers keeps being asked, and its quiet points are used.
	calls = 0
	cuts, _ = windowCuts(context.Background(), 4000, func(t float64) (float64, bool, error) { calls++; return t + 30, true, nil })
	if calls != 2 || cuts[1] != windowTarget+30 {
		t.Fatalf("calls %d, cuts %v", calls, cuts)
	}
	// Cancelled: the probe's error is the context's.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := windowCuts(ctx, 4000, probe); err == nil {
		t.Fatal("a cancelled probe was taken for a failed one")
	}
}

// An ffmpeg before 7 prints a tiny silence time with an exponent.
func TestParseSilencesExponent(t *testing.T) {
	got := parseSilences([]byte("silence_start: 6.25e-05\nsilence_end: 1.40006 | silence_duration: 1.4\n"), 1500, 240)
	if len(got) != 1 || math.Abs(got[0].Start-1500.0000625) > 1e-9 || math.Abs(got[0].End-1501.40006) > 1e-9 {
		t.Fatalf("silences = %+v", got)
	}
}

// A book an older parser parked at markers_normalizing with no chapters, whose file
// has no markers, is recovered into windows (cut at the targets) with no agent round;
// a draft with chapters, or a file with a table, keeps the old rules.
func TestReparseMarkerlessIntoWindows(t *testing.T) {
	work := t.TempDir()
	writeProbe(t, work, `{"format":{"duration":"4000.000"},"chapters":[]}`)
	m, stats, err := ReparseMarkerManifest(work, Manifest{Source: "/book.m4b", Style: StyleMarkers, Duration: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if m.Style != StyleWindows || m.ChapterCount != 3 || !stats.Usable() || m.Chapters[1].Start != windowTarget {
		t.Fatalf("manifest %+v, stats %+v", m, stats)
	}
	if back, err := ReadManifest(work); err != nil || back.Style != StyleWindows {
		t.Fatalf("written manifest %+v, %v", back, err)
	}
	// Denied: a draft that has chapters is the marker rules' (an agent maps them).
	work2 := t.TempDir()
	writeProbe(t, work2, `{"format":{"duration":"4000.000"},"chapters":[]}`)
	m, _, err = ReparseMarkerManifest(work2, Manifest{Source: "/book.m4b", Style: StyleMarkers, Duration: 4000,
		Chapters: []Chapter{{Chapter: 1, Start: 0, End: 4000, Duration: 4000}}})
	if err != nil || m.Style != StyleMarkers {
		t.Fatalf("a draft with chapters: %+v, %v", m, err)
	}
}

// A read-along book's inspect (Phase 3c) windows a marker table that isn't usable
// (chapter 2 missing from the numbering) instead of drafting it for markers_normalizing; any
// other book's inspect keeps the draft.
func TestInspectWindowsAnUnusableTableForReadalong(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	book := genChapteredM4B(t, ffmpeg, dir, []string{"Chapter 1", "Chapter 3", "Chapter 4"}, 2)
	m, stats, err := Inspect(context.Background(), book, filepath.Join(dir, "work"), ffprobe, ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Usable() || m.Style != StyleMarkers {
		t.Fatalf("whole run: style %s, usable %v", m.Style, stats.Usable())
	}
	m, stats, err = InspectWith(context.Background(), book, filepath.Join(dir, "work2"), ffprobe, ffmpeg, InspectOptions{WindowUnusable: true})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Usable() || m.Style != StyleWindows || len(m.Chapters) != 1 || m.Chapters[0].End < 5.9 {
		t.Fatalf("read-along: style %s, usable %v, %+v", m.Style, stats.Usable(), m.Chapters)
	}
	// A usable table stays a markers manifest for a read-along book too.
	gdir := filepath.Join(dir, "g")
	if err := os.MkdirAll(gdir, 0o750); err != nil {
		t.Fatal(err)
	}
	good := genChapteredM4B(t, ffmpeg, gdir, []string{"Chapter 1", "Chapter 2"}, 2)
	m, _, err = InspectWith(context.Background(), good, filepath.Join(dir, "work3"), ffprobe, ffmpeg, InspectOptions{WindowUnusable: true})
	if err != nil || m.Style != StyleMarkers || len(m.Chapters) != 2 {
		t.Fatalf("usable table: %s %d, %v", m.Style, len(m.Chapters), err)
	}
}

// CutSpan joins stretches of chapter FLACs into one clip (the read-along heal).
func TestCutSpan(t *testing.T) {
	ffmpeg, ffprobe := requireFFmpeg(t)
	dir := t.TempDir()
	var files []string
	for k := range 2 {
		p := filepath.Join(dir, ChapterFileName(k+1))
		if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
			"-i", "sine=frequency=220:duration=4", "-ac", "1", "-ar", "16000", p).CombinedOutput(); err != nil {
			t.Fatalf("generate: %v: %s", err, out)
		}
		files = append(files, p)
	}
	out := filepath.Join(dir, "clip.flac")
	if err := CutSpan(context.Background(), ffmpeg, []SpanPart{{files[0], 3, 1}, {files[1], 0, 1.5}}, out); err != nil {
		t.Fatal(err)
	}
	if got := flacSeconds(t, ffprobe, out); math.Abs(got-2.5) > 0.003 {
		t.Fatalf("clip runs %.4f s, want 2.5", got)
	}
	if err := CutSpan(context.Background(), ffmpeg, nil, out); err == nil {
		t.Fatal("an empty clip was cut")
	}
	if err := CutSpan(context.Background(), "", []SpanPart{{files[0], 0, 1}}, out); err == nil {
		t.Fatal("a clip was cut without ffmpeg")
	}
}
