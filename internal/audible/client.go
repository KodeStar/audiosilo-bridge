package audible

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/timeutil"
)

// Client is an authenticated Audible API client for one registered device. Calls
// to the unrestricted endpoints (library, license, stats) are RSA-signed with the
// device key - see signRequest.
type Client struct {
	creds Credentials
	key   *rsa.PrivateKey
	mp    Marketplace
	set   settings
	now   func() time.Time
}

// apiTimeout bounds one API call (a download has none; its context governs it).
const apiTimeout = 90 * time.Second

// NewClient builds a client from stored credentials. The options point it at
// other endpoints or another HTTP client (tests, the fake).
func NewClient(creds Credentials, opts ...Option) (*Client, error) {
	key, err := creds.PrivateKey()
	if err != nil {
		return nil, err
	}
	mp, ok := MarketplaceFor(creds.Marketplace)
	if !ok {
		return nil, fmt.Errorf("unknown marketplace %q", creds.Marketplace)
	}
	return &Client{
		creds: creds,
		key:   key,
		mp:    mp,
		set:   resolve(mp, apiTimeout, opts),
		now:   time.Now,
	}, nil
}

// doSigned issues an RSA-signed request and decodes a JSON response into out. A
// 429/503 is retried with backoff (sendWithBackoff), each attempt signed afresh.
func (c *Client) doSigned(ctx context.Context, method, path string, body []byte, out any) error {
	resp, err := sendWithBackoff(ctx, c.set.http, c.set.sleep, func() (*http.Request, error) {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.set.endpoints.API+path, rdr)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := signRequest(req, body, c.key, c.creds.ADPToken, c.now()); err != nil {
			return nil, err
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		he := &httpError{StatusCode: resp.StatusCode, Method: method, Path: pathOnly(path), Body: snippet(data)}
		if retryable(resp.StatusCode) {
			he.RetryAfter, _ = timeutil.RetryAfter(resp.Header.Get("Retry-After"), time.Now(), timeutil.Audible)
		}
		return he
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// maxAPIBody bounds one API answer read into memory. A 1000-title library page
// with every response group is a few MB; 64 MiB is far past any real one.
const maxAPIBody = 64 << 20

// pathOnly drops a query (ASIN lists, paging) from a path for an error message.
func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// httpError is a non-2xx response from the Audible API. It carries the status code
// so callers can react to it (e.g. split an over-large batch on a 400) instead of
// string-matching the message.
type httpError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string
	// RetryAfter is a 429/503's Retry-After (0 when it sent none, or another
	// status): how long Audible asked the caller to wait.
	RetryAfter time.Duration
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// snippet is the start of an Amazon answer for an error, its account
// identifiers redacted (RedactAccountIDs) before it is cut, so a cut never
// leaves part of an id behind.
func snippet(b []byte) string {
	const max = 200
	// Redact a bounded head: an error body is small, but the read allows 64 MiB.
	const head = 4096
	if len(b) > head {
		b = b[:head]
	}
	s := RedactAccountIDs(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
