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
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 12, BookID: &b.ID, LibraryID: 1, RelPath: "A/Book"}); err != nil {
		t.Fatal(err)
	}
	g, err := db.GetGenerateBook(ctx, 12)
	if err != nil || g.BookID == nil || *g.BookID != b.ID || g.LibraryID != 1 || g.RelPath != "A/Book" || g.EndedAt != "" {
		t.Fatalf("mapping = %+v, %v", g, err)
	}
	// The server moved the job with its book: the mapping follows it.
	if err := db.SetGenerateBookPath(ctx, 12, "A/Book (2026)"); err != nil {
		t.Fatal(err)
	}
	if g, _ := db.GetGenerateBook(ctx, 12); g.RelPath != "A/Book (2026)" || g.BookID == nil || *g.BookID != b.ID {
		t.Fatalf("moved mapping = %+v", g)
	}
	if err := db.SetGenerateBookPath(ctx, 12, "A/Book"); err != nil {
		t.Fatal(err)
	}

	// Ended jobs are the retention sweep's work list, from the moment they ended.
	end := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := db.EndGenerateBook(ctx, 12, 1, end); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.UnsweptGenerateBooks(ctx, end.Add(-time.Second)); len(got) != 0 {
		t.Fatalf("a job that ended after the cutoff was listed: %+v", got)
	}
	if got, _ := db.UnsweptGenerateBooks(ctx, end); len(got) != 1 || got[0].ID != b.ID || got[0].WorkDir != "/w/book" {
		t.Fatalf("ended books = %+v", got)
	}
	// A swept book is never listed again.
	if err := db.MarkGenerateBookSwept(ctx, b.ID, end.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.UnsweptGenerateBooks(ctx, end.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("a swept book was listed: %+v", got)
	}
	if swept, err := db.GenerateBookSwept(ctx, b.ID); err != nil || !swept {
		t.Fatalf("swept = %v, %v", swept, err)
	}
	if swept, _ := db.GenerateBookSwept(ctx, b.ID+100); swept {
		t.Fatal("a book never mapped reads as swept")
	}
	// Mapping the job again (a resume that recreated its book) opens it again.
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 12, BookID: &b.ID, LibraryID: 1, RelPath: "A/Book"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.UnsweptGenerateBooks(ctx, end.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("a reopened job is still listed as ended: %+v", got)
	}
	// Once it ends again it is swept again (the mapping cleared swept_at).
	if err := db.EndGenerateBook(ctx, 12, 1, end); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.UnsweptGenerateBooks(ctx, end.Add(time.Hour)); len(got) != 1 {
		t.Fatalf("a reopened then ended job wasn't listed: %+v", got)
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
	// A deleted book's spend still counts (a regenerate deletes the earlier book):
	// month by month, as its runs started.
	gone, _ := db.CreateBook(ctx, NewBook{SourcePath: "/g", WorkDir: "/wg", Title: "G"})
	at(time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC))
	spend(gone.ID, "fact_pass", 5)
	at(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	spend(gone.ID, "synthesizing", 3)
	if err := db.DeleteBook(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteBook(ctx, gone.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting it again = %v, want ErrNotFound", err)
	}
	if month, err := db.SumBudgetCostSince(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil || month != 11 {
		t.Fatalf("October spend after a delete = %v, %v; want 11 (the deleted book's October run kept)", month, err)
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

// The newest mapping at a library path that leads to the audio run beside an
// ebook run (it holds no notes) is passed over for the ebook run's own.
func TestLatestGenerateBookAtPassesOverTheAudioRun(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	ebook, err := db.CreateBook(ctx, NewBook{SourcePath: "/lib/A/Book", WorkDir: "/w/ebook", Title: "Book", Kind: "ebook", EbookPath: "/lib/A/Book/Book.epub"})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := db.CreateBook(ctx, NewBook{SourcePath: AudioRunKey("/lib/A/Book"), WorkDir: "/w/audio", Title: "Book"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 12, BookID: &ebook.ID, LibraryID: 1, RelPath: "A/Book"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 13, BookID: &audio.ID, LibraryID: 1, RelPath: "A/Book"}); err != nil {
		t.Fatal(err)
	}
	if id, err := db.LatestGenerateBookAt(ctx, 1, "A/Book"); err != nil || id != ebook.ID {
		t.Fatalf("latest = %d, %v; want the ebook run %d", id, err, ebook.ID)
	}
	if err := db.DeleteBook(ctx, ebook.ID); err != nil {
		t.Fatal(err)
	}
	if id, err := db.LatestGenerateBookAt(ctx, 1, "A/Book"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("only the audio run left = %d, %v; want ErrNotFound", id, err)
	}
}

// A job's end stops its engine book only while it still owns it, checked in the
// write: no later attempt of the job recorded, no other open job on the book, and
// with Ended the job's mapping still ended (a later claim's attach reopens it).
func TestSetBookStatusUnlessFollowed(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/s", WorkDir: "/w", Title: "S"})
	claim := func(job int64, attempt int) {
		t.Helper()
		if err := db.RecordClaim(ctx, ServerJob{ID: job, Kind: "sidecars.generate", Attempt: attempt}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: job, BookID: &b.ID, LibraryID: 1, RelPath: "S"}); err != nil {
			t.Fatal(err)
		}
	}
	stop := func(g StopGuard) error {
		_ = db.SetBookStatus(ctx, b.ID, "", "", "")
		return db.SetBookStatusUnlessFollowed(ctx, b.ID, "", "", "failed", "cancelled by user", g)
	}
	stopped := func(what string, err error, want bool) {
		t.Helper()
		got, _ := db.GetBook(ctx, b.ID)
		if want && (err != nil || got.Status != "failed" || got.Error != "cancelled by user") {
			t.Fatalf("%s: %v, status %q error %q; want it stopped", what, err, got.Status, got.Error)
		}
		if !want && (!errors.Is(err, ErrFollowed) || got.Status != "") {
			t.Fatalf("%s: %v, status %q; want ErrFollowed and the book untouched", what, err, got.Status)
		}
	}
	claim(1, 1)
	stopped("the job's own attempt", stop(StopGuard{JobID: 1, Attempt: 1}), true)
	if err := db.EndGenerateBook(ctx, 1, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	stopped("its mapping still ended", stop(StopGuard{JobID: 1, Attempt: 1, Ended: true}), true)
	if err := db.ReopenGenerateBook(ctx, 1); err != nil {
		t.Fatal(err)
	}
	stopped("its mapping reopened", stop(StopGuard{JobID: 1, Attempt: 1, Ended: true}), false)
	claim(1, 2)
	stopped("a later attempt recorded", stop(StopGuard{JobID: 1, Attempt: 1}), false)
	claim(2, 1)
	stopped("another open job on the book", stop(StopGuard{JobID: 1, Attempt: 2}), false)
	if err := db.FinishServerJob(ctx, 2, 1, JobDone, nil, ""); err != nil {
		t.Fatal(err)
	}
	stopped("the other job finished", stop(StopGuard{JobID: 1, Attempt: 2}), true)
}

// A job's end never stops its engine book over what a stage wrote since the end
// read it: a failure keeps its own error and a finished book stays done
// (ErrSettled, told apart from ErrFollowed), while a later claim still reads as
// ErrFollowed whatever the book's status.
func TestSetBookStatusUnlessFollowedLeavesASettledBook(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/s", WorkDir: "/w", Title: "S"})
	if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: "sidecars.generate", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "S"}); err != nil {
		t.Fatal(err)
	}
	g := StopGuard{JobID: 1, Attempt: 1}
	cancel := func() error {
		return db.SetBookStatusUnlessFollowed(ctx, b.ID, "", "", "failed", "cancelled by user", g)
	}

	// The end read the book running; a stage failed it before the cancel's write.
	if err := db.SetBookStatus(ctx, b.ID, "failed", "asr: out of memory", ""); err != nil {
		t.Fatal(err)
	}
	if err := cancel(); !errors.Is(err, ErrSettled) {
		t.Fatalf("a cancel over a failure written since the read = %v, want ErrSettled", err)
	}
	if got, _ := db.GetBook(ctx, b.ID); got.Status != "failed" || got.Error != "asr: out of memory" {
		t.Fatalf("the failure was overwritten: status %q error %q", got.Status, got.Error)
	}

	// The book finished meanwhile.
	if err := db.SetBookStatus(ctx, b.ID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := cancel(); !errors.Is(err, ErrSettled) {
		t.Fatalf("a cancel over a book done since the read = %v, want ErrSettled", err)
	}
	if got, _ := db.GetBook(ctx, b.ID); got.Status != "" {
		t.Fatalf("a done book was cancelled: status %q error %q", got.Status, got.Error)
	}

	// Allowed: the book as read is stopped.
	if err := db.SetBookState(ctx, b.ID, "asr", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := cancel(); err != nil {
		t.Fatalf("a cancel of the book as read = %v", err)
	}

	// A later claim reads as ErrFollowed even with the book changed too.
	if err := db.SetBookStatus(ctx, b.ID, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: "sidecars.generate", Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookStatusUnlessFollowed(ctx, b.ID, "failed", "x", "failed", "cancelled by user", g); !errors.Is(err, ErrFollowed) {
		t.Fatalf("a cancel with a later claim recorded = %v, want ErrFollowed", err)
	}
}

// A job's end is recorded only while no later attempt of it is: one whose attach
// found the mapping open keeps it open.
func TestEndGenerateBookLeavesALaterClaimOpen(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/s", WorkDir: "/w", Title: "S"})
	if err := db.EndGenerateBook(ctx, 1, 1, time.Now()); err != nil {
		t.Fatalf("no mapping = %v, want nil", err)
	}
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "S"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: "sidecars.generate", Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndGenerateBook(ctx, 1, 1, time.Now()); !errors.Is(err, ErrFollowed) {
		t.Fatalf("attempt 1's end with attempt 2 recorded = %v, want ErrFollowed", err)
	}
	if g, _ := db.GetGenerateBook(ctx, 1); g.EndedAt != "" {
		t.Fatalf("attempt 1 ended the mapping attempt 2 follows: %q", g.EndedAt)
	}
	if err := db.EndGenerateBook(ctx, 1, 2, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g, _ := db.GetGenerateBook(ctx, 1); g.EndedAt == "" {
		t.Fatal("attempt 2's own end wasn't recorded")
	}
}
