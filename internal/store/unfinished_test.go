package store

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func bookIDs(books []Book) []int64 {
	out := make([]int64, 0, len(books))
	for _, b := range books {
		out = append(out, b.ID)
	}
	return out
}

// The periodic passes' reads leave out the finished books (done, no status) at
// the query, keep a done book that carries a status, and read a book again as
// soon as it leaves done.
func TestUnfinishedReadsSkipFinishedBooks(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	mk := func(path, st, series, pos string) Book {
		t.Helper()
		b, err := db.CreateBook(ctx, NewBook{SourcePath: path, WorkDir: "/w" + path, Title: path, State: st,
			Series: series, SeriesPos: pos})
		if err != nil {
			t.Fatal(err)
		}
		run, err := db.StartStageRun(ctx, b.ID, "asr", 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishStageRun(ctx, run, true, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := db.SetProgress(ctx, b.ID, "asr", 3, 3); err != nil {
			t.Fatal(err)
		}
		return b
	}
	active := mk("/active", "fact_pass", "", "")
	done := mk("/done", "done", "", "")
	doneSeries := mk("/done-series", "done", "Saga", "2")
	doneFailed := mk("/done-failed", "done", "Saga", "3")
	if err := db.SetBookStatus(ctx, doneFailed.ID, "failed", "cancelled by user", ""); err != nil {
		t.Fatal(err)
	}

	check := func(want ...int64) {
		t.Helper()
		books, err := db.ListUnfinishedBooks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := bookIDs(books); !slices.Equal(got, want) {
			t.Fatalf("ListUnfinishedBooks = %v, want %v", got, want)
		}
		runs, err := db.StageRunsUnfinished(ctx)
		if err != nil {
			t.Fatal(err)
		}
		progress, err := db.ListUnfinishedProgress(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != len(want) || len(progress) != len(want) {
			t.Fatalf("runs for %d books, progress for %d, want %d", len(runs), len(progress), len(want))
		}
		for _, id := range want {
			if len(runs[id]) != 1 || len(progress[id]) != 1 {
				t.Fatalf("book %d: runs %+v, progress %+v", id, runs[id], progress[id])
			}
		}
	}
	check(active.ID, doneFailed.ID)

	// The finished series volumes, identity and position only.
	finished, err := db.ListFinishedSeriesBooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(finished) != 1 {
		t.Fatalf("ListFinishedSeriesBooks = %+v, want the one finished series book", finished)
	}
	if f := finished[0]; f.ID != doneSeries.ID || f.State != "done" || f.Status != "" || f.Series != "Saga" ||
		f.SeriesPos != "2" || f.Title != "" || f.WorkDir != "" {
		t.Fatalf("finished series book = %+v", f)
	}

	// A book that leaves done (a re-run) is read again in full.
	if err := db.SetBookState(ctx, done.ID, "asr", "", "", ""); err != nil {
		t.Fatal(err)
	}
	check(active.ID, done.ID, doneFailed.ID)
	books, _ := db.ListUnfinishedBooks(ctx)
	if books[1].Title != "/done" || books[1].WorkDir != "/w/done" {
		t.Fatalf("a re-run book read without its fields: %+v", books[1])
	}
}
