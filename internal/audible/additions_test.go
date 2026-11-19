package audible

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/testwait"
)

// recordSleeps is a backoff wait that records each wait instead of sleeping.
func recordSleeps(waits *[]time.Duration) Option {
	return withSleep(func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	})
}

func backoffClient(t *testing.T, baseURL string, waits *[]time.Duration) *Client {
	t.Helper()
	keyPEM, _ := pemKey(t, false)
	c, err := NewClient(Credentials{
		ADPToken: "ADP", DevicePrivateKey: keyPEM, DeviceType: deviceType,
		DeviceSerial: "SER", CustomerID: "CUST", Marketplace: "us",
	}, WithEndpoints(Endpoints{API: baseURL}), recordSleeps(waits))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A 429 with Retry-After waits that long and tries again, signing afresh; a 503
// with none backs off exponentially.
func TestBackoffHonoursRetryAfter(t *testing.T) {
	var calls atomic.Int32
	var sigs []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigs = append(sigs, r.Header.Get("x-adp-signature"))
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			_, _ = io.WriteString(w, `{"items":[]}`)
		}
	}))
	defer ts.Close()
	var waits []time.Duration
	if _, err := backoffClient(t, ts.URL, &waits).Library(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] != 2*backoffBase {
		t.Fatalf("waits = %v, want [7s %v]", waits, 2*backoffBase)
	}
	for _, s := range sigs {
		if s == "" {
			t.Fatal("a retried request was not signed")
		}
	}
}

// Denied: a call that keeps answering 429 fails after maxAttempts with the
// status (no endless retry), and a Retry-After past the cap fails at once.
func TestBackoffGivesUp(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	var waits []time.Duration
	_, err := backoffClient(t, ts.URL, &waits).Library(context.Background())
	if HTTPStatus(err) != http.StatusTooManyRequests || int(calls.Load()) != maxAttempts {
		t.Fatalf("err = %v (status %d), calls = %d", err, HTTPStatus(err), calls.Load())
	}

	calls.Store(0)
	long := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer long.Close()
	waits = nil
	_, err = backoffClient(t, long.URL, &waits).Library(context.Background())
	if HTTPStatus(err) != http.StatusServiceUnavailable || calls.Load() != 1 || len(waits) != 0 {
		t.Fatalf("err = %v, calls = %d, waits = %v", err, calls.Load(), waits)
	}
}

// RetryWait says a 429/503 the backoff gave up on is a request to slow down,
// with its Retry-After (a wait past the call's cap included, so a caller that
// backs off for longer honours it); any other answer isn't.
func TestRetryWait(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("asins") {
		case "B000000001":
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
		case "B000000002":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer ts.Close()
	var waits []time.Duration
	c := backoffClient(t, ts.URL, &waits)
	_, err := c.LastPositions(context.Background(), []string{"B000000001"}, nil)
	if wait, ok := RetryWait(err); !ok || wait != time.Hour || len(waits) != 0 {
		t.Fatalf("a 429 asking for an hour: RetryWait = %v, %v (waits %v), err %v", wait, ok, waits, err)
	}
	_, err = c.LastPositions(context.Background(), []string{"B000000002"}, nil)
	if wait, ok := RetryWait(err); !ok || wait != 0 || HTTPStatus(err) != http.StatusServiceUnavailable {
		t.Fatalf("a 503 with no Retry-After: RetryWait = %v, %v, err %v", wait, ok, err)
	}
	_, err = c.LastPositions(context.Background(), []string{"B000000003"}, nil)
	if _, ok := RetryWait(err); ok || !IsRefused(err) {
		t.Fatalf("a 403 is no request to slow down: err %v", err)
	}
	if _, ok := RetryWait(errors.New("network")); ok {
		t.Fatal("a network error is no request to slow down")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"0":                             0,
		"30":                            30 * time.Second,
		"Mon, 02 Nov 2026 12:01:00 GMT": time.Minute,
		"Mon, 02 Nov 2026 11:00:00 GMT": 0, // in the past
	} {
		if got, ok := retryAfter(in, now); !ok || got != want {
			t.Errorf("retryAfter(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "-5", "soon"} {
		if _, ok := retryAfter(in, now); ok {
			t.Errorf("retryAfter(%q) read as a wait", in)
		}
	}
	// A huge value is longer than the Bridge waits, never an overflow into a
	// short or negative wait.
	for _, in := range []string{"9999999999", "18446744074", "9223372036854775807"} {
		if got, ok := retryAfter(in, now); !ok || got <= maxBackoff {
			t.Errorf("retryAfter(%q) = %v, %v; want more than %v", in, got, ok, maxBackoff)
		}
	}
}

func TestRefusedStatus(t *testing.T) {
	for status, want := range map[int]bool{401: true, 403: true, 400: false, 404: false, 500: false} {
		err := error(&httpError{StatusCode: status})
		if IsRefused(err) != want {
			t.Errorf("IsRefused(%d) = %v", status, !want)
		}
	}
	if IsRefused(errors.New("dial tcp: refused")) || HTTPStatus(context.Canceled) != 0 {
		t.Error("a non-HTTP error read as an answer")
	}
}

// The authorize URL follows the endpoints: a fake's sign-in page and its
// maplanding, never Amazon's.
func TestAuthorizeURLEndpoints(t *testing.T) {
	mp, _ := MarketplaceFor("uk")
	u := BuildAuthorizeURL(mp, PKCE{Challenge: "c"}, "SER", WithEndpoints(Endpoints{Amazon: "http://127.0.0.1:9/"}))
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "127.0.0.1:9" || parsed.Path != "/ap/signin" {
		t.Fatalf("authorize URL = %s", u)
	}
	if rt := parsed.Query().Get("openid.return_to"); rt != "http://127.0.0.1:9/ap/maplanding" {
		t.Fatalf("return_to = %q", rt)
	}
	if def := EndpointsFor(mp); def.Amazon != "https://www.amazon.co.uk" || def.AmazonAPI != "https://api.amazon.co.uk" || def.API != "https://api.audible.co.uk" {
		t.Fatalf("EndpointsFor(uk) = %+v", def)
	}
}

// fileServer serves body with http.ServeContent (Range and If-Range against
// etag), recording each request's Range and If-Range; cut > 0 aborts the next
// response after that many bytes, as a dropped connection does.
type fileServer struct {
	*httptest.Server
	body     string
	etag     string
	cut      atomic.Int64
	requests []string // "Range|If-Range" of each request
}

func newFileServer(t *testing.T, body, etag string) *fileServer {
	t.Helper()
	fs := &fileServer{body: body, etag: etag}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.requests = append(fs.requests, r.Header.Get("Range")+"|"+r.Header.Get("If-Range"))
		if fs.etag != "" {
			w.Header().Set("ETag", fs.etag)
		}
		out := w
		if n := fs.cut.Swap(0); n > 0 {
			out = &cutWriter{ResponseWriter: w, left: n}
		}
		http.ServeContent(out, r, "book.aaxc", time.Unix(1_700_000_000, 0), strings.NewReader(fs.body))
	}))
	t.Cleanup(fs.Close)
	return fs
}

// cutWriter aborts the response after left bytes.
type cutWriter struct {
	http.ResponseWriter
	left int64
}

func (c *cutWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		n, _ := c.ResponseWriter.Write(p[:c.left])
		c.ResponseWriter.(http.Flusher).Flush()
		_ = n
		panic(http.ErrAbortHandler)
	}
	c.left -= int64(len(p))
	return c.ResponseWriter.Write(p)
}

func writePartial(t *testing.T, dest, content string, st *partialState) {
	t.Helper()
	if err := os.WriteFile(dest, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if st != nil {
		b, _ := json.Marshal(st)
		if err := os.WriteFile(StatePath(dest), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A download cut part way keeps what arrived and its state; the next one, under
// a new license's URL, resumes with a Range and If-Range and appends; progress
// counts the bytes already held.
func TestDownloadResumesAfterACut(t *testing.T) {
	body := strings.Repeat("0123456789", 3000)
	srv := newFileServer(t, body, `"v1"`)
	srv.cut.Store(10000)
	dest := filepath.Join(t.TempDir(), "acct-B0FAKE0001.aaxc.part")
	cl := testClient(t, srv.URL)
	if err := cl.Download(context.Background(), srv.URL+"/f?sig=one", dest, DownloadOptions{}); err == nil {
		t.Fatal("a cut download must fail")
	}
	if st, err := os.Stat(dest); err != nil || st.Size() == 0 || st.Size() >= int64(len(body)) {
		t.Fatalf("the partial wasn't kept: %v %v", st, err)
	}
	held, _ := os.Stat(dest)
	var last int64
	var hookTotal, hookHeld int64
	err := cl.Download(context.Background(), srv.URL+"/f?sig=two", dest, DownloadOptions{
		ExpectedSize: int64(len(body)),
		OnProgress:   func(copied, _ int64) { last = copied },
		BeforeWrite:  func(total, h int64) error { hookTotal, hookHeld = total, h; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != body {
		t.Fatalf("resumed file has %d bytes, want the whole %d", len(got), len(body))
	}
	want := "bytes=" + strconv.FormatInt(held.Size(), 10) + "-|\"v1\""
	if len(srv.requests) != 2 || srv.requests[1] != want || last != int64(len(body)) {
		t.Fatalf("requests = %v (want %q second), last progress = %d", srv.requests, want, last)
	}
	if hookTotal != int64(len(body)) || hookHeld != held.Size() {
		t.Fatalf("BeforeWrite saw total %d held %d", hookTotal, hookHeld)
	}
}

// Denied: a partial nothing vouches for is never appended to. No state file, a
// state for another size, or a state with neither validator nor size: the file
// starts again without a Range (the last only when the license gives no size
// either: the license's size is proof enough for a 206 of that total).
func TestDownloadUnprovenPartialStartsAgain(t *testing.T) {
	body := strings.Repeat("ab", 700)
	for name, st := range map[string]*partialState{
		"no state":        nil,
		"another size":    {Size: 999, ETag: `"v1"`},
		"nothing to test": {},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newFileServer(t, body, `"v1"`)
			dest := filepath.Join(t.TempDir(), "book.aaxc")
			writePartial(t, dest, "stale-bytes", st)
			expected := int64(len(body))
			if name == "nothing to test" {
				expected = 0
			}
			if err := testClient(t, srv.URL).Download(context.Background(), srv.URL+"/f", dest, DownloadOptions{ExpectedSize: expected}); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(dest); string(got) != body {
				t.Fatalf("file = %d bytes, want %d", len(got), len(body))
			}
			if len(srv.requests) != 1 || srv.requests[0] != "|" {
				t.Fatalf("requests = %v, want one without a Range", srv.requests)
			}
		})
	}
}

// The file changed behind the URL: If-Range doesn't match, the server answers
// 200 with the whole new file, which replaces the partial (one request).
func TestDownloadIfRangeMismatchStartsAgain(t *testing.T) {
	body := strings.Repeat("new!", 400)
	srv := newFileServer(t, body, `"v2"`)
	dest := filepath.Join(t.TempDir(), "book.aaxc")
	writePartial(t, dest, "old-bytes", &partialState{Size: int64(len(body)), ETag: `"v1"`})
	if err := testClient(t, srv.URL).Download(context.Background(), srv.URL+"/f", dest, DownloadOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != body {
		t.Fatalf("file = %q...", string(got)[:12])
	}
	if len(srv.requests) != 1 || srv.requests[0] != `bytes=9-|"v1"` {
		t.Fatalf("requests = %v", srv.requests)
	}
	if st, ok := readState(dest); !ok || st.ETag != `"v2"` {
		t.Fatalf("state = %+v, %v: want the new file's", st, ok)
	}
}

// A 206 that doesn't start where the file ends, or is for a file of another
// size, is not appended: the file starts again.
func TestDownloadRejectsMisplacedRange(t *testing.T) {
	body := strings.Repeat("x", 500)
	for name, cr := range map[string]string{"elsewhere": "bytes 0-499/500", "other size": "bytes 4-499/9999"} {
		t.Run(name, func(t *testing.T) {
			var calls int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Range") != "" {
					w.Header().Set("Content-Range", cr)
					w.WriteHeader(http.StatusPartialContent)
					_, _ = io.WriteString(w, body)
					return
				}
				_, _ = io.WriteString(w, body)
			}))
			defer ts.Close()
			dest := filepath.Join(t.TempDir(), "book.aaxc")
			writePartial(t, dest, "yyyy", &partialState{Size: 500})
			if err := testClient(t, ts.URL).Download(context.Background(), ts.URL+"/f", dest, DownloadOptions{}); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(dest); string(got) != body || calls != 2 {
				t.Fatalf("file = %d bytes (%d calls)", len(got), calls)
			}
		})
	}
}

// A whole file answered 416 with its own (recorded) size is done, not fetched
// again.
func TestDownloadAlreadyWhole(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", "bytes */4")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer ts.Close()
	dest := filepath.Join(t.TempDir(), "book.aaxc")
	writePartial(t, dest, "done", &partialState{Size: 4})
	if err := testClient(t, ts.URL).Download(context.Background(), ts.URL+"/f", dest, DownloadOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "done" {
		t.Fatalf("file = %q", got)
	}
}

// BeforeWrite's refusal (the caller's free-space check) ends the download
// before a byte is written, the partial kept for later.
func TestDownloadBeforeWriteRefuses(t *testing.T) {
	body := strings.Repeat("q", 1000)
	srv := newFileServer(t, body, `"v1"`)
	dest := filepath.Join(t.TempDir(), "book.aaxc")
	writePartial(t, dest, body[:100], &partialState{Size: 1000, ETag: `"v1"`})
	full := errors.New("disk full")
	err := testClient(t, srv.URL).Download(context.Background(), srv.URL+"/f", dest, DownloadOptions{
		BeforeWrite: func(total, held int64) error {
			if total != 1000 || held != 100 {
				t.Errorf("BeforeWrite(%d, %d)", total, held)
			}
			return full
		},
	})
	if !errors.Is(err, full) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != body[:100] {
		t.Fatal("the partial was changed")
	}
}

// RemovePartial removes the file and its state.
func TestRemovePartial(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "book.aaxc")
	writePartial(t, dest, "x", &partialState{Size: 9})
	RemovePartial(dest)
	for _, p := range []string{dest, StatePath(dest)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left", p)
		}
	}
}

// A cancelled download stops and keeps what arrived (the caller decides).
func TestDownloadCancel(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		_, _ = io.WriteString(w, strings.Repeat("c", 10))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	dest := filepath.Join(t.TempDir(), "book.aaxc")
	err := testClient(t, ts.URL).Download(ctx, ts.URL+"/f", dest, DownloadOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Denied: a file past the cap is refused, by its Content-Length before a byte
// is written, or by counting when the server sends none; nothing is kept.
func TestDownloadSizeCap(t *testing.T) {
	body := strings.Repeat("z", 2000)
	withLength := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = io.WriteString(w, body)
	}))
	defer withLength.Close()
	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 4; i++ {
			_, _ = io.WriteString(w, body[:500])
			w.(http.Flusher).Flush()
		}
	}))
	defer chunked.Close()
	for name, ts := range map[string]*httptest.Server{"length": withLength, "chunked": chunked} {
		dest := filepath.Join(t.TempDir(), "book.aaxc")
		err := testClient(t, ts.URL).Download(context.Background(), ts.URL+"/f", dest, DownloadOptions{MaxBytes: 1999})
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("%s: err = %v, want ErrTooLarge", name, err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatalf("%s: an oversized download was kept", name)
		}
	}
	// Allowed: exactly at the cap.
	dest := filepath.Join(t.TempDir(), "book.aaxc")
	if err := testClient(t, chunked.URL).Download(context.Background(), chunked.URL+"/f", dest, DownloadOptions{MaxBytes: 2000}); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
}

// A download error names neither the presigned URL nor its signature.
func TestDownloadErrorHidesURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()
	err := testClient(t, ts.URL).Download(context.Background(), ts.URL+"/f?X-Amz-Signature=SECRETSIG", filepath.Join(t.TempDir(), "x"), DownloadOptions{})
	if err == nil || strings.Contains(err.Error(), "SECRETSIG") || HTTPStatus(err) != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
	// Nor does a request that got no answer (Go's error names the URL).
	gone := httptest.NewServer(http.NotFoundHandler())
	base := gone.URL
	gone.Close()
	err = testClient(t, base).Download(context.Background(), base+"/f.aaxc?Policy=POL&Signature=SECRETSIG&Key-Pair-Id=KP",
		filepath.Join(t.TempDir(), "y"), DownloadOptions{})
	var ue *url.Error
	if err == nil || strings.Contains(err.Error(), "SECRETSIG") || strings.Contains(err.Error(), "Policy") ||
		!errors.As(err, &ue) || !strings.HasSuffix(ue.URL, "/f.aaxc") {
		t.Fatalf("a transport failure = %v", err)
	}
}

// Deregister refreshes an expired access token, then deregisters this device
// only, with the fresh bearer token; neither error nor request leaks a token.
func TestDeregisterRefreshesFirst(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		// The profile's user agent on every identity call (bridge#46).
		if ua := r.Header.Get("User-Agent"); ua != userAgent {
			t.Errorf("%s User-Agent = %q, want %q", r.URL.Path, ua, userAgent)
		}
		switch r.URL.Path {
		case "/auth/token":
			_ = r.ParseForm()
			if r.PostForm.Get("source_token") != "RT" || r.PostForm.Get("requested_token_type") != "access_token" ||
				r.PostForm.Get("app_version") != appVersion {
				t.Errorf("refresh form = %v", r.PostForm)
			}
			_, _ = io.WriteString(w, `{"access_token":"AT2","expires_in":3600}`)
		case "/auth/deregister":
			if got := r.Header.Get("Authorization"); got != "Bearer AT2" {
				t.Errorf("Authorization = %q", got)
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["deregister_all_existing_accounts"] != false {
				t.Errorf("deregister body = %v (must never deregister the account's other devices)", body)
			}
			_, _ = io.WriteString(w, `{"response":{"success":{}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	creds := Credentials{AccessToken: "AT-OLD", RefreshToken: "RT", Marketplace: "us", ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	if err := Deregister(context.Background(), creds, WithEndpoints(Endpoints{AmazonAPI: ts.URL}), noSleep()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/auth/token,/auth/deregister" {
		t.Fatalf("paths = %v", paths)
	}
}

// Denied: an identity refusal is an HTTP error that carries the status and
// Amazon's answer, never the token sent.
func TestDeregisterRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"response":{"error":{"code":"InvalidToken"}}}`)
	}))
	defer ts.Close()
	creds := Credentials{AccessToken: "SECRET-AT", RefreshToken: "SECRET-RT", Marketplace: "us", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	err := Deregister(context.Background(), creds, WithEndpoints(Endpoints{AmazonAPI: ts.URL}), noSleep())
	if HTTPStatus(err) != http.StatusUnauthorized || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
	if _, err := RefreshAccessToken(context.Background(), Credentials{Marketplace: "us"}); err == nil {
		t.Fatal("a refresh with no refresh token should fail before any request")
	}
}

func TestReleaseDay(t *testing.T) {
	for in, want := range map[string]string{"2016-08-01": "2016-08-01", "2016-08-01T00:00:00Z": "2016-08-01",
		"2016-08": "", "2016": "", "": "", "soon": ""} {
		if got := releaseDay(in); got != want {
			t.Errorf("releaseDay(%q) = %q, want %q", in, got, want)
		}
	}
}

// Denied: a partial that can't be removed (a read-only temp folder) and a
// server that answers its Range with a 206 from elsewhere end the download
// with the removal's error, instead of asking for the same Range again and
// again.
func TestDownloadRestartNeedsThePartialGone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only folder")
	}
	var calls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Range", "bytes 0-9/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "0123456789")
	}))
	defer ts.Close()
	dir := filepath.Join(t.TempDir(), "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "book.aaxc")
	writePartial(t, dest, "0123", &partialState{Size: 10})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := testClient(t, ts.URL).Download(context.Background(), ts.URL+"/f", dest, DownloadOptions{})
	if err == nil || calls.Load() > 2 {
		t.Fatalf("err = %v after %d requests", err, calls.Load())
	}
}

// A 200 with no length, cut short, is no whole file: the license's size is
// the file's.
func TestDownloadLengthlessBodyCutShort(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.Repeat("a", 500))
		w.(http.Flusher).Flush() // chunked: no Content-Length
	}))
	defer ts.Close()
	var total int64
	dest := filepath.Join(t.TempDir(), "book.aaxc")
	err := testClient(t, ts.URL).Download(context.Background(), ts.URL+"/f", dest, DownloadOptions{
		ExpectedSize: 2000, BeforeWrite: func(t, _ int64) error { total = t; return nil }})
	if !errors.Is(err, io.ErrUnexpectedEOF) || total != 2000 {
		t.Fatalf("err = %v, BeforeWrite total %d", err, total)
	}
}

// A connection that goes quiet (no answer, or a body that stops) ends the
// attempt with ErrStalled once nothing has arrived for the stall time, keeping
// what arrived; the caller resumes it. The stall times are Scaled: under -race
// on a loaded machine the answer's headers, or a slow body's next bytes, can
// take longer than a stall time sized for a plain build.
func TestDownloadStalls(t *testing.T) {
	stallClient := func(t *testing.T, base string, stall time.Duration) *Client {
		t.Helper()
		keyPEM, _ := pemKey(t, false)
		c, err := NewClient(Credentials{ADPToken: "ADP", DevicePrivateKey: keyPEM, DeviceType: deviceType,
			DeviceSerial: "SER", CustomerID: "CUST", Marketplace: "us"},
			WithEndpoints(Endpoints{API: base}), noSleep(), withStall(stall))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for name, headers := range map[string]bool{"mid-body": true, "no answer": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if headers {
					w.Header().Set("Content-Length", "2000")
					w.Header().Set("ETag", `"v1"`)
					_, _ = io.WriteString(w, strings.Repeat("b", 600))
					w.(http.Flusher).Flush()
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer ts.Close()
			defer close(release)
			dest := filepath.Join(t.TempDir(), "book.aaxc")
			start := time.Now()
			err := stallClient(t, ts.URL, testwait.Scaled(300*time.Millisecond)).Download(context.Background(), ts.URL+"/f", dest, DownloadOptions{ExpectedSize: 2000})
			if !errors.Is(err, ErrStalled) || time.Since(start) > 10*time.Second {
				t.Fatalf("err = %v after %v", err, time.Since(start))
			}
			if got, _ := os.ReadFile(dest); headers && len(got) != 600 {
				t.Fatalf("kept %d bytes, want the 600 that arrived", len(got))
			}
		})
	}
	// A body that keeps coming, however slowly, is no stall: bytes a tenth of
	// the stall time apart, over twice the stall time in all, so only a
	// watchdog every byte resets lets it finish.
	stall := testwait.Scaled(300 * time.Millisecond)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "20")
		for i := range 20 {
			_, _ = io.WriteString(w, strconv.Itoa(i%10))
			w.(http.Flusher).Flush()
			time.Sleep(stall / 10)
		}
	}))
	defer ts.Close()
	dest := filepath.Join(t.TempDir(), "slow.aaxc")
	if err := stallClient(t, ts.URL, stall).Download(context.Background(), ts.URL+"/f", dest, DownloadOptions{}); err != nil {
		t.Fatalf("a slow body: %v", err)
	}
}

// A download URL that doesn't parse, or a redirect to one, is named in Go's
// error with its query: neither the request's URL nor a Location's signature
// reaches the error.
func TestDownloadErrorHidesAnUnparsableURL(t *testing.T) {
	err := testClient(t, "http://127.0.0.1:1").Download(context.Background(),
		"https://cdn.example/f%zz.aaxc?Policy=POL&Signature=SECRETSIG&Key-Pair-Id=KP", filepath.Join(t.TempDir(), "x"), DownloadOptions{})
	if err == nil || strings.Contains(err.Error(), "SECRETSIG") || strings.Contains(err.Error(), "Policy") {
		t.Fatalf("an unparsable URL: %v", err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://cdn.example/f%zz.aaxc?Signature=SECRETSIG2")
		w.WriteHeader(http.StatusFound)
	}))
	defer ts.Close()
	err = testClient(t, ts.URL).Download(context.Background(), ts.URL+"/cds", filepath.Join(t.TempDir(), "y"), DownloadOptions{})
	if err == nil || strings.Contains(err.Error(), "SECRETSIG2") {
		t.Fatalf("an unparsable Location: %v", err)
	}
}

// A title's file name is never hidden (a dot name the server never indexes),
// never empty, and has no separator or character an SMB share refuses.
func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"...And Ladies of the Club": "And Ladies of the Club",
		".hack//Sign":               "hack--Sign",
		"Re: Zero. ":                "Re - Zero",
		" . ":                       "audiobook",
		"Who Moved My Cheese?":      "Who Moved My Cheese",
		"?":                         "audiobook",
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
