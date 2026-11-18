package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agentbox"
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

	// Allowed: its job is open, so a recovery of a failed stage goes through (a
	// cancelled book never is: scheduler.SupervisorApply, bridge#37).
	if err := db.SetBookStatus(ctx, book.ID, "failed", "fact pass failed", ""); err != nil {
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
	if err := db.EndGenerateBook(ctx, 7, 1, time.Now()); err != nil {
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

// An event published after the engine stopped (the page's Retry in flight at a
// stop, a job ending as the Bridge stops) is dropped, never a send on a closed
// channel.
func TestEventPersisterAfterClose(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := newEventPersister(db, quiet(), 4)
	p.start()
	hub := events.NewHub(events.DefaultRingSize)
	hub.SetPersister(p.enqueue)
	if err := hub.Publish("before", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if err := hub.Publish("after", map[string]int{"n": 2}); err != nil {
		t.Fatal(err)
	}
	p.Close() // twice is fine too
	// The hub still works: its lock wasn't left held by a panic.
	if err := hub.Publish("again", map[string]int{"n": 3}); err != nil {
		t.Fatal(err)
	}
}

// The engine follows the agent container's slots: a container that starts after
// the Bridge (no runner.json at the engine's start, so 1) or comes back with
// another --slots sets the agent concurrency to what it reports, while one that
// isn't running (a stale runner.json) changes nothing.
func TestFollowAgentSlots(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	st := agentbox.Status{}
	var got []int
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		followAgentSlots(ctx, func() agentbox.Status { mu.Lock(); defer mu.Unlock(); return st }, time.Millisecond, 1,
			func(n int) { mu.Lock(); defer mu.Unlock(); got = append(got, n) })
	}()
	set := func(s agentbox.Status) { mu.Lock(); defer mu.Unlock(); st = s }
	wait := func(want []int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			mu.Lock()
			ok := slices.Equal(got, want)
			mu.Unlock()
			if ok {
				return
			}
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("set = %v, want %v", got, want)
	}
	set(agentbox.Status{Live: false, Slots: 4}) // a stale runner.json
	time.Sleep(20 * time.Millisecond)
	wait(nil)
	set(agentbox.Status{Live: true, Slots: 3}) // the container came up
	wait([]int{3})
	set(agentbox.Status{Live: true, Slots: 2}) // restarted with fewer
	wait([]int{3, 2})
	cancel()
	<-done
}

// The daily transcript sweep starts once the engine is the running one, so even
// its first pass holds each book through the engine: a book the engine holds (a
// description reading it) keeps its transcripts, where a sweep started before
// the engine took no hold and purged it.
func TestTranscriptSweepStartsWithTheEngine(t *testing.T) {
	b := newTestBridge(t)
	ctx := context.Background()
	work := filepath.Join(b.dataDir, "work")
	ended := time.Now().AddDate(-1, 0, 0)
	book := func(job int64, slug string) (store.Book, string) {
		t.Helper()
		wd := filepath.Join(work, slug)
		raw := filepath.Join(wd, "transcripts-raw")
		if err := os.MkdirAll(raw, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(raw, "ch001.json"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		bk, err := b.db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/" + slug, WorkDir: wd, Title: slug, State: "done"})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: job, BookID: &bk.ID, LibraryID: 1, RelPath: slug}); err != nil {
			t.Fatal(err)
		}
		if err := b.db.EndGenerateBook(ctx, job, 1, ended); err != nil {
			t.Fatal(err)
		}
		return bk, raw
	}
	present := func(dir string) bool { _, err := os.Stat(dir); return err == nil }
	// The sweep takes the books in id order: the held one first, so the free one
	// gone means the sweep has been past it.
	held, heldRaw := book(2, "held")
	_, freeRaw := book(1, "free")
	sched := scheduler.New(b.db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, work, false)
	release, ok := sched.Hold(held.ID)
	if !ok {
		t.Fatal("the engine wouldn't hold the book")
	}
	defer release()
	engineCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	t.Cleanup(func() { stop(); wg.Wait() })
	// The first sweep runs to the free book before engineStarted goes on: one
	// started before the engine is stored would find no engine to hold through
	// and purge the held book too.
	b.engineStarted(engineCtx, &engine{sched: sched}, func(f func()) {
		wg.Go(f)
		waitFor(t, "the first sweep", func() bool { return !present(freeRaw) })
	})
	if !present(heldRaw) {
		t.Fatal("the first sweep purged a book the engine holds")
	}
}
