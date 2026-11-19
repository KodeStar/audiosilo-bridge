package placement

import "testing"

func TestSuggest(t *testing.T) {
	tests := []struct {
		name     string
		in       Input
		wantPath string // expected full path (folder/filename)
		wantKind Kind
	}{
		{
			name: "cradle next book anchors on siblings",
			in: Input{
				Author: "Will Wight", Title: "Waybound", SeriesName: "Cradle",
				Sequence: 12, HasSequence: true, Filename: "Waybound - Cradle, Book 12.m4b",
				Siblings: []Sibling{
					{RelPath: "Will Wight/Cradle/C01 - Unsouled", SeriesIndex: 1},
					{RelPath: "Will Wight/Cradle/C08 - Wintersteel", SeriesIndex: 8},
				},
			},
			wantPath: "Will Wight/Cradle/C12 - Waybound/Waybound - Cradle, Book 12.m4b",
			wantKind: SeriesExisting,
		},
		{
			name: "novella keeps fractional position and padding",
			in: Input{
				Author: "Brandon Sanderson", Title: "Edgedancer", SeriesName: "Stormlight Archive",
				Sequence: 2.5, HasSequence: true, Filename: "Edgedancer.m4b",
				Siblings: []Sibling{
					{RelPath: "Brandon Sanderson/Stormlight Archive/SA01 - The Way of Kings", SeriesIndex: 1},
					{RelPath: "Brandon Sanderson/Stormlight Archive/SA05 - Wind and Truth", SeriesIndex: 5},
				},
			},
			wantPath: "Brandon Sanderson/Stormlight Archive/SA02.5 - Edgedancer/Edgedancer.m4b",
			wantKind: SeriesExisting,
		},
		{
			name: "unprefixed sibling codes take a fractional position as is",
			in: Input{
				Author: "Wen Liang", Title: "Margins", SeriesName: "Mapmakers",
				Sequence: 2.5, HasSequence: true, Filename: "Mapmakers 2.5 - Margins.m4b",
				Siblings: []Sibling{
					{RelPath: "Wen Liang/Mapmakers/1 - The Cartographer's Apprentice", SeriesIndex: 1},
				},
			},
			wantPath: "Wen Liang/Mapmakers/2.5 - Margins/Mapmakers 2.5 - Margins.m4b",
			wantKind: SeriesExisting,
		},
		{
			name: "large series keeps two-digit padding",
			in: Input{
				Author: "B.V. Larson", Title: "Throne World", SeriesName: "Undying Mercenaries",
				Sequence: 21, HasSequence: true, Filename: "Throne World - Undying Mercenaries, Book 21.m4b",
				Siblings: []Sibling{
					{RelPath: "B.V. Larson/Undying Mercenaries/UM01 - Steel World", SeriesIndex: 1},
					{RelPath: "B.V. Larson/Undying Mercenaries/UM20 - Crystal World", SeriesIndex: 20},
				},
			},
			wantPath: "B.V. Larson/Undying Mercenaries/UM21 - Throne World/Throne World - Undying Mercenaries, Book 21.m4b",
			wantKind: SeriesExisting,
		},
		{
			name: "multi-letter shortcode (FWM)",
			in: Input{
				Author: "Shirtaloon", Title: "He Who Fights with Monsters 12", SeriesName: "He Who Fights With Monsters",
				Sequence: 12, HasSequence: true, Filename: "HWFWM 12.m4b",
				Siblings: []Sibling{
					{RelPath: "Shirtaloon/He Who Fights WIth Monsters/FWM01 - He Who Fights With Monsters - Book 1", SeriesIndex: 1},
					{RelPath: "Shirtaloon/He Who Fights WIth Monsters/FWM10 - He Who Fights With Monsters - Book 10", SeriesIndex: 10},
				},
			},
			// Folder names (incl. the "WIth" typo) come verbatim from the sibling.
			wantPath: "Shirtaloon/He Who Fights WIth Monsters/FWM12 - He Who Fights with Monsters 12/HWFWM 12.m4b",
			wantKind: SeriesExisting,
		},
		{
			// A title that cleans down to its number borrows the sibling's naming,
			// renumbered, instead of producing "FWM13 - 13".
			name: "bare title follows sibling naming",
			in: Input{
				Author: "Shirtaloon", Title: "13", SeriesName: "He Who Fights with Monsters",
				Sequence: 13, HasSequence: true, Filename: "HWFWM 13.m4b",
				Siblings: []Sibling{
					{RelPath: "Shirtaloon/He Who Fights With Monsters/FWM09 - He Who Fights With Monsters - Book 9"},
					{RelPath: "Shirtaloon/He Who Fights With Monsters/FWM12 - He Who Fights With Monsters - Book 12", SeriesIndex: 12},
				},
			},
			wantPath: "Shirtaloon/He Who Fights With Monsters/FWM13 - He Who Fights With Monsters - Book 13/HWFWM 13.m4b",
			wantKind: SeriesExisting,
		},
		{
			name: "bare title with real-titled siblings uses the series name",
			in: Input{
				Author: "Will Wight", Title: "13", SeriesName: "Cradle",
				Sequence: 13, HasSequence: true, Filename: "Cradle 13.m4b",
				Siblings: []Sibling{{RelPath: "Will Wight/Cradle/C12 - Waybound", SeriesIndex: 12}},
			},
			wantPath: "Will Wight/Cradle/C13 - Cradle 13/Cradle 13.m4b",
			wantKind: SeriesExisting,
		},
		{
			name: "bare title in a new series uses the series name",
			in: Input{
				Author: "Shirtaloon", Title: "13", SeriesName: "He Who Fights with Monsters",
				Sequence: 13, HasSequence: true, Filename: "HWFWM 13.m4b",
			},
			wantPath: "Shirtaloon/He Who Fights with Monsters/HWFWM13 - He Who Fights with Monsters 13/HWFWM 13.m4b",
			wantKind: SeriesNew,
		},
		{
			name: "universe nesting preserves middle segments",
			in: Input{
				Author: "Robin Hobb", Title: "Ship of Destiny", SeriesName: "Liveship Traders",
				Sequence: 3, HasSequence: true, Filename: "Ship of Destiny.m4b",
				Siblings: []Sibling{
					{RelPath: "Robin Hobb/Realms of the Elderlings/02 - Liveship Traders/LT01 - Ship of Magic", SeriesIndex: 1},
					{RelPath: "Robin Hobb/Realms of the Elderlings/02 - Liveship Traders/LT02 - Mad Ship", SeriesIndex: 2},
				},
			},
			wantPath: "Robin Hobb/Realms of the Elderlings/02 - Liveship Traders/LT03 - Ship of Destiny/Ship of Destiny.m4b",
			wantKind: SeriesExisting,
		},
		{
			name: "new series guesses shortcode from initials",
			in: Input{
				Author: "Jane Doe", Title: "First Light", SeriesName: "Stormlight Archive",
				Sequence: 1, HasSequence: true, Filename: "First Light.m4b",
			},
			wantPath: "Jane Doe/Stormlight Archive/SA01 - First Light/First Light.m4b",
			wantKind: SeriesNew,
		},
		{
			name: "new single-word series uses leading letter",
			in: Input{
				Author: "Will Wight", Title: "Unsouled", SeriesName: "Cradle",
				Sequence: 1, HasSequence: true, Filename: "Unsouled.m4b",
			},
			wantPath: "Will Wight/Cradle/C01 - Unsouled/Unsouled.m4b",
			wantKind: SeriesNew,
		},
		{
			name: "standalone placed under author",
			in: Input{
				Author: "Brandon Sanderson", Title: "Warbreaker", SeriesName: "",
				Filename: "Warbreaker (Unabridged).m4b",
			},
			wantPath: "Brandon Sanderson/Warbreaker/Warbreaker (Unabridged).m4b",
			wantKind: Standalone,
		},
		{
			name: "non-shortcode series style is reproduced",
			in: Input{
				Author: "Lois McMaster Bujold", Title: "Memory", SeriesName: "Vorkosigan Saga",
				Sequence: 10, HasSequence: true, Filename: "Memory.m4b",
				Siblings: []Sibling{
					{RelPath: "Lois McMaster Bujold/Vorkosigan Saga/Vorkosigan Saga 01 - Shards of Honor", SeriesIndex: 1},
					{RelPath: "Lois McMaster Bujold/Vorkosigan Saga/Vorkosigan Saga 09 - Mirror Dance", SeriesIndex: 9},
				},
			},
			wantPath: "Lois McMaster Bujold/Vorkosigan Saga/Vorkosigan Saga 10 - Memory/Memory.m4b",
			wantKind: SeriesExisting,
		},
		{
			// AuthorFolder reuses the server's existing spelling so a differently-
			// spaced author ("L. A. McBride") lands in the existing "L.A. McBride".
			name: "standalone reuses existing author folder spelling",
			in: Input{
				Author: "L. A. McBride", Title: "Some Standalone", SeriesName: "",
				Filename: "Some Standalone.m4b", AuthorFolder: "L.A. McBride",
			},
			wantPath: "L.A. McBride/Some Standalone/Some Standalone.m4b",
			wantKind: Standalone,
		},
		{
			// A hidden (dot) name is never indexed by the server's scan.
			name: "leading dots never make a hidden folder",
			in: Input{
				Author: ".hack Team", Title: "...And Ladies of the Club", Filename: "And Ladies of the Club.m4b",
			},
			wantPath: "hack Team/And Ladies of the Club/And Ladies of the Club.m4b",
			wantKind: Standalone,
		},
		{
			name: "new series reuses existing author folder spelling",
			in: Input{
				Author: "L. A. McBride", Title: "Book One", SeriesName: "Magic & Mayhem",
				Sequence: 1, HasSequence: true, Filename: "Book One.m4b", AuthorFolder: "L.A. McBride",
			},
			wantPath: "L.A. McBride/Magic & Mayhem/MM01 - Book One/Book One.m4b",
			wantKind: SeriesNew,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Suggest(tt.in)
			if got.FullPath() != tt.wantPath {
				t.Errorf("path:\n got: %q\nwant: %q", got.FullPath(), tt.wantPath)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("kind: got %v, want %v", got.Kind, tt.wantKind)
			}
		})
	}
}

func TestParseCode(t *testing.T) {
	tests := []struct {
		code       string
		wantPrefix string
		wantWidth  int
		wantOK     bool
	}{
		{"C08", "C", 2, true},
		{"FWM01", "FWM", 2, true},
		{"SA02.5", "SA", 2, true},
		{"UM21", "UM", 2, true},
		{"Vorkosigan Saga 01", "Vorkosigan Saga ", 2, true},
		{"Warbreaker", "", 0, false},
	}
	for _, tt := range tests {
		p, w, ok := parseCode(tt.code)
		if p != tt.wantPrefix || w != tt.wantWidth || ok != tt.wantOK {
			t.Errorf("parseCode(%q) = (%q,%d,%v), want (%q,%d,%v)",
				tt.code, p, w, ok, tt.wantPrefix, tt.wantWidth, tt.wantOK)
		}
	}
}

func TestFormatSequence(t *testing.T) {
	tests := []struct {
		seq   float64
		width int
		want  string
	}{
		{12, 2, "12"},
		{1, 2, "01"},
		{2.5, 2, "02.5"},
		{21, 2, "21"},
		{105, 2, "105"},
		{3, 3, "003"},
	}
	for _, tt := range tests {
		if got := formatSequence(tt.seq, tt.width); got != tt.want {
			t.Errorf("formatSequence(%v,%d) = %q, want %q", tt.seq, tt.width, got, tt.want)
		}
	}
}

func TestTitleFromSibling(t *testing.T) {
	in := Input{Sequence: 2.5, HasSequence: true}
	for _, tc := range []struct {
		leaf, want string
		ok         bool
	}{
		{"FWM02 - He Who Fights With Monsters - Book 2", "He Who Fights With Monsters - Book 2.5", true},
		{"X02 - Part 12 of Book 2", "Part 12 of Book 2.5", true}, // the standalone 2, not the one in 12
		{"C02 - Soulsmith", "", false},                           // a real title: no number to renumber
		{"FWM02", "", false},                                     // no title part at all
	} {
		got, ok := titleFromSibling(tc.leaf, in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("titleFromSibling(%q) = %q, %v; want %q, %v", tc.leaf, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := titleFromSibling("FWM02 - Book 2", Input{}); ok {
		t.Error("no sequence must not renumber")
	}
}

func TestDropLeadingSequence(t *testing.T) {
	in := Input{Sequence: 2.5, HasSequence: true}
	for _, c := range []struct{ in, want string }{
		{"2.5: Margins", "Margins"},
		{"2.50 - Margins", "Margins"},
		{"2.5", "2.5"},                 // bare: bareTitle names it by series or siblings
		{"2.5: 1", "2.5: 1"},           // nothing of its own after the number
		{"2.5 Margins", "2.5 Margins"}, // no separator: may be the title's own words
		{"25: Margins", "25: Margins"},
		{"2.5 \u2013 Margins", "Margins"},
		{"2.5-Margins Redux", "2.5-Margins Redux"}, // a dash glued to both sides is the title's own
		{"Margins", "Margins"},
	} {
		if got := dropLeadingSequence(c.in, in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
	one := Input{Sequence: 1, HasSequence: true}
	for _, c := range []string{"1-3: Box Set", "1,000 Ships", "1\u20133: Box Set"} {
		if got := dropLeadingSequence(c, one); got != c {
			t.Errorf("%q: a range or a thousands separator is not a separator, got %q", c, got)
		}
	}
	if got := dropLeadingSequence("2.5: Margins", Input{}); got != "2.5: Margins" {
		t.Errorf("no sequence: got %q", got)
	}
}

// SuggestWith drops the book's own position from {Title} and {TitleShort} in
// Auto mode and in a template that renders the position, so "2.5: Margins"
// never doubles the folder code's number; a template without it keeps it.
func TestSuggestWithLeadingSequence(t *testing.T) {
	in := Input{
		Author: "Wen Liang", Title: "2.5: Margins", TitleShort: "2.5: Margins", SeriesName: "Mapmakers",
		Sequence: 2.5, HasSequence: true, Filename: "Mapmakers 2.5 - Margins.m4b",
		Siblings: []Sibling{{RelPath: "Wen Liang/Mapmakers/1 - The Cartographer's Apprentice", SeriesIndex: 1}},
	}
	for _, c := range []struct {
		mode       Mode
		tmpl, want string
	}{
		{ModeAuto, "", "Wen Liang/Mapmakers/2.5 - Margins/Mapmakers 2.5 - Margins.m4b"},
		{ModeTemplate, "{Author}/{Series}/{Seq} - {Title}/{TitleShort}{Ext}", "Wen Liang/Mapmakers/2.5 - Margins/Margins.m4b"},
		{ModeTemplate, "{Author}/{Series}/{ {PaddedSeq} - }{Title}/{OriginalFilename}", "Wen Liang/Mapmakers/2.5 - Margins/Mapmakers 2.5 - Margins.m4b"},
		// No position in the template: the title keeps its number.
		{ModeTemplate, "{Author}/{Series}/{Title}/{OriginalFilename}", "Wen Liang/Mapmakers/2.5 - Margins/Mapmakers 2.5 - Margins.m4b"},
	} {
		sug, err := SuggestWith(c.mode, c.tmpl, in)
		if err != nil {
			t.Fatal(err)
		}
		if got := sug.FullPath(); got != c.want {
			t.Errorf("mode %v: got %q, want %q", c.mode, got, c.want)
		}
	}
}

// Suggest itself drops the book's own position from its titles, as SuggestWith
// does (bridge#47): a direct caller never gets "2.5 - 2.5 - Margins".
func TestSuggestDropsLeadingSequence(t *testing.T) {
	in := Input{
		Author: "Wen Liang", Title: "2.5: Margins", TitleShort: "2.5: Margins", SeriesName: "Mapmakers",
		Sequence: 2.5, HasSequence: true, Filename: "Margins.m4b",
	}
	if got := Suggest(in).RelPath; got != "Wen Liang/Mapmakers/M02.5 - Margins" {
		t.Errorf("got %q", got)
	}
	// Not the book's position: kept.
	in.Title = "3: Margins"
	if got := Suggest(in).RelPath; got != "Wen Liang/Mapmakers/M02.5 - 3 - Margins" {
		t.Errorf("another number: got %q", got)
	}
}

// A title Sanitize leaves empty names its folder by the ASIN (bridge#80), in
// Auto and in a template; a series book is named by its series, as before.
func TestSuggestNamelessTitle(t *testing.T) {
	in := Input{Author: "Ada Fenwick", Title: "???", TitleShort: "???", ASIN: "B0NAMELESS", Filename: "audiobook.m4b"}
	if got := Suggest(in).FullPath(); got != "Ada Fenwick/B0NAMELESS/audiobook.m4b" {
		t.Errorf("auto: got %q", got)
	}
	for tmpl, want := range map[string]string{
		"{Author}/{Title}.m4b":                     "Ada Fenwick/B0NAMELESS.m4b",
		"{Author}/{TitleShort}/{OriginalFilename}": "Ada Fenwick/B0NAMELESS/audiobook.m4b",
	} {
		sug, err := SuggestWith(ModeTemplate, tmpl, in)
		if err != nil || sug.FullPath() != want {
			t.Errorf("%s: got %q, %v; want %q", tmpl, sug.FullPath(), err, want)
		}
	}
	series := in
	series.SeriesName, series.Sequence, series.HasSequence = "Night Road", 3, true
	if got := Suggest(series).RelPath; got != "Ada Fenwick/Night Road/NR03 - Night Road" {
		t.Errorf("series: got %q", got)
	}
}

// Windows reserves device names, alone or before an extension, in any case:
// Sanitize gives each an underscore after its stem (bridge#80); a longer name
// that only starts with one is left alone.
func TestSanitizeReservedNames(t *testing.T) {
	for in, want := range map[string]string{
		"CON":        "CON_",
		"con":        "con_",
		"Nul.m4b":    "Nul_.m4b",
		"aux .m4b":   "aux_.m4b",
		"COM1":       "COM1_",
		"lpt9.txt":   "lpt9_.txt",
		"COM¹":       "COM¹_",
		"PRN?":       "PRN_",
		"CONIN$":     "CONIN$_",
		"conout$":    "conout$_",
		"ConIn$.m4b": "ConIn$_.m4b",
		"CONIN":      "CONIN",
		"CONOUT$S":   "CONOUT$S",
		"Console":    "Console",
		"COM10":      "COM10",
		"Conan":      "Conan",
		"Lpt":        "Lpt",
	} {
		if got := Sanitize(in); got != want {
			t.Errorf("Sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	sug, err := SuggestWith(ModeTemplate, "{Author}/{Title}/{Title}.m4b", Input{Author: "Aux", Title: "Con"})
	if err != nil || sug.FullPath() != "Aux_/Con_/Con_.m4b" {
		t.Errorf("template: %q, %v", sug.FullPath(), err)
	}
}
