package scheduler

import (
	"context"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/state"
)

// The Bridge's description step records "describing" runs on a finished (or
// contributing-parked) book: a stage outside the state machine, with no sentinel.
// Reconcile must not read one as a completed stage whose sentinel went missing and
// rewind the book to it (state.Order of an unknown stage is 0, before everything).
func TestReconcileIgnoresARunOutsideTheStateMachine(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	b := h.addBook(t, db, "described", "", "")
	_ = db.SetBookState(ctx, b.ID, string(state.Contributing), string(state.StatusNeedsAttention), "needs a core record", string(state.ParkCoreNeeded))
	insID, _ := db.StartStageRun(ctx, b.ID, string(state.Inspecting), 1)
	_ = db.FinishStageRun(ctx, insID, true, nil)
	_ = WriteSentinel(b.WorkDir, string(state.Inspecting), StageResult{})
	descID, _ := db.StartStageRun(ctx, b.ID, "describing", 1)
	_ = db.FinishStageRun(ctx, descID, true, nil)

	sched := New(db, h.hub, NewStubExecutor(0, 0), 2, h.workRoot, false)
	sched.ctx = ctx
	if err := sched.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cur, _ := db.GetBook(ctx, b.ID)
	if cur.State != string(state.Contributing) || cur.ParkCode != string(state.ParkCoreNeeded) {
		t.Fatalf("book moved to %q (%q), want it left at contributing", cur.State, cur.ParkCode)
	}
	if succ, _ := db.SucceededStages(ctx, b.ID); !succ["describing"] || !succ[string(state.Inspecting)] {
		t.Fatalf("successes changed: %v", succ)
	}

	// Denied counterpart: an engine stage's success with no sentinel still rewinds.
	asrID, _ := db.StartStageRun(ctx, b.ID, string(state.ASR), 1)
	_ = db.FinishStageRun(ctx, asrID, true, nil)
	if err := sched.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cur, _ := db.GetBook(ctx, b.ID); cur.State != string(state.ASR) {
		t.Fatalf("rewound to %q, want asr", cur.State)
	}
}
