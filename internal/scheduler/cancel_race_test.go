package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// bridge#37: every supervisor write holds only while the book still has the
// status and error SupervisorApply read, so a Cancel landing between that read
// and the write (the job's end stops the book while the supervisor's tick acts)
// is never cleared nor parked over.
func TestSupervisorWritesNeverUndoACancelThatLandedAfterTheRead(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	actions := map[string]func(store.Book) (string, error){
		"terminate_requeue": func(b store.Book) (string, error) { return s.supervisorTerminateRequeue(ctx, b) },
		"supersede_rerun": func(b store.Book) (string, error) {
			return s.supervisorSupersedeRerun(ctx, b, string(state.Inspecting))
		},
		"stop_budget": func(b store.Book) (string, error) {
			return s.supervisorPark(ctx, b, state.ParkSupervisorBudget, "over budget")
		},
		"park_escalate": func(b store.Book) (string, error) {
			return s.supervisorPark(ctx, b, state.ParkSupervisorEscalated, state.SupervisorMessagePrefix+"loop")
		},
	}
	for name, apply := range actions {
		b := h.addBook(t, db, "stale-"+strings.ReplaceAll(name, "_", "-"), "", "")
		if err := db.SetBookStatus(ctx, b.ID, string(state.StatusNeedsAttention), "waiting for a worker", string(state.ParkASRWorkerWaiting)); err != nil {
			t.Fatal(err)
		}
		read, err := db.GetBook(ctx, b.ID) // the supervisor's read: parked
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Cancel(ctx, b.ID); err != nil { // the job's end lands here
			t.Fatal(err)
		}
		if outcome, err := apply(read); err != nil || outcome != statusChangedOutcome {
			t.Fatalf("%s over a cancel = %q, %v", name, outcome, err)
		}
		if got, err := db.GetBook(ctx, b.ID); err != nil || !IsCancelled(got) {
			t.Fatalf("%s undid the cancel: %+v, %v", name, got, err)
		}
	}
}

// A batch re-admission that met the book's status changed (a re-park or a
// Cancel between the list and the write) is logged at Debug, nothing having
// gone wrong; any other failure stays a Warn (bridge#65).
func TestBatchReadmitStatusChangeLogsAtDebug(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logReadmitFailed(log, "auto-readmit", 7, fmt.Errorf("clear: %w", store.ErrStatusChanged))
	if out := buf.String(); !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "book_id=7") {
		t.Fatalf("a status change logged as %q, want Debug", out)
	}
	buf.Reset()
	logReadmitFailed(log, "auto-readmit", 8, errors.New("database is locked"))
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "database is locked") {
		t.Fatalf("a failure logged as %q, want Warn", out)
	}
}

// A job's guarded cancel publishes the cancel only while the book still carries
// it: a later claim of the job that re-admits the book after the cancel's write
// (here as the cancel stops the running stage) published its own state, and the
// page must not be left showing the book cancelled. Without a later claim the
// cancel is published as stored.
func TestGuardedCancelNeverPublishesOverALaterReadmit(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	hub := events.NewHub(64)
	s := New(db, hub, NewStubExecutor(0, 0), 2, h.workRoot, false)
	lastState := func(id int64) (status, errMsg string) {
		t.Helper()
		replay, sub := hub.Subscribe(0)
		sub.Close()
		found := false
		for _, ev := range replay {
			if ev.Type != "book.state" || ev.BookID != id {
				continue
			}
			var p struct{ Status, Error string }
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				t.Fatal(err)
			}
			status, errMsg, found = p.Status, p.Error, true
		}
		if !found {
			t.Fatalf("no book.state for book %d", id)
		}
		return status, errMsg
	}
	guard := func(job int64, b store.Book) store.StopGuard {
		t.Helper()
		if err := db.RecordClaim(ctx, store.ServerJob{ID: job, Kind: "sidecars.generate", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: job, BookID: &b.ID, LibraryID: 1, RelPath: b.Title}); err != nil {
			t.Fatal(err)
		}
		return store.StopGuard{JobID: job, Attempt: 1}
	}

	plain := h.addBook(t, db, "plain", "", "")
	if err := s.CancelUnlessFollowed(ctx, plain.ID, guard(1, plain)); err != nil {
		t.Fatal(err)
	}
	if status, errMsg := lastState(plain.ID); status != string(state.StatusFailed) || errMsg != cancelledReason {
		t.Fatalf("a guarded cancel published %q %q, want it cancelled", status, errMsg)
	}

	readmitted := h.addBook(t, db, "readmitted", "", "")
	g := guard(2, readmitted)
	s.mu.Lock()
	s.inflight[readmitted.ID] = &inflightBook{cancel: func() {
		b, err := db.GetBook(ctx, readmitted.ID)
		if err != nil || !IsCancelled(b) {
			t.Errorf("the stage was stopped before the cancel was written: %+v, %v", b, err)
			return
		}
		if err := db.RecordClaim(ctx, store.ServerJob{ID: g.JobID, Kind: "sidecars.generate", Attempt: 2}); err != nil {
			t.Error(err)
		}
		if err := s.RetryAsRead(ctx, b); err != nil {
			t.Errorf("the later claim's re-admit: %v", err)
		}
	}}
	s.mu.Unlock()
	if err := s.CancelUnlessFollowed(ctx, readmitted.ID, g); err != nil {
		t.Fatal(err)
	}
	if status, errMsg := lastState(readmitted.ID); status != "" || errMsg != "" {
		t.Fatalf("the page was left at %q %q after the re-admit, want it running", status, errMsg)
	}

	// The caller's context ending once the cancel is written (a client gone, a
	// job's end stopping) still publishes it: the store holds the cancel.
	gone := h.addBook(t, db, "gone", "", "")
	goneGuard := guard(3, gone)
	callerCtx, endCaller := context.WithCancel(ctx)
	defer endCaller()
	s.mu.Lock()
	s.inflight[gone.ID] = &inflightBook{cancel: endCaller}
	s.mu.Unlock()
	if err := s.CancelUnlessFollowed(callerCtx, gone.ID, goneGuard); err != nil {
		t.Fatal(err)
	}
	if status, errMsg := lastState(gone.ID); status != string(state.StatusFailed) || errMsg != cancelledReason {
		t.Fatalf("a cancel whose caller's context ended published %q %q, want it cancelled", status, errMsg)
	}
}
