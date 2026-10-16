package workers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/asr"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/workerproto"
)

// fakeLocal is the Bridge's own backend: it writes okRaw, or blocks until its
// context ends when block is set.
type fakeLocal struct {
	block   bool
	started chan struct{}
	runs    atomic.Int32
	ready   atomic.Int32
	// onReady runs inside EnsureReady (a worker connecting meanwhile).
	onReady func()
}

func (f *fakeLocal) ID() string { return "whisper-cpp" }
func (f *fakeLocal) Detect(context.Context) (asr.Capability, error) {
	return asr.Capability{Backend: "whisper-cpp", Available: true, Device: "cpu"}, nil
}
func (f *fakeLocal) EnsureReady(context.Context) error {
	f.ready.Add(1)
	if f.onReady != nil {
		f.onReady()
	}
	return nil
}
func (f *fakeLocal) Transcribe(ctx context.Context, job asr.Job) error {
	f.runs.Add(1)
	if f.started != nil {
		close(f.started)
		f.started = nil
	}
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return os.WriteFile(filepath.Join(job.OutDir, asr.RawOutputName(job.Audio)), []byte(okRaw), 0o600)
}

func job(t *testing.T) asr.Job {
	t.Helper()
	return asr.Job{Audio: flac(t, "seg"), OutDir: t.TempDir(), Language: "en", Duration: 10}
}

func readRaw(t *testing.T, j asr.Job) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(j.OutDir, asr.RawOutputName(j.Audio)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func parkCode(err error) state.ParkCode {
	var pe *scheduler.ParkError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// No worker: the fallback decides. local runs the Bridge's own backend; wait
// parks asr_worker_waiting, and so does no local backend.
func TestRouterFallback(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	local := &fakeLocal{}
	wait := false
	r := &Router{Hub: h, Local: local, LocalCap: asr.Capability{Backend: "whisper-cpp", Available: true, Device: "cpu"},
		Wait: func() bool { return wait }, Log: discard()}
	j := job(t)
	if err := r.Transcribe(context.Background(), j); err != nil || readRaw(t, j) != okRaw || local.runs.Load() != 1 || local.ready.Load() != 1 {
		t.Fatalf("local = %v (runs %d, ready %d)", err, local.runs.Load(), local.ready.Load())
	}
	if r.ID() != "whisper-cpp" {
		t.Fatalf("id = %s", r.ID())
	}
	if c, _ := r.Detect(context.Background()); c.Device != "cpu" || !c.Available {
		t.Fatalf("detect = %+v", c)
	}

	if !r.CanRun() {
		t.Fatal("a usable local backend can't run")
	}
	wait = true
	if r.CanRun() {
		t.Fatal("a wait with no worker can run")
	}
	if err := r.Transcribe(context.Background(), job(t)); parkCode(err) != state.ParkASRWorkerWaiting {
		t.Fatalf("wait = %v", err)
	}
	if c, _ := r.Detect(context.Background()); !c.Available || c.Backend != IDWorker {
		t.Fatalf("detect, wait = %+v", c)
	}
	if local.runs.Load() != 1 {
		t.Fatal("wait ran the local backend")
	}

	// No local backend: a wait for a worker too (re-admitted when one connects).
	none := &Router{Hub: h, Wait: func() bool { return false }, Log: discard()}
	if err := none.Transcribe(context.Background(), job(t)); parkCode(err) != state.ParkASRWorkerWaiting {
		t.Fatalf("no local = %v", err)
	}
	if c, _ := none.Detect(context.Background()); !c.Available || c.Backend != IDWorker || none.CanRun() {
		t.Fatalf("detect, no local = %+v", c)
	}
}

// A connected worker gets the segment; its raw output lands where the local
// backend's would.
func TestRouterRemote(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	ctx := context.Background()
	id, _ := pair(t, h, "studio")
	_, _ = h.Hello(ctx, id, workerproto.Hello{})
	local := &fakeLocal{}
	r := &Router{Hub: h, Local: local, LocalCap: asr.Capability{Available: true}, Wait: func() bool { return false }, Log: discard()}
	go func() {
		task, _ := h.Claim(ctx, id, 5*time.Second)
		if task != nil {
			_ = h.Result(ctx, id, task.ID, workerproto.ResultMeta{Attempt: task.Attempt, Backend: "mlx-whisper"}, []byte(okRaw))
		}
	}()
	if r.ID() != IDWorker {
		t.Fatalf("id = %s", r.ID())
	}
	j := job(t)
	if err := r.Transcribe(ctx, j); err != nil || readRaw(t, j) != okRaw {
		t.Fatal(err)
	}
	if local.runs.Load() != 0 || local.ready.Load() != 0 {
		t.Fatal("a worker's segment ran locally")
	}
	if err := r.EnsureReady(ctx); err != nil || local.ready.Load() != 0 {
		t.Fatal("EnsureReady prepared the local backend while a worker is connected")
	}
}

// A worker that goes away mid-segment: the segment falls back to the local
// backend once its lease runs out.
func TestRouterWorkerLost(t *testing.T) {
	t.Parallel()
	h, _, c := newHub(t)
	ctx := context.Background()
	id, _ := pair(t, h, "studio")
	_, _ = h.Hello(ctx, id, workerproto.Hello{})
	local := &fakeLocal{}
	r := &Router{Hub: h, Local: local, LocalCap: asr.Capability{Available: true}, Wait: func() bool { return false }, Log: discard()}
	go func() {
		if task, _ := h.Claim(ctx, id, 5*time.Second); task != nil {
			c.add(workerproto.LeaseDuration + ConnectedWindow) // and never heard from again
		}
	}()
	j := job(t)
	if err := r.Transcribe(ctx, j); err != nil || readRaw(t, j) != okRaw || local.runs.Load() != 1 {
		t.Fatalf("fallback after a lost worker = %v (local runs %d)", err, local.runs.Load())
	}
}

// A local decode yields to a worker that connects while it runs.
func TestRouterLocalYields(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	ctx := context.Background()
	id, _ := pair(t, h, "studio")
	started := make(chan struct{})
	local := &fakeLocal{block: true, started: started}
	r := &Router{Hub: h, Local: local, LocalCap: asr.Capability{Available: true}, Wait: func() bool { return false }, Log: discard()}
	j := job(t)
	done := make(chan error, 1)
	go func() { done <- r.Transcribe(ctx, j) }()
	<-started
	_, _ = h.Hello(ctx, id, workerproto.Hello{})
	task, err := h.Claim(ctx, id, 5*time.Second)
	if err != nil || task == nil {
		t.Fatalf("the worker got nothing: %v", err)
	}
	if err := h.Result(ctx, id, task.ID, workerproto.ResultMeta{Attempt: task.Attempt}, []byte(okRaw)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || readRaw(t, j) != okRaw {
		t.Fatalf("after yielding = %v", err)
	}
}

// A worker that connects while the local backend is prepared (a first model
// download) gets the segment: the edge isn't lost between the look and the wait.
func TestRouterWorkerConnectsWhileLocalPrepares(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	ctx := context.Background()
	id, _ := pair(t, h, "studio")
	local := &fakeLocal{block: true, onReady: func() { _, _ = h.Hello(ctx, id, workerproto.Hello{}) }}
	r := &Router{Hub: h, Local: local, LocalCap: asr.Capability{Available: true}, Wait: func() bool { return false }, Log: discard()}
	j := job(t)
	done := make(chan error, 1)
	go func() { done <- r.Transcribe(ctx, j) }()
	task, err := h.Claim(ctx, id, 5*time.Second)
	if err != nil || task == nil {
		t.Fatalf("the worker got nothing: %v", err)
	}
	if err := h.Result(ctx, id, task.ID, workerproto.ResultMeta{Attempt: task.Attempt}, []byte(okRaw)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil || readRaw(t, j) != okRaw || local.runs.Load() != 0 {
			t.Fatalf("transcribe = %v (local runs %d)", err, local.runs.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the segment stayed on the local backend")
	}
}

// A cancel ends a segment wherever it runs.
func TestRouterCancel(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	local := &fakeLocal{block: true, started: make(chan struct{})}
	started := local.started
	r := &Router{Hub: h, Local: local, LocalCap: asr.Capability{Available: true}, Wait: func() bool { return false }, Log: discard()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Transcribe(ctx, job(t)) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}
