package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/asr"
	"github.com/kodestar/audiosilo-bridge/internal/auth"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/workerproto"
	"github.com/kodestar/audiosilo-bridge/internal/workers"
)

const okRaw = `{"text":" hi","segments":[{"id":0,"start":0,"end":1,"text":" hi"}]}`

// fakeASR stands in for mlx-whisper: it writes okRaw, or blocks until told
// (gate) or its context ends.
type fakeASR struct {
	mu      sync.Mutex
	gate    chan struct{}
	started chan string
	runs    atomic.Int32
}

func (f *fakeASR) ID() string { return "mlx-whisper" }
func (f *fakeASR) Detect(context.Context) (asr.Capability, error) {
	return asr.Capability{Backend: "mlx-whisper", Available: true, Device: "metal"}, nil
}
func (f *fakeASR) EnsureReady(context.Context) error { return nil }
func (f *fakeASR) Transcribe(ctx context.Context, job asr.Job) error {
	f.runs.Add(1)
	f.mu.Lock()
	gate, started := f.gate, f.started
	f.mu.Unlock()
	if started != nil {
		started <- filepath.Base(job.Audio)
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return os.WriteFile(filepath.Join(job.OutDir, asr.RawOutputName(job.Audio)), []byte(okRaw), 0o600)
}

// stack is the Bridge's worker routes over a real hub, served on loopback.
type stack struct {
	hub   *workers.Hub
	srv   *httptest.Server
	token string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	return newStackWith(t, func(h http.Handler) http.Handler { return h })
}

// newStackWith is newStack with wrap around the Bridge's handler (a slow link).
func newStackWith(t *testing.T, wrap func(http.Handler) http.Handler) *stack {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := workers.New(workers.Options{DB: db, BridgeVersion: "test"})
	_, token, err := hub.CreateWorker(context.Background(), "studio")
	if err != nil {
		t.Fatal(err)
	}
	h := api.New(nil, auth.New(auth.NewMemStore()), hub, http.NotFoundHandler(), discard()).Handler()
	srv := httptest.NewServer(wrap(h))
	t.Cleanup(srv.Close)
	return &stack{hub: hub, srv: srv, token: token}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (s *stack) start(t *testing.T, ctx context.Context, data string, b asr.Backend) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{BridgeURL: s.srv.URL, Token: s.token, DataDir: data, Version: "test", Backend: b, Log: discard(),
			HeartbeatEvery: 50 * time.Millisecond})
	}()
	return done
}

// segment queues a FLAC with the hub (as the Router does) and returns where the
// raw output comes back.
func segment(t *testing.T, hub *workers.Hub, content string) (chan error, chan *workers.Outcome) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ch001.flac")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	errc, outc := make(chan error, 1), make(chan *workers.Outcome, 1)
	go func() {
		// Wait for the worker to connect, as the Router would.
		for !hub.Connected() {
			time.Sleep(10 * time.Millisecond)
		}
		out, err := hub.Run(context.Background(), workers.Segment{Audio: p, Duration: 60, Language: "en"})
		errc <- err
		outc <- out
	}()
	return errc, outc
}

func TestWorkerRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := t.TempDir()
	done := s.start(t, ctx, data, &fakeASR{})
	for i := range 2 {
		errc, outc := segment(t, s.hub, "fLaC audio "+string(rune('a'+i)))
		select {
		case err := <-errc:
			if out := <-outc; err != nil || string(out.Raw) != okRaw || out.Backend != "mlx-whisper" || out.Device != "metal" {
				t.Fatalf("segment %d = %+v, %v", i, out, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the worker never answered")
		}
	}
	vs, _ := s.hub.Workers(ctx)
	if len(vs) != 1 || !vs[0].Connected || vs[0].Backend != "mlx-whisper" || vs[0].Segments != 2 {
		t.Fatalf("worker = %+v", vs)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run = %v", err)
	}
	// Nothing is left in the spool.
	left, _ := filepath.Glob(filepath.Join(data, "spool", "*"))
	for _, p := range left {
		if fi, _ := os.Stat(p); !fi.IsDir() {
			t.Errorf("left in the spool: %s", p)
		}
	}
}

// Killed mid-decode (a stop here), the worker comes back and carries on with
// the segment in hand at once: the Bridge gives it back while its lease holds.
func TestWorkerResumesTheSegmentInHand(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	data := t.TempDir()
	first := &fakeASR{gate: make(chan struct{}), started: make(chan string, 1)}
	ctx1, stop1 := context.WithCancel(context.Background())
	done1 := s.start(t, ctx1, data, first)
	errc, outc := segment(t, s.hub, "a long chapter")
	<-first.started
	stop1() // the worker stops mid-decode
	<-done1
	cur := filepath.Join(data, "spool", "current.json")
	if _, err := os.Stat(cur); err != nil {
		t.Fatalf("the segment in hand wasn't kept: %v", err)
	}
	second := &fakeASR{started: make(chan string, 1)}
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	_ = s.start(t, ctx2, data, second)
	select {
	case err := <-errc:
		if out := <-outc; err != nil || string(out.Raw) != okRaw {
			t.Fatalf("resumed = %+v, %v", out, err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the resumed segment never finished")
	}
	if name := <-second.started; !strings.HasSuffix(name, ".flac") {
		t.Fatalf("the second run decoded %q", name)
	}
	if _, err := os.Stat(cur); !os.IsNotExist(err) {
		t.Fatal("the segment in hand is still recorded after its result")
	}
}

// A transcript finished while the Bridge couldn't take it is kept, and sent
// when the worker starts again.
func TestWorkerSendsKeptResults(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	data := t.TempDir()
	errc, outc := segment(t, s.hub, "kept")
	// Claim it as this worker, then keep a result in the spool as if the
	// upload had failed.
	id, err := s.hub.Authenticate(context.Background(), s.token)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.hub.Hello(context.Background(), id, workerproto.Hello{})
	task, _ := s.hub.Claim(context.Background(), id, 5*time.Second)
	if task == nil {
		t.Fatal("no task")
	}
	sp, _ := openSpool(filepath.Join(data, "spool"))
	raw := filepath.Join(sp.rawDir(), "x.json")
	if err := os.WriteFile(raw, []byte(okRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sp.keepResult(task.ID, workerproto.ResultMeta{Attempt: task.Attempt, Backend: "mlx-whisper"}, raw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &fakeASR{}
	_ = s.start(t, ctx, data, b)
	select {
	case err := <-errc:
		if out := <-outc; err != nil || string(out.Raw) != okRaw {
			t.Fatalf("kept result = %+v, %v", out, err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the kept result was never sent")
	}
	if b.runs.Load() != 0 {
		t.Fatal("the segment was decoded again")
	}
	for deadline := time.Now().Add(5 * time.Second); len(sp.results()) != 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a sent result is still kept")
		}
	}
}

// The lease is renewed while the FLAC downloads, not only while it decodes: a
// long chapter's FLAC on a slow link takes longer than the lease, which would
// otherwise run out and hand the segment to another worker or the Bridge.
func TestWorkerKeepsTheLeaseWhileDownloading(t *testing.T) {
	t.Parallel()
	var beats atomic.Int32
	during := make(chan int32, 1)
	s := newStackWith(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/heartbeat"):
				beats.Add(1)
			case strings.HasSuffix(r.URL.Path, "/audio"):
				// A slow link: the download lasts a few heartbeats (or gives up).
				from := beats.Load()
				for deadline := time.Now().Add(5 * time.Second); beats.Load() < from+3 && time.Now().Before(deadline); {
					time.Sleep(10 * time.Millisecond)
				}
				select {
				case during <- beats.Load() - from:
				default:
				}
			}
			h.ServeHTTP(w, r)
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = s.start(t, ctx, t.TempDir(), &fakeASR{})
	errc, outc := segment(t, s.hub, "a slow FLAC")
	select {
	case err := <-errc:
		if out := <-outc; err != nil || string(out.Raw) != okRaw {
			t.Fatalf("segment = %+v, %v", out, err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the worker never answered")
	}
	if n := <-during; n < 3 {
		t.Fatalf("%d heartbeats while the FLAC downloaded, want the lease kept", n)
	}
}

// The Bridge taking a segment back (its waiter went away) stops the decode.
func TestWorkerStopsWhenNotHeld(t *testing.T) {
	t.Parallel()
	s := newStack(t)
	data := t.TempDir()
	b := &fakeASR{gate: make(chan struct{}), started: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = s.start(t, ctx, data, b)
	p := filepath.Join(t.TempDir(), "ch002.flac")
	_ = os.WriteFile(p, []byte("taken back"), 0o600)
	rctx, rcancel := context.WithCancel(context.Background())
	go func() {
		for !s.hub.Connected() {
			time.Sleep(10 * time.Millisecond)
		}
		_, _ = s.hub.Run(rctx, workers.Segment{Audio: p})
	}()
	<-b.started
	rcancel()
	// The next heartbeat hears not_held and stops.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(data, "spool", "current.json")); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the worker kept the segment the Bridge took back")
}

// A token the Bridge refuses, or another protocol version: the worker says so
// and waits (it never exits, so a service manager doesn't loop on it).
func TestWorkerRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"token", http.StatusUnauthorized, `{"error":"unknown worker token"}`, workerproto.ErrUnauthorized},
		{"protocol", http.StatusConflict, `{"error":"x","code":"protocol_mismatch","protocol":2}`, ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c, err := newClient(srv.URL, "asw_x", "test", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.hello(context.Background(), workerproto.Hello{}); !errors.Is(err, tc.want) {
				t.Fatalf("hello = %v, want %v", err, tc.want)
			}
			if tc.want == ErrProtocol && c.bridgeV != 2 {
				t.Fatalf("the Bridge's version = %d", c.bridgeV)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err = Run(ctx, Options{BridgeURL: srv.URL, Token: "asw_x", DataDir: t.TempDir(), Backend: &fakeASR{}, Log: discard()})
			if err != nil || calls.Load() != 2 {
				t.Fatalf("run = %v after %d calls (it should wait, not retry at once)", err, calls.Load())
			}
		})
	}
}

// A Bridge that answers hello with another version is refused by the worker
// too (a Bridge older than the version check).
func TestWorkerRefusesAnotherBridgeVersion(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(workerproto.HelloReply{Protocol: workerproto.Version + 1})
	}))
	defer srv.Close()
	c, _ := newClient(srv.URL, "asw_x", "test", nil)
	if _, err := c.hello(context.Background(), workerproto.Hello{}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("hello = %v", err)
	}
}

// A damaged download is refused (size or sha256), and nothing is kept.
func TestDownloadChecksTheAudio(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not the audio")
	}))
	defer srv.Close()
	c, _ := newClient(srv.URL, "asw_x", "test", nil)
	dest := filepath.Join(t.TempDir(), "t.flac")
	digest := sha256.Sum256([]byte("the audio"))
	sum := hex.EncodeToString(digest[:])
	err := c.download(context.Background(), workerproto.Task{ID: "t", Size: 9, SHA256: sum}, dest)
	if err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("download = %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a damaged download was kept")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal("a partial download was kept")
	}
}

func TestNewClientNeedsAnHTTPAddress(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "192.168.1.2:8091", "ftp://x", "http://"} {
		if _, err := newClient(bad, "t", "v", nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := newClient("http://192.168.1.2:8091/", "t", "v", nil); err != nil {
		t.Fatal(err)
	}
}

// A Bridge behind a proxy's prefix: every route goes under the prefix as
// written, the RFC 3986 path characters public_url may hold included (a "("
// or "!" stays one, never %28 or %21, so the proxy's prefix match sees it).
func TestClientRoutesKeepThePrefixAsWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ base, want string }{
		{"http://nas.lan:8091", "/worker/v1/tasks/a%2Fb/audio"},
		{"http://proxy.lan/my+bridge(1)/", "/my+bridge(1)/worker/v1/tasks/a%2Fb/audio"},
		{"https://proxy.lan/a!$&()*+,;=:@b/c%20d", "/a!$&()*+,;=:@b/c%20d/worker/v1/tasks/a%2Fb/audio"},
	} {
		c, err := newClient(tc.base, "t", "v", nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := url.Parse(c.route(workerproto.RouteAudio, "a/b", nil))
		if err != nil || got.EscapedPath() != tc.want {
			t.Errorf("%s: route = %v, %v, want %s", tc.base, got, err, tc.want)
		}
	}
	// What the request line carries.
	c, err := newClient("http://proxy.lan/my+bridge(1)/", "t", "v", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, c.route(workerproto.RouteNext, "", url.Values{"wait": {"1"}}), nil)
	if uri := req.URL.RequestURI(); uri != "/my+bridge(1)/worker/v1/tasks/next?wait=1" {
		t.Errorf("request URI = %s", uri)
	}
}

func TestRunNeedsAToken(t *testing.T) {
	t.Parallel()
	if err := Run(context.Background(), Options{BridgeURL: "http://x", DataDir: t.TempDir()}); err == nil {
		t.Fatal("no token accepted")
	}
}

// A speech recognition that isn't available keeps the worker waiting, never
// exiting (a service manager would loop on it).
func TestRunWaitsForTheSpeechRecognition(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := Run(ctx, Options{BridgeURL: "http://127.0.0.1:1", Token: "asw_x", DataDir: t.TempDir(), Backend: &unavailableASR{}, Log: discard()})
	if err != nil || ctx.Err() == nil {
		t.Fatalf("run = %v before its context ended", err)
	}
}

type unavailableASR struct{ fakeASR }

func (*unavailableASR) Detect(context.Context) (asr.Capability, error) {
	return asr.Capability{Backend: "mlx-whisper", Detail: "python3 not found"}, nil
}
