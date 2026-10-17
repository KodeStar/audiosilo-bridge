package audio

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
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
	if m.Style != StyleWindows || m.ChapterCount != 1 || !stats.Windowed || !stats.Usable() || stats.Seen != 0 {
		t.Fatalf("manifest %+v, stats %+v: want one window", m, stats)
	}
	// One marker over the whole file is a title, not a chapter table.
	titled := genChapteredM4B(t, ffmpeg, t.TempDir(), []string{"The Book"}, 5)
	m, stats, err = Inspect(context.Background(), titled, filepath.Join(dir, "work2"), ffprobe, ffmpeg)
	if err != nil || m.Style != StyleWindows || !stats.Windowed || stats.Seen != 1 {
		t.Fatalf("one full marker: %+v %+v %v", m, stats, err)
	}
}
