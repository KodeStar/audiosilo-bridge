package importjob

import (
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-server/pkg/match"
)

func TestAuthorFolders(t *testing.T) {
	m := AuthorFolders([]link.LibraryBook{
		{Path: "L.A. McBride/Series/B01 - One", Authors: []string{"L.A. McBride"}},
		{Path: "L.A. McBride/Series/B02 - Two", Authors: []string{"L.A. McBride"}},
		{Path: "Will Wight/Cradle/C01 - Unsouled", Authors: []string{"Will Wight"}},
		{Path: "loose-single-file.m4b", Authors: []string{"Nobody"}}, // no author folder
	})
	// A differently-spaced author resolves to the existing folder spelling.
	if got := m[match.Normalize("L. A. McBride")]; got != "L.A. McBride" {
		t.Errorf("author folder (spacing) = %q, want %q", got, "L.A. McBride")
	}
	if got := m[match.Normalize("Will Wight")]; got != "Will Wight" {
		t.Errorf("author folder = %q, want %q", got, "Will Wight")
	}
	// An unknown author and a root-level loose file (no folder) have no entry.
	if got := m[match.Normalize("Jane New")]; got != "" {
		t.Errorf("unknown author = %q, want empty", got)
	}
	if got := m[match.Normalize("Nobody")]; got != "" {
		t.Errorf("loose-file author = %q, want empty", got)
	}
}

func TestAuthorFoldersPicksMostCommon(t *testing.T) {
	// Two spellings already on the server; the dominant one wins (deterministically).
	m := AuthorFolders([]link.LibraryBook{
		{Path: "L. A. McBride/A/x", Authors: []string{"L. A. McBride"}},
		{Path: "L.A. McBride/B/y", Authors: []string{"L.A. McBride"}},
		{Path: "L.A. McBride/C/z", Authors: []string{"L.A. McBride"}},
	})
	if got := m[match.Normalize("L.A. McBride")]; got != "L.A. McBride" {
		t.Errorf("most-common folder = %q, want %q", got, "L.A. McBride")
	}
}

func TestSiblingsTolerantSeries(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "Will Wight/Cradle/C01 - Unsouled", Authors: []string{"Will Wight"}, Series: []link.SeriesRef{{Name: "Cradle", Position: "1"}}},
		{Path: "Will Wight/Cradle/C02 - Soulsmith", Authors: []string{"Will Wight"}, Series: []link.SeriesRef{{Name: "The Cradle", Position: "2"}}},
		{Path: "Will Wight/Elder Empire/EE01", Authors: []string{"Will Wight"}, Series: []link.SeriesRef{{Name: "Elder Empire", Position: "1"}}},
	}
	got := NewPlanner(books).siblings("Will Wight", "Cradle")
	if len(got) != 2 {
		t.Fatalf("want 2 Cradle siblings (tolerant of 'The Cradle'), got %d: %+v", len(got), got)
	}
	if s := NewPlanner(books).siblings("Will Wight", ""); s != nil {
		t.Fatalf("standalone: got %+v", s)
	}
	if pos, ok := SeriesPosition(got[1], "Cradle"); !ok || pos != 2 {
		t.Fatalf("SeriesPosition = %v, %v", pos, ok)
	}
	if _, ok := SeriesPosition(got[1], "Elder Empire"); ok {
		t.Fatal("a book outside the series has no position in it")
	}
}

func TestSiblingsMultiAuthor(t *testing.T) {
	books := []link.LibraryBook{
		{Path: "Shirtaloon/HWFWM/FWM01", Authors: []string{"Shirtaloon, Travis Deverell"}, Series: []link.SeriesRef{{Name: "He Who Fights WIth Monsters"}}},
		{Path: "Shirtaloon/HWFWM/FWM02", Authors: []string{"Shirtaloon & Travis Deverell"}, Series: []link.SeriesRef{{Name: "He Who Fights With Monsters"}}},
		{Path: "Someone/HWFWM/X01", Authors: []string{"Someone Else"}, Series: []link.SeriesRef{{Name: "He Who Fights With Monsters"}}},
		{Path: "Shirtaloon/Other/O01", Authors: []string{"Shirtaloon"}, Series: []link.SeriesRef{{Name: "Other Series"}}},
	}
	// Allowed: the Audible record's single author is one of the credited authors.
	got := NewPlanner(books).siblings("Shirtaloon", "He Who Fights with Monsters")
	if len(got) != 2 || got[0].Path != "Shirtaloon/HWFWM/FWM01" || got[1].Path != "Shirtaloon/HWFWM/FWM02" {
		t.Fatalf("multi-author siblings = %+v", got)
	}
	if len(NewPlanner(books).siblings("Travis Deverell", "He Who Fights With Monsters")) != 2 {
		t.Fatal("second credited author should match too")
	}
	// Denied: a different author in the same series, and a partial-name overlap.
	if s := NewPlanner(books).siblings("Jane Doe", "He Who Fights With Monsters"); len(s) != 0 {
		t.Fatalf("unrelated author matched: %+v", s)
	}
	if s := NewPlanner(books).siblings("Travis", "He Who Fights With Monsters"); len(s) != 0 {
		t.Fatalf("partial name matched: %+v", s)
	}
}

func TestSameAuthor(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"L. A. McBride", "L.A. McBride", true},
		{"Shirtaloon, Travis Deverell", "Shirtaloon", true},
		{"Ilona Andrews and Gordon Andrews", "Gordon Andrews", true},
		{"Ilona Andrews; Gordon Andrews", "Ilona Andrews", true},
		{"Brandon Sanderson", "Brandon Anderson", false},
		{"Dan Anderson", "Dan", false},
		{"", "", false},
	} {
		if got := sameAuthor(tc.a, tc.b); got != tc.want {
			t.Errorf("sameAuthor(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
