package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/events"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// bridge#37: the supervisor never readmits a cancelled book. A readmit that read
// the book parked before a Cancel landed (the check-then-act window between the
// supervisor's read and its write) leaves the cancel in place.
func TestReadmitNeverUndoesACancelThatLandedAfterTheRead(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "parked", "", "")
	if err := db.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention), "waiting for a worker", string(state.ParkASRWorkerWaiting)); err != nil {
		t.Fatal(err)
	}
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	read, err := db.GetBook(ctx, b.ID) // the supervisor's read: parked
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, b.ID); err != nil { // the job's end lands here
		t.Fatal(err)
	}
	if err := s.readmit(ctx, read); !errors.Is(err, store.ErrStatusChanged) {
		t.Fatalf("readmit over a cancel = %v, want ErrStatusChanged", err)
	}
	got, err := db.GetBook(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !IsCancelled(got) {
		t.Fatalf("the cancel was undone: status=%q error=%q", got.Status, got.Error)
	}
	// The supervisor's own entry point leaves a cancelled book alone, whatever the
	// action that would write it.
	for _, action := range []string{"readmit", "retry", "terminate_requeue", "supersede_rerun", "stop_budget", "park_escalate"} {
		outcome, err := s.SupervisorApply(ctx, action, b.ID, b.State, "a diagnosis")
		if err != nil || outcome != "book was cancelled; left as it is" {
			t.Fatalf("%s on a cancelled book = %q, %v", action, outcome, err)
		}
	}
	if got, _ := db.GetBook(ctx, b.ID); !IsCancelled(got) {
		t.Fatalf("the supervisor wrote over a cancelled book: %+v", got)
	}
	// A person's Retry still takes a cancelled book up again (allowed).
	if err := s.Retry(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetBook(ctx, b.ID); got.Status != "" {
		t.Fatalf("Retry left status %q", got.Status)
	}
}

// Under -race, a supervisor readmit and a Cancel racing on a parked book always
// end with the book cancelled: whichever order the writes take, the readmit
// never lands after the cancel.
func TestSupervisorReadmitRacingCancelEndsCancelled(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	for i := range 20 {
		b := h.addBook(t, db, "race-"+string(rune('a'+i)), "", "")
		if err := db.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention), "parked", string(state.ParkASRWorkerWaiting)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = s.SupervisorApply(ctx, "readmit", b.ID, b.State)
		}()
		go func() {
			defer wg.Done()
			<-start
			if err := s.Cancel(ctx, b.ID); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		got, err := db.GetBook(ctx, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !IsCancelled(got) {
			t.Fatalf("book %d after the race: status=%q error=%q, want cancelled", b.ID, got.Status, got.Error)
		}
	}
}
