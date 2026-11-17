package api

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// The Accounts tab's Audible section (BRIDGE-PLAN.md Phase 4a, decision 19):
// sign in to an Audible account by pasting back the page Amazon lands on, map
// it to a server user and a books library with a policy, and remove it. The
// sign-in's tokens never reach the page: the routes answer the account's
// label, mapping and how its polls went. The redirect URL the admin pastes
// holds a one-time authorization code; it is taken here and never answered
// back or logged.

// audibleWriteTimeout is how long a sign-in, a mapping or a removal may take to
// answer: each calls Amazon or the server, whose 429/503 backoff may wait
// minutes, and the page's own 60-second write timeout would drop the answer of
// work that went through (a sign-in's code is spent by then).
const audibleWriteTimeout = 15 * time.Minute

// slowAnswer lets a handler that calls Amazon or the server answer after the
// page's write timeout.
func slowAnswer(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(audibleWriteTimeout))
}

// AudibleAccounts is GET /api/accounts/audible.
type AudibleAccounts struct {
	Accounts []AudibleAccount `json:"accounts"`
	// Marketplaces and Policies are the choices the forms offer.
	Marketplaces []string `json:"marketplaces"`
	Policies     []string `json:"policies"`
	// Paired: the Bridge is paired, so an account can be signed in and mapped.
	Paired bool `json:"paired"`
	// Libraries are the server's books libraries (from the last heartbeat).
	Libraries []AudibleLibrary `json:"libraries"`
	// Users are who an account may be mapped to (the server's GET
	// /bridge/users, read live); UsersError why they couldn't be read.
	Users      []AudibleUser `json:"users"`
	UsersError string        `json:"users_error"`
}

// AudibleAccount is one account as the page shows it.
type AudibleAccount struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Marketplace string `json:"marketplace"`
	UserID      int64  `json:"user_id"`
	UserName    string `json:"user_name"`
	LibraryID   int64  `json:"library_id"`
	LibraryName string `json:"library_name"`
	Policy      string `json:"policy"`
	CreatedAt   string `json:"created_at"`
	LastOKAt    string `json:"last_ok_at"`
	LastPollAt  string `json:"last_poll_at"`
	LastTitles  int    `json:"last_titles"`
	LastMatched int    `json:"last_matched"`
	// Refused: Audible refused the sign-in (RefusedAt); signing in again
	// ends it. LastError is why the last poll failed.
	Refused   bool   `json:"refused"`
	RefusedAt string `json:"refused_at"`
	LastError string `json:"last_error"`
	// OtherServer: the mapping was recorded on another server than the one
	// paired now; map it again here.
	OtherServer bool `json:"other_server"`
	// OldProfile: the account's device registered with an older device
	// profile, so Amazon denies its licenses: sign in again to keep backups
	// working (audibleacct.OldProfile).
	OldProfile bool `json:"old_profile"`
}

// AudibleUser is one person an account may be mapped to.
type AudibleUser struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
}

// AudibleLibrary is one books library.
type AudibleLibrary struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// AudibleSignIn is a sign-in waiting for the admin: open URL, sign in at
// Amazon, paste the page it lands on back before ExpiresAt.
type AudibleSignIn struct {
	Login       string    `json:"login"`
	URL         string    `json:"url"`
	Marketplace string    `json:"marketplace"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// AudibleMapping is an account's mapping as the page sends it. Whether its
// places sync is the server console's (Phase 4c): the page sends none, and
// Sync, from a page an earlier Bridge served still open, is ignored.
type AudibleMapping struct {
	Label     string `json:"label"`
	UserID    int64  `json:"user_id"`
	LibraryID int64  `json:"library_id"`
	Policy    string `json:"policy"`
	Sync      bool   `json:"sync"`
}

// AudibleFinish ends a sign-in.
type AudibleFinish struct {
	Login       string `json:"login"`
	RedirectURL string `json:"redirect_url"`
	AudibleMapping
}

// AudibleBackend is the Audible section's part of the Backend.
type AudibleBackend interface {
	Audible(ctx context.Context) AudibleAccounts
	// StartAudibleSignIn begins a sign-in (ErrInvalid: an unknown marketplace).
	StartAudibleSignIn(ctx context.Context, marketplace string) (AudibleSignIn, error)
	// FinishAudibleSignIn ends one (ErrInvalid: a mapping or page to correct;
	// ErrNotFound: the sign-in expired; ErrRefused: Amazon or the server said
	// no; ErrConflict: not paired).
	FinishAudibleSignIn(ctx context.Context, f AudibleFinish) error
	// MapAudibleAccount maps an account again (ErrNotFound: no such account).
	MapAudibleAccount(ctx context.Context, id string, m AudibleMapping) error
	// RemoveAudibleAccount forgets an account here, then removes it on the
	// server and deregisters its device; warning says what didn't happen.
	RemoveAudibleAccount(ctx context.Context, id string) (warning string, err error)
}

func (a *API) audibleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/accounts/audible", a.requireAuth(a.handleAudible))
	mux.HandleFunc("POST /api/accounts/audible/sign-in", a.requireAuth(a.handleAudibleSignIn))
	mux.HandleFunc("POST /api/accounts/audible", a.requireAuth(a.handleAudibleFinish))
	mux.HandleFunc("PUT /api/accounts/audible/{id}", a.requireAuth(a.handleAudibleMap))
	mux.HandleFunc("DELETE /api/accounts/audible/{id}", a.requireAuth(a.handleAudibleRemove))
}

func (a *API) handleAudible(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.b.Audible(r.Context()))
}

func (a *API) handleAudibleSignIn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Marketplace string `json:"marketplace"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s, err := a.b.StartAudibleSignIn(r.Context(), strings.TrimSpace(req.Marketplace))
	if err != nil {
		a.writeBackendError(w, err, "start the Audible sign-in")
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) handleAudibleFinish(w http.ResponseWriter, r *http.Request) {
	slowAnswer(w)
	var req AudibleFinish
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := a.b.FinishAudibleSignIn(r.Context(), req); err != nil {
		a.writeBackendError(w, err, "finish the Audible sign-in")
		return
	}
	writeJSON(w, http.StatusOK, a.b.Audible(r.Context()))
}

func (a *API) handleAudibleMap(w http.ResponseWriter, r *http.Request) {
	slowAnswer(w)
	var req AudibleMapping
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := a.b.MapAudibleAccount(r.Context(), r.PathValue("id"), req); err != nil {
		a.writeBackendError(w, err, "map the Audible account")
		return
	}
	writeJSON(w, http.StatusOK, a.b.Audible(r.Context()))
}

func (a *API) handleAudibleRemove(w http.ResponseWriter, r *http.Request) {
	slowAnswer(w)
	warning, err := a.b.RemoveAudibleAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeBackendError(w, err, "remove the Audible account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audible": a.b.Audible(r.Context()), "warning": warning})
}
