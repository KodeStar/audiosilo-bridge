package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/workerproto"
)

const fakeWorkerToken = "asw_good"

// fakeHub is a WorkerHub that knows one worker (id 1, fakeWorkerToken) and
// answers each task id by what it is named.
type fakeHub struct {
	audio  string
	result []byte
	meta   workerproto.ResultMeta
	hello  workerproto.Hello
	fail   workerproto.Fail
}

func (h *fakeHub) Authenticate(_ context.Context, token string) (int64, error) {
	if token != fakeWorkerToken {
		return 0, workerproto.ErrUnauthorized
	}
	return 1, nil
}
func (h *fakeHub) Hello(_ context.Context, _ int64, hello workerproto.Hello) (workerproto.HelloReply, error) {
	h.hello = hello
	return workerproto.HelloReply{Protocol: workerproto.Version, WorkerID: 1, Name: "studio"}, nil
}
func (h *fakeHub) Claim(_ context.Context, _ int64, wait time.Duration) (*workerproto.Task, error) {
	if wait == 0 {
		return nil, nil
	}
	return &workerproto.Task{ID: "t1", Attempt: 1, Size: 4}, nil
}

// taskErr answers a task by its id: "gone" isn't held, "unknown" isn't known.
func taskErr(id string) error {
	switch id {
	case "gone":
		return workerproto.ErrNotHeld
	case "unknown":
		return workerproto.ErrUnknownTask
	}
	return nil
}
func (h *fakeHub) Audio(_ int64, id string, attempt int) (string, error) {
	if attempt != 1 {
		return "", workerproto.ErrNotHeld
	}
	return h.audio, taskErr(id)
}
func (h *fakeHub) Heartbeat(_ int64, id string, attempt int) (int, error) {
	return attempt + 1, taskErr(id)
}
func (h *fakeHub) Result(_ context.Context, _ int64, id string, meta workerproto.ResultMeta, raw []byte) error {
	if string(raw) == "half" {
		return workerproto.ErrInvalidResult
	}
	h.meta, h.result = meta, raw
	return taskErr(id)
}
func (h *fakeHub) Fail(_ int64, id string, f workerproto.Fail) error { h.fail = f; return taskErr(id) }

// worker sends a worker call (the protocol header set unless proto is given).
func (e *env) worker(method, path, token, body string, proto ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.168.1.30:6000"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	v := strconv.Itoa(workerproto.Version)
	if len(proto) > 0 {
		v = proto[0]
	}
	if v != "" {
		r.Header.Set(workerproto.HeaderProtocol, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

var workerRoutes = [][2]string{
	{"POST", "/worker/v1/hello"}, {"GET", "/worker/v1/tasks/next"}, {"GET", "/worker/v1/tasks/t1/audio?attempt=1"},
	{"POST", "/worker/v1/tasks/t1/heartbeat"}, {"POST", "/worker/v1/tasks/t1/result"}, {"POST", "/worker/v1/tasks/t1/fail"},
}

// Security: the worker routes take a worker token only: no token, a wrong one
// and a page session are 401 (denied); the worker's own goes through (allowed).
func TestWorkerRoutesNeedAWorkerToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.hub.audio = writeTemp(t, "flac")
	for _, rt := range workerRoutes {
		for _, tok := range []string{"", "asw_wrong", e.token} {
			if w := e.worker(rt[0], rt[1], tok, "{}"); w.Code != http.StatusUnauthorized {
				t.Errorf("denied: %s %s with %q = %d, want 401", rt[0], rt[1], tok, w.Code)
			}
		}
		if w := e.worker(rt[0], rt[1], fakeWorkerToken, "{}"); w.Code == http.StatusUnauthorized || w.Code >= 500 {
			t.Errorf("allowed: %s %s with the worker token = %d %s", rt[0], rt[1], w.Code, w.Body.String())
		}
	}
}

// The protocol version: another version (or none) is refused with
// protocol_mismatch and the Bridge's own version, on every route.
func TestWorkerProtocolMismatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, rt := range workerRoutes {
		for _, v := range []string{"", "0", strconv.Itoa(workerproto.Version + 1), "x"} {
			w := e.worker(rt[0], rt[1], fakeWorkerToken, "{}", v)
			want := `"code":"protocol_mismatch","protocol":` + strconv.Itoa(workerproto.Version)
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), want) {
				t.Errorf("%s %s with protocol %q = %d %s", rt[0], rt[1], v, w.Code, w.Body.String())
			}
		}
	}
}

// Security: the LAN guard covers the worker routes too.
func TestWorkerRoutesAreLANOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := httptest.NewRequest(http.MethodPost, "/worker/v1/hello", strings.NewReader("{}"))
	r.RemoteAddr = "203.0.113.9:4000"
	r.Header.Set("Authorization", "Bearer "+fakeWorkerToken)
	r.Header.Set(workerproto.HeaderProtocol, strconv.Itoa(workerproto.Version))
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a public address = %d, want 403", w.Code)
	}
}

func TestWorkerCalls(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.hub.audio = writeTemp(t, "fLaC-audio")

	// Hello: unknown fields are fine (a newer worker within the version).
	w := e.worker("POST", "/worker/v1/hello", fakeWorkerToken, `{"protocol":1,"backend":"mlx-whisper","later":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"studio"`) || e.hub.hello.Backend != "mlx-whisper" {
		t.Fatalf("hello = %d %s %+v", w.Code, w.Body.String(), e.hub.hello)
	}
	if w := e.worker("POST", "/worker/v1/hello", fakeWorkerToken, `nope`); w.Code != http.StatusBadRequest {
		t.Fatalf("a bad hello = %d", w.Code)
	}

	// Next: 204 without a wait, a task with one, 400 for a bad wait.
	if w := e.worker("GET", "/worker/v1/tasks/next", fakeWorkerToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("next, no wait = %d", w.Code)
	}
	if w := e.worker("GET", "/worker/v1/tasks/next?wait=999", fakeWorkerToken, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"t1"`) {
		t.Fatalf("next = %d %s", w.Code, w.Body.String())
	}
	if w := e.worker("GET", "/worker/v1/tasks/next?wait=-1", fakeWorkerToken, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("next, bad wait = %d", w.Code)
	}

	// Audio: the file with a Range answer; another attempt is not held.
	if w := e.worker("GET", "/worker/v1/tasks/t1/audio?attempt=1", fakeWorkerToken, ""); w.Code != http.StatusOK ||
		w.Body.String() != "fLaC-audio" || w.Header().Get("Content-Type") != "audio/flac" {
		t.Fatalf("audio = %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
	}
	if w := e.worker("GET", "/worker/v1/tasks/t1/audio?attempt=2", fakeWorkerToken, ""); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), `"code":"not_held"`) {
		t.Fatalf("audio, other attempt = %d %s", w.Code, w.Body.String())
	}

	// Heartbeat answers the attempt held; gone and unknown tasks map to their codes.
	if w := e.worker("POST", "/worker/v1/tasks/t1/heartbeat", fakeWorkerToken, `{"attempt":3}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"attempt":4`) {
		t.Fatalf("heartbeat = %d %s", w.Code, w.Body.String())
	}
	if w := e.worker("POST", "/worker/v1/tasks/gone/heartbeat", fakeWorkerToken, `{"attempt":1}`); w.Code != http.StatusConflict {
		t.Fatalf("heartbeat, gone = %d", w.Code)
	}
	if w := e.worker("POST", "/worker/v1/tasks/unknown/heartbeat", fakeWorkerToken, `{"attempt":1}`); w.Code != http.StatusNotFound ||
		!strings.Contains(w.Body.String(), `"code":"unknown_task"`) {
		t.Fatalf("heartbeat, unknown = %d %s", w.Code, w.Body.String())
	}

	// Result: the raw body, the metadata in the query; an incomplete one is 422.
	q := workerproto.ResultMeta{Attempt: 2, Backend: "mlx-whisper", Device: "metal", Model: "turbo", DecodeSeconds: 61.5}.Query().Encode()
	if w := e.worker("POST", "/worker/v1/tasks/t1/result?"+q, fakeWorkerToken, `{"text":"x","segments":[NaN]}`); w.Code != http.StatusNoContent {
		t.Fatalf("result = %d %s", w.Code, w.Body.String())
	}
	if string(e.hub.result) != `{"text":"x","segments":[NaN]}` || e.hub.meta.Attempt != 2 || e.hub.meta.DecodeSeconds != 61.5 ||
		e.hub.meta.Device != "metal" {
		t.Fatalf("result seen = %q %+v", e.hub.result, e.hub.meta)
	}
	if w := e.worker("POST", "/worker/v1/tasks/t1/result", fakeWorkerToken, `half`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an incomplete result = %d", w.Code)
	}

	// Fail.
	if w := e.worker("POST", "/worker/v1/tasks/t1/fail", fakeWorkerToken, `{"attempt":1,"error":"boom"}`); w.Code != http.StatusNoContent ||
		e.hub.fail.Error != "boom" {
		t.Fatalf("fail = %d %+v", w.Code, e.hub.fail)
	}
	if w := e.worker("GET", "/worker/v1/nope", fakeWorkerToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown worker route = %d", w.Code)
	}
}

func TestWorkerResultTooLarge(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	big := strings.Repeat("x", workerproto.MaxResultBytes+1)
	if w := e.worker("POST", "/worker/v1/tasks/t1/result", fakeWorkerToken, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized result = %d", w.Code)
	}
}

// The page's worker routes: list, add (the token once), remove.
func TestPageWorkers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if w := e.do("GET", "/api/workers", e.token, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"connected":true`) {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	if w := e.do("POST", "/api/workers", e.token, `{"name":"Mac Studio"}`); w.Code != http.StatusCreated ||
		!strings.Contains(w.Body.String(), `"token":"asw_secret"`) || e.b.createdWorker != "Mac Studio" {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	if w := e.do("POST", "/api/workers", e.token, `{"name":" "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("create, no name = %d", w.Code)
	}
	if w := e.do("DELETE", "/api/workers/2", e.token, ""); w.Code != http.StatusNoContent || e.b.revokedWorker != 2 {
		t.Fatalf("revoke = %d", w.Code)
	}
	if w := e.do("DELETE", "/api/workers/9", e.token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("revoke, none = %d", w.Code)
	}
	if w := e.do("DELETE", "/api/workers/x", e.token, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("revoke, bad id = %d", w.Code)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "seg.flac")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
