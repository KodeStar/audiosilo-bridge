package workers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/workerproto"
)

// A complete raw transcript (the openai-whisper/mlx shape).
const okRaw = `{"text":" hello","segments":[{"id":0,"start":0,"end":1.5,"text":" hello"}]}`

// clock is a settable time for the hub's leases and windows.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newHub(t *testing.T) (*Hub, *store.DB, *clock) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := &clock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	return New(Options{DB: db, BridgeVersion: "0.2.0", Now: c.now}), db, c
}

// pair makes a worker and authenticates it, returning its id and token.
func pair(t *testing.T, h *Hub, name string) (int64, string) {
	t.Helper()
	w, token, err := h.CreateWorker(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	id, err := h.Authenticate(context.Background(), token)
	if err != nil || id != w.ID {
		t.Fatalf("authenticate = %d, %v", id, err)
	}
	return id, token
}

func flac(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ch001.flac")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Security: a token authenticates only its own worker, only while the worker
// exists (allowed and denied).
func TestTokens(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	ctx := context.Background()
	if _, _, err := h.CreateWorker(ctx, "  \x00 "); !errors.Is(err, ErrNoName) {
		t.Fatalf("no name = %v", err)
	}
	id, token := pair(t, h, "Mac Studio")
	for _, bad := range []string{"", "asw_", token + "x", token[4:], "Bearer " + token} {
		if _, err := h.Authenticate(ctx, bad); !errors.Is(err, workerproto.ErrUnauthorized) {
			t.Errorf("denied: %q = %v", bad, err)
		}
	}
	if err := h.RevokeWorker(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Authenticate(ctx, token); !errors.Is(err, workerproto.ErrUnauthorized) {
		t.Fatalf("a revoked token = %v", err)
	}
	if err := h.RevokeWorker(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoking twice = %v", err)
	}
}

// The whole loop: queue, claim, audio, heartbeat, result.
func TestRunClaimResult(t *testing.T) {
	t.Parallel()
	h, db, _ := newHub(t)
	ctx := context.Background()
	id, _ := pair(t, h, "studio")
	if _, err := h.Hello(ctx, id, workerproto.Hello{Protocol: 1, Version: "0.2.0", Backend: "mlx-whisper", Device: "metal",
		Platform: "darwin/arm64", Model: "turbo"}); err != nil {
		t.Fatal(err)
	}
	if !h.Connected() {
		t.Fatal("a worker that said hello isn't connected")
	}
	audio := flac(t, "fLaC 1")
	type res struct {
		out *Outcome
		err error
	}
	done := make(chan res, 1)
	go func() {
		out, err := h.Run(ctx, Segment{Audio: audio, Start: 20, Duration: 85, Language: "en"})
		done <- res{out, err}
	}()
	task, err := h.Claim(ctx, id, 5*time.Second)
	if err != nil || task == nil {
		t.Fatalf("claim = %+v, %v", task, err)
	}
	if task.Attempt != 1 || task.Start != 20 || task.Duration != 85 || task.Size != 6 || task.SHA256 == "" || task.Language != "en" {
		t.Fatalf("task = %+v", task)
	}
	if p, err := h.Audio(id, task.ID, 1); err != nil || p != audio {
		t.Fatalf("audio = %q, %v", p, err)
	}
	if _, err := h.Audio(id, task.ID, 2); !errors.Is(err, workerproto.ErrNotHeld) {
		t.Fatalf("audio, another attempt = %v", err)
	}
	if a, err := h.Heartbeat(id, task.ID, 1); err != nil || a != 1 {
		t.Fatalf("heartbeat = %d, %v", a, err)
	}
	if err := h.Result(ctx, id, task.ID, workerproto.ResultMeta{Attempt: 1}, []byte(`{"text":`)); !errors.Is(err, workerproto.ErrInvalidResult) {
		t.Fatalf("an incomplete result = %v", err)
	}
	meta := workerproto.ResultMeta{Attempt: 1, Backend: "mlx-whisper", Device: "metal", Model: "turbo", DecodeSeconds: 4}
	if err := h.Result(ctx, id, task.ID, meta, []byte(okRaw)); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || string(r.out.Raw) != okRaw || r.out.Worker != "studio" || r.out.Device != "metal" {
		t.Fatalf("run = %+v, %v", r.out, r.err)
	}
	// Done: a late heartbeat isn't held (stop), a second result neither.
	if _, err := h.Heartbeat(id, task.ID, 1); !errors.Is(err, workerproto.ErrNotHeld) {
		t.Fatalf("heartbeat after done = %v", err)
	}
	if err := h.Result(ctx, id, task.ID, meta, []byte(okRaw)); !errors.Is(err, workerproto.ErrNotHeld) {
		t.Fatalf("a second result = %v", err)
	}
	if _, err := h.Heartbeat(id, "nosuch", 1); !errors.Is(err, workerproto.ErrUnknownTask) {
		t.Fatalf("heartbeat, unknown = %v", err)
	}
	ws, _ := db.ListWorkers(ctx)
	if len(ws) != 1 || ws[0].Segments != 1 || ws[0].AudioSec != 85 || ws[0].DecodeSec != 4 || ws[0].Backend != "mlx-whisper" {
		t.Fatalf("worker stats = %+v", ws)
	}
}

// The same audio and options are the same task id; other options another.
func TestTaskID(t *testing.T) {
	t.Parallel()
	a := taskID("sum", Segment{Language: "en"})
	if a != taskID("sum", Segment{Language: "en", Start: 9, Audio: "/elsewhere"}) {
		t.Fatal("the id follows the audio's place, not its content")
	}
	for _, s := range []Segment{{Language: "de"}, {Language: "en", NoContext: true}, {Language: "en", InitialPrompt: "Kvothe"}} {
		if taskID("sum", s) == a {
			t.Fatalf("%+v has the same id", s)
		}
	}
	if taskID("other", Segment{Language: "en"}) == a {
		t.Fatal("other audio has the same id")
	}
}

// No worker connected: Run says so at once (the Router falls back).
func TestRunNoWorker(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	if _, err := h.Run(context.Background(), Segment{Audio: flac(t, "x")}); !errors.Is(err, ErrNoWorker) {
		t.Fatalf("run = %v", err)
	}
	if h.Connected() {
		t.Fatal("connected with none")
	}
}

// A worker that stops answering: its lease runs out, the segment goes back in
// the queue, and with no worker connected Run reports ErrNoWorker. A worker
// coming back while the lease holds carries on (a restart).
func TestLeaseExpiry(t *testing.T) {
	t.Parallel()
	h, _, c := newHub(t)
	ctx := context.Background()
	id, _ := pair(t, h, "studio")
	_, _ = h.Hello(ctx, id, workerproto.Hello{Protocol: 1})
	audio := flac(t, "x")
	errc := make(chan error, 1)
	go func() { _, err := h.Run(ctx, Segment{Audio: audio}); errc <- err }()
	task, err := h.Claim(ctx, id, 5*time.Second)
	if err != nil || task == nil {
		t.Fatalf("claim = %v", err)
	}
	// Within the lease: the same worker (restarted) beats and carries on.
	c.add(30 * time.Second)
	if a, err := h.Heartbeat(id, task.ID, task.Attempt); err != nil || a != task.Attempt {
		t.Fatalf("heartbeat in the lease = %d, %v", a, err)
	}
	// Past the lease and the connected window: back in the queue, then ErrNoWorker.
	c.add(workerproto.LeaseDuration + ConnectedWindow)
	select {
	case err := <-errc:
		if !errors.Is(err, ErrNoWorker) {
			t.Fatalf("run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run didn't give up")
	}
	// The task is gone now: the old holder's beat isn't held.
	if _, err := h.Heartbeat(id, task.ID, task.Attempt); !errors.Is(err, workerproto.ErrNotHeld) {
		t.Fatalf("heartbeat after it ended = %v", err)
	}
}

// A queued task (a lease that ran out, or one the Bridge asked for again after
// a restart) is given back to the worker that still beats it, with a new
// attempt; another worker holding it can't be beaten away.
func TestHeartbeatAdopts(t *testing.T) {
	t.Parallel()
	h, _, c := newHub(t)
	ctx := context.Background()
	a, _ := pair(t, h, "a")
	b, _ := pair(t, h, "b")
	_, _ = h.Hello(ctx, a, workerproto.Hello{})
	_, _ = h.Hello(ctx, b, workerproto.Hello{})
	audio := flac(t, "y")
	go func() { _, _ = h.Run(ctx, Segment{Audio: audio}) }()
	task, _ := h.Claim(ctx, a, 5*time.Second)
	if task == nil {
		t.Fatal("no task")
	}
	if _, err := h.Heartbeat(b, task.ID, task.Attempt); !errors.Is(err, workerproto.ErrNotHeld) {
		t.Fatalf("another worker's beat = %v", err)
	}
	// a's lease runs out (b keeps polling, so a worker stays connected).
	c.add(workerproto.LeaseDuration + time.Second)
	h.mu.Lock()
	h.expireLocked(c.now())
	h.touchLocked(b, c.now())
	h.mu.Unlock()
	got, err := h.Heartbeat(a, task.ID, task.Attempt)
	if err != nil || got != task.Attempt+1 {
		t.Fatalf("adopt = %d, %v", got, err)
	}
	if next, _ := h.Claim(ctx, b, 0); next != nil {
		t.Fatalf("an adopted task was claimed again: %+v", next)
	}
	if p, err := h.Audio(a, task.ID, got); err != nil || p != audio {
		t.Fatalf("audio under the new attempt = %q, %v", p, err)
	}
}

// A result is taken whoever holds the task (the id names the same audio): a
// worker whose lease ran out and went to another still counts.
func TestResultFromAnEarlierHolder(t *testing.T) {
	t.Parallel()
	h, _, c := newHub(t)
	ctx := context.Background()
	a, _ := pair(t, h, "a")
	b, _ := pair(t, h, "b")
	_, _ = h.Hello(ctx, a, workerproto.Hello{})
	audio := flac(t, "z")
	done := make(chan error, 1)
	go func() { _, err := h.Run(ctx, Segment{Audio: audio}); done <- err }()
	task, _ := h.Claim(ctx, a, 5*time.Second)
	c.add(workerproto.LeaseDuration + time.Second)
	again, _ := h.Claim(ctx, b, 5*time.Second)
	if again == nil || again.ID != task.ID || again.Attempt != 2 {
		t.Fatalf("the reclaim = %+v", again)
	}
	if err := h.Result(ctx, a, task.ID, workerproto.ResultMeta{Attempt: 1}, []byte(okRaw)); err != nil {
		t.Fatalf("a's late result = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := h.Heartbeat(b, task.ID, 2); !errors.Is(err, workerproto.ErrNotHeld) {
		t.Fatalf("b's beat after a's result = %v", err)
	}
}

// Fail fails the segment; a stale attempt's failure isn't held.
func TestFail(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	ctx := context.Background()
	id, _ := pair(t, h, "studio")
	_, _ = h.Hello(ctx, id, workerproto.Hello{})
	done := make(chan error, 1)
	go func() { _, err := h.Run(ctx, Segment{Audio: flac(t, "f")}); done <- err }()
	task, _ := h.Claim(ctx, id, 5*time.Second)
	if err := h.Fail(id, task.ID, workerproto.Fail{Attempt: 2, Error: "old"}); !errors.Is(err, workerproto.ErrNotHeld) {
		t.Fatalf("another attempt's failure = %v", err)
	}
	if err := h.Fail(id, task.ID, workerproto.Fail{Attempt: 1, Error: "decoder crashed"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || err.Error() != `the ASR worker "studio" failed: decoder crashed` {
		t.Fatalf("run = %v", err)
	}
}

// The waiter going away (a cancel, a pause) takes the segment out; its holder
// hears that it isn't held.
func TestRunCancelled(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	id, _ := pair(t, h, "studio")
	_, _ = h.Hello(context.Background(), id, workerproto.Hello{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := h.Run(ctx, Segment{Audio: flac(t, "c")}); done <- err }()
	task, _ := h.Claim(context.Background(), id, 5*time.Second)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v", err)
	}
	if _, err := h.Heartbeat(id, task.ID, 1); !errors.Is(err, workerproto.ErrNotHeld) {
		t.Fatalf("heartbeat after the cancel = %v", err)
	}
}

// Revoking a worker gives its segment back and stops its long poll.
func TestRevokeRequeues(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	ctx := context.Background()
	a, _ := pair(t, h, "a")
	b, _ := pair(t, h, "b")
	_, _ = h.Hello(ctx, a, workerproto.Hello{})
	_, _ = h.Hello(ctx, b, workerproto.Hello{})
	go func() { _, _ = h.Run(ctx, Segment{Audio: flac(t, "r")}) }()
	task, _ := h.Claim(ctx, a, 5*time.Second)
	if err := h.RevokeWorker(ctx, a); err != nil {
		t.Fatal(err)
	}
	again, _ := h.Claim(ctx, b, 5*time.Second)
	if again == nil || again.ID != task.ID {
		t.Fatalf("the revoked worker's segment = %+v", again)
	}
	if _, err := h.Claim(ctx, a, 0); !errors.Is(err, workerproto.ErrUnauthorized) {
		t.Fatalf("a revoked worker's claim = %v", err)
	}
	// A call authenticated before the removal can't bring the worker back:
	// not as connected, nor as the holder of its old segment.
	if _, err := h.Heartbeat(a, task.ID, task.Attempt); !errors.Is(err, workerproto.ErrUnauthorized) {
		t.Fatalf("a revoked worker's heartbeat = %v", err)
	}
	if _, err := h.Audio(a, task.ID, task.Attempt); !errors.Is(err, workerproto.ErrUnauthorized) {
		t.Fatalf("a revoked worker's audio = %v", err)
	}
	if err := h.Result(ctx, a, task.ID, workerproto.ResultMeta{}, []byte(okRaw)); !errors.Is(err, workerproto.ErrUnauthorized) {
		t.Fatalf("a revoked worker's result = %v", err)
	}
	if _, err := h.Hello(ctx, a, workerproto.Hello{}); !errors.Is(err, workerproto.ErrUnauthorized) {
		t.Fatalf("a revoked worker's hello = %v", err)
	}
	h.mu.Lock()
	_, back := h.live[a]
	h.mu.Unlock()
	if back {
		t.Fatal("a revoked worker is in the registry again")
	}
}

// WhenConnected closes when a worker connects with none connected, and again
// only after a time with none (the edge is read from the state, not kept).
func TestWhenConnected(t *testing.T) {
	t.Parallel()
	h, _, c := newHub(t)
	id, _ := pair(t, h, "studio")
	closed := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	first := h.WhenConnected()
	_, _ = h.Hello(context.Background(), id, workerproto.Hello{})
	if !closed(first) {
		t.Fatal("WhenConnected wasn't closed by the first connection")
	}
	second := h.WhenConnected()
	_, _ = h.Claim(context.Background(), id, 0)
	if closed(second) {
		t.Fatal("a call from a connected worker closed it again")
	}
	c.add(ConnectedWindow + time.Second)
	_, _ = h.Claim(context.Background(), id, 0) // back after a time with none, no Connected() call between
	if !closed(second) {
		t.Fatal("a worker coming back wasn't announced")
	}
}

func TestWorkersView(t *testing.T) {
	t.Parallel()
	h, _, _ := newHub(t)
	ctx := context.Background()
	id, _ := pair(t, h, "studio")
	_, _ = pair(t, h, "never")
	_, _ = h.Hello(ctx, id, workerproto.Hello{Backend: "whisper-cpp", Device: "cuda", Version: "0.2.0"})
	vs, err := h.Workers(ctx)
	if err != nil || len(vs) != 2 {
		t.Fatalf("workers = %+v, %v", vs, err)
	}
	if !vs[0].Connected || vs[0].Device != "cuda" || vs[0].LastSeenAt == "" || vs[1].Connected || vs[1].LastSeenAt != "" {
		t.Fatalf("views = %+v", vs)
	}
}

func TestCleanText(t *testing.T) {
	t.Parallel()
	if got := cleanText(" a\x00b\nc ", 10); got != "a b c" {
		t.Fatalf("clean = %q", got)
	}
	if got := cleanText("ééé", 5); got != "éé" {
		t.Fatalf("cut on a rune = %q", got)
	}
	if got := cleanText("a\xffb", 10); got != "ab" {
		t.Fatalf("invalid utf-8 = %q", got)
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
