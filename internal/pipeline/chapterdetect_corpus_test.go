package pipeline

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/chapterdetect"
)

// TestChapterDetectSpikeCorpus runs the chapter detection, through the stage's own
// word loading, on the 2026-10-17 spike's real books (BRIDGE-PLAN.md, Phase 1d):
// AUDIOSILO_CHAPTERS_SPIKE_DIR names the spike folder (work/<book>/windows.json and
// raw/w%03d.json, the mlx-whisper raws of each ~25-minute window; silence/<book>.log,
// ffmpeg silencedetect over the whole book; truth/<book>.json, a meta chapter list,
// or truth/<book>.probe.json, the markers that were stripped). The transcripts are
// copyrighted and stay outside the repo. A book whose windows aren't all decoded
// is skipped.
//
// It holds the spike's results: every story boundary the narration announces is
// found within 1 s, and nothing is invented but the real spoken headings the
// publisher's list lacks.
func TestChapterDetectSpikeCorpus(t *testing.T) {
	dir := os.Getenv("AUDIOSILO_CHAPTERS_SPIKE_DIR")
	if dir == "" {
		t.Skip("AUDIOSILO_CHAPTERS_SPIKE_DIR not set")
	}
	// What the reference has that the narration doesn't say, and what the narration
	// says that the reference doesn't: The Emperor's Soul's publisher markers merge
	// its spoken Prologue, Day Five and Day Seventy into the chapters before them,
	// and put a boundary (014) where no heading is spoken.
	missedOK := map[string][]string{"emperors-soul": {"014"}}
	inventedOK := map[string][]string{"emperors-soul": {"Prologue", "Day Five", "Day 70"}}
	ran := 0
	for _, book := range []string{"christmas-carol", "emperors-soul", "the-black-gate", "cosmic-ride", "past-tense"} {
		t.Run(book, func(t *testing.T) {
			windows, duration, ok := spikeWindows(t, dir, book)
			if !ok {
				t.Skipf("%s: its windows aren't all transcribed yet", book)
			}
			ran++
			tl, err := loadWindowTimeline(windows, func(w audio.Chapter) ([]byte, error) {
				return os.ReadFile(filepath.Join(dir, "work", book, "raw", fmt.Sprintf("w%03d.json", w.Chapter-1))) //nolint:gosec // test corpus
			})
			if err != nil {
				t.Fatal(err)
			}
			res := chapterdetect.Detect(tl.words(), spikeSilences(t, dir, book), duration)
			if !res.Confident {
				t.Fatalf("not confident: %s", res.Reason)
			}
			truth := spikeTruth(t, dir, book)
			var errs []float64
			for _, tb := range truth {
				if tb.at <= 1 || spikeMatter(tb.title) || slices.Contains(missedOK[book], tb.title) {
					continue
				}
				best := math.Inf(1)
				for _, c := range res.Chapters {
					best = math.Min(best, math.Min(math.Abs(c.Start-tb.at), math.Abs(c.HeadingAt-tb.at)))
				}
				if best > 1 {
					t.Errorf("story boundary %q at %.1f s not found within 1 s (nearest %.2f s)", tb.title, tb.at, best)
					continue
				}
				errs = append(errs, best)
			}
			var invented []string
			for _, c := range res.Chapters {
				if !c.Spoken {
					continue
				}
				near := false
				for _, tb := range truth {
					near = near || math.Abs(c.HeadingAt-tb.at) <= 10
				}
				if near {
					continue
				}
				if !slices.ContainsFunc(inventedOK[book], func(s string) bool { return strings.EqualFold(s, c.Title) }) {
					t.Errorf("invented a boundary: %q at %.1f s", c.Title, c.HeadingAt)
				}
				invented = append(invented, c.Title)
			}
			slices.Sort(errs)
			med, top := 0.0, 0.0
			if len(errs) > 0 {
				med, top = errs[len(errs)/2], errs[len(errs)-1]
			}
			entries := groupByPosition(detectedManifest(audio.Manifest{Duration: duration}, res.Chapters), tl)
			t.Logf("%s: %.1f h, %d chapters (%d headings), %d story boundaries found, error median %.2f s max %.2f s; "+
				"spoken headings the reference lacks: %v; %d list entries", book, duration/3600, len(res.Chapters),
				res.Headings, len(errs), med, top, invented, len(entries))
			t.Logf("%s titles: %s", book, strings.Join(chapterTitles(res.Chapters), " | "))
		})
	}
	if ran == 0 {
		t.Fatal("no complete book in the spike folder")
	}
}

func chapterTitles(chs []chapterdetect.Chapter) []string {
	out := make([]string, len(chs))
	for i, c := range chs {
		out[i] = c.Title
	}
	return out
}

// spikeWindows reads work/<book>/windows.json as manifest windows (chapters 1..W),
// and whether every window has its raw transcript.
func spikeWindows(t *testing.T, dir, book string) ([]audio.Chapter, float64, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "work", book, "windows.json")) //nolint:gosec // test corpus
	if err != nil {
		return nil, 0, false
	}
	var ws struct {
		Duration float64 `json:"duration"`
		Windows  []struct {
			I     int     `json:"i"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(raw, &ws); err != nil {
		t.Fatal(err)
	}
	out := make([]audio.Chapter, 0, len(ws.Windows))
	for _, x := range ws.Windows {
		if _, err := os.Stat(filepath.Join(dir, "work", book, "raw", fmt.Sprintf("w%03d.json", x.I))); err != nil {
			return nil, 0, false
		}
		out = append(out, audio.Chapter{Chapter: x.I + 1, Start: x.Start, End: x.End, Duration: x.End - x.Start})
	}
	return out, ws.Duration, len(out) > 0
}

var (
	reSpikeSilStart = regexp.MustCompile(`silence_start: (-?[\d.]+)`)
	reSpikeSilEnd   = regexp.MustCompile(`silence_end: ([\d.]+)`)
)

// spikeSilences parses silence/<book>.log.
func spikeSilences(t *testing.T, dir, book string) []chapterdetect.Silence {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "silence", book+".log")) //nolint:gosec // test corpus
	if err != nil {
		t.Fatal(err)
	}
	var out []chapterdetect.Silence
	start, open := 0.0, false
	for _, line := range strings.Split(string(raw), "\n") {
		if m := reSpikeSilStart.FindStringSubmatch(line); m != nil {
			start, _ = strconv.ParseFloat(m[1], 64)
			start, open = math.Max(start, 0), true
		}
		if m := reSpikeSilEnd.FindStringSubmatch(line); m != nil && open {
			end, _ := strconv.ParseFloat(m[1], 64)
			out = append(out, chapterdetect.Silence{Start: start, End: end})
			open = false
		}
	}
	return out
}

type spikeBoundary struct {
	title string
	at    float64
}

// spikeTruth reads the reference chapter list.
func spikeTruth(t *testing.T, dir, book string) []spikeBoundary {
	t.Helper()
	var out []spikeBoundary
	if raw, err := os.ReadFile(filepath.Join(dir, "truth", book+".json")); err == nil { //nolint:gosec // test corpus
		var d struct {
			Chapters []struct {
				Title   string `json:"title"`
				StartMs int64  `json:"start_ms"`
			} `json:"chapters"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		for _, c := range d.Chapters {
			out = append(out, spikeBoundary{c.Title, float64(c.StartMs) / 1000})
		}
		return out
	}
	raw, err := os.ReadFile(filepath.Join(dir, "truth", book+".probe.json")) //nolint:gosec // test corpus
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Chapters []struct {
			StartTime string            `json:"start_time"`
			Tags      map[string]string `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Chapters {
		at, _ := strconv.ParseFloat(c.StartTime, 64)
		out = append(out, spikeBoundary{c.Tags["title"], at})
	}
	return out
}

var reSpikeMatter = regexp.MustCompile(`(?i)credits|dedication|copyright|acknowledg|about the|^preface|introduction|opening|closing|^001$`)

// spikeMatter is the spike's matter rule: a reference entry that is no story chapter.
func spikeMatter(title string) bool { return reSpikeMatter.MatchString(title) }
