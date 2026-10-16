package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/workerproto"
)

func newTestBridge(t *testing.T) *Bridge {
	t.Helper()
	b, _, err := New(context.Background(), Options{DataDir: t.TempDir(), Version: "0.2.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.db.Close() })
	return b
}

// Workers: add (the token once), the page's list, the console's list (only
// workers that have connected, with their platform and ASR), remove.
func TestWorkersOnThePageAndTheHeartbeat(t *testing.T) {
	b := newTestBridge(t)
	ctx := context.Background()
	if _, err := b.CreateWorker(ctx, " "); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("no name = %v", err)
	}
	nw, err := b.CreateWorker(ctx, "Mac Studio")
	if err != nil || !strings.HasPrefix(nw.Token, "asw_") || nw.Worker.Name != "Mac Studio" {
		t.Fatalf("create = %+v, %v", nw, err)
	}
	if _, err := b.CreateWorker(ctx, "never connects"); err != nil {
		t.Fatal(err)
	}
	if got := b.linkWorkers(ctx); len(got) != 0 {
		t.Fatalf("workers that never connected went to the server: %+v", got)
	}
	id, err := b.hub.Authenticate(ctx, nw.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.hub.Hello(ctx, id, workerproto.Hello{Version: "0.2.0", Platform: "darwin/arm64", Backend: "mlx-whisper", Device: "metal"}); err != nil {
		t.Fatal(err)
	}
	page, err := b.Workers(ctx)
	if err != nil || len(page) != 2 || !page[0].Connected || page[1].Connected {
		t.Fatalf("page = %+v, %v", page, err)
	}
	got := b.linkWorkers(ctx)
	if len(got) != 1 || got[0].Name != "Mac Studio" || got[0].Platform != "darwin/arm64, mlx-whisper (metal)" || got[0].Version != "0.2.0" ||
		got[0].LastSeen == "" {
		t.Fatalf("heartbeat workers = %+v", got)
	}
	if err := b.RevokeWorker(ctx, nw.Worker.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.RevokeWorker(ctx, nw.Worker.ID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoke twice = %v", err)
	}
	if _, err := b.hub.Authenticate(ctx, nw.Token); err == nil {
		t.Fatal("a revoked worker's token still works")
	}
}

// The heartbeat stays inside the server's bounds (64 workers, 128 bytes a
// field): more would refuse the whole heartbeat.
func TestLinkWorkersFitTheServer(t *testing.T) {
	b := newTestBridge(t)
	ctx := context.Background()
	long := strings.Repeat("v", 400)
	for i := range maxLinkWorkers + 6 {
		nw, err := b.CreateWorker(ctx, "w"+strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		id, err := b.hub.Authenticate(ctx, nw.Token)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.hub.Hello(ctx, id, workerproto.Hello{Version: long, Platform: "linux/amd64"}); err != nil {
			t.Fatal(err)
		}
	}
	got := b.linkWorkers(ctx)
	if len(got) != maxLinkWorkers {
		t.Fatalf("%d workers to the server, want %d", len(got), maxLinkWorkers)
	}
	for _, w := range got {
		if len(w.Version) > maxLinkWorkerText || len(w.Platform) > maxLinkWorkerText {
			t.Fatalf("a field beyond the server's bound: %d, %d bytes", len(w.Version), len(w.Platform))
		}
	}
}

// The ASR fallback is a page setting, locked by the environment.
func TestSaveSettingsFallback(t *testing.T) {
	b := newTestBridge(t)
	ctx := context.Background()
	s := b.Settings()
	if s.ASRWorkerFallback != "local" {
		t.Fatalf("default = %+v", s)
	}
	s.ASRWorkerFallback = "wait"
	if err := b.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := b.Settings().ASRWorkerFallback; got != "wait" || b.config().ASR.WorkerFallback != "wait" {
		t.Fatalf("saved = %q", got)
	}
	// An empty fallback (an older page) keeps it.
	s.ASRWorkerFallback = ""
	if err := b.SaveSettings(ctx, s); err != nil || b.Settings().ASRWorkerFallback != "wait" {
		t.Fatalf("empty = %v, %q", err, b.Settings().ASRWorkerFallback)
	}
	s.ASRWorkerFallback = "gpu"
	if err := b.SaveSettings(ctx, s); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("an unknown fallback = %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(b.dataDir, "config.yaml"))
	if !strings.Contains(string(raw), "worker_fallback: wait") {
		t.Fatalf("config.yaml = %s", raw)
	}
}

func TestSaveSettingsFallbackLocked(t *testing.T) {
	t.Setenv("AUDIOSILO_BRIDGE_ASR_WORKER_FALLBACK", "wait")
	b := newTestBridge(t)
	s := b.Settings()
	if s.ASRWorkerFallback != "wait" || !strings.Contains(strings.Join(s.Locked, ","), "asr.worker_fallback") {
		t.Fatalf("settings = %+v", s)
	}
	s.ASRWorkerFallback = "local"
	if err := b.SaveSettings(context.Background(), s); !errors.Is(err, api.ErrLocked) {
		t.Fatalf("a locked change = %v", err)
	}
	s.ASRWorkerFallback = "wait"
	if err := b.SaveSettings(context.Background(), s); err != nil {
		t.Fatalf("the locked value sent back = %v", err)
	}
}
