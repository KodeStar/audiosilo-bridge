package importjob

import (
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/placement"
	"github.com/kodestar/audiosilo-bridge/internal/source"
)

// planOptions and planAll plan records in turn over one library's books.
type planOptions struct {
	Mode     placement.Mode
	Template string
	Books    []link.LibraryBook
}

func planAll(recs []source.Record, opts planOptions) ([]PlannedItem, error) {
	p := NewPlanner(opts.Books)
	items := make([]PlannedItem, 0, len(recs))
	for _, rec := range recs {
		item, err := p.Plan(rec, opts.Mode, opts.Template)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func TestPlan(t *testing.T) {
	// An existing sibling in the Cradle series (drives the C##/width-2 convention).
	books := []link.LibraryBook{
		{Path: "Will Wight/Cradle/C01 - Unsouled", Title: "Unsouled", Authors: []string{"Will Wight"}, Series: []link.SeriesRef{{Name: "Cradle", Position: "1"}}},
	}
	recs := []source.Record{
		{FileName: "Soulsmith.m4b", Title: "Soulsmith", Author: "Will Wight", Series: "Cradle", Sequence: 2, HasSequence: true},
		{FileName: "Unsouled.m4b", Title: "Unsouled", Author: "Will Wight", Series: "Cradle", Sequence: 1, HasSequence: true},
	}
	items, err := planAll(recs, planOptions{Mode: placement.ModeAuto, Books: books})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items", len(items))
	}

	// Book 2 (new) anchors on sibling C01 → C02.
	if items[0].RelPath != "Will Wight/Cradle/C02 - Soulsmith/Soulsmith.m4b" {
		t.Fatalf("Soulsmith placement = %q", items[0].RelPath)
	}
}

// The real He Who Fights with Monsters case: the server's tags credit both authors
// and spell the series folder "WIth", while the Audible record carries only its
// first author and a title that cleans down to the bare number. The book must land
// in the existing series folder, named like its siblings - not in a near-duplicate
// "He Who Fights with Monsters/HWFWM13 - 13" beside it.
func TestPlanMultiAuthorSiblingsBareTitle(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "Shirtaloon/He Who Fights WIth Monsters/FWM01 - He Who Fights With Monsters - Book 1", Title: "He Who Fights with Monsters", Authors: []string{"Shirtaloon, Travis Deverell"}, Series: []link.SeriesRef{{Name: "He Who Fights WIth Monsters"}}},
		{Path: "Shirtaloon/He Who Fights WIth Monsters/FWM12 - He Who Fights With Monsters - Book 12", Title: "He Who Fights with Monsters 12: A LitRPG Adventure", Authors: []string{"Shirtaloon, Travis Deverell"}, Series: []link.SeriesRef{{Name: "He Who Fights WIth Monsters"}}},
	}
	recs := []source.Record{{
		FileName: "He Who Fights with Monsters 13 - A LitRPG Adventure.m4b",
		Title:    "He Who Fights with Monsters 13: A LitRPG Adventure",
		Author:   "Shirtaloon", Series: "He Who Fights with Monsters", Sequence: 13, HasSequence: true,
	}}
	items, err := planAll(recs, planOptions{Mode: placement.ModeAuto, Books: books})
	if err != nil {
		t.Fatal(err)
	}
	want := "Shirtaloon/He Who Fights WIth Monsters/FWM13 - He Who Fights With Monsters - Book 13/He Who Fights with Monsters 13 - A LitRPG Adventure.m4b"
	if items[0].RelPath != want {
		t.Fatalf("placement:\n got: %q\nwant: %q", items[0].RelPath, want)
	}
}

// A book in several series anchors a placement in any of them, at its position
// in that series.
func TestPlanSiblingInSecondSeries(t *testing.T) {
	books := []link.LibraryBook{{
		Path: "Pratchett/Witches/W01 - Equal Rites", Title: "Equal Rites", Authors: []string{"Terry Pratchett"},
		Series: []link.SeriesRef{{Name: "Discworld", Position: "3"}, {Name: "Witches", Position: "1"}},
	}}
	recs := []source.Record{{FileName: "wa.m4b", Title: "Wyrd Sisters", Author: "Terry Pratchett", Series: "Witches", Sequence: 2, HasSequence: true}}
	items, err := planAll(recs, planOptions{Mode: placement.ModeAuto, Books: books})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].RelPath != "Pratchett/Witches/W02 - Wyrd Sisters/wa.m4b" {
		t.Fatalf("placement = %q", items[0].RelPath)
	}
}

func TestPlanTemplateMode(t *testing.T) {
	recs := []source.Record{{FileName: "Warbreaker.m4b", Title: "Warbreaker", Author: "Brandon Sanderson"}}
	items, err := planAll(recs, planOptions{
		Mode: placement.ModeTemplate, Template: "{Author}/{Title}/{OriginalFilename}",
	})
	if err != nil {
		t.Fatal(err)
	}
	if items[0].RelPath != "Brandon Sanderson/Warbreaker/Warbreaker.m4b" {
		t.Fatalf("template placement = %q", items[0].RelPath)
	}
}

// A title that opens with an article before its series name is cleaned of
// both: never "The : Books 1-3" or "The 3" (a bare number keeps its series
// words, as CleanTitle gives any title that is only its series and number).
func TestPlanArticleBeforeSeries(t *testing.T) {
	for _, c := range []struct{ title, series, want string }{
		{"The Orchard Cycle: Books 1-3", "Orchard Cycle", "Mara Quill/Orchard Cycle/Books 1-3/x.m4b"},
		{"The Mayor of Noobtown 3", "Mayor of Noobtown", "Mara Quill/Mayor of Noobtown/Mayor of Noobtown 3/x.m4b"},
		{"Orchard Cycle: Books 1-3", "Orchard Cycle", "Mara Quill/Orchard Cycle/Books 1-3/x.m4b"},
		{"The Orchard Cycles Reborn", "Orchard Cycle", "Mara Quill/Orchard Cycle/The Orchard Cycles Reborn/x.m4b"},
	} {
		items, err := planAll([]source.Record{{FileName: "x.m4b", Title: c.title, Author: "Mara Quill", Series: c.series}},
			planOptions{Mode: placement.ModeAuto})
		if err != nil {
			t.Fatal(err)
		}
		if items[0].RelPath != c.want {
			t.Errorf("%q: got %q, want %q", c.title, items[0].RelPath, c.want)
		}
	}
}

// "<Series> N: Subtitle" cleans to "N: Subtitle" once the series is stripped;
// the folder code carries the number already, so the folder is named by the
// subtitle alone ("2.5 - Margins", never "2.5 - 2.5 - Margins"). The file keeps
// its original name, as every placement does. A number that is not the
// record's position, a title of its own and a bare number stay as they were.
func TestPlanSeriesNumberTitle(t *testing.T) {
	plain := []link.LibraryBook{{
		Path: "Wen Liang/Mapmakers/1 - The Cartographer's Apprentice", Title: "The Cartographer's Apprentice",
		Authors: []string{"Wen Liang"}, Series: []link.SeriesRef{{Name: "Mapmakers", Position: "1"}},
	}}
	coded := []link.LibraryBook{{
		Path: "Wen Liang/Mapmakers/MM01 - The Cartographer's Apprentice", Title: "The Cartographer's Apprentice",
		Authors: []string{"Wen Liang"}, Series: []link.SeriesRef{{Name: "Mapmakers", Position: "1"}},
	}}
	for _, c := range []struct {
		name  string
		books []link.LibraryBook
		title string
		seq   float64
		want  string
	}{
		{"decimal", plain, "Mapmakers 2.5: Margins", 2.5, "Wen Liang/Mapmakers/2.5 - Margins/x.m4b"},
		{"integer", plain, "Mapmakers 3: The Coast", 3, "Wen Liang/Mapmakers/3 - The Coast/x.m4b"},
		{"hyphen", plain, "Mapmakers 3 - The Coast", 3, "Wen Liang/Mapmakers/3 - The Coast/x.m4b"},
		{"en dash", plain, "Mapmakers 3 \u2013 The Coast", 3, "Wen Liang/Mapmakers/3 - The Coast/x.m4b"},
		{"article", plain, "The Mapmakers 3: The Coast", 3, "Wen Liang/Mapmakers/3 - The Coast/x.m4b"},
		{"shortcode decimal", coded, "Mapmakers 2.5: Margins", 2.5, "Wen Liang/Mapmakers/MM02.5 - Margins/x.m4b"},
		{"shortcode integer", coded, "Mapmakers 3: The Coast", 3, "Wen Liang/Mapmakers/MM03 - The Coast/x.m4b"},
		{"new series", nil, "Mapmakers 3: The Coast", 3, "Wen Liang/Mapmakers/M03 - The Coast/x.m4b"},
		{"marker already handled", plain, "Mapmakers, Book 3: The Coast", 3, "Wen Liang/Mapmakers/3 - The Coast/x.m4b"},
		{"not this position", plain, "Mapmakers 7: The Coast", 3, "Wen Liang/Mapmakers/3 - 7 - The Coast/x.m4b"},
		{"title of its own", plain, "3 Days of Rain", 3, "Wen Liang/Mapmakers/3 - 3 Days of Rain/x.m4b"},
	} {
		recs := []source.Record{{FileName: "x.m4b", Title: c.title, Author: "Wen Liang", Series: "Mapmakers", Sequence: c.seq, HasSequence: true}}
		items, err := planAll(recs, planOptions{Mode: placement.ModeAuto, Books: c.books})
		if err != nil {
			t.Fatal(err)
		}
		if items[0].RelPath != c.want {
			t.Errorf("%s (%q): got %q, want %q", c.name, c.title, items[0].RelPath, c.want)
		}
	}
}

// TestPlannerKindAndSiblings: each item says how it was placed and how many of
// its series' books the library has; the index finds the author's books of the
// series in list order; a template that can't place a record still says its
// kind.
func TestPlannerKindAndSiblings(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "Will Wight/Cradle/C01 - Unsouled", Title: "Unsouled", Authors: []string{"Will Wight"}, Series: []link.SeriesRef{{Name: "Cradle", Position: "1"}}},
		{Path: "Other/Cradle/X01", Title: "Other Cradle", Authors: []string{"Someone Else"}, Series: []link.SeriesRef{{Name: "Cradle", Position: "1"}}},
		{Path: "Will Wight/Cradle/C03 - Blackflame", Title: "Blackflame", Authors: []string{"Will Wight"},
			Series: []link.SeriesRef{{Name: "The Cradle", Position: "3"}, {Name: "cradle", Position: "3"}}},
		{Path: "Will Wight/Elder Empire/EE01", Title: "Of Sea and Shadow", Authors: []string{"Will Wight"}, Series: []link.SeriesRef{{Name: "Elder Empire", Position: "1"}}},
	}
	recs := []source.Record{
		{FileName: "Soulsmith.m4b", Title: "Soulsmith", Author: "Will Wight", Series: "Cradle", Sequence: 2, HasSequence: true},
		{FileName: "Traveler.m4b", Title: "The Traveler's Gate", Author: "Will Wight", Series: "Traveler's Gate", Sequence: 1, HasSequence: true},
		{FileName: "Lost.m4b", Title: "Lost", Author: "Will Wight"},
	}
	p := NewPlanner(books)
	for i, want := range []struct {
		kind     placement.Kind
		siblings []string
	}{
		{placement.SeriesExisting, []string{"Will Wight/Cradle/C01 - Unsouled", "Will Wight/Cradle/C03 - Blackflame"}},
		{placement.SeriesNew, nil},
		{placement.Standalone, nil},
	} {
		item, err := p.Plan(recs[i], placement.ModeAuto, "")
		if err != nil || item.Kind != want.kind || item.Siblings != len(want.siblings) {
			t.Errorf("%s: %+v, %v; want %v with %d siblings", recs[i].Title, item, err, want.kind, len(want.siblings))
		}
		var viaIndex []string
		for _, b := range p.siblings(recs[i].Author, recs[i].Series) {
			viaIndex = append(viaIndex, b.Path)
		}
		if strings.Join(viaIndex, "|") != strings.Join(want.siblings, "|") {
			t.Errorf("%s: siblings %v, want %v", recs[i].Title, viaIndex, want.siblings)
		}
		items, err := planAll(recs[i:i+1], planOptions{Mode: placement.ModeAuto, Books: books})
		if err != nil || items[0] != item {
			t.Errorf("%s: Plan = %+v, %v; the planner %+v", recs[i].Title, items, err, item)
		}
	}
	item, err := p.Plan(recs[0], placement.ModeTemplate, "{Author}/{Nope}")
	if err == nil || item.RelPath != "" || item.Kind != placement.SeriesExisting || item.Siblings != 2 {
		t.Errorf("a bad template = %+v, %v", item, err)
	}
}

// bridge#43: titles of a series the library has none of, planned together
// (Expect), join the first by position, whatever order they come in; the
// folders are the ones each would get alone. Denied: a series the library has,
// a title outside the batch, one without an ASIN and a lone title are each
// planned as before.
func TestPlannerExpectPlansANewSeriesTogether(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "Will Wight/Cradle/C01 - Unsouled", Title: "Unsouled", Authors: []string{"Will Wight"}, Series: []link.SeriesRef{{Name: "Cradle", Position: "1"}}},
	}
	dawn := func(asin, title string, seq float64) source.Record {
		return source.Record{ASIN: asin, FileName: title + ".m4b", Title: title, Author: "New Writer", Series: "Dawn Road", Sequence: seq, HasSequence: true}
	}
	recs := []source.Record{
		dawn("B3", "Third Dawn", 3),
		dawn("B1", "First Dawn", 1),
		dawn("B2", "Second Dawn", 2),
		{ASIN: "BC", FileName: "Soulsmith.m4b", Title: "Soulsmith", Author: "Will Wight", Series: "Cradle", Sequence: 2, HasSequence: true},
		{ASIN: "BL", FileName: "Lone.m4b", Title: "Lone", Author: "Other Writer", Series: "Lone Road", Sequence: 1, HasSequence: true},
		{FileName: "Fourth Dawn.m4b", Title: "Fourth Dawn", Author: "New Writer", Series: "Dawn Road", Sequence: 4, HasSequence: true},
	}
	alone, err := planAll(recs, planOptions{Mode: placement.ModeAuto, Books: books})
	if err != nil {
		t.Fatal(err)
	}
	p := NewPlanner(books)
	p.Expect(recs)
	for i, want := range []struct {
		kind     placement.Kind
		siblings int
	}{
		{placement.SeriesExisting, 2}, {placement.SeriesNew, 0}, {placement.SeriesExisting, 2},
		{placement.SeriesExisting, 1}, {placement.SeriesNew, 0}, {placement.SeriesNew, 0},
	} {
		item, err := p.Plan(recs[i], placement.ModeAuto, "")
		if err != nil || item.Kind != want.kind || item.Siblings != want.siblings {
			t.Errorf("%s: %+v, %v; want %v with %d siblings", recs[i].Title, item, err, want.kind, want.siblings)
		}
		if item.RelPath != alone[i].RelPath {
			t.Errorf("%s: planned together at %q, alone at %q", recs[i].Title, item.RelPath, alone[i].RelPath)
		}
	}
	// A title the batch didn't name is planned alone.
	if item, _ := p.Plan(dawn("B9", "Ninth Dawn", 9), placement.ModeAuto, ""); item.Kind != placement.SeriesNew {
		t.Errorf("outside the batch: %+v", item)
	}
}

// A batch groups by the sibling rule (sameAuthor): a title credited "New
// Writer, Co Author" joins "New Writer"'s new series, as it would once the
// first is backed up; another author's series of the same name stays apart.
func TestPlannerExpectGroupsByTheSiblingAuthorRule(t *testing.T) {
	rec := func(asin, author string, seq float64) source.Record {
		return source.Record{ASIN: asin, FileName: asin + ".m4b", Title: "Dawn " + asin, Author: author, Series: "Dawn Road", Sequence: seq, HasSequence: true}
	}
	recs := []source.Record{
		rec("B1", "New Writer", 1),
		rec("B2", "New Writer, Co Author", 2),
		rec("B3", "Someone Else", 3),
	}
	p := NewPlanner(nil)
	p.Expect(recs)
	for i, want := range []struct {
		kind     placement.Kind
		siblings int
	}{{placement.SeriesNew, 0}, {placement.SeriesExisting, 1}, {placement.SeriesNew, 0}} {
		item, err := p.Plan(recs[i], placement.ModeAuto, "")
		if err != nil || item.Kind != want.kind || item.Siblings != want.siblings {
			t.Errorf("%s: %+v, %v; want %v with %d siblings", recs[i].ASIN, item, err, want.kind, want.siblings)
		}
	}
}

// A batch's titles anchor on the ones before them by position, each where it
// is planned beside those: what backing them up in that order leaves. So a
// later title's own credit or a three-digit position never moves an earlier
// one, a template that names the folder alone anchors on that folder (not its
// parent, whose name may read as a code), and a loose file at the library's
// root is a book to join.
func TestPlannerExpectAnchorsOnTheTitlesBefore(t *testing.T) {
	rec := func(asin, title, author, series string, seq float64) source.Record {
		return source.Record{ASIN: asin, FileName: title + ".m4b", Title: title, Author: author, Series: series, Sequence: seq, HasSequence: true}
	}
	for _, c := range []struct {
		name, template string
		recs           []source.Record
		want           []string
		siblings       []int
	}{
		{"a co-credited title between two", "", []source.Record{
			rec("B1", "One", "New Writer", "Dawn Road", 1),
			rec("B2", "Two", "New Writer & Co Author", "Dawn Road", 2),
			rec("B3", "Three", "New Writer", "Dawn Road", 3),
		}, []string{
			"New Writer/Dawn Road/DR01 - One/One.m4b",
			"New Writer/Dawn Road/DR02 - Two/Two.m4b",
			"New Writer/Dawn Road/DR03 - Three/Three.m4b",
		}, []int{0, 2, 2}},
		{"a three-digit position after", "", []source.Record{
			rec("B100", "Hundred", "New Writer", "Dawn Road", 100),
			rec("B1", "One", "New Writer", "Dawn Road", 1),
			rec("B2", "Two", "New Writer", "Dawn Road", 2),
		}, []string{
			"New Writer/Dawn Road/DR100 - Hundred/Hundred.m4b",
			"New Writer/Dawn Road/DR01 - One/One.m4b",
			"New Writer/Dawn Road/DR02 - Two/Two.m4b",
		}, []int{2, 0, 2}},
		{"a template naming the folder alone", "{Author}/{Series}/{SHORTCODE}{PaddedSeq} - {Title}", []source.Record{
			rec("B1", "One", "New Writer", "Area 51", 1),
			rec("B2", "Two", "New Writer", "Area 51", 2),
		}, []string{"New Writer/Area 51/A01 - One", "New Writer/Area 51/A02 - Two"}, []int{0, 1}},
		{"a template naming the file", "{Author}/{Series}/{SHORTCODE}{PaddedSeq} - {Title}/{OriginalFilename}", []source.Record{
			rec("B1", "One", "New Writer", "Area 51", 1),
			rec("B2", "Two", "New Writer", "Area 51", 2),
		}, []string{"New Writer/Area 51/A01 - One/One.m4b", "New Writer/Area 51/A02 - Two/Two.m4b"}, []int{0, 1}},
		{"loose files at the root", "{Title}.m4b", []source.Record{
			rec("B1", "One", "New Writer", "Dawn Road", 1),
			rec("B2", "Two", "New Writer", "Dawn Road", 2),
		}, []string{"One.m4b", "Two.m4b"}, []int{0, 1}},
	} {
		mode := placement.ModeAuto
		if c.template != "" {
			mode = placement.ModeTemplate
		}
		p := NewPlanner(nil)
		p.Expect(c.recs)
		for i, r := range c.recs {
			item, err := p.Plan(r, mode, c.template)
			if err != nil || item.RelPath != c.want[i] || item.Siblings != c.siblings[i] {
				t.Errorf("%s, %s: %q with %d siblings, %v; want %q with %d", c.name, r.ASIN, item.RelPath, item.Siblings, err, c.want[i], c.siblings[i])
			}
			if wantKind := placement.SeriesExisting; c.siblings[i] > 0 && item.Kind != wantKind {
				t.Errorf("%s, %s: kind %v, want %v", c.name, r.ASIN, item.Kind, wantKind)
			}
		}
	}
}
