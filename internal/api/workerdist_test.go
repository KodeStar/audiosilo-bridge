package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
// when the fetch reached its host and port.
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
	// A public address on another host: the address the fetch reached.
	e.b.saved.PublicURL = "http://192.168.1.20:8091/"
	if body := fetchScript(t, e, "10.0.0.7:18091"); !strings.Contains(body, "BRIDGE='http://10.0.0.7:18091'") ||
		strings.Contains(body, "192.168.1.20") {
		t.Fatalf("script with another public address = %.300s", body)
	}
	// The public address's own host and port: the public address, as the
	// Workers tab's command has it (a path prefix, https behind a TLS proxy
	// whose request reaches the Bridge as plain http).
	for _, tc := range []struct{ pub, host, want string }{
		{"http://192.168.1.20:8091/", "192.168.1.20:8091", "http://192.168.1.20:8091"},
		{"http://proxy.lan/bridge/", "proxy.lan", "http://proxy.lan/bridge"},
		{"https://bridge.example.com/bridge", "bridge.example.com", "https://bridge.example.com/bridge"},
		{"https://bridge.example.com/bridge", "Bridge.Example.com:443", "https://bridge.example.com/bridge"},
	} {
		e.b.saved.PublicURL = tc.pub
		body := fetchScript(t, e, tc.host)
		if !strings.Contains(body, "\nBRIDGE='"+tc.want+"'\n") {
			t.Fatalf("script for public %q via %q = %.300s", tc.pub, tc.host, body)
		}
		// The download goes through the same prefix.
		if !strings.Contains(body, `"$BRIDGE/worker/download/$os/$arch"`) {
			t.Fatal("the script's download URL isn't under $BRIDGE")
		}
	}
	e.b.saved.PublicURL = ""
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
	for _, remote := range []string{"203.0.113.9:4000", "100.64.1.2:4000"} { // CGNAT / Tailscale isn't local
		// A download is a 403 (the script's `curl -f` fails loudly on it).
		r := e.do("GET", "/worker/download/darwin/arm64", "", "", remote)
		if r.Code != http.StatusForbidden || strings.Contains(r.Body.String(), "GZ") {
			t.Fatalf("download from %s = %d %q", remote, r.Code, r.Body.String())
		}
		// install.sh is a script that says why and exits 1, never install.sh
		// (`curl -fsSL` would drop a 403's body and sh would exit 0 on nothing).
		req := httptest.NewRequest("GET", RouteWorkerScript, nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", "192.168.1.30")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.HasPrefix(body, "#!/bin/sh\n") || strings.Contains(body, "BRIDGE=") ||
			!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("script from %s = %d %v %q", remote, rec.Code, rec.Header(), body)
		}
		if stderr, code := runScript(t, body); code != 1 || !strings.Contains(stderr, "only serves the worker install to its local network") {
			t.Fatalf("refused script: exit %d, %q", code, stderr)
		}
		// Every other route keeps the JSON 403.
		for _, path := range []string{"/api/status", "/api/health", "/worker/v1/claim", "/"} {
			r := e.do("GET", path, "", "", remote)
			if r.Code != http.StatusForbidden || !strings.Contains(r.Body.String(), `"lan_only"`) ||
				!strings.HasPrefix(r.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("%s from %s = %d %q", path, remote, r.Code, r.Body.String())
			}
		}
		if r := e.do("POST", RouteWorkerScript, "", "", remote); r.Code != http.StatusForbidden {
			t.Fatalf("POST script from %s = %d", remote, r.Code)
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
	// Answered 200 as a script that says why and exits 1 (`curl -fsSL` would
	// drop an error status's body, and sh would exit 0 on nothing).
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "rm -rf") || strings.Contains(rec.Body.String(), "BRIDGE=") {
		t.Fatalf("bad host = %d %q", rec.Code, rec.Body.String())
	}
	if stderr, code := runScript(t, rec.Body.String()); code != 1 || !strings.Contains(stderr, "isn't a plain host name or IP address") {
		t.Fatalf("bad host script: exit %d, %q", code, stderr)
	}
}

// Without the image's folder (a native run), both routes say where the
// releases are: the download as a 404, the script as a script that prints it
// and exits 1. The status says there are no downloads.
func TestWorkerDistMissing(t *testing.T) {
	t.Parallel()
	e := distEnv(t, true)
	r := e.do("GET", "/worker/download/darwin/arm64", "", "")
	if r.Code != http.StatusNotFound || !strings.Contains(r.Body.String(), workerdist.ReleasesURL) {
		t.Fatalf("download = %d %q", r.Code, r.Body.String())
	}
	r = e.do("GET", RouteWorkerScript, "", "")
	if r.Code != http.StatusOK || strings.Contains(r.Body.String(), "BRIDGE=") ||
		!strings.HasPrefix(r.Header().Get("Content-Type"), "text/plain") || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("script = %d %q", r.Code, r.Body.String())
	}
	if stderr, code := runScript(t, r.Body.String()); code != 1 || !strings.Contains(stderr, workerdist.ReleasesURL) {
		t.Fatalf("missing script: exit %d, %q", code, stderr)
	}
	var st Status
	r = e.do("GET", "/api/status", e.token, "")
	if err := json.Unmarshal(r.Body.Bytes(), &st); err != nil || st.WorkerDownloads {
		t.Fatalf("status = %s, %v", r.Body.String(), err)
	}
	e = distEnv(t, false)
	r = e.do("GET", "/api/status", e.token, "")
	if err := json.Unmarshal(r.Body.Bytes(), &st); err != nil || !st.WorkerDownloads {
		t.Fatalf("status = %s, %v", r.Body.String(), err)
	}
}

// fetchScript fetches install.sh from the LAN with the Host given.
func fetchScript(t *testing.T, e *env, host string) string {
	t.Helper()
	req := httptest.NewRequest("GET", RouteWorkerScript, nil)
	req.RemoteAddr, req.Host = "192.168.1.30:5000", host
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("script via %q = %d", host, rec.Code)
	}
	return rec.Body.String()
}

// runScript runs a served script with sh: its stderr and exit code.
func runScript(t *testing.T, text string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command("sh")
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return stderr.String(), cmd.ProcessState.ExitCode()
}
