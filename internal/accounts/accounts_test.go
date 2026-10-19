package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/ghauth"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// fakeGitHub answers the device flow (approved on the second poll), the
// refresh (rotating the pair, or refusing when badRefresh), the revoke and
// /user.
type fakeGitHub struct {
	mu          sync.Mutex
	polls       int
	refreshes   []string
	revoked     []string
	badRefresh  bool
	revokeFails bool
	denied      bool
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, map[string]any{"device_code": "dev", "user_code": "WXYZ-0001",
			"verification_uri": "https://github.com/login/device", "expires_in": 900, "interval": 5})
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.PostForm.Get("grant_type") == "refresh_token" {
			f.refreshes = append(f.refreshes, r.PostForm.Get("refresh_token"))
			if f.badRefresh {
				answer(w, http.StatusOK, map[string]any{"error": "bad_refresh_token"})
				return
			}
			n := len(f.refreshes)
			answer(w, http.StatusOK, map[string]any{"access_token": "ghu_r" + string(rune('0'+n)), "expires_in": 28800,
				"refresh_token": "ghr_r" + string(rune('0'+n)), "refresh_token_expires_in": 15897600})
			return
		}
		f.polls++
		switch {
		case f.denied:
			answer(w, http.StatusOK, map[string]any{"error": "access_denied"})
		case f.polls < 2:
			answer(w, http.StatusOK, map[string]any{"error": "authorization_pending"})
		default:
			answer(w, http.StatusOK, map[string]any{"access_token": "ghu_first", "expires_in": 28800,
				"refresh_token": "ghr_first", "refresh_token_expires_in": 15897600})
		}
	})
	mux.HandleFunc("POST /credentials/revoke", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Credentials []string `json:"credentials"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.revoked = append(f.revoked, body.Credentials...)
		fails := f.revokeFails
		f.mu.Unlock()
		if fails {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, map[string]any{"login": "octocat", "id": 1})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type rig struct {
	gh      *GitHub
	vault   *secrets.MemStore
	db      *store.DB
	fake    *fakeGitHub
	clock   *clock
	changes *atomic.Int32
}

func newRig(t *testing.T) *rig {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fakeGitHub{}
	srv := f.serve(t)
	c := &clock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	r := &rig{vault: secrets.NewMemStore(), db: db, fake: f, clock: c, changes: &atomic.Int32{}}
	r.gh = NewGitHub(GitHubOptions{Vault: r.vault, DB: db,
		Client:   &ghauth.Client{ClientID: "Iv23test", WebBase: srv.URL, APIBase: srv.URL, Now: c.Now},
		Now:      c.Now,
		Sleep:    func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		OnChange: func() { r.changes.Add(1) }})
	return r
}

// signIn runs the device flow to its end.
func (r *rig) signIn(t *testing.T) {
	t.Helper()
	p, err := r.gh.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.UserCode != "WXYZ-0001" || p.VerificationURI != "https://github.com/login/device" {
		t.Fatalf("pending = %+v", p)
	}
	waitFor(t, func() bool { return r.gh.State(context.Background()).SignedIn })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDeviceFlowSignsIn(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if st := r.gh.State(ctx); !st.Available || st.SignedIn {
		t.Fatalf("before = %+v", st)
	}
	if _, err := r.gh.Token(ctx); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("token before sign-in: %v", err)
	}
	r.signIn(t)
	st := r.gh.State(ctx)
	if st.Login != "octocat" || st.Pending != nil || st.Refused || st.SignedInAt == "" {
		t.Fatalf("after = %+v", st)
	}
	if got, err := r.gh.Token(ctx); err != nil || got != "ghu_first" {
		t.Fatalf("token = %q, %v", got, err)
	}
	if r.gh.Login(ctx) != "octocat" {
		t.Fatal("login")
	}
	waitFor(t, func() bool { return r.changes.Load() >= 1 })
	raw, _ := r.vault.Get(secrets.GitHubUserToken)
	if !strings.Contains(raw, "ghr_first") {
		t.Fatal("the refresh token is sealed with the access token")
	}
}

func TestDeviceFlowDenied(t *testing.T) {
	r := newRig(t)
	r.fake.denied = true
	if _, err := r.gh.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r.gh.State(context.Background()).FlowError != "" })
	st := r.gh.State(context.Background())
	if st.SignedIn || st.Pending != nil || !strings.Contains(st.FlowError, "cancelled") {
		t.Fatalf("state = %+v", st)
	}
}

func TestNoClientID(t *testing.T) {
	r := newRig(t)
	r.gh.o.Client.ClientID = ""
	if r.gh.State(context.Background()).Available {
		t.Fatal("no client ID: sign-in isn't available")
	}
	if _, err := r.gh.Start(context.Background()); !errors.Is(err, ghauth.ErrNoClientID) {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshRotatesAndPersists(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	r.clock.Add(8*time.Hour - time.Minute) // within refreshBefore of the expiry
	got, err := r.gh.Token(ctx)
	if err != nil || got != "ghu_r1" {
		t.Fatalf("refreshed token = %q, %v", got, err)
	}
	raw, _ := r.vault.Get(secrets.GitHubUserToken)
	var tok ghauth.Token
	if err := json.Unmarshal([]byte(raw), &tok); err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "ghu_r1" || tok.RefreshToken != "ghr_r1" || !tok.ExpiresAt.Equal(r.clock.Now().Add(8*time.Hour)) {
		t.Fatalf("sealed pair = %+v (the new pair replaces the old one in one write)", tok)
	}
	// The next call uses the new pair, no refresh.
	if got, _ := r.gh.Token(ctx); got != "ghu_r1" || len(r.fake.refreshes) != 1 {
		t.Fatalf("token = %q, refreshes = %v", got, r.fake.refreshes)
	}
	// A second refresh uses the rotated refresh token, never the old one.
	r.clock.Add(8 * time.Hour)
	if got, _ := r.gh.Token(ctx); got != "ghu_r2" || r.fake.refreshes[1] != "ghr_r1" {
		t.Fatalf("token = %q, refreshes = %v", got, r.fake.refreshes)
	}
}

func TestRefreshSingleFlight(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	r.clock.Add(8 * time.Hour)
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() { defer wg.Done(); got[i], _ = r.gh.Token(ctx) }()
	}
	wg.Wait()
	for _, tok := range got {
		if tok != "ghu_r1" {
			t.Fatalf("tokens = %v", got)
		}
	}
	if len(r.fake.refreshes) != 1 {
		t.Fatalf("refreshes = %v (one refresh, the others reuse its pair)", r.fake.refreshes)
	}
}

func TestBadRefreshRefuses(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	r.fake.badRefresh = true
	r.clock.Add(9 * time.Hour)
	if _, err := r.gh.Token(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if st := r.gh.State(ctx); !st.Refused || r.gh.Login(ctx) != "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestRefusedUntilSignedInAgain(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	before := r.changes.Load()
	r.gh.Refused(ctx, "ghu_old_replaced")
	if _, err := r.gh.Token(ctx); err != nil {
		t.Fatal("a 401 on a token a refresh replaced is ignored")
	}
	r.gh.Refused(ctx, "ghu_first")
	if _, err := r.gh.Token(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused (a 401 marks the sign-in refused)", err)
	}
	if r.changes.Load() == before {
		t.Fatal("a refusal kicks the heartbeat")
	}
	st := r.gh.State(ctx)
	if !st.Refused || st.LastError != RefusedText {
		t.Fatalf("state = %+v", st)
	}
	r.fake.polls = 0
	r.signIn(t)
	waitFor(t, func() bool { _, err := r.gh.Token(ctx); return err == nil })
}

func TestSignOutRevokesAndDeletes(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	revokeErr, err := r.gh.SignOut(ctx)
	if err != nil || revokeErr != nil {
		t.Fatalf("sign out = %v, %v", revokeErr, err)
	}
	if strings.Join(r.fake.revoked, ",") != "ghu_first,ghr_first" {
		t.Fatalf("revoked = %v (both tokens)", r.fake.revoked)
	}
	if st := r.gh.State(ctx); st.SignedIn {
		t.Fatalf("state = %+v", st)
	}
	if _, err := r.db.GetAccount(ctx, ProviderGitHub); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("account row: %v", err)
	}
}

func TestSignOutDeletesWhenRevokeFails(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	r.fake.revokeFails = true
	revokeErr, err := r.gh.SignOut(ctx)
	if err != nil || revokeErr == nil {
		t.Fatalf("sign out = %v, %v (the revoke's failure is reported)", revokeErr, err)
	}
	if v, _ := r.vault.Get(secrets.GitHubUserToken); v != "" {
		t.Fatal("a failed revoke never blocks the local delete")
	}
}

func TestSignOutCancelsAFlow(t *testing.T) {
	r := newRig(t)
	r.gh.o.Sleep = func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }
	if _, err := r.gh.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.gh.State(context.Background()).Pending == nil {
		t.Fatal("a flow is pending")
	}
	if _, err := r.gh.SignOut(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.gh.State(context.Background()).Pending != nil {
		t.Fatal("sign-out ends the flow")
	}
}

func TestAgentCredential(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	vault := secrets.NewMemStore()
	var changes int
	a := NewAgent(vault, db, nil, func() { changes++ })
	if _, _, _, ok := a.Credential(ctx, "claude"); ok {
		t.Fatal("no credential yet")
	}
	for _, bad := range []struct{ kind, value string }{
		{"github_user_token", "x"}, {AgentClaudeToken, ""}, {AgentClaudeToken, "two words"},
		{AgentClaudeToken, "line\nbreak"}, {AgentClaudeToken, strings.Repeat("a", MaxCredentialBytes+1)},
	} {
		if err := a.Set(ctx, bad.kind, bad.value); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("Set(%q) = %v, want ErrInvalidCredential", bad.kind, err)
		}
	}
	if err := a.Set(ctx, AgentAnthropicKey, " sk-ant-api-1 "); err != nil {
		t.Fatal(err)
	}
	if kind, v, since, ok := a.Credential(ctx, "claude"); !ok || kind != CredentialAPIKey || v != "sk-ant-api-1" || since.IsZero() {
		t.Fatalf("claude = %q %q %v %v", kind, v, since, ok)
	}
	if err := a.Set(ctx, AgentClaudeToken, "sk-ant-oat01-x"); err != nil {
		t.Fatal(err)
	}
	if kind, v, _, _ := a.Credential(ctx, "claude"); kind != CredentialOAuthToken || v != "sk-ant-oat01-x" {
		t.Fatalf("a subscription token comes first: %q %q", kind, v)
	}
	if _, _, _, ok := a.Credential(ctx, "codex"); ok {
		t.Fatal("codex has no credential")
	}
	st := a.State(ctx)
	if !st.Set[AgentClaudeToken] || !st.Set[AgentAnthropicKey] || st.Set[AgentOpenAIKey] || st.SetAt == "" {
		t.Fatalf("state = %+v", st)
	}
	if err := a.Delete(ctx, AgentClaudeToken); err != nil {
		t.Fatal(err)
	}
	if kind, _, _, _ := a.Credential(ctx, "claude"); kind != CredentialAPIKey {
		t.Fatal("the API key is left")
	}
	if err := a.Delete(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAccount(ctx, ProviderAgent); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("deleting every credential forgets the account")
	}
	if changes != 4 {
		t.Fatalf("changes = %d", changes)
	}
}
