package audible

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/timeutil"
)

// Backoff for an answer that asks the caller to slow down (429 Too Many Requests,
// 503 Service Unavailable): Audible's rate limits are undocumented, so every call
// waits and tries again a few times rather than failing a whole poll.
const (
	// maxAttempts is how many times one call is sent before its 429/503 is the
	// answer.
	maxAttempts = 4
	// backoffBase is the first wait when the answer has no Retry-After; each
	// later wait doubles it.
	backoffBase = 2 * time.Second
	// maxBackoff caps one wait, Retry-After included: a server asking for an hour
	// fails the call instead of holding a job that long.
	maxBackoff = 2 * time.Minute
)

// retryable reports whether status asks the caller to back off and try again.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// errBackoffTooLong is the answer when Retry-After asks for more than maxBackoff.
var errBackoffTooLong = errors.New("audible asked to wait longer than the Bridge will")

// sendWithBackoff sends the request newReq builds (a fresh one per attempt, so a
// signature carries its own date) under the profile's user agent (Go's default
// one names no app the device registered as), and returns the first answer
// that isn't a 429/503, or the last one. A 429/503 body is drained and closed
// before the wait.
func sendWithBackoff(ctx context.Context, hc *http.Client, sleep func(context.Context, time.Duration) error,
	newReq func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, withoutQuery(err) // a URL that doesn't parse is named in the error
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, withoutQuery(err)
		}
		if !retryable(resp.StatusCode) || attempt == maxAttempts {
			return resp, nil
		}
		wait, ok := timeutil.RetryAfter(resp.Header.Get("Retry-After"), time.Now(), timeutil.Audible)
		if !ok {
			wait = backoffBase << (attempt - 1)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if wait > maxBackoff {
			return nil, &httpError{StatusCode: resp.StatusCode, Method: req.Method, Path: req.URL.Path, Body: errBackoffTooLong.Error(),
				RetryAfter: wait}
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// withoutQuery drops the query (and any userinfo) from the URL a failed
// request's error names: a download's is presigned (its Policy, Signature and
// Key-Pair-Id are the right to fetch the file), and errors are logged and
// reported. A cause that quotes a URL itself (a redirect's Location that
// doesn't parse) is left out whole.
func withoutQuery(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	inner := ue.Err
	if inner != nil && (strings.Contains(inner.Error(), "://") || strings.Contains(inner.Error(), "?")) {
		inner = errURLInCause
	}
	u, perr := url.Parse(ue.URL)
	if perr != nil {
		return &url.Error{Op: ue.Op, URL: "(the request's URL)", Err: inner}
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.User = "", false, "", nil
	return &url.Error{Op: ue.Op, URL: u.String(), Err: inner}
}

// errURLInCause stands in for a request error's cause that quoted a URL.
var errURLInCause = errors.New("the request failed (its error quoted a URL, left out)")

// HTTPStatus is the Audible or Amazon status code behind err, or 0 when err
// isn't an HTTP answer (a network failure, a cancelled context).
func HTTPStatus(err error) int {
	var he *httpError
	if errors.As(err, &he) {
		return he.StatusCode
	}
	return 0
}

// RetryWait reports whether err is Audible asking the caller to slow down (a
// 429 or 503 the backoff gave up on) and how long it asked to wait: its
// Retry-After, 0 when it sent none.
func RetryWait(err error) (time.Duration, bool) {
	var he *httpError
	if !errors.As(err, &he) || !retryable(he.StatusCode) {
		return 0, false
	}
	return he.RetryAfter, true
}

// IsRefused reports whether err is Audible refusing the device's credentials
// (401 or 403 on a signed call): the account must sign in again.
func IsRefused(err error) bool {
	s := HTTPStatus(err)
	return s == http.StatusUnauthorized || s == http.StatusForbidden
}
