package readalong

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
)

// Whole-book time (decision 10): a transcript's times are chapter-relative and the
// manifest only places a chapter in its FILE (files style: start 0; markers,
// windows and detected: its start in the one file). Whole-book time is the sum of
// the SERVER's durations of the files before it, in the server's order, plus the
// in-file start, plus the word's time. The manifest's own cross-file starts (the
// Bridge's ffprobe) are never used.

// AudioFile is one of the book's audio files in the server's order: its path
// relative to the book (a single-file book: the base name) and the server's
// duration.
type AudioFile struct {
	Path     string
	Duration float64
}

// ManifestChapter is what time composition needs of a manifest chapter: its
// number, its start (in-file for the single-file styles) and, files style, its
// file relative to the book.
type ManifestChapter struct {
	Chapter  int
	Start    float64
	FilePath string
}

// PlacedChapter is a transcribed chapter on the whole-book timeline.
type PlacedChapter struct {
	Chapter int
	File    int
	// Start and End are in-file seconds (End: the next chapter's start in the same
	// file, else the file's end); Offset is the whole-book time of its second 0.
	Start, End float64
	Offset     float64
}

// Timeline places a book's chapters on the server's whole-book timeline.
type Timeline struct {
	Files     []AudioFile
	FileStart []float64
	Chapters  []PlacedChapter
}

// Manifest styles (audio.StyleFiles and the single-file ones), named here so the
// package stays a leaf.
const (
	StyleFiles   = "files"
	StyleMarkers = "markers"
	StyleWindows = "windows"
)

// ErrNoAudio is a timeline without files or chapters.
var ErrNoAudio = errors.New("the book has no audio files to line up")

// Compose places a manifest's chapters on the server's files.
func Compose(style string, chapters []ManifestChapter, files []AudioFile) (Timeline, error) {
	if len(files) == 0 || len(chapters) == 0 {
		return Timeline{}, ErrNoAudio
	}
	tl := Timeline{Files: files, FileStart: make([]float64, len(files))}
	acc := 0.0
	for k, f := range files {
		if !(f.Duration > 0) || math.IsInf(f.Duration, 0) {
			return Timeline{}, fmt.Errorf("the server has no length for %s", f.Path)
		}
		tl.FileStart[k] = acc
		acc += f.Duration
	}
	index := map[string]int{}
	for k, f := range files {
		index[f.Path] = k
	}
	for _, c := range chapters {
		pc := PlacedChapter{Chapter: c.Chapter}
		switch style {
		case StyleFiles:
			k, ok := index[c.FilePath]
			if !ok {
				return Timeline{}, fmt.Errorf("chapter %d's file %q isn't one of the server's files for the book", c.Chapter, c.FilePath)
			}
			pc.File = k
		case StyleMarkers, StyleWindows:
			if len(files) != 1 {
				return Timeline{}, fmt.Errorf("a %s manifest over %d files", style, len(files))
			}
			pc.Start = math.Max(0, c.Start)
		default:
			return Timeline{}, fmt.Errorf("no audio timeline for a %q manifest", style)
		}
		pc.Offset = tl.FileStart[pc.File] + pc.Start
		tl.Chapters = append(tl.Chapters, pc)
	}
	sort.SliceStable(tl.Chapters, func(x, y int) bool { return tl.Chapters[x].Offset < tl.Chapters[y].Offset })
	for k := range tl.Chapters {
		c := &tl.Chapters[k]
		c.End = files[c.File].Duration
		if k+1 < len(tl.Chapters) && tl.Chapters[k+1].File == c.File {
			c.End = tl.Chapters[k+1].Start
		}
	}
	return tl, nil
}

// Duration is the book's length: the server's durations summed.
func (tl Timeline) Duration() float64 {
	if len(tl.Files) == 0 {
		return 0
	}
	last := len(tl.Files) - 1
	return tl.FileStart[last] + tl.Files[last].Duration
}

// ChapterAt is the index in Chapters of the chapter playing at whole-book time t
// (the first one before t starts).
func (tl Timeline) ChapterAt(t float64) int {
	k := sort.Search(len(tl.Chapters), func(k int) bool { return tl.Chapters[k].Offset > t }) - 1
	return max(k, 0)
}

// fileAt is the file playing at whole-book time t.
func (tl Timeline) fileAt(t float64) int {
	k := sort.Search(len(tl.FileStart), func(k int) bool { return tl.FileStart[k] > t }) - 1
	return max(k, 0)
}

// Patch is a re-transcribed window: in-file seconds of one file, its words in
// in-file time.
type Patch struct {
	File       int
	Start, End float64
	Words      []Word
}

// BookWords lays the chapters' words (chapter-relative, by chapter number) on the
// whole-book timeline, file by file: a patch's window replaces the words that
// start inside it, and each file's words are sorted by start.
func (tl Timeline) BookWords(words map[int][]Word, patches []Patch) []Word {
	var out []Word
	for f := range tl.Files {
		base := tl.FileStart[f]
		var fileWords []Word
		var own []Patch
		for _, p := range patches {
			if p.File == f {
				own = append(own, p)
			}
		}
		for _, c := range tl.Chapters {
			if c.File != f {
				continue
			}
			for _, w := range words[c.Chapter] {
				inFile := c.Start + w.Start
				if inPatch(own, inFile) {
					continue
				}
				fileWords = append(fileWords, Word{W: w.W, Start: base + inFile, End: base + c.Start + w.End})
			}
		}
		for _, p := range own {
			for _, w := range p.Words {
				fileWords = append(fileWords, Word{W: w.W, Start: base + w.Start, End: base + w.End})
			}
		}
		slices.SortStableFunc(fileWords, func(a, b Word) int {
			switch {
			case a.Start < b.Start:
				return -1
			case a.Start > b.Start:
				return 1
			}
			return 0
		})
		out = append(out, fileWords...)
	}
	return out
}

func inPatch(patches []Patch, t float64) bool {
	for _, p := range patches {
		if p.Start <= t && t < p.End {
			return true
		}
	}
	return false
}

// Window is a stretch of one file to transcribe again: in-file seconds, and how
// many unaligned sentences it covers (0 for a loop window from tail_verdicts).
type Window struct {
	File       int
	Start, End float64
	Gaps       int
}

// The heal windows' bounds (the spike's): 0.5 s into the aligned neighbours, 2 to
// 600 s long.
const (
	healPad = 0.5
	healMin = 2.0
	healMax = 600.0
	// A section's trailing run (no aligned sentence after it in the section) runs
	// from its last aligned sentence for as long as its text could take to narrate
	// at a slow 10 characters a second, plus 10 s, never past its chapter's end nor
	// the next section's first aligned sentence. CotW's chapter-end loops took 13.5.
	healTrailCharsPerSec = 10.0
	healTrailSlack       = 10.0
)

// ChapterWindow is a chapter-relative stretch as a window (a tail_verdicts
// window: end <= start runs to the chapter's end). ok is false for a chapter the
// timeline doesn't have.
func (tl Timeline) ChapterWindow(chapter int, start, end float64) (Window, bool) {
	for _, c := range tl.Chapters {
		if c.Chapter != chapter {
			continue
		}
		a := c.Start + math.Max(0, start)
		b := c.End
		if end > start {
			b = math.Min(c.End, c.Start+end)
		}
		if b <= a {
			return Window{}, false
		}
		return Window{File: c.File, Start: a, End: b}, true
	}
	return Window{}, false
}

// LoopWindow is a chapter's looped stretch (the transcript check's, chapter-relative
// first to last second) as a window, 0.5 s wider each side and within the chapter;
// ok is false for a chapter the timeline doesn't have, or a stretch under 2 or over
// 600 s (a phrase the book itself repeats across a chapter, not a loop to heal).
func (tl Timeline) LoopWindow(chapter int, from, to float64) (Window, bool) {
	if to <= from {
		return Window{}, false
	}
	w, ok := tl.ChapterWindow(chapter, from-healPad, to+healPad)
	if d := w.End - w.Start; !ok || d < healMin || d > healMax {
		return Window{}, false
	}
	return w, true
}

// chapterEndAt is the whole-book end of the chapter playing at whole-book t (t
// itself when none does). trailingWindow asks it at the window's start, half a
// second inside the last aligned sentence, so a sentence ending on a file boundary
// is its own chapter's.
func (tl Timeline) chapterEndAt(t float64) float64 {
	f := tl.fileAt(t)
	if f >= len(tl.FileStart) {
		return t
	}
	x := t - tl.FileStart[f]
	for _, c := range tl.Chapters {
		if c.File == f && c.Start <= x && x < c.End {
			return tl.FileStart[f] + c.End
		}
	}
	return t
}

// HealWindows plans the gap heal (decision 8): every run of unaligned sentences
// between two aligned ones in a section becomes [prev.end - 0.5, next.start + 0.5]
// when 2 to 600 s long, and a section's trailing run (a loop that took its
// chapter's end, say) [prev.end - 0.5, ...] for as long as its text could take
// (healTrailCharsPerSec), never past its chapter's end nor the next section's first
// aligned sentence, at most 600 s; each cut at a FILE boundary into one window per
// file (each again 2 to 600 s); then the extra windows (tail_verdicts, the
// transcript check's loops) are added and overlapping windows in a file merged.
func (tl Timeline) HealWindows(res Result, extra []Window) []Window {
	// nextT0[i] is the start of the first aligned sentence after section i.
	nextT0 := make([]float64, len(res.Sections))
	next := math.Inf(1)
	for i := len(res.Sections) - 1; i >= 0; i-- {
		nextT0[i] = next
		if sp := res.Sections[i].Spans; len(sp) > 0 {
			next = sp[0].T0
			for _, s := range sp {
				next = math.Min(next, s.T0)
			}
		}
	}
	var wins []Window
	for si, sec := range res.Sections {
		type item struct {
			start, chars int
			span         *Span
		}
		items := make([]item, 0, len(sec.Spans)+len(sec.Gaps))
		for k := range sec.Spans {
			items = append(items, item{start: sec.Spans[k].Start, span: &sec.Spans[k]})
		}
		for _, g := range sec.Gaps {
			items = append(items, item{start: g.Start, chars: max(0, g.End-g.Start)})
		}
		sort.SliceStable(items, func(x, y int) bool { return items[x].start < items[y].start })
		for k := 0; k < len(items); {
			if items[k].span != nil {
				k++
				continue
			}
			j := k
			for j < len(items) && items[j].span == nil {
				j++
			}
			switch {
			case k > 0 && j < len(items):
				a, b := items[k-1].span.T1-healPad, items[j].span.T0+healPad
				if d := b - a; d >= healMin && d <= healMax {
					wins = append(wins, tl.cutAtFiles(a, b, j-k)...)
				}
			case k > 0:
				chars := 0
				for _, it := range items[k:j] {
					chars += it.chars
				}
				if a, b, ok := tl.trailingWindow(items[k-1].span.T1, nextT0[si], chars); ok {
					wins = append(wins, tl.cutAtFiles(a, b, j-k)...)
				}
			}
			k = j
		}
	}
	wins = append(wins, extra...)
	return mergeWindows(wins)
}

// trailingWindow is the whole-book stretch a section's trailing run of chars
// characters may lie in after its last aligned sentence ended at t1: to its
// chapter's end, the next aligned sentence (next) or as long as the text could take
// (healTrailCharsPerSec), whichever comes first, at most 600 s. ok is false for no
// text, or a stretch under 2 s.
func (tl Timeline) trailingWindow(t1, next float64, chars int) (a, b float64, ok bool) {
	if chars <= 0 {
		return 0, 0, false
	}
	a = t1 - healPad
	b = min(tl.chapterEndAt(a), next+healPad, t1+float64(chars)/healTrailCharsPerSec+healTrailSlack, a+healMax)
	return a, b, b-a >= healMin
}

// cutAtFiles cuts whole-book [a, b] into one in-file window per file it touches.
func (tl Timeline) cutAtFiles(a, b float64, gaps int) []Window {
	var out []Window
	a = math.Max(0, a)
	for f := tl.fileAt(a); f < len(tl.Files) && tl.FileStart[f] < b; f++ {
		lo := math.Max(a, tl.FileStart[f]) - tl.FileStart[f]
		hi := math.Min(b, tl.FileStart[f]+tl.Files[f].Duration) - tl.FileStart[f]
		if d := hi - lo; d >= healMin && d <= healMax {
			out = append(out, Window{File: f, Start: lo, End: hi, Gaps: gaps})
		}
	}
	return out
}

func mergeWindows(wins []Window) []Window {
	slices.SortStableFunc(wins, func(a, b Window) int {
		if a.File != b.File {
			return a.File - b.File
		}
		switch {
		case a.Start < b.Start:
			return -1
		case a.Start > b.Start:
			return 1
		}
		return 0
	})
	var out []Window
	for _, w := range wins {
		if n := len(out); n > 0 && out[n-1].File == w.File && w.Start <= out[n-1].End {
			out[n-1].End = math.Max(out[n-1].End, w.End)
			out[n-1].Gaps += w.Gaps
			continue
		}
		out = append(out, w)
	}
	return out
}

// ClipPart is the share of one chapter's audio a window takes: chapter-relative
// seconds.
type ClipPart struct {
	Chapter  int
	From, To float64
}

// Parts are the chapters' shares of a window, in order: the clip to transcribe
// is their audio joined (the chapters of one file are contiguous).
func (tl Timeline) Parts(w Window) []ClipPart {
	var out []ClipPart
	for _, c := range tl.Chapters {
		if c.File != w.File || c.End <= w.Start || c.Start >= w.End {
			continue
		}
		out = append(out, ClipPart{Chapter: c.Chapter, From: math.Max(w.Start, c.Start) - c.Start, To: math.Min(w.End, c.End) - c.Start})
	}
	return out
}
