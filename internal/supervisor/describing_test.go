package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/pricing"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The Bridge's description step runs on a finished book outside the scheduler: an
// open "describing" run has no scheduler worker, and a finished one no sentinel.
// The supervisor recovers engine stages only, so its run reads drop them and
// neither is an incident; the same open run under an engine stage still is.
func TestDescribingRunsAreNotIncidents(t *testing.T) {
	t.Parallel()
	ok := true
	old := "2026-10-16T08:00:00.000000000Z"
	runs := []store.StageRun{
		{ID: 1, Stage: "describing", StartedAt: old, FinishedAt: old, Ok: &ok},
		{ID: 2, Stage: "describing", StartedAt: old, HeartbeatAt: old, ProgressAt: old},
	}
	db := supervisorDB(t)
	s := New(db, config.Default().Supervisor, pricing.Table{Version: "test"}, nil, Hooks{})
	pass := passReads{runtime: Runtime{ActiveBooks: map[int64]bool{}}, runtimeAt: time.Now().UTC()}
	book := store.Book{ID: 7, State: "done", WorkDir: t.TempDir()}
	snap := s.buildSnapshot(book, engineRuns(runs), pass)
	if len(snap.Runs) != 0 || len(snap.Artifacts) != 0 {
		t.Fatalf("snapshot kept describing runs: runs %+v, artifacts %+v", snap.Runs, snap.Artifacts)
	}
	if got := Classify(snap, s.policy); len(got) != 0 {
		t.Fatalf("describing runs classified as %+v", got)
	}

	engine := []store.StageRun{{ID: 3, Stage: "fact_pass", StartedAt: old, HeartbeatAt: old, ProgressAt: old}}
	if kept := engineRuns(engine); &kept[0] != &engine[0] {
		t.Fatal("engineRuns copied a slice with nothing to drop")
	}
	if got := Classify(s.buildSnapshot(store.Book{ID: 8, State: "fact_pass", WorkDir: t.TempDir()}, engine, pass), s.policy); len(got) == 0 {
		t.Fatal("an orphaned engine run was not an incident")
	}

	// The reads drop them at the boundary, one book's and every book's.
	ctx := context.Background()
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/x", WorkDir: t.TempDir(), Title: "X"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.StartStageRun(ctx, b.ID, "fact_pass", 1)
	_, _ = db.StartStageRun(ctx, b.ID, "describing", 1)
	if got, err := s.engineRunsOf(ctx, b.ID); err != nil || len(got) != 1 || got[0].Stage != "fact_pass" {
		t.Fatalf("engineRunsOf = %+v, %v", got, err)
	}
	if all, err := s.engineRunsUnfinished(ctx); err != nil || len(all[b.ID]) != 1 || all[b.ID][0].Stage != "fact_pass" {
		t.Fatalf("engineRunsUnfinished = %+v, %v", all, err)
	}
}
