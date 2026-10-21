package netguard

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error deliberately contains no remote error text or URL.
type Error struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

func (e *Error) Error() string { return e.Message }

type Request struct {
	URL          string
	ETag         string
	LastModified string
	MaxBytes     int64
}
type Response struct {
	Status       int
	Body         []byte
	URL          string
	Permanent    bool
	ETag         string
	LastModified string
	CacheControl string
}

func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Fetch retries only transient failures, enforcing the limit on decoded bytes.
func (c *Client) Fetch(ctx context.Context, in Request) (*Response, error) {
	if !validURL(in.URL) {
		return nil, &Error{Code: "invalid_feed_url", Message: "The feed URL is invalid."}
	}
	if in.MaxBytes <= 0 {
		return nil, &Error{Code: "too_large", Message: "The response size limit is invalid."}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for attempt := 0; ; attempt++ {
		out, err := c.once(ctx, in)
		if err == nil {
			return out, nil
		}
		failure := classify(err)
		if attempt >= 2 || !retryable(failure) || failure.RetryAfterSeconds > 10 || ctx.Err() != nil {
			return nil, failure
		}
		d := []time.Duration{time.Second, 4 * time.Second}[attempt]
		d = time.Duration(float64(d) * (0.8 + rand.Float64()*0.4)) //nolint:gosec // retry jitter is not a credential
		d = max(d, time.Duration(failure.RetryAfterSeconds)*time.Second)
		if err := c.wait(ctx, d); err != nil {
			return nil, classify(err)
		}
	}
}

func (c *Client) once(ctx context.Context, in Request) (*Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// permanent: every redirect followed was a 301 or 308. A response that
	// followed none is no move, even when URL.String spells the URL another
	// way (it re-encodes userinfo that NormalizeFeedURL keeps as given).
	permanent, redirected := true, false
	httpc := &http.Client{Transport: c.transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return &Error{Code: "http_310", Message: "The feed redirected too many times."}
		}
		if !validURL(req.URL.String()) {
			return &Error{Code: "invalid_feed_url", Message: "The feed redirected to an invalid URL."}
		}
		redirected = true
		if req.Response.StatusCode != 301 && req.Response.StatusCode != 308 {
			permanent = false
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
	if err != nil {
		return nil, &Error{Code: "invalid_feed_url", Message: "The feed URL is invalid."}
	}
	req.Header.Set("User-Agent", "AudioSilo-Bridge/"+c.version+" (+https://audiosilo.app)")
	req.Header.Set("Accept-Encoding", "gzip")
	if in.ETag != "" {
		req.Header.Set("If-None-Match", in.ETag)
	}
	if in.LastModified != "" {
		req.Header.Set("If-Modified-Since", in.LastModified)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := &Response{Status: resp.StatusCode, URL: resp.Request.URL.String(), Permanent: redirected && permanent && resp.Request.URL.String() != in.URL && resp.StatusCode == 200, ETag: header(resp.Header.Get("ETag")), LastModified: header(resp.Header.Get("Last-Modified")), CacheControl: resp.Header.Get("Cache-Control")}
	if resp.StatusCode == 304 {
		return out, nil
	}
	if resp.StatusCode != 200 {
		code := "http_" + strconv.Itoa(resp.StatusCode)
		if resp.StatusCode == 410 {
			code = "gone"
		}
		return nil, &Error{Code: code, Message: "The feed returned HTTP " + strconv.Itoa(resp.StatusCode) + ".", RetryAfterSeconds: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	if resp.ContentLength > in.MaxBytes {
		return nil, &Error{Code: "too_large", Message: "The response exceeds the size limit."}
	}
	// Cancel a blocked network read after a gap with no body progress.
	timer := time.AfterFunc(c.stall, cancel)
	defer timer.Stop()
	rd := io.Reader(&progressReader{r: resp.Body, timer: timer, stall: c.stall})
	encoding := strings.TrimSpace(strings.ToLower(resp.Header.Get("Content-Encoding")))
	if encoding == "gzip" {
		gz, e := gzip.NewReader(rd)
		if e != nil {
			if corrupt(e) {
				return nil, &Error{Code: "not_a_feed", Message: "The response has invalid gzip data."}
			}
			return nil, e // the connection's fault (classify): retried
		}
		defer func() { _ = gz.Close() }()
		rd = gz
	} else if encoding != "" && encoding != "identity" {
		return nil, &Error{Code: "not_a_feed", Message: "The response uses an unsupported encoding."}
	}
	out.Body, err = io.ReadAll(io.LimitReader(rd, in.MaxBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Code: "timeout", Message: "The feed stopped responding."}
		}
		if corrupt(err) {
			return nil, &Error{Code: "not_a_feed", Message: "The response has invalid gzip data."}
		}
		return nil, err
	}
	if int64(len(out.Body)) > in.MaxBytes {
		return nil, &Error{Code: "too_large", Message: "The response exceeds the size limit."}
	}
	return out, nil
}

// corrupt reports whether a gzip error is the data's fault (an empty body, a
// bad header, checksum or stream), which fails at once; a read the connection
// broke is retried.
func corrupt(err error) bool {
	var stream flate.CorruptInputError
	return errors.Is(err, io.EOF) || errors.Is(err, gzip.ErrHeader) || errors.Is(err, gzip.ErrChecksum) || errors.As(err, &stream)
}

type progressReader struct {
	r     io.Reader
	timer *time.Timer
	stall time.Duration
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, e := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.stall)
	}
	return n, e
}

func header(s string) string {
	// Validators are persisted by the server. A URL-valued validator could
	// reflect a private request URL back into its database and job results.
	lower := strings.ToLower(s)
	if len(s) > 512 || strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		return ""
	}
	return s
}
func retryAfter(s string, now time.Time) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return min(max(n, 0), 86400)
	}
	if t, err := http.ParseTime(s); err == nil {
		return min(max(int(t.Sub(now).Seconds()), 0), 86400)
	}
	return 0
}
func retryable(e *Error) bool {
	return e.Code == "dns" || e.Code == "timeout" || e.Code == "network" || e.Code == "http_408" || e.Code == "http_429" || strings.HasPrefix(e.Code, "http_5")
}
func classify(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, ErrBlocked) {
		return &Error{Code: "blocked_address", Message: "The feed address is not public or explicitly allowed."}
	}
	var ne net.Error
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &Error{Code: "timeout", Message: "The feed did not respond in time."}
	}
	var cert *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &cert) || errors.As(err, &unknown) {
		return &Error{Code: "tls", Message: "The feed's secure connection could not be verified."}
	}
	return &Error{Code: "network", Message: "The feed could not be reached."}
}
