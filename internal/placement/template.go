package placement

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Mode selects how SuggestWith computes a destination.
type Mode int

const (
	// ModeAuto reproduces Suggest(): anchor on existing series siblings, falling
	// back to a guessed shortcode. The default.
	ModeAuto Mode = iota
	// ModeTemplate renders a user-supplied path template.
	ModeTemplate
)

// SuggestWith computes a destination via the Auto sibling-matching logic
// (ModeAuto, or an empty template) or by rendering a user template (ModeTemplate).
// Template mode still resolves {SHORTCODE} and the padding width from the siblings,
// so "match the existing convention" is available as values a template can consume.
// Auto mode, and a template that renders the position ({Seq} or {PaddedSeq}),
// drop the book's own position from the front of its titles first
// (dropLeadingSequence); a template without it keeps the number in the title.
func SuggestWith(mode Mode, tmpl string, in Input) (Suggestion, error) {
	dropSeq := func() {
		in.Title, in.TitleShort = dropLeadingSequence(in.Title, in), dropLeadingSequence(in.TitleShort, in)
	}
	if mode == ModeAuto || strings.TrimSpace(tmpl) == "" {
		dropSeq()
		return Suggest(in), nil
	}
	t, err := Parse(tmpl)
	if err != nil {
		return Suggestion{}, err
	}
	if t.uses("Seq", "PaddedSeq") {
		dropSeq()
	}
	rel, err := t.Render(varsFromInput(in))
	if err != nil {
		return Suggestion{}, err
	}
	dir, file := path.Split(rel)
	return Suggestion{
		RelPath:  strings.TrimSuffix(dir, "/"),
		Filename: file,
		Kind:     KindFor(in),
		Note:     "rendered from the library template",
	}, nil
}

// KindFor is the Kind a suggestion for in has: standalone without a series,
// else series_existing when the library has siblings, else series_new.
func KindFor(in Input) Kind {
	switch {
	case in.SeriesName == "":
		return Standalone
	case len(in.Siblings) > 0:
		return SeriesExisting
	default:
		return SeriesNew
	}
}

// Vars is the resolved variable set for one book, consumed by a Template.
type Vars struct {
	Author, Series, Title, TitleShort, Narrator, ASIN, Year string
	Shortcode                                               string
	Sequence                                                float64
	HasSequence                                             bool
	PadWidth                                                int // {PaddedSeq} default width
	Filename                                                string
}

// varsFromInput extracts template variables from an Input, resolving the shortcode
// and padding width from the series siblings (reusing the Auto-mode primitives).
func varsFromInput(in Input) Vars {
	title := in.Title
	if bareTitle(title) && in.SeriesName != "" {
		title = seriesTitle(in) // "He Who Fights with Monsters 13", not "13"
	}
	short := strings.TrimSpace(in.TitleShort)
	if short == "" || bareTitle(short) {
		short = title
	}
	code, width := resolveShortcode(in)
	return Vars{
		Author:      in.Author,
		Series:      in.SeriesName,
		Title:       title,
		TitleShort:  short,
		Narrator:    in.Narrator,
		ASIN:        in.ASIN,
		Year:        in.Year,
		Shortcode:   code,
		Sequence:    in.Sequence,
		HasSequence: in.HasSequence,
		PadWidth:    width,
		Filename:    in.Filename,
	}
}

// resolveShortcode derives the series shortcode and padding width: from the
// highest sibling whose code parses, else a guess from the series name (width 2).
func resolveShortcode(in Input) (code string, width int) {
	if in.SeriesName == "" {
		return "", 2
	}
	if len(in.Siblings) > 0 {
		if tmpl, ok := pickTemplate(in.Siblings); ok {
			if prefix, w, parsed := parseCode(leafCode(lastSegment(tmpl.RelPath))); parsed {
				return prefix, w
			}
		}
	}
	return guessShortcode(in.SeriesName), 2
}

// knownVars is the variable palette; Parse rejects any other name.
var knownVars = map[string]bool{
	"Author": true, "Series": true, "Title": true, "TitleShort": true,
	"Seq": true, "PaddedSeq": true, "SHORTCODE": true, "ASIN": true,
	"Narrator": true, "Year": true, "OriginalFilename": true, "Ext": true,
}

// Template is a compiled path template. Syntax: literal text, `{Var}` / `{Var:mods}`
// tokens, and optional groups `{ ... }` that collapse to empty when a variable
// inside renders empty (e.g. `{ {PaddedSeq} - }` vanishes for a standalone). A
// variable token is a `{` immediately followed by a letter-led name and then `:`
// or `}`; anything else after `{` starts an optional group. `\` escapes a brace.
type Template struct {
	nodes []node
}

type node interface {
	// render returns the node's text and whether it contained an empty variable
	// (so an enclosing optional group can decide whether to collapse).
	render(v Vars) (text string, emptyVar bool)
}

type literalNode struct{ text string }

func (n literalNode) render(Vars) (string, bool) { return n.text, false }

type varNode struct {
	name string
	mods modifiers
}

func (n varNode) render(v Vars) (string, bool) {
	val := v.resolve(n.name, n.mods)
	return val, val == ""
}

// groupNode is an optional group: it collapses to "" if any variable inside it
// renders empty.
type groupNode struct{ children []node }

func (n groupNode) render(v Vars) (string, bool) {
	var sb strings.Builder
	anyEmpty := false
	for _, c := range n.children {
		txt, empty := c.render(v)
		if empty {
			anyEmpty = true
		}
		sb.WriteString(txt)
	}
	if anyEmpty {
		return "", false
	}
	return sb.String(), false
}

type modifiers struct {
	width              *int
	upper, lower, trim bool
}

// Parse compiles a path template.
func Parse(src string) (*Template, error) {
	p := &tmplParser{s: src}
	nodes, err := p.parseSeq(false)
	if err != nil {
		return nil, err
	}
	return &Template{nodes: nodes}, nil
}

// uses reports whether the template renders any of the named variables.
func (t *Template) uses(names ...string) bool {
	var walk func([]node) bool
	walk = func(nodes []node) bool {
		for _, n := range nodes {
			switch n := n.(type) {
			case varNode:
				if slices.Contains(names, n.name) {
					return true
				}
			case groupNode:
				if walk(n.children) {
					return true
				}
			}
		}
		return false
	}
	return walk(t.nodes)
}

// Render produces the destination path (folder + filename), sanitizing each path
// segment and dropping any that collapsed to empty.
func (t *Template) Render(v Vars) (string, error) {
	var sb strings.Builder
	for _, n := range t.nodes {
		txt, _ := n.render(v)
		sb.WriteString(txt)
	}
	parts := strings.Split(sb.String(), "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := Sanitize(p); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return "", fmt.Errorf("template rendered an empty path")
	}
	return strings.Join(out, "/"), nil
}

type tmplParser struct {
	s   string
	pos int
}

func (p *tmplParser) parseSeq(inGroup bool) ([]node, error) {
	var nodes []node
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			nodes = append(nodes, literalNode{lit.String()})
			lit.Reset()
		}
	}
	for p.pos < len(p.s) {
		switch c := p.s[p.pos]; c {
		case '\\':
			if p.pos+1 < len(p.s) {
				lit.WriteByte(p.s[p.pos+1])
				p.pos += 2
				continue
			}
			lit.WriteByte('\\')
			p.pos++
		case '}':
			if inGroup {
				flush()
				p.pos++ // consume '}'
				return nodes, nil
			}
			return nil, fmt.Errorf("unexpected '}' at position %d", p.pos)
		case '{':
			vn, isVar, err := p.tryVar()
			if isVar {
				if err != nil {
					return nil, err
				}
				flush()
				nodes = append(nodes, vn)
				continue
			}
			flush()
			p.pos++ // consume '{'
			children, err := p.parseSeq(true)
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, groupNode{children})
		default:
			lit.WriteByte(c)
			p.pos++
		}
	}
	if inGroup {
		return nil, fmt.Errorf("unclosed '{' (optional group)")
	}
	flush()
	return nodes, nil
}

// tryVar inspects a '{' at p.pos. isVar is true when it is syntactically a
// variable token (`{`, a letter-led name, then `:` or `}`); err is non-nil for a
// malformed/unknown variable. When isVar is false it is an optional group and
// p.pos is unchanged. On success (isVar, err==nil) p.pos advances past '}'.
func (p *tmplParser) tryVar() (varNode, bool, error) {
	i := p.pos + 1
	if i >= len(p.s) || !isLetter(p.s[i]) {
		return varNode{}, false, nil
	}
	start := i
	for i < len(p.s) && isIdentChar(p.s[i]) {
		i++
	}
	if i >= len(p.s) || (p.s[i] != '}' && p.s[i] != ':') {
		return varNode{}, false, nil // not closed/modified right after the name → group
	}
	name := p.s[start:i]
	var mods modifiers
	if p.s[i] == ':' {
		j := i + 1
		for j < len(p.s) && p.s[j] != '}' && p.s[j] != '{' {
			j++
		}
		if j >= len(p.s) || p.s[j] != '}' {
			return varNode{name: name}, true, fmt.Errorf("malformed modifier in {%s:...}", name)
		}
		m, err := parseMods(p.s[i+1 : j])
		if err != nil {
			return varNode{name: name}, true, fmt.Errorf("{%s}: %w", name, err)
		}
		mods = m
		i = j
	}
	if !knownVars[name] {
		return varNode{name: name}, true, fmt.Errorf("unknown variable {%s}", name)
	}
	p.pos = i + 1 // consume through '}'
	return varNode{name: name, mods: mods}, true, nil
}

func parseMods(spec string) (modifiers, error) {
	var m modifiers
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, hasVal := strings.Cut(part, "=")
		switch key {
		case "width":
			if !hasVal {
				return m, fmt.Errorf("width modifier needs a value")
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return m, fmt.Errorf("invalid width %q", val)
			}
			m.width = &n
		case "upper":
			m.upper = true
		case "lower":
			m.lower = true
		case "trim":
			m.trim = true
		default:
			return m, fmt.Errorf("unknown modifier %q", key)
		}
	}
	return m, nil
}

func (v Vars) resolve(name string, m modifiers) string {
	var val string
	switch name {
	case "Author":
		val = v.Author
	case "Series":
		val = v.Series
	case "Title":
		val = v.Title
	case "TitleShort":
		val = v.TitleShort
	case "Narrator":
		val = v.Narrator
	case "ASIN":
		val = v.ASIN
	case "Year":
		val = v.Year
	case "SHORTCODE":
		val = v.Shortcode
	case "OriginalFilename":
		val = v.Filename
	case "Ext":
		val = path.Ext(v.Filename)
	case "Seq":
		if v.HasSequence {
			val = formatSequence(v.Sequence, 1)
		}
	case "PaddedSeq":
		if v.HasSequence {
			w := v.PadWidth
			if w < 1 {
				w = 2
			}
			if m.width != nil {
				w = *m.width
			}
			val = formatSequence(v.Sequence, w)
		}
	}
	if m.trim {
		val = strings.TrimSpace(val)
	}
	if m.upper {
		val = strings.ToUpper(val)
	}
	if m.lower {
		val = strings.ToLower(val)
	}
	// A separator inside a variable value (e.g. metadata "AC/DC") must not create
	// path segments; collapse it so only the template's literal slashes split dirs.
	return pathSepReplacer.Replace(val)
}

var pathSepReplacer = strings.NewReplacer("/", "-", "\\", "-")

func isLetter(b byte) bool    { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }
func isIdentChar(b byte) bool { return isLetter(b) || (b >= '0' && b <= '9') }
