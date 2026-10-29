package readalong

import (
	"fmt"

	"github.com/kodestar/audiosilo-bridge/internal/epubcfi"
)

// Text is an EPUB's walked text: every content document in the spine, linear or
// not (decision 12), with the walk the CFIs are made from.
type Text struct {
	EPUB  *EPUB
	walks map[int]*epubcfi.Walk
	secs  map[int]Section
	order []int
}

// LoadText walks every content document of the EPUB's spine.
func LoadText(e *EPUB) (*Text, error) {
	t := &Text{EPUB: e, walks: map[int]*epubcfi.Walk{}, secs: map[int]Section{}}
	for _, s := range e.Spine {
		if !s.IsContent() {
			continue
		}
		doc, err := e.Document(s)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotEPUB, err)
		}
		t.walks[s.Index] = epubcfi.WalkText(doc)
		t.secs[s.Index] = s
		t.order = append(t.order, s.Index)
	}
	return t, nil
}

// Sections are the sections to align: the linear content documents in spine
// order (the spike left the others out: a linear="no" item is a note or a
// pop-up, not narrated in place).
func (t *Text) Sections() []TextSection {
	var out []TextSection
	for _, idx := range t.order {
		s := t.secs[idx]
		if !s.Linear {
			continue
		}
		w := t.walks[idx]
		out = append(out, TextSection{Index: idx, Href: s.RelHref, Text: w.Text, Breaks: w.Breaks})
	}
	return out
}

// CFI is the full CFI (the section's step, then the in-document range) of rune
// span [start, end) of section index's text.
func (t *Text) CFI(index, start, end int) (string, error) {
	w, ok := t.walks[index]
	if !ok {
		return "", fmt.Errorf("spine item %d has no text", index)
	}
	local, err := w.CFI(start, end)
	if err != nil {
		return "", err
	}
	return epubcfi.JoinIndir(t.secs[index].CFI, local), nil
}

// Resolve resolves a full CFI of section index on a fresh parse, as the reader
// does, and returns the text it covers.
func (t *Text) Resolve(index int, cfi string) (string, error) {
	doc, err := t.EPUB.Document(t.secs[index])
	if err != nil {
		return "", err
	}
	r, err := epubcfi.Resolve(doc, cfi)
	if err != nil {
		return "", err
	}
	return r.Text(doc), nil
}

// Slice is section index's text [start, end).
func (t *Text) Slice(index, start, end int) string {
	return string(t.walks[index].Text[start:end])
}
