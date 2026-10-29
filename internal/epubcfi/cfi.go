package epubcfi

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Part is one step of a CFI path (foliate's part object): Offset, Temporal are
// nil when the CFI names none (a JS undefined, which foliate tells apart from 0).
type Part struct {
	Index    int
	ID       string
	Offset   *int
	Temporal *float64
	Spatial  []float64
	Text     []string
	Side     string
}

// Path is one indirection level of a CFI: the steps up to a "!".
type Path []Part

// CFI is a parsed CFI: Paths for a plain one, or Parent, Start and End for a
// range (IsRange).
type CFI struct {
	Paths              []Path
	Parent, Start, End []Path
	IsRange            bool
}

var isCFI = regexp.MustCompile(`^epubcfi\((.*)\)$`)

// IsCFI reports whether s is wrapped as epubcfi(...).
func IsCFI(s string) bool { return isCFI.MatchString(s) }

func unwrap(s string) string {
	if m := isCFI.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

func wrap(s string) string {
	if isCFI.MatchString(s) {
		return s
	}
	return "epubcfi(" + s + ")"
}

// JoinIndir joins CFIs with indirection steps ("!"), as foliate's joinIndir.
func JoinIndir(xs ...string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = unwrap(x)
	}
	return "epubcfi(" + strings.Join(parts, "!") + ")"
}

var cfiEscaper = strings.NewReplacer("^", "^^", "[", "^[", "]", "^]", "(", "^(", ")", "^)", ",", "^,", ";", "^;", "=", "^=")

func escapeCFI(s string) string { return cfiEscaper.Replace(s) }

// token is one tokenizer output: its type ("/", ":", "~", "@", "[", ";s", "!",
// ",", ";<name>") and value.
type token struct {
	typ string
	num float64
	str string
}

func isDigit(c string) bool { return len(c) == 1 && c[0] >= '0' && c[0] <= '9' }

// tokenize ports foliate's tokenizer, character (code point) by character.
func tokenize(s string) []token {
	var tokens []token
	state, value, escape := "", "", false
	hasState := false
	push := func(t token) {
		tokens = append(tokens, t)
		state, hasState, value = "", false, ""
	}
	cat := func(c string) {
		value += c
		escape = false
	}
	runes := []rune(strings.TrimSpace(s))
	chars := make([]string, 0, len(runes)+1)
	for _, r := range runes {
		chars = append(chars, string(r))
	}
	chars = append(chars, "")
	for _, c := range chars {
		if c == "^" && !escape {
			escape = true
			continue
		}
		switch {
		case hasState && state == "!":
			push(token{typ: "!"})
		case hasState && state == ",":
			push(token{typ: ","})
		case hasState && (state == "/" || state == ":"):
			if isDigit(c) {
				cat(c)
				continue
			}
			n, _ := strconv.Atoi(value)
			push(token{typ: state, num: float64(n)})
		case hasState && state == "~":
			if isDigit(c) || c == "." {
				cat(c)
				continue
			}
			f, _ := strconv.ParseFloat(value, 64)
			push(token{typ: "~", num: f})
		case hasState && state == "@":
			if c == ":" {
				f, _ := strconv.ParseFloat(value, 64)
				push(token{typ: "@", num: f})
				state, hasState = "@", true
				continue
			}
			if isDigit(c) || c == "." {
				cat(c)
				continue
			}
			f, _ := strconv.ParseFloat(value, 64)
			push(token{typ: "@", num: f})
		case hasState && state == "[":
			switch {
			case c == ";" && !escape:
				push(token{typ: "[", str: value})
				state, hasState = ";", true
			case c == "," && !escape:
				push(token{typ: "[", str: value})
				state, hasState = "[", true
			case c == "]" && !escape:
				push(token{typ: "[", str: value})
			default:
				cat(c)
			}
			continue
		case hasState && strings.HasPrefix(state, ";"):
			switch {
			case c == "=" && !escape:
				state, value = ";"+value, ""
			case c == ";" && !escape:
				push(token{typ: state, str: value})
				state, hasState = ";", true
			case c == "]" && !escape:
				push(token{typ: state, str: value})
			default:
				cat(c)
			}
			continue
		}
		switch c {
		case "/", ":", "~", "@", "[", "!", ",":
			state, hasState = c, true
		}
	}
	return tokens
}

// parsePath ports foliate's parser over one indirection level's tokens.
func parsePath(tokens []token) Path {
	var parts Path
	state := ""
	for _, t := range tokens {
		if t.typ == "/" {
			parts = append(parts, Part{Index: int(t.num)})
		} else if len(parts) > 0 {
			last := &parts[len(parts)-1]
			switch t.typ {
			case ":":
				v := int(t.num)
				last.Offset = &v
			case "~":
				v := t.num
				last.Temporal = &v
			case "@":
				last.Spatial = append(last.Spatial, t.num)
			case ";s":
				last.Side = t.str
			case "[":
				if state == "/" && t.str != "" {
					last.ID = t.str
				} else {
					last.Text = append(last.Text, t.str)
					continue
				}
			}
		}
		state = t.typ
	}
	return parts
}

// splitTokens splits tokens at every token of type typ (dropping it).
func splitTokens(tokens []token, typ string) [][]token {
	out := [][]token{{}}
	for _, t := range tokens {
		if t.typ == typ {
			out = append(out, []token{})
			continue
		}
		out[len(out)-1] = append(out[len(out)-1], t)
	}
	return out
}

func parseIndir(tokens []token) []Path {
	var out []Path
	for _, seg := range splitTokens(tokens, "!") {
		out = append(out, parsePath(seg))
	}
	return out
}

// ErrBadCFI is a CFI foliate couldn't take either.
var ErrBadCFI = errors.New("malformed CFI")

// Parse parses a CFI (wrapped or not) as foliate's parse.
func Parse(s string) (CFI, error) {
	tokens := tokenize(unwrap(s))
	segs := splitTokens(tokens, ",")
	switch len(segs) {
	case 1:
		return CFI{Paths: parseIndir(tokens)}, nil
	case 3:
		return CFI{Parent: parseIndir(segs[0]), Start: parseIndir(segs[1]), End: parseIndir(segs[2]), IsRange: true}, nil
	}
	return CFI{}, ErrBadCFI
}

func formatNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func partString(p Part) string {
	var b strings.Builder
	param := ""
	if p.Side != "" {
		param = ";s=" + p.Side
	}
	b.WriteString("/" + strconv.Itoa(p.Index))
	if p.ID != "" {
		b.WriteString("[" + escapeCFI(p.ID) + param + "]")
	}
	if p.Offset != nil && p.Index%2 != 0 {
		b.WriteString(":" + strconv.Itoa(*p.Offset))
	}
	if p.Temporal != nil && *p.Temporal != 0 {
		b.WriteString("~" + formatNum(*p.Temporal))
	}
	if len(p.Spatial) > 0 {
		s := make([]string, len(p.Spatial))
		for i, v := range p.Spatial {
			s[i] = formatNum(v)
		}
		b.WriteString("@" + strings.Join(s, ":"))
	}
	if p.Text != nil || (p.ID == "" && p.Side != "") {
		t := make([]string, len(p.Text))
		for i, v := range p.Text {
			t[i] = escapeCFI(v)
		}
		b.WriteString("[" + strings.Join(t, ",") + param + "]")
	}
	return b.String()
}

func pathsString(paths []Path) string {
	s := make([]string, len(paths))
	for i, path := range paths {
		var b strings.Builder
		for _, p := range path {
			b.WriteString(partString(p))
		}
		s[i] = b.String()
	}
	return strings.Join(s, "!")
}

// String renders the CFI wrapped, as foliate's toString.
func (c CFI) String() string {
	if c.IsRange {
		return wrap(pathsString(c.Parent) + "," + pathsString(c.Start) + "," + pathsString(c.End))
	}
	return wrap(pathsString(c.Paths))
}

func clonePath(p Path) Path { return append(Path(nil), p...) }

// concatArrays joins b onto a, a's last path continued by b's first.
func concatArrays(a, b []Path) []Path {
	out := make([]Path, 0, len(a)+len(b))
	for _, p := range a[:len(a)-1] {
		out = append(out, clonePath(p))
	}
	last := clonePath(a[len(a)-1])
	if len(b) > 0 {
		last = append(last, b[0]...)
	}
	out = append(out, last)
	for _, p := range b[min(1, len(b)):] {
		out = append(out, clonePath(p))
	}
	return out
}

// Collapse is the range's start (or end, toEnd) as a plain CFI's paths; a plain
// CFI's own.
func (c CFI) Collapse(toEnd bool) []Path {
	if !c.IsRange {
		return c.Paths
	}
	if len(c.Parent) == 0 {
		if toEnd {
			return c.End
		}
		return c.Start
	}
	if toEnd {
		return concatArrays(c.Parent, c.End)
	}
	return concatArrays(c.Parent, c.Start)
}

func noOffset(p *Part) bool { return p == nil || p.Offset == nil || *p.Offset == 0 }

// buildRange ports foliate's buildRange over two plain CFIs' paths.
func buildRange(from, to []Path) string {
	localFrom, localTo := from[len(from)-1], to[len(to)-1]
	var localParent, localStart, localEnd Path
	push := true
	for i := 0; i < max(len(localFrom), len(localTo)); i++ {
		var a, b *Part
		if i < len(localFrom) {
			a = &localFrom[i]
		}
		if i < len(localTo) {
			b = &localTo[i]
		}
		push = push && a != nil && b != nil && a.Index == b.Index && noOffset(a) && noOffset(b)
		if push {
			localParent = append(localParent, *a)
			continue
		}
		if a != nil {
			localStart = append(localStart, *a)
		}
		if b != nil {
			localEnd = append(localEnd, *b)
		}
	}
	parent := append(append([]Path(nil), from[:len(from)-1]...), localParent)
	return CFI{Parent: parent, Start: []Path{localStart}, End: []Path{localEnd}, IsRange: true}.String()
}

// Compare orders two CFIs as foliate's compare: -1, 0 or 1.
func Compare(a, b CFI) int {
	if a.IsRange || b.IsRange {
		if c := comparePaths(a.Collapse(false), b.Collapse(false)); c != 0 {
			return c
		}
		return comparePaths(a.Collapse(true), b.Collapse(true))
	}
	return comparePaths(a.Paths, b.Paths)
}

// CompareStrings is Compare over two CFI strings.
func CompareStrings(a, b string) (int, error) {
	pa, err := Parse(a)
	if err != nil {
		return 0, err
	}
	pb, err := Parse(b)
	if err != nil {
		return 0, err
	}
	return Compare(pa, pb), nil
}

func comparePaths(a, b []Path) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		var p, q Path
		if i < len(a) {
			p = a[i]
		}
		if i < len(b) {
			q = b[i]
		}
		maxIndex := max(len(p), len(q)) - 1
		for j := 0; j <= maxIndex; j++ {
			if j >= len(p) {
				return -1
			}
			if j >= len(q) {
				return 1
			}
			x, y := p[j], q[j]
			if x.Index > y.Index {
				return 1
			}
			if x.Index < y.Index {
				return -1
			}
			if j == maxIndex && x.Offset != nil && y.Offset != nil {
				if *x.Offset > *y.Offset {
					return 1
				}
				if *x.Offset < *y.Offset {
					return -1
				}
			}
		}
	}
	return 0
}
