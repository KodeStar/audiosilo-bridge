package contrib

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/accounts"
)

// ErrNoCredential is returned by TokenSource.Resolve when no GitHub credential
// resolves: no AUDIOSILO_BRIDGE_GITHUB_TOKEN, no GitHub sign-in (or one GitHub
// refused) and no `gh auth token`. It is a sentinel so callers can errors.Is it
// (the issue contribution mode parks the book on this; unauthenticated public
// polling ignores it).
var ErrNoCredential = errors.New("contrib: no GitHub credential (sign in to GitHub on the Bridge's Accounts tab)")

// ErrSignInUnrefreshed is the ErrNoCredential (errors.Is matches both) of a
// Bridge that is signed in to GitHub but whose expired sign-in couldn't be
// refreshed because GitHub couldn't be reached (accounts.ErrUnrefreshed), with
// nothing else to fall back on. Waiting until GitHub can be reached ends it, not
// a sign-in, so callers word it apart.
var ErrSignInUnrefreshed = fmt.Errorf("%w: the sign-in couldn't be refreshed (GitHub couldn't be reached)", ErrNoCredential)

// GitHubTokenEnv names the variable that gives the Bridge a GitHub token for
// headless setups. It is read live at every Resolve (never sealed), and wins
// over the Accounts tab's sign-in.
const GitHubTokenEnv = "AUDIOSILO_BRIDGE_GITHUB_TOKEN"

// from values reported alongside a resolved token.
const (
	FromEnv    = "env"
	FromDevice = "device"
	FromGH     = "gh"
)

// ghTokenTimeout bounds the `gh auth token` shell-out.
const ghTokenTimeout = 15 * time.Second

// ghReuseFor is how long a `gh auth token` answer (a token, or none) is reused:
// Resolve runs at every heartbeat and every Status poll of the page, and gh is a
// process.
const ghReuseFor = time.Minute

// DeviceTokens is the Accounts tab's GitHub sign-in (*accounts.GitHub): its
// access token (refreshed when it nears its expiry; an error when there is no
// usable sign-in), Refused to record a 401 and OK a call that worked.
type DeviceTokens interface {
	Token(ctx context.Context) (string, error)
	Refused(ctx context.Context, token string)
	OK(ctx context.Context, token string)
}

// TokenSource resolves a GitHub credential for the REST client: the
// environment's AUDIOSILO_BRIDGE_GITHUB_TOKEN (read live), then the device-flow
// sign-in, then the GitHub CLI (`gh auth token`). A token GitHub refused (a 401,
// Refused) is skipped until it changes. The resolved token is never logged or
// placed in argv.
type TokenSource struct {
	device DeviceTokens
	// getenv reads the environment; ghToken resolves a token via the GitHub CLI.
	// Both are fields so tests can substitute fakes.
	getenv  func(string) string
	ghToken func(ctx context.Context) (string, error)
	now     func() time.Time
	// onChange runs after a refusal (the heartbeat's can_contribute drops).
	onChange func()

	mu sync.Mutex
	// refused holds the fingerprints of the environment and gh tokens GitHub
	// refused (the device sign-in records its own refusal).
	refused map[string]bool
	// gh is the last `gh auth token` answer ("" for none) and when it came.
	gh   string
	ghAt time.Time
}

// NewTokenSource returns a TokenSource over device (nil: no sign-in), calling
// onChange (nil: nothing) after GitHub refuses a token.
func NewTokenSource(device DeviceTokens, onChange func()) *TokenSource {
	if onChange == nil {
		onChange = func() {}
	}
	ts := &TokenSource{device: device, getenv: os.Getenv, now: time.Now, onChange: onChange, refused: map[string]bool{}}
	ts.ghToken = func(ctx context.Context) (string, error) {
		ghPath, err := exec.LookPath("gh")
		if err != nil {
			return "", fmt.Errorf("gh CLI not found on PATH: %w", err)
		}
		return ghAuthTokenExec(ctx, ghPath)
	}
	return ts
}

// Resolve returns (token, from, nil) where from is FromEnv, FromDevice or
// FromGH, trying them in that order. When none yields a usable token it returns
// ("", "", ErrNoCredential), or ErrSignInUnrefreshed when the sign-in is there
// but GitHub couldn't be reached to refresh it. The error path never carries a
// token.
func (t *TokenSource) Resolve(ctx context.Context) (token, from string, err error) {
	if v := strings.TrimSpace(t.getenv(GitHubTokenEnv)); v != "" && !t.isRefused(v) {
		return v, FromEnv, nil
	}
	unrefreshed := false
	if t.device != nil {
		tok, derr := t.device.Token(ctx)
		if derr == nil && tok != "" {
			return tok, FromDevice, nil
		}
		unrefreshed = errors.Is(derr, accounts.ErrUnrefreshed)
	}
	if tok := t.ghCLIToken(ctx); tok != "" && !t.isRefused(tok) {
		return tok, FromGH, nil
	}
	if unrefreshed {
		return "", "", ErrSignInUnrefreshed
	}
	return "", "", ErrNoCredential
}

// ghCLIToken is `gh auth token`'s answer ("" for none), reused for ghReuseFor.
// An answer cut short by ctx isn't kept.
func (t *TokenSource) ghCLIToken(ctx context.Context) string {
	if t.ghToken == nil {
		return ""
	}
	t.mu.Lock()
	if !t.ghAt.IsZero() && t.now().Sub(t.ghAt) < ghReuseFor {
		tok := t.gh
		t.mu.Unlock()
		return tok
	}
	t.mu.Unlock()
	tok, err := t.ghToken(ctx)
	if err != nil {
		tok = ""
	}
	if ctx.Err() == nil {
		t.mu.Lock()
		t.gh, t.ghAt = tok, t.now()
		t.mu.Unlock()
	}
	return tok
}

// Refused records that GitHub refused token (resolved from from) with a 401:
// the device sign-in is marked refused (until a new sign-in), an environment or
// gh token is skipped until it changes.
func (t *TokenSource) Refused(ctx context.Context, from, token string) {
	if from == FromDevice {
		if t.device != nil {
			t.device.Refused(ctx, token)
		}
		return
	}
	t.mu.Lock()
	t.refused[fingerprint(token)] = true
	t.ghAt = time.Time{} // ask gh again: it may hold a new token by now
	t.mu.Unlock()
	t.onChange()
}

// OK records that a call made with token (resolved from from) worked: the
// device sign-in keeps when it last did (the Accounts tab's last_ok_at); an
// environment or gh token has no record.
func (t *TokenSource) OK(ctx context.Context, from, token string) {
	if from == FromDevice && t.device != nil {
		t.device.OK(ctx, token)
	}
}

func (t *TokenSource) isRefused(token string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refused[fingerprint(token)]
}

// fingerprint is a token's sha256: only it is kept for a refusal.
func fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Refuser is a TokenResolver that records a 401 (*TokenSource).
type Refuser interface {
	Refused(ctx context.Context, from, token string)
}

// Confirmer is a TokenResolver that records a call that worked (*TokenSource).
type Confirmer interface {
	OK(ctx context.Context, from, token string)
}

// NewClientFor is NewClient for a token resolved by r: when r records refusals,
// a 401 on any call reports the token as refused (GitHub revoked or expired it);
// when it records calls that worked, a call answered as wanted reports that.
func NewClientFor(baseURL, token, from string, r TokenResolver) *Client {
	c := NewClient(baseURL, token)
	c.from = from
	if token == "" {
		return c
	}
	if rf, ok := r.(Refuser); ok {
		c.onUnauthorized = func(ctx context.Context) { rf.Refused(ctx, from, token) }
	}
	if cf, ok := r.(Confirmer); ok {
		c.onOK = func(ctx context.Context) { cf.OK(ctx, from, token) }
	}
	return c
}

// ghAuthTokenExec runs `<ghPath> auth token` (argv-only, no shell) with a short
// timeout and returns the trimmed stdout. On any failure it returns an error
// that NEVER includes stdout: `gh auth token` prints the token to stdout, so
// leaking stdout into an error would defeat the whole point. stderr (which gh
// does not use for the token) is included, trimmed, for debuggability.
func ghAuthTokenExec(ctx context.Context, ghPath string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, ghTokenTimeout)
	defer cancel()

	// argv-only exec (no shell), so there is no injection surface. The token
	// rides on stdout; we never place it in argv or an error.
	cmd := exec.CommandContext(runCtx, ghPath, "auth", "token")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if runErr := cmd.Run(); runErr != nil {
		if runCtx.Err() != nil && ctx.Err() == nil {
			return "", fmt.Errorf("gh auth token timed out after %s", ghTokenTimeout)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if msg := trimStderr(errBuf.String()); msg != "" {
			return "", fmt.Errorf("gh auth token failed: %s", msg)
		}
		// runErr is an *exec.ExitError whose Error() is "exit status N" - it does
		// not carry the child's output, so it cannot leak the token.
		return "", fmt.Errorf("gh auth token failed: %w", runErr)
	}
	return strings.TrimSpace(out.String()), nil
}

// trimStderr trims and caps a stderr snippet for inclusion in an error. gh never
// writes the token to stderr, so this is safe; the cap keeps errors small.
func trimStderr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
