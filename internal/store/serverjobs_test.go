package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestServerJobLifecycle(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	lib, path := int64(3), "Author/Book"
	job := ServerJob{ID: 41, Kind: "bridge.verify", LibraryID: &lib, Path: &path, Params: json.RawMessage(`{"hold_seconds":2}`), Attempt: 1}
	if err := db.RecordClaim(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := db.SetServerJobProgress(ctx, 41, 1, json.RawMessage(`{"step":"stat"}`)); err != nil {
		t.Fatal(err)
	}
	// A stale attempt's progress changes nothing.
	if err := db.SetServerJobProgress(ctx, 41, 9, json.RawMessage(`{"step":"stale"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetServerJob(ctx, 41)
	if err != nil || got.State != JobRunning || string(got.Progress) != `{"step":"stat"}` || *got.Path != path || *got.LibraryID != 3 || got.Reported {
		t.Fatalf("running = %+v, %v", got, err)
	}
	open, _ := db.OpenServerJobs(ctx)
	if len(open) != 1 {
		t.Fatalf("open = %+v", open)
	}

	if err := db.FinishServerJob(ctx, 41, 1, JobDone, json.RawMessage(`{"ok":true}`), ""); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenServerJobs(ctx); len(open) != 1 || open[0].Reported {
		t.Fatalf("an unreported job isn't open: %+v", open)
	}
	if err := db.MarkServerJobReported(ctx, 41, 1); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenServerJobs(ctx); len(open) != 0 {
		t.Fatalf("a reported job is still open: %+v", open)
	}

	// Claimed again (a new attempt) replaces the row.
	job.Attempt = 2
	if err := db.RecordClaim(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetServerJob(ctx, 41)
	if got.Attempt != 2 || got.State != JobRunning || got.Reported || got.FinishedAt != nil || string(got.Result) != "{}" {
		t.Fatalf("reclaimed = %+v", got)
	}
	// A lost job needs no report.
	if err := db.FinishServerJob(ctx, 41, 2, JobLost, nil, "the lease ran out"); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenServerJobs(ctx); len(open) != 0 {
		t.Fatalf("a lost job is open: %+v", open)
	}
	if _, err := db.GetServerJob(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
}

func TestServerJobListAndPrune(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for id := int64(1); id <= 5; id++ {
		if err := db.RecordClaim(ctx, ServerJob{ID: id, Kind: "bridge.verify", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		if id < 5 {
			_ = db.FinishServerJob(ctx, id, 1, JobDone, nil, "")
			_ = db.MarkServerJobReported(ctx, id, 1)
		}
	}
	page, _ := db.ListServerJobs(ctx, 0, 2)
	if len(page) != 2 || page[0].ID != 5 || page[1].ID != 4 {
		t.Fatalf("first page = %+v", page)
	}
	page, _ = db.ListServerJobs(ctx, 4, 10)
	if len(page) != 3 || page[0].ID != 3 {
		t.Fatalf("second page = %+v", page)
	}
	n, err := db.PruneServerJobs(ctx, 2)
	if err != nil || n != 3 {
		t.Fatalf("pruned %d, %v; want 3", n, err)
	}
	// The running job is never pruned, however old.
	if err := db.RecordClaim(ctx, ServerJob{ID: 6, Kind: "bridge.verify", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PruneServerJobs(ctx, 0); err != nil {
		t.Fatal(err)
	}
	all, _ := db.ListServerJobs(ctx, 0, 50)
	for _, j := range all {
		if j.State != JobRunning {
			t.Fatalf("kept a finished job past keep 0: %+v", all)
		}
	}
	if len(all) != 2 {
		t.Fatalf("the running jobs went: %+v", all)
	}
}

func TestDeleteSetting(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if err := db.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSetting(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.GetSetting(ctx, "k"); ok {
		t.Fatal("still set")
	}
	if err := db.DeleteSetting(ctx, "k"); err != nil {
		t.Fatal(err)
	}
}

// A claim records the server it came from, and a running job's park message
// lives in error until it runs again; a new attempt starts unparked.
func TestServerJobServerAndPark(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if err := db.RecordClaim(ctx, ServerJob{ID: 5, Kind: "sidecars.generate", Attempt: 1, ServerID: "srv-a"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetServerJobReport(ctx, 5, 1, json.RawMessage(`{"step":"waiting"}`), "the agent container isn't answering"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetServerJob(ctx, 5)
	if got.ServerID != "srv-a" || got.State != JobRunning || got.Error != "the agent container isn't answering" ||
		string(got.Progress) != `{"step":"waiting"}` {
		t.Fatalf("parked = %+v", got)
	}
	if err := db.SetServerJobReport(ctx, 5, 1, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetServerJob(ctx, 5); got.Error != "" {
		t.Fatalf("unparked = %+v", got)
	}
	_ = db.SetServerJobReport(ctx, 5, 1, nil, "parked again")
	if err := db.RecordClaim(ctx, ServerJob{ID: 5, Kind: "sidecars.generate", Attempt: 2, ServerID: "srv-b"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetServerJob(ctx, 5); got.ServerID != "srv-b" || got.Error != "" || got.Attempt != 2 {
		t.Fatalf("reclaimed = %+v", got)
	}
	// A finished job's park message can't be written over its error.
	_ = db.FinishServerJob(ctx, 5, 2, JobFailed, nil, "boom")
	_ = db.SetServerJobReport(ctx, 5, 2, nil, "late park")
	if got, _ := db.GetServerJob(ctx, 5); got.Error != "boom" {
		t.Fatalf("a finished job's error = %q", got.Error)
	}
}
