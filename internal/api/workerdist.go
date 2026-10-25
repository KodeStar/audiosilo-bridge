package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/workerdist"
)

// The worker download routes: install.sh and the gzipped worker builds the
// image carries (internal/workerdist). Outside workerproto.Prefix, which takes
// worker tokens only, and behind the LAN guard like the page, but with NO page
// password: a `curl | sh` carries no session, the program is public, and the
// worker token in the user's command is the credential. Neither route reads a
// path from the request: the OS and architecture pick from a fixed set.
const (
	RouteWorkerScript   = "/worker/install.sh"
	RouteWorkerDownload = "/worker/download/{os}/{arch}"
)

// workerDownloadTimeout bounds a worker build's download (the write deadline).
const workerDownloadTimeout = 10 * time.Minute

func (a *API) workerDistRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+RouteWorkerScript, a.handleWorkerScript)
	mux.HandleFunc("GET "+RouteWorkerDownload, a.handleWorkerDownload)
	// Anything else under /worker/ is no page route either.
	mux.HandleFunc("/worker/", func(w http.ResponseWriter, r *http.Request) {
		writePlain(w, http.StatusNotFound, "not found")
	})
}

// plainHeaders marks a response for a curl: plain text, never cached.
func plainHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
}

// writePlain answers a curl: plain text, so the message reads in a terminal.
func writePlain(w http.ResponseWriter, status int, msg string) {
	plainHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n")) //nolint:gosec // text/plain under nosniff, a fixed sentence
}

// handleWorkerScript serves install.sh with the address this request reached
// baked in. It always answers 200 with a script: when install.sh can't be
// served, a script that says why and exits 1 (`curl -fsSL` would drop an error
// status's body, and `sh` would run the empty script and exit 0).
func (a *API) handleWorkerScript(w http.ResponseWriter, r *http.Request) {
	text, err := a.workerScript(r)
	if err != nil {
		text = workerdist.ErrorScript(err.Error())
	}
	plainHeaders(w)
	_, _ = w.Write([]byte(text)) //nolint:gosec // text/plain under nosniff; the address is validated and single-quoted
}

func (a *API) workerScript(r *http.Request) (string, error) {
	if !a.dist.Available() {
		return "", workerdist.ErrMissing
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	addr, err := workerdist.Address(scheme, r.Host)
	if err != nil {
		return "", err
	}
	return workerdist.Script(addr)
}

func (a *API) handleWorkerDownload(w http.ResponseWriter, r *http.Request) {
	f, info, err := a.dist.Open(r.PathValue("os"), r.PathValue("arch"))
	switch {
	case errors.Is(err, workerdist.ErrUnknownPlatform), errors.Is(err, workerdist.ErrMissing):
		writePlain(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		a.log.Warn("bridge page: worker download failed", "err", err)
		writePlain(w, http.StatusInternalServerError, "the worker download failed")
		return
	}
	defer func() { _ = f.Close() }()
	// About 8 MB: give a slow link (Wi-Fi, a tunnel) more than the page's 60 s,
	// but not the hour a segment's FLAC gets: this route takes no credential.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(workerDownloadTimeout))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+info.Name()+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", info.ModTime(), f)
}
