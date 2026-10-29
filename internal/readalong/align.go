package readalong

import (
	"math"
	"slices"
	"sort"
	"strings"
)

// The aligner: a port of the spike's whisper_align.py
// (.readalong-spike/tools/align/whisper_align.py), which READ-ALONG-RESEARCH.md
// section 4 describes. Both texts become word tokens; word 5-grams unique in both
// vote, per section, for where the section sits in the narration (RANSAC on the
// offset, then the longest increasing run, iterated so a section read in two
// places makes two units); overlapping claims go to the stronger unit; a
// Needleman-Wunsch pass with fuzzy and 2:1 merge moves fills between the anchors
// and at the section edges; a sentence's time is its matched words' first start
// to last end.

// gramN is the anchor n-gram length.
const gramN = 5

// maxDPCells bounds one Needleman-Wunsch matrix (the spike's 4,000,000).
const maxDPCells = 4_000_000

// Word is one recognised word on the whole-book timeline (seconds).
type Word struct {
	W          string
	Start, End float64
}

// TextSection is one spine item's walked text to align (epubcfi.WalkText).
type TextSection struct {
	Index int
	// Href is the map's href: the item's path relative to the OPF's folder.
	Href   string
	Text   []rune
	Breaks []int
}

// Span is an aligned sentence: its rune span in the section's text and its
// whole-book times.
type Span struct {
	Start, End      int
	T0, T1          float64
	Matched, Tokens int
}

// Gap is a sentence the aligner could not place.
type Gap struct{ Start, End int }

// SectionResult is one section's sentences.
type SectionResult struct {
	Index      int
	Href       string
	Spans      []Span
	Gaps       []Gap
	Candidates int
	Anchors    int
	Units      int
}

// Result is an alignment: the sections in spine order and counts.
type Result struct {
	Sections         []SectionResult
	BookTokens       int
	ASRTokens        int
	MatchedTokens    int
	Sentences        int
	AlignedSentences int
	// DPSkipped counts gaps between anchors too big to fill (left unmatched).
	DPSkipped int
}

// bookTok and asrTok are the two token streams.
type bookTok struct {
	id         int32
	start, end int
}

type asrTok struct {
	id     int32
	t0, t1 float64
}

// aligner holds the interned vocabulary and the fuzzy-match cache.
type aligner struct {
	ids   map[string]int32
	words []string
	runes [][]rune
	fuzzy map[int64]bool
	B     []bookTok
	T     []asrTok
}

func (a *aligner) intern(s string) int32 {
	if id, ok := a.ids[s]; ok {
		return id
	}
	id := int32(len(a.words))
	a.ids[s] = id
	a.words = append(a.words, s)
	a.runes = append(a.runes, []rune(s))
	return id
}

// sim is the spike's word score: 2 equal, 1 a close spelling (ratio >= 0.75, both
// at least three letters), else -1.
func (a *aligner) sim(b, t int32) int32 {
	if b == t {
		return 2
	}
	rb, rt := a.runes[b], a.runes[t]
	if len(rb) < 3 || len(rt) < 3 {
		return -1
	}
	// ratio can't reach 0.75 when the lengths are too far apart.
	if 2.0*float64(min(len(rb), len(rt)))/float64(len(rb)+len(rt)) < 0.75 {
		return -1
	}
	key := int64(b)<<32 | int64(t)
	close, ok := a.fuzzy[key]
	if !ok {
		close = ratio(rb, rt) >= 0.75
		a.fuzzy[key] = close
	}
	if close {
		return 1
	}
	return -1
}

// ASR tokens: a word's tokens share its time evenly.
func (a *aligner) addWords(words []Word) {
	for _, w := range words {
		toks := Tokens([]rune(w.W))
		if len(toks) == 0 {
			continue
		}
		dur := (w.End - w.Start) / float64(len(toks))
		for q, tk := range toks {
			t0 := w.Start + float64(q)*dur
			a.T = append(a.T, asrTok{id: a.intern(tk.Norm), t0: t0, t1: t0 + dur})
		}
	}
}

type secInfo struct {
	TextSection
	b0, b1     int
	candidates int
	anchors    int
	units      int
	// spans are the live units' book ranges: a sentence's times run on from the
	// sentence before only inside one unit (one continuous reading).
	spans []unitSpan
}

type unitSpan struct{ b0, b1, id int }

// unitOf is the live unit whose book range holds token i, -1 for none.
func (s *secInfo) unitOf(i int) int {
	for _, u := range s.spans {
		if u.b0 <= i && i < u.b1 {
			return u.id
		}
	}
	return -1
}

type pair struct{ i, j int }

type unit struct {
	sec      *secInfo
	b0, b1   int
	anchors  []pair
	tLo, tHi int
}

// Align aligns the sections' text to the words (whole-book times, in time order).
// Sections with no text or not linear are left out, as the spike did.
func Align(sections []TextSection, words []Word) Result {
	a := &aligner{ids: map[string]int32{}, fuzzy: map[int64]bool{}}
	a.addWords(words)
	var secs []*secInfo
	for _, s := range sections {
		if strings.TrimSpace(string(s.Text)) == "" {
			continue
		}
		toks := Tokens(s.Text)
		si := &secInfo{TextSection: s, b0: len(a.B)}
		for _, tk := range toks {
			a.B = append(a.B, bookTok{id: a.intern(tk.Norm), start: tk.Start, end: tk.End})
		}
		si.b1 = len(a.B)
		secs = append(secs, si)
	}
	cand := a.candidates()
	var units []*unit
	for _, sec := range secs {
		units = append(units, a.place(sec, cand)...)
	}
	live := resolve(units)
	for id, u := range live {
		u.sec.anchors += len(u.anchors)
		u.sec.units++
		u.sec.spans = append(u.sec.spans, unitSpan{u.b0, u.b1, id})
	}
	sort.SliceStable(live, func(x, y int) bool { return live[x].anchors[0].j < live[y].anchors[0].j })
	for k, u := range live {
		u.tLo, u.tHi = 0, len(a.T)
		if k > 0 {
			u.tLo = live[k-1].anchors[len(live[k-1].anchors)-1].j + 1
		}
		if k+1 < len(live) {
			u.tHi = live[k+1].anchors[0].j
		}
	}
	match, skipped := a.fill(live)
	res := Result{BookTokens: len(a.B), ASRTokens: len(a.T), MatchedTokens: len(match), DPSkipped: skipped}
	for _, sec := range secs {
		sr := a.sentences(sec, match)
		res.Sentences += len(sr.Spans) + len(sr.Gaps)
		res.AlignedSentences += len(sr.Spans)
		res.Sections = append(res.Sections, sr)
	}
	return res
}

// candidates are the (book, asr) token positions of every word 5-gram that occurs
// exactly once in each text, sorted.
func (a *aligner) candidates() []pair {
	type gram [gramN]int32
	grams := func(n int, id func(int) int32) map[gram][]int {
		out := map[gram][]int{}
		for k := 0; k+gramN <= n; k++ {
			var g gram
			for q := range gramN {
				g[q] = id(k + q)
			}
			if len(out[g]) < 2 {
				out[g] = append(out[g], k)
			}
		}
		return out
	}
	gB := grams(len(a.B), func(k int) int32 { return a.B[k].id })
	gT := grams(len(a.T), func(k int) int32 { return a.T[k].id })
	var cand []pair
	for g, v := range gB {
		if t := gT[g]; len(v) == 1 && len(t) == 1 {
			cand = append(cand, pair{v[0], t[0]})
		}
	}
	slices.SortFunc(cand, func(x, y pair) int {
		if x.i != y.i {
			return x.i - y.i
		}
		return x.j - y.j
	})
	return cand
}

// floorDiv is Python's // for ints.
func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// place finds where one section sits in the narration: up to eight clusters of
// anchors (RANSAC on d = j - i in a window of 2W, then the longest increasing
// run), kept when they don't overlap in book order, each a unit over its share of
// the section.
func (a *aligner) place(sec *secInfo, cand []pair) []*unit {
	lo := sort.Search(len(cand), func(k int) bool { return cand[k].i >= sec.b0 })
	hi := sort.Search(len(cand), func(k int) bool { return cand[k].i >= sec.b1 })
	remaining := cand[lo:hi]
	sec.candidates = len(remaining)
	nB := sec.b1 - sec.b0
	w := max(40, int(0.08*float64(nB)))
	firstNeed := math.Max(3, 0.02*float64(nB))
	laterNeed := math.Max(20, 0.02*float64(nB))
	var clusters [][]pair
	for range 8 {
		if len(remaining) < 3 {
			break
		}
		ds := make([]int, len(remaining))
		for k, p := range remaining {
			ds[k] = p.j - p.i
		}
		slices.Sort(ds)
		best, bd, l := 0, 0, 0
		for h := range ds {
			for ds[h]-ds[l] > 2*w {
				l++
			}
			if h-l+1 > best {
				best, bd = h-l+1, floorDiv(ds[l]+ds[h], 2)
			}
		}
		var inl, rest []pair
		for _, p := range remaining {
			if d := p.j - p.i - bd; d <= w && d >= -w {
				inl = append(inl, p)
			} else {
				rest = append(rest, p)
			}
		}
		anc := lis(inl)
		need := firstNeed
		if len(clusters) > 0 {
			need = laterNeed
		}
		if float64(len(anc)) < need {
			break
		}
		clusters = append(clusters, anc)
		remaining = rest
	}
	// Larger clusters first; a cluster loses its anchors inside a kept one's book
	// range. (The spike's condition for a second cluster read as always true, a
	// precedence slip; this keeps it to its intended threshold.)
	order := make([][]pair, len(clusters))
	copy(order, clusters)
	sort.SliceStable(order, func(x, y int) bool { return len(order[x]) > len(order[y]) })
	var kept [][]pair
	for _, c := range order {
		var c2 []pair
		for _, p := range c {
			inside := false
			for _, k := range kept {
				if k[0].i <= p.i && p.i <= k[len(k)-1].i {
					inside = true
					break
				}
			}
			if !inside {
				c2 = append(c2, p)
			}
		}
		need := firstNeed
		if len(kept) > 0 {
			need = laterNeed
		}
		if len(c2) > 0 && float64(len(c2)) >= need {
			kept = append(kept, c2)
		}
	}
	sort.SliceStable(kept, func(x, y int) bool { return kept[x][0].i < kept[y][0].i })
	out := make([]*unit, 0, len(kept))
	for k, c := range kept {
		b0, b1 := sec.b0, sec.b1
		if k > 0 {
			b0 = (kept[k-1][len(kept[k-1])-1].i+c[0].i)/2 + 1
		}
		if k < len(kept)-1 {
			b1 = (c[len(c)-1].i+kept[k+1][0].i)/2 + 1
		}
		out = append(out, &unit{sec: sec, b0: b0, b1: b1, anchors: c})
	}
	return out
}

// lis is the longest strictly increasing (in j) subsequence of pairs sorted by i.
func lis(pairs []pair) []pair {
	var tails []int
	var tailsIdx []int
	prev := make([]int, len(pairs))
	for k, p := range pairs {
		pos := sort.SearchInts(tails, p.j)
		if pos == len(tails) {
			tails = append(tails, p.j)
			tailsIdx = append(tailsIdx, k)
		} else {
			tails[pos] = p.j
			tailsIdx[pos] = k
		}
		prev[k] = -1
		if pos > 0 {
			prev[k] = tailsIdx[pos-1]
		}
	}
	if len(tailsIdx) == 0 {
		return nil
	}
	var out []pair
	for k := tailsIdx[len(tailsIdx)-1]; k >= 0; k = prev[k] {
		out = append(out, pairs[k])
	}
	slices.Reverse(out)
	return out
}

// resolve settles overlapping claims on the narration: strongest unit first, a
// weaker one loses its anchors inside a stronger one's span and goes when too few
// are left.
func resolve(units []*unit) []*unit {
	sort.SliceStable(units, func(x, y int) bool { return len(units[x].anchors) > len(units[y].anchors) })
	var taken [][2]int
	var live []*unit
	for _, u := range units {
		var keep []pair
		for _, p := range u.anchors {
			inside := false
			for _, t := range taken {
				if t[0] <= p.j && p.j <= t[1] {
					inside = true
					break
				}
			}
			if !inside {
				keep = append(keep, p)
			}
		}
		if float64(len(keep)) < math.Max(3, 0.02*float64(u.b1-u.b0)) {
			continue
		}
		u.anchors = keep
		live = append(live, u)
		taken = append(taken, [2]int{keep[0].j, keep[len(keep)-1].j})
	}
	return live
}

// fill matches tokens between consecutive anchors and at each unit's edges.
func (a *aligner) fill(live []*unit) (map[int]int, int) {
	match := map[int]int{}
	skipped := 0
	setDefault := func(pairs []pair) {
		for _, p := range pairs {
			if _, ok := match[p.i]; !ok {
				match[p.i] = p.j
			}
		}
	}
	for _, u := range live {
		anc := u.anchors
		for _, p := range anc {
			match[p.i] = p.j
		}
		for k := 0; k+1 < len(anc); k++ {
			pa, pb := anc[k], anc[k+1]
			if pb.i-pa.i > 1 && pb.j-pa.j > 1 {
				r, ok := a.nw(pa.i+1, pb.i, pa.j+1, pb.j, false, false)
				if !ok {
					skipped++
					continue
				}
				setDefault(r)
			}
		}
		first, last := anc[0], anc[len(anc)-1]
		if lead := first.i - u.b0; lead > 0 {
			lo := max(u.tLo, first.j-int(float64(lead)*1.5)-20)
			r, _ := a.nw(u.b0, first.i, lo, first.j, true, false)
			setDefault(r)
		}
		if tail := u.b1 - 1 - last.i; tail > 0 {
			hi := min(u.tHi, last.j+1+int(float64(tail)*1.5)+20)
			r, _ := a.nw(last.i+1, u.b1, last.j+1, hi, false, true)
			setDefault(r)
		}
	}
	return match, skipped
}

// The DP's moves.
const (
	mvUp    = 1 // book token unmatched
	mvLeft  = 2 // asr token unmatched
	mvDiag  = 3
	mvMerge = 4 // two book tokens are one asr token
	mvSplit = 5 // one book token is two asr tokens
)

// nw aligns B[i0:i1] with T[j0:j1] and returns the matched pairs (absolute
// indices), false when the matrix is over maxDPCells. freeLead lets the narration
// start anywhere in the window, freeTail end anywhere.
func (a *aligner) nw(i0, i1, j0, j1 int, freeLead, freeTail bool) ([]pair, bool) {
	n, m := i1-i0, j1-j0
	if n <= 0 || m <= 0 {
		return nil, true
	}
	if n*m > maxDPCells {
		return nil, false
	}
	const gap = -1
	cols := m + 1
	S := make([]int32, (n+1)*cols)
	P := make([]int8, (n+1)*cols) // every cell of S is written below: row 0, column 0, then the rest
	for j := 1; j <= m; j++ {
		if freeLead {
			S[j] = 0
		} else {
			S[j] = int32(gap * j)
		}
		P[j] = mvLeft
	}
	for i := 1; i <= n; i++ {
		S[i*cols] = int32(gap * i)
		P[i*cols] = mvUp
	}
	bt := make([]int32, n)
	for i := range n {
		bt[i] = a.B[i0+i].id
	}
	tt := make([]int32, m)
	for j := range m {
		tt[j] = a.T[j0+j].id
	}
	concat := func(x, y, whole int32) bool {
		sx, sy, sw := a.words[x], a.words[y], a.words[whole]
		return len(sx)+len(sy) == len(sw) && strings.HasPrefix(sw, sx) && strings.HasSuffix(sw, sy)
	}
	for i := 1; i <= n; i++ {
		row, up := i*cols, (i-1)*cols
		b := bt[i-1]
		for j := 1; j <= m; j++ {
			best := S[up+j-1] + a.sim(b, tt[j-1])
			mv := int8(mvDiag)
			if v := S[up+j] + gap; v > best {
				best, mv = v, mvUp
			}
			if v := S[row+j-1] + gap; v > best {
				best, mv = v, mvLeft
			}
			if i >= 2 && concat(bt[i-2], b, tt[j-1]) {
				if v := S[(i-2)*cols+j-1] + 3; v > best {
					best, mv = v, mvMerge
				}
			}
			if j >= 2 && concat(tt[j-2], tt[j-1], b) {
				if v := S[up+j-2] + 3; v > best {
					best, mv = v, mvSplit
				}
			}
			S[row+j] = best
			P[row+j] = mv
		}
	}
	i, j := n, m
	if freeTail {
		j = 0
		for jj := 1; jj <= m; jj++ {
			if S[n*cols+jj] > S[n*cols+j] {
				j = jj
			}
		}
	}
	var pairs []pair
	for i > 0 || j > 0 {
		if i == 0 {
			j--
			continue
		}
		if j == 0 {
			i--
			continue
		}
		switch P[i*cols+j] {
		case mvDiag:
			if a.sim(bt[i-1], tt[j-1]) > 0 {
				pairs = append(pairs, pair{i0 + i - 1, j0 + j - 1})
			}
			i, j = i-1, j-1
		case mvUp:
			i--
		case mvLeft:
			j--
		case mvMerge:
			pairs = append(pairs, pair{i0 + i - 1, j0 + j - 1}, pair{i0 + i - 2, j0 + j - 1})
			i, j = i-2, j-1
		case mvSplit:
			pairs = append(pairs, pair{i0 + i - 1, j0 + j - 1}, pair{i0 + i - 1, j0 + j - 2})
			i, j = i-1, j-2
		}
	}
	return pairs, true
}

// sentences times each sentence of a section from its matched tokens: aligned
// when at least a third of its tokens matched (one for a sentence of one or two),
// first match's start to last match's end, backed off 0.3 s per unmatched word at
// either edge, kept after the previous sentence of the same unit (the spike kept
// it after the previous one of the section, which pushes a section read in two
// places, the later half first, past its own reading), at least 0.2 s long.
func (a *aligner) sentences(sec *secInfo, match map[int]int) SectionResult {
	out := SectionResult{Index: sec.Index, Href: sec.Href, Candidates: sec.candidates, Anchors: sec.anchors, Units: sec.units}
	starts := make([]int, sec.b1-sec.b0)
	for k := range starts {
		starts[k] = a.B[sec.b0+k].start
	}
	havePrev := false
	prevEnd := 0.0
	prevUnit := -1
	for _, s := range SplitSentences(sec.Text, sec.Breaks) {
		cs, ce := s[0], s[1]
		k0 := sec.b0 + sort.SearchInts(starts, cs)
		k1 := sec.b0 + sort.SearchInts(starts, ce)
		ntok := k1 - k0
		if ntok <= 0 {
			continue
		}
		var m []pair
		for k := k0; k < k1; k++ {
			if j, ok := match[k]; ok {
				m = append(m, pair{k, j})
			}
		}
		ok := len(m) >= 1
		if ntok > 2 {
			ok = float64(len(m)) >= math.Max(1, 0.34*float64(ntok))
		}
		if !ok {
			out.Gaps = append(out.Gaps, Gap{cs, ce})
			continue
		}
		first, last := m[0], m[len(m)-1]
		if u := sec.unitOf(first.i); u != prevUnit {
			havePrev, prevUnit = false, u
		}
		t0, t1 := a.T[first.j].t0, a.T[last.j].t1
		if first.i > k0 {
			t0 -= 0.3 * float64(first.i-k0)
			if havePrev {
				t0 = math.Max(t0, prevEnd)
			}
		}
		if last.i < k1-1 {
			t1 += 0.3 * float64(k1-1-last.i)
		}
		if havePrev && t0 < prevEnd {
			t0 = prevEnd
		}
		if t1 <= t0 {
			t1 = t0 + 0.2
		}
		prevEnd, havePrev = t1, true
		out.Spans = append(out.Spans, Span{Start: cs, End: ce, T0: round3(t0), T1: round3(t1), Matched: len(m), Tokens: ntok})
	}
	return out
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
