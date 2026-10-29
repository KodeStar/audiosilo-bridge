package epubcfi_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/epubcfi"
	"github.com/kodestar/audiosilo-bridge/internal/readalong"
)

// golden is testdata/golden.json, written by foliate's own epubcfi.js on jsdom
// (scripts/cfi-golden/gen.mjs).
type golden struct {
	EpubcfiSHA256 string       `json:"epubcfi_sha256"`
	Books         []goldenBook `json:"books"`
}

type goldenBook struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	OPF    string `json:"opf"`
	Spine  []struct {
		Index     int    `json:"index"`
		IDRef     string `json:"idref"`
		Href      string `json:"href"`
		MediaType string `json:"media_type"`
		Linear    bool   `json:"linear"`
		CFI       string `json:"cfi"`
	} `json:"spine"`
	RawSections []json.RawMessage `json:"sections"`
	Compares    []struct {
		A   string `json:"a"`
		B   string `json:"b"`
		Cmp int    `json:"cmp"`
	} `json:"compares"`
	Collapses []struct {
		CFI   string `json:"cfi"`
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"collapses"`
}

type goldenSection struct {
	Index      int    `json:"index"`
	Href       string `json:"href"`
	TextLen16  int    `json:"text_len16"`
	TextSHA256 string `json:"text_sha256"`
	Breaks16   []int  `json:"breaks16"`
	Ranges     []struct {
		Start int    `json:"start"`
		End   int    `json:"end"`
		CFI   string `json:"cfi"`
		Text  string `json:"text"`
		CDATA bool   `json:"cdata"`
	} `json:"ranges"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func fileSHA(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// The copied epubcfi.js is the pinned one, byte for byte, and the golden file was
// written with it.
func TestGoldenPin(t *testing.T) {
	f, err := os.Open("../../scripts/cfi-golden/PIN.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	want := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) == 2 && fields[1] == "epubcfi.js" {
			want = fields[0]
		}
	}
	if want == "" {
		t.Fatal("PIN.txt has no epubcfi.js line")
	}
	if got := fileSHA(t, "../../scripts/cfi-golden/epubcfi.js"); got != want {
		t.Fatalf("scripts/cfi-golden/epubcfi.js sha256 %s, the pin says %s", got, want)
	}
	if g := loadGolden(t); g.EpubcfiSHA256 != want {
		t.Fatalf("golden.json was written with epubcfi.js %s, the pin says %s: run gen.mjs again", g.EpubcfiSHA256, want)
	}
}

// Both directions against foliate: the section CFIs (fromElements), the walk's
// text and breaks, every range's CFI (fromRange) and what each golden CFI resolves
// to on a fresh parse (toRange), compare and collapse.
func TestGolden(t *testing.T) {
	g := loadGolden(t)
	for _, b := range g.Books {
		t.Run(b.Name, func(t *testing.T) {
			path := "testdata/" + b.Name + ".epub"
			if got := fileSHA(t, path); got != b.SHA256 {
				t.Fatalf("%s changed since golden.json was written", path)
			}
			e, err := readalong.OpenEPUB(path)
			if err != nil {
				t.Fatal(err)
			}
			if e.OPFPath != b.OPF || len(e.Spine) != len(b.Spine) {
				t.Fatalf("opf %q spine %d, want %q %d", e.OPFPath, len(e.Spine), b.OPF, len(b.Spine))
			}
			bySection := map[int]readalong.Section{}
			for i, s := range b.Spine {
				got := e.Spine[i]
				if got.Index != s.Index || got.IDRef != s.IDRef || got.Href != s.Href || got.MediaType != s.MediaType ||
					got.Linear != s.Linear || got.CFI != s.CFI {
					t.Fatalf("spine %d = %+v, want %+v", i, got, s)
				}
				bySection[s.Index] = got
			}
			ranges := 0
			for _, rawSec := range b.RawSections {
				var gs goldenSection
				if err := json.Unmarshal(rawSec, &gs); err != nil {
					t.Fatal(err)
				}
				sec := bySection[gs.Index]
				doc, err := e.Document(sec)
				if err != nil {
					t.Fatal(err)
				}
				w := epubcfi.WalkText(doc)
				text := string(w.Text)
				sum := sha256.Sum256([]byte(text))
				if got := w.Offset16(len(w.Text)); got != gs.TextLen16 || hex.EncodeToString(sum[:]) != gs.TextSHA256 {
					t.Fatalf("%s: walk text %d units %q, want %d units (sha %s)", gs.Href, got, text, gs.TextLen16, gs.TextSHA256)
				}
				if len(w.Breaks) != len(gs.Breaks16) {
					t.Fatalf("%s: %d breaks, want %d", gs.Href, len(w.Breaks), len(gs.Breaks16))
				}
				for i, br := range w.Breaks {
					if w.Offset16(br) != gs.Breaks16[i] {
						t.Fatalf("%s: break %d at %d, want %d", gs.Href, i, w.Offset16(br), gs.Breaks16[i])
					}
				}
				for _, r := range gs.Ranges {
					ranges++
					local, err := w.CFI(w.RuneOffset(r.Start), w.RuneOffset(r.End))
					if err != nil {
						t.Fatal(err)
					}
					if got := epubcfi.JoinIndir(sec.CFI, local); got != r.CFI {
						t.Errorf("%s [%d,%d): CFI %s, want %s", gs.Href, r.Start, r.End, got, r.CFI)
					}
					// A fresh parse, as the reader resolves it.
					fresh, err := e.Document(sec)
					if err != nil {
						t.Fatal(err)
					}
					rng, err := epubcfi.Resolve(fresh, r.CFI)
					if err != nil {
						t.Fatalf("%s: resolve %s: %v", gs.Href, r.CFI, err)
					}
					if got := rng.Text(fresh); got != r.Text && !r.CDATA {
						t.Errorf("%s: %s resolves to %q, want %q", gs.Href, r.CFI, got, r.Text)
					}
				}
			}
			if ranges == 0 {
				t.Fatal("no ranges tested")
			}
			for _, c := range b.Compares {
				got, err := epubcfi.CompareStrings(c.A, c.B)
				if err != nil || got != c.Cmp {
					t.Errorf("compare(%s, %s) = %d, %v; want %d", c.A, c.B, got, err, c.Cmp)
				}
			}
			for _, c := range b.Collapses {
				p, err := epubcfi.Parse(c.CFI)
				if err != nil {
					t.Fatal(err)
				}
				if got := (epubcfi.CFI{Paths: p.Collapse(false)}).String(); got != c.Start {
					t.Errorf("collapse(%s) = %s, want %s", c.CFI, got, c.Start)
				}
				if got := (epubcfi.CFI{Paths: p.Collapse(true)}).String(); got != c.End {
					t.Errorf("collapse(%s, true) = %s, want %s", c.CFI, got, c.End)
				}
				if got := p.String(); got != c.CFI {
					t.Errorf("parse+toString(%s) = %s", c.CFI, got)
				}
			}
		})
	}
}
