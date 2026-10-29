package epubcfi

import (
	"strings"
	"testing"
)

// The expectations are foliate's epubcfi.js (pin 78914aef) run on the same input.
func TestParseCollapseRoundTrip(t *testing.T) {
	cases := []struct{ in, start, end string }{
		{"epubcfi(/6/4[chap^[1^]]!/4/2[a^,b]/1:3)", "epubcfi(/6/4[chap^[1^]]!/4/2[a^,b]/1:3)", "epubcfi(/6/4[chap^[1^]]!/4/2[a^,b]/1:3)"},
		{"/6/4!/4/10/2/1:0", "epubcfi(/6/4!/4/10/2/1:0)", "epubcfi(/6/4!/4/10/2/1:0)"},
		// An offset on an element step is never printed (even indices).
		{"epubcfi(/6/4[x;s=b]!/4/2:5)", "epubcfi(/6/4[x;s=b]!/4/2)", "epubcfi(/6/4[x;s=b]!/4/2)"},
		{"epubcfi(/6/2!/4/2~12.5@10:20/1:3)", "epubcfi(/6/2!/4/2~12.5@10:20/1:3)", "epubcfi(/6/2!/4/2~12.5@10:20/1:3)"},
		{"epubcfi(/6/2!/4/2/1:3[yyy,zzz;s=a])", "epubcfi(/6/2!/4/2/1:3[yyy,zzz;s=a])", "epubcfi(/6/2!/4/2/1:3[yyy,zzz;s=a])"},
		{"epubcfi(/6/4!/4,/2/1:1,/4/1:2)", "epubcfi(/6/4!/4/2/1:1)", "epubcfi(/6/4!/4/4/1:2)"},
		{"epubcfi(/6/4!/4/2,/1:0,/1:0)", "epubcfi(/6/4!/4/2/1:0)", "epubcfi(/6/4!/4/2/1:0)"},
	}
	for _, c := range cases {
		p, err := Parse(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := (CFI{Paths: p.Collapse(false)}).String(); got != c.start {
			t.Errorf("collapse(%s) = %s, want %s", c.in, got, c.start)
		}
		if got := (CFI{Paths: p.Collapse(true)}).String(); got != c.end {
			t.Errorf("collapse(%s, true) = %s, want %s", c.in, got, c.end)
		}
	}
	if _, err := Parse("epubcfi(/6/4,/1:0)"); err == nil {
		t.Fatal("a range with two parts parsed")
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"epubcfi(/6/4!/4/2/1:3)", "epubcfi(/6/4!/4/2/1:10)", -1},
		{"epubcfi(/6/4!/4/2/1:3)", "epubcfi(/6/4!/4/2)", 1},
		{"epubcfi(/6/4!/4/2)", "epubcfi(/6/4!/4/2/1:3)", -1},
		{"epubcfi(/6/6!/4/2/1:3)", "epubcfi(/6/4!/4/20/1:9)", 1},
		{"epubcfi(/6/4!/4/2,/1:0,/1:5)", "epubcfi(/6/4!/4/2,/1:0,/1:9)", -1},
		{"epubcfi(/6/4!/4/2/1:3)", "epubcfi(/6/4!/4/2/1:3)", 0},
		{"epubcfi(/6/4!/4/2/3:3)", "epubcfi(/6/4!/4/2/1:30)", 1},
	}
	for _, c := range cases {
		if got, err := CompareStrings(c.a, c.b); err != nil || got != c.want {
			t.Errorf("compare(%s, %s) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
	}
}

func TestDOMKeepsWhatTheBrowserWould(t *testing.T) {
	doc, err := ParseXML([]byte(`<?xml version="1.0"?><!DOCTYPE html><html xmlns="http://www.w3.org/1999/xhtml">` +
		`<body><p id="a">One<!-- c -->two<?pi x?><![CDATA[ & three]]> &amp;&nbsp;four</p>  <p/></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	p := doc.GetElementByID("a")
	if p == nil || len(p.Children) != 1 || p.Children[0].Data != "Onetwo & three & four" {
		t.Fatalf("p = %+v", p)
	}
	body := Body(doc)
	if len(body.Children) != 3 || body.Children[1].Data != "  " {
		t.Fatalf("whitespace text between elements is kept: %d children", len(body.Children))
	}
	if _, err := ParseXML([]byte("  ")); err == nil {
		t.Fatal("a document with no element parsed")
	}
	h, err := ParseDocument([]byte("<p>Left <b>open"), "text/html")
	if err != nil || h.Root.Name != "html" || !strings.Contains(Body(h).Children[0].Children[0].Data, "Left") {
		t.Fatalf("html parse: %v", err)
	}
}

func TestLen16(t *testing.T) {
	if Len16("a😀b") != 4 || ByteOffset16("a😀b", 3) != 5 || ByteOffset16("ab", 9) != 2 {
		t.Fatal("UTF-16 lengths")
	}
	w := &Walk{Text: []rune("a😀b")}
	if w.Offset16(2) != 3 || w.RuneOffset(3) != 2 || w.RuneOffset(99) != 3 {
		t.Fatal("walk offsets")
	}
	if _, err := (&Walk{}).CFI(0, 1); err == nil {
		t.Fatal("a walk with no text made a CFI")
	}
}

func TestResolveRefusesWhatLeadsNowhere(t *testing.T) {
	// No head: the body is the html element's first element child, /2.
	doc, err := ParseXML([]byte(`<html><body><p>x</p></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"epubcfi(/6/2!/2/40/1:0)", "epubcfi(/6/2)", "epubcfi(/6/2!/2/2/1:0)x,y"} {
		if _, err := Resolve(doc, c); err == nil {
			t.Errorf("%s resolved", c)
		}
	}
	r, err := Resolve(doc, "epubcfi(/6/2!/2/2,/1:0,/1:1)")
	if err != nil || r.Text(doc) != "x" {
		t.Fatalf("resolve = %+v, %v", r, err)
	}
}
