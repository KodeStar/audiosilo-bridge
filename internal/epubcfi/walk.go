package epubcfi

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf16"
)

// blockElements start and end a block: their edges are hard sentence breaks
// (the spike's epublib.mjs BLOCK set).
var blockElements = map[string]bool{
	"p": true, "div": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"li": true, "pre": true, "blockquote": true, "br": true, "tr": true, "td": true, "th": true,
	"table": true, "section": true, "article": true, "header": true, "footer": true, "hr": true,
	"dt": true, "dd": true, "figcaption": true, "ul": true, "ol": true, "dl": true,
}

// Walk is a content document's body text as the reader sees it (the spike's
// walkText): every text node under the body in tree order (script and style
// skipped), Text their characters joined, Starts each node's first character in
// Text and Breaks the block edges, sorted and distinct. Offsets here are CODE
// POINTS (runes), the aligner's; CFI converts to the DOM's UTF-16.
type Walk struct {
	Nodes  []*Node
	Starts []int
	Text   []rune
	Breaks []int
}

// Body is the document's body element (the first "body" element), else its
// document element.
func Body(doc *Document) *Node {
	if bs := doc.ElementsByName("body"); len(bs) > 0 {
		return bs[0]
	}
	return doc.Root
}

// WalkText walks doc's body text.
func WalkText(doc *Document) *Walk {
	w := &Walk{}
	seen := map[int]bool{}
	addBreak := func(off int) {
		if !seen[off] {
			seen[off] = true
			w.Breaks = append(w.Breaks, off)
		}
	}
	var visit func(n *Node)
	visit = func(n *Node) {
		if n.Type == TextNode {
			w.Nodes = append(w.Nodes, n)
			w.Starts = append(w.Starts, len(w.Text))
			w.Text = append(w.Text, []rune(n.Data)...)
			return
		}
		if n.Type != ElementNode {
			return
		}
		name := strings.ToLower(n.Name)
		if name == "script" || name == "style" {
			return
		}
		blk := blockElements[name]
		if blk {
			addBreak(len(w.Text))
		}
		for _, c := range n.Children {
			visit(c)
		}
		if blk {
			addBreak(len(w.Text))
		}
	}
	visit(Body(doc))
	sort.Ints(w.Breaks)
	return w
}

// locate is the node holding character off (a start) or off-1 (an end) and the
// UTF-16 offset of off in it (the spike's locate).
func (w *Walk) locate(off int, isEnd bool) (*Node, int) {
	target := off
	if isEnd {
		target = max(off-1, 0)
	}
	ans := sort.Search(len(w.Starts), func(i int) bool { return w.Starts[i] > target }) - 1
	ans = max(ans, 0)
	for ans < len(w.Nodes)-1 && w.Nodes[ans].Data == "" {
		ans++
	}
	n := w.Nodes[ans]
	k := min(max(off-w.Starts[ans], 0), len([]rune(n.Data)))
	return n, Len16(string([]rune(n.Data)[:k]))
}

// ErrNoText is a range over a document with no body text.
var ErrNoText = errors.New("the section has no text")

// CFI is the in-document range CFI of characters [start, end) of the walk (code
// points), foliate's fromRange over the nodes holding them.
func (w *Walk) CFI(start, end int) (string, error) {
	if len(w.Nodes) == 0 {
		return "", ErrNoText
	}
	sn, so := w.locate(start, false)
	en, eo := w.locate(end, true)
	return FromRange(sn, so, en, eo), nil
}

// Offset16 is code point offset off of the walk's text in UTF-16 code units.
func (w *Walk) Offset16(off int) int {
	n := 0
	for _, r := range w.Text[:min(max(off, 0), len(w.Text))] {
		n += utf16.RuneLen(r)
	}
	return n
}

// RuneOffset is the code point offset of UTF-16 offset off16 of the walk's text
// (one inside a surrogate pair counts its code point's start).
func (w *Walk) RuneOffset(off16 int) int {
	n := 0
	for i, r := range w.Text {
		if n >= off16 {
			return i
		}
		n += utf16.RuneLen(r)
	}
	return len(w.Text)
}
