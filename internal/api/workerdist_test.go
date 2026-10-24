package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/workerdist"
)

// distEnv is an env whose API serves the worker builds from a folder holding
// darwin/arm64's only (dir "" when missing is true: no folder at all).
func distEnv(t *testing.T, missing bool) *env {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "workers")
	if !missing {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "audiosilo-bridge_darwin_arm64.gz"), []byte("GZ"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return newEnvWith(t, func(a *API) { a.dist = workerdist.Dist{Dir: dir} })
}

// The script and the downloads answer the LAN with no page password, the
// script with the address it was fetched from baked in, or the public address
// once one is set.
func TestWorkerDistAllowed(t *testing.T) {
	t.Parallel()
	e := distEnv(t, false)
	r := e.do("GET", RouteWorkerScript, "", "")
	if r.Code != http.StatusOK || !strings.HasPrefix(r.Body.String(), "#!/bin/sh") ||
		!strings.Contains(r.Body.String(), "BRIDGE='http://example.com'") {
		t.Fatalf("script = %d %.200s", r.Code, r.Body.String())
	}
	if ct := r.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("script content type = %q", ct)
	}
	e.b.saved.PublicURL = "http://192.168.1.20:8091/"
	if r := e.do("GET", RouteWorkerScript, "", ""); !strings.Contains(r.Body.String(), "BRIDGE='http://192.168.1.20:8091'") {
		t.Fatalf("script with a public address = %.300s", r.Body.String())
	}
	r = e.do("GET", "/worker/download/darwin/arm64", "", "", "127.0.0.1:4000")
	if r.Code != http.StatusOK || r.Body.String() != "GZ" || r.Header().Get("Content-Type") != "application/gzip" ||
		!strings.Contains(r.Header().Get("Content-Disposition"), "audiosilo-bridge_darwin_arm64.gz") {
		t.Fatalf("download = %d %q %v", r.Code, r.Body.String(), r.Header())
	}
	// A worker token or a page session changes nothing (neither is needed).
	if r := e.do("GET", "/worker/download/darwin/arm64", e.token, ""); r.Code != http.StatusOK {
		t.Fatalf("download with a session = %d", r.Code)
	}
}

// Security: refused from a public address (the LAN guard), whatever forwarded
// header it sends; and only the fixed platforms, never a path.
func TestWorkerDistDenied(t *testing.T) {
	t.Parallel()
	e := distEnv(t, false)
	for _, path := range []string{RouteWorkerScript, "/worker/download/darwin/arm64"} {
		r := e.do("GET", path, "", "", "203.0.113.9:4000")
		if r.Code != http.StatusForbidden || strings.Contains(r.Body.String(), "GZ") {
			t.Fatalf("%s from a public address = %d %q", path, r.Code, r.Body.String())
		}
		r = e.do("GET", path, "", "", "100.64.1.2:4000") // CGNAT / Tailscale isn't local
		if r.Code != http.StatusForbidden {
			t.Fatalf("%s from CGNAT = %d", path, r.Code)
		}
	}
	for _, path := range []string{
		"/worker/download/windows/amd64", "/worker/download/darwin/386", "/worker/download/linux/arm64",
		"/worker/download/darwin/arm64.gz", "/worker/download/..%2f..%2fetc/passwd", "/worker/download/darwin",
	} {
		r := e.do("GET", path, "", "")
		if r.Code != http.StatusNotFound || strings.Contains(r.Body.String(), "GZ") {
			t.Fatalf("%s = %d %q", path, r.Code, r.Body.String())
		}
	}
	if r := e.do("POST", RouteWorkerScript, "", ""); r.Code != http.StatusNotFound {
		t.Fatalf("POST script = %d", r.Code)
	}
	// A Host that can't go into a script is refused, not baked in.
	req := httptest.NewRequest("GET", RouteWorkerScript, nil)
	req.RemoteAddr = "192.168.1.20:5000"
	req.Host = "evil';rm -rf ~;'"
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "rm -rf") {
		t.Fatalf("bad host = %d %q", rec.Code, rec.Body.String())
	}
}

// Without the image's folder (a native run), both routes say where the
// releases are, and the status says there are no downloads.
func TestWorkerDistMissing(t *testing.T) {
	t.Parallel()
	e := distEnv(t, true)
	for _, path := range []string{RouteWorkerScript, "/worker/download/darwin/arm64"} {
		r := e.do("GET", path, "", "")
		if r.Code != http.StatusNotFound || !strings.Contains(r.Body.String(), workerdist.ReleasesURL) {
			t.Fatalf("%s = %d %q", path, r.Code, r.Body.String())
		}
	}
	var st Status
	r := e.do("GET", "/api/status", e.token, "")
	if err := json.Unmarshal(r.Body.Bytes(), &st); err != nil || st.WorkerDownloads {
		t.Fatalf("status = %s, %v", r.Body.String(), err)
	}
	e = distEnv(t, false)
	r = e.do("GET", "/api/status", e.token, "")
	if err := json.Unmarshal(r.Body.Bytes(), &st); err != nil || !st.WorkerDownloads {
		t.Fatalf("status = %s, %v", r.Body.String(), err)
	}
}
