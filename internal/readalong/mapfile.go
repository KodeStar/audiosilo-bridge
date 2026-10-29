package readalong

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"time"
)

// The map file (audiosilo-readalong/v1, plan "Wire shapes"): keys in this order,
// whole-book seconds rounded to 0.01 (file durations to 0.001), sections in spine
// order with hrefs relative to the OPF's folder, spans in text order, paths
// relative to the book.

// Format is the map's format string.
const Format = "audiosilo-readalong/v1"

// AlignerName is the generator's aligner: this port of the spike's whisper-words
// aligner, version 1.
const AlignerName = "whisper-words/1"

// MaxMapBytes bounds a map file (decision 16, the server's maxReadalongFile).
const MaxMapBytes = 16 << 20

// audioGapMin is the shortest stretch without a span listed as an audio gap.
const audioGapMin = 5.0

// Map is a read-along map.
type Map struct {
	Format      string       `json:"format"`
	Ebook       MapEbook     `json:"ebook"`
	Audio       MapAudio     `json:"audio"`
	Granularity string       `json:"granularity"`
	Generator   MapGenerator `json:"generator"`
	GeneratedAt string       `json:"generated_at"`
	Coverage    Coverage     `json:"coverage"`
	Sections    []MapSection `json:"sections"`
	AudioGaps   []TimeRange  `json:"audio_gaps"`
}

// MapEbook names the ebook the map is for.
type MapEbook struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

// MapAudio is the audio the map is for.
type MapAudio struct {
	Duration float64   `json:"duration"`
	Files    []MapFile `json:"files"`
}

// MapFile is one audio file, relative to the book.
type MapFile struct {
	Path     string  `json:"path"`
	Duration float64 `json:"duration"`
}

// MapGenerator says what made the map.
type MapGenerator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Aligner string `json:"aligner"`
	ASR     string `json:"asr"`
}

// Coverage is the share of the read body's text lined up, and of the audio.
type Coverage struct {
	Text  float64 `json:"text"`
	Audio float64 `json:"audio"`
}

// MapSection is one spine item's spans and gaps.
type MapSection struct {
	Href  string    `json:"href"`
	Spans []MapSpan `json:"spans"`
	Gaps  []MapGap  `json:"gaps,omitempty"`
}

// MapSpan is a sentence and when it's read.
type MapSpan struct {
	CFI   string  `json:"cfi"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// MapGap is a sentence not lined up.
type MapGap struct {
	CFI   string `json:"cfi"`
	Chars int    `json:"chars"`
}

// TimeRange is a stretch of audio.
type TimeRange struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Generator is what Build writes as the map's generator and stamp.
type Generator struct {
	Version string
	ASR     string
	Now     time.Time
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

// BodyCoverage is the share of the text lined up over the read body: the
// sentences from the first aligned one to the last (front and back matter the
// narrator didn't read don't count against it), by characters.
func BodyCoverage(res Result) float64 {
	type sent struct {
		chars   int
		aligned bool
	}
	var all []sent
	for _, sec := range res.Sections {
		items := make([]struct {
			start int
			sent
		}, 0, len(sec.Spans)+len(sec.Gaps))
		for _, sp := range sec.Spans {
			items = append(items, struct {
				start int
				sent
			}{sp.Start, sent{sp.End - sp.Start, true}})
		}
		for _, g := range sec.Gaps {
			items = append(items, struct {
				start int
				sent
			}{g.Start, sent{g.End - g.Start, false}})
		}
		sort.SliceStable(items, func(x, y int) bool { return items[x].start < items[y].start })
		for _, it := range items {
			all = append(all, it.sent)
		}
	}
	first, last := -1, -1
	for k, s := range all {
		if s.aligned {
			if first < 0 {
				first = k
			}
			last = k
		}
	}
	if first < 0 {
		return 0
	}
	total, aligned := 0, 0
	for _, s := range all[first : last+1] {
		total += s.chars
		if s.aligned {
			aligned += s.chars
		}
	}
	return round4(float64(aligned) / float64(max(total, 1)))
}

// AudioCoverage is the share of the audio inside a span.
func AudioCoverage(res Result, duration float64) float64 {
	if duration <= 0 {
		return 0
	}
	spoken := 0.0
	for _, sec := range res.Sections {
		for _, sp := range sec.Spans {
			spoken += sp.T1 - sp.T0
		}
	}
	return round4(math.Min(1, spoken/duration))
}

// Build writes the map of an alignment: CFIs from the text, times clamped to the
// book and rounded, the coverage and the audio gaps.
func Build(res Result, text *Text, tl Timeline, ebookPath string, gen Generator) (Map, error) {
	duration := tl.Duration()
	m := Map{
		Format:      Format,
		Ebook:       MapEbook{File: path.Base(ebookPath), SHA256: text.EPUB.SHA256},
		Audio:       MapAudio{Duration: round2(duration)},
		Granularity: "sentence",
		Generator:   MapGenerator{Name: "audiosilo-bridge", Version: gen.Version, Aligner: AlignerName, ASR: gen.ASR},
		GeneratedAt: gen.Now.UTC().Format(time.RFC3339),
		Coverage:    Coverage{Text: BodyCoverage(res), Audio: AudioCoverage(res, duration)},
		Sections:    []MapSection{},
		AudioGaps:   []TimeRange{},
	}
	for _, f := range tl.Files {
		m.Audio.Files = append(m.Audio.Files, MapFile{Path: f.Path, Duration: round3(f.Duration)})
	}
	clamp := func(t float64) float64 { return round2(math.Min(math.Max(t, 0), duration)) }
	var all []MapSpan
	for _, sec := range res.Sections {
		if len(sec.Spans) == 0 && len(sec.Gaps) == 0 {
			continue
		}
		out := MapSection{Href: sec.Href, Spans: []MapSpan{}}
		for _, sp := range sec.Spans {
			cfi, err := text.CFI(sec.Index, sp.Start, sp.End)
			if err != nil {
				return Map{}, fmt.Errorf("%s: %w", sec.Href, err)
			}
			ms := MapSpan{CFI: cfi, Start: clamp(sp.T0), End: clamp(sp.T1)}
			ms.End = math.Max(ms.End, ms.Start)
			out.Spans = append(out.Spans, ms)
			all = append(all, ms)
		}
		for _, g := range sec.Gaps {
			cfi, err := text.CFI(sec.Index, g.Start, g.End)
			if err != nil {
				return Map{}, fmt.Errorf("%s: %w", sec.Href, err)
			}
			out.Gaps = append(out.Gaps, MapGap{CFI: cfi, Chars: g.End - g.Start})
		}
		m.Sections = append(m.Sections, out)
	}
	m.AudioGaps = audioGaps(all, duration)
	return m, nil
}

// audioGaps are the stretches over audioGapMin with no span.
func audioGaps(spans []MapSpan, duration float64) []TimeRange {
	sorted := append([]MapSpan(nil), spans...)
	sort.SliceStable(sorted, func(x, y int) bool { return sorted[x].Start < sorted[y].Start })
	out := []TimeRange{}
	cur := 0.0
	for _, s := range sorted {
		if s.Start-cur > audioGapMin {
			out = append(out, TimeRange{Start: round2(cur), End: round2(s.Start)})
		}
		cur = math.Max(cur, s.End)
	}
	if duration-cur > audioGapMin {
		out = append(out, TimeRange{Start: round2(cur), End: round2(duration)})
	}
	return out
}

// ErrMapTooBig is a map over MaxMapBytes.
var ErrMapTooBig = errors.New("the read-along map is over 16 MiB")

// Encode is the map's file bytes (compact JSON and a newline), refused over
// MaxMapBytes.
func Encode(m Map) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > MaxMapBytes {
		return nil, ErrMapTooBig
	}
	return raw, nil
}

// Spans counts the map's spans.
func (m Map) Spans() int {
	n := 0
	for _, s := range m.Sections {
		n += len(s.Spans)
	}
	return n
}
