package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/state"
)

// A hold reserves a book for work outside the scheduler: a second hold is refused,
// Delete and PurgeScratch see the book busy, no stage is dispatched for it, and
// once released (twice is harmless) the book is the scheduler's again.
func TestHold(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	done := h.addBook(t, db, "done-book", "", "")
	_ = db.SetBookState(ctx, done.ID, string(state.Done), "", "", "")
	queued := h.addBook(t, db, "queued-book", "", "")

	sched := New(db, h.hub, NewStubExecutor(time.Millisecond, 2*time.Millisecond), 2, h.workRoot, false)
	release, ok := sched.Hold(done.ID)
	if !ok {
		t.Fatal("a free book's hold was refused")
	}
	if _, again := sched.Hold(done.ID); again {
		t.Fatal("a held book was held twice")
	}
	if err := sched.Delete(ctx, done.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("Delete of a held book = %v, want ErrBusy", err)
	}
	if err := sched.PurgeScratch(ctx, done.ID); !errors.Is(err, ErrBusy) {
		t.Fatalf("PurgeScratch of a held book = %v, want ErrBusy", err)
	}

	// A held queued book runs no stage until the hold ends.
	releaseQueued, ok := sched.Hold(queued.ID)
	if !ok {
		t.Fatal("the queued book's hold was refused")
	}
	runCtx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { _ = sched.Start(runCtx); close(stopped) }()
	defer func() { cancel(); <-stopped }()
	sched.Notify()
	time.Sleep(50 * time.Millisecond)
	// (The waypoint promotion out of queued runs nothing.)
	if runs, _ := db.ListStageRuns(ctx, queued.ID); len(runs) != 0 {
		t.Fatalf("a held book ran %+v", runs)
	}
	releaseQueued()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := db.GetBook(ctx, queued.ID); b.State == string(state.Done) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the released book never ran")
		}
		time.Sleep(5 * time.Millisecond)
	}

	release()
	release()
	if err := sched.Delete(ctx, done.ID); err != nil {
		t.Fatalf("Delete after the release = %v", err)
	}
}
