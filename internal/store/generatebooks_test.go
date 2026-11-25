package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/state"
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
	if err := db.EndGenerateBook(ctx, StopGuard{JobID: 12, Attempt: 1}, end); err != nil {
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
	if err := db.EndGenerateBook(ctx, StopGuard{JobID: 12, Attempt: 1}, end); err != nil {
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

// The share_off rule (keepSharedOffLocal): a sidecars.generate job's books
// turn local wherever its share_off or its mapping is written: the claim, the
// answer heard since (even once the job ended, never for another attempt), a
// mapping put after it and a continue's. A book done keeps its mode, another
// kind's job turns nothing, and nothing turns back.
func TestShareOffKeepsTheJobsBooksLocal(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	book := func(name string) int64 {
		t.Helper()
		b, err := db.CreateBook(ctx, NewBook{SourcePath: "/s/" + name, WorkDir: "/w/" + name, Title: name, ContribMode: ContribModeIssue})
		if err != nil {
			t.Fatal(err)
		}
		return b.ID
	}
	claim := func(id int64, kind string, attempt int, shareOff bool) {
		t.Helper()
		if err := db.RecordClaim(ctx, ServerJob{ID: id, Kind: kind, Attempt: attempt, ShareOff: shareOff}); err != nil {
			t.Fatal(err)
		}
	}
	mapTo := func(id, bookID int64) {
		t.Helper()
		if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: id, BookID: &bookID, LibraryID: 1, RelPath: "S"}); err != nil {
			t.Fatal(err)
		}
	}
	mode := func(id int64) string {
		t.Helper()
		b, err := db.GetBook(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return b.ContribMode
	}

	// A re-claim with share_off turns the book the job's mapping names local.
	resumed := book("resumed")
	claim(1, "sidecars.generate", 1, false)
	mapTo(1, resumed)
	if mode(resumed) != ContribModeIssue {
		t.Fatal("a job without share_off turned its book local")
	}
	claim(1, "sidecars.generate", 2, true)
	if mode(resumed) != ContribModeLocal {
		t.Fatal("a claim with share_off left the job's book shared")
	}
	// A claim later without it turns nothing back.
	claim(1, "sidecars.generate", 3, false)
	if mode(resumed) != ContribModeLocal {
		t.Fatal("a claim without share_off turned the book back")
	}

	// A mapping put after the claim's share_off turns the new book local.
	fresh := book("fresh")
	claim(2, "sidecars.generate", 1, true)
	mapTo(2, fresh)
	if mode(fresh) != ContribModeLocal {
		t.Fatal("a book mapped to a job that heard share_off stayed shared")
	}

	// An answer heard since: never another attempt's, and not a done book.
	heard, done := book("heard"), book("done")
	claim(3, "sidecars.generate", 2, false)
	mapTo(3, heard)
	if err := db.SetServerJobShareOff(ctx, 3, 1); err != nil || mode(heard) != ContribModeIssue {
		t.Fatalf("an earlier attempt's answer: %v, mode %q", err, mode(heard))
	}
	if err := db.SetBookState(ctx, done, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	claim(4, "sidecars.generate", 1, false)
	mapTo(4, done)
	if err := db.SetServerJobShareOff(ctx, 4, 1); err != nil || mode(done) != ContribModeIssue {
		t.Fatalf("a done book: %v, mode %q", err, mode(done))
	}
	if err := db.SetServerJobShareOff(ctx, 3, 2); err != nil || mode(heard) != ContribModeLocal {
		t.Fatalf("the attempt's answer: %v, mode %q", err, mode(heard))
	}

	// An answer read after the job ended (a Refresh that crossed a cancel).
	ended := book("ended")
	claim(5, "sidecars.generate", 1, false)
	mapTo(5, ended)
	if err := db.FinishServerJob(ctx, 5, 1, JobCancelled, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetServerJobShareOff(ctx, 5, 1); err != nil || mode(ended) != ContribModeLocal {
		t.Fatalf("an answer after the cancel: %v, mode %q", err, mode(ended))
	}

	// Another kind's job turns nothing.
	other := book("other")
	claim(6, "chapters.detect", 1, true)
	mapTo(6, other)
	if err := db.SetServerJobShareOff(ctx, 6, 1); err != nil || mode(other) != ContribModeIssue {
		t.Fatalf("a chapters job's share_off: %v, mode %q", err, mode(other))
	}

	// A continue maps the job in its own write, over the mode it sets.
	cb, err := db.CreateBook(ctx, NewBook{SourcePath: "/s/cont", WorkDir: "/w/cont", Title: "C", Goal: state.GoalChapters})
	if err != nil {
		t.Fatal(err)
	}
	cont := cb.ID
	if err := db.SetBookState(ctx, cont, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	claim(7, "sidecars.generate", 1, true)
	if err := db.ContinueChapters(ctx, cont, "done", "splitting", NewBook{Title: "C", ContribMode: ContribModeIssue},
		GenerateBook{ServerJobID: 7, LibraryID: 1, RelPath: "C"}); err != nil {
		t.Fatal(err)
	}
	if mode(cont) != ContribModeLocal {
		t.Fatal("a continue for a job that heard share_off left the book shared")
	}
	if b, err := db.GetBook(ctx, resumed); err != nil || b.ShareSkipped != ShareSkippedContributionsOff {
		t.Fatalf("a book share_off turned local = %+v %v, want share_skipped contributions_off", b, err)
	}
}

// KeepBookLocal turns a book local with why (the contributing stage found no
// share job, or the server said share_off): never a done book, nor a local
// one's reason over, and a continue that sets the mode again clears it.
func TestKeepBookLocal(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	get := func(id int64) Book {
		t.Helper()
		b, err := db.GetBook(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	b, err := db.CreateBook(ctx, NewBook{SourcePath: "/s/k", WorkDir: "/w/k", Title: "K", ContribMode: ContribModeIssue,
		Goal: state.GoalChapters})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.KeepBookLocal(ctx, b.ID, ShareSkippedNoShareJob); err != nil {
		t.Fatal(err)
	}
	if got := get(b.ID); got.ContribMode != ContribModeLocal || got.ShareSkipped != ShareSkippedNoShareJob {
		t.Fatalf("kept local = %q %q, want local no_share_job", got.ContribMode, got.ShareSkipped)
	}
	if err := db.KeepBookLocal(ctx, b.ID, ShareSkippedContributionsOff); err != nil ||
		get(b.ID).ShareSkipped != ShareSkippedNoShareJob {
		t.Fatalf("a local book's reason was written over: %v %q", err, get(b.ID).ShareSkipped)
	}
	if err := db.SetBookState(ctx, b.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.ContinueChapters(ctx, b.ID, "done", "splitting", NewBook{Title: "K", ContribMode: ContribModeIssue},
		GenerateBook{}); err != nil {
		t.Fatal(err)
	}
	if got := get(b.ID); got.ContribMode != ContribModeIssue || got.ShareSkipped != "" {
		t.Fatalf("after a continue = %q %q, want issue and no reason", got.ContribMode, got.ShareSkipped)
	}

	done, err := db.CreateBook(ctx, NewBook{SourcePath: "/s/d", WorkDir: "/w/d", Title: "D", ContribMode: ContribModeIssue})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, done.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.KeepBookLocal(ctx, done.ID, ShareSkippedNoShareJob); err != nil || get(done.ID).ContribMode != ContribModeIssue {
		t.Fatalf("a done book: %v, mode %q, want issue", err, get(done.ID).ContribMode)
	}
}

// ResumeShare turns a book kept local with no_share_job back to issue once
// its share job follows it again with its share on (allowed), and never while
// a share_off stands on the book, its job doesn't follow it (mapping ended, or
// another attempt), or the book is local for any other reason (denied).
// ShareResumable answers the same, writing nothing.
func TestResumeShare(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*DB, int64) {
		t.Helper()
		db := open(t)
		if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: "sidecars.generate", Attempt: 2}); err != nil {
			t.Fatal(err)
		}
		b, err := db.CreateMappedBook(ctx, NewBook{SourcePath: "/s/r", WorkDir: "/w/r", Title: "R", ContribMode: ContribModeIssue},
			GenerateBook{ServerJobID: 1, LibraryID: 1, RelPath: "R"})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.KeepBookLocal(ctx, b.ID, ShareSkippedNoShareJob); err != nil {
			t.Fatal(err)
		}
		return db, b.ID
	}
	for _, c := range []struct {
		name    string
		prep    func(t *testing.T, db *DB, id int64)
		attempt int
		want    bool
	}{
		{"follows again", func(*testing.T, *DB, int64) {}, 2, true},
		{"another attempt", func(*testing.T, *DB, int64) {}, 1, false},
		{"its own share_off", func(t *testing.T, db *DB, _ int64) {
			if err := db.SetServerJobShareOff(ctx, 1, 2); err != nil {
				t.Fatal(err)
			}
		}, 2, false},
		{"another job's share_off", func(t *testing.T, db *DB, id int64) {
			if err := db.RecordClaim(ctx, ServerJob{ID: 2, Kind: "sidecars.generate", Attempt: 1, ShareOff: true}); err != nil {
				t.Fatal(err)
			}
			if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 2, BookID: &id, LibraryID: 1, RelPath: "R"}); err != nil {
				t.Fatal(err)
			}
		}, 2, false},
		{"mapping ended", func(t *testing.T, db *DB, _ int64) {
			if err := db.EndGenerateBook(ctx, StopGuard{JobID: 1, Attempt: 2}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}, 2, false},
		{"kept local for share_off", func(t *testing.T, db *DB, id int64) {
			if _, err := db.sql.ExecContext(ctx, `UPDATE books SET share_skipped = ? WHERE id = ?`,
				ShareSkippedContributionsOff, id); err != nil {
				t.Fatal(err)
			}
		}, 2, false},
		{"a share_off a later claim wrote over", func(t *testing.T, db *DB, _ int64) {
			// The job's attempt heard share_off; its next claim, the module on
			// again, resets the row's share_off: the book keeps the share dropped.
			if err := db.SetServerJobShareOff(ctx, 1, 2); err != nil {
				t.Fatal(err)
			}
			if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: "sidecars.generate", Attempt: 3}); err != nil {
				t.Fatal(err)
			}
		}, 3, false},
		{"a done book's share_off", func(t *testing.T, db *DB, id int64) {
			if err := db.SetBookState(ctx, id, "done", "", "", ""); err != nil {
				t.Fatal(err)
			}
			if err := db.SetServerJobShareOff(ctx, 1, 2); err != nil {
				t.Fatal(err)
			}
			if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: "sidecars.generate", Attempt: 3}); err != nil {
				t.Fatal(err)
			}
		}, 3, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, id := setup(t)
			c.prep(t, db, id)
			ok, err := db.ShareResumable(ctx, 1, c.attempt, id)
			if err != nil || ok != c.want {
				t.Fatalf("ShareResumable = %v %v, want %v", ok, err, c.want)
			}
			if b, _ := db.GetBook(ctx, id); b.ContribMode != ContribModeLocal {
				t.Fatalf("ShareResumable wrote the mode: %q", b.ContribMode)
			}
			turned, err := db.ResumeShare(ctx, 1, c.attempt, id)
			if err != nil || turned != c.want {
				t.Fatalf("ResumeShare = %v %v, want %v", turned, err, c.want)
			}
			b, err := db.GetBook(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			wantMode, local := ContribModeIssue, !c.want
			if local {
				wantMode = ContribModeLocal
			}
			if b.ContribMode != wantMode || (b.ShareSkipped == "") == local {
				t.Fatalf("book = %q %q, want mode %q", b.ContribMode, b.ShareSkipped, wantMode)
			}
		})
	}
}

// CreateMappedBook writes the book and its job's mapping together (the job's
// share_off applied in the same write); a book it can't insert (a duplicate
// source) leaves no mapping behind.
func TestCreateMappedBook(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: "sidecars.generate", Attempt: 1, ShareOff: true}); err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateMappedBook(ctx, NewBook{SourcePath: "/s/m", WorkDir: "/w/m", Title: "M", ContribMode: ContribModeIssue},
		GenerateBook{ServerJobID: 1, LibraryID: 1, RelPath: "M"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := db.GetGenerateBook(ctx, 1)
	if err != nil || g.BookID == nil || *g.BookID != b.ID {
		t.Fatalf("mapping = %+v %v, want book %d", g, err, b.ID)
	}
	if got, err := db.GetBook(ctx, b.ID); err != nil || got.ContribMode != ContribModeLocal {
		t.Fatalf("book of a share_off job = %+v %v, want local", got, err)
	}

	if _, err := db.CreateMappedBook(ctx, NewBook{SourcePath: "/s/m", WorkDir: "/w/m2", Title: "M"},
		GenerateBook{ServerJobID: 2, LibraryID: 1, RelPath: "M"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate = %v, want ErrDuplicate", err)
	}
	if _, err := db.GetGenerateBook(ctx, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a duplicate's mapping = %v, want none", err)
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
	if got[0].Kind != "audio" || got[0].CostUSD != 10 || got[0].DurationSec != 7200 {
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
	if err := db.EndGenerateBook(ctx, StopGuard{JobID: 1, Attempt: 1}, time.Now()); err != nil {
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
	if err := db.EndGenerateBook(ctx, StopGuard{JobID: 1, Attempt: 1}, time.Now()); err != nil {
		t.Fatalf("no mapping = %v, want nil", err)
	}
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "S"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: "sidecars.generate", Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndGenerateBook(ctx, StopGuard{JobID: 1, Attempt: 1}, time.Now()); !errors.Is(err, ErrFollowed) {
		t.Fatalf("attempt 1's end with attempt 2 recorded = %v, want ErrFollowed", err)
	}
	if g, _ := db.GetGenerateBook(ctx, 1); g.EndedAt != "" {
		t.Fatalf("attempt 1 ended the mapping attempt 2 follows: %q", g.EndedAt)
	}
	if err := db.EndGenerateBook(ctx, StopGuard{JobID: 1, Attempt: 2}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if g, _ := db.GetGenerateBook(ctx, 1); g.EndedAt == "" {
		t.Fatal("attempt 2's own end wasn't recorded")
	}
}

// Job ids are each server's own, so another server's claim (the Bridge paired
// since) may hold a job's row under the same id and attempt: a guard of the
// earlier claim refuses then (denied: its book stopped, its mapping ended, by
// an end that is no longer that claim's), as it does for another pairing's
// claim of the same server; the claim holding the row passes (allowed), and a
// guard with no server (a claim no runner made) still reads the attempt only.
func TestStopGuardTellsClaimsOfTheSameIDApart(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/s", WorkDir: "/w", Title: "S"})
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "S"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: "sidecars.generate", Attempt: 1, ServerID: "b", Pairing: "p2"}); err != nil {
		t.Fatal(err)
	}
	cancel := func(g StopGuard) error {
		_ = db.SetBookStatus(ctx, b.ID, "", "", "")
		return db.SetBookStatusUnlessFollowed(ctx, b.ID, "", "", "failed", "cancelled by user", g)
	}
	for name, g := range map[string]StopGuard{
		"another server's claim": {JobID: 1, Attempt: 1, ServerID: "a", Pairing: "p1"},
		"another pairing's":      {JobID: 1, Attempt: 1, ServerID: "b", Pairing: "p1"},
	} {
		if other, err := db.OtherClaim(ctx, g); err != nil || !other {
			t.Fatalf("%s: OtherClaim = %v %v, want true", name, other, err)
		}
		if err := cancel(g); !errors.Is(err, ErrFollowed) {
			t.Fatalf("%s: cancel = %v, want ErrFollowed", name, err)
		}
		if got, _ := db.GetBook(ctx, b.ID); got.Status != "" {
			t.Fatalf("%s: the book was stopped: status %q", name, got.Status)
		}
		if err := db.EndGenerateBook(ctx, g, time.Now()); !errors.Is(err, ErrFollowed) {
			t.Fatalf("%s: end = %v, want ErrFollowed", name, err)
		}
		if m, _ := db.GetGenerateBook(ctx, 1); m.EndedAt != "" {
			t.Fatalf("%s: the claim's mapping was ended: %q", name, m.EndedAt)
		}
	}
	for name, g := range map[string]StopGuard{
		"the claim's own": {JobID: 1, Attempt: 1, ServerID: "b", Pairing: "p2"},
		"no runner's":     {JobID: 1, Attempt: 1},
	} {
		if other, err := db.OtherClaim(ctx, g); err != nil || other {
			t.Fatalf("%s: OtherClaim = %v %v, want false", name, other, err)
		}
		if err := cancel(g); err != nil {
			t.Fatalf("%s: cancel = %v", name, err)
		}
	}
	if err := db.EndGenerateBook(ctx, StopGuard{JobID: 1, Attempt: 1, ServerID: "b", Pairing: "p2"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.GetGenerateBook(ctx, 1); m.EndedAt == "" {
		t.Fatal("the claim's own end wasn't recorded")
	}
}

// Another server's claim of the same id and attempt of a kind that follows no
// engine book (its podcast.fetch) never takes the mapping's book over: the
// earlier claim's guarded cancel and end still pass (allowed), where refused
// the book would run on with its mapping open and nothing left to stop it; a
// claim of an engine kind still refuses them (denied).
func TestStopGuardIgnoresAClaimThatFollowsNoEngineBook(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/s", WorkDir: "/w", Title: "S"})
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "S"}); err != nil {
		t.Fatal(err)
	}
	dropped := StopGuard{JobID: 1, Attempt: 1, ServerID: "a", Pairing: "p1"}
	record := func(kind string) {
		t.Helper()
		if err := db.RecordClaim(ctx, ServerJob{ID: 1, Kind: kind, Attempt: 1, ServerID: "b", Pairing: "p2"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"sidecars.generate", "chapters.detect", "readalong.align"} {
		record(kind)
		if other, err := db.OtherClaim(ctx, dropped); err != nil || !other {
			t.Fatalf("%s: OtherClaim = %v %v, want true", kind, other, err)
		}
	}
	record("podcast.fetch")
	if other, err := db.OtherClaim(ctx, dropped); err != nil || other {
		t.Fatalf("OtherClaim = %v %v, want false", other, err)
	}
	if err := db.SetBookStatusUnlessFollowed(ctx, b.ID, "", "", "failed", "cancelled by user", dropped); err != nil {
		t.Fatalf("cancel = %v", err)
	}
	if err := db.EndGenerateBook(ctx, dropped, time.Now()); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.GetGenerateBook(ctx, 1); m.EndedAt == "" {
		t.Fatal("the earlier claim's end wasn't recorded")
	}
}

// Only a live claim of another server or pairing follows the book: a finished
// row of one, left under the job's id when the guard's own claim could not be
// recorded (RecordClaim failed), never keeps that claim from stopping and
// ending its own book (allowed: a guard of a server's claim, and one no runner
// made, whatever the old row's attempt). The same row still running refuses
// them (denied), and a later attempt of the guard's own server and pairing
// counts finished or not (denied).
func TestStopGuardIgnoresAFinishedClaimOfAnotherServer(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, _ := db.CreateBook(ctx, NewBook{SourcePath: "/s", WorkDir: "/w", Title: "S"})
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "S"}); err != nil {
		t.Fatal(err)
	}
	row := func(j ServerJob, finished bool) {
		t.Helper()
		j.ID, j.Kind = 1, "sidecars.generate"
		if err := db.RecordClaim(ctx, j); err != nil {
			t.Fatal(err)
		}
		if finished {
			if err := db.FinishServerJob(ctx, 1, j.Attempt, JobDone, nil, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	guards := map[string]StopGuard{
		"a server's claim": {JobID: 1, Attempt: 1, ServerID: "a", Pairing: "p1"},
		"no runner's":      {JobID: 1, Attempt: 1},
	}
	other := func(g StopGuard) bool {
		t.Helper()
		got, err := db.OtherClaim(ctx, g)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, attempt := range []int{1, 3} {
		row(ServerJob{Attempt: attempt, ServerID: "b", Pairing: "p2"}, true)
		for name, g := range guards {
			if other(g) {
				t.Fatalf("%s: another server's finished row (attempt %d) is taken for a claim that follows the book", name, attempt)
			}
		}
	}
	g := guards["a server's claim"]
	_ = db.SetBookStatus(ctx, b.ID, "", "", "")
	if err := db.SetBookStatusUnlessFollowed(ctx, b.ID, "", "", "failed", "cancelled by user", g); err != nil {
		t.Fatalf("the claim's own cancel = %v", err)
	}
	if err := db.EndGenerateBook(ctx, g, time.Now()); err != nil {
		t.Fatalf("the claim's own end = %v", err)
	}
	row(ServerJob{Attempt: 3, ServerID: "b", Pairing: "p2"}, false)
	for name, g := range guards {
		if !other(g) {
			t.Fatalf("%s: another server's running claim is not taken for one that follows the book", name)
		}
	}
	row(ServerJob{Attempt: 2, ServerID: "a", Pairing: "p1"}, true)
	if !other(g) {
		t.Fatal("a later attempt of the claim's own server and pairing, finished, is not taken for another claim")
	}
}
