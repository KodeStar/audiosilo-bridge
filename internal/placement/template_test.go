package placement

import (
	"path"
	"testing"
)

func TestTemplateBasicVars(t *testing.T) {
	tm, err := Parse("{Author}/{Title}")
	if err != nil {
		t.Fatal(err)
	}
	got, err := tm.Render(Vars{Author: "Brandon Sanderson", Title: "Warbreaker"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Brandon Sanderson/Warbreaker" {
		t.Fatalf("got %q", got)
	}
}

func TestTemplateOptionalGroupCollapse(t *testing.T) {
	tm, err := Parse("{Author}/{ {PaddedSeq} - }{TitleShort}/{OriginalFilename}")
	if err != nil {
		t.Fatal(err)
	}
	withSeq, _ := tm.Render(Vars{
		Author: "Will Wight", TitleShort: "Soulsmith",
		Sequence: 2, HasSequence: true, PadWidth: 2, Filename: "Soulsmith.m4b",
	})
	if withSeq != "Will Wight/02 - Soulsmith/Soulsmith.m4b" {
		t.Fatalf("with seq: got %q", withSeq)
	}
	// No sequence: the whole group (including its " - " literal) collapses.
	noSeq, _ := tm.Render(Vars{
		Author: "Will Wight", TitleShort: "Soulsmith", HasSequence: false, Filename: "Soulsmith.m4b",
	})
	if noSeq != "Will Wight/Soulsmith/Soulsmith.m4b" {
		t.Fatalf("no seq: got %q", noSeq)
	}
}

func TestTemplatePaddingAndModifiers(t *testing.T) {
	tm, err := Parse("{PaddedSeq:width=3}-{SHORTCODE:lower}-{Author:upper}")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := tm.Render(Vars{Sequence: 5, HasSequence: true, PadWidth: 2, Shortcode: "SA", Author: "abc"})
	if got != "005-sa-ABC" {
		t.Fatalf("got %q", got)
	}
}

func TestTemplateFractionalSequence(t *testing.T) {
	tm, _ := Parse("{PaddedSeq}")
	got, _ := tm.Render(Vars{Sequence: 2.5, HasSequence: true, PadWidth: 2})
	if got != "02.5" {
		t.Fatalf("got %q", got)
	}
}

func TestTemplateSanitizesSegments(t *testing.T) {
	tm, _ := Parse("{Author}/{Title}")
	// A slash in metadata collapses to '-' (one segment); ':' becomes ' -'.
	got, _ := tm.Render(Vars{Author: "AC/DC", Title: "Back: In"})
	if got != "AC-DC/Back - In" {
		t.Fatalf("got %q", got)
	}
}

// The characters an SMB share or a Windows disk can't store never reach a
// generated name (bridge#47), and the gaps they leave close up.
func TestSanitizeDropsCharactersSMBRefuses(t *testing.T) {
	for in, want := range map[string]string{
		"Who Moved My Cheese?":             "Who Moved My Cheese",
		"M*A*S*H":                          "MASH",
		`The "Real" Story`:                 "The 'Real' Story",
		"Either | Or":                      "Either - Or",
		"<Untitled> Book":                  "Untitled Book",
		"Why? * Because":                   "Why Because",
		"Title : Sub":                      "Title - Sub",
		"Bell\x07 and\x1bEscape":           "Bell and Escape",
		"Mr. Penumbra's 24-Hour Bookstore": "Mr. Penumbra's 24-Hour Bookstore",
		"...And Ladies of the Club":        "And Ladies of the Club",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	tm, _ := Parse("{Author}/{Title}")
	if got, _ := tm.Render(Vars{Author: "Spencer Johnson", Title: "Who Moved My Cheese?"}); got != "Spencer Johnson/Who Moved My Cheese" {
		t.Fatalf("template = %q", got)
	}
}

func TestSuggestWithTemplateUsesSiblings(t *testing.T) {
	in := Input{
		Author: "Will Wight", Title: "Waybound", TitleShort: "Waybound",
		SeriesName: "Cradle", Sequence: 12, HasSequence: true,
		Filename: "Waybound - Cradle, Book 12.m4b",
		Siblings: []Sibling{{RelPath: "Will Wight/Cradle/C11 - Wintersteel", SeriesIndex: 11}},
	}
	sug, err := SuggestWith(ModeTemplate,
		"{Author}/{Series}/{ {SHORTCODE}{PaddedSeq} - }{TitleShort}/{OriginalFilename}", in)
	if err != nil {
		t.Fatal(err)
	}
	got := path.Join(sug.RelPath, sug.Filename)
	want := "Will Wight/Cradle/C12 - Waybound/Waybound - Cradle, Book 12.m4b"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSuggestWithAutoMatchesSuggest(t *testing.T) {
	in := Input{Author: "Brandon Sanderson", Title: "Warbreaker", Filename: "Warbreaker.m4b"}
	sug, err := SuggestWith(ModeAuto, "ignored-template", in)
	if err != nil {
		t.Fatal(err)
	}
	want := Suggest(in)
	if sug.RelPath != want.RelPath || sug.Filename != want.Filename {
		t.Fatalf("Auto mode diverged from Suggest: %+v vs %+v", sug, want)
	}
	// An empty template also falls back to Auto.
	if s, err := SuggestWith(ModeTemplate, "  ", in); err != nil || s.RelPath != want.RelPath {
		t.Fatalf("empty template should fall back to Auto: %+v, %v", s, err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{
		"{Foo}",               // unknown variable
		"{Author",             // unclosed group/brace
		"{PaddedSeq:bogus}",   // unknown modifier
		"{PaddedSeq:width=x}", // bad width value
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q): expected an error", bad)
		}
	}
}

// A bare (number-only) title renders as "<Series> <n>" in template mode too.
func TestTemplateBareTitle(t *testing.T) {
	sug, err := SuggestWith(ModeTemplate, "{Author}/{Series}/{Title}/{OriginalFilename}", Input{
		Author: "Shirtaloon", Title: "13", TitleShort: "13", SeriesName: "He Who Fights with Monsters",
		Sequence: 13, HasSequence: true, Filename: "b.m4b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := path.Join(sug.RelPath, sug.Filename); got != "Shirtaloon/He Who Fights with Monsters/He Who Fights with Monsters 13/b.m4b" {
		t.Fatalf("got %q", got)
	}
}
