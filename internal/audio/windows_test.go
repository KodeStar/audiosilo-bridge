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
