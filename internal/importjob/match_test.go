package importjob

import (
	"testing"

	"github.com/kodestar/audiosilo-server/pkg/match"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/placement"
	"github.com/kodestar/audiosilo-bridge/internal/source"
)

func TestMatchInBooks(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "Wight/Cradle/C01 - Unsouled", Title: "Unsouled", Authors: []string{"Will Wight"}, ASIN: "B01"},
		{Path: "Sanderson/Mistborn", Title: "The Final Empire", Authors: []string{"Brandon Sanderson"}},
	}

	// ASIN wins.
	if b, ok := MatchInBooks(books, source.Record{ASIN: "B01", Title: "different"}); !ok || b.Path != "Wight/Cradle/C01 - Unsouled" {
		t.Fatalf("ASIN match failed: %+v ok=%v", b, ok)
	}
	// Fall back to author + title (tolerant) when no ASIN.
	if b, ok := MatchInBooks(books, source.Record{Author: "brandon sanderson", Title: "the final empire"}); !ok || b.Path != "Sanderson/Mistborn" {
		t.Fatalf("author/title match failed: %+v ok=%v", b, ok)
	}
	// TitleShort also considered.
	if _, ok := MatchInBooks(books, source.Record{Author: "Will Wight", TitleShort: "Unsouled"}); !ok {
		t.Fatal("titleShort match failed")
	}
	// No match.
	if _, ok := MatchInBooks(books, source.Record{ASIN: "ZZZ", Author: "Nobody", Title: "Nothing"}); ok {
		t.Fatal("unexpected match")
	}
}

// Real library titles are wildly inconsistent (verified against a 1080-book server):
// full Audible titles with series prefixes + "(Book N)", clean book titles, shortcode
// prefixes, series suffixes, and "Series Name N" with the number only. The server
// rarely has ASINs or a series_index. These cases are drawn from that real data.
func TestMatchInBooksRealWorld(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "k/dwk05", Authors: []string{"Jeff Kinney"}, Series: []link.SeriesRef{{Name: "Diary of a Wimpy Kid"}}, Title: "Diary of a Wimpy Kid: The Ugly Truth (Book 5)"},
		{Path: "k/dwk04", Authors: []string{"Jeff Kinney"}, Series: []link.SeriesRef{{Name: "Diary of a Wimpy Kid"}}, Title: "Diary Of A Wimpy Kid 4 - Dog Days"},
		{Path: "k/dwk15", Authors: []string{"Jeff Kinney"}, Series: []link.SeriesRef{{Name: "Diary of a Wimpy Kid"}}, Title: "The Deep End - Diary of a Wimpy Kid Series, Book 15"},
		{Path: "a/rrp3", Authors: []string{"Actus"}, Series: []link.SeriesRef{{Name: "Return of the Runebound Professor"}}, Title: "Return of the Runebound Professor 3"},
		{Path: "a/rrp5", Authors: []string{"Actus"}, Series: []link.SeriesRef{{Name: "Return of the Runebound Professor"}}, Title: "Return of the Runebound Professor 5: A Progression Fantasy Epic"},
		{Path: "kay/legion", Authors: []string{"A. F. Kay"}, Series: []link.SeriesRef{{Name: "Divine Apostasy"}}, Title: "Legion's Fifth Vault"},
		{Path: "oster/sand", Authors: []string{"Aaron Oster"}, Series: []link.SeriesRef{{Name: "Rise to Omniscience"}}, Title: "RO07 - Sandqueen"},
		{Path: "oster/silver", Authors: []string{"Aaron Oster"}, Series: []link.SeriesRef{{Name: "Rise to Omniscience"}}, Title: "Silverspear: Rise to Omniscience, Book 6"},
		{Path: "w/unsouled", Authors: []string{"Will Wight"}, Series: []link.SeriesRef{{Name: "Cradle", Position: "1"}}, Title: "Unsouled", ASIN: "B01"},
	}
	cases := []struct {
		name     string
		rec      source.Record
		wantPath string // "" = expect no match
	}{
		{"full title + (Book N)", recWith("Diary of a Wimpy Kid: The Ugly Truth (Book 5)", "Jeff Kinney", "Diary of a Wimpy Kid", 5), "k/dwk05"},
		{"alt scheme N - title", recWith("Diary of a Wimpy Kid: Dog Days (Book 4)", "Jeff Kinney", "Diary of a Wimpy Kid", 4), "k/dwk04"},
		{"series suffix on server", recWith("Diary of a Wimpy Kid: The Deep End (Book 15)", "Jeff Kinney", "Diary of a Wimpy Kid", 15), "k/dwk15"},
		{"series-name-N, residual is just the number", recWith("Return of the Runebound Professor 3", "Actus", "Return of the Runebound Professor", 3), "a/rrp3"},
		{"series-name-N picks the right number", recWith("Return of the Runebound Professor 5", "Actus", "Return of the Runebound Professor", 5), "a/rrp5"},
		{"clean server title vs subtitled audible", recWith("Legion's Fifth Vault: A Fantasy LitRPG Adventure", "A. F. Kay", "Divine Apostasy", 5), "kay/legion"},
		{"shortcode prefix on server", recWith("Sandqueen", "Aaron Oster", "Rise to Omniscience", 7), "oster/sand"},
		{"series suffix on server 2", recWith("Silverspear", "Aaron Oster", "Rise to Omniscience", 6), "oster/silver"},
		{"absent book number does not false-match", recWith("Return of the Runebound Professor 9", "Actus", "Return of the Runebound Professor", 9), ""},
		{"different author no match", recWith("The Ugly Truth", "Someone Else", "Whatever", 5), ""},
	}
	for _, tc := range cases {
		got, ok := MatchInBooks(books, tc.rec)
		if tc.wantPath == "" {
			if ok {
				t.Errorf("%s: expected no match, got %q", tc.name, got.Title)
			}
			continue
		}
		if !ok || got.Path != tc.wantPath {
			t.Errorf("%s: got %q (path %q, ok=%v), want path %q", tc.name, got.Title, got.Path, ok, tc.wantPath)
		}
	}
	// ASIN still wins outright.
	if got, ok := MatchInBooks(books, source.Record{ASIN: "B01", Title: "anything"}); !ok || got.Path != "w/unsouled" {
		t.Errorf("ASIN match failed: %+v ok=%v", got, ok)
	}
}

// recWith builds an Audible record with a series sequence (the common case).
func recWith(title, author, series string, seq float64) source.Record {
	return source.Record{
		Title: title, TitleShort: title, Author: author,
		Series: series, Sequence: seq, HasSequence: true,
	}
}

func TestPlannerSiblings(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "Kinney/Diary of a Wimpy Kid/DWK01", Authors: []string{"Jeff Kinney"}, Series: []link.SeriesRef{{Name: "Diary of a Wimpy Kid", Position: "1"}}},
		{Path: "Kinney/Diary of a Wimpy Kid/DWK09", Authors: []string{"Jeff Kinney"}, Series: []link.SeriesRef{{Name: "Diary of a Wimpy Kid", Position: "9"}}},
		{Path: "Other/Book", Authors: []string{"Someone Else"}, Series: []link.SeriesRef{{Name: "Other Series"}}},
	}
	got := NewPlanner(books).siblings("Jeff Kinney", "Diary of a Wimpy Kid")
	if len(got) != 2 {
		t.Fatalf("want 2 siblings, got %d: %+v", len(got), got)
	}
	if NewPlanner(books).siblings("Jeff Kinney", "") != nil {
		t.Fatal("a standalone (no series) should have no siblings")
	}
}

// Plan places from the library books it is given and makes no server calls
// (there is no client to make them with); matching is BookMatcher's.
func TestPlanUsesLocalBooksNoQueries(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "Kinney/Diary of a Wimpy Kid/DWK01 - The First", Authors: []string{"Jeff Kinney"}, Series: []link.SeriesRef{{Name: "Diary of a Wimpy Kid", Position: "1"}}, ASIN: "B1"},
	}
	recs := []source.Record{
		{ASIN: "B1", Title: "The First", Author: "Jeff Kinney", Series: "Diary of a Wimpy Kid", Sequence: 1, HasSequence: true, FileName: "a.m4b"},
		{ASIN: "B5", Title: "The Ugly Truth", Author: "Jeff Kinney", Series: "Diary of a Wimpy Kid", Sequence: 5, HasSequence: true, FileName: "b.m4b"},
	}
	items, err := planAll(recs, planOptions{Mode: placement.ModeAuto, Books: books})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	if _, ok := MatchInBooks(books, recs[0]); !ok {
		t.Error("book B1 should be matched as already on the server (local match)")
	}
	if _, ok := MatchInBooks(books, recs[1]); ok {
		t.Error("book B5 should NOT be on the server")
	}
}

// The person-gate prefilter must answer exactly what match.Best over every
// candidate answers: the same candidate, or no match.
func TestBookMatcherSameAsBest(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "k/dwk05", Authors: []string{"Jeff Kinney"}, Series: []link.SeriesRef{{Name: "Diary of a Wimpy Kid"}}, Title: "Diary of a Wimpy Kid: The Ugly Truth (Book 5)"},
		{Path: "k/dwk04", Authors: []string{"Jeff Kinney"}, Series: []link.SeriesRef{{Name: "Diary of a Wimpy Kid"}}, Title: "Diary Of A Wimpy Kid 4 - Dog Days"},
		{Path: "a/rrp3", Authors: []string{"Actus"}, Series: []link.SeriesRef{{Name: "Return of the Runebound Professor", Position: "3"}}, Title: "Return of the Runebound Professor 3"},
		{Path: "oster/sand", Authors: []string{"Aaron Oster", "Richard Sashigane"}, Series: []link.SeriesRef{{Name: "Rise to Omniscience"}, {Name: "Omniverse"}}, Title: "RO07 - Sandqueen"},
		{Path: "n/narrated", Authors: []string{"Anon"}, Narrators: []string{"Travis Baldree"}, Title: "Legends & Lattes"},
		{Path: "n/narrator-author", Authors: []string{"Travis Baldree"}, Title: "Bookshops & Bonedust"},
		{Path: "w/unsouled", Authors: []string{"Will Wight"}, Series: []link.SeriesRef{{Name: "Cradle", Position: "1"}}, Title: "Unsouled", ASIN: "B01"},
		{Path: "x/noauthor", Title: "Unsouled"},
	}
	recs := []source.Record{
		recWith("Diary of a Wimpy Kid: The Ugly Truth (Book 5)", "Jeff Kinney", "Diary of a Wimpy Kid", 5),
		recWith("Dog Days", "jeff kinney", "Diary of a Wimpy Kid", 4),
		recWith("Return of the Runebound Professor 3", "Actus", "", 0),
		recWith("Sandqueen", "Aaron Oster", "Omniverse", 7),
		{Title: "Legends & Lattes", Author: "Travis Baldree"},
		{Title: "Bookshops & Bonedust", Author: "Someone", Narrator: "Travis Baldree"},
		{Title: "Unsouled", Author: "Will Wight"},
		{Title: "Unsouled", ASIN: "b01"},
		{Title: "Unsouled"},
		{Title: "Nothing", Author: "Nobody"},
	}
	for _, rec := range recs {
		m := NewBookMatcher(books)
		q := match.Query{Title: rec.Title, TitleShort: rec.TitleShort, Author: rec.Author, Series: rec.Series,
			Sequence: rec.Sequence, HasSequence: rec.HasSequence, ASIN: rec.ASIN}
		if rec.Narrator != "" {
			q.Narrators = []string{rec.Narrator}
		}
		wantI, wantScore, wantOK := match.BestWith(m.cands, q, match.ImportOptions())
		gotI, gotScore, gotOK := m.best(q, false)
		if gotI != wantI || gotOK != wantOK || gotScore != wantScore {
			t.Errorf("%q by %q: best = %d %.2f %v, match.BestWith = %d %.2f %v", rec.Title, rec.Author, gotI, gotScore, gotOK, wantI, wantScore, wantOK)
		}
	}
}

// Repeated queries cross partitions and people without changing scores, tie
// order, or the mapping from a series candidate back to its library book.
func TestBookMatcherReusesPartitions(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "wrong-person", Title: "Iron Vale", Authors: []string{"Other Writer"}, Series: series("Iron Vale", "5")},
		{Path: "single", Title: "Iron Vale 5", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "5")},
		{Path: "pack", Title: "Iron Vale", Authors: []string{"Ona Fell"}, Series: []link.SeriesRef{{Name: "Fell Worlds", Position: "5"}, {Name: "Iron Vale", Position: "5-6"}}, ASIN: "B0PACK0001"},
		{Path: "tied-pack", Title: "Iron Vale", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "5-6")},
		{Path: "other-pack", Title: "Iron Vale", Authors: []string{"Ona Fell"}, Series: series("Iron Vale", "3-4")},
		{Path: "narrated", Title: "Quiet Harbour", Authors: []string{"Anon"}, Narrators: []string{"Ona Fell"}},
	}
	m := NewBookMatcher(books)
	cases := []struct {
		rec  source.Record
		want string
	}{
		{audibleRec("", "Iron Vale 5", "Ona Fell", "Iron Vale", "5"), "single"},
		{audibleRec("", "Iron Vale: Books 5-6", "Ona Fell", "Iron Vale", "5-6"), "pack"},
		{audibleRec("", "Iron Vale: Books 3-4", "Ona Fell", "Iron Vale", "3-4"), "other-pack"},
		{source.Record{ASIN: "b0pack0001", Title: "Unrelated", Author: "Nobody"}, "pack"},
		{source.Record{Title: "Quiet Harbour", Author: "Ona Fell"}, "narrated"},
		{source.Record{Title: "Quiet Harbour", Author: "Nobody"}, ""},
		{audibleRec("", "Iron Vale 5", "Other Writer", "Iron Vale", "5"), "wrong-person"},
	}
	for pass := range 3 {
		for _, tc := range cases {
			got, score, ok := m.MatchScored(tc.rec)
			fresh, freshScore, freshOK := NewBookMatcher(books).MatchScored(tc.rec)
			if got.Path != tc.want || ok != (tc.want != "") || fresh.Path != got.Path || freshOK != ok || freshScore != score {
				t.Errorf("pass %d, %q: got %q %.2f %v, want %q, fresh %q %.2f %v", pass, tc.rec.Title,
					got.Path, score, ok, tc.want, fresh.Path, freshScore, freshOK)
			}
		}
	}
}
