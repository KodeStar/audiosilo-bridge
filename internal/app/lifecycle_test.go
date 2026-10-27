package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

// jobServer is a fuller fake server: every pairing mints a new token and
// revokes the old one (as ExchangeBridgeCode does), heartbeats can be slow or
// failing, book lookups slow, and one queued bridge.verify job is handed out.
type jobServer struct {
	root      string
	beatDelay time.Duration
	beatFails atomic.Int32 // the next n heartbeats answer 503
	bookDelay time.Duration
	hold      int // the queued job's hold_seconds; -1: no job

	mu      sync.Mutex
	tokens  int
	live    string
	handed  bool
	beats   int
	updates []link.Update
}

func (f *jobServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/bridge/pair" {
		f.mu.Lock()
		f.tokens++
		f.live = "tok-" + string(rune('0'+f.tokens))
		tok := f.live
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": tok, "server_id": "s", "server_name": "Home"})
		return
	}
	f.mu.Lock()
	live := f.live
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+live {
		http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/bridge/status":
		time.Sleep(f.beatDelay)
		f.mu.Lock()
		f.beats++
		f.mu.Unlock()
		if f.beatFails.Load() > 0 {
			f.beatFails.Add(-1)
			http.Error(w, `{"error":"starting"}`, http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(link.StatusReply{ServerTime: "now", Libraries: []link.Library{{ID: 1, Name: "Books", Root: f.root}}})
	case r.URL.Path == "/api/v1/bridge/libraries/1/book":
		time.Sleep(f.bookDelay)
		_ = json.NewEncoder(w).Encode(link.Book{LibraryID: 1, Path: "A", Files: []link.BookFile{{Path: "A/1.mp3", Size: 3}}})
	case r.URL.Path == "/api/v1/bridge/jobs/next":
		f.mu.Lock()
		handed := f.handed || f.hold < 0
		f.handed = true
		f.mu.Unlock()
		if handed {
			time.Sleep(20 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		lib := int64(1)
		params, _ := json.Marshal(map[string]int{"hold_seconds": f.hold})
		_ = json.NewEncoder(w).Encode(link.Job{ID: 9, Kind: "bridge.verify", LibraryID: &lib, Params: params, State: "claimed", Attempts: 1})
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/bridge/jobs/"):
		var u link.Update
		_ = json.NewDecoder(r.Body).Decode(&u)
		f.mu.Lock()
		f.updates = append(f.updates, u)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(link.Job{ID: 9, State: "running", Attempts: 1})
	default:
		http.NotFound(w, r)
	}
}

func (f *jobServer) final() (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.updates) - 1; i >= 0; i-- {
		if s := f.updates[i].State; s != nil && *s != link.StateRunning {
			e := ""
			if f.updates[i].Error != nil {
				e = *f.updates[i].Error
			}
			return *s, e
		}
	}
	return "", ""
}

func (f *jobServer) beatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beats
}

func bookRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "A"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "A", "1.mp3"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// start runs a Bridge in data until the test ends.
func start(t *testing.T, data string) *Bridge {
	t.Helper()
	b, _, err := New(context.Background(), Options{DataDir: data, Version: "0.1.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return b
}

// A job claimed or resumed right after a start waits for the first heartbeat
// on its link, so the server's libraries are known: a first heartbeat that
// fails (the server still starting) doesn't fail the job.
func TestJobsWaitForTheFirstHeartbeat(t *testing.T) {
	fs := &jobServer{root: bookRoot(t), beatDelay: 50 * time.Millisecond}
	fs.beatFails.Store(1)
	srv := httptest.NewServer(fs)
	defer srv.Close()
	data := t.TempDir()
	// Paired before (the server keeps its old heartbeat, so it hands out a job
	// at once): a pairing made by a first process, then a restart.
	b0, _, err := New(context.Background(), Options{DataDir: data, Version: "0.1.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if err := b0.conn.Pair(context.Background(), srv.URL, "code"); err != nil {
		t.Fatal(err)
	}
	_ = b0.db.Close()
	b := start(t, data)
	b.Kick() // the next heartbeat at once rather than in 30 s
	waitFor(t, "the job's end", func() bool { s, _ := fs.final(); return s != "" })
	if s, e := fs.final(); s != link.StateDone {
		t.Fatalf("the job ended %s (%q), want done", s, e)
	}
}

// A heartbeat while a mapping check runs (a settings save, the 30 s tick of a
// long check) asks for no second check and beats once: it never spins.
func TestNoHeartbeatStormDuringACheck(t *testing.T) {
	fs := &jobServer{root: bookRoot(t), bookDelay: 700 * time.Millisecond, hold: -1}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	b := start(t, t.TempDir())
	if err := b.Pair(context.Background(), srv.URL, "code"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the first heartbeat started the slow check
	before := fs.beatCount()
	b.Kick()
	time.Sleep(300 * time.Millisecond)
	if n := fs.beatCount() - before; n > 2 {
		t.Fatalf("%d heartbeats in 0.3 s while a check runs", n)
	}
	waitFor(t, "the check's results", func() bool {
		for _, e := range b.libs.Entries(b.mount) {
			if e.Status != nil && e.Status.Mapped {
				return true
			}
		}
		return false
	})
}

// Security: a 401 on a link a re-pair replaced (a job of the previous pairing
// still reporting with its revoked token) never refuses the new pairing
// (denied); a 401 on the current link does (allowed). A retry that answers
// after an unpair doesn't mark the Bridge paired again.
func TestOnlyTheCurrentLinkIsRefused(t *testing.T) {
	fs := &jobServer{root: bookRoot(t), hold: -1}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	b := start(t, t.TempDir())
	if err := b.Pair(context.Background(), srv.URL, "code-1"); err != nil {
		t.Fatal(err)
	}
	old := b.conn.Client()
	if err := b.Unpair(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Pair(context.Background(), srv.URL, "code-2"); err != nil {
		t.Fatal(err)
	}
	b.tokenRefused(old)
	if p := b.conn.Pairing(); !p.Paired || b.conn.Client() == nil {
		t.Fatalf("a 401 on the replaced link refused the new pairing: %+v", p)
	}
	current := b.conn.Client()
	b.tokenRefused(current)
	if p := b.conn.Pairing(); p.Paired || b.conn.Client() != nil {
		t.Fatalf("a 401 on the current link didn't refuse it: %+v", p)
	}
	// The refusal's retry answers after an unpair: nothing is accepted.
	if err := b.Unpair(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.conn.accept(context.Background(), current)
	if p := b.conn.Pairing(); p.Paired {
		t.Fatalf("a late retry marked an unpaired Bridge paired: %+v", p)
	}
}

// Saving one setting keeps a locked one out of config.yaml: the page sends it
// back unchanged, and the environment's value must never reach the file.
func TestSaveSettingsKeepsLockedValuesOut(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "config.yaml"), []byte("public_url: http://file.lan:8091\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOSILO_BRIDGE_PUBLIC_URL", "http://env.lan:8091")
	b, _, err := New(context.Background(), Options{DataDir: data, Version: "0.1.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.db.Close() }()
	if err := b.SaveSettings(context.Background(), api.Settings{PublicURL: b.Settings().PublicURL, MappingCheckMinutes: 15}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(data, "config.yaml"))
	if strings.Contains(string(raw), "env.lan") || !strings.Contains(string(raw), "file.lan") || !strings.Contains(string(raw), "check_minutes: 15") {
		t.Fatalf("config.yaml = %s", raw)
	}
	if s := b.Settings(); s.PublicURL != "http://env.lan:8091" || s.MappingCheckMinutes != 15 {
		t.Fatalf("settings = %+v", s)
	}
}

// Pairing errors say what went wrong: a bad address is the user's (400); a
// server without the Bridge routes (404, or 405 from an older server's
// catch-all) and a certificate the Bridge can't trust are refusals that say
// what to do, not "could not reach the server".
func TestPairErrors(t *testing.T) {
	b, _, err := New(context.Background(), Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.db.Close() }()
	if err := b.Pair(context.Background(), "audiosilo:8080", "code"); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("a bad address = %v, want ErrInvalid", err)
	}
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		err := b.Pair(context.Background(), old.URL, "code")
		old.Close()
		if !errors.Is(err, api.ErrRefused) || !strings.Contains(err.Error(), "doesn't know about a Bridge") {
			t.Errorf("an older server's %d = %v", status, err)
		}
	}
	tlsSrv := httptest.NewTLSServer(http.NotFoundHandler()) // a certificate no system root signed
	defer tlsSrv.Close()
	err = b.Pair(context.Background(), tlsSrv.URL, "code")
	if !errors.Is(err, api.ErrRefused) || !strings.Contains(err.Error(), "http://127.0.0.1:") {
		t.Errorf("a self-signed certificate = %v, want a refusal naming the http:// address", err)
	}
}

// A server the Bridge never reached gets a sentence that names the address and
// says what to use instead, not Go's "dial tcp: lookup ..." chain.
func TestPairUnreachableIsWorded(t *testing.T) {
	b, _, err := New(context.Background(), Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.db.Close() }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	_ = ln.Close()
	err = b.Pair(context.Background(), closed+"/", "code")
	if !errors.Is(err, api.ErrUnreachable) {
		t.Fatalf("a closed port = %v, want ErrUnreachable", err)
	}
	want := "Couldn't reach " + closed + " (nothing answered on that port). Use the server's address"
	if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "dial tcp") {
		t.Errorf("a closed port = %q, want %q", err, want)
	}
}

// pairError words each failure by where it happened. Only a request that got
// no answer says "Couldn't reach"; a 5xx says the server answered with an
// error; an answer that can't be used says so; a failure after the server
// accepted the code says the pairing wasn't saved here and a new code is needed.
func TestPairErrorWording(t *testing.T) {
	const addr = "http://audiosilo:8080"
	wrap := func(e error) error { return &url.Error{Op: "Post", URL: addr + "/api/v1/bridge/pair", Err: e} }
	cases := []struct {
		name string
		err  error
		kind error
		want string
	}{
		{"no such host", wrap(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "audiosilo", IsNotFound: true}}), api.ErrUnreachable,
			"Couldn't reach http://audiosilo:8080 (no such host). Use the server's address as this computer reaches it, like http://192.168.1.10:8080."},
		{"lookup failed", wrap(&net.DNSError{Err: "server misbehaving", Name: "audiosilo"}), api.ErrUnreachable, "(its name couldn't be looked up)"},
		{"refused", wrap(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), api.ErrUnreachable, "(nothing answered on that port)"},
		{"no route to host", wrap(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}), api.ErrUnreachable, "(no route to that address)"},
		{"network unreachable", wrap(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}), api.ErrUnreachable, "(no route to that address)"},
		{"dns timeout", wrap(&net.DNSError{Err: "i/o timeout", IsTimeout: true}), api.ErrUnreachable, "(its name couldn't be looked up)"},
		{"dial timeout", wrap(&net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}), api.ErrUnreachable, "(it didn't answer in time)"},
		{"other transport", wrap(errors.New("EOF")), api.ErrUnreachable, "Couldn't reach http://audiosilo:8080: EOF."},
		{"answer timed out", fmt.Errorf("read: %w", context.DeadlineExceeded), api.ErrUnreachable, "(it didn't answer in time)"},
		{"5xx", &link.APIError{Status: 500, Message: "database is locked"}, api.ErrUnreachable,
			"The server at http://audiosilo:8080 answered with an error (500: database is locked). Try again"},
		{"5xx no message", &link.APIError{Status: 503}, api.ErrUnreachable, "answered with an error (503). Try again"},
		{"4xx", &link.APIError{Status: 409, Message: "another Bridge is pairing"}, api.ErrRefused, "the server refused the pairing: another Bridge is pairing"},
		{"bad answer", errors.New("decode the server's answer to POST /api/v1/bridge/pair: unexpected end of JSON input"), api.ErrUnreachable,
			"The server at http://audiosilo:8080 gave an answer this Bridge couldn't use (decode"},
		{"not saved", notSaved(errors.New("disk full")), api.ErrFailed,
			"The server accepted the pairing, but this Bridge couldn't save it (disk full). Fix that (see the Bridge's log), make a new code"},
		{"code", link.ErrUnauthorized, api.ErrRefused, "The code is wrong, used or expired."},
	}
	for _, c := range cases {
		err := pairError(addr+"/ ", c.err)
		if !errors.Is(err, c.kind) || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "Post ") {
			t.Errorf("%s: %v, want %v %q", c.name, err, c.kind, c.want)
		}
		if c.kind != api.ErrUnreachable && strings.Contains(err.Error(), "Couldn't reach") {
			t.Errorf("%s: %q says Couldn't reach, but the server answered", c.name, err)
		}
	}
	if pairError(addr, nil) != nil {
		t.Error("pairError(nil) != nil")
	}
}

// End to end: a server answering 500, one answering 200 with no usable body,
// and a pairing the server accepted but this Bridge couldn't save (its
// database closed) are each worded for where they failed.
func TestPairAnsweredOrNotSaved(t *testing.T) {
	b, _, err := New(context.Background(), Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.db.Close() }()
	answer := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
	}
	failing := answer(http.StatusInternalServerError, `{"error":"boom"}`)
	defer failing.Close()
	if err := b.Pair(context.Background(), failing.URL, "code"); !errors.Is(err, api.ErrUnreachable) ||
		!strings.Contains(err.Error(), "answered with an error (500") || strings.Contains(err.Error(), "Couldn't reach") {
		t.Errorf("a 500 = %v", err)
	}
	garbled := answer(http.StatusOK, `not json`)
	defer garbled.Close()
	if err := b.Pair(context.Background(), garbled.URL, "code"); err == nil || !strings.Contains(err.Error(), "couldn't use") {
		t.Errorf("an unusable answer = %v", err)
	}
	ok := answer(http.StatusOK, `{"token":"bt_x","server_id":"s1","server_name":"Home"}`)
	defer ok.Close()
	_ = b.db.Close() // saving the pairing fails after the server accepted the code
	err = b.Pair(context.Background(), ok.URL, "code")
	if !errors.Is(err, api.ErrFailed) || !strings.Contains(err.Error(), "accepted the pairing, but this Bridge couldn't save it") ||
		strings.Contains(err.Error(), "Couldn't reach") {
		t.Errorf("a pairing not saved = %v", err)
	}
	if b.conn.Client() != nil {
		t.Error("a pairing not saved left the Bridge paired")
	}
	if tok, _ := b.conn.vault.Get(secrets.ServerToken); tok != "" {
		t.Error("a pairing not saved kept its token")
	}
}

// timeoutErr is a net.Error that timed out.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// failingListener accepts nothing: its Accept fails for good at once.
type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) {
	return nil, errors.New("accept: permanent failure")
}

// When the page's server fails, Run stops the heartbeat and the runner and
// returns the error, instead of waiting for them for ever.
func TestRunReturnsWhenThePageFails(t *testing.T) {
	orig := listen
	listen = func(ctx context.Context, addr string) (net.Listener, error) {
		ln, err := orig(ctx, addr)
		return failingListener{ln}, err
	}
	defer func() { listen = orig }()
	b, _, err := New(context.Background(), Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "permanent failure") {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after the page's server failed")
	}
}

// Two mapping edits at once (two library cards saved together) both land:
// config.yaml's load, edit and write never interleave.
func TestConcurrentMappingEditsBothLand(t *testing.T) {
	b, _, err := New(context.Background(), Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.db.Close() }()
	const n = 8
	libs := make([]link.Library, 0, n)
	for i := range n {
		libs = append(libs, link.Library{ID: int64(i + 1), Root: "/srv"})
	}
	b.libs.SetLibraries(libs)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.SetMount(context.Background(), int64(i+1), filepath.Join("/mnt", string(rune('a'+i)))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := len(b.config().Libraries); got != n {
		t.Fatalf("%d of %d mappings saved: %+v", got, n, b.config().Libraries)
	}
}
