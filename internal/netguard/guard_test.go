package netguard

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}
func localClient(t *testing.T) *Client {
	t.Helper()
	c := New(Options{AllowNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, Version: "0.9.0"})
	c.wait = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(c.Close)
	return c
}
func errorCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("got %v, want %s", err, want)
	}
}

func TestDeniedAddressMatrix(t *testing.T) {
	t.Parallel()
	denied := []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.0.1", "169.254.169.254", "100.64.0.1", "fc00::1", "64:ff9b::7f00:1", "0.0.0.0", "224.0.0.1", "fe80::1", "2002:7f00:1::1", "::7f00:1", "fec0::1", "192.0.0.1", "198.18.0.1"}
	for _, s := range denied {
		t.Run(s, func(t *testing.T) {
			c := New(Options{})
			defer c.Close()
			if c.allowed(netip.MustParseAddr(s)) {
				t.Fatal("allowed non-public address")
			}
			if !errors.Is(c.control("tcp", netip.AddrPortFrom(netip.MustParseAddr(s), 80).String(), nil), ErrBlocked) {
				t.Fatal("Control did not block")
			}
		})
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "64:ff9b::808:808"} {
		if !New(Options{}).allowed(netip.MustParseAddr(s)) {
			t.Errorf("public address denied: %s", s)
		}
	}
	c := New(Options{Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})})
	defer c.Close()
	_, err := c.Fetch(context.Background(), Request{URL: "http://public.example/feed?auth=PRIVATE123", MaxBytes: 100})
	errorCode(t, err, "blocked_address")
	if strings.Contains(err.Error(), "PRIVATE123") {
		t.Fatal("credential leaked")
	}
}
func TestInjectedPublicResolverAndAllowList(t *testing.T) {
	t.Parallel()
	resolved := false
	c := New(Options{Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		resolved = true
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	})})
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.dial(ctx, "tcp", "public.example:80")
	if !resolved || errors.Is(err, ErrBlocked) {
		t.Fatalf("public DNS answer denied: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("User-Agent") != "AudioSilo-Bridge/0.9.0 (+https://audiosilo.app)" || r.Header.Get("If-None-Match") != "abc" || r.Header.Get("If-Modified-Since") != "yesterday" {
			t.Error("request contract", r.Header)
		}
		fmt.Fprint(w, "feed")
	}))
	defer server.Close()
	c = localClient(t)
	out, err := c.Fetch(context.Background(), Request{URL: server.URL, ETag: "abc", LastModified: "yesterday", MaxBytes: 100})
	if err != nil || string(out.Body) != "feed" {
		t.Fatalf("listed CIDR refused: %v", err)
	}
	denied := New(Options{})
	defer denied.Close()
	_, err = denied.Fetch(context.Background(), Request{URL: server.URL, MaxBytes: 100})
	errorCode(t, err, "blocked_address")
}
func TestRedirectGuardAndLimit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/private":
			http.Redirect(w, r, "http://169.254.169.254/feed", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusMovedPermanently)
		case "/permanent":
			http.Redirect(w, r, "/feed", http.StatusPermanentRedirect)
		case "/temporary":
			http.Redirect(w, r, "/feed", http.StatusFound)
		case "/mixed":
			http.Redirect(w, r, "/temporary", http.StatusMovedPermanently)
		case "/file":
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		default:
			fmt.Fprint(w, "feed")
		}
	}))
	defer server.Close()
	c := localClient(t)
	for endpoint, code := range map[string]string{"/private": "blocked_address", "/loop": "http_310", "/file": "invalid_feed_url"} {
		_, err := c.Fetch(context.Background(), Request{URL: server.URL + endpoint, MaxBytes: 100})
		errorCode(t, err, code)
	}
	for endpoint, want := range map[string]bool{"/permanent": true, "/temporary": false, "/mixed": false} {
		out, err := c.Fetch(context.Background(), Request{URL: server.URL + endpoint, MaxBytes: 100})
		if err != nil || out.Permanent != want || out.URL != server.URL+"/feed" {
			t.Fatalf("%s: %+v %v", endpoint, out, err)
		}
	}
}
func TestCapsStallAndConditional(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/declared":
			w.Header().Set("Content-Length", "10000")
			w.WriteHeader(200)
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(strings.Repeat("x", 10000)))
			_ = gz.Close()
		case "/chunked":
			w.(http.Flusher).Flush()
			fmt.Fprint(w, strings.Repeat("x", 1000))
		case "/stall":
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/304":
			w.Header().Set("ETag", "abc")
			w.WriteHeader(304)
		case "/headers":
			w.Header().Set("ETag", `"https://example.com/feed?auth=PRIVATE123"`)
			w.Header().Set("Last-Modified", "https://example.com/feed?auth=PRIVATE123")
			fmt.Fprint(w, "feed")
		default:
			w.Header().Set("Content-Encoding", "br")
			fmt.Fprint(w, "x")
		}
	}))
	defer server.Close()
	c := localClient(t)
	c.stall = 20 * time.Millisecond
	for _, endpoint := range []string{"/declared", "/gzip", "/chunked"} {
		_, err := c.Fetch(context.Background(), Request{URL: server.URL + endpoint, MaxBytes: 100})
		errorCode(t, err, "too_large")
	}
	_, err := c.Fetch(context.Background(), Request{URL: server.URL + "/stall", MaxBytes: 100})
	errorCode(t, err, "timeout")
	_, err = c.Fetch(context.Background(), Request{URL: server.URL + "/unsupported", MaxBytes: 100})
	errorCode(t, err, "not_a_feed")
	out, err := c.Fetch(context.Background(), Request{URL: server.URL + "/304", MaxBytes: 100})
	if err != nil || out.Status != 304 || out.ETag != "abc" || len(out.Body) != 0 {
		t.Fatalf("304: %+v %v", out, err)
	}
	out, err = c.Fetch(context.Background(), Request{URL: server.URL + "/headers", MaxBytes: 100})
	if err != nil || out.ETag != "" || out.LastModified != "" {
		t.Fatal("private URL reflected through validators", err)
	}
}
func TestRetryPolicyAndSafeFailures(t *testing.T) {
	t.Parallel()
	for _, status := range []int{500, 408, 429, 404, 410, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) < 3 {
					w.WriteHeader(status)
					return
				}
				fmt.Fprint(w, "feed")
			}))
			defer server.Close()
			c := localClient(t)
			_, err := c.Fetch(context.Background(), Request{URL: server.URL + "/?auth=PRIVATE123", MaxBytes: 100})
			if status == 500 || status == 408 || status == 429 {
				if err != nil || attempts.Load() != 3 {
					t.Fatalf("retry: %d %v", attempts.Load(), err)
				}
			} else {
				if attempts.Load() != 1 {
					t.Fatal("retried permanent failure")
				}
				code := "http_" + fmt.Sprint(status)
				if status == 410 {
					code = "gone"
				}
				errorCode(t, err, code)
			}
		})
	}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer server.Close()
	c := localClient(t)
	_, err := c.Fetch(context.Background(), Request{URL: server.URL, MaxBytes: 100})
	var e *Error
	if !errors.As(err, &e) || e.RetryAfterSeconds != 60 || attempts.Load() != 1 {
		t.Fatalf("Retry-After: %v", err)
	}
	c = New(Options{Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, errors.New("secret dns details")
	})})
	defer c.Close()
	c.wait = func(context.Context, time.Duration) error { return nil }
	_, err = c.Fetch(context.Background(), Request{URL: "https://example.org/SECRET123", MaxBytes: 100})
	errorCode(t, err, "dns")
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("DNS error leaked")
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsServer.Close()
	c = localClient(t)
	_, err = c.Fetch(context.Background(), Request{URL: tlsServer.URL, MaxBytes: 100})
	errorCode(t, err, "tls")
}
func TestDialTriesEveryAddress(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "feed") }))
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	// The first address (TEST-NET-1) never answers: its share of the dial time
	// runs out and the working one is still tried.
	c := New(Options{AllowNetworks: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("127.0.0.1/32")}, Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("127.0.0.1")}, nil
	})})
	defer c.Close()
	out, err := c.Fetch(context.Background(), Request{URL: "http://dual.example:" + port + "/feed", MaxBytes: 100})
	if err != nil || string(out.Body) != "feed" {
		t.Fatal(out, err)
	}
}
func TestPermanentOnlyAfterARedirect(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/feed", http.StatusMovedPermanently)
			return
		}
		fmt.Fprint(w, "feed")
	}))
	defer server.Close()
	c := localClient(t)
	// URL.String spells this userinfo another way (pa%21ss): no redirect is
	// no move.
	out, err := c.Fetch(context.Background(), Request{URL: strings.Replace(server.URL, "http://", "http://u:pa!ss@", 1) + "/feed", MaxBytes: 100})
	if err != nil || out.Permanent {
		t.Fatalf("a fetch with no redirect reported a move: %+v %v", out, err)
	}
	out, err = c.Fetch(context.Background(), Request{URL: server.URL + "/old", MaxBytes: 100})
	if err != nil || !out.Permanent || out.URL != server.URL+"/feed" {
		t.Fatalf("301: %+v %v", out, err)
	}
}
func TestGzipDataErrorsFailAtOnce(t *testing.T) {
	t.Parallel()
	var corrupt, truncated atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		if r.URL.Path == "/truncated" {
			// The connection drops inside the gzip header.
			truncated.Add(1)
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte{0x1f, 0x8b, 8})
			return
		}
		corrupt.Add(1)
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		_, _ = gz.Write([]byte(strings.Repeat("feed ", 100)))
		_ = gz.Close()
		raw := b.Bytes()
		raw[len(raw)-5] ^= 0xff // a wrong checksum
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	c := localClient(t)
	_, err := c.Fetch(context.Background(), Request{URL: server.URL + "/corrupt", MaxBytes: 1000})
	errorCode(t, err, "not_a_feed")
	if corrupt.Load() != 1 {
		t.Fatal("retried a corrupt body", corrupt.Load())
	}
	_, err = c.Fetch(context.Background(), Request{URL: server.URL + "/truncated", MaxBytes: 1000})
	errorCode(t, err, "network")
	if truncated.Load() != 3 {
		t.Fatal("a broken connection was not retried", truncated.Load())
	}
}
