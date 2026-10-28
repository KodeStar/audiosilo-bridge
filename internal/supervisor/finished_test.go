package supervisor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/pricing"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The tick leaves finished books (done, no status) out: one whose work dir is
// gone (no sentinel for its finished stages) records nothing, while a done book
// carrying a status is still looked at. The finished book is read again from
// the first tick after it leaves done (a re-run), and its missing sentinel is
// then an incident.
func TestCheckSkipsFinishedBooksUntilTheyLeaveDone(t *testing.T) {
	ctx := context.Background()
	db := supervisorDB(t)
	_ = db.EnsureBatch(ctx, "fin", time.Now())
	mk := func(path string) store.Book {
		t.Helper()
		// The work dir is empty: the finished stage's sentinel is missing.
		b, err := db.CreateBook(ctx, store.NewBook{BatchID: "fin", SourcePath: path, WorkDir: t.TempDir(), Title: path,
			State: string(state.Done)})
		if err != nil {
			t.Fatal(err)
		}
		run, err := db.StartStageRun(ctx, b.ID, string(state.ASR), 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishStageRun(ctx, run, true, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		return b
	}
	finished := mk("/finished")
	withStatus := mk("/done-failed")
	if err := db.SetBookStatus(ctx, withStatus.ID, string(state.StatusFailed), "stopped", ""); err != nil {
		t.Fatal(err)
	}
	var seen [][]int64
	s := New(db, config.Default().Supervisor, pricing.Table{Version: "test"}, nil, Hooks{
		Runtime: func(books []store.Book) Runtime {
			ids := make([]int64, 0, len(books))
			for _, b := range books {
				ids = append(ids, b.ID)
			}
			seen = append(seen, ids)
			return Runtime{ActiveBooks: map[int64]bool{}}
		},
	})
	incidents := func() map[int64]int {
		t.Helper()
		runs, err := db.RecentSupervisorRuns(ctx, "fin", 50)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]int{}
		for _, r := range runs {
			if r.BookID != nil {
				out[*r.BookID]++
			}
		}
		return out
	}

	s.check(ctx, "test")
	if got := seen[len(seen)-1]; len(got) != 1 || got[0] != withStatus.ID {
		t.Fatalf("the tick read books %v, want only the done book with a status (%d)", got, withStatus.ID)
	}
	got := incidents()
	if got[finished.ID] != 0 {
		t.Fatalf("a finished book recorded %d incidents", got[finished.ID])
	}
	if got[withStatus.ID] == 0 {
		t.Fatal("a done book with a status was not looked at")
	}

	// A re-run takes the book out of done: the next tick reads it.
	if err := db.SetBookState(ctx, finished.ID, string(state.FactPass), "", "", ""); err != nil {
		t.Fatal(err)
	}
	s.check(ctx, "test")
	if got := incidents(); got[finished.ID] == 0 {
		t.Fatal("a book that left done was not looked at again")
	}
}
