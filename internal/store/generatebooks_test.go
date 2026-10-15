package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGenerateBooksMapAJobToItsBook(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, err := db.CreateBook(ctx, NewBook{SourcePath: "/lib/A/Book", WorkDir: "/w/book", Title: "Book", ContribMode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetBookBySourcePath(ctx, "/lib/A/Book"); err != nil || got.ID != b.ID || got.ContribMode != "local" {
		t.Fatalf("by source path = %+v, %v", got, err)
	}
	if _, err := db.GetBookBySourcePath(ctx, "/lib/other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown source path = %v, want ErrNotFound", err)
	}
	if _, err := db.GetGenerateBook(ctx, 12); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no mapping = %v, want ErrNotFound", err)
	}
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 12, BookID: &b.ID, ServerID: "srv", LibraryID: 1, RelPath: "A/Book"}); err != nil {
		t.Fatal(err)
	}
	g, err := db.GetGenerateBook(ctx, 12)
	if err != nil || g.BookID == nil || *g.BookID != b.ID || g.ServerID != "srv" || g.LibraryID != 1 || g.RelPath != "A/Book" || g.EndedAt != "" {
		t.Fatalf("mapping = %+v, %v", g, err)
	}

	// Ended jobs are the retention sweep's work list, from the moment they ended.
	end := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := db.EndGenerateBook(ctx, 12, end); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.EndedGenerateBooks(ctx, end.Add(-time.Second)); len(got) != 0 {
		t.Fatalf("a job that ended after the cutoff was listed: %+v", got)
	}
	if got, _ := db.EndedGenerateBooks(ctx, end); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("ended books = %+v", got)
	}
	// Mapping the job again (a resume that recreated its book) opens it again.
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 12, BookID: &b.ID, ServerID: "srv", LibraryID: 1, RelPath: "A/Book"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.EndedGenerateBooks(ctx, end.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("a reopened job is still listed as ended: %+v", got)
	}

	// Deleting the engine book (a regenerate) keeps the mapping with no book.
	if err := db.DeleteBook(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if g, err := db.GetGenerateBook(ctx, 12); err != nil || g.BookID != nil {
		t.Fatalf("after the book's delete = %+v, %v", g, err)
	}
}

func TestOpenGenerateJobsForBook(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/s", WorkDir: "/w", Title: "S"})
	for _, id := range []int64{1, 2, 3} {
		if err := db.RecordClaim(ctx, ServerJob{ID: id, Kind: "sidecars.generate", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: id, BookID: &b.ID, LibraryID: 1, RelPath: "S"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.FinishServerJob(ctx, 3, 1, JobDone, nil, ""); err != nil {
		t.Fatal(err)
	}
	got, err := db.OpenGenerateJobsForBook(ctx, b.ID, 1)
	if err != nil || len(got) != 1 || got[0] != 2 {
		t.Fatalf("open jobs other than 1 = %v, %v (want [2]: 3 finished)", got, err)
	}
}

func TestCostSums(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	old := nowFn
	t.Cleanup(func() { nowFn = old })
	at := func(ts time.Time) { nowFn = func() time.Time { return ts } }

	spend := func(book int64, stage string, cost float64) {
		t.Helper()
		id, err := db.StartStageRun(ctx, book, stage, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddOpenStageRunUsage(ctx, book, stage, "opus", 10, 10, cost); err != nil {
			t.Fatal(err)
		}
		if err := db.FinishStageRun(ctx, id, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := db.CreateBook(ctx, NewBook{SourcePath: "/a", WorkDir: "/wa", Title: "A"})
	e, _ := db.CreateBook(ctx, NewBook{SourcePath: "/e", WorkDir: "/we", Title: "E", Kind: "ebook", EbookPath: "/e/e.epub"})
	at(time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC))
	spend(a.ID, "fact_pass", 4)
	at(time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	spend(a.ID, "synthesizing", 6)
	spend(e.ID, "fact_pass", 2)
	// A superseded run still counts: spend is spend.
	if err := db.SupersedeStageSuccesses(ctx, e.ID, "fact_pass"); err != nil {
		t.Fatal(err)
	}

	month, err := db.SumBudgetCostSince(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || month != 8 {
		t.Fatalf("October spend = %v, %v; want 8 (the September run is out)", month, err)
	}

	if err := db.SetBookState(ctx, a.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookDuration(ctx, a.ID, 7200); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookChapters(ctx, a.ID, 10); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, e.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := db.FinishedBooks(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("finished = %+v, %v", got, err)
	}
	if got[0].Kind != "audio" || got[0].CostUSD != 10 || got[0].DurationSec != 7200 || got[0].Chapters != 10 {
		t.Fatalf("the audio book = %+v", got[0])
	}
	if got[1].Kind != "ebook" || got[1].CostUSD != 2 {
		t.Fatalf("the ebook = %+v", got[1])
	}
}
