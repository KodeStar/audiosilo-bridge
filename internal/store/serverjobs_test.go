package store

import (
	"context"
	"database/sql"
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

// A job's share_off (server#218) is kept for its attempt only: a stale
// attempt's answer records nothing, and a new claim carries its own (a re-claim
// the server no longer says it for clears it). An answer read once the attempt
// ended still records it (the book stays local for a later Retry).
func TestServerJobShareOff(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	job := ServerJob{ID: 42, Kind: "sidecars.generate", Attempt: 1}
	if err := db.RecordClaim(ctx, job); err != nil {
		t.Fatal(err)
	}
	shareOff := func() bool {
		t.Helper()
		got, err := db.GetServerJob(ctx, 42)
		if err != nil {
			t.Fatal(err)
		}
		return got.ShareOff
	}
	if err := db.SetServerJobShareOff(ctx, 42, 9); err != nil || shareOff() {
		t.Fatalf("a stale attempt's share_off: %v, kept %v", err, shareOff())
	}
	if err := db.SetServerJobShareOff(ctx, 42, 1); err != nil || !shareOff() {
		t.Fatalf("the attempt's share_off: %v, kept %v", err, shareOff())
	}
	job.Attempt = 2
	if err := db.RecordClaim(ctx, job); err != nil || shareOff() {
		t.Fatalf("a re-claim without share_off: %v, kept %v", err, shareOff())
	}
	job.Attempt, job.ShareOff = 3, true
	if err := db.RecordClaim(ctx, job); err != nil || !shareOff() {
		t.Fatalf("a claim with share_off: %v, kept %v", err, shareOff())
	}
	if err := db.FinishServerJob(ctx, 42, 3, JobDone, nil, ""); err != nil {
		t.Fatal(err)
	}
	job.Attempt, job.ShareOff = 4, false
	if err := db.RecordClaim(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishServerJob(ctx, 42, 4, JobDone, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetServerJobShareOff(ctx, 42, 4); err != nil || !shareOff() {
		t.Fatalf("a finished attempt's share_off: %v, kept %v", err, shareOff())
	}
}

// A claim keeps the pairing it was recorded under (0021). A dropped job is
// recorded lost only while its row is still that claim (allowed); a claim of
// another pairing or server holding the row under the same id and attempt is
// left alone (denied). Rows from before the pairing was kept are given one,
// and only they.
func TestServerJobPairing(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if err := db.RecordClaim(ctx, ServerJob{ID: 5, Kind: "podcast.fetch", Attempt: 1, ServerID: "s", Pairing: "p2"}); err != nil {
		t.Fatal(err)
	}
	for _, dropped := range []ServerJob{
		{ID: 5, Attempt: 1, ServerID: "s", Pairing: "p1"},
		{ID: 5, Attempt: 1, ServerID: "x", Pairing: "p2"},
		{ID: 5, Attempt: 2, ServerID: "s", Pairing: "p2"},
	} {
		if own, err := db.LoseServerJob(ctx, dropped, "dropped"); err != nil || own {
			t.Fatalf("%+v: own %v, %v", dropped, own, err)
		}
	}
	if sj, err := db.GetServerJob(ctx, 5); err != nil || sj.State != JobRunning || sj.Pairing != "p2" {
		t.Fatalf("another claim's row: %+v %v", sj, err)
	}
	if own, err := db.LoseServerJob(ctx, ServerJob{ID: 5, Attempt: 1, ServerID: "s", Pairing: "p2", Result: json.RawMessage(`{"n":1}`)}, "dropped"); err != nil || !own {
		t.Fatalf("own %v, %v", own, err)
	}
	if sj, err := db.GetServerJob(ctx, 5); err != nil || sj.State != JobLost || !sj.Reported || sj.Error != "dropped" || string(sj.Result) != `{"n":1}` {
		t.Fatalf("the dropped row: %+v %v", sj, err)
	}

	// Rows from before the pairing was kept: the paired server's join the
	// pairing in place (allowed); another server's, and one whose server isn't
	// known, get the orphan id no pairing has (denied).
	for id, server := range map[int64]string{6: "s", 7: "x", 8: ""} {
		if err := db.RecordClaim(ctx, ServerJob{ID: id, Kind: "podcast.fetch", Attempt: 1, ServerID: server}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AdoptServerJobs(ctx, "s", "p3", "orphan"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{5: "p2", 6: "p3", 7: "orphan", 8: "orphan"} {
		if sj, err := db.GetServerJob(ctx, id); err != nil || sj.Pairing != want {
			t.Fatalf("job %d: %+v %v, want pairing %q", id, sj, err, want)
		}
	}
}

// Migration 0021 on a Bridge with jobs: an existing row reads no pairing and
// only an adoption gives it one; unpaired (no server), every row is orphaned.
func TestMigrationServerJobsPairing(t *testing.T) {
	ctx := context.Background()
	sqldb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	sqldb.SetMaxOpenConns(1)
	db := &DB{sql: sqldb}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.migrateTo(ctx, "0019_books_share_skipped.sql"); err != nil {
		t.Fatal(err)
	}
	if _, err := sqldb.ExecContext(ctx, `INSERT INTO server_jobs (id, kind, params, attempt, server_id, state, claimed_at, updated_at)
		VALUES (9, 'bridge.verify', '{}', 1, 's', 'running', '2026-11-01T00:00:00Z', '2026-11-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if sj, err := db.GetServerJob(ctx, 9); err != nil || sj.Pairing != "" || sj.State != JobRunning {
		t.Fatalf("migrated row = %+v, %v", sj, err)
	}
	if err := db.AdoptServerJobs(ctx, "", "", "orphan"); err != nil {
		t.Fatal(err)
	}
	if sj, err := db.GetServerJob(ctx, 9); err != nil || sj.Pairing != "orphan" {
		t.Fatalf("a row adopted while unpaired = %+v, %v", sj, err)
	}
}
