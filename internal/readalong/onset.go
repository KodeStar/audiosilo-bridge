package readalong

import (
	"math"
	"slices"
	"sort"
)

// The onset snap (decision 9): a port of the spike's onset.py and snap.py.
// Whisper gives a pause to the word after it, so a sentence's start sits at the
// end of the word before; each start moves to the speech onset (model-free, on
// 10 ms frame energies) less a 0.1 s lead. Energies are on one 10 ms grid per
// chapter (only they are kept in memory, never the samples), so an onset is on
// that grid rather than the spike's window-relative one (at most 10 ms apart).

// SampleRate is the rate the energies are computed at (the chapter FLACs').
const SampleRate = 16000

// FrameSamples is one 10 ms frame.
const FrameSamples = SampleRate / 100

// Energy is one frame's level: 10 log10(mean square + 1e-10).
func Energy(frame []float32) float64 {
	s := 0.0
	for _, x := range frame {
		s += float64(x) * float64(x)
	}
	return 10 * math.Log10(s/float64(len(frame))+1e-10)
}

// Energies are the whole 10 ms frames' levels of 16 kHz mono samples.
func Energies(pcm []float32) []float64 {
	out := make([]float64, 0, len(pcm)/FrameSamples)
	for k := 0; k+FrameSamples <= len(pcm); k += FrameSamples {
		out = append(out, Energy(pcm[k:k+FrameSamples]))
	}
	return out
}

// percentile is numpy's default (linear) percentile of xs.
func percentile(sorted []float64, p float64) float64 {
	pos := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	if lo+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (sorted[lo+1]-sorted[lo])*(pos-float64(lo))
}

// The onset kinds.
const (
	OnsetAfterPause = "after-pause"
	OnsetRunStart   = "run-start"
	OnsetMidFlow    = "mid-flow"
	OnsetNone       = "no-onset"
	OnsetShort      = "short"
)

const (
	onsetBack = 0.5
	onsetFwd  = 3.0
	minQuiet  = 15 // frames of pause
	minVoiced = 5  // frames of speech
)

// Onset is the speech onset nearest after or at t (chapter seconds), from the
// chapter's energies: in a pause, the start of the next voiced run (>= 50 ms); on
// a word tail just before a pause (>= 150 ms), the speech after it; in speech, the
// start of this voiced run when a pause ends within 0.5 s before t; else t
// (mid-flow).
func Onset(e []float64, t float64) (float64, string) {
	lo := math.Max(0, t-1.5)
	hi := math.Min(float64(len(e))/100, t+onsetFwd+0.5)
	gLo, gHi := int(lo*100), int(hi*100)
	if gHi > len(e) {
		gHi = len(e)
	}
	if gHi-gLo < 30 {
		return t, OnsetShort
	}
	win := e[gLo:gHi]
	lo = float64(gLo) / 100
	sorted := slices.Clone(win)
	sort.Float64s(sorted)
	p10, p95 := percentile(sorted, 10), percentile(sorted, 95)
	thr := p10 + 0.3*(p95-p10)
	n := len(win)
	quiet := make([]bool, n)
	for k, v := range win {
		quiet[k] = v < thr
	}
	anyQuiet := func(a, b int) bool {
		for k := a; k < b && k < n; k++ {
			if quiet[k] {
				return true
			}
		}
		return false
	}
	allQuiet := func(a, b int) bool {
		for k := a; k < b && k < n; k++ {
			if !quiet[k] {
				return false
			}
		}
		return true
	}
	f := min(max(int((t-lo)*100), 0), n-1)
	fwd := int(onsetFwd * 100)
	if quiet[f] {
		for g := f; g < min(n-minVoiced, f+fwd); g++ {
			if !anyQuiet(g, g+minVoiced) {
				return lo + float64(g)/100, OnsetAfterPause
			}
		}
		return t, OnsetNone
	}
	for g := f; g < min(n-minQuiet, f+30); g++ {
		if allQuiet(g, g+minQuiet) {
			h := g + minQuiet
			for h < n-minVoiced && (quiet[h] || anyQuiet(h, h+minVoiced)) {
				h++
			}
			if h < n-minVoiced && h-f <= fwd {
				return lo + float64(h)/100, OnsetAfterPause
			}
			break
		}
	}
	back := int(onsetBack * 100)
	q := 0
	for g := f; g > 0 && f-g < back; {
		g--
		if quiet[g] {
			q++
			if q >= minQuiet {
				return lo + float64(g+q)/100, OnsetRunStart
			}
		} else {
			q = 0
		}
	}
	return t, OnsetMidFlow
}

// SnapStats counts the onset kinds and the mean start shift.
type SnapStats struct {
	Kinds     map[string]int `json:"kinds"`
	MeanShift float64        `json:"mean_shift"`
}

// Snap moves every span's start to its onset - 0.1 s, never past its end - 0.2 s
// nor earlier than its raw start - 0.5 s. energies gives a chapter's frame
// energies (by chapter number); chapters are asked for in time order, once each.
func Snap(res *Result, tl Timeline, energies func(chapter int) ([]float64, error)) (SnapStats, error) {
	var all []*Span
	for s := range res.Sections {
		for k := range res.Sections[s].Spans {
			all = append(all, &res.Sections[s].Spans[k])
		}
	}
	sort.SliceStable(all, func(x, y int) bool { return all[x].T0 < all[y].T0 })
	stats := SnapStats{Kinds: map[string]int{}}
	cur := -1
	var e []float64
	moved := 0.0
	for _, sp := range all {
		raw := sp.T0
		c := tl.Chapters[tl.ChapterAt(raw)]
		if c.Chapter != cur {
			var err error
			if e, err = energies(c.Chapter); err != nil {
				return SnapStats{}, err
			}
			cur = c.Chapter
		}
		o, kind := Onset(e, raw-c.Offset)
		stats.Kinds[kind]++
		nt := raw
		if kind == OnsetAfterPause || kind == OnsetRunStart {
			nt = c.Offset + o - 0.10
		}
		nt = math.Min(nt, sp.T1-0.2)
		sp.T0 = round3(math.Max(nt, raw-0.5))
		moved += sp.T0 - raw
	}
	if len(all) > 0 {
		stats.MeanShift = round3(moved / float64(len(all)))
	}
	return stats, nil
}
