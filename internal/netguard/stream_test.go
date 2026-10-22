package netguard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSharedResumeVectors(t *testing.T) {
	b, err := os.ReadFile("../podcast/testdata/phase2c2/resume-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Cases []struct {
			Name       string
			Offset     int64
			PriorTotal int64  `json:"prior_total"`
			PriorETag  string `json:"prior_etag"`
			Status     int
			ETag       string
			Range      string `json:"content_range"`
			Length     int64  `json:"content_length"`
			Encoding   string
			Action     string
		}
	}
	if err := json.Unmarshal(b, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors.Cases {
		r := &StreamResponse{Status: v.Status, ETag: v.ETag, ContentRange: v.Range, ContentLength: v.Length, Encoding: v.Encoding}
		if got := ResumeAction(v.Offset, v.PriorTotal, v.PriorETag, r); got != v.Action {
			t.Errorf("%s: %s want %s", v.Name, got, v.Action)
		}
	}
	for _, tag := range []string{"", `W/"one"`, "one", `"a"b"`, "\"line\n\""} {
		if StrongETag(tag) {
			t.Fatalf("weak/invalid tag accepted: %q", tag)
		}
	}
}
func TestStreamGuardHeadersRedirectsAndStall(t *testing.T) {
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Range") != "bytes=5-" || r.Header.Get("If-Range") != `"one"` {
			t.Error("stream request", r.Method, r.Header)
		}
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, strings.Replace(base, "127.0.0.1", "other.example", 1)+"/target", http.StatusFound)
		case "/target":
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("credentials escaped origin")
			}
			fmt.Fprint(w, "audio")
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/blocked":
			http.Redirect(w, r, "http://169.254.169.254/private", http.StatusFound)
		case "/stall":
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			fmt.Fprint(w, "audio")
		}
	}))
	defer server.Close()
	base = server.URL
	c := localClient(t)
	c.resolver = resolverFunc(func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host == "other.example" {
			host = "127.0.0.1"
		}
		ip, err := netip.ParseAddr(host)
		return []netip.Addr{ip}, err
	})
	for _, p := range []string{"/audio", "/redirect"} {
		raw := strings.Replace(base, "http://", "http://source:secret@", 1) + p
		r, err := c.Stream(context.Background(), StreamRequest{URL: raw, Offset: 5, ETag: `"one"`})
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil || string(b) != "audio" {
			t.Fatal(string(b), err)
		}
	}
	for _, tc := range []struct{ p, code string }{{"/blocked", "blocked_address"}, {"/loop", "http_310"}} {
		_, err := c.Stream(context.Background(), StreamRequest{URL: base + tc.p, Offset: 5, ETag: `"one"`})
		errorCode(t, err, tc.code)
	}
	denied := New(Options{})
	defer denied.Close()
	_, err := denied.Stream(context.Background(), StreamRequest{URL: base + "/audio"})
	errorCode(t, err, "blocked_address")
	c.stall = 20 * time.Millisecond
	r, err := c.Stream(context.Background(), StreamRequest{URL: base + "/stall", Offset: 5, ETag: `"one"`})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r.Body)
	r.Body.Close()
	if err == nil {
		t.Fatal("stalled body accepted")
	}
}
