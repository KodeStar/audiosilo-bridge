// Package ghauth is the Bridge's GitHub App user sign-in (BRIDGE-PLAN.md Phase
// 2b): the device flow (a code the admin types at github.com/login/device, polled
// until they approve it), the refresh of an expiring user token with only the
// client ID (a device-flow token needs no client secret), the revoke of a token
// pair at sign-out (POST /credentials/revoke, unauthenticated) and the signed-in
// user's login. A leaf: standard library only.
//
// Every URL is injectable (Client.WebBase, Client.APIBase) so the tests drive a
// fake GitHub. Tokens never appear in an error: GitHub's error answers carry an
// error code and a description, never the credential.
package ghauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultClientID is the AudioSilo contributions GitHub App's client ID (App id
// AppID, slug AppSlug; a public App, device flow on, user tokens expiring).
// github.client_id (AUDIOSILO_BRIDGE_GITHUB_CLIENT_ID) overrides it; an empty
// client ID makes sign-in unavailable.
const DefaultClientID = "Iv23likw0zS13hG4spdo"

// AppID and AppSlug name the App: meta's routing workflow matches an issue's
// performed_via_github_app.id against AppID.
const (
	AppID   = 5387686
	AppSlug = "audiosilo-contributions"
)

// Default bases: the web host (device codes, tokens) and the REST API.
const (
	DefaultWebBase = "https://github.com"
	DefaultAPIBase = "https://api.github.com"
)

// VerificationURI is where the admin types the code (GitHub's own answer is used
// when it sends one).
const VerificationURI = "https://github.com/login/device"

// AuthorizationsURL is where a user removes the App's authorization for good
// (only they can: the Bridge's revoke ends its tokens).
const AuthorizationsURL = "https://github.com/settings/apps/authorizations"

// The device flow's poll interval floor and slow_down step (GitHub's numbers).
const (
	defaultInterval = 5 * time.Second
	slowDownStep    = 5 * time.Second
)

// requestTimeout bounds one call.
const requestTimeout = 20 * time.Second

// maxBody bounds what is read of an answer.
const maxBody = 1 << 20

// The device flow's outcomes that aren't a token.
var (
	// ErrPending: the admin hasn't entered the code yet (keep polling).
	ErrPending = errors.New("ghauth: authorization pending")
	// ErrSlowDown: polling too fast (add 5 s to the interval).
	ErrSlowDown = errors.New("ghauth: slow down")
	// ErrExpired: the code expired before it was entered (request a new one).
	ErrExpired = errors.New("the code expired before it was entered on GitHub: start the sign-in again")
	// ErrDenied: the admin cancelled on GitHub.
	ErrDenied = errors.New("the sign-in was cancelled on GitHub")
	// ErrBadRefresh: the refresh token is no longer valid (sign in again).
	ErrBadRefresh = errors.New("GitHub refused the sign-in's refresh token: sign in to GitHub again")
	// ErrUnauthorized: GitHub refused the access token (a 401).
	ErrUnauthorized = errors.New("GitHub refused the sign-in")
	// ErrNoClientID: no client ID is configured, so there is no App to sign in to.
	ErrNoClientID = errors.New("GitHub sign-in is unavailable: set github.client_id (AUDIOSILO_BRIDGE_GITHUB_CLIENT_ID)")
)

// Error is any other error GitHub answered (its code and description).
type Error struct {
	Code        string
	Description string
	Status      int
}

func (e *Error) Error() string {
	msg := "GitHub answered"
	if e.Status != 0 && e.Status != http.StatusOK {
		msg += fmt.Sprintf(" %d", e.Status)
	}
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Description != "" {
		msg += ": " + e.Description
	}
	return msg
}

// Client calls GitHub for one App.
type Client struct {
	ClientID string
	// WebBase is github.com (device codes and tokens), APIBase api.github.com
	// (revoke, user); "" takes the defaults.
	WebBase string
	APIBase string
	HTTP    *http.Client
	// Now is the clock the token expiries are computed with (time.Now when nil).
	Now func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) web() string { return base(c.WebBase, DefaultWebBase) }
func (c *Client) api() string { return base(c.APIBase, DefaultAPIBase) }

func base(v, def string) string {
	if v = strings.TrimRight(strings.TrimSpace(v), "/"); v != "" {
		return v
	}
	return def
}

func (c *Client) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// DeviceCode is a device flow in progress: the code the admin types at
// VerificationURI, and the device code the Bridge polls with (never shown).
type DeviceCode struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
	Interval        time.Duration
}

// Token is a user access token, with its refresh token and expiries when the App
// expires its tokens (zero times otherwise).
type Token struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitzero"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitzero"`
}

// Expiring reports whether the token expires (it carries a refresh token).
func (t Token) Expiring() bool { return t.RefreshToken != "" && !t.ExpiresAt.IsZero() }

// RequestCode starts a device flow (POST /login/device/code).
func (c *Client) RequestCode(ctx context.Context) (DeviceCode, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return DeviceCode{}, ErrNoClientID
	}
	var out struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		oauthError
	}
	if err := c.postForm(ctx, c.web()+"/login/device/code", url.Values{"client_id": {c.ClientID}}, &out); err != nil {
		return DeviceCode{}, err
	}
	if err := out.err(); err != nil {
		return DeviceCode{}, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return DeviceCode{}, errors.New("GitHub's device code answer carried no code")
	}
	dc := DeviceCode{DeviceCode: out.DeviceCode, UserCode: out.UserCode, VerificationURI: out.VerificationURI,
		Interval: time.Duration(out.Interval) * time.Second}
	if dc.VerificationURI == "" || !strings.HasPrefix(dc.VerificationURI, "https://") {
		dc.VerificationURI = VerificationURI
	}
	if dc.Interval < defaultInterval {
		dc.Interval = defaultInterval
	}
	expires := time.Duration(out.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = 15 * time.Minute
	}
	dc.ExpiresAt = c.now().Add(expires)
	return dc, nil
}

// Poll asks once whether the admin approved the code: a Token, or ErrPending,
// ErrSlowDown (the interval to use next is returned), ErrExpired, ErrDenied or
// another error.
func (c *Client) Poll(ctx context.Context, deviceCode string) (Token, time.Duration, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return Token{}, 0, ErrNoClientID
	}
	form := url.Values{"client_id": {c.ClientID}, "device_code": {deviceCode},
		"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
	var out tokenAnswer
	if err := c.postForm(ctx, c.web()+"/login/oauth/access_token", form, &out); err != nil {
		return Token{}, 0, err
	}
	switch out.Error {
	case "":
	case "authorization_pending":
		return Token{}, 0, ErrPending
	case "slow_down":
		return Token{}, time.Duration(out.Interval) * time.Second, ErrSlowDown
	case "expired_token", "token_expired":
		return Token{}, 0, ErrExpired
	case "access_denied":
		return Token{}, 0, ErrDenied
	default:
		return Token{}, 0, out.err()
	}
	tok, err := out.token(c.now())
	return tok, 0, err
}

// Wait polls until the admin approves the code (a Token), the code expires
// (ErrExpired), they cancel (ErrDenied), another error, or ctx ends. A poll that
// gets no answer (GitHub unreachable, a server error, a rate limit) doesn't end
// the flow: the code still stands, so it polls on, slower (RFC 8628 section
// 3.5), until the code expires. sleep waits between polls (a test passes one
// that doesn't).
func (c *Client) Wait(ctx context.Context, dc DeviceCode, sleep func(context.Context, time.Duration) error) (Token, error) {
	if sleep == nil {
		sleep = sleepCtx
	}
	interval := max(dc.Interval, defaultInterval)
	for {
		if !dc.ExpiresAt.IsZero() && !c.now().Before(dc.ExpiresAt) {
			return Token{}, ErrExpired
		}
		if err := sleep(ctx, interval); err != nil {
			return Token{}, err
		}
		tok, next, err := c.Poll(ctx, dc.DeviceCode)
		switch {
		case err == nil:
			return tok, nil
		case errors.Is(err, ErrPending):
		case errors.Is(err, ErrSlowDown):
			// GitHub sends the new interval; a missing one is the old plus 5 s.
			interval = max(next, interval+slowDownStep)
		case ctx.Err() == nil && unanswered(err):
			interval += slowDownStep
		default:
			return Token{}, err
		}
	}
}

// unanswered reports whether a poll's err is GitHub not answering it: it
// couldn't be reached (or timed out), or answered a server error or a rate
// limit without an OAuth error code.
func unanswered(err error) bool {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.Code == "" && (ge.Status >= http.StatusInternalServerError || ge.Status == http.StatusTooManyRequests)
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// Refresh trades a refresh token for a new pair (grant_type refresh_token, the
// client ID only: a device-flow token needs no secret). The old pair stops
// working (the old access token answers 401 at once), so the caller persists the
// new one before using it, and refreshes one at a time. A refresh GitHub
// answers with any error is ErrBadRefresh.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Token, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return Token{}, ErrNoClientID
	}
	form := url.Values{"client_id": {c.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
	var out tokenAnswer
	if err := c.postForm(ctx, c.web()+"/login/oauth/access_token", form, &out); err != nil {
		return Token{}, err
	}
	if out.Error != "" {
		// Whatever the code (bad_refresh_token, or incorrect_client_credentials for
		// a refresh token already spent, as GitHub answers live), the pair is no
		// good: the admin signs in again.
		return Token{}, fmt.Errorf("%w (%s)", ErrBadRefresh, out.Error)
	}
	tok, err := out.token(c.now())
	return tok, err
}

// Revoke ends the given tokens (POST /credentials/revoke, no authentication:
// it takes ghu_ and ghr_ tokens). GitHub emails the owner about it.
func (c *Client) Revoke(ctx context.Context, tokens ...string) error {
	creds := []string{}
	for _, t := range tokens {
		if t = strings.TrimSpace(t); t != "" {
			creds = append(creds, t)
		}
	}
	if len(creds) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string][]string{"credentials": creds})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api()+"/credentials/revoke", bytes.NewReader(body))
	if err != nil {
		return err
	}
	apiHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc().Do(req)
	if err != nil {
		return fmt.Errorf("revoke the GitHub sign-in: %w", stripURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Status: resp.StatusCode, Description: snippet(raw)}
	}
	return nil
}

// User is the signed-in GitHub user.
type User struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// User returns the token's user (GET /user, no permission needed). A refused
// token is ErrUnauthorized.
func (c *Client) User(ctx context.Context, token string) (User, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api()+"/user", nil)
	if err != nil {
		return User{}, err
	}
	apiHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpc().Do(req)
	if err != nil {
		return User{}, fmt.Errorf("read the GitHub user: %w", stripURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return User{}, ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return User{}, &Error{Status: resp.StatusCode, Description: snippet(raw)}
	}
	var u User
	if err := json.Unmarshal(raw, &u); err != nil || u.Login == "" {
		return User{}, errors.New("GitHub's user answer carried no login")
	}
	return u, nil
}

// oauthError is GitHub's error shape on the OAuth endpoints (HTTP 200 with an
// error field, for the device flow's states too).
type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (e oauthError) err() error {
	if e.Error == "" {
		return nil
	}
	return &Error{Code: e.Error, Description: e.ErrorDescription}
}

// tokenAnswer is the access_token endpoint's answer.
type tokenAnswer struct {
	AccessToken           string `json:"access_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Interval              int    `json:"interval"`
	oauthError
}

func (a tokenAnswer) token(now time.Time) (Token, error) {
	if a.AccessToken == "" {
		return Token{}, errors.New("GitHub's token answer carried no token")
	}
	t := Token{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken}
	if a.ExpiresIn > 0 {
		t.ExpiresAt = now.Add(time.Duration(a.ExpiresIn) * time.Second)
	}
	if a.RefreshTokenExpiresIn > 0 {
		t.RefreshExpiresAt = now.Add(time.Duration(a.RefreshTokenExpiresIn) * time.Second)
	}
	return t, nil
}

// postForm posts a form to an OAuth endpoint and decodes the JSON answer (GitHub
// answers form-encoded without Accept: application/json). A non-2xx answer is
// decoded too when it carries an OAuth error.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "audiosilo-bridge")
	resp, err := c.httpc().Do(req)
	if err != nil {
		return fmt.Errorf("reach GitHub: %w", stripURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var oe oauthError
		if json.Unmarshal(raw, &oe) == nil && oe.Error != "" {
			// The token endpoint's error states (the refresh's bad_refresh_token
			// among them) may come with a 4xx: decode them for the caller.
			return json.Unmarshal(raw, out)
		}
		return &Error{Status: resp.StatusCode, Description: snippet(raw)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("GitHub's answer isn't JSON: %w", err)
	}
	return nil
}

func apiHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "audiosilo-bridge")
}

// stripURL drops the request URL from a transport error: a device code or a
// token never rides in a URL here, but the error stays short.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// snippet is a short, single-line piece of an answer for an error.
func snippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "")
	}
	return s
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
