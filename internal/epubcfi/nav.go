package epubcfi

import (
	"errors"
	"slices"
	"unicode/utf16"
)

// slotKind is what one entry of indexChildNodes holds.
type slotKind int

const (
	slotNode   slotKind = iota // a child (an element, or a text chunk)
	slotNull                   // the empty chunk between two adjacent elements
	slotBefore                 // the virtual position before the first chunk
	slotAfter                  // the virtual position after the last
	slotFirst                  // the missing chunk before a first element
	slotLast                   // the missing chunk after a last element
)

type slot struct {
	kind slotKind
	node *Node
}

// indexChildNodes ports foliate's: the children arranged as [chunk, element,
// chunk, ..., element, chunk] with the virtual before/after, so elements sit at
// even indices and character data at odd ones. Adjacent character data is one
// node in this DOM, so a chunk is one node.
func indexChildNodes(n *Node) []slot {
	var nodes []slot
	for _, c := range n.Children {
		if len(nodes) == 0 {
			nodes = append(nodes, slot{node: c})
			continue
		}
		last := nodes[len(nodes)-1]
		if c.Type == TextNode {
			nodes = append(nodes, slot{node: c})
			continue
		}
		if last.kind == slotNode && last.node.Type == ElementNode {
			nodes = append(nodes, slot{kind: slotNull})
		}
		nodes = append(nodes, slot{node: c})
	}
	if len(nodes) > 0 && nodes[0].node.Type == ElementNode {
		nodes = append([]slot{{kind: slotFirst}}, nodes...)
	}
	if k := len(nodes); k > 0 && nodes[k-1].kind == slotNode && nodes[k-1].node.Type == ElementNode {
		nodes = append(nodes, slot{kind: slotLast})
	}
	nodes = append([]slot{{kind: slotBefore}}, nodes...)
	return append(nodes, slot{kind: slotAfter})
}

// slotIndex is node's index in indexChildNodes(parent), -1 when it isn't a child,
// counted without building the list: a CFI per sentence over a long section's
// body would otherwise rebuild it once per sentence end.
func slotIndex(parent, node *Node) int {
	i := 1 // slotBefore
	for k, c := range parent.Children {
		if k == 0 {
			if c.Type == ElementNode {
				i++ // slotFirst
			}
		} else if c.Type != TextNode && parent.Children[k-1].Type == ElementNode {
			i++ // slotNull
		}
		if c == node {
			return i
		}
		i++
	}
	return -1
}

// nodeToParts ports foliate's: the steps from the document element down to node,
// offset (a text node's, UTF-16) on the last.
func nodeToParts(node *Node, offset *int) Path {
	parent := node.Parent
	part := Part{ID: node.ID(), Index: slotIndex(parent, node), Offset: offset}
	var out Path
	if parent != node.doc.Root && parent.Type != DocumentNode {
		out = append(nodeToParts(parent, nil), part)
	} else {
		out = Path{part}
	}
	return slices.DeleteFunc(out, func(p Part) bool { return p.Index == -1 })
}

// FromRange is the CFI of the range from (start, startOff) to (end, endOff): text
// nodes and UTF-16 offsets, as foliate's fromRange (no filter). A collapsed range
// is a plain CFI.
func FromRange(start *Node, startOff int, end *Node, endOff int) string {
	s := nodeToParts(start, &startOff)
	if start == end && startOff == endOff {
		return CFI{Paths: []Path{s}}.String()
	}
	e := nodeToParts(end, &endOff)
	return buildRange([]Path{s}, []Path{e})
}

// FromElements is the CFIs of sorted elements of one parent (foliate's
// fromElements: the OPF's itemrefs give the spine's section CFIs).
func FromElements(elements []*Node) []string {
	if len(elements) == 0 {
		return nil
	}
	parent := elements[0].Parent
	var parts Path
	if parent.Type != DocumentNode {
		parts = nodeToParts(parent, nil)
	}
	var out []string
	for index, s := range indexChildNodes(parent) {
		if len(out) == len(elements) {
			break
		}
		if el := elements[len(out)]; s.kind == slotNode && s.node == el {
			p := append(clonePath(parts), Part{ID: el.ID(), Index: index})
			out = append(out, CFI{Paths: []Path{p}}.String())
		}
	}
	return out
}

// Boundary is one end of a Range: a container and an offset (UTF-16 in a text
// node, a child index in an element), as a DOM Range's.
type Boundary struct {
	Node   *Node
	Offset int
}

// Range is a DOM range over a Document.
type Range struct {
	Start, End Boundary
}

// errUnresolved is a CFI whose steps lead nowhere in the document.
var errUnresolved = errors.New("the CFI doesn't resolve in the document")

// boundary is partsToNode's answer.
type boundary struct {
	node          *Node
	offset        int
	before, after bool
}

// partsToNode ports foliate's (no filter).
func partsToNode(root *Node, parts Path) (boundary, error) {
	if len(parts) == 0 {
		return boundary{}, errUnresolved
	}
	if id := parts[len(parts)-1].ID; id != "" {
		if el := root.doc.GetElementByID(id); el != nil {
			return boundary{node: el}, nil
		}
	}
	node := root
	for _, p := range parts {
		if node == nil {
			return boundary{}, errUnresolved
		}
		idx := indexChildNodes(node)
		if p.Index < 0 || p.Index >= len(idx) {
			node = nil
			continue
		}
		s := idx[p.Index]
		switch s.kind {
		case slotFirst:
			if len(node.Children) > 0 {
				return boundary{node: node.Children[0]}, nil
			}
			return boundary{node: node}, nil
		case slotLast:
			if len(node.Children) > 0 {
				return boundary{node: node.Children[len(node.Children)-1]}, nil
			}
			return boundary{node: node}, nil
		case slotBefore:
			return boundary{node: node, before: true}, nil
		case slotAfter:
			return boundary{node: node, after: true}, nil
		case slotNull:
			node = nil
		default:
			node = s.node
		}
	}
	if node == nil {
		return boundary{}, errUnresolved
	}
	off := 0
	if o := parts[len(parts)-1].Offset; o != nil {
		off = *o
	}
	return boundary{node: node, offset: off}, nil
}

func childIndex(n *Node) int {
	return slices.Index(n.Parent.Children, n)
}

func (b boundary) point() Boundary {
	switch {
	case b.before:
		return Boundary{Node: b.node.Parent, Offset: childIndex(b.node)}
	case b.after:
		return Boundary{Node: b.node.Parent, Offset: childIndex(b.node) + 1}
	}
	return Boundary{Node: b.node, Offset: b.offset}
}

// ToRange resolves c in doc as foliate's toRange: c's first path is the
// document's (the package step already removed).
func ToRange(doc *Document, c CFI) (Range, error) {
	sp, ep := c.Collapse(false), c.Collapse(true)
	if len(sp) == 0 || len(ep) == 0 {
		return Range{}, errUnresolved
	}
	s, err := partsToNode(doc.Root, sp[0])
	if err != nil {
		return Range{}, err
	}
	e, err := partsToNode(doc.Root, ep[0])
	if err != nil {
		return Range{}, err
	}
	return Range{Start: s.point(), End: e.point()}, nil
}

// Resolve resolves a whole-book CFI (its package step first, as the map's are)
// in doc, the section it names.
func Resolve(doc *Document, cfi string) (Range, error) {
	c, err := Parse(cfi)
	if err != nil {
		return Range{}, err
	}
	if c.IsRange {
		if len(c.Parent) == 0 {
			return Range{}, errUnresolved
		}
		c.Parent = c.Parent[1:]
	} else {
		if len(c.Paths) == 0 {
			return Range{}, errUnresolved
		}
		c.Paths = c.Paths[1:]
	}
	return ToRange(doc, c)
}

// positions is every text node's place in the document's character data (UTF-16,
// all of it in tree order), and each element's at its start and end.
type positions struct {
	text       []uint16
	textStart  map[*Node]int
	open, shut map[*Node]int
}

func positionsOf(doc *Document) positions {
	p := positions{textStart: map[*Node]int{}, open: map[*Node]int{}, shut: map[*Node]int{}}
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Type == TextNode {
			p.textStart[n] = len(p.text)
			p.text = append(p.text, utf16.Encode([]rune(n.Data))...)
			return
		}
		p.open[n] = len(p.text)
		for _, c := range n.Children {
			walk(c)
		}
		p.shut[n] = len(p.text)
	}
	walk(doc.Node)
	return p
}

func (p positions) at(b Boundary) int {
	if b.Node == nil {
		return 0
	}
	if b.Node.Type == TextNode {
		return p.textStart[b.Node] + min(max(b.Offset, 0), b.Node.Len16())
	}
	if b.Offset < len(b.Node.Children) && b.Offset >= 0 {
		c := b.Node.Children[b.Offset]
		if c.Type == TextNode {
			return p.textStart[c]
		}
		return p.open[c]
	}
	return p.shut[b.Node]
}

// Text is the range's text, as a DOM Range's toString: every text node's data
// within it, in tree order.
func (r Range) Text(doc *Document) string {
	p := positionsOf(doc)
	s, e := p.at(r.Start), p.at(r.End)
	if e <= s {
		return ""
	}
	return string(utf16.Decode(p.text[s:e]))
}
