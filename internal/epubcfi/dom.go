// Package epubcfi is a Go port of foliate-js's epubcfi.js (the reader's vendored
// copy, pin 78914aef: fromRange, toRange, fromElements, parse, compare, joinIndir)
// over a minimal DOM, so the Bridge writes the CFIs the player's reader resolves
// (Bridge Phase 3c, decision 11). It is a leaf: the standard library plus
// golang.org/x/net/html.
//
// The DOM keeps what a browser's would for a CFI: elements (local name, the id
// attribute and the others) and character data. Comments, processing
// instructions and doctypes are dropped, and character data that ends up adjacent
// once they are (text, CDATA) is one text node: foliate's indexChildNodes merges
// adjacent character data into one chunk, so the CFIs are the same. Whitespace
// text is kept. OFFSETS ARE UTF-16 CODE UNITS (a DOM offset), never bytes or
// runes: Node.Len16 and the Range offsets count them.
//
// The golden fixtures (testdata/golden.json) are written by foliate's own
// epubcfi.js on jsdom (scripts/cfi-golden), and the tests hold this port to them
// in both directions.
package epubcfi

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// NodeType is a DOM node type (the DOM's numbers).
type NodeType int

// The node types the DOM keeps.
const (
	ElementNode  NodeType = 1
	TextNode     NodeType = 3
	DocumentNode NodeType = 9
)

// Attr is one attribute of an element (its local name).
type Attr struct {
	Name  string
	Value string
}

// Node is one node of the DOM.
type Node struct {
	Type NodeType
	// Name is an element's local name as written (an HTML parse lowercases it).
	Name string
	// Attrs are an element's attributes; Data is a text node's characters.
	Attrs    []Attr
	Data     string
	Parent   *Node
	Children []*Node
	doc      *Document
}

// Document is a parsed document: Root is its document element.
type Document struct {
	Node *Node
	Root *Node
	ids  map[string]*Node
}

// Attr returns an element's attribute (its local name), "" when it has none.
func (n *Node) Attr(name string) string {
	for _, a := range n.Attrs {
		if a.Name == name {
			return a.Value
		}
	}
	return ""
}

// ID is an element's id attribute ("" for none, and for a text node).
func (n *Node) ID() string {
	if n.Type != ElementNode {
		return ""
	}
	return n.Attr("id")
}

// Len16 is a text node's length in UTF-16 code units (DOM's length).
func (n *Node) Len16() int { return Len16(n.Data) }

// Len16 is s's length in UTF-16 code units.
func Len16(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// ByteOffset16 is the byte offset in s of UTF-16 offset off (clamped to s).
func ByteOffset16(s string, off int) int {
	n := 0
	for i, r := range s {
		if n >= off {
			return i
		}
		n += utf16.RuneLen(r)
	}
	return len(s)
}

// Document is the node's document.
func (n *Node) Document() *Document { return n.doc }

// GetElementByID is the first element in document order with id (nil when none).
func (d *Document) GetElementByID(id string) *Node { return d.ids[id] }

// ElementsByName are the elements with local name (case-insensitively) in
// document order.
func (d *Document) ElementsByName(name string) []*Node {
	var out []*Node
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.Type == ElementNode && strings.EqualFold(n.Name, name) {
			out = append(out, n)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(d.Node)
	return out
}

// builder assembles a document, merging adjacent character data.
type builder struct {
	doc *Document
	cur *Node
}

func newBuilder() *builder {
	d := &Document{ids: map[string]*Node{}}
	d.Node = &Node{Type: DocumentNode, doc: d}
	return &builder{doc: d, cur: d.Node}
}

func (b *builder) open(name string, attrs []Attr) {
	n := &Node{Type: ElementNode, Name: name, Attrs: attrs, Parent: b.cur, doc: b.doc}
	b.cur.Children = append(b.cur.Children, n)
	if b.cur == b.doc.Node && b.doc.Root == nil {
		b.doc.Root = n
	}
	if id := n.Attr("id"); id != "" {
		if _, seen := b.doc.ids[id]; !seen {
			b.doc.ids[id] = n
		}
	}
	b.cur = n
}

func (b *builder) close() {
	if b.cur.Parent != nil {
		b.cur = b.cur.Parent
	}
}

func (b *builder) text(s string) {
	if s == "" || b.cur == b.doc.Node {
		return // character data outside the document element is no node of it
	}
	if k := len(b.cur.Children); k > 0 && b.cur.Children[k-1].Type == TextNode {
		b.cur.Children[k-1].Data += s
		return
	}
	b.cur.Children = append(b.cur.Children, &Node{Type: TextNode, Data: s, Parent: b.cur, doc: b.doc})
}

// ErrNoRoot is a document with no element.
var ErrNoRoot = errors.New("the document has no element")

// ParseXML parses an XML document (an XHTML content document, the OPF, the
// container) as a browser's DOMParser would for application/xhtml+xml or
// application/xml, leniently: encoding/xml non-strict with the HTML entities, so
// an XHTML file that names &nbsp; still parses. Element and attribute names are
// local names.
func ParseXML(raw []byte) (*Document, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	dec.CharsetReader = charset.NewReaderLabel
	b := newBuilder()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			attrs := make([]Attr, 0, len(t.Attr))
			for _, a := range t.Attr {
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
					continue
				}
				attrs = append(attrs, Attr{Name: a.Name.Local, Value: a.Value})
			}
			b.open(t.Name.Local, attrs)
		case xml.EndElement:
			b.close()
		case xml.CharData:
			if utf8.Valid(t) {
				b.text(string(t))
			} else {
				b.text(strings.ToValidUTF8(string(t), "�"))
			}
		}
	}
	if b.doc.Root == nil {
		return nil, ErrNoRoot
	}
	return b.doc, nil
}

// ParseHTML parses a text/html document as a browser's HTML parser would
// (golang.org/x/net/html follows the same tree construction).
func ParseHTML(raw []byte) (*Document, error) {
	root, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse HTML: %w", err)
	}
	b := newBuilder()
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.ElementNode:
				attrs := make([]Attr, 0, len(c.Attr))
				for _, a := range c.Attr {
					attrs = append(attrs, Attr{Name: a.Key, Value: a.Val})
				}
				b.open(c.Data, attrs)
				walk(c)
				b.close()
			case html.TextNode:
				b.text(c.Data)
			}
		}
	}
	walk(root)
	if b.doc.Root == nil {
		return nil, ErrNoRoot
	}
	return b.doc, nil
}

// ParseDocument parses a content document by its media type: text/html with the HTML
// parser, anything else as XML (foliate's rule).
func ParseDocument(raw []byte, mediaType string) (*Document, error) {
	if mediaType == "text/html" {
		return ParseHTML(raw)
	}
	return ParseXML(raw)
}
