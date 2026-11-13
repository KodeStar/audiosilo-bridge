package netguard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRequestAllowFirstURLAndRedirects: a request's Allow is asked about the
// first URL and every redirect's; a refusal fails it as blocked_host before
// the refused address is reached, for Stream and Fetch alike.
func TestRequestAllowFirstURLAndRedirects(t *testing.T) {
	var base string
	var reachedOther atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Host != "" && strings.HasPrefix(r.Host, "other.example"):
			reachedOther.Add(1)
			fmt.Fprint(w, "other")
		case r.URL.Path == "/to-allowed":
			http.Redirect(w, r, strings.Replace(base, "127.0.0.1", "allowed.example", 1)+"/body", http.StatusFound)
		case r.URL.Path == "/to-other":
			http.Redirect(w, r, strings.Replace(base, "127.0.0.1", "other.example", 1)+"/body", http.StatusFound)
		default:
			fmt.Fprint(w, "body")
		}
	}))
	defer server.Close()
	base = server.URL
	c := localClient(t)
	c.resolver = resolverFunc(func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if strings.HasSuffix(host, ".example") {
			host = "127.0.0.1"
		}
		ip, err := netip.ParseAddr(host)
		return []netip.Addr{ip}, err
	})
	// Admits 127.0.0.1 and allowed.example only.
	allow := func(u *url.URL) bool { return u.Hostname() == "127.0.0.1" || u.Hostname() == "allowed.example" }
	other := strings.Replace(base, "127.0.0.1", "other.example", 1)

	t.Run("allowed", func(t *testing.T) {
		for _, p := range []string{"/body", "/to-allowed"} {
			r, err := c.Stream(context.Background(), StreamRequest{URL: base + p, Allow: allow})
			if err != nil {
				t.Fatal(p, err)
			}
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if string(b) != "body" {
				t.Fatalf("%s: %q", p, b)
			}
			if !strings.HasPrefix(r.ContentType, "text/plain") {
				t.Fatalf("%s: content type %q", p, r.ContentType)
			}
			f, err := c.Fetch(context.Background(), Request{URL: base + p, MaxBytes: 100, Allow: allow})
			if err != nil || string(f.Body) != "body" {
				t.Fatal(p, err)
			}
		}
	})
	t.Run("denied", func(t *testing.T) {
		for _, raw := range []string{other + "/body", base + "/to-other"} {
			_, err := c.Stream(context.Background(), StreamRequest{URL: raw, Allow: allow})
			errorCode(t, err, "blocked_host")
			_, err = c.Fetch(context.Background(), Request{URL: raw, MaxBytes: 100, Allow: allow})
			errorCode(t, err, "blocked_host")
		}
		if n := reachedOther.Load(); n != 0 {
			t.Fatalf("a refused host was reached %d times", n)
		}
	})
	t.Run("no allow keeps the old rule", func(t *testing.T) {
		r, err := c.Stream(context.Background(), StreamRequest{URL: base + "/to-other"})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if string(b) != "other" {
			t.Fatalf("%q", b)
		}
	})
}
