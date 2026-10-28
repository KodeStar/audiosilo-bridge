package scheduler

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The dispatch read leaves the finished books (done, no status) out except as
// series identities: a finished first volume still keeps the second one a wave
// deep in the ASR order, a finished seriesless book is not read at all, a done
// book with a status is read in full, and a book that leaves done is read in
// full again.
func TestDispatchBooksReadsFinishedBooksOnlyForSeriesRanks(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	mk := func(slug, series, pos string, st state.State) store.Book {
		t.Helper()
		b := h.addBook(t, db, slug, series, pos)
		if err := db.SetBookState(ctx, b.ID, string(st), "", "", ""); err != nil {
			t.Fatal(err)
		}
		b.State = string(st)
		return b
	}
	a1 := mk("a1", "A", "1", state.Done)
	a2 := mk("a2", "A", "2", state.ASR)
	b1 := mk("b1", "B", "1", state.ASR)
	loose := mk("loose", "", "", state.Done)
	failed := mk("failed", "A", "3", state.Done)
	if err := db.SetBookStatus(ctx, failed.ID, string(state.StatusFailed), "stopped", ""); err != nil {
		t.Fatal(err)
	}
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)

	books, err := s.dispatchBooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bookIDs(books), []int64{a1.ID, a2.ID, b1.ID, failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("dispatch read %v, want %v (no seriesless finished book)", got, want)
	}
	if books[0].Title != "" || books[0].Series != "A" || books[0].SeriesPos != "1" || books[0].State != string(state.Done) {
		t.Fatalf("finished series book = %+v, want its series identity only", books[0])
	}
	if books[3].Title != "failed" {
		t.Fatalf("a done book with a status was not read in full: %+v", books[3])
	}
	candidates := []store.Book{books[1], books[2]}
	sortASRLane(candidates, books)
	if got := bookIDs(candidates); !slices.Equal(got, []int64{b1.ID, a2.ID}) {
		t.Fatalf("ASR order = %v, want B1 before A2 (A1 finished keeps A2 a wave deep)", got)
	}

	// A re-run takes the seriesless book out of done: it is read in full again.
	if err := db.SetBookState(ctx, loose.ID, string(state.Contributing), "", "", ""); err != nil {
		t.Fatal(err)
	}
	if books, err = s.dispatchBooks(ctx); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(books, func(b store.Book) bool { return b.ID == loose.ID })
	if i < 0 || books[i].Title != "loose" || books[i].WorkDir == "" {
		t.Fatalf("a book that left done was not read in full: %v", bookIDs(books))
	}
}

// A finished book the scheduler no longer reads is dispatched again once it
// leaves done: its stage runs a second time and it finishes again.
func TestBookLeavingDoneIsDispatchedAgain(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	h.addBook(t, db, "again", "", "")
	exec := NewStubExecutor(time.Millisecond, 2*time.Millisecond)
	books := runUntil(t, db, h.hub, exec, 1, allDone, waitLimit)
	if !allDone(books) {
		t.Fatal("the book never finished")
	}
	// Back to its last stage the way a re-run goes: the stage's success
	// superseded and its sentinel removed.
	b, ctx := books[0], context.Background()
	if err := db.SupersedeStageSuccesses(ctx, b.ID, string(state.Contributing)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(SentinelPath(b.WorkDir, string(state.Contributing))); err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, string(state.Contributing), "", "", ""); err != nil {
		t.Fatal(err)
	}
	ranAgain := func(books []store.Book) bool {
		runs, err := db.ListStageRuns(ctx, b.ID)
		n := 0
		for _, r := range runs {
			if r.Stage == string(state.Contributing) {
				n++
			}
		}
		return allDone(books) && err == nil && n == 2 && SentinelExists(b.WorkDir, string(state.Contributing))
	}
	if books = runUntil(t, db, h.hub, exec, 1, ranAgain, waitLimit); !ranAgain(books) {
		t.Fatalf("the re-run book was not dispatched again: %+v", books)
	}
}
