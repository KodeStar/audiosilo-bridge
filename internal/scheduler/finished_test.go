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

	books, finished, err := s.dispatchBooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bookIDs(books), []int64{a2.ID, b1.ID, failed.ID}; !slices.Equal(got, want) {
		t.Fatalf("dispatch read %v, want %v (no finished book in full)", got, want)
	}
	if want := []state.SeriesQueueItem{{ID: a1.ID, Series: "A", SeriesPos: "1"}}; !slices.Equal(finished, want) {
		t.Fatalf("finished series volumes = %+v, want %+v (no seriesless finished book)", finished, want)
	}
	if books[2].Title != "failed" {
		t.Fatalf("a done book with a status was not read in full: %+v", books[2])
	}
	candidates := []store.Book{books[0], books[1]}
	sortASRLane(candidates, books, finished)
	if got := bookIDs(candidates); !slices.Equal(got, []int64{b1.ID, a2.ID}) {
		t.Fatalf("ASR order = %v, want B1 before A2 (A1 finished keeps A2 a wave deep)", got)
	}

	// A re-run takes the seriesless book out of done: it is read in full again.
	if err := db.SetBookState(ctx, loose.ID, string(state.Contributing), "", "", ""); err != nil {
		t.Fatal(err)
	}
	if books, _, err = s.dispatchBooks(ctx); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(books, func(b store.Book) bool { return b.ID == loose.ID })
	if i < 0 || books[i].Title != "loose" || books[i].WorkDir == "" {
		t.Fatalf("a book that left done was not read in full: %v", bookIDs(books))
	}
}

// The two dispatch reads are not one snapshot: a book that reached done between
// them is in both, and only its finished identity is kept, so it is neither
// ranked twice in its series nor offered to a lane at the stage it just
// finished.
func TestDropFinishedKeepsABookInBothReadsOnce(t *testing.T) {
	unfinished := func() []store.Book {
		return []store.Book{
			{ID: 1, State: string(state.ASR), Series: "A", SeriesPos: "2", Title: "one"},
			{ID: 3, State: string(state.Contributing), Series: "A", SeriesPos: "3", Title: "three"},
			{ID: 6, State: string(state.FactPass), Series: "B", SeriesPos: "1", Title: "six"},
		}
	}
	finished := []state.SeriesQueueItem{{ID: 2, Series: "A", SeriesPos: "1"}, {ID: 3, Series: "A", SeriesPos: "3"}, {ID: 9, Series: "B", SeriesPos: "2"}}
	if got := dropFinished(unfinished(), finished); !slices.Equal(bookIDs(got), []int64{1, 6}) {
		t.Fatalf("kept %v, want [1 6]", bookIDs(got))
	}
	if got := dropFinished(unfinished(), nil); !slices.Equal(bookIDs(got), []int64{1, 3, 6}) {
		t.Fatalf("no finished books: %v", bookIDs(got))
	}
	if got := dropFinished(nil, finished); len(got) != 0 {
		t.Fatalf("no unfinished books: %v", bookIDs(got))
	}
	// Book 3 is ranked once, as finished: A1 (2) rank 0, A2 (1) rank 1, A3 (3) rank 2.
	ranks := asrBreadthRanks(dropFinished(unfinished(), finished), finished)
	if ranks[2] != 0 || ranks[1] != 1 || ranks[3] != 2 || ranks[6] != 0 || ranks[9] != 1 {
		t.Fatalf("ranks = %v", ranks)
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
