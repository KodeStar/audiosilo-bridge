package readalong

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/epubcfi"
)

// The expectations in these tables are the spike's Python (textnorm.py,
// difflib) run on the same input.

func TestTokens(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"In 1805, he paid 21 pounds.", "in eighteen oh five he paid twenty one pounds"},
		{"Café naïve O’Brien’s rock_n roll", "cafe naive obriens rock n roll"},
		{"1900 1066 2024 1999 1909 0 007 1234567 12345678",
			"one thousand nine hundred one thousand sixty six two thousand twenty four nineteen ninety nine " +
				"nineteen oh nine zero seven 1234567 12345678"},
		{"ﬁne Ⅻ x²", "fine xii x2"},
		{"don't - 'quoted' it's", "dont quoted its"},
		{"   ", ""},
	}
	for _, c := range cases {
		var got []string
		for _, tk := range Tokens([]rune(c.in)) {
			got = append(got, tk.Norm)
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("Tokens(%q) = %q, want %q", c.in, strings.Join(got, " "), c.want)
		}
	}
	// A number's words share its span; offsets are runes.
	toks := Tokens([]rune("é 1805"))
	if len(toks) != 4 || toks[0] != (Token{"e", 0, 1}) || toks[1] != (Token{"eighteen", 2, 6}) || toks[3].End != 6 {
		t.Fatalf("offsets: %+v", toks)
	}
}

// The spelled-out numbers are the words the chapter detector reads back
// (audio.ParseNumberWords' vocabulary).
func TestNumberWordsRoundTrip(t *testing.T) {
	for n := 0; n < 1_000_000; n += 37 {
		s := fmt.Sprint(n)
		if len(s) == 4 && n >= 1100 && n <= 1999 && n%100 != 0 {
			continue // years read in pairs
		}
		words := strings.Join(NumberWords(s), " ")
		if got, ok := audio.ParseNumberWords(words); !ok || got != n {
			t.Fatalf("%d -> %q -> %d, %v", n, words, got, ok)
		}
	}
}

func TestSplitSentences(t *testing.T) {
	cases := []struct {
		in     string
		breaks []int
		want   []string
	}{
		{`Mr. Smith went home. He slept! "Why?" she asked. 3 cats. e.g. this. J. R. Smith.`, nil,
			[]string{"Mr. Smith went home.", "He slept!", `"Why?" she asked.`, "3 cats. e.g. this.", "J. R. Smith."}},
		{"A first. A second", []int{9}, []string{"A first.", "A second"}},
		{"Is it... Yes. no stop. “Quoted.” Next…  Done", nil, []string{"Is it...", "Yes. no stop.", "“Quoted.”", "Next…", "Done"}},
		{"Hi.)  (Next one.", nil, []string{"Hi.)", "(Next one."}},
		{"One two. Three. -- ... ", []int{0, 4}, []string{"One", "two.", "Three. -- ..."}},
	}
	for _, c := range cases {
		text := []rune(c.in)
		var got []string
		for _, s := range SplitSentences(text, c.breaks) {
			got = append(got, string(text[s[0]:s[1]]))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("SplitSentences(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRatio(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"colour", "color", 0.9090909090909091},
		{"abcd", "bcde", 0.75},
		{"night", "knight", 0.9090909090909091},
		{"aaab", "abaa", 0.5},
		{"wonderland", "wonderlands", 0.9523809523809523},
		{"xyz", "abc", 0},
		{"katie", "kate", 0.8888888888888888},
		{"abcabc", "cbacba", 0.5},
	}
	for _, c := range cases {
		if got := ratio([]rune(c.a), []rune(c.b)); got != c.want {
			t.Errorf("ratio(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// words makes ASR words from text, one a second from start.
func words(text string, start float64) []Word {
	var out []Word
	for k, w := range strings.Fields(text) {
		t0 := start + float64(k)
		out = append(out, Word{W: " " + w, Start: t0, End: t0 + 0.8})
	}
	return out
}

func section(index int, text string) TextSection {
	r := []rune(text)
	return TextSection{Index: index, Href: fmt.Sprintf("s%d.xhtml", index), Text: r}
}

// lorem is distinct-enough prose: every 5-gram unique.
func lorem(seed, n int) string {
	var b strings.Builder
	for k := range n {
		fmt.Fprintf(&b, "W%dx%d ", seed, k)
		if k%9 == 8 {
			b.WriteString("and then. ")
		}
	}
	return b.String()
}

func TestAlignExactAndOrderFree(t *testing.T) {
	a := section(1, "The cat sat on the mat. It was a fine day for a cat to sit. Then the dog came by and barked loudly at it.")
	b := section(2, "Chapter two begins here with some words. The weather turned cold and the river froze over in the night.")
	// The narration reads section 2 first, then section 1.
	w := append(words(string(b.Text), 0), words(string(a.Text), 100)...)
	res := Align([]TextSection{a, b}, w)
	if res.Sentences != 5 || res.AlignedSentences != 5 {
		t.Fatalf("aligned %d of %d", res.AlignedSentences, res.Sentences)
	}
	first := res.Sections[0].Spans[0]
	if first.T0 != 100 || first.T1 != 105.8 {
		t.Fatalf("section 1's first sentence at %v-%v", first.T0, first.T1)
	}
	if got := res.Sections[1].Spans[0]; got.T0 != 0 {
		t.Fatalf("section 2 starts at %v", got.T0)
	}
}

// A section read in two places (a story split over two files) makes two units.
func TestAlignIteratedPlacement(t *testing.T) {
	first, second := lorem(1, 300), lorem(2, 300)
	sec := section(0, first+second)
	w := append(words(second, 0), words(lorem(9, 200), 400)...)
	w = append(w, words(first, 1000)...)
	res := Align([]TextSection{sec}, w)
	if res.Sections[0].Units != 2 {
		t.Fatalf("units = %d, want 2", res.Sections[0].Units)
	}
	spans := res.Sections[0].Spans
	if len(spans) < 60 || spans[0].T0 < 1000 || spans[len(spans)-1].T0 > 400 {
		t.Fatalf("spans %d, first at %v, last at %v", len(spans), spans[0].T0, spans[len(spans)-1].T0)
	}
}

// Two sections claiming the same narration: the stronger keeps it.
func TestAlignOverlapGoesToTheStronger(t *testing.T) {
	text := lorem(3, 200)
	strong := section(0, text)
	weak := section(1, strings.Join(strings.Fields(text)[:60], " "))
	res := Align([]TextSection{strong, weak}, words(text, 0))
	if len(res.Sections[0].Spans) == 0 || len(res.Sections[1].Spans) != 0 {
		t.Fatalf("strong %d spans, weak %d", len(res.Sections[0].Spans), len(res.Sections[1].Spans))
	}
}

func TestNWMergesAndFuzzy(t *testing.T) {
	a := &aligner{ids: map[string]int32{}, fuzzy: map[int64]bool{}}
	for _, s := range []string{"to", "day", "colour", "x"} {
		a.B = append(a.B, bookTok{id: a.intern(s)})
	}
	for _, s := range []string{"today", "color", "y"} {
		a.T = append(a.T, asrTok{id: a.intern(s)})
	}
	pairs, ok := a.nw(0, 4, 0, 3, false, false)
	if !ok {
		t.Fatal("refused")
	}
	slices.SortFunc(pairs, func(x, y pair) int { return x.i - y.i })
	want := []pair{{0, 0}, {1, 0}, {2, 1}}
	if !slices.Equal(pairs, want) {
		t.Fatalf("pairs %v, want %v", pairs, want)
	}
	// One book word heard as two.
	a.B, a.T = nil, nil
	for _, s := range []string{"someone", "came"} {
		a.B = append(a.B, bookTok{id: a.intern(s)})
	}
	for _, s := range []string{"some", "one", "came"} {
		a.T = append(a.T, asrTok{id: a.intern(s)})
	}
	pairs, _ = a.nw(0, 2, 0, 3, false, false)
	m := map[int]int{}
	for _, p := range pairs {
		if _, ok := m[p.i]; !ok {
			m[p.i] = p.j
		}
	}
	if m[0] != 1 || m[1] != 2 {
		t.Fatalf("split pairs %v", pairs)
	}
	if _, ok := a.nw(0, maxDPCells, 0, 2, false, false); ok {
		t.Fatal("an oversized matrix was filled")
	}
}

func TestAlignBacksOffAndPoorMatch(t *testing.T) {
	text := lorem(4, 120)
	sec := section(0, text)
	res := Align([]TextSection{sec}, words(text, 10))
	if BodyCoverage(res) != 1 {
		t.Fatalf("coverage %v", BodyCoverage(res))
	}
	if res.Sections[0].Spans[0].T0 != 10 {
		t.Fatalf("first span %v", res.Sections[0].Spans[0])
	}
	// Another book's narration lines up nothing.
	res = Align([]TextSection{sec}, words(lorem(5, 120), 0))
	if res.AlignedSentences != 0 || BodyCoverage(res) != 0 {
		t.Fatalf("aligned %d", res.AlignedSentences)
	}
}

func TestBodyCoverageCountsTheReadBody(t *testing.T) {
	res := Result{Sections: []SectionResult{
		{Gaps: []Gap{{0, 100}}}, // front matter, not read
		{Spans: []Span{{Start: 0, End: 50}, {Start: 80, End: 100}}, Gaps: []Gap{{50, 80}}},
		{Gaps: []Gap{{0, 1000}}}, // back matter
	}}
	if got := BodyCoverage(res); got != 0.7 {
		t.Fatalf("body coverage %v, want 0.7", got)
	}
}

func TestComposeTimeline(t *testing.T) {
	files := []AudioFile{{"a.mp3", 100}, {"b.mp3", 50.5}, {"c.mp3", 20}}
	// files style: the manifest's own (drifted) starts are ignored, the server's
	// order and durations used.
	tl, err := Compose(StyleFiles, []ManifestChapter{
		{Chapter: 1, Start: 0, FilePath: "a.mp3"},
		{Chapter: 2, Start: 99.2, FilePath: "c.mp3"},
		{Chapter: 3, Start: 120, FilePath: "b.mp3"},
	}, files)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]float64{}
	for _, c := range tl.Chapters {
		got[c.Chapter] = c.Offset
	}
	if got[1] != 0 || got[3] != 100 || got[2] != 150.5 || tl.Duration() != 170.5 {
		t.Fatalf("offsets %v, duration %v", got, tl.Duration())
	}
	if _, err := Compose(StyleFiles, []ManifestChapter{{Chapter: 1, FilePath: "x.mp3"}}, files); err == nil {
		t.Fatal("a chapter on a file the server doesn't have composed")
	}
	// markers, windows (and detected, a markers manifest): in-file starts.
	one := []AudioFile{{"book.m4b", 3000}}
	for _, style := range []string{StyleMarkers, StyleWindows} {
		tl, err := Compose(style, []ManifestChapter{{Chapter: 2, Start: 1500}, {Chapter: 1, Start: 0}}, one)
		if err != nil {
			t.Fatal(err)
		}
		if tl.Chapters[0].Chapter != 1 || tl.Chapters[1].Offset != 1500 || tl.Chapters[0].End != 1500 || tl.Chapters[1].End != 3000 {
			t.Fatalf("%s: %+v", style, tl.Chapters)
		}
	}
	if _, err := Compose(StyleMarkers, []ManifestChapter{{Chapter: 1}}, files); err == nil {
		t.Fatal("a markers manifest over three files composed")
	}
	if _, err := Compose("ebook", []ManifestChapter{{Chapter: 1}}, one); err == nil {
		t.Fatal("an ebook manifest composed")
	}
	if _, err := Compose(StyleMarkers, []ManifestChapter{{Chapter: 1}}, []AudioFile{{"x", 0}}); err == nil {
		t.Fatal("a file with no length composed")
	}
}

func TestBookWordsSplicesPatches(t *testing.T) {
	tl, err := Compose(StyleMarkers, []ManifestChapter{{Chapter: 1, Start: 0}, {Chapter: 2, Start: 10}}, []AudioFile{{"b.m4b", 20}})
	if err != nil {
		t.Fatal(err)
	}
	w := map[int][]Word{
		1: {{"a", 1, 2}, {"loop", 8, 9}, {"loop", 9, 9.5}},
		2: {{"loop", 0.2, 0.5}, {"b", 3, 4}},
	}
	got := tl.BookWords(w, []Patch{{File: 0, Start: 7.5, End: 10.4, Words: []Word{{"fixed", 8.1, 8.6}, {"words", 9.0, 9.4}}}})
	var s []string
	for _, x := range got {
		s = append(s, fmt.Sprintf("%s@%g", x.W, x.Start))
	}
	if strings.Join(s, " ") != "a@1 fixed@8.1 words@9 b@13" {
		t.Fatalf("words %v", s)
	}
}

func TestHealWindows(t *testing.T) {
	tl, err := Compose(StyleFiles, []ManifestChapter{{Chapter: 1, FilePath: "a"}, {Chapter: 2, FilePath: "b"}},
		[]AudioFile{{"a", 100}, {"b", 100}})
	if err != nil {
		t.Fatal(err)
	}
	res := Result{Sections: []SectionResult{
		{Spans: []Span{{Start: 0, T0: 10, T1: 20}, {Start: 30, T0: 30, T1: 40}, {Start: 60, T0: 41, T1: 42}, {Start: 90, T0: 130, T1: 140}},
			Gaps: []Gap{{Start: 10}, {Start: 20}, {Start: 50}, {Start: 70}, {Start: 80}, {Start: 95}}},
		// a gap between neighbours under 2 s, and one with no neighbour after
		{Spans: []Span{{Start: 0, T0: 150, T1: 151}, {Start: 20, T0: 151.5, T1: 152}}, Gaps: []Gap{{Start: 10}, {Start: 30}}},
	}}
	extra, ok := tl.ChapterWindow(2, 80, 0) // a tail loop: to the chapter's end
	if !ok {
		t.Fatal("no chapter 2")
	}
	got := tl.HealWindows(res, []Window{extra})
	want := []Window{
		{File: 0, Start: 19.5, End: 30.5, Gaps: 2},
		// two runs touching (merged), the second cut at the file's end...
		{File: 0, Start: 39.5, End: 100, Gaps: 3},
		{File: 1, Start: 0, End: 30.5, Gaps: 2}, // ...and the rest in the next file
		{File: 1, Start: 80, End: 100},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("windows\n%+v\nwant\n%+v", got, want)
	}
	if _, ok := tl.ChapterWindow(9, 0, 1); ok {
		t.Fatal("a window on a chapter the book doesn't have")
	}
	parts := tl.Parts(Window{File: 1, Start: 10, End: 20})
	if len(parts) != 1 || parts[0] != (ClipPart{Chapter: 2, From: 10, To: 20}) {
		t.Fatalf("parts %+v", parts)
	}
}

// A section's trailing run of unaligned sentences (nothing aligned after it in the
// section) is healed from its last aligned sentence for as long as its text could
// take, bounded by its chapter's end and the next section's first aligned sentence.
func TestHealWindowsTrailingRun(t *testing.T) {
	tl, err := Compose(StyleFiles, []ManifestChapter{{Chapter: 1, FilePath: "a"}, {Chapter: 2, FilePath: "b"}},
		[]AudioFile{{"a", 1000}, {"b", 1000}})
	if err != nil {
		t.Fatal(err)
	}
	sec := func(t1 float64, gapChars ...int) SectionResult {
		s := SectionResult{Spans: []Span{{Start: 0, End: 10, T0: t1 - 5, T1: t1}}}
		at := 10
		for _, c := range gapChars {
			s.Gaps = append(s.Gaps, Gap{Start: at, End: at + c})
			at += c
		}
		return s
	}
	for _, tc := range []struct {
		name string
		res  Result
		want []Window
	}{
		{"bounded by the text", Result{Sections: []SectionResult{sec(100, 300, 200)}},
			[]Window{{File: 0, Start: 99.5, End: 160, Gaps: 2}}},
		{"bounded by the chapter's end", Result{Sections: []SectionResult{sec(900, 5000)}},
			[]Window{{File: 0, Start: 899.5, End: 1000, Gaps: 1}}},
		{"bounded by the next section's first sentence", Result{Sections: []SectionResult{sec(100, 5000), sec(150)}},
			[]Window{{File: 0, Start: 99.5, End: 145.5, Gaps: 1}}},
		{"never past 600 s", Result{Sections: []SectionResult{sec(10, 9000)}},
			[]Window{{File: 0, Start: 9.5, End: 609.5, Gaps: 1}}},
		// Denied: no text to place, a sentence aligned at the chapter's very end,
		// and a section with nothing aligned at all (no sentence to start from).
		{"no text", Result{Sections: []SectionResult{sec(100, 0)}}, nil},
		{"at the chapter's end", Result{Sections: []SectionResult{sec(1000, 300)}}, nil},
		{"nothing aligned", Result{Sections: []SectionResult{{Gaps: []Gap{{Start: 0, End: 400}}}}}, nil},
	} {
		if got := tl.HealWindows(tc.res, nil); !slices.Equal(got, tc.want) {
			t.Errorf("%s: windows %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// The Call of the Wild's shape (the 3c-1 live run): chapter 1's last 230 s are a
// "buck buck" loop, so the section's last 3101 characters never aligned and the
// next section starts in the next file. The trailing run is healed to the
// chapter's end, and the transcript check's loop range lands on the same window.
func TestHealWindowsLoopedChapterEnd(t *testing.T) {
	tl, err := Compose(StyleFiles, []ManifestChapter{{Chapter: 1, FilePath: "01.mp3"}, {Chapter: 2, FilePath: "02.mp3"}},
		[]AudioFile{{"01.mp3", 1510.89}, {"02.mp3", 1400}})
	if err != nil {
		t.Fatal(err)
	}
	res := Result{Sections: []SectionResult{
		{Spans: []Span{{Start: 0, End: 100, T0: 0, T1: 600}, {Start: 100, End: 22000, T0: 600, T1: 1276.42}},
			Gaps: []Gap{{Start: 22000, End: 23500}, {Start: 23500, End: 25101}}},
		{Spans: []Span{{Start: 0, End: 50, T0: 1512, T1: 1520}}},
	}}
	want := []Window{{File: 0, Start: 1275.92, End: 1510.89, Gaps: 2}}
	if got := tl.HealWindows(res, nil); !slices.Equal(got, want) {
		t.Fatalf("windows %+v, want %+v", got, want)
	}
	loop, ok := tl.LoopWindow(1, 1277, 1507)
	if !ok || loop != (Window{File: 0, Start: 1276.5, End: 1507.5}) {
		t.Fatalf("loop window %+v %v", loop, ok)
	}
	if got := tl.HealWindows(res, []Window{loop}); !slices.Equal(got, want) {
		t.Fatalf("with the loop: %+v, want %+v", got, want)
	}
	// Denied: a "loop" over 600 s (a phrase the book repeats), a chapter the book
	// doesn't have, an empty range.
	for _, r := range [][3]float64{{1, 10, 900}, {9, 10, 20}, {1, 30, 30}} {
		if w, ok := tl.LoopWindow(int(r[0]), r[1], r[2]); ok {
			t.Errorf("loop window %v = %+v", r, w)
		}
	}
}

func TestHealWindowsJoinChaptersOfOneFile(t *testing.T) {
	tl, _ := Compose(StyleMarkers, []ManifestChapter{{Chapter: 1, Start: 0}, {Chapter: 2, Start: 50}}, []AudioFile{{"b", 100}})
	parts := tl.Parts(Window{File: 0, Start: 40, End: 60})
	want := []ClipPart{{Chapter: 1, From: 40, To: 50}, {Chapter: 2, From: 0, To: 10}}
	if !slices.Equal(parts, want) {
		t.Fatalf("parts %+v", parts)
	}
}

// energy levels for a pattern: '.' quiet (-80 dB), '#' speech (-20 dB), 10 ms each.
func levels(pattern string) []float64 {
	out := make([]float64, 0, len(pattern))
	for _, c := range pattern {
		if c == '#' {
			out = append(out, -20)
		} else {
			out = append(out, -80)
		}
	}
	return out
}

func TestOnsetKinds(t *testing.T) {
	speech := strings.Repeat("#", 200)
	pause := strings.Repeat(".", 40)
	e := levels(speech + pause + speech + pause)
	// In the pause: the next voiced run.
	if o, k := Onset(e, 2.1); k != OnsetAfterPause || math.Abs(o-2.4) > 1e-9 {
		t.Fatalf("in a pause: %v %s", o, k)
	}
	// On the word tail just before the pause.
	if o, k := Onset(e, 1.9); k != OnsetAfterPause || math.Abs(o-2.4) > 1e-9 {
		t.Fatalf("tail: %v %s", o, k)
	}
	// Just after a pause: the run's start.
	if o, k := Onset(e, 2.6); k != OnsetRunStart || math.Abs(o-2.4) > 1e-9 {
		t.Fatalf("run start: %v %s", o, k)
	}
	// Deep in speech.
	if o, k := Onset(e, 1.0); k != OnsetMidFlow || o != 1.0 {
		t.Fatalf("mid-flow: %v %s", o, k)
	}
	if _, k := Onset(e[:20], 0.1); k != OnsetShort {
		t.Fatalf("short: %s", k)
	}
	if _, k := Onset(levels(strings.Repeat("#", 50)+strings.Repeat(".", 500)), 1.0); k != OnsetNone {
		t.Fatalf("no onset: %s", k)
	}
	pcm := make([]float32, 3*FrameSamples+10)
	for k := range FrameSamples {
		pcm[FrameSamples+k] = 0.5
	}
	if got := Energies(pcm); len(got) != 3 || got[0] != -100 || math.Abs(got[1]-10*math.Log10(0.25+1e-10)) > 1e-9 {
		t.Fatalf("energies %v", got)
	}
}

func TestSnap(t *testing.T) {
	tl, _ := Compose(StyleFiles, []ManifestChapter{{Chapter: 1, FilePath: "a"}, {Chapter: 2, FilePath: "b"}},
		[]AudioFile{{"a", 4.8}, {"b", 10}})
	speech, pause := strings.Repeat("#", 200), strings.Repeat(".", 40)
	energies := map[int][]float64{1: levels(speech + pause + speech + pause), 2: levels(speech + pause + speech + pause)}
	res := Result{Sections: []SectionResult{{Spans: []Span{
		{T0: 2.1, T1: 3},               // in a pause: onset 2.4 - 0.1
		{T0: 4.8 + 1.0, T1: 4.8 + 1.5}, // mid-flow in file 2: kept
		{T0: 4.8 + 2.1, T1: 4.8 + 2.4}, // capped at end - 0.2
	}}}}
	asked := []int{}
	stats, err := Snap(&res, tl, func(ch int) ([]float64, error) { asked = append(asked, ch); return energies[ch], nil })
	if err != nil {
		t.Fatal(err)
	}
	sp := res.Sections[0].Spans
	if sp[0].T0 != 2.3 || sp[1].T0 != 5.8 || sp[2].T0 != 7 || !slices.Equal(asked, []int{1, 2}) {
		t.Fatalf("snapped %+v, asked %v", sp, asked)
	}
	if stats.Kinds[OnsetAfterPause] != 2 || stats.Kinds[OnsetMidFlow] != 1 {
		t.Fatalf("stats %+v", stats)
	}
	// Never earlier than raw - 0.5 s.
	res = Result{Sections: []SectionResult{{Spans: []Span{{T0: 3.0, T1: 5}}}}}
	_, _ = Snap(&res, tl, func(int) ([]float64, error) { return levels(strings.Repeat("#", 50) + strings.Repeat(".", 400)), nil })
	if res.Sections[0].Spans[0].T0 != 3.0 {
		t.Fatalf("no onset moved the start to %v", res.Sections[0].Spans[0].T0)
	}
}

func TestBuildMap(t *testing.T) {
	e, err := OpenEPUB("../epubcfi/testdata/synthetic.epub")
	if err != nil {
		t.Fatal(err)
	}
	text, err := LoadText(e)
	if err != nil {
		t.Fatal(err)
	}
	secs := text.Sections()
	tl, _ := Compose(StyleMarkers, []ManifestChapter{{Chapter: 1}}, []AudioFile{{"Book One.m4b", 60.12345}})
	s0 := secs[0]
	res := Result{Sections: []SectionResult{
		{Index: s0.Index, Href: s0.Href, Spans: []Span{{Start: 0, End: 5, T0: -0.4, T1: 8.004}, {Start: 6, End: 9, T0: 20.006, T1: 20.001}},
			Gaps: []Gap{{Start: 10, End: 14}}},
		{Index: secs[1].Index, Href: secs[1].Href},
	}}
	m, err := Build(res, text, tl, "dir/Book One.epub", Generator{Version: "1.2.3", ASR: "whisper-large-v3-turbo", Now: time.Date(2026, 11, 2, 10, 11, 12, 0, time.FixedZone("x", 3600))})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	dec := json.NewDecoder(bytes.NewReader(raw))
	_, _ = dec.Token()
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		_ = dec.Decode(&skip)
	}
	if strings.Join(keys, ",") != "format,ebook,audio,granularity,generator,generated_at,coverage,sections,audio_gaps" {
		t.Fatalf("keys %v", keys)
	}
	if m.GeneratedAt != "2026-11-02T09:11:12Z" || m.Ebook.File != "Book One.epub" || m.Ebook.SHA256 != e.SHA256 ||
		m.Audio.Duration != 60.12 || m.Audio.Files[0] != (MapFile{"Book One.m4b", 60.123}) {
		t.Fatalf("head %+v", m)
	}
	if len(m.Sections) != 1 || m.Sections[0].Href != s0.Href {
		t.Fatalf("sections %+v (an empty one is left out)", m.Sections)
	}
	sp := m.Sections[0].Spans
	if sp[0].Start != 0 || sp[0].End != 8 || sp[1].Start != 20.01 || sp[1].End != 20.01 || m.Sections[0].Gaps[0].Chars != 4 {
		t.Fatalf("spans %+v", m.Sections[0])
	}
	if want := []TimeRange{{8, 20.01}, {20.01, 60.12}}; !slices.Equal(m.AudioGaps, want) {
		t.Fatalf("audio gaps %v", m.AudioGaps)
	}
	if got, err := text.Resolve(s0.Index, sp[0].CFI); err != nil || got != text.Slice(s0.Index, 0, 5) {
		t.Fatalf("CFI %s resolves to %q, %v", sp[0].CFI, got, err)
	}
	if !strings.Contains(string(raw), `"gaps":[`) || strings.Count(string(raw), `"gaps"`) != 1 {
		t.Fatal("gaps are written only where there are some")
	}
	big := Map{Sections: []MapSection{{Href: strings.Repeat("x", MaxMapBytes)}}}
	if _, err := Encode(big); err == nil {
		t.Fatal("a map over 16 MiB encoded")
	}
}

// testdata/alice.readalong.json is the spike's Alice map regenerated in the v1
// format (the parity test's AUDIOSILO_READALONG_MAPS): it decodes, re-encodes to
// the same bytes, and every CFI resolves on the fixture EPUB (the same text).
func TestFixtureMap(t *testing.T) {
	raw, err := os.ReadFile("testdata/alice.readalong.json")
	if err != nil {
		t.Fatal(err)
	}
	var m Map
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	again, err := Encode(m)
	if err != nil || !bytes.Equal(again, raw) {
		t.Fatalf("re-encoded differently: %v", err)
	}
	if m.Format != Format || m.Coverage.Text < 0.98 || len(m.Audio.Files) != 12 || m.Spans() < 1400 {
		t.Fatalf("head %+v, %d spans", m.Coverage, m.Spans())
	}
	e, err := OpenEPUB("../epubcfi/testdata/alice.epub")
	if err != nil {
		t.Fatal(err)
	}
	byHref := map[string]Section{}
	for _, s := range e.Spine {
		byHref[s.RelHref] = s
	}
	for _, sec := range m.Sections {
		s, ok := byHref[sec.Href]
		if !ok {
			t.Fatalf("href %s isn't in the spine", sec.Href)
		}
		doc, err := e.Document(s)
		if err != nil {
			t.Fatal(err)
		}
		for _, sp := range sec.Spans {
			r, err := epubcfi.Resolve(doc, sp.CFI)
			if err != nil || strings.TrimSpace(r.Text(doc)) == "" {
				t.Fatalf("%s: %v", sp.CFI, err)
			}
			if !(sp.Start >= 0 && sp.Start <= sp.End && sp.End <= m.Audio.Duration) {
				t.Fatalf("span times %+v", sp)
			}
		}
	}
}
