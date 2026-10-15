package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/events"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/supervisor"
)

// The supervisor's automatic actions reach only a book a sidecars.generate job
// still follows: a book whose job ended (here an admin's cancel) stays as the
// job left it instead of running again with nothing to take its result.
func TestSupervisorApplyOnlyFollowedBooks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	work := filepath.Join(dir, "work")
	sched := scheduler.New(db, events.NewHub(events.DefaultRingSize), scheduler.NewStubExecutor(0, 0), 1, work, false)
	batch, err := db.CreateBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	book, err := db.CreateBook(ctx, store.NewBook{BatchID: batch.ID, SourcePath: "/lib/A/B", WorkDir: filepath.Join(work, "b"),
		Title: "B", State: "fact_pass"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 7, BookID: &book.ID, LibraryID: 1, RelPath: "A/B"}); err != nil {
		t.Fatal(err)
	}
	apply := supervisorApply(db, sched)
	inc := supervisor.Incident{BookID: book.ID, Stage: "fact_pass", Diagnosis: "parked"}

	// Allowed: its job is open, so a recovery goes through.
	if err := sched.Cancel(ctx, book.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(ctx, supervisor.ActionReadmit, inc); err != nil {
		t.Fatalf("a followed book's readmit = %v", err)
	}
	if got, _ := db.GetBook(ctx, book.ID); got.Status != "" {
		t.Fatalf("a followed book wasn't readmitted: %+v", got)
	}

	// Denied: its job ended (cancelled), so nothing brings the book back.
	if err := sched.Cancel(ctx, book.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.EndGenerateBook(ctx, 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, a := range []supervisor.Action{supervisor.ActionParkEscalate, supervisor.ActionRetry, supervisor.ActionReadmit} {
		if _, err := apply(ctx, a, inc); err == nil {
			t.Errorf("%s on a book no job follows went through", a)
		}
	}
	if got, _ := db.GetBook(ctx, book.ID); !scheduler.IsCancelled(got) {
		t.Fatalf("an ended job's book was revived: %+v", got)
	}
	if _, err := apply(ctx, supervisor.ActionFallbackBackend, inc); err == nil {
		t.Fatal("a backend failover went through")
	}
}

// A long transcription isn't stopped for its length (it bounds itself chapter
// by chapter, and a CPU takes hours on a long book); an agent stage still is.
func TestSupervisorApplyLeavesALongTranscription(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	work := filepath.Join(dir, "work")
	sched := scheduler.New(db, events.NewHub(events.DefaultRingSize), scheduler.NewStubExecutor(0, 0), 1, work, false)
	batch, err := db.CreateBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apply := supervisorApply(db, sched)
	for i, stage := range []string{"asr", "fact_pass"} {
		book, err := db.CreateBook(ctx, store.NewBook{BatchID: batch.ID, SourcePath: "/lib/" + stage, WorkDir: filepath.Join(work, stage),
			Title: stage, State: stage})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: int64(10 + i), BookID: &book.ID, LibraryID: 1, RelPath: stage}); err != nil {
			t.Fatal(err)
		}
		_, err = apply(ctx, supervisor.ActionStopBudget, supervisor.Incident{BookID: book.ID, Stage: stage, Diagnosis: "stage exceeded its duration limit"})
		got, _ := db.GetBook(ctx, book.ID)
		switch stage {
		case "asr":
			if err == nil || got.Status != "" {
				t.Fatalf("a long transcription was stopped: %v, %+v", err, got)
			}
		default:
			if err != nil || got.ParkCode != "supervisor_budget" {
				t.Fatalf("an agent stage over its limit wasn't stopped: %v, %+v", err, got)
			}
		}
	}
}
