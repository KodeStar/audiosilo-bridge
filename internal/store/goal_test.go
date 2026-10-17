package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/state"
)

// TestChaptersGoal: a chapters-only book keeps its goal, is left out of the
// estimates' finished books, and ContinueChapters turns a done one (only a done one)
// into a whole-pipeline book at splitting with the new job's mode and work.
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
	// Denied: a chapters book that isn't done yet doesn't continue.
	if err := db.ContinueChapters(ctx, b.ID, "local", "w-1"); !errors.Is(err, ErrNotContinuable) {
		t.Fatalf("continue while queued = %v, want ErrNotContinuable", err)
	}
	if err := db.SetBookState(ctx, b.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := db.FinishedBooks(ctx); err != nil || len(got) != 0 {
		t.Fatalf("finished = %+v, %v; a chapters book has no characters to count", got, err)
	}
	if err := db.ContinueChapters(ctx, b.ID, "issue", "w-1"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetBook(ctx, b.ID)
	if got.Goal != "" || got.State != "splitting" || got.Status != "" || got.ContribMode != "issue" || got.WorkID != "w-1" {
		t.Fatalf("continued = %+v", got)
	}
	// Denied: a whole-pipeline book (this one now) isn't a chapters book.
	if err := db.SetBookState(ctx, b.ID, "done", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.ContinueChapters(ctx, b.ID, "local", ""); !errors.Is(err, ErrNotContinuable) {
		t.Fatalf("continue a whole book = %v, want ErrNotContinuable", err)
	}
	if got, err := db.FinishedBooks(ctx); err != nil || len(got) != 1 {
		t.Fatalf("finished after the continue = %+v, %v", got, err)
	}
}

func TestGenerateBookSwept(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	b, err := db.CreateBook(ctx, NewBook{SourcePath: "/lib/B", WorkDir: "/w/b", Title: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutGenerateBook(ctx, GenerateBook{ServerJobID: 3, BookID: &b.ID, LibraryID: 1, RelPath: "B"}); err != nil {
		t.Fatal(err)
	}
	if swept, err := db.GenerateBookSwept(ctx, b.ID); err != nil || swept {
		t.Fatalf("swept = %v, %v", swept, err)
	}
	if err := db.MarkGenerateBookSwept(ctx, b.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if swept, err := db.GenerateBookSwept(ctx, b.ID); err != nil || !swept {
		t.Fatalf("swept = %v, %v", swept, err)
	}
}
