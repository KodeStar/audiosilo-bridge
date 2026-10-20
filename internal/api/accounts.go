package api

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// The Accounts tab (BRIDGE-PLAN.md Phase 2b, decisions 17 and 19): the Bridge's
// GitHub sign-in (the GitHub App's device flow) for contributions, and the
// agent's credential the Bridge hands to the agent container. A credential's
// value is taken here and never answered back: the routes say whether one is
// set, which kind, and since when.

// Accounts is GET /api/accounts.
type Accounts struct {
	GitHub GitHubAccount `json:"github"`
	Agent  AgentAccount  `json:"agent"`
}

// GitHubAccount is the GitHub sign-in and where a contribution's token comes
// from now.
type GitHubAccount struct {
	// Available: a GitHub App client ID is configured, so a sign-in can start.
	Available bool `json:"available"`
	// SignedIn: a sign-in is stored; Refused: GitHub stopped taking it.
	SignedIn   bool   `json:"signed_in"`
	Refused    bool   `json:"refused"`
	Login      string `json:"login"`
	SignedInAt string `json:"signed_in_at"`
	LastOKAt   string `json:"last_ok_at"`
	LastError  string `json:"last_error"`
	// Pending is the sign-in waiting for the code to be entered on GitHub;
	// FlowError why the last one ended without a sign-in.
	Pending   *GitHubPending `json:"pending"`
	FlowError string         `json:"flow_error"`
	// EnvToken: AUDIOSILO_BRIDGE_GITHUB_TOKEN is set (it wins over the sign-in).
	EnvToken bool `json:"env_token"`
	// From is where a contribution's token comes from now: env, device, gh, or
	// "" for none; CanContribute whether one does.
	From          string `json:"from"`
	CanContribute bool   `json:"can_contribute"`
	// ManageURL is where the admin reviews or revokes the App's access on GitHub.
	ManageURL string `json:"manage_url"`
}

// GitHubPending is a sign-in code to enter at VerificationURI before ExpiresAt.
type GitHubPending struct {
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// AgentAccount is the agent's credential as the Bridge holds it, and the agent
// container's view of it.
type AgentAccount struct {
	// Backend is the CLI the agent container runs (claude or codex); Kinds the
	// credential kinds it takes, in the order it uses them.
	Backend string   `json:"backend"`
	Kinds   []string `json:"kinds"`
	// Set names the kinds the Bridge holds (claude_oauth_token,
	// anthropic_api_key, openai_api_key); SetAt when the last was saved;
	// SetAtByKind when each kind held was (Phase 2b; a kind without its own
	// time is left out).
	Set         map[string]bool   `json:"set"`
	SetAt       string            `json:"set_at"`
	SetAtByKind map[string]string `json:"set_at_by_kind"`
	// Available and Detail are the agent container's state; Handoff that its
	// runs get the Bridge's credential; ContainerCredential the kind the
	// container has of its own ("" for none), which wins over the Bridge's.
	Available           bool   `json:"available"`
	Detail              string `json:"detail"`
	Handoff             bool   `json:"handoff"`
	ContainerCredential string `json:"container_credential"`
}

// AccountsBackend is the Accounts tab's part of the Backend.
type AccountsBackend interface {
	Accounts(ctx context.Context) Accounts
	// StartGitHubSignIn begins the device flow (ErrRefused: no client ID, or
	// GitHub refused; ErrUnreachable: GitHub can't be reached).
	StartGitHubSignIn(ctx context.Context) (GitHubPending, error)
	// SignOutGitHub forgets the sign-in (ending one in progress) and revokes it
	// on GitHub; revokeErr says the revoke failed (the sign-in is gone here).
	SignOutGitHub(ctx context.Context) (revokeErr string, err error)
	// SetAgentCredential seals one agent credential (ErrInvalid: an unknown
	// kind or a malformed value, said without the value).
	SetAgentCredential(ctx context.Context, kind, value string) error
	// DeleteAgentCredential removes one kind ("" for all).
	DeleteAgentCredential(ctx context.Context, kind string) error
}

func (a *API) accountRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/accounts", a.requireAuth(a.handleAccounts))
	mux.HandleFunc("POST /api/accounts/github/device", a.requireAuth(a.handleGitHubDevice))
	mux.HandleFunc("DELETE /api/accounts/github", a.requireAuth(a.handleGitHubSignOut))
	mux.HandleFunc("PUT /api/accounts/agent", a.requireAuth(a.handleSetAgent))
	mux.HandleFunc("DELETE /api/accounts/agent", a.requireAuth(a.handleDeleteAgent))
}

func (a *API) handleAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.b.Accounts(r.Context()))
}

func (a *API) handleGitHubDevice(w http.ResponseWriter, r *http.Request) {
	p, err := a.b.StartGitHubSignIn(r.Context())
	if err != nil {
		a.writeBackendError(w, err, "start the GitHub sign-in")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) handleGitHubSignOut(w http.ResponseWriter, r *http.Request) {
	revokeErr, err := a.b.SignOutGitHub(r.Context())
	if err != nil {
		a.writeBackendError(w, err, "sign out of GitHub")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": a.b.Accounts(r.Context()), "revoke_error": revokeErr})
}

func (a *API) handleSetAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := a.b.SetAgentCredential(r.Context(), strings.TrimSpace(req.Kind), req.Value); err != nil {
		a.writeBackendError(w, err, "save the agent credential")
		return
	}
	writeJSON(w, http.StatusOK, a.b.Accounts(r.Context()))
}

func (a *API) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := a.b.DeleteAgentCredential(r.Context(), strings.TrimSpace(r.URL.Query().Get("kind"))); err != nil {
		a.writeBackendError(w, err, "remove the agent credential")
		return
	}
	writeJSON(w, http.StatusOK, a.b.Accounts(r.Context()))
}
