// Package accounts holds the Bridge's external accounts (BRIDGE-PLAN.md
// decision 19, Phase 2b): the GitHub sign-in a contribution files its issues
// with (the device flow, the refresh of its expiring token, the sign-out that
// revokes it) and the agent's credential the Bridge hands the agent container
// per run. The credentials live in the sealed vault (internal/secrets); the
// store's accounts table keeps what the Accounts page shows about them (the
// login, when it was set, when it last worked, a refusal).
//
// Nothing here logs or returns a credential except to the code that uses it:
// GitHub.Token for a contribution's REST client, Agent.Credential for the
// agent container's hand-off file.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
	"github.com/kodestar/audiosilo-bridge/internal/ghauth"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Providers: the accounts table's keys.
const (
	ProviderGitHub = "github"
	ProviderAgent  = "agent"
)

// Store is the part of the store the accounts write (*store.DB).
type Store interface {
	GetAccount(ctx context.Context, provider string) (store.Account, error)
	SignInAccount(ctx context.Context, provider, login string, at time.Time) error
	AccountOK(ctx context.Context, provider string, at time.Time) error
	RefuseAccount(ctx context.Context, provider, why string, at time.Time) error
	DeleteAccount(ctx context.Context, provider string) error
}

// refreshBefore is how long before its expiry an access token is refreshed.
const refreshBefore = 5 * time.Minute

// refreshRetry spaces the refreshes after one that got no answer from GitHub:
// Token runs at every heartbeat and page poll.
const refreshRetry = time.Minute

// Errors of the GitHub sign-in.
var (
	// ErrNotSignedIn: no GitHub sign-in on this Bridge.
	ErrNotSignedIn = errors.New("the Bridge isn't signed in to GitHub")
	// ErrRefused: GitHub refused the sign-in (revoked, or the refresh token
	// expired); signing in again ends it.
	ErrRefused = errors.New("GitHub refused the Bridge's sign-in: sign in again on the Accounts tab")
	// ErrUnrefreshed: the sign-in's access token expired and its refresh got no
	// answer from GitHub (unreachable, a server error). The sign-in stands and the
	// refresh is tried again after refreshRetry: waiting for GitHub ends it, not a
	// new sign-in.
	ErrUnrefreshed = errors.New("the Bridge's GitHub sign-in couldn't be refreshed: GitHub couldn't be reached")
)

// RefusedText is the accounts row's last_error after a 401.
const RefusedText = "GitHub refused the sign-in (it was revoked or expired): sign in again"

// GitHubOptions wire a GitHub account.
type GitHubOptions struct {
	Vault secrets.Store
	DB    Store
	// Client is the App's GitHub client (its client ID and bases).
	Client *ghauth.Client
	Log    *slog.Logger
	// OnChange runs after a sign-in, a sign-out or a refusal (the heartbeat's
	// checklist and can_contribute move).
	OnChange func()
	// Now and Sleep are the clock and the device flow's wait (tests replace them).
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// GitHub is the Bridge's GitHub sign-in.
type GitHub struct {
	o GitHubOptions

	mu sync.Mutex
	// flow is the device flow in progress (nil for none); flowErr why the last
	// one ended without a sign-in ("" when it signed in or none ran).
	flow    *deviceFlow
	flowErr string
	// refreshMu makes the refresh single-flight: GitHub ends the old pair at the
	// first, so callers wait here and then read the pair it stored. It guards
	// refreshErr, the last refresh's failure to get GitHub's answer, and when
	// (nil and zero after a refresh that worked, a sign-in or a sign-out).
	refreshMu       sync.Mutex
	refreshErr      error
	refreshFailedAt time.Time
}

type deviceFlow struct {
	code   ghauth.DeviceCode
	cancel context.CancelFunc
}

// NewGitHub returns the GitHub account.
func NewGitHub(o GitHubOptions) *GitHub {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.OnChange == nil {
		o.OnChange = func() {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &GitHub{o: o}
}

// Pending is a device flow waiting for the admin: the code to type at
// VerificationURI before ExpiresAt.
type Pending struct {
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// GitHubState is the GitHub sign-in as the Accounts page shows it.
type GitHubState struct {
	// Available: a client ID is configured, so sign-in can start.
	Available bool `json:"available"`
	// SignedIn: a sign-in is stored (Refused says whether GitHub still takes it).
	SignedIn   bool   `json:"signed_in"`
	Login      string `json:"login"`
	SignedInAt string `json:"signed_in_at"`
	LastOKAt   string `json:"last_ok_at"`
	Refused    bool   `json:"refused"`
	LastError  string `json:"last_error"`
	// Pending is the device flow in progress; FlowError why the last one ended
	// without a sign-in.
	Pending   *Pending `json:"pending"`
	FlowError string   `json:"flow_error"`
}

// State is the sign-in now.
func (g *GitHub) State(ctx context.Context) GitHubState {
	st := GitHubState{Available: strings.TrimSpace(g.o.Client.ClientID) != ""}
	g.mu.Lock()
	if g.flow != nil {
		st.Pending = &Pending{UserCode: g.flow.code.UserCode, VerificationURI: g.flow.code.VerificationURI,
			ExpiresAt: g.flow.code.ExpiresAt.UTC()}
	}
	st.FlowError = g.flowErr
	g.mu.Unlock()
	if tok, err := g.stored(); err == nil && tok.AccessToken != "" {
		st.SignedIn = true
	}
	if a, err := g.o.DB.GetAccount(ctx, ProviderGitHub); err == nil && st.SignedIn {
		st.Login, st.SignedInAt, st.LastOKAt, st.LastError = a.Login, a.SignedInAt, a.LastOKAt, a.LastError
		st.Refused = a.RefusedAt != ""
	}
	return st
}

// Login is the signed-in user's login ("" for none, or while GitHub refuses it).
func (g *GitHub) Login(ctx context.Context) string {
	st := g.State(ctx)
	if !st.SignedIn || st.Refused {
		return ""
	}
	return st.Login
}

// Start begins a device flow (ending one in progress): the code is returned for
// the page to show, and the Bridge polls GitHub in the background until the
// admin approves it (the sign-in is stored), cancels, or the code expires.
func (g *GitHub) Start(ctx context.Context) (Pending, error) {
	code, err := g.o.Client.RequestCode(ctx)
	if err != nil {
		return Pending{}, err
	}
	fctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f := &deviceFlow{code: code, cancel: cancel}
	g.mu.Lock()
	if g.flow != nil {
		g.flow.cancel()
	}
	g.flow, g.flowErr = f, ""
	g.mu.Unlock()
	go g.wait(fctx, f)
	return Pending{UserCode: code.UserCode, VerificationURI: code.VerificationURI, ExpiresAt: code.ExpiresAt.UTC()}, nil
}

// wait polls one device flow to its end.
func (g *GitHub) wait(ctx context.Context, f *deviceFlow) {
	tok, err := g.o.Client.Wait(ctx, f.code, g.o.Sleep)
	var login string
	if err == nil {
		var u ghauth.User
		if u, err = g.o.Client.User(ctx, tok.AccessToken); err == nil {
			login = u.Login
		}
	}
	if err == nil {
		err = g.keep(ctx, f, tok, login)
	}
	g.mu.Lock()
	current := g.flow == f
	if current {
		g.flow = nil
		if err != nil && !errors.Is(err, context.Canceled) {
			g.flowErr = flowErrorText(err)
		}
	}
	g.mu.Unlock()
	f.cancel()
	switch {
	case err == nil:
		g.o.Log.Info("accounts: signed in to GitHub", "login", login)
		g.o.OnChange()
	case errors.Is(err, context.Canceled):
	default:
		g.o.Log.Warn("accounts: the GitHub sign-in didn't complete", "err", err)
	}
}

// flowErrorText is a failed device flow's reason for the page.
func flowErrorText(err error) string {
	switch {
	case errors.Is(err, ghauth.ErrExpired), errors.Is(err, ghauth.ErrDenied):
		return err.Error()
	}
	return "The sign-in didn't complete: " + err.Error()
}

// keep stores flow f's sign-in unless f was ended meanwhile (a sign-out, a
// cancel, a newer sign-in): under the refresh lock, so a sign-out that ends f
// deletes the sign-in after it is stored, never before (context.Canceled when f
// is no longer the flow in progress).
func (g *GitHub) keep(ctx context.Context, f *deviceFlow, tok ghauth.Token, login string) error {
	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()
	g.mu.Lock()
	current := g.flow == f
	g.mu.Unlock()
	if !current {
		return context.Canceled
	}
	g.refreshErr, g.refreshFailedAt = nil, time.Time{}
	return g.store(ctx, tok, login)
}

// store seals a new sign-in and records it.
func (g *GitHub) store(ctx context.Context, tok ghauth.Token, login string) error {
	if err := g.seal(tok); err != nil {
		return fmt.Errorf("store the sign-in: %w", err)
	}
	return g.o.DB.SignInAccount(context.WithoutCancel(ctx), ProviderGitHub, login, g.o.Now())
}

// seal writes the token pair into the vault in one write.
func (g *GitHub) seal(tok ghauth.Token) error {
	raw, err := json.Marshal(tok) //nolint:gosec // G117: the pair is sealed into the vault, its one home
	if err != nil {
		return err
	}
	return g.o.Vault.Set(secrets.GitHubUserToken, string(raw))
}

// Cancel ends the device flow in progress (none: nothing to do).
func (g *GitHub) Cancel() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.flow != nil {
		g.flow.cancel()
		g.flow = nil
	}
}

// SignOut ends a device flow in progress, deletes the sign-in here and asks
// GitHub to revoke both tokens. The revoke is best effort (GitHub may be
// unreachable): its failure is returned beside the local delete that always
// happens, for the page to mention.
func (g *GitHub) SignOut(ctx context.Context) (revokeErr error, err error) {
	g.Cancel()
	tok, err := g.forget(ctx)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.flowErr = ""
	g.mu.Unlock()
	g.o.OnChange()
	if tok.AccessToken != "" || tok.RefreshToken != "" {
		if rerr := g.o.Client.Revoke(ctx, tok.AccessToken, tok.RefreshToken); rerr != nil {
			g.o.Log.Warn("accounts: GitHub didn't revoke the signed-out tokens", "err", rerr)
			revokeErr = rerr
		}
	}
	g.o.Log.Info("accounts: signed out of GitHub")
	return revokeErr, nil
}

// forget deletes the sign-in here and returns the pair it held (for the revoke,
// which runs after the refresh lock is let go: Token callers don't wait on
// GitHub).
func (g *GitHub) forget(ctx context.Context) (ghauth.Token, error) {
	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()
	g.refreshErr, g.refreshFailedAt = nil, time.Time{}
	tok, _ := g.stored()
	if err := g.o.Vault.Delete(secrets.GitHubUserToken); err != nil {
		return ghauth.Token{}, err
	}
	if err := g.o.DB.DeleteAccount(ctx, ProviderGitHub); err != nil {
		return ghauth.Token{}, err
	}
	return tok, nil
}

// stored is the sealed sign-in (a zero Token for none).
func (g *GitHub) stored() (ghauth.Token, error) {
	raw, err := g.o.Vault.Get(secrets.GitHubUserToken)
	if err != nil || raw == "" {
		return ghauth.Token{}, err
	}
	var tok ghauth.Token
	if err := json.Unmarshal([]byte(raw), &tok); err != nil {
		return ghauth.Token{}, errors.New("the stored GitHub sign-in is unreadable: sign in again")
	}
	return tok, nil
}

// Token is the sign-in's access token for a REST call, refreshed first when it
// expires within refreshBefore (the new pair sealed in one write before it is
// used). ErrNotSignedIn for none, ErrRefused while GitHub refuses it. A refresh
// that didn't get GitHub's answer keeps the pair and is tried again a minute
// later; meanwhile the access token serves until it expires, then it is an error.
func (g *GitHub) Token(ctx context.Context) (string, error) {
	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()
	tok, err := g.stored()
	if err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", ErrNotSignedIn
	}
	switch a, err := g.o.DB.GetAccount(ctx, ProviderGitHub); {
	case err == nil && a.RefusedAt != "":
		return "", ErrRefused
	case err != nil && !errors.Is(err, store.ErrNotFound):
		// Unknown whether GitHub refused it: never hand out a refused token.
		return "", fmt.Errorf("read the GitHub sign-in: %w", err)
	}
	now := g.o.Now()
	if !tok.Expiring() || now.Before(tok.ExpiresAt.Add(-refreshBefore)) {
		return tok.AccessToken, nil
	}
	if !tok.RefreshExpiresAt.IsZero() && !now.Before(tok.RefreshExpiresAt) {
		g.refuse(ctx, "the GitHub sign-in expired (the Bridge was offline for months): sign in again")
		return "", ErrRefused
	}
	if g.refreshErr != nil && now.Sub(g.refreshFailedAt) < refreshRetry {
		return g.unrefreshed(tok, now, g.refreshErr)
	}
	// The refresh runs to its end whatever the caller does meanwhile (a page
	// request that went away, the heartbeat stopping): cut short after GitHub took
	// it, the new pair would be lost with the old one already spent.
	fresh, err := g.o.Client.Refresh(context.WithoutCancel(ctx), tok.RefreshToken)
	if errors.Is(err, ghauth.ErrBadRefresh) {
		// GitHub answered: the refresh token is spent or revoked, for good.
		g.o.Log.Warn("accounts: GitHub refused to refresh the sign-in", "err", err)
		g.refreshErr, g.refreshFailedAt = nil, time.Time{}
		g.refuse(ctx, ghauth.ErrBadRefresh.Error())
		return "", ErrRefused
	}
	if err != nil {
		// No answer from GitHub (unreachable, a server error): the pair is kept and
		// tried again after refreshRetry. A refresh token GitHub did spend meanwhile
		// answers bad_refresh_token then, which refuses the sign-in above. The clock
		// is read again: a refresh GitHub never answers takes ghauth's request
		// timeout, and the access token may have expired meanwhile.
		now = g.o.Now()
		g.o.Log.Warn("accounts: refreshing the GitHub sign-in failed; it is tried again in a minute", "err", err)
		g.refreshErr, g.refreshFailedAt = err, now
		return g.unrefreshed(tok, now, err)
	}
	g.refreshErr, g.refreshFailedAt = nil, time.Time{}
	if fresh.RefreshToken == "" {
		// GitHub turned expiry off for the App: the new token doesn't expire.
		fresh.ExpiresAt, fresh.RefreshExpiresAt = time.Time{}, time.Time{}
	}
	// The old pair is dead now: the new one must be kept before it is used.
	if err := g.seal(fresh); err != nil {
		g.o.Log.Error("accounts: the refreshed GitHub sign-in couldn't be stored; sign in again after a restart", "err", err)
		return "", fmt.Errorf("store the refreshed sign-in: %w", err)
	}
	_ = g.o.DB.AccountOK(context.WithoutCancel(ctx), ProviderGitHub, now)
	g.o.Log.Info("accounts: refreshed the GitHub sign-in")
	return fresh.AccessToken, nil
}

// unrefreshed is Token's answer while a refresh couldn't get GitHub's answer:
// the access token until it expires, then why it couldn't be refreshed.
func (g *GitHub) unrefreshed(tok ghauth.Token, now time.Time, err error) (string, error) {
	if now.Before(tok.ExpiresAt) {
		return tok.AccessToken, nil
	}
	return "", fmt.Errorf("%w: %w", ErrUnrefreshed, err)
}

// Refused records that GitHub refused token (a 401 on a REST call made with it):
// the sign-in is not used again until a new one, and the heartbeat says the
// Bridge can't contribute. A 401 on a token a refresh has since replaced (a call
// in flight while it refreshed: GitHub ends the old token at once), or on one
// past its expiry (served while GitHub couldn't be reached to refresh it: the
// refresh decides then), says nothing about the sign-in and is ignored.
func (g *GitHub) Refused(ctx context.Context, token string) {
	cur, ok := g.current(token)
	if !ok || (cur.Expiring() && !g.o.Now().Before(cur.ExpiresAt)) {
		return
	}
	g.refuse(ctx, RefusedText)
}

// OK records that a REST call made with token worked (last_ok_at), for the
// sign-in's current token only, as Refused. It never ends a refusal: only a new
// sign-in does (a call answered just before GitHub refused the token can land
// after the refusal).
func (g *GitHub) OK(ctx context.Context, token string) {
	if _, ok := g.current(token); !ok {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if a, err := g.o.DB.GetAccount(ctx, ProviderGitHub); err != nil || a.RefusedAt != "" {
		return
	}
	_ = g.o.DB.AccountOK(ctx, ProviderGitHub, g.o.Now())
}

// current is the sign-in's stored pair and whether token is its access token,
// once a refresh in progress is done.
func (g *GitHub) current(token string) (ghauth.Token, bool) {
	g.refreshMu.Lock()
	cur, _ := g.stored()
	g.refreshMu.Unlock()
	return cur, cur.AccessToken != "" && cur.AccessToken == token
}

func (g *GitHub) refuse(ctx context.Context, why string) {
	if tok, _ := g.stored(); tok.AccessToken == "" {
		return
	}
	if err := g.o.DB.RefuseAccount(context.WithoutCancel(ctx), ProviderGitHub, why, g.o.Now()); err != nil {
		g.o.Log.Warn("accounts: recording GitHub's refusal failed", "err", err)
	}
	g.o.Log.Warn("accounts: GitHub refused the sign-in", "why", why)
	g.o.OnChange()
}

// --- the agent's credential ---

// The agent credential kinds the page sets, as vault names: a claude
// setup-token (a subscription), an Anthropic API key (claude, billed) or an
// OpenAI API key (codex, billed).
const (
	AgentClaudeToken  = secrets.ClaudeOAuthToken
	AgentAnthropicKey = secrets.AnthropicAPIKey
	AgentOpenAIKey    = secrets.OpenAIAPIKey
)

// AgentKinds are the agent credential kinds, in the page's order.
var AgentKinds = []string{AgentClaudeToken, AgentAnthropicKey, AgentOpenAIKey}

// ErrInvalidCredential is a value or kind the page can't store: errors.Is
// matches it, and the error's text is the problem alone (never the value).
var ErrInvalidCredential = errors.New("invalid credential")

type invalidError struct{ reason string }

func (e invalidError) Error() string        { return e.reason }
func (e invalidError) Is(target error) bool { return target == ErrInvalidCredential }

func invalid(format string, args ...any) error {
	return invalidError{fmt.Sprintf(format, args...)}
}

// Agent is the agent's credential in the vault.
type Agent struct {
	vault    secrets.Store
	db       Store
	now      func() time.Time
	onChange func()
}

// NewAgent returns the agent's credential store; onChange runs after a change.
func NewAgent(vault secrets.Store, db Store, now func() time.Time, onChange func()) *Agent {
	if now == nil {
		now = time.Now
	}
	if onChange == nil {
		onChange = func() {}
	}
	return &Agent{vault: vault, db: db, now: now, onChange: onChange}
}

// AgentState is which agent credentials the vault holds (never a value), when
// they last changed (SetAt: a save, or a removal that left others), and when
// each was saved (SetAtByKind: only kinds set, each from its own row; one saved
// before the rows per kind has none).
type AgentState struct {
	Set         map[string]bool   `json:"set"`
	SetAt       string            `json:"set_at"`
	SetAtByKind map[string]string `json:"set_at_by_kind"`
}

// State is the agent's credentials now.
func (a *Agent) State(ctx context.Context) AgentState {
	st := AgentState{Set: map[string]bool{}, SetAtByKind: map[string]string{}}
	for _, k := range AgentKinds {
		ok, _ := a.vault.Present(k)
		st.Set[k] = ok
		if !ok {
			continue
		}
		if acc, err := a.db.GetAccount(ctx, kindProvider(k)); err == nil && acc.SignedInAt != "" {
			st.SetAtByKind[k] = acc.SignedInAt
		}
	}
	if acc, err := a.db.GetAccount(ctx, ProviderAgent); err == nil {
		st.SetAt = acc.SignedInAt
	}
	return st
}

// Set stores a credential of kind (one of AgentKinds). The value is checked
// for shape only, by the rules the runner reads credential.json with: one line
// of printable text, at most agentproto.MaxCredentialValueBytes.
func (a *Agent) Set(ctx context.Context, kind, value string) error {
	if !slices.Contains(AgentKinds, kind) {
		return invalid("unknown credential kind %q", kind)
	}
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return invalid("the credential is empty")
	case len(value) > agentproto.MaxCredentialValueBytes:
		return invalid("the credential is longer than %d characters", agentproto.MaxCredentialValueBytes)
	case !agentproto.PrintableLine(value):
		return invalid("the credential holds a space, a line break or a character a key never has (paste it as one line)")
	}
	if err := a.vault.Set(kind, value); err != nil {
		return err
	}
	now := a.now()
	if err := a.db.SignInAccount(ctx, kindProvider(kind), "", now); err != nil {
		return err
	}
	if err := a.db.SignInAccount(ctx, ProviderAgent, "", now); err != nil {
		return err
	}
	a.onChange()
	return nil
}

// kindProvider is the accounts row that keeps when an agent credential kind was
// set (the agent's own row keeps when any was): the time Credential reports
// moves only when that credential changes, so setting another kind never ends a
// refusal of the one in use.
func kindProvider(kind string) string { return ProviderAgent + "/" + kind }

// Delete removes the credential of kind ("" removes every agent credential).
func (a *Agent) Delete(ctx context.Context, kind string) error {
	kinds := AgentKinds
	if kind != "" {
		if !slices.Contains(AgentKinds, kind) {
			return invalid("unknown credential kind %q", kind)
		}
		kinds = []string{kind}
	}
	for _, k := range kinds {
		if err := a.vault.Delete(k); err != nil {
			return err
		}
		if err := a.db.DeleteAccount(ctx, kindProvider(k)); err != nil {
			return err
		}
	}
	left := slices.ContainsFunc(AgentKinds, func(k string) bool { ok, _ := a.vault.Present(k); return ok })
	if !left {
		if err := a.db.DeleteAccount(ctx, ProviderAgent); err != nil {
			return err
		}
	} else if err := a.db.SignInAccount(ctx, ProviderAgent, "", a.now()); err != nil {
		return err
	}
	a.onChange()
	return nil
}

// cliKinds is which vault credentials each CLI takes, in the order it uses
// them (a subscription token first for claude), with agentproto's kind for each.
var cliKinds = map[string][]struct{ name, kind string }{
	agentproto.CLIClaude: {{AgentClaudeToken, agentproto.CredentialOAuthToken}, {AgentAnthropicKey, agentproto.CredentialAPIKey}},
	agentproto.CLICodex:  {{AgentOpenAIKey, agentproto.CredentialAPIKey}},
}

// KindsFor are the vault credentials cli ("claude" or "codex") takes, in its order.
func KindsFor(cli string) []string {
	var names []string
	for _, k := range cliKinds[cli] {
		names = append(names, k.name)
	}
	return names
}

// Credential is the vault's credential for cli ("claude" or "codex"): its
// agentproto kind, its value and when it was set. ok is false when there is none.
func (a *Agent) Credential(ctx context.Context, cli string) (kind, value string, since time.Time, ok bool) {
	for _, o := range cliKinds[cli] {
		v, err := a.vault.Get(o.name)
		if err != nil || v == "" {
			continue
		}
		// Its own row, else the agent's (a credential set before the rows per kind).
		for _, p := range []string{kindProvider(o.name), ProviderAgent} {
			if acc, err := a.db.GetAccount(ctx, p); err == nil {
				since, _ = time.Parse(time.RFC3339Nano, acc.SignedInAt)
				break
			}
		}
		return o.kind, v, since, true
	}
	return "", "", time.Time{}, false
}
