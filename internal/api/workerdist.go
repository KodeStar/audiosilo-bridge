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

func (a *API) handleWorkerScript(w http.ResponseWriter, r *http.Request) {
	if !a.dist.Available() {
		writePlain(w, http.StatusNotFound, workerdist.ErrMissing.Error())
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	addr, err := workerdist.Address(a.b.Settings().PublicURL, scheme, r.Host)
	if err != nil {
		writePlain(w, http.StatusBadRequest, err.Error()+": set the Bridge's public address in Settings")
		return
	}
	text, err := workerdist.Script(addr)
	if err != nil {
		writePlain(w, http.StatusBadRequest, err.Error()+": set the Bridge's public address in Settings")
		return
	}
	plainHeaders(w)
	_, _ = w.Write([]byte(text)) //nolint:gosec // text/plain under nosniff; the address is validated and single-quoted
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
	// About 8 MB: give a slow link (Wi-Fi, a tunnel) more than the page's 60 s.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(audioWriteTimeout))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+info.Name()+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", info.ModTime(), f)
}
