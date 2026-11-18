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

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
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
	// refreshDown answers a refresh 502 (counted in downHits, never rotating);
	// refreshGate, when set, holds a refresh until it is closed, after sending
	// on refreshSeen.
	refreshDown bool
	downHits    int
	refreshGate chan struct{}
	refreshSeen chan struct{}
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
		if r.PostForm.Get("grant_type") == "refresh_token" {
			f.mu.Lock()
			gate, seen := f.refreshGate, f.refreshSeen
			f.mu.Unlock()
			if gate != nil {
				seen <- struct{}{}
				<-gate
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.PostForm.Get("grant_type") == "refresh_token" {
			if f.refreshDown {
				f.downHits++
				w.WriteHeader(http.StatusBadGateway)
				return
			}
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

// A refresh that gets no answer from GitHub (unreachable, a 502) never refuses
// the sign-in: the access token serves until it expires, the refresh is tried
// again a minute later, and it goes through once GitHub answers.
func TestRefreshWithoutAnswerKeepsSignIn(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	hits := func() int { r.fake.mu.Lock(); defer r.fake.mu.Unlock(); return r.fake.downHits }
	r.fake.mu.Lock()
	r.fake.refreshDown = true
	r.fake.mu.Unlock()
	r.clock.Add(8*time.Hour - time.Minute) // within refreshBefore of the expiry
	if got, err := r.gh.Token(ctx); err != nil || got != "ghu_first" {
		t.Fatalf("token = %q, %v (the access token still serves)", got, err)
	}
	if _, err := r.gh.Token(ctx); err != nil || hits() != 1 {
		t.Fatalf("err = %v, refreshes tried = %d (one a minute)", err, hits())
	}
	r.clock.Add(2 * time.Minute) // expired, and a minute since the last try
	if _, err := r.gh.Token(ctx); !errors.Is(err, ErrUnrefreshed) || errors.Is(err, ErrRefused) || hits() != 2 {
		t.Fatalf("err = %v, refreshes tried = %d (an expired token without an answer is no refusal)", err, hits())
	}
	// GitHub answered (a 502): said so, never "couldn't be reached" (bridge#25).
	if _, err := r.gh.Token(ctx); !strings.Contains(err.Error(), "server error (502)") ||
		strings.Contains(err.Error(), "couldn't be reached") || r.gh.Unrefreshed() != "GitHub answered with a server error (502)" {
		t.Fatalf("err = %v, unrefreshed %q", err, r.gh.Unrefreshed())
	}
	if st := r.gh.State(ctx); st.Refused || !st.SignedIn {
		t.Fatalf("state = %+v", st)
	}
	// A Token that fails before the refresh (a request gone away) says
	// nothing of it: the reason stands, so the next token still signals.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.gh.Token(gone); err == nil || errors.Is(err, ErrUnrefreshed) ||
		r.gh.Unrefreshed() != "GitHub answered with a server error (502)" {
		t.Fatalf("a cancelled token: %v, unrefreshed %q", err, r.gh.Unrefreshed())
	}
	r.fake.mu.Lock()
	r.fake.refreshDown = false
	r.fake.mu.Unlock()
	r.clock.Add(time.Minute)
	changes := r.changes.Load()
	if got, err := r.gh.Token(ctx); err != nil || got != "ghu_r1" {
		t.Fatalf("token = %q, %v (GitHub answers again)", got, err)
	}
	// The token is handed out again: the change is signalled (the heartbeat and
	// the parked books), once.
	if r.gh.Unrefreshed() != "" || r.changes.Load() != changes+1 {
		t.Fatalf("unrefreshed %q, changes %d (want none, %d)", r.gh.Unrefreshed(), r.changes.Load(), changes+1)
	}
	if _, err := r.gh.Token(ctx); err != nil || r.changes.Load() != changes+1 {
		t.Fatalf("a second token: %v, changes %d", err, r.changes.Load())
	}
}

// A refresh that never reached GitHub says so, apart from one GitHub answered
// (allowed: unreachable reads as unreachable; denied: an answer never does,
// TestRefreshUnanswered), and a sign-out forgets it.
func TestRefreshUnreachableWording(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	r.gh.o.Client.WebBase = gone.URL
	r.clock.Add(8*time.Hour + time.Minute) // past the expiry
	_, err := r.gh.Token(ctx)
	if !errors.Is(err, ErrUnrefreshed) || !strings.Contains(err.Error(), "GitHub couldn't be reached") ||
		r.gh.Unrefreshed() != "GitHub couldn't be reached" {
		t.Fatalf("err = %v, unrefreshed %q", err, r.gh.Unrefreshed())
	}
	if _, err := r.gh.SignOut(ctx); err != nil {
		t.Fatal(err)
	}
	if r.gh.Unrefreshed() != "" {
		t.Fatalf("unrefreshed after a sign-out = %q", r.gh.Unrefreshed())
	}
}

// refreshProblem tells each failure apart.
func TestRefreshProblem(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&ghauth.Error{Status: http.StatusBadGateway}, "GitHub answered with a server error (502)"},
		{&ghauth.Error{Status: http.StatusTooManyRequests}, "GitHub is limiting the Bridge's requests (429)"},
		{&ghauth.Error{Status: http.StatusNotFound}, "GitHub answered 404 instead of a new token"},
		{errors.New("GitHub's answer isn't JSON: x"), "GitHub's answer wasn't a new token"},
		{ghauth.ErrNoClientID, ghauth.ErrNoClientID.Error()},
	} {
		if got := refreshProblem(tc.err); got != tc.want {
			t.Errorf("refreshProblem(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// A refresh GitHub doesn't answer takes as long as ghauth lets it, and the
// access token can expire meanwhile: Token judges the expiry after the refresh
// (no expired token handed out), a 401 on a token past its expiry refuses
// nothing, and the sign-in works again once GitHub answers.
func TestRefreshOutlastingTheToken(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	r.fake.mu.Lock()
	r.fake.refreshDown = true
	r.fake.refreshGate, r.fake.refreshSeen = make(chan struct{}), make(chan struct{}, 1)
	r.fake.mu.Unlock()
	r.clock.Add(8*time.Hour - 10*time.Second) // ten seconds before the expiry
	done := make(chan error, 1)
	go func() { _, err := r.gh.Token(ctx); done <- err }()
	<-r.fake.refreshSeen
	r.clock.Add(20 * time.Second) // the refresh hangs past the expiry
	close(r.fake.refreshGate)
	if err := <-done; !errors.Is(err, ErrUnrefreshed) {
		t.Fatalf("err = %v, want ErrUnrefreshed (the token expired during the refresh)", err)
	}
	r.gh.Refused(ctx, "ghu_first") // a call made with it before it expired
	if st := r.gh.State(ctx); st.Refused || !st.SignedIn {
		t.Fatalf("state = %+v (a 401 on an expired token is no refusal)", st)
	}
	r.fake.mu.Lock()
	r.fake.refreshDown = false
	r.fake.mu.Unlock()
	r.clock.Add(time.Minute)
	if got, err := r.gh.Token(ctx); err != nil || got != "ghu_r1" {
		t.Fatalf("token = %q, %v (GitHub answers again)", got, err)
	}
}

// A refresh outlives its caller: cut short after GitHub rotated the pair, the
// new pair would be lost and the sign-in with it.
func TestRefreshOutlivesItsCaller(t *testing.T) {
	r := newRig(t)
	r.signIn(t)
	r.fake.mu.Lock()
	r.fake.refreshGate, r.fake.refreshSeen = make(chan struct{}), make(chan struct{}, 1)
	r.fake.mu.Unlock()
	r.clock.Add(8 * time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	type answer struct {
		tok string
		err error
	}
	done := make(chan answer, 1)
	go func() { tok, err := r.gh.Token(ctx); done <- answer{tok, err} }()
	<-r.fake.refreshSeen
	cancel() // the page request went away mid-refresh
	close(r.fake.refreshGate)
	got := <-done
	if got.err != nil || got.tok != "ghu_r1" {
		t.Fatalf("token = %q, %v", got.tok, got.err)
	}
	raw, _ := r.vault.Get(secrets.GitHubUserToken)
	if !strings.Contains(raw, "ghr_r1") || r.gh.State(context.Background()).Refused {
		t.Fatal("the rotated pair is kept and the sign-in works")
	}
}

// A device flow ended (signed out, cancelled, or replaced by a newer one) while
// it was finishing stores nothing.
func TestEndedFlowIsNotKept(t *testing.T) {
	r := newRig(t)
	f := &deviceFlow{cancel: func() {}}
	err := r.gh.keep(context.Background(), f, ghauth.Token{AccessToken: "ghu_late", RefreshToken: "ghr_late"}, "late")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if v, _ := r.vault.Get(secrets.GitHubUserToken); v != "" || r.gh.State(context.Background()).SignedIn {
		t.Fatal("an ended flow's sign-in was stored")
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

// TestOKRecordsWhenTheSignInWorked: a call that worked with the current token
// moves last_ok_at; one with a replaced token doesn't, and none ends a refusal.
func TestOKRecordsWhenTheSignInWorked(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.signIn(t)
	signedIn := r.gh.State(ctx).LastOKAt
	r.clock.Add(time.Hour)
	r.gh.OK(ctx, "ghu_old_replaced")
	if got := r.gh.State(ctx).LastOKAt; got != signedIn {
		t.Fatalf("last_ok_at = %q, want %q: a replaced token's call says nothing", got, signedIn)
	}
	r.gh.OK(ctx, "ghu_first")
	if got, want := r.gh.State(ctx).LastOKAt, store.Timestamp(r.clock.Now()); got != want {
		t.Fatalf("last_ok_at = %q, want %q", got, want)
	}
	r.gh.Refused(ctx, "ghu_first")
	r.gh.OK(ctx, "ghu_first")
	if _, err := r.gh.Token(ctx); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused: a call that worked never ends a refusal", err)
	}
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

// Setting or removing another kind never moves the time of the credential in
// use (agentbox keeps a refusal of it until that time moves); replacing it does.
func TestAgentCredentialSincePerKind(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	c := &clock{now: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	a := NewAgent(secrets.NewMemStore(), db, c.Now, nil, nil)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(a.Set(ctx, AgentClaudeToken, "sk-ant-oat01-x"))
	set := c.Now()
	c.Add(time.Hour)
	must(a.Set(ctx, AgentOpenAIKey, "sk-openai-1"))
	must(a.Set(ctx, AgentAnthropicKey, "sk-ant-api-1"))
	keySet := c.Now()
	must(a.Delete(ctx, AgentOpenAIKey))
	if kind, _, since, _ := a.Credential(ctx, "claude"); kind != agentproto.CredentialOAuthToken || !since.Equal(set) {
		t.Fatalf("claude = %q since %v, want the token's own %v", kind, since, set)
	}
	c.Add(time.Hour)
	must(a.Set(ctx, AgentClaudeToken, "sk-ant-oat01-y"))
	if _, _, since, _ := a.Credential(ctx, "claude"); !since.Equal(c.Now()) {
		t.Fatalf("a replaced token's time = %v, want %v", since, c.Now())
	}
	must(a.Delete(ctx, AgentClaudeToken))
	if kind, _, since, _ := a.Credential(ctx, "claude"); kind != agentproto.CredentialAPIKey || !since.Equal(keySet) {
		t.Fatalf("claude = %q since %v, want the key's own %v", kind, since, keySet)
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
	a := NewAgent(vault, db, nil, func() { changes++ }, nil)
	if _, _, _, ok := a.Credential(ctx, "claude"); ok {
		t.Fatal("no credential yet")
	}
	for _, bad := range []struct{ kind, value string }{
		{"github_user_token", "x"}, {AgentClaudeToken, ""}, {AgentClaudeToken, "two words"},
		{AgentClaudeToken, "line\nbreak"}, {AgentClaudeToken, strings.Repeat("a", agentproto.MaxCredentialValueBytes+1)},
	} {
		if err := a.Set(ctx, bad.kind, bad.value); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("Set(%q) = %v, want ErrInvalidCredential", bad.kind, err)
		}
	}
	if err := a.Set(ctx, AgentAnthropicKey, " sk-ant-api-1 "); err != nil {
		t.Fatal(err)
	}
	if kind, v, since, ok := a.Credential(ctx, "claude"); !ok || kind != agentproto.CredentialAPIKey || v != "sk-ant-api-1" || since.IsZero() {
		t.Fatalf("claude = %q %q %v %v", kind, v, since, ok)
	}
	if err := a.Set(ctx, AgentClaudeToken, "sk-ant-oat01-x"); err != nil {
		t.Fatal(err)
	}
	if kind, v, _, _ := a.Credential(ctx, "claude"); kind != agentproto.CredentialOAuthToken || v != "sk-ant-oat01-x" {
		t.Fatalf("a subscription token comes first: %q %q", kind, v)
	}
	if _, _, _, ok := a.Credential(ctx, "codex"); ok {
		t.Fatal("codex has no credential")
	}
	st := a.State(ctx)
	if !st.Set[AgentClaudeToken] || !st.Set[AgentAnthropicKey] || st.Set[AgentOpenAIKey] || st.SetAt == "" ||
		st.SetAtByKind[AgentClaudeToken] == "" || st.SetAtByKind[AgentAnthropicKey] == "" || len(st.SetAtByKind) != 2 {
		t.Fatalf("state = %+v", st)
	}
	if err := a.Delete(ctx, AgentClaudeToken); err != nil {
		t.Fatal(err)
	}
	if kind, _, _, _ := a.Credential(ctx, "claude"); kind != agentproto.CredentialAPIKey {
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

// The endpoint's key (bridge#84) is the openai-compatible backend's alone:
// never codex's (a run against OpenAI), and codex's OpenAI key never the
// endpoint's.
func TestAgentEndpointKey(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if got := KindsFor(agent.IDOpenAICompatible); len(got) != 1 || got[0] != AgentEndpointKey {
		t.Fatalf("KindsFor = %v", got)
	}
	addr := "https://openrouter.ai/api/v1"
	a := NewAgent(secrets.NewMemStore(), db, nil, nil, func() string { return addr })
	if err := a.Set(ctx, AgentOpenAIKey, "sk-openai"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := a.Credential(ctx, agent.IDOpenAICompatible); ok {
		t.Fatal("the OpenAI key went to the endpoint")
	}
	if err := a.Set(ctx, AgentEndpointKey, "sk-or-v1-endpoint"); err != nil {
		t.Fatal(err)
	}
	if kind, v, since, ok := a.Credential(ctx, agent.IDOpenAICompatible); !ok || kind != agentproto.CredentialEndpointKey ||
		v != "sk-or-v1-endpoint" || since.IsZero() {
		t.Fatalf("endpoint = %q %q %v %v", kind, v, since, ok)
	}
	if _, v, _, _ := a.Credential(ctx, "codex"); v != "sk-openai" {
		t.Fatalf("codex got %q", v)
	}
	if st := a.State(ctx); !st.Set[AgentEndpointKey] || st.SetAtByKind[AgentEndpointKey] == "" {
		t.Fatalf("state = %+v", st)
	}
}

// The endpoint's key is kept for the address it was entered for: it is the
// endpoint's credential, and goes along with a check, only while that is the
// endpoint's address (allowed); under any other address, however it changed,
// it is no credential, isn't shown as set, goes to no check, and is dropped
// (denied). Without an address there is nothing to enter a key for.
func TestAgentEndpointKeyIsItsAddressAlone(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	const first, other = "https://openrouter.ai/api/v1", "https://elsewhere.example/v1"
	addr := ""
	vault := secrets.NewMemStore()
	var changes int
	a := NewAgent(vault, db, nil, func() { changes++ }, func() string { return addr })
	if err := a.Set(ctx, AgentEndpointKey, "sk-first"); !errors.Is(err, ErrInvalidCredential) || !strings.Contains(err.Error(), "address") {
		t.Fatalf("a key with no address = %v", err)
	}
	if ok, _ := vault.Present(AgentEndpointKey); ok {
		t.Fatal("a key with no address was stored")
	}
	addr = first
	if err := a.Set(ctx, AgentEndpointKey, "sk-first"); err != nil {
		t.Fatal(err)
	}
	if key, err := a.EndpointKey(ctx, first); err != nil || key != "sk-first" {
		t.Fatalf("its own address: %q, %v", key, err)
	}
	if key, _ := a.EndpointKey(ctx, other); key != "" {
		t.Fatalf("a check of another address got the key %q", key)
	}
	if dropped, err := a.DropStaleEndpointKey(ctx); dropped || err != nil {
		t.Fatalf("a key for the address now was dropped: %v, %v", dropped, err)
	}

	// The address moves (the page, config.yaml or the environment).
	addr = other
	if _, v, _, ok := a.Credential(ctx, agent.IDOpenAICompatible); ok || v != "" {
		t.Fatalf("the key went to another address: %q", v)
	}
	for _, to := range []string{first, other} {
		if key, _ := a.EndpointKey(ctx, to); key != "" {
			t.Fatalf("EndpointKey(%s) = %q after the address moved", to, key)
		}
	}
	if st := a.State(ctx); st.Set[AgentEndpointKey] || st.SetAtByKind[AgentEndpointKey] != "" {
		t.Fatalf("a key for another address shows as set: %+v", st)
	}
	before := changes
	if dropped, err := a.DropStaleEndpointKey(ctx); !dropped || err != nil {
		t.Fatalf("drop = %v, %v", dropped, err)
	}
	if ok, _ := vault.Present(AgentEndpointKey); ok || changes != before {
		t.Fatalf("after the drop: present %v, changes %d (want %d)", ok, changes, before)
	}
	// Moving back doesn't bring it back, and a key entered now is this address's.
	addr = first
	if _, _, _, ok := a.Credential(ctx, agent.IDOpenAICompatible); ok {
		t.Fatal("a dropped key came back")
	}
	addr = other
	if err := a.Set(ctx, AgentEndpointKey, "sk-other"); err != nil {
		t.Fatal(err)
	}
	if _, v, _, _ := a.Credential(ctx, agent.IDOpenAICompatible); v != "sk-other" {
		t.Fatalf("the new address's key = %q", v)
	}
	// A key replaced after the address moved is the new address's, never the
	// old key under the new address.
	addr = first
	if err := a.Set(ctx, AgentEndpointKey, "sk-first-again"); err != nil {
		t.Fatal(err)
	}
	if key, _ := a.EndpointKey(ctx, first); key != "sk-first-again" {
		t.Fatalf("replaced = %q", key)
	}
}
