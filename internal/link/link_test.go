package link

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// serverClientPattern is audiosilo-server's auth.clientPattern: the header the
// Bridge sends must parse there, or the server ignores it.
var serverClientPattern = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9 ._-]{0,39}?)(?:/([A-Za-z0-9][A-Za-z0-9.+_-]{0,31}))?(?: \(([A-Za-z0-9][A-Za-z0-9 ._-]{0,23})\))?$`)

func TestClientValueParsesOnTheServer(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"0.1.0", "", "v1.2.3-rc.1+abc", "weird version with spaces/and;stuff", strings.Repeat("9", 60)} {
		got := clientValue(v)
		m := serverClientPattern.FindStringSubmatch(got)
		if m == nil || m[1] != ClientName {
			t.Errorf("clientValue(%q) = %q, which the server can't parse", v, got)
		}
	}
}

// fake records the last request and answers with a canned status and body.
type fake struct {
	t      *testing.T
	status int
	body   string
	last   *http.Request
	sent   string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.last, f.sent = r, string(b)
	if f.status == 0 {
		f.status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = io.WriteString(w, f.body)
}

func newFake(t *testing.T, status int, body string) (*fake, *Client) {
	t.Helper()
	f := &fake{t: t, status: status, body: body}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/", "tok-123", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestPairSendsNoCredential(t *testing.T) {
	t.Parallel()
	f := &fake{status: http.StatusCreated, body: `{"token":"secret","server_id":"s1","server_name":"Home"}`}
	srv := httptest.NewServer(f)
	defer srv.Close()
	p, err := Pair(context.Background(), srv.URL, " ABCD-1234 ", "0.1.0")
	if err != nil || p.Token != "secret" || p.ServerName != "Home" {
		t.Fatalf("Pair = %+v, %v", p, err)
	}
	if f.last.URL.Path != "/api/v1/bridge/pair" || f.last.Method != http.MethodPost || f.last.Header.Get("Authorization") != "" {
		t.Fatalf("request = %s %s auth=%q", f.last.Method, f.last.URL.Path, f.last.Header.Get("Authorization"))
	}
	if f.sent != `{"code":"ABCD-1234"}` {
		t.Fatalf("body = %s", f.sent)
	}
	f.status, f.body = http.StatusUnauthorized, `{"error":"invalid or expired pairing code"}`
	if _, err := Pair(context.Background(), srv.URL, "x", "0.1.0"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a wrong code = %v, want ErrUnauthorized", err)
	}
}

func TestRequestsCarryTokenAndClient(t *testing.T) {
	t.Parallel()
	f, c := newFake(t, http.StatusOK, `{"server_time":"2026-10-15T10:00:00Z","libraries":[{"id":1,"name":"Books","root":"/books"}]}`)
	reply, err := c.Heartbeat(context.Background(), Status{Version: "0.1.0", Kinds: []string{"bridge.verify"}})
	if err != nil || len(reply.Libraries) != 1 || reply.Libraries[0].Root != "/books" {
		t.Fatalf("Heartbeat = %+v, %v", reply, err)
	}
	if f.last.Method != http.MethodPut || f.last.URL.Path != "/api/v1/bridge/status" {
		t.Fatalf("request = %s %s", f.last.Method, f.last.URL.Path)
	}
	if f.last.Header.Get("Authorization") != "Bearer tok-123" || !strings.HasPrefix(f.last.Header.Get(ClientHeader), "AudioSilo Bridge/0.1.0 (") {
		t.Fatalf("headers = %v", f.last.Header)
	}
	var st Status
	if err := json.Unmarshal([]byte(f.sent), &st); err != nil || st.Kinds[0] != "bridge.verify" {
		t.Fatalf("sent = %s", f.sent)
	}
}

func TestClaim(t *testing.T) {
	t.Parallel()
	f, c := newFake(t, http.StatusNoContent, "")
	job, err := c.Claim(context.Background(), []string{"bridge.verify", "sidecars.generate"}, time.Hour)
	if job != nil || err != nil {
		t.Fatalf("204 = %+v, %v", job, err)
	}
	q := f.last.URL.Query()
	if q.Get("wait") != "25" || strings.Join(q["kind"], ",") != "bridge.verify,sidecars.generate" {
		t.Fatalf("query = %v", q)
	}
	f.status, f.body = http.StatusOK, `{"id":7,"kind":"bridge.verify","library_id":1,"path":null,"params":{"hold_seconds":3},"state":"claimed","attempts":2}`
	job, err = c.Claim(context.Background(), []string{"bridge.verify"}, time.Second)
	if err != nil || job.ID != 7 || job.Attempts != 2 || job.Path != nil || *job.LibraryID != 1 || string(job.Params) != `{"hold_seconds":3}` {
		t.Fatalf("claim = %+v, %v", job, err)
	}
	f.status, f.body = http.StatusConflict, `{"error":"send a status first","code":"no_heartbeat"}`
	if _, err := c.Claim(context.Background(), []string{"bridge.verify"}, 0); !errors.Is(err, ErrNoHeartbeat) {
		t.Fatalf("no heartbeat = %v", err)
	}
}

func TestUpdateJob(t *testing.T) {
	t.Parallel()
	f, c := newFake(t, http.StatusOK, `{"id":7,"state":"running","cancel_requested":true}`)
	running := StateRunning
	job, err := c.UpdateJob(context.Background(), 7, Update{Attempt: 1, State: &running, Progress: json.RawMessage(`{"step":"stat"}`)})
	if err != nil || !job.CancelRequested {
		t.Fatalf("update = %+v, %v", job, err)
	}
	if f.last.Method != http.MethodPatch || f.last.URL.Path != "/api/v1/bridge/jobs/7" || f.sent != `{"attempt":1,"state":"running","progress":{"step":"stat"}}` {
		t.Fatalf("request = %s %s %s", f.last.Method, f.last.URL.Path, f.sent)
	}
	for _, code := range []string{"not_held", "finished"} {
		f.status, f.body = http.StatusConflict, `{"error":"x","code":"`+code+`"}`
		if _, err := c.UpdateJob(context.Background(), 7, Update{Attempt: 1}); !errors.Is(err, ErrNotHeld) {
			t.Errorf("409 %s = %v, want ErrNotHeld", code, err)
		}
	}
}

func TestBookAndRescan(t *testing.T) {
	t.Parallel()
	f, c := newFake(t, http.StatusOK, `{"library_id":1,"path":"A/B","files":[{"path":"A/B/1.mp3","size":10,"mod_time":5}]}`)
	b, err := c.Book(context.Background(), 1, "A/B")
	if err != nil || len(b.Files) != 1 || b.Files[0].Size != 10 {
		t.Fatalf("Book = %+v, %v", b, err)
	}
	if f.last.URL.Path != "/api/v1/bridge/libraries/1/book" || f.last.URL.Query().Get("path") != "A/B" {
		t.Fatalf("request = %s", f.last.URL)
	}
	if _, err := c.Book(context.Background(), 1, ""); err != nil || f.last.URL.RawQuery != "" {
		t.Fatalf("sample book: %v %q", err, f.last.URL.RawQuery)
	}
	f.status, f.body = http.StatusNotFound, `{"error":"no indexed book there","code":"no_book"}`
	if _, err := c.Book(context.Background(), 1, ""); !errors.Is(err, ErrNoBook) {
		t.Fatalf("no book = %v", err)
	}
	f.status, f.body = http.StatusOK, `{"path":"A/B"}`
	if p, err := c.Rescan(context.Background(), 1, "A/B/1.mp3"); err != nil || p != "A/B" || f.sent != `{"library_id":1,"path":"A/B/1.mp3"}` {
		t.Fatalf("Rescan = %q, %v, sent %s", p, err, f.sent)
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	f, c := newFake(t, http.StatusUnauthorized, `{"error":"invalid or expired token"}`)
	if _, err := c.Heartbeat(context.Background(), Status{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401 = %v", err)
	}
	f.status, f.body = http.StatusInternalServerError, `{"error":"could not save the status"}`
	var apiErr *APIError
	if _, err := c.Heartbeat(context.Background(), Status{}); !errors.As(err, &apiErr) || apiErr.Status != 500 || apiErr.Message != "could not save the status" {
		t.Fatalf("500 = %v", err)
	}
	f.status, f.body = http.StatusBadGateway, `<html>proxy</html>`
	if _, err := c.Heartbeat(context.Background(), Status{}); !errors.As(err, &apiErr) || apiErr.Message != "Bad Gateway" {
		t.Fatalf("an HTML 502 = %v", err)
	}
}

func TestServerAddress(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"http://audiosilo:8080", "https://books.example.com/", "http://192.168.1.2:8080/sub"} {
		if _, err := New(ok, "", "x"); err != nil {
			t.Errorf("allowed: %q = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "audiosilo:8080", "ftp://x", "http://u:p@x", "http://x/?q=1", "http:///nohost"} {
		if _, err := New(bad, "", "x"); err == nil {
			t.Errorf("denied: %q accepted", bad)
		}
	}
}
