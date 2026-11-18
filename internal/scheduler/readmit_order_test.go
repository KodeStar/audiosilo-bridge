package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/events"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// parkedAudit is a book parked at auditing with its fix budget spent: a fixing
// success per round, the audit's own success and sentinel, and the audit-loop
// files, all of which a Retry of it wipes.
func parkedAudit(t *testing.T, h *harness, db *store.DB, slug string) store.Book {
	t.Helper()
	ctx := context.Background()
	b := h.addBook(t, db, slug, "", "")
	if err := db.SetBookState(ctx, b.ID, string(state.Auditing), string(state.StatusNeedsAttention),
		"audit failed after maximum fix attempts", string(state.ParkFixLoopExhausted)); err != nil {
		t.Fatal(err)
	}
	for range state.MaxFixAttempts {
		recordSuccess(t, db, b.ID, state.Fixing)
	}
	recordSuccess(t, db, b.ID, state.Auditing)
	if err := WriteSentinel(b.WorkDir, string(state.Auditing), StageResult{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{AuditRoundsFile, AuditAcceptedFile} {
		if err := os.WriteFile(filepath.Join(b.WorkDir, name), []byte("[]"), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
	}
	b, err := db.GetBook(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// assertAuditUntouched fails unless the parked audit's sentinel, successes and
// loop files are all still there.
func assertAuditUntouched(t *testing.T, db *store.DB, b store.Book) {
	t.Helper()
	ctx := context.Background()
	if !SentinelExists(b.WorkDir, string(state.Auditing)) {
		t.Error("the auditing sentinel was removed")
	}
	if n, _ := db.CountStageSuccesses(ctx, b.ID, string(state.Fixing)); n != state.MaxFixAttempts {
		t.Errorf("fixing successes = %d, want %d kept", n, state.MaxFixAttempts)
	}
	if n, _ := db.CountStageSuccesses(ctx, b.ID, string(state.Auditing)); n != 1 {
		t.Errorf("auditing successes = %d, want 1 kept", n)
	}
	for _, name := range []string{AuditRoundsFile, AuditAcceptedFile} {
		if _, err := os.Stat(filepath.Join(b.WorkDir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
	if got, err := db.GetBook(ctx, b.ID); err != nil || !IsCancelled(got) {
		t.Errorf("the cancel was undone: %+v, %v", got, err)
	}
}

// cancelledAfterRead is a scheduler over a parked audit (slug) as read, then
// cancelled: a Cancel landing between a caller's read and its change.
func cancelledAfterRead(t *testing.T, slug string) (*Scheduler, *harness, *store.DB, store.Book) {
	t.Helper()
	h := newHarness(t)
	db := h.openDB(t)
	read := parkedAudit(t, h, db, slug)
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 2, h.workRoot, false)
	if err := s.Cancel(context.Background(), read.ID); err != nil {
		t.Fatal(err)
	}
	return s, h, db, read
}

// bridge#65: a readmit that met a Cancel landing after its read changes nothing
// at all: not the status, and not the sentinel, the superseded successes or the
// audit-loop files it would have wiped for a Retry that went through.
func TestReadmitOverACancelLeavesTheBookAsItWas(t *testing.T) {
	s, _, db, read := cancelledAfterRead(t, "readmit-cancel")
	ctx := context.Background()
	if err := s.readmit(ctx, read); !errors.Is(err, store.ErrStatusChanged) {
		t.Fatalf("readmit over a cancel = %v, want ErrStatusChanged", err)
	}
	assertAuditUntouched(t, db, read)
}

// bridge#65: the supervisor's supersede_rerun over a Cancel that landed after its
// read neither moves the book back nor supersedes or drops anything.
func TestSupersedeRerunOverACancelLeavesTheBookAsItWas(t *testing.T) {
	s, _, db, read := cancelledAfterRead(t, "rerun-cancel")
	ctx := context.Background()
	outcome, err := s.supervisorSupersedeRerun(ctx, read, string(state.Fixing))
	if err != nil || outcome != statusChangedOutcome {
		t.Fatalf("supersede_rerun over a cancel = %q, %v", outcome, err)
	}
	if got, _ := db.GetBook(ctx, read.ID); got.State != string(state.Auditing) {
		t.Errorf("the book moved back to %q", got.State)
	}
	assertAuditUntouched(t, db, read)
}

// bridge#65 (P1): a job's Retry reads the book parked and the engine's Retry
// re-admits it later; a Cancel between the two used to be read as a cancelled
// book to take up again. RetryAsRead holds the caller's read: the Retry is refused
// with ErrStatusChanged and the book stays cancelled, untouched.
func TestRetryAsReadRefusesABookCancelledSinceTheRead(t *testing.T) {
	s, h, db, read := cancelledAfterRead(t, "retry-cancel")
	ctx := context.Background()
	if err := s.RetryAsRead(ctx, read); !errors.Is(err, store.ErrStatusChanged) {
		t.Fatalf("a Retry of the parked read over a cancel = %v, want ErrStatusChanged", err)
	}
	assertAuditUntouched(t, db, read)

	// The read it was given decides what applies: a running book's read is no Retry.
	running := h.addBook(t, db, "retry-running", "", "")
	if err := s.RetryAsRead(ctx, running); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("a Retry of a running book = %v, want ErrInvalidOp", err)
	}
	// One read just now re-admits as Retry does.
	again := parkedAudit(t, h, db, "retry-landed")
	if err := s.RetryAsRead(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetBook(ctx, again.ID); got.Status != "" || SentinelExists(again.WorkDir, string(state.Auditing)) {
		t.Fatalf("after the Retry: status %q, sentinel kept %v", got.Status,
			SentinelExists(again.WorkDir, string(state.Auditing)))
	}
}

// A readmit clears the status before it drops the sentinel (bridge#65), so the
// dispatch loop is held off the book meanwhile: no stage starts on it while a
// control change holds it, nor in a pass that read its books before the release
// (it may have read the book half changed). The next pass takes it.
func TestDispatchSkipsABookHeldMidChange(t *testing.T) {
	t.Parallel()
	s := New(nil, events.NewHub(8), NewStubExecutor(0, 0), 2, "", false)
	dispatchable := func(id int64) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.dispatchableLocked(id)
	}
	s.beginPass()
	release := s.holdDispatch(7)
	if dispatchable(7) {
		t.Fatal("a held book is dispatchable")
	}
	if !dispatchable(8) {
		t.Fatal("another book is held too")
	}
	inner := s.holdDispatch(7) // two changes at once: both must end
	release()
	release() // idempotent
	if dispatchable(7) {
		t.Fatal("the book was let go while another change still holds it")
	}
	inner()
	if dispatchable(7) {
		t.Fatal("a pass that read its books before the release starts the book")
	}
	select {
	case <-s.wake:
	default:
		t.Fatal("the release didn't wake the dispatch loop")
	}
	s.beginPass()
	if !dispatchable(7) {
		t.Fatal("the next pass doesn't take the book")
	}
	if len(s.releasedAt) != 0 {
		t.Fatalf("release marks the pass has passed are kept: %v", s.releasedAt)
	}
}

// bridge#65: a start's reconcile rewinds a book by its state alone, so a cancel
// written to the store after the reconcile read the book (the Bridge's
// CancelStored while its engine starts) is never written back over.
func TestReconcileRewindKeepsAStatusWrittenSinceItsRead(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "reconcile-cancel", "", "")
	if err := db.SetBookState(ctx, b.ID, string(state.Auditing), "", "", ""); err != nil {
		t.Fatal(err)
	}
	recordSuccess(t, db, b.ID, state.Inspecting) // its sentinel is gone: the rewind goes back there
	read, err := db.GetBook(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := CancelStored(ctx, db, b.ID); err != nil {
		t.Fatal(err)
	}
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 1, h.workRoot, false)
	if err := s.reconcileBook(ctx, read, map[string]bool{string(state.Inspecting): true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetBook(ctx, b.ID); got.State != string(state.Inspecting) || !IsCancelled(got) {
		t.Fatalf("after the reconcile: state %q, status %q error %q; want rewound and still cancelled",
			got.State, got.Status, got.Error)
	}
}
