package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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
	if len(s.released) != 0 {
		t.Fatalf("release marks the pass has passed are kept: %v", s.released)
	}
}

// bridge#65: a Cancel holds the dispatch loop off the book as a readmit does, so
// a pass that read the book before the cancel was written starts no stage on it
// once the cancel, finding no stage to stop, has returned. The next pass reads it
// cancelled.
func TestCancelKeepsAPassThatReadTheBookBeforeItOffTheBook(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "cancel-mid-pass", "", "")
	if err := db.SetBookState(ctx, b.ID, string(state.Inspecting), "", "", ""); err != nil {
		t.Fatal(err)
	}
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 1, h.workRoot, false)
	s.ctx = ctx
	s.beginPass()
	read, err := db.GetBook(ctx, b.ID) // the pass's read: runnable
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if s.startWorker(read, state.LaneMechanical) {
		s.wg.Wait()
		t.Fatal("the pass started a stage on the book after its Cancel returned")
	}
	s.beginPass()
	s.mu.Lock()
	again := s.dispatchableLocked(b.ID)
	s.mu.Unlock()
	if !again {
		t.Fatal("the cancel keeps the book off every later pass (a Retry could never run it)")
	}
}

// bridge#65: the pass moves a book on from a waypoint by its state alone, so a
// cancel written after the pass read its books (the Bridge's CancelStored while
// its engine starts, or a Cancel) is never written back over.
func TestWaypointAdvanceKeepsAStatusWrittenSinceItsRead(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "waypoint-cancel", "", "")
	read, err := db.GetBook(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.IsWaypoint(state.State(read.State)) || read.Status != "" {
		t.Fatalf("a new book waits at %q with status %q, want a waypoint and none", read.State, read.Status)
	}
	if err := CancelStored(ctx, db, b.ID); err != nil {
		t.Fatal(err)
	}
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 1, h.workRoot, false)
	next, ok := s.advanceWaypoint(ctx, read)
	if !ok {
		t.Fatal("the waypoint wasn't advanced")
	}
	if got, _ := db.GetBook(ctx, b.ID); got.State != next || !IsCancelled(got) {
		t.Fatalf("after the advance: state %q, status %q error %q; want %q and still cancelled",
			got.State, got.Status, got.Error, next)
	}
}

// bridge#65: a stage's own park or failure is never written over a cancel (one
// landing as the stage ended, or written to the store while it ran): a park over
// it could be re-admitted with no job following the book. Any other book takes it.
func TestAStagesOutcomeIsNeverWrittenOverACancel(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, events.NewHub(8), NewStubExecutor(0, 0), 1, h.workRoot, false)
	cancelled := h.addBook(t, db, "outcome-cancel", "", "")
	if err := CancelStored(ctx, db, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	s.setStatus(cancelled.ID, state.StatusNeedsAttention, "rate limited", state.ParkAgentRateLimited, time.Now().Add(time.Hour))
	if got, _ := db.GetBook(ctx, cancelled.ID); !IsCancelled(got) || got.RetryAt != "" {
		t.Fatalf("the stage's park was written over the cancel: status %q error %q retry_at %q", got.Status, got.Error, got.RetryAt)
	}
	running := h.addBook(t, db, "outcome-running", "", "")
	s.setStatus(running.ID, state.StatusNeedsAttention, "rate limited", state.ParkAgentRateLimited, time.Now().Add(time.Hour))
	if got, _ := db.GetBook(ctx, running.ID); !state.IsParkedWith(got.Status, got.ParkCode, state.ParkAgentRateLimited) || got.RetryAt == "" {
		t.Fatalf("a running book's park wasn't written: status %q park %q retry_at %q", got.Status, got.ParkCode, got.RetryAt)
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

// lateSentinelExecutor's stage runs until its ctx ends and then, as a stage
// whose last write lands with the cancel, writes its sentinel and returns
// success. started and returned close for its first run only.
type lateSentinelExecutor struct {
	calls             atomic.Int32
	started, returned chan struct{}
}

func (e *lateSentinelExecutor) Execute(ctx context.Context, b store.Book, stage state.State, _ StageReport) (StageResult, error) {
	if e.calls.Add(1) == 1 {
		close(e.started)
		defer close(e.returned)
	}
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond)
	res := happyPath()
	return res, WriteSentinel(b.WorkDir, string(stage), res)
}

// supersede_rerun of the stage running on a book waits for the cancelled worker
// before it rewinds: a sentinel the stage wrote as the cancel landed is dropped
// with the others, where it used to outlive the rewind and the re-run replayed it.
func TestSupersedeRerunWaitsForTheCancelledWorker(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "late-sentinel", "", "")
	exec := &lateSentinelExecutor{started: make(chan struct{}), returned: make(chan struct{})}
	s := New(db, h.hub, exec, 1, h.workRoot, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	s.Notify()
	select {
	case <-exec.started:
	case <-time.After(waitLimit):
		t.Fatal("the stage never started")
	}
	running, err := db.GetBook(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := s.supervisorSupersedeRerun(context.Background(), running, running.State)
	if err != nil || outcome == statusChangedOutcome {
		t.Fatalf("supersede_rerun = %q, %v", outcome, err)
	}
	select {
	case <-exec.returned:
	case <-time.After(waitLimit):
		t.Fatal("the cancelled stage never returned")
	}
	if SentinelExists(running.WorkDir, running.State) {
		t.Fatalf("the cancelled %s run's sentinel outlived the rewind: the re-run would replay it", running.State)
	}
}

// publishOnCancelExecutor's first stage runs until its ctx ends and then, as a
// stage whose advance the cancel landed after, publishes the book (taking its
// publish lock) before it returns. started closes for that first run.
type publishOnCancelExecutor struct {
	s       *Scheduler
	calls   atomic.Int32
	started chan struct{}
}

func (e *publishOnCancelExecutor) Execute(ctx context.Context, b store.Book, _ state.State, _ StageReport) (StageResult, error) {
	first := e.calls.Add(1) == 1
	if first {
		close(e.started)
	}
	<-ctx.Done()
	if first {
		time.Sleep(20 * time.Millisecond) // the supersede has gone on past its cancel
		e.s.publishStored(context.Background(), b.ID)
	}
	return StageResult{}, ctx.Err()
}

// supersede_rerun waits for the stage worker it cancelled before it takes the
// book's publish lock, never under it: a worker publishing on its way out returns
// at once, where one waiting for that lock would hold the rewind to workerExitWait.
func TestSupersedeRerunAwaitsItsWorkerOutsideThePublishLock(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "publish-on-exit", "", "")
	exec := &publishOnCancelExecutor{started: make(chan struct{})}
	s := New(db, h.hub, exec, 1, h.workRoot, false)
	exec.s = s
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	s.Notify()
	select {
	case <-exec.started:
	case <-time.After(waitLimit):
		t.Fatal("the stage never started")
	}
	running, err := db.GetBook(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	outcome, err := s.supervisorSupersedeRerun(context.Background(), running, running.State)
	if err != nil || outcome == statusChangedOutcome {
		t.Fatalf("supersede_rerun = %q, %v", outcome, err)
	}
	if took := time.Since(start); took >= workerExitWait/2 {
		t.Fatalf("supersede_rerun took %v: its cancelled worker's last publish waited for the lock it held", took)
	}
}

// supersede_rerun reads the book again once its stage is stopped: one the stage
// advanced to Ready as it was stopped (the supervisor's read was short of it) is
// refused, as at the read, and never moved back.
func TestSupersedeRerunRefusesABookThatReachedReadyMeanwhile(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	b := h.addBook(t, db, "advanced", "", "")
	if err := db.SetBookPipelineState(ctx, b.ID, string(state.Auditing)); err != nil {
		t.Fatal(err)
	}
	read, err := db.GetBook(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookPipelineState(ctx, b.ID, string(state.Ready)); err != nil { // the stage's advance
		t.Fatal(err)
	}
	if outcome, err := s.supervisorSupersedeRerun(ctx, read, string(state.Auditing)); !errors.Is(err, ErrInvalidOp) {
		t.Fatalf("supersede_rerun of a book at ready = %q, %v, want ErrInvalidOp", outcome, err)
	}
	if got, err := db.GetBook(ctx, b.ID); err != nil || got.State != string(state.Ready) {
		t.Fatalf("the book moved back: %+v, %v", got, err)
	}
}
