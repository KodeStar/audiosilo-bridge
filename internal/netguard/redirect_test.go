package netguard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestRedirectCredentialRule(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		allowed      bool
	}{
		{"same-origin", "https://source:secret@feed.example/next", true},
		{"case-insensitive-origin", "https://source:secret@FEED.EXAMPLE/next", true},
		{"explicit-default-port", "https://source:secret@feed.example:443/next", true},
		{"subdomain", "https://source:secret@child.feed.example/next", false},
		{"other-host", "https://source:secret@other.example/next", false},
		{"other-port", "https://source:secret@feed.example:8443/next", false},
		{"other-scheme", "http://source:secret@feed.example/next", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from := httptest.NewRequest(http.MethodGet, "https://source:secret@feed.example/feed", nil)
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			req.Header.Set("Authorization", "Bearer source-secret")
			req.Header.Set("Cookie", "session=source-secret")
			req.Header.Set("Accept-Encoding", "gzip")
			stripRedirectCredentials(req, []*http.Request{from})
			if (req.URL.User != nil) != tc.allowed || (req.Header.Get("Authorization") != "") != tc.allowed || (req.Header.Get("Cookie") != "") != tc.allowed {
				t.Fatal("redirect credential rule violated")
			}
			if req.Header.Get("Accept-Encoding") != "gzip" {
				t.Fatal("redirect lost the encoding preference")
			}
		})
	}
}

func TestFetchRedirectCredentials(t *testing.T) {
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same", "/cross":
			if user, pass, ok := r.BasicAuth(); !ok || user != "source" || pass != "secret" {
				t.Error("source did not receive its credentials")
			}
			target := "/same-target"
			if r.URL.Path == "/cross" {
				// Explicit userinfo in Location must also be stripped across origins.
				target = strings.Replace(base, "127.0.0.1", "source:secret@other.example", 1) + "/cross-target"
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
		case "/same-target":
			if user, pass, ok := r.BasicAuth(); !ok || user != "source" || pass != "secret" {
				t.Error("same-origin redirect lost its credentials")
			}
			fmt.Fprint(w, "feed")
		case "/cross-target":
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("cross-origin redirect received source credentials")
			}
			fmt.Fprint(w, "feed")
		default:
			t.Error("unexpected redirect path", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	base = server.URL
	c := localClient(t)
	c.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	for _, endpoint := range []string{"/same", "/cross"} {
		t.Run(endpoint, func(t *testing.T) {
			raw := strings.Replace(base, "http://", "http://source:secret@", 1) + endpoint
			out, err := c.Fetch(context.Background(), Request{URL: raw, MaxBytes: 100})
			if err != nil {
				t.Fatal(err)
			}
			if string(out.Body) != "feed" || !out.Permanent {
				t.Fatal("redirect changed feed behavior", out.Status, out.Permanent)
			}
			if strings.Contains(out.URL, "source:secret@") != (endpoint == "/same") {
				t.Fatal("unexpected redirected userinfo")
			}
		})
	}
}

func TestRedirectCredentialsStayStripped(t *testing.T) {
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/source":
			if r.Header.Get("Authorization") != "Bearer source-secret" || r.Header.Get("Cookie") != "session=source-secret" {
				t.Error("source lost its headers")
			}
			http.Redirect(w, r, strings.Replace(base, "127.0.0.1", "child.feed.example", 1)+"/hop", http.StatusFound)
		case "/hop", "/target":
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("redirect chain restored source credentials", r.URL.Path)
			}
			if r.URL.Path == "/hop" {
				http.Redirect(w, r, "/target", http.StatusFound)
			} else {
				fmt.Fprint(w, "feed")
			}
		default:
			t.Error("unexpected redirect path", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	base = server.URL
	c := localClient(t)
	c.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	client := &http.Client{Transport: c.transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		stripRedirectCredentials(req, via)
		return nil
	}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, strings.Replace(base, "127.0.0.1", "feed.example", 1)+"/source", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer source-secret")
	req.Header.Set("Cookie", "session=source-secret")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("redirect failed", resp.StatusCode)
	}
}
