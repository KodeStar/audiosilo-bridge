package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/state"
)

// TestChaptersGoal: a chapters-only book keeps its goal, is left out of the
// estimates' finished books, and ContinueChapters turns a done one (only a done one)
// into a whole-pipeline book at splitting with the new job's identity, mode and
// work, mapped to the new job in the same transaction.
func TestChaptersGoal(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, err := db.CreateBook(ctx, NewBook{SourcePath: "/lib/A/book.mp3", WorkDir: "/w/a", Title: "A", Goal: state.GoalChapters})
	if err != nil || b.Goal != state.GoalChapters {
		t.Fatalf("created %+v, %v", b, err)
	}
	if got, _ := db.GetBook(ctx, b.ID); got.Goal != state.GoalChapters {
		t.Fatalf("read back goal %q", got.Goal)
	}
	job := GenerateBook{ServerJobID: 7, LibraryID: 1, RelPath: "A/book.mp3"}
	// Denied: a chapters book that isn't done yet doesn't continue, and no mapping
	// is made for it.
	if err := db.ContinueChapters(ctx, b.ID, "done", "splitting", NewBook{ContribMode: "local", WorkID: "w-1"}, job); !errors.Is(err, ErrNotContinuable) {
		t.Fatalf("continue while queued = %v, want ErrNotContinuable", err)
	}
	if _, err := db.GetGenerateBook(ctx, 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused continue mapped its job: %v", err)
	}
	if err := db.SetBookState(ctx, b.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := db.FinishedBooks(ctx); err != nil || len(got) != 0 {
		t.Fatalf("finished = %+v, %v; a chapters book has no characters to count", got, err)
	}
	// The book was matched since the chapters job made it: the continue takes the
	// new job's identity.
	nb := NewBook{Title: "The Book", Authors: []string{"An Author"}, Narrators: []string{"A Reader"}, Series: "The Saga",
		SeriesPos: "2", ASIN: "B0MATCHED", ISBN: "978", IdentitySources: map[string]string{"title": "server"},
		ContribMode: "issue", WorkID: "w-1"}
	if err := db.ContinueChapters(ctx, b.ID, "done", "splitting", nb, job); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetBook(ctx, b.ID)
	if got.Goal != "" || got.State != "splitting" || got.Status != "" || got.ContribMode != "issue" || got.WorkID != "w-1" ||
		got.Title != "The Book" || got.Series != "The Saga" || got.SeriesPos != "2" || got.ASIN != "B0MATCHED" ||
		got.ISBN != "978" || len(got.Authors) != 1 || got.Authors[0] != "An Author" || got.WorkDir != "/w/a" ||
		got.SourcePath != "/lib/A/book.mp3" {
		t.Fatalf("continued = %+v", got)
	}
	if g, err := db.GetGenerateBook(ctx, 7); err != nil || g.BookID == nil || *g.BookID != b.ID || g.EndedAt != "" {
		t.Fatalf("the job's mapping = %+v, %v", g, err)
	}
	// Denied: a whole-pipeline book (this one now) isn't a chapters book.
	if err := db.SetBookState(ctx, b.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.ContinueChapters(ctx, b.ID, "done", "splitting", NewBook{ContribMode: "local"}, GenerateBook{}); !errors.Is(err, ErrNotContinuable) {
		t.Fatalf("continue a whole book = %v, want ErrNotContinuable", err)
	}
	if got, err := db.FinishedBooks(ctx); err != nil || len(got) != 1 {
		t.Fatalf("finished after the continue = %+v, %v", got, err)
	}
}

// A chapters book the retention swept has no transcripts to continue on: it is not
// continuable, and its row is left as it is.
func TestContinueChaptersRefusesASweptBook(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, err := db.CreateBook(ctx, NewBook{SourcePath: "/lib/B", WorkDir: "/w/b", Title: "B", Goal: state.GoalChapters})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 3, BookID: &b.ID, LibraryID: 1, RelPath: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGenerateBookSwept(ctx, b.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.ContinueChapters(ctx, b.ID, "done", "splitting", NewBook{}, GenerateBook{ServerJobID: 4, LibraryID: 1, RelPath: "B"}); !errors.Is(err, ErrNotContinuable) {
		t.Fatalf("continue a swept book = %v, want ErrNotContinuable", err)
	}
	if got, _ := db.GetBook(ctx, b.ID); got.State != "done" || got.Goal != state.GoalChapters {
		t.Fatalf("the swept book was moved: %+v", got)
	}
}

// A chapters book that stopped at chapter_detecting continues there only once it
// failed (a cancel is a failure): at the same state, its status cleared in the
// same statement (so a crash after it never leaves the book stopped), with the new
// job's identity and mapping. Parked (its job may still follow it), asked for at
// another state, or swept, it doesn't, and its row is left as it is.
func TestContinueChaptersAtDetection(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, err := db.CreateBook(ctx, NewBook{SourcePath: "/lib/D.m4b", WorkDir: "/w/d", Title: "D", Goal: state.GoalChapters})
	if err != nil {
		t.Fatal(err)
	}
	at := string(state.ChapterDetecting)
	if err := db.SetBookState(ctx, b.ID, at, string(state.StatusNeedsAttention), "no chapters", string(state.ParkChaptersNotDetected)); err != nil {
		t.Fatal(err)
	}
	job := GenerateBook{ServerJobID: 9, LibraryID: 1, RelPath: "D.m4b"}
	// Denied: parked, not stopped.
	if err := db.ContinueChapters(ctx, b.ID, at, at, NewBook{Title: "D"}, job); !errors.Is(err, ErrNotContinuable) {
		t.Fatalf("continue a parked book = %v, want ErrNotContinuable", err)
	}
	if err := db.SetBookStatus(ctx, b.ID, string(state.StatusFailed), "cancelled by user", ""); err != nil {
		t.Fatal(err)
	}
	// Denied: not where the caller looked.
	if err := db.ContinueChapters(ctx, b.ID, "done", "splitting", NewBook{Title: "D"}, job); !errors.Is(err, ErrNotContinuable) {
		t.Fatalf("continue from done = %v, want ErrNotContinuable", err)
	}
	if _, err := db.GetGenerateBook(ctx, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused continue mapped its job: %v", err)
	}
	// Allowed: stopped there, going on there.
	if err := db.ContinueChapters(ctx, b.ID, at, at, NewBook{Title: "The D", Series: "S", SeriesPos: "1", ContribMode: "local"}, job); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetBook(ctx, b.ID)
	if got.Goal != "" || got.State != at || got.Status != "" || got.Error != "" || got.Title != "The D" ||
		got.Series != "S" || got.ContribMode != "local" {
		t.Fatalf("continued = %+v", got)
	}
	if g, err := db.GetGenerateBook(ctx, 9); err != nil || g.BookID == nil || *g.BookID != b.ID {
		t.Fatalf("the job's mapping = %+v, %v", g, err)
	}

	// Denied: swept.
	c, err := db.CreateBook(ctx, NewBook{SourcePath: "/lib/E.m4b", WorkDir: "/w/e", Title: "E", Goal: state.GoalChapters})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, c.ID, at, string(state.StatusFailed), "cancelled by user", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 10, BookID: &c.ID, LibraryID: 1, RelPath: "E.m4b"}); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkGenerateBookSwept(ctx, c.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.ContinueChapters(ctx, c.ID, at, at, NewBook{}, GenerateBook{ServerJobID: 11, LibraryID: 1, RelPath: "E.m4b"}); !errors.Is(err, ErrNotContinuable) {
		t.Fatalf("continue a swept book = %v, want ErrNotContinuable", err)
	}
	if got, _ := db.GetBook(ctx, c.ID); got.Goal != state.GoalChapters || got.Status != string(state.StatusFailed) {
		t.Fatalf("the swept book was moved: %+v", got)
	}
}

// A book a generate job continued has two jobs' mappings; its retention counts from
// the later job's end, never the earlier chapters job's.
func TestUnsweptCountsFromTheLatestJob(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, err := db.CreateBook(ctx, NewBook{SourcePath: "/lib/C", WorkDir: "/w/c", Title: "C"})
	if err != nil {
		t.Fatal(err)
	}
	chaptersEnd := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	generateEnd := chaptersEnd.Add(34 * 24 * time.Hour)
	for _, g := range []GenerateBook{{ServerJobID: 100, BookID: &b.ID, LibraryID: 1, RelPath: "C"},
		{ServerJobID: 200, BookID: &b.ID, LibraryID: 1, RelPath: "C"}} {
		if err := db.PutGenerateBook(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.EndGenerateBook(ctx, 100, chaptersEnd); err != nil {
		t.Fatal(err)
	}
	// Denied while the later job is still open.
	if got, _ := db.UnsweptGenerateBooks(ctx, chaptersEnd.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("a book a job still follows was listed: %+v", got)
	}
	if err := db.EndGenerateBook(ctx, 200, generateEnd); err != nil {
		t.Fatal(err)
	}
	// Denied: a day after the generate job ended, with 30 days' retention.
	if got, _ := db.UnsweptGenerateBooks(ctx, generateEnd.Add(24*time.Hour).Add(-30*24*time.Hour)); len(got) != 0 {
		t.Fatalf("listed by the earlier job's end: %+v", got)
	}
	// Allowed: once the later job's end is past the cutoff.
	if got, _ := db.UnsweptGenerateBooks(ctx, generateEnd); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("listed = %+v, want the book", got)
	}
}

// A done chapters book can carry a status: a cancel or a supervisor park landing
// as its last stage finished keeps it (the advance writes only the state). The
// continue clears it with the rest, so the book runs at splitting instead of
// failing the new job there with the old run's stop.
func TestContinueChaptersClearsADoneBooksStatus(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for i, st := range []struct{ status, err, code string }{
		{string(state.StatusFailed), "cancelled by user", ""},
		{string(state.StatusNeedsAttention), "supervisor parked this book for operator review", string(state.ParkSupervisorEscalated)},
	} {
		b, err := db.CreateBook(ctx, NewBook{SourcePath: fmt.Sprintf("/lib/S%d.m4b", i), WorkDir: fmt.Sprintf("/w/s%d", i),
			Title: "S", Goal: state.GoalChapters})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetBookState(ctx, b.ID, "done", st.status, st.err, st.code); err != nil {
			t.Fatal(err)
		}
		job := GenerateBook{ServerJobID: int64(20 + i), LibraryID: 1, RelPath: "S.m4b"}
		if err := db.ContinueChapters(ctx, b.ID, "done", "splitting", NewBook{Title: "S"}, job); err != nil {
			t.Fatal(err)
		}
		got, _ := db.GetBook(ctx, b.ID)
		if got.Goal != "" || got.State != "splitting" || got.Status != "" || got.Error != "" || got.ParkCode != "" {
			t.Fatalf("continued a done book with status %q: %+v", st.status, got)
		}
	}
}
