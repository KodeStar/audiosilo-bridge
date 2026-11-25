package importjob

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/source"
)

// audibleRec is an Audible record as the library listing makes it: the title,
// the first author, the series and its position as Audible spells it.
func audibleRec(asin, title, author, series, pos string) source.Record {
	r := source.Record{ASIN: asin, Title: title, TitleShort: title, Author: author, Authors: []string{author},
		Series: series, SeriesPosition: pos}
	if v, err := strconv.ParseFloat(pos, 64); err == nil {
		r.Sequence, r.HasSequence = v, true
	}
	return r
}

func series(name, pos string) []link.SeriesRef { return []link.SeriesRef{{Name: name, Position: pos}} }

// TestMatcherOverMissShapes runs the matcher over a made-up library shaped
// like the misses of a real 747-title Audible account (2026-11-03): bare
// Audible titles against wordy server titles with no position, positions the
// listing fills from folder codes, a title that is only a number, a residual
// with no words, one title filed under two series names, "(Unabridged)" on a
// genre tail, publisher packs with range positions, sub-series volumes that
// all landed on one book, and titles that are not on the server. Every title
// lands where it should, or nowhere.
func TestMatcherOverMissShapes(t *testing.T) {
	books := []link.LibraryBook{
		// Bare Audible title vs a wordy title naming its place after the series
		// (the listing has no position: the "Series, Book N" marker confirms).
		{Path: "Vale/Ember Heart/EH01 - Coal Will", Title: "Coal Will: Ember Heart Series, Book 1", Authors: []string{"Ivo Vale"}, Series: series("Ember Heart", "")},
		{Path: "Vale/Ember Heart/EH02 - Ember Heart", Title: "Ember Heart - Book 2: Land of Ash", Authors: []string{"Ivo Vale"}, Series: series("Ember Heart", "")},
		{Path: "Vale/Ember Heart/EH03 - Ember Heart", Title: "Ember Heart - Book 3: Land of Smoke", Authors: []string{"Ivo Vale"}, Series: series("Ember Heart", "")},
		// The position the listing filled from the folder code (DM02 -> 2).
		{Path: "Rook/Glass Moons/GM01 - Oathbound", Title: "Oathbound Mender - Under the Glass Moons, Book 1", Authors: []string{"Pell Rook"}, Series: series("Under the Glass Moons", "1")},
		{Path: "Rook/Glass Moons/GM02 - Argo", Title: "Voyage on the Argo", Authors: []string{"Pell Rook"}, Series: series("Under the Glass Moons", "2")},
		// A title that is only a number.
		{Path: "Blair/1985", Title: "1985 (Unabridged)", Authors: []string{"Eric Blair"}},
		// The residual after the series has no words ("The").
		{Path: "Rimm/Mayor of Dunmere/MD01 - The Mayor of Dunmere", Title: "The Mayor of Dunmere", Authors: []string{"Ry Rimm"}, Series: series("Mayor of Dunmere", "")},
		// One title, filed under two series names.
		{Path: "Kade/Maze World/MW01 - Seer's Eye", Title: "Seer's Eye: Waking: A Maze World Novel", Authors: []string{"Aro Kade"}, Series: series("Maze World", "")},
		// "(Unabridged)" after the genre tail, the position from the folder code.
		{Path: "Garth/Prime Hunter/PH15 - 15", Title: "The Prime Hunter 15:  A LitRPG Adventure (Unabridged)", Authors: []string{"Zog Garth"}, Series: series("The Prime Hunter", "15")},
		{Path: "Garth/Prime Hunter/PH14 - 14", Title: "The Prime Hunter 14:  A LitRPG Adventure (Unabridged)", Authors: []string{"Zog Garth"}, Series: series("The Prime Hunter", "14")},
		// Publisher packs: the listing reads LAM05-06 as the range "5-6".
		{Path: "Sisk/Arch Mage/AM01-02", Title: "Arch Mage: Publisher's Pack - Arch Mage, Book 1, 2", Authors: []string{"Mo Sisk"}, Series: series("Arch Mage", "1-2")},
		{Path: "Sisk/Arch Mage/AM03-04", Title: "Arch Mage: Publisher's Pack 2 - Arch Mage, Book 3, 4", Authors: []string{"Mo Sisk"}, Series: series("Arch Mage", "3-4")},
		{Path: "Sisk/Arch Mage/AM05-06", Title: "Arch Mage: Publisher's Pack 3 - Arch Mage, Book 5, 6", Authors: []string{"Mo Sisk"}, Series: series("Arch Mage", "5-6")},
		{Path: "Sisk/Arch Mage/AM07-08", Title: "Arch Mage: Publisher's Pack 4 - Books 7-8", Authors: []string{"Mo Sisk"}, Series: series("Arch Mage", "7-8")},
		// Sub-series volumes: only volume 1 is here, as series book 4.
		{Path: "Yrs/Fate Cycle/FC04 - Strands of Fate - Volume 1", Title: "Strands of Fate: Volume 1", Authors: []string{"Yrs Ill"}, Series: series("Fate Cycle", "")},
		// A Dresden-like short story collection: junk title, position from the folder.
		{Path: "Butch/Wizard Files/WF15.5 Short Cases", Title: "The Wizard Files", Authors: []string{"Jim Butch"}, Series: series("The Wizard Files", "15.5")},
	}
	cases := []struct {
		rec  source.Record
		want string // "" = not on this server
	}{
		{audibleRec("B0SYN00001", "Ember Heart", "Ivo Vale", "Ember Heart Series", "1"), "Vale/Ember Heart/EH01 - Coal Will"},
		{audibleRec("B0SYN00002", "Ember Heart", "Ivo Vale", "Ember Heart Series", "2"), "Vale/Ember Heart/EH02 - Ember Heart"},
		{audibleRec("B0SYN00003", "Ember Heart", "Ivo Vale", "Ember Heart Series", "3"), "Vale/Ember Heart/EH03 - Ember Heart"},
		{audibleRec("B0SYN00004", "Ember Heart", "Ivo Vale", "Ember Heart Series", "4"), ""},
		{audibleRec("B0SYN00011", "Under the Glass Moons", "Pell Rook", "Under the Glass Moons", "1"), "Rook/Glass Moons/GM01 - Oathbound"},
		{audibleRec("B0SYN00012", "Under the Glass Moons 2", "Pell Rook", "Under the Glass Moons", "2"), "Rook/Glass Moons/GM02 - Argo"},
		{audibleRec("B0SYN00013", "Under the Glass Moons 3", "Pell Rook", "Under the Glass Moons", "3"), ""},
		{audibleRec("B0SYN00021", "1985", "Eric Blair", "", ""), "Blair/1985"},
		{audibleRec("B0SYN00022", "The Mayor of Dunmere", "Ry Rimm", "Dunmere", "1"), "Rimm/Mayor of Dunmere/MD01 - The Mayor of Dunmere"},
		{audibleRec("B0SYN00023", "Seer's Eye: Waking: A Maze World Novel", "Aro Kade", "Seer's Eye", "1"), "Kade/Maze World/MW01 - Seer's Eye"},
		{audibleRec("B0SYN00024", "The Prime Hunter 15", "Zog Garth", "The Prime Hunter", "15"), "Garth/Prime Hunter/PH15 - 15"},
		{audibleRec("B0SYN00031", "Arch Mage: Publisher's Pack 2", "Mo Sisk", "Arch Mage", "3-4"), "Sisk/Arch Mage/AM03-04"},
		{audibleRec("B0SYN00032", "Arch Mage: Publisher's Pack 3", "Mo Sisk", "Arch Mage", "5-6"), "Sisk/Arch Mage/AM05-06"},
		{audibleRec("B0SYN00033", "Arch Mage: Publisher's Pack 4", "Mo Sisk", "Arch Mage", "7-8"), "Sisk/Arch Mage/AM07-08"},
		// A single volume of the series is in none of the packs.
		{audibleRec("B0SYN00034", "Arch Mage 5", "Mo Sisk", "Arch Mage", "5"), ""},
		{audibleRec("B0SYN00041", "Short Cases", "Jim Butch", "The Wizard Files", "15.5"), "Butch/Wizard Files/WF15.5 Short Cases"},
		{audibleRec("B0SYN00051", "A House at the Edge", "Amy Sparks", "", ""), ""},
	}
	m := NewBookMatcher(books)
	for _, tc := range cases {
		b, ok := m.Match(tc.rec)
		got := ""
		if ok {
			got = b.Path
		}
		if got != tc.want {
			t.Errorf("%q #%s: matched %q, want %q", tc.rec.Title, tc.rec.SeriesPosition, got, tc.want)
		}
	}
}

// claimsOf is the matcher's claims for recs, as the poll makes them.
func claimsOf(books []link.LibraryBook, recs []source.Record) []Claim {
	m := NewBookMatcher(books)
	claims := make([]Claim, len(recs))
	for i, r := range recs {
		if b, score, ok := m.MatchScored(r); ok {
			agrees, disagrees := PositionAgrees(r, b)
			claims[i] = Claim{Path: b.Path, Score: score, Work: WorkKey(r), Agrees: agrees, Disagrees: disagrees}
		}
	}
	return claims
}

// TestMatcherContestedVolumes: sub-series volumes land on the one volume the
// library holds; KeepBest leaves it to none of them when they tie and nothing
// tells them apart, to the best when one scores higher, and to the volume
// whose number is the book's when the book has one (a folder code's on its
// main series included, whatever that series is called). A title whose
// number is another volume's is plainly not here: no contest.
func TestMatcherContestedVolumes(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "Yrs/Fate Cycle/FC04 - Strands of Fate - Volume 1", Title: "Strands of Fate - Volume 1", Authors: []string{"Yrs Ill"}, Series: series("Fate Cycle", "4")},
		{Path: "Val/Road to Craft/RC03", Title: "Road to Craft 3", Authors: []string{"Val Erio"}, Series: series("Road to Craft", "3")},
		// The Rivers of London shape: the folder code's series ("L-T") is not
		// the one Audible names, and the title names the series.
		{Path: "Ben Aar/LT02 - Moon over Tidewater", Title: "Moon over Tidewater: Lanterns of London, Book 2", Authors: []string{"Ben Aar"},
			Series: series("L-T", "2")},
		// The System Universe shape: the series' opening title (named for the
		// series) and the eighth both match the eighth's folder equally.
		{Path: "Kai Orr/GU08 - Grid Clash", Title: "Grid Universe: Grid Clash", Authors: []string{"Kai Orr"}, Series: series("G-U", "8")},
		// No number on the book: nothing tells two volumes apart.
		{Path: "Ann Vey/Harbor Street/Moon over Harbor", Title: "Moon over Harbor: Lights of Harbor, Book 2", Authors: []string{"Ann Vey"},
			Series: series("Harbor Street", "")},
	}
	recs := []source.Record{
		audibleRec("B0SYN00061", "Strands of Fate: Volume 1", "Yrs Ill", "Fate Cycle", "4"),
		audibleRec("B0SYN00062", "Strands of Fate: Volume 3", "Yrs Ill", "Fate Cycle", "6"),
		audibleRec("B0SYN00071", "Road to Craft", "Val Erio", "Road to Craft", "1"),
		audibleRec("B0SYN00073", "Road to Craft 3", "Val Erio", "Road to Craft", "3"),
		audibleRec("B0SYN00081", "Lanterns of London", "Ben Aar", "Lanterns of London", "1"),
		audibleRec("B0SYN00082", "Moon over Tidewater", "Ben Aar", "Lanterns of London", "2"),
		audibleRec("B0SYN00091", "Grid Universe", "Kai Orr", "Grid Universe", "1"),
		audibleRec("B0SYN00098", "Grid Clash", "Kai Orr", "Grid Universe", "8"),
		audibleRec("B0SYN00101", "Lights of Harbor", "Ann Vey", "Lights of Harbor", "1"),
		audibleRec("B0SYN00102", "Moon over Harbor", "Ann Vey", "Lights of Harbor", "2"),
	}
	claims := claimsOf(books, recs)
	for i, c := range claims {
		if c.Path == "" {
			t.Fatalf("%q matched nothing (claims %+v)", recs[i].Title, claims)
		}
	}
	keep, contested := KeepBest(claims)
	for i, want := range []struct{ keep, contested bool }{
		{true, false}, {false, false}, // FC04: volume 1 agrees, volume 3 is another number
		{false, false}, {true, false}, // RC03
		{false, false}, {true, false}, // LT02: book 2 by the folder code's number
		{false, false}, {true, false}, // GU08
		{false, true}, {false, true}, // Harbor: no number on the book, a tie
	} {
		if keep[i] != want.keep || contested[i] != want.contested {
			t.Errorf("%q: kept %v contested %v, want %v %v (claims %+v)", recs[i].Title, keep[i], contested[i], want.keep, want.contested, claims)
		}
	}
}

func TestKeepBest(t *testing.T) {
	cases := []struct {
		name      string
		claims    []Claim
		want      []bool
		contested []bool
	}{
		{"one claim each", []Claim{{Path: "a", Score: 1}, {Path: "b", Score: 1.2}}, []bool{true, true}, []bool{false, false}},
		{"best score keeps", []Claim{{Path: "a", Score: 1.5, Work: "x"}, {Path: "a", Score: 2.5, Work: "y"}, {Path: "a", Score: 1, Work: "z"}}, []bool{false, true, false}, []bool{true, false, true}},
		{"a tie keeps none", []Claim{{Path: "a", Score: 2.5, Work: "x"}, {Path: "a", Score: 2.5, Work: "y"}}, []bool{false, false}, []bool{true, true}},
		{"a tie below the best is no matter", []Claim{{Path: "a", Score: 1, Work: "x"}, {Path: "a", Score: 1, Work: "y"}, {Path: "a", Score: 3, Work: "z"}}, []bool{false, false, true}, nil},
		{"the same work twice keeps both", []Claim{{Path: "a", Score: 3, Work: "w"}, {Path: "a", Score: 3, Work: "w"}}, []bool{true, true}, []bool{false, false}},
		{"the same work tied with another keeps none", []Claim{{Path: "a", Score: 3, Work: "w"}, {Path: "a", Score: 3, Work: "w"}, {Path: "a", Score: 3, Work: "v"}}, []bool{false, false, false}, nil},
		{"no work key is never a duplicate", []Claim{{Path: "a", Score: 3}, {Path: "a", Score: 3}}, []bool{false, false}, nil},
		{"an ASIN match beats a title match", []Claim{{Path: "a", Score: 3, Work: "x"}, {Path: "a", Score: 10, Work: "y"}}, []bool{false, true}, nil},
		{"the same work below the best keeps it too", []Claim{{Path: "a", Score: 3, Work: "w"}, {Path: "a", Score: 10, Work: "w"}, {Path: "a", Score: 2, Work: "v"}}, []bool{true, true, false}, nil},
		{"no keeper, no same work below it", []Claim{{Path: "a", Score: 3, Work: "w"}, {Path: "a", Score: 3, Work: "v"}, {Path: "a", Score: 1, Work: "w"}}, []bool{false, false, false}, nil},
		{"no claim", []Claim{{}, {Path: "a", Score: 1}}, []bool{false, true}, []bool{false, false}},
		// The series position tells volumes apart.
		{"agreeing beats a tie", []Claim{{Path: "a", Score: 2.5, Work: "x", Agrees: true}, {Path: "a", Score: 2.5, Work: "y"}}, []bool{true, false}, []bool{false, true}},
		{"agreeing beats a higher score", []Claim{{Path: "a", Score: 3, Work: "x", Disagrees: true}, {Path: "a", Score: 2, Work: "y", Agrees: true}}, []bool{false, true}, []bool{false, false}},
		{"an ASIN match beats agreeing", []Claim{{Path: "a", Score: 2, Work: "x", Agrees: true}, {Path: "a", Score: 10, Work: "y", Disagrees: true}}, []bool{false, true}, []bool{true, false}},
		{"two agreeing works still contest", []Claim{{Path: "a", Score: 2, Work: "x", Agrees: true}, {Path: "a", Score: 2, Work: "y", Agrees: true}, {Path: "a", Score: 3, Work: "z", Disagrees: true}}, []bool{false, false, false}, []bool{true, true, false}},
		{"the better of two agreeing keeps", []Claim{{Path: "a", Score: 2, Work: "x", Agrees: true}, {Path: "a", Score: 2.5, Work: "y", Agrees: true}, {Path: "a", Score: 3, Work: "z"}}, []bool{false, true, false}, []bool{true, false, true}},
		{"a disagreeing loser is no contest", []Claim{{Path: "a", Score: 2, Work: "x", Disagrees: true}, {Path: "a", Score: 2, Work: "y", Disagrees: true}}, []bool{false, false}, []bool{false, false}},
		{"one claim keeps whatever its number", []Claim{{Path: "a", Score: 2, Work: "x", Disagrees: true}}, []bool{true}, []bool{false}},
		{"the agreeing work owned twice keeps both", []Claim{{Path: "a", Score: 2, Work: "w", Agrees: true}, {Path: "a", Score: 2, Work: "w", Agrees: true}, {Path: "a", Score: 2.5, Work: "v"}}, []bool{true, true, false}, []bool{false, false, true}},
	}
	for _, tc := range cases {
		keep, contested := KeepBest(tc.claims)
		if fmt.Sprint(keep) != fmt.Sprint(tc.want) {
			t.Errorf("%s: keep = %v, want %v", tc.name, keep, tc.want)
		}
		want := tc.contested
		if want == nil { // every loser contests
			for _, k := range tc.want {
				want = append(want, !k)
			}
		}
		if fmt.Sprint(contested) != fmt.Sprint(want) {
			t.Errorf("%s: contested = %v, want %v", tc.name, contested, want)
		}
	}
}

// TestPositionAgrees: the title's number against the book's in the title's
// series; the book's main series' number only when the title names no series,
// or when it is the folder code's leading the book's folder (bridge#43: a
// franchise's number says nothing about the title's).
func TestPositionAgrees(t *testing.T) {
	cases := []struct {
		name            string
		series          string // the title's ("" none)
		pos             string
		path            string // the book's ("" a folder with no code)
		book            []link.SeriesRef
		agree, disagree bool
	}{
		{"the same number", "Lanterns of London", "2", "", series("Lanterns of London", "2"), true, false},
		{"another number", "Lanterns of London", "1", "", series("Lanterns of London", "2"), false, true},
		{"2 is 2.0", "Lanterns of London", "2", "", series("Lanterns of London", "2.0"), true, false},
		{"the folder code's number", "Lanterns of London", "2", "Aar/LT02 - Moon over Tidewater", series("L-T", "2"), true, false},
		{"the folder code's other number", "Lanterns of London", "3", "Aar/LT 02 Moon", series("L-T", "2"), false, true},
		{"a franchise's number", "Lanterns of London", "1", "Aar/Moon over Tidewater", series("Aar Universe", "7"), false, false},
		{"a folder code of another number", "Lanterns of London", "2", "Aar/LT07 - Moon", series("Aar Universe", "2"), false, false},
		{"no code: the letters run on", "Lanterns of London", "2", "Aar/LT02x Moon", series("L-T", "2"), false, false},
		{"no code: not the series' initials", "Lanterns of London", "7", "Aar/Book 07 - Moon", series("Aar Universe", "7"), false, false},
		// The server's reading of a code, case by case: a range or a third
		// number joined on is never its first number, a decimal part can't be
		// backed off, and 0 is no position.
		{"no code: a range", "Lanterns of London", "5", "Aar/LT05-06 - Moon", series("L-T", "5"), false, false},
		{"no code: an en dash range", "Lanterns of London", "5", "Aar/LT05\u201306 - Moon", series("L-T", "5"), false, false},
		{"no code: a third number joined on", "Lanterns of London", "5", "Aar/LT05-06-07 - Moon", series("L-T", "5"), false, false},
		{"no code: letters after the decimal part", "Lanterns of London", "15", "Aar/LT15.5x Moon", series("L-T", "15"), false, false},
		{"no code: 0", "Lanterns of London", "0", "Aar/LT00 - Moon", series("L-T", "0"), false, false},
		{"a decimal code", "Lanterns of London", "15.5", "Aar/LT15.5 Moon", series("L-T", "15.5"), true, false},
		{"a hyphen that joins no number", "Lanterns of London", "5", "Aar/LT05-Moon", series("L-T", "5"), true, false},
		{"a code of some initials in order", "Lanterns of London", "3", "Aar/AU03 - Moon", series("Aar's Wide Universe", "3"), true, false},
		{"no series on the title: the main one", "", "2", "", series("Lanterns of London", "2"), true, false},
		{"no series on the title: another number", "", "3", "", series("Lanterns of London", "2"), false, true},
		{"the named series over the main one", "Lanterns of London", "4", "", []link.SeriesRef{{Name: "Universe", Position: "9"}, {Name: "lanterns of london", Position: "4"}}, true, false},
		{"the named series without a number", "Lanterns of London", "4", "", []link.SeriesRef{{Name: "Universe", Position: "4"}, {Name: "Lanterns of London", Position: ""}}, false, false},
		{"a range on the book", "Lanterns of London", "5", "", series("Lanterns of London", "5-6"), false, false},
		{"a range on the title", "Lanterns of London", "5-6", "", series("Lanterns of London", "5"), false, false},
		{"no number on the title", "Lanterns of London", "", "", series("Lanterns of London", "2"), false, false},
		{"no number on the book", "Lanterns of London", "2", "", series("Lanterns of London", ""), false, false},
		{"no series on the book", "Lanterns of London", "2", "", nil, false, false},
	}
	for _, tc := range cases {
		rec := audibleRec("B0POS00001", "Moon over Tidewater", "Ben Aar", tc.series, tc.pos)
		p := tc.path
		if p == "" {
			p = "Aar/Moon over Tidewater"
		}
		a, d := PositionAgrees(rec, link.LibraryBook{Path: p, Series: tc.book})
		if a != tc.agree || d != tc.disagree {
			t.Errorf("%s: agrees %v disagrees %v, want %v %v", tc.name, a, d, tc.agree, tc.disagree)
		}
	}
}

// TestFolderCodeIsServerVectors: folderCodeIs reads a folder code exactly as
// the server's metadata.FolderCodePosition does, on that function's own test
// table (name, series, the position it reads, a range's end; want 0 is no
// code). Every number in a name agrees only when the server reads it as the
// code's single position: never a range's start, a number cut short, or 0.
func TestFolderCodeIsServerVectors(t *testing.T) {
	number := regexp.MustCompile(`\d+(?:\.\d+)?`)
	for _, tc := range []struct {
		name, series string
		want, end    float64
	}{
		{"DH06 - Land of Magic", "Dragon Heart", 6, 0},
		{"BDM01 - Oathbound Healer", "Beneath the Dragoneye Moons", 1, 0},
		{"FWM01 - He Who Fights With Monsters - Book 1", "He Who Fights With Monsters", 1, 0},
		{"DF15.5 Brief Cases", "Dresden Files", 15.5, 0},
		{"PH15 - 15", "Primal Hunter", 15, 0},
		// A range (an omnibus) keeps its end.
		{"LAM05-06", "Legend of the Arch Magus", 5, 6},
		{"LAM05–06 - Omnibus", "Legend of the Arch Magus", 5, 6},
		{"DF15.5-16", "Dresden Files", 15.5, 16},
		{"LAM06-05", "Legend of the Arch Magus", 0, 0},
		{"LAM05-05", "Legend of the Arch Magus", 0, 0},
		{"LAM05-06x", "Legend of the Arch Magus", 5, 0},
		{"LAM05-0612", "Legend of the Arch Magus", 0, 0},
		{"LAM05-06-07", "Legend of the Arch Magus", 0, 0},
		{"LAM05-06 - 07 Omnibus", "Legend of the Arch Magus", 5, 6},
		// The code written twice on a range is an omnibus, not its first volume.
		{"DH01-DH03", "Dragon Heart", 0, 0},
		{"DH01 – dh03 Omnibus", "Dragon Heart", 0, 0},
		{"C1-C3 Box Set", "Dragon Heart", 0, 0},
		{"DH06 - DH Side Story", "Dragon Heart", 6, 0},
		// A pack word: a single position is none, a range still reads.
		{"LAM05-06a - Omnibus", "Legend of the Arch Magus", 0, 0},
		{"DH01 - Books 1-3", "Dragon Heart", 0, 0},
		{"DH01 – books 1 – 3", "Dragon Heart", 0, 0},
		{"DH06 Boxed Set", "Dragon Heart", 0, 0},
		{"DH06 - Boxset", "Dragon Heart", 0, 0},
		{"DH06 - The Complete Collection", "Dragon Heart", 0, 0},
		{"C02 - Box Set", "Dragon Heart", 0, 0},
		{"DH06 - Book 6", "Dragon Heart", 6, 0},
		{"DH06 - Collectors", "Dragon Heart", 6, 0},
		{"DH06 - 07 Land", "Dragon Heart", 6, 0},
		{"DH06-2019 - Land", "Dragon Heart", 6, 0},
		{"DH06–07x", "Dragon Heart", 6, 0},
		{"DH06-07xyz", "Dragon Heart", 0, 0},
		{"DH06-2019-07", "Dragon Heart", 0, 0},
		{"c02 Soulsmith", "Dragon Heart", 2, 0},
		{"C02-03", "Dragon Heart", 2, 3},
		{"C02", "", 0, 0},
		{"DH06-Land", "Dragon Heart", 6, 0},
		{"HALO 07-08", "Halo", 0, 0},
		{"EE07 - Forged in Blood Part 2", "Emperor's Edge", 7, 0},
		{"RADT01 - Reborn as a Demonic Tree", "Reborn as a Demonic Tree", 1, 0},
		{"WT01", "World-Tree", 1, 0},
		{"C04 - Skysworn", "Cradle", 4, 0},
		{"dh06 - land of magic.m4b", "Dragon Heart", 6, 0},
		// A franchise's own numbering: the code isn't the series' initials.
		{"HALO 07 - Evolutions (M4B)", "Halo", 0, 0},
		{"MB01 - The Final Empire", "Mistborn", 0, 0},
		{"AF05 - Legion's Fifth Vault", "Divine Apostasy", 0, 0},
		{"CD1", "02 - How to Be a Pirate", 0, 0},
		// No code, or not one.
		{"01. The Enchanted Wood", "The Faraway Tree", 0, 0},
		{"Brief Cases", "Dresden Files", 0, 0},
		{"DH06x - Land", "Dragon Heart", 0, 0},
		{"DF15.5x Brief Cases", "Dresden Files", 0, 0},
		{"DF15. Brief Cases", "Dresden Files", 15, 0},
		{"DH1234 - Land", "Dragon Heart", 0, 0},
		{"DH0 - Prologue", "Dragon Heart", 0, 0},
		{"DH1999", "Dragon Heart", 0, 0},
		{"DH06", "", 0, 0},
	} {
		// Each number in the name, whole, without its decimal part and cut to
		// the three digits a code's number holds, and the server's reading.
		probes := map[float64]bool{tc.want: true, tc.end: true}
		for _, n := range number.FindAllString(tc.name, -1) {
			whole, _, _ := strings.Cut(n, ".")
			for _, s := range []string{n, whole, whole[:min(len(whole), 3)]} {
				if v, err := strconv.ParseFloat(s, 64); err == nil {
					probes[v] = true
				}
			}
		}
		single := tc.want != 0 && tc.end == 0
		for p := range probes {
			pos := strconv.FormatFloat(p, 'f', -1, 64)
			if got, want := folderCodeIs("Aar/"+tc.name, tc.series, pos), single && p == tc.want; got != want {
				t.Errorf("folderCodeIs(%q, %q, %s) = %v, want %v", tc.name, tc.series, pos, got, want)
			}
		}
	}
}

// TestMatcherRangeBooks: a book whose position is a range (a pack's folder,
// "LAM05-06" listed as "5-6") is matched by the pack starting where it does,
// before a single volume's folder at that position; never by a single volume
// (by position or by title), so it never takes a volume's ASIN by enrichment;
// and never by a pack starting elsewhere.
func TestMatcherRangeBooks(t *testing.T) {
	pack := audibleRec("B0SYN00081", "Iron Vale: Books 5-6", "Ona Fell", "Iron Vale", "5-6")
	vol5 := audibleRec("B0SYN00082", "Iron Vale 5", "Ona Fell", "Iron Vale", "5")
	ash := audibleRec("B0SYN00083", "Ash Road", "Ona Fell", "Iron Vale", "5")
	cases := []struct {
		name  string
		books []link.LibraryBook
		rec   source.Record
		want  string
	}{
		{"a single volume by position", []link.LibraryBook{
			{Path: "Fell/Iron Vale/IV05-06", Title: "Iron Vale", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "5-6")},
		}, vol5, ""},
		{"a single volume by title", []link.LibraryBook{
			{Path: "Fell/Iron Vale/IV05-06", Title: "Ash Road and Ember Road", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "5-6")},
		}, ash, ""},
		{"a range in another series too", []link.LibraryBook{
			{Path: "Fell/Iron Vale/IV05-06", Title: "Iron Vale", Authors: []string{"Ona Fell"},
				Series: []link.SeriesRef{{Name: "Fell Worlds", Position: "5"}, {Name: "Iron Vale", Position: "5-6"}}},
		}, vol5, ""},
		{"the pack on its range", []link.LibraryBook{
			{Path: "Fell/Iron Vale/IV05-06", Title: "Iron Vale", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "5-6")},
		}, pack, "Fell/Iron Vale/IV05-06"},
		{"the pack on its range before volume 5", []link.LibraryBook{
			{Path: "Fell/Iron Vale/IV05 - Iron Vale", Title: "Iron Vale 5", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "5")},
			{Path: "Fell/Iron Vale/IV05-06", Title: "Iron Vale", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "5-6")},
		}, pack, "Fell/Iron Vale/IV05-06"},
		{"the pack on a folder at its start", []link.LibraryBook{
			{Path: "Fell/Iron Vale/IV05 - Books 5-6", Title: "Iron Vale", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "5")},
		}, pack, "Fell/Iron Vale/IV05 - Books 5-6"},
		{"not another pack", []link.LibraryBook{
			{Path: "Fell/Iron Vale/IV03-04", Title: "Iron Vale: Books 3-4", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "3-4")},
		}, pack, ""},
		{"a range's own ASIN", []link.LibraryBook{
			{Path: "Fell/Iron Vale/IV05-06", Title: "Iron Vale", Authors: []string{"Ona Fell"}, ASIN: "B0SYN00082", Series: series("Iron Vale", "5-6")},
		}, vol5, "Fell/Iron Vale/IV05-06"},
	}
	for _, tc := range cases {
		got := ""
		if b, ok := NewBookMatcher(tc.books).Match(tc.rec); ok {
			got = b.Path
		}
		if got != tc.want {
			t.Errorf("%s: %q #%s matched %q, want %q", tc.name, tc.rec.Title, tc.rec.SeriesPosition, got, tc.want)
		}
	}
	// The folder the old listing gave as "5" was volume 5's by position: the
	// range is what keeps them apart.
	old := []link.LibraryBook{{Path: "Fell/Iron Vale/IV05-06", Title: "Iron Vale", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "5")}}
	if _, ok := NewBookMatcher(old).Match(vol5); !ok {
		t.Error("a single position no longer matches its volume")
	}
}

func TestRangeStart(t *testing.T) {
	for pos, want := range map[string]float64{"5-6": 5, "0.5-4": 0.5, " 7 - 9 ": 7, "1–3": 1} {
		if v, ok := rangeStart(pos); !ok || v != want {
			t.Errorf("rangeStart(%q) = %v %v, want %v", pos, v, ok, want)
		}
	}
	for _, pos := range []string{"", "5", "5-", "a-b", "Book 1-2", "5-6-7"} {
		if _, ok := rangeStart(pos); ok {
			t.Errorf("rangeStart(%q) is a range", pos)
		}
	}
}

// WorkKey: one book owned twice (title and authors alike, case, spacing and
// author order aside) is one work; another author, title or position is not.
func TestWorkKey(t *testing.T) {
	base := source.Record{Title: "Equal Measures", Authors: []string{"Tess Prat", "Ann Co"}, SeriesPosition: "3"}
	same := []source.Record{
		{Title: "equal  measures", Authors: []string{"Ann Co", "tess prat"}, SeriesPosition: "3"},
		{Title: "Equal Measures", Authors: []string{"Tess Prat", "Ann Co", "Tess Prat"}, SeriesPosition: " 3 "},
	}
	for _, r := range same {
		if WorkKey(r) != WorkKey(base) {
			t.Errorf("%+v is not the same work as %+v", r, base)
		}
	}
	other := []source.Record{
		{Title: "Equal Measures", Authors: []string{"Tess Prat"}, SeriesPosition: "3"},
		{Title: "Equal Measures 2", Authors: []string{"Tess Prat", "Ann Co"}, SeriesPosition: "3"},
		{Title: "Equal Measures", Authors: []string{"Tess Prat", "Ann Co"}, SeriesPosition: "4"},
	}
	for _, r := range other {
		if WorkKey(r) == WorkKey(base) {
			t.Errorf("%+v is the same work as %+v", r, base)
		}
	}
	if WorkKey(source.Record{Title: "X", Author: "A"}) != WorkKey(source.Record{Title: "X", Authors: []string{"A"}}) {
		t.Error("the first author alone is not the author set")
	}
	// A title that folds to nothing is no identity, so two such records never
	// share a book as one work.
	blank := source.Record{Title: " -- ", Authors: []string{"Tess Prat"}, SeriesPosition: "3"}
	if k := WorkKey(blank); k != "" {
		t.Errorf("a wordless title's key = %q, want none", k)
	}
	if keep, _ := KeepBest([]Claim{{Path: "a", Score: 2, Work: WorkKey(blank)}, {Path: "a", Score: 2, Work: WorkKey(blank)}}); keep[0] || keep[1] {
		t.Errorf("two wordless titles tied on one book kept %v", keep)
	}
}

// bridge#43: volumes Audible lists under their series' bare name with no
// position are no one work (a tie between them keeps none); a number, given
// as a position or only as the parsed sequence, still tells them apart, and
// a standalone title with no series keeps its key.
func TestWorkKeyBareSeriesTitle(t *testing.T) {
	bare := source.Record{Title: "The Long Road", Series: "The Long Road", Authors: []string{"Ann Co"}}
	if k := WorkKey(bare); k != "" {
		t.Errorf("a bare series title's key = %q, want none", k)
	}
	if keep, contested := KeepBest([]Claim{{Path: "a", Score: 2, Work: WorkKey(bare)}, {Path: "a", Score: 2, Work: WorkKey(bare)}}); keep[0] || keep[1] ||
		!contested[0] || !contested[1] {
		t.Errorf("two bare-series volumes tied on one book: keep %v contested %v", keep, contested)
	}
	one, two := bare, bare
	one.HasSequence, one.Sequence = true, 1
	two.HasSequence, two.Sequence = true, 2
	if WorkKey(one) == "" || WorkKey(one) == WorkKey(two) {
		t.Errorf("sequences: %q vs %q", WorkKey(one), WorkKey(two))
	}
	if WorkKey(source.Record{Title: "The Long Road", Series: "The Long Road", SeriesPosition: "2", Authors: []string{"Ann Co"}}) == "" {
		t.Error("a bare series title with a position lost its key")
	}
	// The series named as the matcher names it: Audible's trailing " Series",
	// a leading article.
	for _, series := range []string{"The Long Road Series", "Long Road"} {
		if k := WorkKey(source.Record{Title: "The Long Road", Series: series, Authors: []string{"Ann Co"}}); k != "" {
			t.Errorf("a bare title in %q: key = %q, want none", series, k)
		}
	}
	// Two non-Latin names that both reduce to nothing under the ASCII-only
	// normalising are told apart.
	if WorkKey(source.Record{Title: "長い道", Series: "別の道", Authors: []string{"Ann Co"}}) == "" {
		t.Error("a non-Latin titled volume lost its key")
	}
	if WorkKey(source.Record{Title: "The Long Road", Authors: []string{"Ann Co"}}) == "" {
		t.Error("a standalone title lost its key")
	}
	if WorkKey(source.Record{Title: "Into the Dark", Series: "The Long Road", Authors: []string{"Ann Co"}}) == "" {
		t.Error("a titled volume with no position lost its key")
	}
}

// Twins: the books the library holds twice (title, authors and series alike,
// case and order aside) are each other's twins; another author, series
// position or title is not.
func TestTwins(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "A/One", Title: "Equal Measures", Authors: []string{"Tess Prat", "Ann Co"}, Series: []link.SeriesRef{{Name: "Measures", Position: "1"}}},
		{Path: "B/One", Title: "equal measures", Authors: []string{"ann co", "Tess Prat"}, Series: []link.SeriesRef{{Name: "The Measures", Position: "1"}}},
		{Path: "A/Two", Title: "Equal Measures", Authors: []string{"Tess Prat", "Ann Co"}, Series: []link.SeriesRef{{Name: "Measures", Position: "2"}}},
		{Path: "A/Other", Title: "Equal Measures", Authors: []string{"Tess Prat"}, Series: []link.SeriesRef{{Name: "Measures", Position: "1"}}},
		{Path: "X/1", Title: " -- "},
		{Path: "X/2", Title: " -- "},
		// Two non-Latin series the ASCII-only normalising reduces to nothing.
		{Path: "J/1", Title: "Equal Measures", Authors: []string{"Tess Prat"}, Series: []link.SeriesRef{{Name: "長い道", Position: "1"}}},
		{Path: "J/2", Title: "Equal Measures", Authors: []string{"Tess Prat"}, Series: []link.SeriesRef{{Name: "別の道", Position: "1"}}},
		// One position written two ways is one position (bridge#64).
		{Path: "N/1", Title: "Second Wind", Authors: []string{"Tess Prat"}, Series: []link.SeriesRef{{Name: "Winds", Position: "1"}}},
		{Path: "N/2", Title: "Second Wind", Authors: []string{"Tess Prat"}, Series: []link.SeriesRef{{Name: "Winds", Position: " 1.0"}}},
		{Path: "N/3", Title: "Second Wind", Authors: []string{"Tess Prat"}, Series: []link.SeriesRef{{Name: "Winds", Position: "1.5"}}},
	}
	got := Twins(books)
	if len(got) != 4 || !got["A/One"] || !got["B/One"] || !got["N/1"] || !got["N/2"] {
		t.Fatalf("twins = %v", got)
	}
}

// SpanMismatch checks both directions before ASIN enrichment, and that two
// packs span the same volumes.
func TestSpanMismatch(t *testing.T) {
	pack := audibleRec("B0SYN00081", "Iron Vale: Books 5-6", "Ona Fell", "Iron Vale", "5-6")
	longer := audibleRec("B0SYN00085", "Iron Vale: Books 5-8", "Ona Fell", "Iron Vale", "5-8")
	vol5 := audibleRec("B0SYN00082", "Iron Vale 5", "Ona Fell", "Iron Vale", "5")
	single := link.LibraryBook{Path: "Fell/Iron Vale/IV05 - Ash Road", Series: series("Iron Vale", "5")}
	ranged := link.LibraryBook{Path: "Fell/Iron Vale/IV05-06", Series: series("Iron Vale", "5-6")}
	opening := link.LibraryBook{Path: "Fell/Iron Vale/IV01-03", Series: series("Iron Vale", "1-3")}
	box := audibleRec("B0SYN00083", "Iron Vale Box Set", "Ona Fell", "Iron Vale", "")
	boundedBox := audibleRec("B0SYN00086", "Iron Vale Box Set: Books 5-6", "Ona Fell", "Iron Vale", "")
	numberedBox := audibleRec("B0SYN00084", box.Title, box.Author, box.Series, "1")
	labeled := link.LibraryBook{Path: "Fell/Iron Vale/Iron Vale Box Set (Books 4-6)", Title: "Iron Vale Box Set", Series: series("Iron Vale", "")}
	labeledFirst := link.LibraryBook{Path: "Fell/Iron Vale/Iron Vale Box Set (Books 1-3)", Title: "Iron Vale Box Set", Series: series("Iron Vale", "")}
	unbounded := link.LibraryBook{Path: "Fell/Iron Vale/Iron Vale Box Set", Title: "Iron Vale Box Set", Series: series("Iron Vale", "")}
	firstBox := audibleRec("B0SYN00087", "Iron Vale Box Set: Books 1-3", "Ona Fell", "Iron Vale", "")
	for _, tc := range []struct {
		rec  source.Record
		b    link.LibraryBook
		want bool
	}{
		{pack, single, true},
		{pack, link.LibraryBook{Path: "Fell/Iron Vale"}, true},
		{pack, ranged, false},
		{longer, ranged, true}, // starts where the folder does, ends elsewhere
		{vol5, single, false},
		{vol5, ranged, true},
		{box, opening, false},
		{box, ranged, true}, // no bounds: a later pack's folder may be another box set
		{boundedBox, ranged, false},
		{boundedBox, opening, true},
		{box, single, true},
		{numberedBox, ranged, true},
		{box, labeledFirst, false},
		{box, labeled, true}, // the folder names a later pack
		{boundedBox, labeled, true},
		{vol5, labeled, true},
		// A labeled pack stating no bounds is taken for the opening one, as a
		// title stating none is: a title naming later bounds keeps off it.
		{box, unbounded, false},
		{firstBox, unbounded, false},
		{boundedBox, unbounded, true},
	} {
		if got := SpanMismatch(tc.rec, tc.b); got != tc.want {
			t.Errorf("SpanMismatch(%q #%s, %q) = %v, want %v", tc.rec.Title, tc.rec.SeriesPosition, tc.b.Path, got, tc.want)
		}
	}
}

func TestPositionlessPack(t *testing.T) {
	const name = "Iron Vale"
	book := func(path, title, pos string) link.LibraryBook {
		return link.LibraryBook{Path: path, Title: title, Authors: []string{"Ona Fell"}, Series: series(name, pos)}
	}
	one := book("one", "Iron Vale", "1")
	alias := book("franchise", name, "1")
	alias.Series = append(alias.Series, link.SeriesRef{Name: "Iron Vale Worlds", Position: "1"})
	first := book("first pack", "Iron Vale Box Set", "1-3")
	later := book("later pack", "Iron Vale Box Set", "4-6")
	for _, tc := range []struct {
		name, title, pos string
		books            []link.LibraryBook
		want             string
	}{
		{"box set against range", "Iron Vale Box Set", "", []link.LibraryBook{one, first}, first.Path},
		{"folder names the pack, track title is uninformative", "Iron Vale Omnibus, Books 1-3", "", []link.LibraryBook{book("Book 1 - Omnibus, Books 1-3", "End Credits", "1")}, "Book 1 - Omnibus, Books 1-3"},
		{"numbered pack with its own folder label", "Iron Vale: Origin Box Set (5-8)", "2", []link.LibraryBook{book("Book 2 - Iron Vale Origin Box Set (5-8)", "End Credits", "2")}, "Book 2 - Iron Vale Origin Box Set (5-8)"},
		{"range folder named for series", "Iron Vale Box Set", "", []link.LibraryBook{book("pack", name, "1-3")}, "pack"},
		{"single against range", "Iron Vale", "1", []link.LibraryBook{first}, ""},
		{"single without position against range", "Iron Vale", "", []link.LibraryBook{first}, ""},
		{"pack against single", "Iron Vale Box Set", "", []link.LibraryBook{one}, ""},
		{"range against opening volume under a second series", "Iron Vale: Books 1-3", "1-3", []link.LibraryBook{alias}, ""},
		{"range against range", "Iron Vale: Books 1-3", "1-3", []link.LibraryBook{one, later, first}, first.Path},
		{"other bounds in title", "Iron Vale: Books 4-6", "", []link.LibraryBook{first, later}, later.Path},
		{"range cannot fall back to another labeled pack", "Iron Vale Box Set: Books 1-3", "1-3", []link.LibraryBook{book("other", "Iron Vale Box Set: Books 4-6", "1")}, ""},
		{"same start, other end", "Iron Vale: Books 1-5", "", []link.LibraryBook{first}, ""},
		// A labeled pack's bounds count from its folder too, its title stating none.
		{"bounds in the folder only", "Iron Vale Box Set: Books 1-3", "", []link.LibraryBook{book("Iron Vale Box Set (Books 4-6)", "Iron Vale Box Set", "")}, ""},
		{"range against folder bounds", "Iron Vale: Books 1-3", "1-3", []link.LibraryBook{book("Iron Vale Box Set (Books 4-6)", "Iron Vale Box Set", ""),
			book("Iron Vale Box Set (Books 1-3)", "Iron Vale Box Set", "")}, "Iron Vale Box Set (Books 1-3)"},
		{"equally good packs", "Iron Vale Box Set", "", []link.LibraryBook{later, first}, ""},
		{"stated single position", "Iron Vale Box Set", "1", []link.LibraryBook{first}, ""},
		{"other series by same author", "Iron Vale Box Set", "", []link.LibraryBook{{Path: "other", Title: "North Wind Box Set", Authors: one.Authors, Series: series("North Wind", "1-3")}}, ""},
		// One pack listed under two spellings of its series scores alike under
		// each: that is one book, not a tie between two.
		{"pack under two spellings of its series", "Iron Vale Box Set", "", []link.LibraryBook{{Path: "both", Title: name, Authors: one.Authors,
			Series: []link.SeriesRef{{Name: name, Position: "1-3"}, {Name: "The Iron Vale Series", Position: "1-3"}}}}, "both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := audibleRec("B0SYN00081", tc.title, "Ona Fell", name, tc.pos)
			got, ok := MatchInBooks(tc.books, rec)
			if got.Path != tc.want || ok != (tc.want != "") {
				t.Fatalf("matched %q (%v), want %q", got.Path, ok, tc.want)
			}
		})
	}
}

// A store can leave a collection label off a title ("Four" for "Four: A
// Divergent Collection"): a single volume claims a book labeled as a pack only
// when nothing scores better, never when it is the series-named opening
// volume, and never takes its ASIN (SpanMismatch).
func TestLabeledPackOverride(t *testing.T) {
	four := link.LibraryBook{Path: "Roth/Divergent/Four", Title: "Four: A Divergent Collection", Authors: []string{"Vera Roth"}, Series: series("Divergent", "")}
	box := link.LibraryBook{Path: "Roth/Divergent/Divergent Box Set", Title: "Divergent Box Set", Authors: four.Authors, Series: series("Divergent", "")}
	opener := link.LibraryBook{Path: "Roth/Divergent/Divergent", Title: "Divergent", Authors: four.Authors, Series: series("Divergent", "1")}
	for _, tc := range []struct {
		name, title, pos string
		books            []link.LibraryBook
		want             string
	}{
		{"label left off the title", "Four", "", []link.LibraryBook{four}, four.Path},
		{"series-named opener", "Divergent", "1", []link.LibraryBook{box}, ""},
		{"opener beside its own volume", "Divergent", "1", []link.LibraryBook{box, opener}, opener.Path},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := audibleRec("B0SYN00091", tc.title, "Vera Roth", "Divergent", tc.pos)
			got, ok := MatchInBooks(tc.books, rec)
			if got.Path != tc.want || ok != (tc.want != "") {
				t.Fatalf("matched %q (%v), want %q", got.Path, ok, tc.want)
			}
			if ok && got.Path == four.Path && !SpanMismatch(rec, got) {
				t.Fatalf("a single volume on a labeled pack may take its ASIN")
			}
		})
	}
}
