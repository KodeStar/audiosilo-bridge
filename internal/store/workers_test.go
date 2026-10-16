package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkers(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	w, err := db.CreateWorker(ctx, "Mac Studio", "hash-1")
	if err != nil || w.ID == 0 || w.Name != "Mac Studio" || w.CreatedAt == "" || w.LastSeenAt != "" {
		t.Fatalf("create = %+v, %v", w, err)
	}
	if _, err := db.CreateWorker(ctx, "again", "hash-1"); err == nil {
		t.Fatal("a token hash was used twice")
	}
	if got, err := db.WorkerByTokenHash(ctx, "hash-1"); err != nil || got.ID != w.ID {
		t.Fatalf("by hash = %+v, %v", got, err)
	}
	if _, err := db.WorkerByTokenHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown hash = %v", err)
	}
	seen := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	if err := db.SetWorkerInfo(ctx, w.ID, WorkerInfo{Version: "0.2.0", Platform: "darwin/arm64", Backend: "mlx-whisper",
		Device: "metal", Model: "turbo"}, seen); err != nil {
		t.Fatal(err)
	}
	if err := db.AddWorkerSegment(ctx, w.ID, 600, 30); err != nil {
		t.Fatal(err)
	}
	if err := db.AddWorkerSegment(ctx, w.ID, 300, 20); err != nil {
		t.Fatal(err)
	}
	if err := db.TouchWorker(ctx, w.ID, seen.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetWorker(ctx, w.ID)
	if got.Backend != "mlx-whisper" || got.Device != "metal" || got.Segments != 2 || got.AudioSec != 900 || got.DecodeSec != 50 ||
		got.LastSeenAt != Timestamp(seen.Add(time.Minute)) {
		t.Fatalf("worker = %+v", got)
	}
	if ws, _ := db.ListWorkers(ctx); len(ws) != 1 {
		t.Fatalf("list = %+v", ws)
	}
	if err := db.DeleteWorker(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteWorker(ctx, w.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice = %v", err)
	}
	if _, err := db.GetWorker(ctx, w.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
	if ws, _ := db.ListWorkers(ctx); len(ws) != 0 {
		t.Fatalf("list after delete = %+v", ws)
	}
}
