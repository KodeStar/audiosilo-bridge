package supervisor

import (
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/pricing"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The Bridge's description step runs on a finished book outside the scheduler: an
// open "describing" run has no scheduler worker, and a finished one no sentinel.
// The supervisor recovers engine stages only, so neither is an incident; the same
// open run under an engine stage still is.
func TestDescribingRunsAreNotIncidents(t *testing.T) {
	t.Parallel()
	ok := true
	old := "2026-10-16T08:00:00.000000000Z"
	runs := []store.StageRun{
		{ID: 1, Stage: "describing", StartedAt: old, FinishedAt: old, Ok: &ok},
		{ID: 2, Stage: "describing", StartedAt: old, HeartbeatAt: old, ProgressAt: old},
	}
	s := New(supervisorDB(t), config.Default().Supervisor, pricing.Table{Version: "test"}, nil, Hooks{})
	pass := passReads{runtime: Runtime{ActiveBooks: map[int64]bool{}}, runtimeAt: time.Now().UTC()}
	book := store.Book{ID: 7, State: "done", WorkDir: t.TempDir()}
	snap := s.buildSnapshot(book, runs, pass)
	if len(snap.Runs) != 0 || len(snap.Artifacts) != 0 {
		t.Fatalf("snapshot kept describing runs: runs %+v, artifacts %+v", snap.Runs, snap.Artifacts)
	}
	if got := Classify(snap, s.policy); len(got) != 0 {
		t.Fatalf("describing runs classified as %+v", got)
	}

	engine := []store.StageRun{{ID: 3, Stage: "fact_pass", StartedAt: old, HeartbeatAt: old, ProgressAt: old}}
	if got := Classify(s.buildSnapshot(store.Book{ID: 8, State: "fact_pass", WorkDir: t.TempDir()}, engine, pass), s.policy); len(got) == 0 {
		t.Fatal("an orphaned engine run was not an incident")
	}
}
