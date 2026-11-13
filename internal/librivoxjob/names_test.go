package librivoxjob

import (
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
)

func TestSegment(t *testing.T) {
	long := strings.Repeat("a", 300)
	wide := strings.Repeat("é", 200) // 2 bytes each
	for _, tc := range []struct{ in, want string }{
		{"Johnny Crow's Garden", "Johnny Crow's Garden"},
		{"../../etc/passwd", "-..-etc-passwd"},
		{"..", ""},
		{".hack", "hack"},
		{"...And Ladies of the Club", "And Ladies of the Club"},
		{"   ", ""},
		{"", ""},
		{"Title: Subtitle", "Title - Subtitle"},
		{"a\\b", "a-b"},
		{"tab\there\nnewline\x00nul", "tab here newline nul"},
		{"two  spaces", "two spaces"},
		{long, strings.Repeat("a", maxName)},
	} {
		if got := segment(tc.in); got != tc.want {
			t.Errorf("segment(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	got := segment(wide)
	if len(got) > maxName || !utf8.ValidString(got) || len(got) != maxName {
		t.Errorf("a multi-byte over-long name cut to %d bytes (valid %v)", len(got), utf8.ValidString(got))
	}
	// Cut ending in a space or a dot is trimmed again.
	if got := segment(strings.Repeat("a", maxName-1) + " b"); got != strings.Repeat("a", maxName-1) {
		t.Errorf("a cut name kept its trailing space: %q", got)
	}
}

func TestBookNames(t *testing.T) {
	book := metaops.LibrivoxBook{Title: "Metamorphosis (version 2)",
		Authors: []metaops.LibrivoxPerson{{FirstName: "Franz", LastName: "Kafka"}}}
	one := &metaops.LibrivoxWork{Title: "Metamorphosis", Authors: []string{"Franz Kafka"},
		Recordings: []metaops.LibrivoxRecording{{ID: "r1", LibrivoxID: 527}, {ID: "audible", LibrivoxID: 0}}}
	two := &metaops.LibrivoxWork{Title: "Metamorphosis", Authors: []string{"Franz Kafka"},
		Recordings: []metaops.LibrivoxRecording{{ID: "r1", LibrivoxID: 527}, {ID: "r2", LibrivoxID: 5214}}}
	for _, tc := range []struct {
		name                string
		book                metaops.LibrivoxBook
		work                *metaops.LibrivoxWork
		wantAuthor, wantTit string
	}{
		{"one LibriVox recording: the work's title", book, one, "Franz Kafka", "Metamorphosis"},
		{"two: the project's own title", book, two, "Franz Kafka", "Metamorphosis (version 2)"},
		{"no work: the project's", book, nil, "Franz Kafka", "Metamorphosis (version 2)"},
		{"empty work title: the project's", book, &metaops.LibrivoxWork{Authors: []string{"Franz Kafka"}}, "Franz Kafka", "Metamorphosis (version 2)"},
		{"empty everything", metaops.LibrivoxBook{}, nil, "", "LibriVox 192"},
		{"dot titles", metaops.LibrivoxBook{Title: "..."}, &metaops.LibrivoxWork{Title: ".."}, "", "LibriVox 192"},
		{"first credit", book, &metaops.LibrivoxWork{Title: "T", Authors: []string{"A", "B", "C"}}, "A", "T"},
		{"Various over three", book, &metaops.LibrivoxWork{Title: "T", Authors: []string{"A", "B", "C", "D"}}, "Various", "T"},
		{"Various from the feed", metaops.LibrivoxBook{Title: "Short Story Collection Vol. 001", Authors: []metaops.LibrivoxPerson{
			{LastName: "A"}, {LastName: "B"}, {LastName: "C"}, {LastName: "D"}}}, nil, "Various", "Short Story Collection Vol. 001"},
		{"author traversal", book, &metaops.LibrivoxWork{Title: "T", Authors: []string{"../x"}}, "-x", "T"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, ti := bookNames(192, tc.book, tc.work)
			if a != tc.wantAuthor || ti != tc.wantTit {
				t.Fatalf("got %q / %q, want %q / %q", a, ti, tc.wantAuthor, tc.wantTit)
			}
		})
	}
}

func TestDestination(t *testing.T) {
	books := []link.LibraryBook{{Path: "L.Leslie Brooke/Other Book", Authors: []string{"L. Leslie Brooke"}}}
	for _, tc := range []struct {
		author, title, parent, dest string
	}{
		{"L. Leslie Brooke", "Johnny Crow's Garden", "L.Leslie Brooke", "L.Leslie Brooke/Johnny Crow's Garden"},
		{"Franz Kafka", "Metamorphosis (version 2)", "Franz Kafka", "Franz Kafka/Metamorphosis (version 2)"},
		{"", "Anonymous Tales", "", "Anonymous Tales"},
		{"Various", "Short Story Collection Vol. 001", "Various", "Various/Short Story Collection Vol. 001"},
	} {
		parent, dest := destination(tc.author, tc.title, books)
		if parent != tc.parent || dest != tc.dest {
			t.Errorf("destination(%q, %q) = %q, %q; want %q, %q", tc.author, tc.title, parent, dest, tc.parent, tc.dest)
		}
	}
}

func TestSectionNames(t *testing.T) {
	sections := []metaops.LibrivoxSection{
		{SectionNumber: "0", Title: "Introduction", ListenURL: "https://archive.org/download/x/intro_64kb.mp3"},
		{SectionNumber: "1", Title: "", ListenURL: "https://archive.org/download/x/one.ogg"},
		{SectionNumber: "2", Title: "../../escape", ListenURL: "https://archive.org/download/x/two"},
		{SectionNumber: "3", Title: ".hidden", ListenURL: "https://archive.org/download/x/three.MP3?x=1"},
		{SectionNumber: "4", Title: strings.Repeat("w", 400), ListenURL: "https://archive.org/download/x/four.exe"},
	}
	got := sectionNames(sections)
	want := []string{
		"01 - Introduction.mp3",
		"02 - Section 2.ogg",
		"03 - -..-escape.mp3",
		"04 - hidden.mp3",
		"05 - " + strings.Repeat("w", maxName) + ".mp3",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("section %d: %q, want %q", i+1, got[i], want[i])
		}
		if len(got[i]+partSuffix) > 255 {
			t.Errorf("section %d's partial name is %d bytes", i+1, len(got[i]+partSuffix))
		}
	}
	// The width is the count's, at least two digits.
	many := make([]metaops.LibrivoxSection, 120)
	names := sectionNames(many)
	if names[0] != "001 - Section 1.mp3" || names[119] != "120 - Section 120.mp3" {
		t.Errorf("120 sections: %q .. %q", names[0], names[119])
	}
	if one := sectionNames(many[:1]); one[0] != "01 - Section 1.mp3" {
		t.Errorf("one section: %q", one[0])
	}
}

func TestFallbackNameFits(t *testing.T) {
	name := fallbackName(strings.Repeat("t", maxName), 99999999)
	if len(name) > 255 {
		t.Fatalf("the fallback name is %d bytes", len(name))
	}
}

func TestAllowedURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://archive.org/download/x/1.mp3":             true,
		"https://www.archive.org/download/x/1.mp3":         true,
		"https://ia800300.us.archive.org/12/items/x/1.mp3": true,
		"https://ARCHIVE.ORG/download/x/1.mp3":             true,
		"https://archive.org./download/x/1.mp3":            true,
		"https://archive.org:443/download/x/1.mp3":         true,
		"http://archive.org/download/x/1.mp3":              false,
		"http://www.archive.org/download/x/1.mp3":          false,
		"https://librivox.org/x":                           false,
		"https://www.librivox.org/x":                       false,
		"https://archive.org.evil.example/x":               false,
		"https://evilarchive.org/x":                        false,
		"https://user:pw@archive.org/x":                    false,
		"https://archive.org:8443/x":                       false,
		"ftp://archive.org/x":                              false,
		"https://207.241.224.2/x":                          false,
		"//archive.org/x":                                  false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := AllowedURL(u); got != want {
			t.Errorf("AllowedURL(%s) = %v, want %v", raw, got, want)
		}
	}
	if AllowedURL(nil) {
		t.Error("a nil URL allowed")
	}
}
