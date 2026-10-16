package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/workerproto"
)

// The worker routes (Phase 1b): the ASR workers' side of the Bridge, under
// workerproto.Prefix on the page's server (and behind the same LAN guard). They
// take a worker token only, never a page session, and the page's routes never
// take a worker token. Transport only: the dispatcher (internal/workers) holds
// the queue and the leases.

// WorkerHub is what the worker routes drive.
type WorkerHub interface {
	Authenticate(ctx context.Context, token string) (int64, error)
	Hello(ctx context.Context, id int64, hello workerproto.Hello) (workerproto.HelloReply, error)
	Claim(ctx context.Context, id int64, wait time.Duration) (*workerproto.Task, error)
	Audio(id int64, taskID string, attempt int) (string, error)
	Heartbeat(id int64, taskID string, attempt int) (int, error)
	Result(ctx context.Context, id int64, taskID string, meta workerproto.ResultMeta, raw []byte) error
	Fail(id int64, taskID string, f workerproto.Fail) error
}

// How long a transfer may take: a long chapter's FLAC is over 100 MB, which a
// slow link (Wi-Fi, a tunnel) needs minutes for, beyond the page's own timeouts.
const (
	audioWriteTimeout = time.Hour
	resultReadTimeout = 10 * time.Minute
)

func (a *API) workerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+workerproto.RouteHello, a.worker(a.handleWorkerHello))
	mux.HandleFunc("GET "+workerproto.RouteNext, a.worker(a.handleWorkerNext))
	mux.HandleFunc("GET "+workerproto.RouteAudio, a.worker(a.handleWorkerAudio))
	mux.HandleFunc("POST "+workerproto.RouteHeartbeat, a.worker(a.handleWorkerHeartbeat))
	mux.HandleFunc("POST "+workerproto.RouteResult, a.worker(a.handleWorkerResult))
	mux.HandleFunc("POST "+workerproto.RouteFail, a.worker(a.handleWorkerFail))
	mux.HandleFunc(workerproto.Prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
}

// worker accepts a worker token speaking this protocol version: 401 for any
// other token (a page session included), then 409 protocol_mismatch.
func (a *API) worker(next func(http.ResponseWriter, *http.Request, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.workers == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		id, err := a.workers.Authenticate(r.Context(), bearerToken(r))
		switch {
		case errors.Is(err, workerproto.ErrUnauthorized):
			writeError(w, http.StatusUnauthorized, "unknown worker token: make one on the Bridge's page (Workers)")
			return
		case err != nil:
			a.log.Warn("bridge: worker auth check failed", "err", err)
			writeError(w, http.StatusInternalServerError, "auth error")
			return
		}
		if v := r.Header.Get(workerproto.HeaderProtocol); v != strconv.Itoa(workerproto.Version) {
			writeJSON(w, http.StatusConflict, workerproto.ErrorBody{Code: workerproto.CodeProtocolMismatch,
				Protocol: workerproto.Version,
				Error:    "this Bridge speaks worker protocol " + strconv.Itoa(workerproto.Version) + ": update the worker or the Bridge"})
			return
		}
		next(w, r, id)
	}
}

// decodeWorkerJSON decodes a small JSON body. Unknown fields are allowed: a
// newer worker may send more within the same protocol version.
func decodeWorkerJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// writeWorkerError maps the dispatcher's errors.
func (a *API) writeWorkerError(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, workerproto.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unknown worker token")
	case errors.Is(err, workerproto.ErrUnknownTask):
		writeErrorCode(w, http.StatusNotFound, workerproto.CodeUnknownTask, "the Bridge has no such task now")
	case errors.Is(err, workerproto.ErrNotHeld):
		writeErrorCode(w, http.StatusConflict, workerproto.CodeNotHeld, "this worker doesn't hold the task")
	case errors.Is(err, workerproto.ErrInvalidResult):
		writeErrorCode(w, http.StatusUnprocessableEntity, workerproto.CodeInvalidResult, "the transcript isn't a complete one")
	default:
		a.log.Warn("bridge: worker "+op+" failed", "err", err)
		writeError(w, http.StatusInternalServerError, op+" failed")
	}
}

func (a *API) handleWorkerHello(w http.ResponseWriter, r *http.Request, id int64) {
	var hello workerproto.Hello
	if !decodeWorkerJSON(w, r, &hello) {
		return
	}
	reply, err := a.workers.Hello(r.Context(), id, hello)
	if err != nil {
		a.writeWorkerError(w, err, "hello")
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

func (a *API) handleWorkerNext(w http.ResponseWriter, r *http.Request, id int64) {
	wait := 0
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "wait must be a number of seconds")
			return
		}
		wait = min(n, workerproto.MaxWaitSeconds)
	}
	task, err := a.workers.Claim(r.Context(), id, time.Duration(wait)*time.Second)
	if err != nil {
		a.writeWorkerError(w, err, "claim")
		return
	}
	if task == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// attempt reads the ?attempt= a task call names (0 when it names none, which
// matches no claim).
func attempt(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("attempt"))
	return n
}

func (a *API) handleWorkerAudio(w http.ResponseWriter, r *http.Request, id int64) {
	path, err := a.workers.Audio(id, r.PathValue("id"), attempt(r))
	if err != nil {
		a.writeWorkerError(w, err, "audio")
		return
	}
	f, err := os.Open(path) //nolint:gosec // a segment FLAC the engine queued, never a client's path
	if err != nil {
		a.writeWorkerError(w, err, "audio")
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		a.writeWorkerError(w, err, "audio")
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(audioWriteTimeout))
	w.Header().Set("Content-Type", "audio/flac")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (a *API) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request, id int64) {
	var hb workerproto.Heartbeat
	if !decodeWorkerJSON(w, r, &hb) {
		return
	}
	held, err := a.workers.Heartbeat(id, r.PathValue("id"), hb.Attempt)
	if err != nil {
		a.writeWorkerError(w, err, "heartbeat")
		return
	}
	writeJSON(w, http.StatusOK, workerproto.HeartbeatReply{Attempt: held})
}

func (a *API) handleWorkerResult(w http.ResponseWriter, r *http.Request, id int64) {
	// The write deadline too: the page server's runs from the request's start,
	// so a slow upload would lose the answer to a transcript it took.
	rc, deadline := http.NewResponseController(w), time.Now().Add(resultReadTimeout)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline.Add(time.Minute))
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, workerproto.MaxResultBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "the transcript is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "couldn't read the transcript")
		return
	}
	if err := a.workers.Result(r.Context(), id, r.PathValue("id"), workerproto.ParseResultMeta(r.URL.Query()), raw); err != nil {
		a.writeWorkerError(w, err, "result")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleWorkerFail(w http.ResponseWriter, r *http.Request, id int64) {
	var f workerproto.Fail
	if !decodeWorkerJSON(w, r, &f) {
		return
	}
	if err := a.workers.Fail(id, r.PathValue("id"), f); err != nil {
		a.writeWorkerError(w, err, "fail")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
