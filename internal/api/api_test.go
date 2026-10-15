package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/auth"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/logring"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// backend is a fake Backend recording what the handlers asked for.
type backend struct {
	pairErr, mountErr, settingsErr error
	paired                         [2]string
	mount                          string
	saved                          Settings
	jobs                           []store.ServerJob
	cancelled                      int64
	logsLevel                      slog.Level
}

func (b *backend) Status() Status { return Status{Version: "0.1.0", Kinds: []string{"bridge.verify"}} }
func (b *backend) Pair(_ context.Context, url, code string) error {
	b.paired = [2]string{url, code}
	return b.pairErr
}
func (b *backend) Unpair(context.Context) error { return nil }
func (b *backend) Libraries() []Library         { return []Library{} }
func (b *backend) SetMount(_ context.Context, id int64, path string) error {
	b.mount = fmt.Sprintf("%d=%s", id, path)
	return b.mountErr
}
func (b *backend) CheckNow(context.Context) {}
func (b *backend) Settings() Settings {
	return Settings{PublicURL: b.saved.PublicURL, Locked: []string{}}
}
func (b *backend) SaveSettings(_ context.Context, s Settings) error {
	b.saved = s
	return b.settingsErr
}
func (b *backend) Jobs(_ context.Context, before int64, limit int) ([]store.ServerJob, error) {
	if limit < len(b.jobs) {
		return b.jobs[:limit], nil
	}
	return b.jobs, nil
}
func (b *backend) CancelJob(id int64) bool { b.cancelled = id; return id == 7 }
func (b *backend) Logs(_ uint64, _ int, level slog.Level) []logring.Entry {
	b.logsLevel = level
	return []logring.Entry{}
}
func (b *backend) Workers() []link.Worker { return []link.Worker{} }

type env struct {
	h     http.Handler
	b     *backend
	token string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	am := auth.New(auth.NewMemStore())
	pw, err := am.EnsureAdmin()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := am.Login(pw)
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{}
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "page") })
	return &env{h: New(b, am, page, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler(), b: b, token: tok}
}

// do sends a request from a LAN address unless remote is given.
func (e *env) do(method, path, token, body string, remote ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.168.1.20:5000"
	if len(remote) > 0 {
		r.RemoteAddr = remote[0]
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// Security: the page answers loopback and private addresses only (allowed and
// denied), whatever a forwarded header claims.
func TestLocalAddress(t *testing.T) {
	t.Parallel()
	for _, a := range []string{"127.0.0.1:1", "[::1]:1", "10.0.0.5:1", "172.16.3.4:1", "192.168.1.2:1", "169.254.1.1:1",
		"[fe80::1]:1", "[fd00::5]:1", "[::ffff:192.168.1.2]:1"} {
		if !LocalAddress(a) {
			t.Errorf("allowed: %s refused", a)
		}
	}
	for _, a := range []string{"8.8.8.8:1", "[2001:db8::1]:1", "100.64.1.1:1", "172.32.0.1:1", "garbage", ""} {
		if LocalAddress(a) {
			t.Errorf("denied: %s accepted", a)
		}
	}
	e := newEnv(t)
	for _, path := range []string{"/", "/api/health", "/api/status"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "203.0.113.9:4000"
		r.Header.Set("X-Forwarded-For", "192.168.1.2")
		r.Header.Set("Authorization", "Bearer "+e.token)
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"lan_only"`) {
			t.Errorf("a public address on %s = %d %s, want 403 lan_only", path, w.Code, w.Body.String())
		}
	}
	if w := e.do(http.MethodGet, "/api/health", "", "", "127.0.0.1:9"); w.Code != http.StatusOK {
		t.Errorf("loopback health = %d", w.Code)
	}
}

// Security: every API route but login and health needs a session, loopback
// included (allowed with the token; denied without one or with a wrong one).
func TestEveryRouteNeedsASession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	routes := [][2]string{
		{"POST", "/api/password"}, {"GET", "/api/status"}, {"POST", "/api/pair"},
		{"POST", "/api/unpair"}, {"GET", "/api/libraries"}, {"PUT", "/api/libraries/1"}, {"POST", "/api/libraries/check"},
		{"GET", "/api/settings"}, {"PUT", "/api/settings"}, {"GET", "/api/jobs"}, {"POST", "/api/jobs/1/cancel"},
		{"GET", "/api/logs"}, {"GET", "/api/workers"},
		{"POST", "/api/logout"}, // last: it ends the session
	}
	for _, rt := range routes {
		for _, tok := range []string{"", "not-a-session"} {
			if w := e.do(rt[0], rt[1], tok, "{}", "127.0.0.1:5"); w.Code != http.StatusUnauthorized {
				t.Errorf("denied: %s %s with %q = %d, want 401", rt[0], rt[1], tok, w.Code)
			}
		}
		if w := e.do(rt[0], rt[1], e.token, "{}"); w.Code == http.StatusUnauthorized {
			t.Errorf("allowed: %s %s with a session = 401", rt[0], rt[1])
		}
	}
	if w := e.do("GET", "/api/nope", e.token, ""); w.Code != http.StatusNotFound {
		t.Errorf("an unknown API route = %d", w.Code)
	}
	if w := e.do("GET", "/settings", "", ""); w.Code != http.StatusOK || w.Body.String() != "page" {
		t.Errorf("the page = %d %q", w.Code, w.Body.String())
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if w := e.do("POST", "/api/login", "", `{"password":"wrong"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", w.Code)
	}
	if w := e.do("POST", "/api/login", "", `{"password":"x","extra":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", w.Code)
	}
	// The limiter: a burst of wrong passwords from one address is refused.
	var limited bool
	for range 20 {
		if w := e.do("POST", "/api/login", "", `{"password":"wrong"}`, "10.0.0.9:1"); w.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("the login limiter never refused")
	}
	if w := e.do("POST", "/api/logout", e.token, ""); w.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", w.Code)
	}
	if w := e.do("GET", "/api/status", e.token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("a signed-out session = %d", w.Code)
	}
}

func TestPairAndErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if w := e.do("POST", "/api/pair", e.token, `{"server_url":"","code":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("no address = %d", w.Code)
	}
	if w := e.do("POST", "/api/pair", e.token, `{"server_url":" http://audiosilo:8080 ","code":"ABCD-1234"}`); w.Code != http.StatusOK || e.b.paired != [2]string{"http://audiosilo:8080", "ABCD-1234"} {
		t.Fatalf("pair = %d %v", w.Code, e.b.paired)
	}
	for err, want := range map[error]int{
		fmt.Errorf("%w: wrong code", ErrRefused):   http.StatusUnprocessableEntity,
		fmt.Errorf("%w: paired", ErrConflict):      http.StatusConflict,
		fmt.Errorf("%w: dial tcp", ErrUnreachable): http.StatusBadGateway,
		fmt.Errorf("%w: bad", ErrInvalid):          http.StatusBadRequest,
		fmt.Errorf("disk on fire at /secret/path"): http.StatusInternalServerError,
	} {
		e.b.pairErr = err
		w := e.do("POST", "/api/pair", e.token, `{"server_url":"http://a","code":"c"}`)
		if w.Code != want {
			t.Errorf("%v = %d, want %d", err, w.Code, want)
		}
		if want == http.StatusInternalServerError && strings.Contains(w.Body.String(), "secret") {
			t.Errorf("an internal error was echoed: %s", w.Body.String())
		}
		if want != http.StatusInternalServerError && !strings.Contains(w.Body.String(), `"error":"`+err.Error()+`"`) {
			t.Errorf("the message isn't the sentence alone: %s", w.Body.String())
		}
	}
}

func TestMountsSettingsJobsLogs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if w := e.do("PUT", "/api/libraries/x", e.token, `{"mount_path":"/b"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", w.Code)
	}
	if w := e.do("PUT", "/api/libraries/3", e.token, `{"mount_path":" /books "}`); w.Code != http.StatusOK || e.b.mount != "3=/books" {
		t.Fatalf("set mount = %d %q", w.Code, e.b.mount)
	}
	e.b.mountErr = Errorf(ErrLocked, "AUDIOSILO_BRIDGE_LIBRARIES sets the mappings")
	w := e.do("PUT", "/api/libraries/3", e.token, `{"mount_path":"/b"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"locked"`) {
		t.Fatalf("locked mount = %d %s", w.Code, w.Body.String())
	}
	if w := e.do("PUT", "/api/settings", e.token, `{"public_url":" http://bridge.lan:8091 ","mapping_check_minutes":5}`); w.Code != http.StatusOK ||
		e.b.saved.PublicURL != "http://bridge.lan:8091" || e.b.saved.MappingCheckMinutes != 5 {
		t.Fatalf("save settings = %d %+v", w.Code, e.b.saved)
	}
	for i := range 3 {
		e.b.jobs = append(e.b.jobs, store.ServerJob{ID: int64(10 - i)})
	}
	var page struct {
		Jobs       []store.ServerJob `json:"jobs"`
		NextBefore int64             `json:"next_before"`
	}
	_ = json.Unmarshal(e.do("GET", "/api/jobs?limit=2", e.token, "").Body.Bytes(), &page)
	if len(page.Jobs) != 2 || page.NextBefore != 9 {
		t.Fatalf("jobs page = %+v", page)
	}
	if w := e.do("POST", "/api/jobs/7/cancel", e.token, ""); w.Code != http.StatusAccepted {
		t.Fatalf("cancel = %d", w.Code)
	}
	if w := e.do("POST", "/api/jobs/8/cancel", e.token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("cancel of a job not running here = %d", w.Code)
	}
	if w := e.do("GET", "/api/logs?level=nonsense", e.token, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad level = %d", w.Code)
	}
	if w := e.do("GET", "/api/logs?level=WARN", e.token, ""); w.Code != http.StatusOK || e.b.logsLevel != slog.LevelWarn {
		t.Fatalf("logs = %d %v", w.Code, e.b.logsLevel)
	}
	if h := e.do("GET", "/api/status", e.token, "").Header(); h.Get("Cache-Control") != "no-store" || h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("API headers = %v", h)
	}
}

func TestPassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if w := e.do("POST", "/api/password", e.token, `{"current":"wrong","new":"long enough pw"}`); w.Code != http.StatusForbidden {
		t.Fatalf("wrong current = %d", w.Code)
	}
}
