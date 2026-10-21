package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// fakeServer is the server side of the Bridge routes, enough for the app: a
// pairing code, heartbeats (recorded), one library with one book, no jobs.
type fakeServer struct {
	t       *testing.T
	root    string
	mu      sync.Mutex
	revoked bool
	beats   []link.Status
	clients []string
}

const goodCode = "GOOD-CODE"

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients = append(f.clients, r.Header.Get(link.ClientHeader))
	if r.URL.Path == "/api/v1/bridge/pair" {
		var req struct{ Code string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Code != goodCode {
			http.Error(w, `{"error":"invalid or expired pairing code"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"bridge-secret-token","server_id":"srv-1","server_name":"Home"}`)
		return
	}
	if f.revoked || r.Header.Get("Authorization") != "Bearer bridge-secret-token" {
		http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/bridge/status":
		var st link.Status
		_ = json.NewDecoder(r.Body).Decode(&st)
		f.beats = append(f.beats, st)
		_ = json.NewEncoder(w).Encode(link.StatusReply{ServerTime: "now", Libraries: []link.Library{{ID: 1, Name: "Books", Root: f.root}}})
	case r.URL.Path == "/api/v1/bridge/libraries/1/book":
		_ = json.NewEncoder(w).Encode(link.Book{LibraryID: 1, Path: "A/B", Files: []link.BookFile{{Path: "A/B/1.mp3", Size: 3}}})
	case r.URL.Path == "/api/v1/bridge/jobs/next":
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// lastBeat is the newest heartbeat.
func (f *fakeServer) lastBeat() (link.Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.beats) == 0 {
		return link.Status{}, false
	}
	return f.beats[len(f.beats)-1], true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPairBeatMapAndRevoke(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "A", "B"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "A", "B", "1.mp3"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{t: t, root: root}
	srv := httptest.NewServer(fs)
	defer srv.Close()

	data := t.TempDir()
	t.Setenv("AUDIOSILO_BRIDGE_PUBLIC_URL", "http://bridge.lan:8091")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, oneTime, err := New(ctx, Options{DataDir: data, Version: "0.1.0", Listen: "127.0.0.1:0", Logger: log})
	if err != nil || oneTime == "" {
		t.Fatalf("New = %v, one-time password %q", err, oneTime)
	}
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	if err := b.Pair(ctx, srv.URL, "WRONG"); !errors.Is(err, api.ErrRefused) {
		t.Fatalf("a wrong code = %v, want ErrRefused", err)
	}
	if err := b.Pair(ctx, srv.URL, goodCode); err != nil {
		t.Fatal(err)
	}
	if err := b.Pair(ctx, srv.URL, goodCode); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("pairing twice = %v, want ErrConflict", err)
	}
	// The token is sealed: neither the database nor config.yaml holds it.
	for _, name := range []string{store.FileName, "config.yaml"} {
		raw, _ := os.ReadFile(filepath.Join(data, name))
		if strings.Contains(string(raw), "bridge-secret-token") {
			t.Fatalf("%s holds the token in the clear", name)
		}
	}

	// The second heartbeat carries the mapping check of the library the first
	// one learned about.
	waitFor(t, "a mapped heartbeat", func() bool {
		st, ok := fs.lastBeat()
		return ok && len(st.Libraries) == 1 && st.Libraries[0].Mapped && st.Libraries[0].Writable
	})
	st, _ := fs.lastBeat()
	if st.Version != "0.1.0" || strings.Join(st.Kinds, ",") != "bridge.verify,chapters.detect,meta.correct,podcast.fetch,podcast.poll,podcast.retain,podcast.subscribe,sidecars.contribute,sidecars.describe,sidecars.generate" || st.UIURL != "http://bridge.lan:8091" {
		t.Fatalf("heartbeat = %+v", st)
	}
	// Once the engine runs, every heartbeat carries the estimates.
	waitFor(t, "the engine", func() bool { return b.engine.get() != nil })
	b.Kick()
	waitFor(t, "a heartbeat with estimates", func() bool {
		st, ok := fs.lastBeat()
		return ok && st.Estimates != nil && st.Estimates.Sidecars != nil
	})
	st, _ = fs.lastBeat()
	if e := st.Estimates.Sidecars; e.AgentBackend != "claude" || e.AgentReady || e.MonthlyBudgetUSD != 100 || e.Samples != 0 ||
		e.ASRSecondsPerAudioHour == 0 || e.AgentUSDPerAudioHour != nil {
		t.Fatalf("estimates = %+v", e)
	}
	if s := b.Status(); !s.Online || s.LastHeartbeat == nil {
		t.Fatalf("status = %+v", s)
	}
	fs.mu.Lock()
	for _, c := range fs.clients {
		if !strings.HasPrefix(c, "AudioSilo Bridge/0.1.0 (") {
			t.Errorf("client header = %q", c)
		}
	}
	fs.mu.Unlock()

	// Mapping the library elsewhere is saved and checked again.
	wrong := t.TempDir()
	if err := b.SetMount(ctx, 1, wrong); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "an unmapped heartbeat", func() bool {
		st, ok := fs.lastBeat()
		return ok && len(st.Libraries) == 1 && !st.Libraries[0].Mapped && st.Libraries[0].MountPath == wrong
	})
	if err := b.SetMount(ctx, 9, wrong); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown library = %v", err)
	}
	if err := b.SetMount(ctx, 1, "relative"); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("a relative folder = %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(data, "config.yaml"))
	if !strings.Contains(string(raw), wrong) || strings.Contains(string(raw), "bridge.lan") {
		t.Fatalf("config.yaml = %s", raw)
	}
	if err := b.SaveSettings(ctx, api.Settings{PublicURL: "http://other.lan", MappingCheckMinutes: 10}); !errors.Is(err, api.ErrLocked) {
		t.Fatalf("a setting the environment sets = %v, want ErrLocked", err)
	}

	// The server refuses the token: the Bridge stops (no client), says why, and
	// keeps the token for its retries.
	fs.mu.Lock()
	fs.revoked = true
	fs.mu.Unlock()
	b.Kick()
	waitFor(t, "the refusal", func() bool { return !b.conn.Pairing().Paired })
	if reason := b.conn.Pairing().UnpairedReason; !strings.Contains(reason, "refused") {
		t.Fatalf("reason = %q", reason)
	}
	if b.conn.Client() != nil || b.conn.retryClient() != nil {
		t.Fatal("a refused token is used before its retry is due")
	}
	// Once the server takes it again (the admin is an admin again), a due retry
	// ends the refusal.
	fs.mu.Lock()
	fs.revoked = false
	fs.mu.Unlock()
	b.conn.mu.Lock()
	b.conn.refusedAt = time.Now().Add(-RefusedRetry)
	b.conn.mu.Unlock()
	b.Kick()
	waitFor(t, "the token to work again", func() bool { return b.conn.Pairing().Paired && b.conn.Client() != nil })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}

	// A restart keeps the pairing (and the token).
	b2, oneTime, err := New(context.Background(), Options{DataDir: data, Version: "0.1.0", Listen: "127.0.0.1:0", Logger: log})
	if err != nil || oneTime != "" {
		t.Fatalf("restart = %v, %q", err, oneTime)
	}
	if p := b2.conn.Pairing(); !p.Paired || p.UnpairedReason != "" || p.ServerName != "Home" {
		t.Fatalf("after a restart = %+v", p)
	}
	if err := b2.Unpair(context.Background()); err != nil || b2.conn.Client() != nil {
		t.Fatalf("unpair from the page = %v", err)
	}
	_ = b2.db.Close()
}

// A start whose listener fails doesn't use up the pairing code.
func TestPairCodeNotSpentWhenTheListenerFails(t *testing.T) {
	fs := &fakeServer{t: t, root: t.TempDir()}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	t.Setenv("AUDIOSILO_BRIDGE_SERVER_URL", srv.URL)
	t.Setenv("AUDIOSILO_BRIDGE_PAIR_CODE", goodCode)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, _, err := New(context.Background(), Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "not-an-address", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Run(context.Background()); err == nil {
		t.Fatal("Run with a bad listen address succeeded")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.clients) != 0 {
		t.Fatal("the code was exchanged before the listener failed")
	}
}

// The page's engine view and job routes before the engine has started: the
// configured budgets and retention show, a Retry of an unknown job is a 404 and
// an unknown job's detail too.
func TestEngineViewAndJobRoutes(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "config.yaml"),
		[]byte("agent:\n  monthly_budget_usd: 40\n  backend: codex\ntranscripts:\n  retention_days: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _, err := New(context.Background(), Options{DataDir: data, Version: "0.1.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.db.Close() })
	e := b.Engine(context.Background())
	if e.Started || e.Agent.Backend != "codex" || e.MonthlyBudgetUSD != 40 || e.TranscriptRetentionDays != 7 ||
		e.Agent.StagingDir != filepath.Join(data, "staging") || e.Agent.Available {
		t.Fatalf("engine = %+v", e)
	}
	if err := b.RetryJob(context.Background(), 5); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a retry of an unknown job = %v", err)
	}
	if _, err := b.Job(context.Background(), 5); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("an unknown job = %v", err)
	}
	if k := strings.Join(b.runner.Kinds(), ","); k != "bridge.verify,chapters.detect,meta.correct,podcast.fetch,podcast.poll,podcast.retain,podcast.subscribe,sidecars.contribute,sidecars.describe,sidecars.generate" {
		t.Fatalf("kinds = %s", k)
	}
}

// AUDIOSILO_BRIDGE_GITHUB_TOKEN is read from the environment when a contribution
// needs it, never sealed (Phase 2b): a github_pat an earlier version sealed is
// deleted at the start, so a token outlives its variable no more; the token is
// never logged.
func TestGitHubTokenFromTheEnvironment(t *testing.T) {
	data := t.TempDir()
	var logs strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(p)
	}), nil))
	vault, _, err := secrets.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(secrets.GitHubPAT, "ghp_sealed_by_an_old_version"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(contrib.GitHubTokenEnv, "ghp_from_the_environment")
	b, _, err := New(context.Background(), Options{DataDir: data, Version: "0.1.0", Listen: "127.0.0.1:0", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.db.Close() }()
	if ok, _ := b.vault.Present(secrets.GitHubPAT); ok {
		t.Fatal("the sealed github_pat survived the start")
	}
	if !b.canContribute(context.Background()) || b.contributeFrom(context.Background()) != contrib.FromEnv {
		t.Fatal("the environment's token doesn't resolve")
	}
	acc := b.Accounts(context.Background())
	if !acc.GitHub.EnvToken || acc.GitHub.From != contrib.FromEnv || !acc.GitHub.CanContribute {
		t.Fatalf("accounts = %+v", acc.GitHub)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), "ghp_") {
		t.Fatalf("the token reached the log: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "removed the sealed copy") {
		t.Fatalf("the removal wasn't logged: %s", logs.String())
	}
}

// A github_pat sealed with another secret key (the key file replaced) can't be
// opened, and nothing reads it: an unpaired Bridge still starts, and it goes.
func TestUnreadableSealedGitHubPATNeverStopsTheStart(t *testing.T) {
	data := t.TempDir()
	vault, _, err := secrets.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(secrets.GitHubPAT, "ghp_sealed_by_an_old_version"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(data, secrets.KeyFileName)); err != nil {
		t.Fatal(err)
	}
	b, _, err := New(context.Background(), Options{DataDir: data, Version: "0.1.0", Listen: "127.0.0.1:0",
		Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = b.db.Close() }()
	if _, err := os.Stat(filepath.Join(data, "secrets", secrets.GitHubPAT+".enc")); !os.IsNotExist(err) {
		t.Fatalf("the unreadable github_pat stayed: %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
