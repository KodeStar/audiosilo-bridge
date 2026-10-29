package readalong_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/epubcfi"
	"github.com/kodestar/audiosilo-bridge/internal/readalong"
)

// TestParityWithSpike holds the Go aligner to the spike's Python
// (decision 26): on each corpus book's transcripts-json and EPUB, heal and snap
// off, against whisper-noheal.spans.json (pre-snap). Set AUDIOSILO_READALONG_SPIKE
// to the spike's folder (.readalong-spike) to run it.
func TestParityWithSpike(t *testing.T) {
	root := os.Getenv("AUDIOSILO_READALONG_SPIKE")
	if root == "" {
		t.Skip("AUDIOSILO_READALONG_SPIKE not set")
	}
	var manifest []struct {
		Slug  string `json:"slug"`
		EPUB  string `json:"epub"`
		Files []struct {
			Path     string  `json:"path"`
			Duration float64 `json:"duration"`
		} `json:"files"`
	}
	readJSON(t, filepath.Join(root, "corpus/manifest.json"), &manifest)
	for _, b := range manifest {
		t.Run(b.Slug, func(t *testing.T) {
			work := filepath.Join(root, "work", b.Slug)
			var py pySpans
			readJSON(t, filepath.Join(work, "whisper-noheal.spans.json"), &py)
			e, err := readalong.OpenEPUB(filepath.Join(root, "corpus", b.Slug, b.EPUB))
			if err != nil {
				t.Fatal(err)
			}
			text, err := readalong.LoadText(e)
			if err != nil {
				t.Fatal(err)
			}
			checkWalkMatchesSpike(t, filepath.Join(work, "text.json"), text)
			var files []readalong.AudioFile
			var chapters []readalong.ManifestChapter
			for k, f := range b.Files {
				files = append(files, readalong.AudioFile{Path: filepath.Base(f.Path), Duration: f.Duration})
				chapters = append(chapters, readalong.ManifestChapter{Chapter: k + 1, FilePath: filepath.Base(f.Path)})
			}
			tl, err := readalong.Compose(readalong.StyleFiles, chapters, files)
			if err != nil {
				t.Fatal(err)
			}
			words := map[int][]readalong.Word{}
			for k := range b.Files {
				words[k+1] = loadWords(t, filepath.Join(work, "transcripts-json", fmt.Sprintf("ch%03d.json", k+1)))
			}
			start := time.Now()
			res := readalong.Align(text.Sections(), tl.BookWords(words, nil))
			took := time.Since(start)
			pyRes := py.result()
			goCov, pyCov := readalong.BodyCoverage(res), readalong.BodyCoverage(pyRes)
			t.Logf("go: %d/%d sentences aligned, body %.4f, %d matched tokens (%s); python: %d/%d, body %.4f, %d matched",
				res.AlignedSentences, res.Sentences, goCov, res.MatchedTokens, took.Round(time.Millisecond),
				py.Stats.AlignedSentences, py.Stats.Sentences, pyCov, py.Stats.MatchedTokens)
			if d := math.Abs(float64(res.AlignedSentences-py.Stats.AlignedSentences)) / float64(py.Stats.AlignedSentences); d > 0.02 {
				t.Errorf("aligned sentences %d vs the spike's %d (%.1f%% apart)", res.AlignedSentences, py.Stats.AlignedSentences, 100*d)
			}
			if goCov < pyCov-0.005 {
				t.Errorf("body coverage %.4f, the spike's %.4f", goCov, pyCov)
			}
			pyStart := map[[2]int]float64{}
			for _, s := range pyRes.Sections {
				for _, sp := range s.Spans {
					pyStart[[2]int{s.Index, sp.Start}] = sp.T0
				}
			}
			var diffs []float64
			for _, s := range res.Sections {
				for _, sp := range s.Spans {
					if t0, ok := pyStart[[2]int{s.Index, sp.Start}]; ok {
						diffs = append(diffs, math.Abs(sp.T0-t0))
					}
				}
			}
			sort.Float64s(diffs)
			if len(diffs) == 0 || diffs[len(diffs)/2] > 0.05 {
				t.Errorf("shared sentence starts: %d, median |d| %v", len(diffs), median(diffs))
			}
			m, err := readalong.Build(res, text, tl, b.EPUB, readalong.Generator{Version: "spike-parity",
				ASR: "mlx-community/whisper-large-v3-turbo", Now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)})
			if err != nil {
				t.Fatal(err)
			}
			checkCFIsResolve(t, e, text, res, m)
			// AUDIOSILO_READALONG_MAPS=<dir> writes each book's map (heal and snap off):
			// the spike's maps in the v1 format (testdata/alice.readalong.json is one).
			if dir := os.Getenv("AUDIOSILO_READALONG_MAPS"); dir != "" {
				raw, err := readalong.Encode(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, b.Slug+".readalong.json"), raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	return xs[len(xs)/2]
}

type pySpans struct {
	Sections []struct {
		Index int `json:"index"`
		Spans []struct {
			Start int     `json:"start"`
			End   int     `json:"end"`
			T0    float64 `json:"t0"`
			T1    float64 `json:"t1"`
		} `json:"spans"`
		Gaps []struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"gaps"`
	} `json:"sections"`
	Stats struct {
		Sentences        int `json:"sentences"`
		AlignedSentences int `json:"aligned_sentences"`
		MatchedTokens    int `json:"matched_tokens"`
	} `json:"stats"`
}

func (p pySpans) result() readalong.Result {
	var r readalong.Result
	for _, s := range p.Sections {
		sr := readalong.SectionResult{Index: s.Index}
		for _, sp := range s.Spans {
			sr.Spans = append(sr.Spans, readalong.Span{Start: sp.Start, End: sp.End, T0: sp.T0, T1: sp.T1})
		}
		for _, g := range s.Gaps {
			sr.Gaps = append(sr.Gaps, readalong.Gap{Start: g.Start, End: g.End})
		}
		r.Sections = append(r.Sections, sr)
	}
	return r
}

func readJSON(t *testing.T, p string, v any) {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

func loadWords(t *testing.T, p string) []readalong.Word {
	t.Helper()
	var doc struct {
		Segments []struct {
			Words []struct {
				W     string  `json:"w"`
				Start float64 `json:"start"`
				End   float64 `json:"end"`
			} `json:"words"`
		} `json:"segments"`
	}
	readJSON(t, p, &doc)
	var out []readalong.Word
	for _, s := range doc.Segments {
		for _, w := range s.Words {
			out = append(out, readalong.Word{W: w.W, Start: w.Start, End: w.End})
		}
	}
	return out
}

// checkWalkMatchesSpike: the Go walk is the spike's jsdom one, section by section.
func checkWalkMatchesSpike(t *testing.T, p string, text *readalong.Text) {
	t.Helper()
	var spike struct {
		Spine []struct {
			Index  int    `json:"index"`
			Text   string `json:"text"`
			Linear *bool  `json:"linear"`
		} `json:"spine"`
	}
	readJSON(t, p, &spike)
	secs := map[int]readalong.TextSection{}
	for _, s := range text.Sections() {
		secs[s.Index] = s
	}
	for _, s := range spike.Spine {
		if s.Linear == nil || !*s.Linear {
			continue
		}
		if got := string(secs[s.Index].Text); got != s.Text {
			t.Fatalf("section %d: the Go walk's text differs from the spike's (%d vs %d runes)", s.Index, len([]rune(got)), len([]rune(s.Text)))
		}
	}
}

// checkCFIsResolve: every span and gap CFI resolves, on a fresh parse, to the
// text it was made from.
func checkCFIsResolve(t *testing.T, e *readalong.EPUB, text *readalong.Text, res readalong.Result, m readalong.Map) {
	t.Helper()
	spine := map[int]readalong.Section{}
	for _, s := range e.Spine {
		spine[s.Index] = s
	}
	k := 0
	n := 0
	for _, sec := range res.Sections {
		if len(sec.Spans) == 0 && len(sec.Gaps) == 0 {
			continue
		}
		ms := m.Sections[k]
		k++
		doc, err := e.Document(spine[sec.Index])
		if err != nil {
			t.Fatal(err)
		}
		check := func(cfi string, start, end int) {
			r, err := epubcfi.Resolve(doc, cfi)
			if err != nil {
				t.Fatalf("%s: %v", cfi, err)
			}
			if got, want := r.Text(doc), text.Slice(sec.Index, start, end); got != want {
				t.Fatalf("%s resolves to %q, want %q", cfi, got, want)
			}
			n++
		}
		for q, sp := range sec.Spans {
			check(ms.Spans[q].CFI, sp.Start, sp.End)
		}
		for q, g := range sec.Gaps {
			check(ms.Gaps[q].CFI, g.Start, g.End)
		}
	}
	if n == 0 || !slices.ContainsFunc(m.Sections, func(s readalong.MapSection) bool { return len(s.Spans) > 0 }) {
		t.Fatal("no CFIs checked")
	}
}
