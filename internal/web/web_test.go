package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesIndex(t *testing.T) {
	t.Parallel()
	h := New()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "AudioSilo Bridge") {
		t.Errorf("index body missing wordmark")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("index served without a CSP")
	}
	// The page asked for by its file name carries the same CSP.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("GET /index.html = %d, CSP %q, Cache-Control %q", rec.Code, rec.Header().Get("Content-Security-Policy"), rec.Header().Get("Cache-Control"))
	}
}

func TestClientRouteFallsBackToIndex(t *testing.T) {
	t.Parallel()
	h := New()
	rec := httptest.NewRecorder()
	// An extensionless path is a client-side route -> index.html (200).
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200 (SPA fallback)", rec.Code)
	}
}

func TestMissingFileIs404(t *testing.T) {
	h := New()
	// A path that looks like a file (has an extension) and is absent must 404,
	// so /config.json is not masked by the SPA fallback.
	for _, p := range []string{"/config.json", "/assets/missing.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}
