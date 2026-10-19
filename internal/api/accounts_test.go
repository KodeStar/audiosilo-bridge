package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeAccounts is the fake Backend's Accounts tab.
type fakeAccounts struct {
	startErr  error
	signedOut bool
	revokeErr string
	set       map[string]string
	deleted   []string
}

func (b *backend) Accounts(context.Context) Accounts {
	set := map[string]bool{}
	for k := range b.acc.set {
		set[k] = true
	}
	return Accounts{GitHub: GitHubAccount{Available: true, SignedIn: !b.acc.signedOut, Login: "octocat", From: "device", CanContribute: true},
		Agent: AgentAccount{Backend: "claude", Kinds: []string{"claude_oauth_token", "anthropic_api_key"}, Set: set}}
}
func (b *backend) StartGitHubSignIn(context.Context) (GitHubPending, error) {
	if b.acc.startErr != nil {
		return GitHubPending{}, b.acc.startErr
	}
	return GitHubPending{UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device",
		ExpiresAt: time.Date(2026, 10, 19, 12, 15, 0, 0, time.UTC)}, nil
}
func (b *backend) SignOutGitHub(context.Context) (string, error) {
	b.acc.signedOut = true
	return b.acc.revokeErr, nil
}
func (b *backend) SetAgentCredential(_ context.Context, kind, value string) error {
	if kind != "claude_oauth_token" || strings.ContainsAny(value, " \n") {
		return Errorf(ErrInvalid, "that isn't a credential this Bridge takes")
	}
	if b.acc.set == nil {
		b.acc.set = map[string]string{}
	}
	b.acc.set[kind] = value
	return nil
}
func (b *backend) DeleteAgentCredential(_ context.Context, kind string) error {
	b.acc.deleted = append(b.acc.deleted, kind)
	delete(b.acc.set, kind)
	return nil
}

func TestAccountsRoutes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.do(http.MethodGet, "/api/accounts", e.token, "")
	var acc Accounts
	if err := json.Unmarshal(w.Body.Bytes(), &acc); w.Code != http.StatusOK || err != nil || acc.GitHub.Login != "octocat" {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}

	w = e.do(http.MethodPost, "/api/accounts/github/device", e.token, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"user_code":"ABCD-1234"`) {
		t.Fatalf("device = %d %s", w.Code, w.Body.String())
	}
	e.b.acc.startErr = Errorf(ErrRefused, "sign-in isn't available: set github.client_id")
	if w = e.do(http.MethodPost, "/api/accounts/github/device", e.token, ""); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refused device = %d %s", w.Code, w.Body.String())
	}

	e.b.acc.revokeErr = "GitHub didn't confirm the revoke"
	w = e.do(http.MethodDelete, "/api/accounts/github", e.token, "")
	if w.Code != http.StatusOK || !e.b.acc.signedOut || !strings.Contains(w.Body.String(), `"revoke_error":"GitHub didn't confirm the revoke"`) {
		t.Fatalf("sign out = %d %s", w.Code, w.Body.String())
	}

	// A credential's value is taken and never answered back.
	w = e.do(http.MethodPut, "/api/accounts/agent", e.token, `{"kind":"claude_oauth_token","value":"sk-ant-oat01-secret"}`)
	if w.Code != http.StatusOK || e.b.acc.set["claude_oauth_token"] != "sk-ant-oat01-secret" ||
		strings.Contains(w.Body.String(), "secret") || !strings.Contains(w.Body.String(), `"claude_oauth_token":true`) {
		t.Fatalf("set = %d %s", w.Code, w.Body.String())
	}
	w = e.do(http.MethodPut, "/api/accounts/agent", e.token, `{"kind":"claude_oauth_token","value":"has space-secret"}`)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("bad value = %d %s", w.Code, w.Body.String())
	}
	if w = e.do(http.MethodPut, "/api/accounts/agent", e.token, `{"kind":"x","value":"v","extra":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", w.Code)
	}
	w = e.do(http.MethodDelete, "/api/accounts/agent?kind=claude_oauth_token", e.token, "")
	if w.Code != http.StatusOK || len(e.b.acc.deleted) != 1 || e.b.acc.deleted[0] != "claude_oauth_token" {
		t.Fatalf("delete = %d %v", w.Code, e.b.acc.deleted)
	}
}

// Security: the Accounts routes refuse a request without a session, and a public
// address even with one (denied), as every page route does.
func TestAccountsRoutesDenied(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, rt := range [][2]string{{"GET", "/api/accounts"}, {"POST", "/api/accounts/github/device"},
		{"DELETE", "/api/accounts/github"}, {"PUT", "/api/accounts/agent"}, {"DELETE", "/api/accounts/agent"}} {
		if w := e.do(rt[0], rt[1], "", `{"kind":"claude_oauth_token","value":"v"}`); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated = %d", rt[0], rt[1], w.Code)
		}
		if w := e.do(rt[0], rt[1], e.token, `{"kind":"claude_oauth_token","value":"v"}`, "203.0.113.9:1"); w.Code != http.StatusForbidden {
			t.Errorf("%s %s from a public address = %d", rt[0], rt[1], w.Code)
		}
	}
	if e.b.acc.signedOut || len(e.b.acc.set) > 0 || len(e.b.acc.deleted) > 0 {
		t.Fatalf("a denied request reached the backend: %+v", e.b.acc)
	}
}
