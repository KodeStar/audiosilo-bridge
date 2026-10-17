// Package chapterdetect finds a chapterless audiobook's chapters in its own
// narration (BRIDGE-PLAN.md Phase 1d). A single audio file with no chapter markers
// is transcribed in long windows; this package reads the words of that transcript
// on the whole-book timeline, with the silences ffmpeg heard, and returns the
// chapters the narrator announces ("Chapter Twelve", "Stave Four. The Last of the
// Spirits", "Day 58", "Epilogue") together with a confidence verdict.
//
// It is pure (no I/O, no clock) so every rule is table-tested. The rules are the
// 2026-10-17 spike's, measured on five real books against their publishers'
// chapter lists:
//
//   - Silence alone can't place chapters: at 2 s it invents dozens of boundaries
//     per book, at 4 s it misses most of them. Silence is the PAUSE TEST a heading
//     must pass, never a detector of its own.
//   - The heading word's start is the best boundary: the publisher's chapter start
//     sat within half a second of it on every book, while the middle of the
//     silence before it was 1.3 to 2.6 s early.
//
// A wrong chapter is a wrong spoiler position (recaps and character reveals are
// gated by chapter), so the verdict errs towards "not confident": the caller parks
// the book for a person rather than guessing.
package chapterdetect

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
)

// Word is one transcribed word on the whole-book timeline (seconds).
type Word struct {
	Text       string
	Start, End float64
}

// Silence is one run of silence ffmpeg heard, on the whole-book timeline.
type Silence = audio.Silence

// Chapter is one detected chapter on the whole-book timeline.
type Chapter struct {
	Title string  `json:"title"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	// Spoken: the title came from a heading in the narration. False for an
	// unheaded "Opening" and for the one chapter of a short book with no heading.
	Spoken bool `json:"spoken"`
	// Heading is the heading as it was heard (the heading word and its number,
	// both headings of a merged pair), HeadingAt where the heading word starts and
	// Pause the pause before it: the evidence a person reads to check a boundary.
	Heading   string  `json:"heading,omitempty"`
	HeadingAt float64 `json:"heading_at,omitempty"`
	Pause     float64 `json:"pause,omitempty"`
}

// Result is a detection: the chapters (contiguous from 0 to the duration) and the
// verdict. Reason says in a few plain words why a detection is not confident.
type Result struct {
	Chapters  []Chapter `json:"chapters"`
	Confident bool      `json:"confident"`
	Reason    string    `json:"reason,omitempty"`
	// Headings is how many headings were kept (after the merge).
	Headings int `json:"headings"`
}

// The rule constants (seconds unless named otherwise), each from the spike.
const (
	// MinPause is the pause a heading must follow. A narrator pauses 2 to 4 s at
	// a chapter (a Podium book 4.1 s at every one); 0.7 s let "dedication" in from
	// the middle of a sentence on The Black Gate.
	MinPause = 1.5
	// silenceBefore / silenceAfter bound where a silence may END around a heading
	// word's start and still be the pause before it: whisper's word timestamps run
	// a little early or late of the audio.
	silenceBefore = 3.0
	silenceAfter  = 0.6
	// mergeWindow: two headings this close are one ("Epilogue. Day 101").
	mergeWindow = 6.0
	// boundaryLead puts the boundary just before the heading word, never before
	// the end of the word before it.
	boundaryLead = 0.2
	// titleGap ends a spoken title: the narrator pauses after "Stave Four. The
	// Last of the Spirits" before the prose starts.
	titleGap = 0.8
	// titleWords bounds how far past the heading word a title may run.
	titleWords = 10
	// maxFrontMatter: audio before the first heading shorter than this (credits,
	// "This is Audible", a dedication) is part of the first chapter; a longer
	// unheaded opening is a chapter of its own.
	maxFrontMatter = 180.0
	// shortBook: a book under an hour with no heading is one chapter, and the
	// confidence checks below apply only from an hour up.
	shortBook = 3600.0
	// longChapterFloor and longChapterFactor: a chapter longer than
	// max(90 min, 3 x the median) means headings were missed.
	longChapterFloor  = 5400.0
	longChapterFactor = 3.0
	// maxNumber bounds a heading's number (digits 1-999).
	maxNumber = 999
)

// OpeningTitle names an unheaded opening longer than maxFrontMatter.
const OpeningTitle = "Opening"

// MaxTitleBytes is the longest chapter title kept: the server's bound on a
// chapters.json title, which every title the Bridge composes keeps to.
const MaxTitleBytes = 200

// numbered heading words take a number after them; alone ones don't.
// "dedication" is deliberately absent: it is matter, and the spike heard it in
// the middle of a sentence.
var (
	numbered = []string{"chapter", "part", "book", "stave", "day", "volume", "episode"}
	alone    = []string{"prologue", "epilogue", "interlude", "preface", "introduction", "afterword", "foreword", "intermission"}
	ordinals = map[string]int{"first": 1, "second": 2, "third": 3, "fourth": 4, "fifth": 5,
		"sixth": 6, "seventh": 7, "eighth": 8, "ninth": 9, "tenth": 10}
)

// candidate is a heading word that passed the pause test.
type candidate struct {
	idx   int    // the heading word's index in words
	kind  string // the heading word, normalized
	num   int    // its number (0 for an alone word)
	used  int    // how many words the number took
	t     float64
	pause float64
}

// Detect finds the chapters in words (sorted by start) with the silences heard
// and the book's duration.
func Detect(words []Word, silences []Silence, duration float64) Result {
	sil := slices.Clone(silences)
	sort.Slice(sil, func(i, j int) bool { return sil[i].End < sil[j].End })
	cands := candidates(words, sil)
	runs := numberedRuns(cands)
	kept := keepHeadings(cands, runs)
	heads := mergeHeadings(words, kept)
	res := Result{Chapters: chaptersFrom(words, heads, duration), Headings: len(heads)}
	res.Confident, res.Reason = verdict(res.Chapters, len(heads), runs, duration)
	return res
}

// norm is a word as the vocabulary compares it: lower case, punctuation trimmed
// from both ends (an inner hyphen or apostrophe stays: "twenty-one", "chapter's").
func norm(s string) string {
	return strings.ToLower(strings.TrimFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }))
}

// candidates are the heading words that follow a pause, a numbered one only with
// its number.
func candidates(words []Word, sil []Silence) []candidate {
	var out []candidate
	for i, w := range words {
		k := norm(w.Text)
		isNumbered, isAlone := slices.Contains(numbered, k), slices.Contains(alone, k)
		if !isNumbered && !isAlone {
			continue
		}
		c := candidate{idx: i, kind: k, t: w.Start}
		if isNumbered {
			c.num, c.used = numberAt(words, i+1)
			if c.num == 0 {
				continue
			}
			// "This is the end of Cosmic Ride, Book 10 of The Messenger", "Book 11 of
			// the series", "Chapter 3 of the report": a number followed by "of" names
			// a place in something else, never a heading (the spike's one invented
			// boundary, in Cosmic Ride's end credits). Only when it runs on: a title
			// that starts with "Of" after a break ("Chapter 3. Of Mice and Men") stays.
			if numberRunsIntoOf(words, i+c.used) {
				continue
			}
		}
		c.pause = pauseBefore(words, sil, i)
		// The start of the book counts as a pause.
		if i > 0 && c.pause < MinPause {
			continue
		}
		out = append(out, c)
	}
	return out
}

// pauseBefore is the pause before words[i]: the gap from the word before it, or
// the longest silence that ends within [start-silenceBefore, start+silenceAfter],
// whichever is longer.
func pauseBefore(words []Word, sil []Silence, i int) float64 {
	t := words[i].Start
	pause := t
	if i > 0 {
		pause = t - words[i-1].End
	}
	from := sort.Search(len(sil), func(j int) bool { return sil[j].End >= t-silenceBefore })
	for j := from; j < len(sil) && sil[j].End <= t+silenceAfter; j++ {
		pause = math.Max(pause, sil[j].End-sil[j].Start)
	}
	return pause
}

// numberAt reads the number that starts at words[i]: digits 1-999, an ordinal
// first..tenth, or a spelled-out cardinal of up to three words ("twenty one",
// "one hundred four"), the longest that parses. It returns the number and how many
// words it took, or 0, 0.
func numberAt(words []Word, i int) (n, used int) {
	if i >= len(words) {
		return 0, 0
	}
	first := norm(words[i].Text)
	if first == "" {
		return 0, 0
	}
	if isDigits(first) {
		v, err := strconv.Atoi(first)
		if err != nil || v < 1 || v > maxNumber {
			return 0, 0
		}
		return v, 1
	}
	if v, ok := ordinals[first]; ok {
		return v, 1
	}
	for size := min(3, len(words)-i); size >= 1; size-- {
		toks := make([]string, size)
		for k := range size {
			toks[k] = norm(words[i+k].Text)
		}
		// "Chapter one and the ..." reads "one and" as 1 too; the number ends at "one".
		if toks[size-1] == "and" {
			continue
		}
		if v, ok := audio.ParseNumberWords(strings.Join(toks, " ")); ok && v >= 1 && v <= maxNumber {
			return v, size
		}
	}
	return 0, 0
}

// numberRunsIntoOf reports whether the number ending at words[last] runs straight
// on into "of": no punctuation after the number and no titleGap pause before it.
func numberRunsIntoOf(words []Word, last int) bool {
	if last+1 >= len(words) || norm(words[last+1].Text) != "of" {
		return false
	}
	num := strings.TrimSpace(words[last].Text)
	r, _ := utf8.DecodeLastRuneInString(num)
	return (unicode.IsLetter(r) || unicode.IsDigit(r)) && words[last+1].Start-words[last].End < titleGap
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// numberedRuns keeps, per numbered heading word, the longest strictly increasing
// run of numbers in time order (numbers may skip: "Day 2, 3, 12, 17"). A stray
// "chapter four" in the prose after a pause falls out of the run; a run of one is
// kept. The runs are keyed by heading word.
func numberedRuns(cands []candidate) map[string][]candidate {
	runs := map[string][]candidate{}
	for _, kind := range numbered {
		var seq []candidate
		for _, c := range cands {
			if c.kind == kind {
				seq = append(seq, c)
			}
		}
		if len(seq) == 0 {
			continue
		}
		// O(n^2) longest increasing subsequence: a book has tens of candidates. The
		// first longest wins, each predecessor the earliest that gives it.
		best, prev := make([]int, len(seq)), make([]int, len(seq))
		top := 0
		for a := range seq {
			best[a], prev[a] = 1, -1
			for b := range a {
				if seq[b].num < seq[a].num && best[b]+1 > best[a] {
					best[a], prev[a] = best[b]+1, b
				}
			}
			if best[a] > best[top] {
				top = a
			}
		}
		var run []candidate
		for j := top; j >= 0; j = prev[j] {
			run = append(run, seq[j])
		}
		slices.Reverse(run)
		runs[kind] = run
	}
	return runs
}

// keepHeadings is every alone candidate and every numbered run's members, in time
// order.
func keepHeadings(cands []candidate, runs map[string][]candidate) []candidate {
	var kept []candidate
	for _, c := range cands {
		if c.num == 0 {
			kept = append(kept, c)
		}
	}
	for _, run := range runs {
		kept = append(kept, run...)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].t < kept[j].t })
	return kept
}

// heading is a kept heading after the merge: its first candidate and its title.
type heading struct {
	candidate
	title   string
	heard   string
	spanEnd int // the last word index the title covers
}

// mergeHeadings folds a heading into the one before it when it starts within
// mergeWindow of it ("Epilogue. Day 101" is one heading): the first is kept and
// the title gets both.
func mergeHeadings(words []Word, kept []candidate) []heading {
	var out []heading
	for _, c := range kept {
		title, end := titleOf(words, c)
		heard := spokenText(words[c.idx : c.idx+c.used+1])
		if n := len(out); n > 0 && c.t-out[n-1].t < mergeWindow {
			last := &out[n-1]
			if c.idx > last.spanEnd {
				last.title = ClipTitle(strings.TrimRight(last.title, ".") + ". " + title)
				last.spanEnd = end
			}
			last.heard += " " + heard
			continue
		}
		out = append(out, heading{candidate: c, title: title, heard: heard, spanEnd: end})
	}
	return out
}

// titleOf is a heading's spoken title: its words up to the first gap of titleGap
// or more within the next titleWords words (never shorter than the heading word
// and its number), else the heading word and its number; trimmed of trailing
// punctuation, an all lower or all upper case title cased word by word. It
// returns the title and the last word index it covers.
func titleOf(words []Word, c candidate) (string, int) {
	end := c.idx + c.used
	for j := end; j <= c.idx+titleWords && j+1 < len(words); j++ {
		if words[j+1].Start-words[j].End >= titleGap {
			end = j
			break
		}
	}
	return ClipTitle(caseTitle(trimPunct(spokenText(words[c.idx : end+1])))), end
}

// spokenText joins words as heard.
func spokenText(ws []Word) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		if t := strings.TrimSpace(w.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// trimPunct trims punctuation, symbols and space from both ends.
func trimPunct(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsSpace(r) })
}

// caseTitle title-cases an all lower or all upper case title word by word
// ("stave two the first of the three spirits" -> "Stave Two The First Of The
// Three Spirits"); a mixed-case one is the narration's transcriber's and is kept.
func caseTitle(s string) string {
	hasUpper, hasLower := false, false
	for _, r := range s {
		hasUpper = hasUpper || unicode.IsUpper(r)
		hasLower = hasLower || unicode.IsLower(r)
	}
	if hasUpper && hasLower {
		return s
	}
	fields := strings.Fields(s)
	for i, f := range fields {
		r, size := utf8.DecodeRuneInString(f)
		fields[i] = string(unicode.ToUpper(r)) + strings.ToLower(f[size:])
	}
	return strings.Join(fields, " ")
}

// ClipTitle bounds a title to MaxTitleBytes, at a rune boundary.
func ClipTitle(s string) string {
	if len(s) <= MaxTitleBytes {
		return s
	}
	cut := MaxTitleBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}

// chaptersFrom lays the headings out as contiguous chapters from 0 to duration.
// Audio before the first heading joins the first chapter when it is short (front
// matter), else it is an "Opening" chapter; credits after the last heading stay in
// the last chapter. A book with no heading is one chapter.
func chaptersFrom(words []Word, heads []heading, duration float64) []Chapter {
	var chs []Chapter
	for _, h := range heads {
		start := h.t - boundaryLead
		if h.idx > 0 {
			start = math.Max(start, words[h.idx-1].End)
		}
		start = math.Max(start, 0)
		if n := len(chs); n > 0 && start <= chs[n-1].Start {
			continue // a boundary must move forward (whisper's times are not strictly increasing)
		}
		chs = append(chs, Chapter{Title: h.title, Start: start, Spoken: true, Heading: h.heard, HeadingAt: h.t,
			Pause: math.Round(h.pause*100) / 100})
	}
	switch {
	case len(chs) == 0:
		chs = []Chapter{{Start: 0}}
	case chs[0].Start < maxFrontMatter:
		chs[0].Start = 0
	default:
		chs = append([]Chapter{{Title: OpeningTitle, Start: 0}}, chs...)
	}
	for i := range chs {
		if i+1 < len(chs) {
			chs[i].End = chs[i+1].Start
		} else {
			chs[i].End = math.Max(duration, chs[i].Start)
		}
	}
	return chs
}

// verdict decides whether a book of an hour or more was chaptered with confidence:
// two headings at least, no chapter so long that headings were plainly missed, and
// a numbered run (other than "day", whose numbers skip by design) that misses no
// more than a quarter of its numbers. A shorter book is always confident.
func verdict(chs []Chapter, headings int, runs map[string][]candidate, duration float64) (bool, string) {
	if duration < shortBook {
		return true, ""
	}
	switch headings {
	case 0:
		return false, "no chapter headings were heard in the narration"
	case 1:
		return false, "only one chapter heading was heard in the narration"
	}
	lengths := make([]float64, len(chs))
	for i, c := range chs {
		lengths[i] = c.End - c.Start
	}
	limit := math.Max(longChapterFloor, longChapterFactor*median(lengths))
	for i, l := range lengths {
		if l > limit {
			return false, fmt.Sprintf("chapter %d runs %s, so headings were probably missed", i+1, spokenDuration(l))
		}
	}
	kind, run := mainRun(runs)
	if kind != "" && kind != "day" && len(run) > 1 {
		first, last := run[0].num, run[len(run)-1].num
		if span := last - first + 1; 4*(span-len(run)) > span {
			return false, fmt.Sprintf("the narration numbers %ss %d to %d but only %d were heard", kind, first, last, len(run))
		}
	}
	return true, ""
}

// mainRun is the numbered heading word with the longest run (the first such in
// the vocabulary's order on a tie).
func mainRun(runs map[string][]candidate) (string, []candidate) {
	var kind string
	for _, k := range numbered {
		if len(runs[k]) > len(runs[kind]) {
			kind = k
		}
	}
	return kind, runs[kind]
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// spokenDuration words a length for a reason: "2 h 40 min", "95 min".
func spokenDuration(sec float64) string {
	mins := int(math.Round(sec / 60))
	if mins < 120 {
		return fmt.Sprintf("%d min", mins)
	}
	if rest := mins % 60; rest != 0 {
		return fmt.Sprintf("%d h %d min", mins/60, rest)
	}
	return fmt.Sprintf("%d h", mins/60)
}
