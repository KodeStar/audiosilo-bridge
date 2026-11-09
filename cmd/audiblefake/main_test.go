//go:build audiblefake

package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/audible/audiblefake"
)

// Every route the help names is one the fake serves (never a 404), and the
// preorder routes are named.
func TestHelpNamesServedRoutes(t *testing.T) {
	text := help("127.0.0.1:8099")
	for _, want := range []string{"/1.0/orders", "/1.0/catalog/products", "/_fake/release", "127.0.0.1:8099"} {
		if !strings.Contains(text, want) {
			t.Errorf("the help doesn't name %s", want)
		}
	}
	h := audiblefake.New(audiblefake.Options{KeyBits: 1024}).Handler()
	routes := regexp.MustCompile(`(GET|POST) (/[A-Za-z0-9_./-]+)`).FindAllStringSubmatch(text, -1)
	if len(routes) < 10 {
		t.Fatalf("routes found = %d", len(routes))
	}
	for _, m := range routes {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m[1], m[2], nil))
		// A served route may answer 404 for a thing it names (release with
		// no asin), never the mux's "page not found".
		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "404 page not found") {
			t.Errorf("%s %s is not served", m[1], m[2])
		}
	}
}
