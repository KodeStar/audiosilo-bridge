package audiblejob

import (
	"context"
	"errors"
	"net/http"
	"path"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/source"
)

// pack is a box set of the Orchard Cycle's first three books: a range
// position and no single sequence, as Audible lists one.
func pack() source.Record {
	r := rec("B0FAKE0030", "The Orchard Cycle: Books 1-3", "Mara Quill", "Orchard Cycle", 0)
	r.SeriesPosition = "1-3"
	return r
}

// A backup is a book of its own: a destination inside a book the library
// already has (a folder the server lists as a book, its own or another, or a
// folder already holding audio) fails exists before the license, saying what
// is in the way (conflict, which the console reads), and nothing is written.
func TestImportRefusesJoiningABook(t *testing.T) {
	copper := link.LibraryBook{Path: "Mara Quill/Orchard Cycle/OC02 - The Copper Grove", Title: "The Copper Grove",
		Authors: []string{"Mara Quill"}, Series: []link.SeriesRef{{Name: "Orchard Cycle", Position: "2"}}}
	grove := rec("B0FAKE0002", "The Copper Grove", "Mara Quill", "Orchard Cycle", 2)
	for _, tc := range []struct {
		name     string
		title    source.Record
		books    []link.LibraryBook
		dest     string
		audio    string // a file already in the library (a book not indexed yet)
		conflict string
	}{
		{name: "a pack joins a single volume", title: pack(), dest: "Mara Quill/Orchard Cycle/OC01 - The Glass Orchard", conflict: link.ConflictBook},
		{name: "a title joins another book", title: grove, dest: "Mara Quill/Orchard Cycle/OC03 - The Iron Thicket", conflict: link.ConflictBook},
		{name: "a title joins its own book", title: grove, books: append(goldenBooks(), copper), dest: copper.Path, conflict: link.ConflictBook},
		{name: "Auto lands in a book's folder", title: grove, books: append(goldenBooks(), copper), conflict: link.ConflictBook},
		{name: "a folder holding audio", title: grove, dest: "Loose/Grove", audio: "Loose/Grove/part1.mp3", conflict: link.ConflictAudio},
		{name: "a file at the destination", title: grove, dest: "Loose/Grove", audio: "Loose/Grove/The Copper Grove.m4b",
			conflict: link.ConflictFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			e.accts.title = tc.title
			if tc.books != nil {
				e.srv.books = tc.books
			}
			if tc.audio != "" {
				e.writeLibrary(tc.audio, "an owned book")
			}
			before := e.libraryFiles()
			_, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.ASIN = tc.title.ASIN; p.DestPath = tc.dest }))
			var f *Failure
			if !errors.As(err, &f) || f.Code != CodeExists || f.Conflict != tc.conflict {
				t.Fatalf("err = %v (%+v), want exists with conflict %q", err, f, tc.conflict)
			}
			if e.accts.licenses != 0 || e.accts.downloads != 0 || !slices.Equal(e.libraryFiles(), before) || len(e.srv.enriched) != 0 {
				t.Fatalf("licenses %d, downloads %d, files %v, enriched %v: nothing may happen",
					e.accts.licenses, e.accts.downloads, e.libraryFiles(), e.srv.enriched)
			}
		})
	}
}

// The book a placed file lands in takes the title's ASIN only when it is the
// title: a new book (a pack placed as a pack included). A file a dest_path puts
// in a disc folder of another book (the one join vacant can't see: the folder
// is new) leaves that book's ASIN alone. The book the rescan answers decides: a
// book in a folder above the new one (a loose file in the series folder) is
// another book, and the new one is the title's.
func TestImportEnrichesOnlyTheTitlesBook(t *testing.T) {
	extras := link.LibraryBook{Path: "Mara Quill/Orchard Cycle", Title: "Orchard Cycle Extras", Authors: []string{"Mara Quill"}}
	const thicket = "Mara Quill/Orchard Cycle/OC03 - The Iron Thicket"
	for _, tc := range []struct {
		name   string
		title  source.Record
		books  []link.LibraryBook
		dest   string
		bookOf func(p string) string // the rescan's answer (nil: the file's folder)
		enrich bool
	}{
		{name: "a new book", title: rec("B0FAKE0002", "The Copper Grove", "Mara Quill", "Orchard Cycle", 2), enrich: true},
		{name: "a pack placed as a pack", title: pack(), enrich: true},
		{name: "a title joins a disc folder of another book", title: rec("B0FAKE0002", "The Copper Grove", "Mara Quill", "Orchard Cycle", 2),
			dest: thicket + "/Disc 2", bookOf: func(string) string { return thicket }},
		{name: "a new book below a loose file's folder", title: rec("B0FAKE0002", "The Copper Grove", "Mara Quill", "Orchard Cycle", 2),
			books: append(goldenBooks(), extras), enrich: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newImportEnv(t)
			e.accts.title = tc.title
			if tc.books != nil {
				e.srv.books = tc.books
			}
			e.srv.bookOf = tc.bookOf
			res, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.ASIN = tc.title.ASIN; p.DestPath = tc.dest }))
			if err != nil {
				t.Fatal(err)
			}
			got := res.(*ImportResult)
			if got.Enriched != tc.enrich {
				t.Fatalf("enriched = %v, want %v (result %s)", got.Enriched, tc.enrich, mustJSON(t, got))
			}
			if tc.enrich {
				if want := got.Path + "=" + tc.title.ASIN; !slices.Equal(e.srv.enriched, []string{want}) {
					t.Fatalf("enriched %v, want %s", e.srv.enriched, want)
				}
				return
			}
			if len(e.srv.enriched) != 0 || slices.Contains(e.steps, StepEnrich) {
				t.Fatalf("enriched %v, steps %v: the joined book took the title's ASIN", e.srv.enriched, e.steps)
			}
			if len(e.libraryFiles()) != 1 || len(e.srv.rescanned) != 1 {
				t.Fatalf("files %v, rescanned %v: the book must still be placed and indexed", e.libraryFiles(), e.srv.rescanned)
			}
		})
	}
}

// A disc beside an indexed sibling disc: the parent the rescan may join them
// at takes no ASIN, the disc's own book does, and the sibling needs no
// listing of the library to say so.
func TestImportBesideIndexedDisc(t *testing.T) {
	const parent = "Other Tapes"
	const old = parent + "/CD1"
	const dest = parent + "/CD2"
	for _, answer := range []string{parent, dest} {
		t.Run(path.Base(answer), func(t *testing.T) {
			e := newImportEnv(t)
			e.writeLibrary(old+"/tape.mp3", "other recording")
			e.srv.books = []link.LibraryBook{{Path: old, Title: "Tape"}}
			e.srv.bookOf = func(string) string { return answer }
			out, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.DestPath = dest }))
			if err != nil {
				t.Fatal(err)
			}
			got := out.(*ImportResult)
			wantEnrich := answer == dest
			if got.Path != answer || got.Enriched != wantEnrich || (len(e.srv.enriched) > 0) != wantEnrich ||
				!slices.Equal(e.srv.rescanned, []string{dest + "/The Copper Grove.m4b"}) || len(e.libraryFiles()) != 2 ||
				e.srv.lists != 0 {
				t.Fatalf("placed %+v, rescanned %v, enriched %v, library listings %d", got, e.srv.rescanned, e.srv.enriched, e.srv.lists)
			}
		})
	}
}

func TestImportRejectedTitleDoesNotEnrichItsOldMatch(t *testing.T) {
	e := newImportEnv(t)
	const book = "Other Tapes"
	e.srv.books = []link.LibraryBook{{Path: book, Title: e.accts.title.Title, Authors: []string{e.accts.title.Author}}}
	e.srv.rejected = map[string]bool{e.accts.title.ASIN: true}
	e.srv.bookOf = func(string) string { return book }
	res, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.DestPath = book + "/CD2" }))
	if err != nil || res.(*ImportResult).Enriched || len(e.srv.enriched) != 0 {
		t.Fatalf("result %v, err %v, enriched %v", res, err, e.srv.enriched)
	}
}

// A lone new disc folder is the title's own book: nothing indexed beside it,
// so the library isn't listed, and the rescan answering the disc takes the ASIN.
func TestImportLoneDiscFolder(t *testing.T) {
	e := newImportEnv(t)
	const dest = "New Tapes/CD1"
	out, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.DestPath = dest }))
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(*ImportResult); got.Path != dest || !got.Enriched || e.srv.lists != 0 {
		t.Fatalf("placed %+v, library listings %d", got, e.srv.lists)
	}
}

// The account gone on the server when a joined book's verdict is read again
// is the import's unknown_account, the book placed but given no ASIN.
func TestImportJoinedBookUnknownAccount(t *testing.T) {
	e := newImportEnv(t)
	const book = "Other Tapes"
	e.srv.books = []link.LibraryBook{{Path: book, Title: e.accts.title.Title, Authors: []string{e.accts.title.Author}}}
	e.srv.rejectedErr = link.ErrUnknownAccount
	e.srv.bookOf = func(string) string { return book }
	_, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.DestPath = book + "/CD2" }))
	var f *Failure
	if !errors.As(err, &f) || f.Code != CodeUnknownAccount || len(e.srv.enriched) != 0 {
		t.Fatalf("err %v, enriched %v", err, e.srv.enriched)
	}
}

func TestImportRetryWithOldDiscMarker(t *testing.T) {
	e := newImportEnv(t)
	const parent = "Other Tapes"
	const dest = parent + "/Disk_2"
	const file = dest + "/The Copper Grove.m4b"
	e.writeLibrary(parent+"/Disc-1/tape.mp3", "old recording")
	e.writeLibrary(file, "placed earlier")
	marker := NewImport(e.opts).markerPath(e.accts.acct.ID, e.accts.title.ASIN)
	if err := writePlaced(marker, placedRequest{DestPath: dest}, e.libs.lib.ID, e.root, file, nil); err != nil {
		t.Fatal(err)
	}
	e.srv.bookOf = func(string) string { return parent }
	out, err := e.run(context.Background(), e.params(func(p *ImportParams) { p.DestPath = dest }))
	if err != nil || out.(*ImportResult).Enriched || len(e.srv.enriched) != 0 || e.accts.downloads != 0 {
		t.Fatalf("result %v, err %v, enriched %v, downloads %d", out, err, e.srv.enriched, e.accts.downloads)
	}
}

// A retry after the install keeps the first run's verdict on the ASIN: the
// marker carries it, so the joined book isn't given it on the second try.
func TestImportRetryKeepsNoEnrich(t *testing.T) {
	e := newImportEnv(t)
	e.accts.title = pack()
	e.srv.rescanErr = []error{errors.New("busy"), errors.New("busy"), errors.New("busy")}
	const thicket = "Mara Quill/Orchard Cycle/OC03 - The Iron Thicket"
	e.srv.bookOf = func(string) string { return thicket }
	params := e.params(func(p *ImportParams) {
		p.ASIN = "B0FAKE0030"
		p.DestPath = thicket + "/Disc 2"
	})
	if _, err := e.run(context.Background(), params); err == nil {
		t.Fatal("the rescan failing must fail the job")
	}
	res, err := e.run(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if res.(*ImportResult).Enriched || len(e.srv.enriched) != 0 || e.accts.downloads != 1 {
		t.Fatalf("result %s, enriched %v, downloads %d", mustJSON(t, res), e.srv.enriched, e.accts.downloads)
	}
}

func tooMany(after time.Duration) error {
	return &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: after, RetryAfterSet: after > 0}
}

// The import's one enrichment PUT waits out the server's 429s as the poll's
// does: Retry-After when sent, else the doubling backoff; after
// enrichMaxLimited in a row the book is placed without the ASIN, no failure.
func TestImportEnrichPacedOn429(t *testing.T) {
	t.Run("explicit zero", func(t *testing.T) {
		e := newImportEnv(t)
		e.srv.enrichErr = &link.APIError{Status: http.StatusTooManyRequests, RetryAfterSet: true}
		res, err := e.run(context.Background(), e.params(nil))
		if err != nil || res.(*ImportResult).Enriched || len(e.srv.enriched) != enrichMaxLimited ||
			!slices.Equal(e.waits, make([]time.Duration, enrichMaxLimited-1)) {
			t.Fatalf("res %v, err %v, PUTs %v, waits %v", res, err, e.srv.enriched, e.waits)
		}
	})
	t.Run("Retry-After then accepted", func(t *testing.T) {
		e := newImportEnv(t)
		e.srv.enrichErrs = []error{tooMany(3 * time.Second), tooMany(0)}
		res, err := e.run(context.Background(), e.params(nil))
		if err != nil {
			t.Fatal(err)
		}
		if !res.(*ImportResult).Enriched || len(e.srv.enriched) != 3 {
			t.Fatalf("result %s, enriched %v", mustJSON(t, res), e.srv.enriched)
		}
		if len(e.waits) != 2 || e.waits[0] != 3*time.Second || e.waits[1] < enrichBackoff || e.waits[1] > 2*enrichBackoff {
			t.Fatalf("waits %v, want Retry-After then a jittered second backoff", e.waits)
		}
	})
	t.Run("always 429", func(t *testing.T) {
		e := newImportEnv(t)
		e.srv.enrichErr = tooMany(0)
		res, err := e.run(context.Background(), e.params(nil))
		if err != nil {
			t.Fatal(err)
		}
		if res.(*ImportResult).Enriched || len(e.srv.enriched) != enrichMaxLimited {
			t.Fatalf("result %s, %d PUTs, want %d", mustJSON(t, res), len(e.srv.enriched), enrichMaxLimited)
		}
		want := []time.Duration{enrichBackoff, 2 * enrichBackoff, 4 * enrichBackoff, 8 * enrichBackoff}
		if len(e.waits) != len(want) {
			t.Fatalf("waits %v, want %d", e.waits, len(want))
		}
		for i, d := range e.waits {
			if d < want[i]/2 || d > want[i] {
				t.Fatalf("wait %d = %v, want %v..%v", i, d, want[i]/2, want[i])
			}
		}
		if res.(*ImportResult).Path != path.Dir(copperGrove) {
			t.Fatalf("path = %s", res.(*ImportResult).Path)
		}
	})
	t.Run("a Retry-After past the cap", func(t *testing.T) {
		e := newImportEnv(t)
		e.srv.enrichErr = tooMany(enrichMaxWait + time.Second)
		res, err := e.run(context.Background(), e.params(nil))
		if err != nil || res.(*ImportResult).Enriched || len(e.srv.enriched) != 1 || len(e.waits) != 0 {
			t.Fatalf("res %v, err %v, PUTs %v, waits %v", res, err, e.srv.enriched, e.waits)
		}
	})
}

// joinedBooks applies the poll's twin rule (bridge#64): a book the library
// holds twice may not take the title's ASIN when the file joins it, since the
// title's words can't tell the copies apart; the only copy may.
func TestJoinedBooksTwins(t *testing.T) {
	grove := rec("B0FAKE0002", "The Copper Grove", "Mara Quill", "Orchard Cycle", 2)
	copper := link.LibraryBook{Path: "Mara Quill/Orchard Cycle/OC02 - The Copper Grove", Title: "The Copper Grove",
		Authors: []string{"Mara Quill"}, Series: []link.SeriesRef{{Name: "Orchard Cycle", Position: "2"}}}
	copy2 := copper
	copy2.Path = "Mara Quill/Copper Grove (another copy)"
	copy2.Series = []link.SeriesRef{{Name: "Orchard Cycle", Position: "2.0"}}
	rel := path.Join(copper.Path, "Disc 2", "The Copper Grove.m4b") // a disc folder of the book

	if got := joinedBooks(grove, []link.LibraryBook{copper}, rel); !got[copper.Path] {
		t.Fatalf("the only copy = %v, want it to take the ASIN", got)
	}
	got := joinedBooks(grove, []link.LibraryBook{copper, copy2}, rel)
	if v, ok := got[copper.Path]; !ok || v {
		t.Fatalf("a twin = %v, want it joined without the ASIN", got)
	}
}
