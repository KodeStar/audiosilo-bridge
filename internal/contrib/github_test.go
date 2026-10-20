package contrib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testRepo = "KodeStar/audiosilo-meta"

// newTestClient wires a Client to a handler-backed httptest server, closed at
// test end.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-token")
}

// readJSON decodes a request body into a map for assertions.
func readJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}

func TestCreateIssue(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/"+testRepo+"/issues" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q", got)
		}
		body := readJSON(t, r)
		if body["title"] != "[characters] a-work" {
			t.Fatalf("title = %v", body["title"])
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"number":42,"html_url":"https://github.com/x/y/issues/42","labels":[{"name":"data"},{"name":"data:characters"}]}`)
	})
	iss, err := c.CreateIssue(context.Background(), testRepo, "[characters] a-work", "body", []string{"data", "data:characters"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if iss.Number != 42 || iss.URL != "https://github.com/x/y/issues/42" {
		t.Fatalf("issue = %+v", iss)
	}
	if len(iss.Labels) != 2 || iss.Labels[0] != "data" {
		t.Fatalf("labels = %v", iss.Labels)
	}
}

// TestLabelVerificationFlow: CreateIssue echoes the labels, but a later GetIssue
// shows them dropped (GitHub silently strips labels for a non-collaborator).
func TestLabelVerificationFlow(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/"+testRepo+"/issues":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":7,"html_url":"u","labels":[{"name":"data"},{"name":"data:characters"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+testRepo+"/issues/7":
			// Labels dropped.
			io.WriteString(w, `{"number":7,"html_url":"u","labels":[]}`)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	created, err := c.CreateIssue(context.Background(), testRepo, "t", "b", []string{"data", "data:characters"})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Labels) != 2 {
		t.Fatalf("create should echo labels, got %v", created.Labels)
	}
	got, err := c.GetIssue(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Labels) != 0 {
		t.Fatalf("labels should read as dropped on GET, got %v", got.Labels)
	}
}

func TestCreateGist(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/gists" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body := readJSON(t, r)
		if body["public"] != false {
			t.Fatalf("secret gist must be public=false, got %v", body["public"])
		}
		if body["description"] != "AudioSilo characters" {
			t.Fatalf("description = %v", body["description"])
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"files":{"characters.json":{"raw_url":"https://gist.githubusercontent.com/x/raw/y/characters.json"}}}`)
	})
	urls, err := c.CreateGist(context.Background(), "AudioSilo characters", map[string]string{"characters.json": "{}"}, true)
	if err != nil {
		t.Fatalf("CreateGist: %v", err)
	}
	if urls["characters.json"] != "https://gist.githubusercontent.com/x/raw/y/characters.json" {
		t.Fatalf("raw url = %v", urls)
	}
}

// TestFindIntakePR checks the head-branch query and merged_at -> Merged mapping.
func TestFindIntakePR(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/pulls" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("head"); got != "KodeStar:intake/issue-5" {
			t.Fatalf("head = %q", got)
		}
		if got := r.URL.Query().Get("state"); got != "all" {
			t.Fatalf("state = %q", got)
		}
		io.WriteString(w, `[{"number":123,"html_url":"https://github.com/x/y/pull/123","state":"closed","merged_at":"2026-07-17T00:00:00Z"}]`)
	})
	pr, found, err := c.FindIntakePR(context.Background(), testRepo, 5)
	if err != nil || !found {
		t.Fatalf("FindIntakePR found=%v err=%v", found, err)
	}
	if pr.Number != 123 || !pr.Merged {
		t.Fatalf("pr = %+v (merged_at should map to Merged)", pr)
	}
}

// TestFindIntakePRNone: an empty list is found=false, no error.
func TestFindIntakePRNone(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	_, found, err := c.FindIntakePR(context.Background(), testRepo, 5)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("expected found=false for an empty list")
	}
}

func TestGetPullMerged(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/pulls/123" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `{"number":123,"html_url":"u","state":"closed","merged":true,"base":{"sha":"b4se"},"head":{"sha":"h3ad"}}`)
	})
	pr, err := c.GetPull(context.Background(), testRepo, 123)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Merged || pr.State != "closed" || pr.BaseSHA != "b4se" || pr.HeadSHA != "h3ad" {
		t.Fatalf("pr = %+v", pr)
	}
}

func TestCompare(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/compare/b4se...h3ad" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `{"merge_base_commit":{"sha":"mb"},"files":[{"filename":"data/works/c/cat.json","status":"modified"},`+
			`{"filename":"data/works/c/dog.json","status":"renamed","previous_filename":"data/works/c/cow.json"}]}`)
	})
	got, err := c.Compare(context.Background(), testRepo, "b4se", "h3ad")
	if err != nil {
		t.Fatal(err)
	}
	want := []ChangedFile{
		{Filename: "data/works/c/cat.json"},
		{Filename: "data/works/c/dog.json", PreviousFilename: "data/works/c/cow.json"},
	}
	if got.MergeBase != "mb" || len(got.Files) != 2 || got.Files[0] != want[0] || got.Files[1] != want[1] {
		t.Fatalf("compare = %+v", got)
	}
}

func TestFileAt(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github.raw+json" {
			t.Fatalf("Accept = %q, want the raw media type", r.Header.Get("Accept"))
		}
		if r.URL.Query().Get("ref") != "abc123" {
			t.Fatalf("ref = %q", r.URL.Query().Get("ref"))
		}
		switch r.URL.Path {
		case "/repos/" + testRepo + "/contents/data/works/0/0.json":
			io.WriteString(w, `{"entries":{}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	got, found, err := c.FileAt(context.Background(), testRepo, "data/works/0/0.json", "abc123")
	if err != nil || !found || string(got) != `{"entries":{}}` {
		t.Fatalf("FileAt = %q found=%v err=%v", got, found, err)
	}
	_, found, err = c.FileAt(context.Background(), testRepo, "data/works/0/gone.json", "abc123")
	if err != nil || found {
		t.Fatalf("missing file: found=%v err=%v, want found=false and no error", found, err)
	}
}

func TestIssueComments(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/issues/7/comments" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `[{"body":"first","user":{"login":"a"}},{"body":"second","user":{"login":"github-actions[bot]"}}]`)
	})
	got, err := c.IssueComments(context.Background(), testRepo, 7)
	if err != nil || len(got) != 2 || got[1].Body != "second" || got[1].Author != "github-actions[bot]" {
		t.Fatalf("comments = %+v err=%v", got, err)
	}
}

// TestRateLimit: a 403 with x-ratelimit-remaining:0 becomes a *RateLimitError
// (and is not retried).
func TestRateLimit(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	})
	_, err := c.GetIssue(context.Background(), testRepo, 1)
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if rl.Status != http.StatusForbidden {
		t.Fatalf("rate-limit status = %d", rl.Status)
	}
	if calls != 1 {
		t.Fatalf("rate limit must not be retried, calls=%d", calls)
	}
	// A 403 that is NOT a rate limit (remaining header absent) is a plain APIError.
	c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"forbidden"}`)
	})
	var api *APIError
	if err := func() error { _, e := c2.GetIssue(context.Background(), testRepo, 1); return e }(); !errors.As(err, &api) {
		t.Fatalf("non-rate-limit 403 err = %v, want *APIError", err)
	}
}

// TestRetryOn5xx: the first attempt 500s, the retry succeeds.
func TestRetryOn5xx(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"message":"boom"}`)
			return
		}
		io.WriteString(w, `{"number":8,"html_url":"u","labels":[]}`)
	})
	iss, err := c.GetIssue(context.Background(), testRepo, 8)
	if err != nil {
		t.Fatalf("GetIssue after retry: %v", err)
	}
	if iss.Number != 8 {
		t.Fatalf("issue = %+v", iss)
	}
	if calls != 2 {
		t.Fatalf("expected exactly one retry, calls=%d", calls)
	}
}

// TestNoRetryOfPOSTOn5xx: a POST that 5xxs is sent once (GitHub may have made
// the issue before it failed).
func TestNoRetryOfPOSTOn5xx(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"message":"boom"}`)
	})
	var apiErr *APIError
	if _, err := c.CreateIssue(context.Background(), testRepo, "t", "b", nil); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("a POST was sent %d times", calls)
	}
}

// pagedList answers a list endpoint from rows, honouring per_page and page, and
// counts the pages it served.
func pagedList(t *testing.T, rows *[]map[string]any, pages *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if per <= 0 || page <= 0 {
			t.Errorf("unpaged list read: %s", r.URL.RawQuery)
			return
		}
		*pages++
		lo, hi := min((page-1)*per, len(*rows)), min(page*per, len(*rows))
		_ = json.NewEncoder(w).Encode((*rows)[lo:hi])
	}
}

// TestIssuesSince: issues created since the time, pull requests and older ones
// left out, read back page by page until a row made before since; a list that
// doesn't reach back within maxListPages pages is incomplete.
func TestIssuesSince(t *testing.T) {
	since := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var query string
	rows := []map[string]any{
		{"number": 9, "html_url": "u9", "title": "[corrections] A", "body": "b", "created_at": "2026-10-19T12:05:00Z"},
		{"number": 8, "html_url": "u8", "title": "pr", "created_at": "2026-10-19T12:04:00Z", "pull_request": map[string]any{}},
		{"number": 7, "html_url": "u7", "title": "old", "body": nil, "created_at": "2026-10-19T11:00:00Z"},
	}
	pages := 0
	list := pagedList(t, &rows, &pages)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		list(w, r)
	})
	got, complete, err := c.IssuesSince(context.Background(), testRepo, since)
	if err != nil || !complete || len(got) != 1 || got[0].Number != 9 || got[0].Body != "b" {
		t.Fatalf("got %+v complete %v err %v", got, complete, err)
	}
	if !strings.Contains(query, "sort=created") || !strings.Contains(query, "state=all") || !strings.Contains(query, "since=2026-10-19T12%3A00%3A00Z") {
		t.Fatalf("query %s", query)
	}
	// Two pages and more of pull requests made since: the issue past them is
	// found, and the row made before since ends the read.
	rows = nil
	for i := range 2*IssuesPage + 3 {
		rows = append(rows, map[string]any{"number": 1000 - i, "title": "pr", "created_at": "2026-10-19T13:00:00Z",
			"pull_request": map[string]any{}})
	}
	rows = append(rows, map[string]any{"number": 5, "title": "mine", "body": "x", "created_at": "2026-10-19T12:01:00Z"},
		map[string]any{"number": 4, "title": "old", "created_at": "2026-10-19T11:00:00Z"})
	for range IssuesPage {
		rows = append(rows, map[string]any{"number": 3, "title": "older", "created_at": "2026-10-18T11:00:00Z"})
	}
	pages = 0
	got, complete, err = c.IssuesSince(context.Background(), testRepo, since)
	if err != nil || !complete || len(got) != 1 || got[0].Number != 5 || pages != 3 {
		t.Fatalf("paged: got %+v complete %v err %v pages %d", got, complete, err, pages)
	}
	rows = nil
	for i := range maxListPages * IssuesPage {
		rows = append(rows, map[string]any{"number": 100 + i, "title": "x", "created_at": "2026-10-19T13:00:00Z"})
	}
	pages = 0
	if _, complete, err := c.IssuesSince(context.Background(), testRepo, since); err != nil || complete || pages != maxListPages {
		t.Fatalf("more newer issues than the Bridge reads: complete %v err %v pages %d", complete, err, pages)
	}
}

// IssuesSinceBy narrows the list to one creator; Login reads /user once.
func TestIssuesSinceByAndLogin(t *testing.T) {
	var query string
	users := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			users++
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "kodestar"})
			return
		}
		query = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode([]any{})
	})
	for range 2 {
		login, err := c.Login(context.Background())
		if err != nil || login != "kodestar" {
			t.Fatalf("login %q %v", login, err)
		}
	}
	if users != 1 {
		t.Fatalf("GET /user %d times, want once", users)
	}
	login := "kodestar"
	if _, complete, err := c.IssuesSinceBy(context.Background(), testRepo, time.Now(), login); err != nil || !complete {
		t.Fatalf("complete %v err %v", complete, err)
	}
	if !strings.Contains(query, "creator=kodestar") {
		t.Fatalf("query %s", query)
	}
}

// TestGistsSince: the gists made since the time, with their description and
// files; the list's order isn't documented, so an older gist ends nothing and it
// is read to its end, incomplete past maxListPages pages.
func TestGistsSince(t *testing.T) {
	since := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	rows := []map[string]any{
		{"id": "g1", "created_at": "2026-10-19T11:00:00Z", "files": map[string]any{}},
		{"id": "g2", "description": "AudioSilo characters", "created_at": "2026-10-19T12:05:00Z", "files": map[string]any{
			"characters.json": map[string]any{"raw_url": "raw2", "size": 70000}}},
	}
	pages := 0
	var path string
	list := pagedList(t, &rows, &pages)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path + "?" + r.URL.RawQuery
		list(w, r)
	})
	got, complete, err := c.GistsSince(context.Background(), since)
	if err != nil || !complete || len(got) != 1 || got[0].Description != "AudioSilo characters" ||
		got[0].Files["characters.json"] != "raw2" {
		t.Fatalf("got %+v complete %v err %v", got, complete, err)
	}
	if !strings.HasPrefix(path, "/gists?") || !strings.Contains(path, "since=2026-10-19T12%3A00%3A00Z") {
		t.Fatalf("path %s", path)
	}
	rows = nil
	for i := range maxListPages * GistsPage {
		rows = append(rows, map[string]any{"id": strconv.Itoa(i), "created_at": "2026-10-19T13:00:00Z"})
	}
	pages = 0
	if _, complete, err := c.GistsSince(context.Background(), since); err != nil || complete || pages != maxListPages {
		t.Fatalf("more newer gists than the Bridge reads: complete %v err %v pages %d", complete, err, pages)
	}
}

// A list that fails partway returns the rows read before the failing page with
// the error, never complete: a lookup decides on them first.
func TestListsKeepTheRowsBeforeAFailedPage(t *testing.T) {
	since := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var rows []map[string]any
	pages := 0
	list := pagedList(t, &rows, &pages)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page >= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		list(w, r)
	})
	for i := range IssuesPage {
		rows = append(rows, map[string]any{"number": 100 + i, "title": "x", "created_at": "2026-10-19T13:00:00Z"})
	}
	var apiErr *APIError
	issues, complete, err := c.IssuesSince(context.Background(), testRepo, since)
	if !errors.As(err, &apiErr) || complete || len(issues) != IssuesPage || issues[0].Number != 100 {
		t.Fatalf("issues: %d rows, complete %v, err %v", len(issues), complete, err)
	}
	rows = nil
	for i := range GistsPage {
		rows = append(rows, map[string]any{"id": strconv.Itoa(i), "created_at": "2026-10-19T13:00:00Z"})
	}
	gists, complete, err := c.GistsSince(context.Background(), since)
	if !errors.As(err, &apiErr) || complete || len(gists) != GistsPage {
		t.Fatalf("gists: %d rows, complete %v, err %v", len(gists), complete, err)
	}
}

// Now is GitHub's clock, learned from the Date of its answers: a Client that has
// had none asks GET /rate_limit once.
func TestNow(t *testing.T) {
	behind := 5 * time.Minute
	probes := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rate_limit" {
			probes++
		}
		w.Header().Set("Date", time.Now().Add(-behind).UTC().Format(http.TimeFormat))
		io.WriteString(w, `{}`)
	})
	for range 2 {
		got := c.Now(context.Background())
		if d := time.Now().Add(-behind).Sub(got); d < -time.Second || d > 2*time.Second {
			t.Fatalf("Now = %v, %v off GitHub's clock", got, d)
		}
	}
	if probes != 1 {
		t.Fatalf("GET /rate_limit %d times, want once", probes)
	}
	// An answer from any call teaches it, and a failed probe leaves the
	// Bridge's own clock.
	d := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Add(-behind).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := d.GetIssue(context.Background(), testRepo, 1); err == nil {
		t.Fatal("a 404 should fail")
	}
	if got := d.Now(context.Background()); time.Since(got) < behind-2*time.Second {
		t.Fatalf("a 404's Date was not learned: %v", got)
	}
	gone := NewClient("http://127.0.0.1:1", "")
	if got := gone.Now(context.Background()); time.Since(got).Abs() > time.Second {
		t.Fatalf("unreachable: Now = %v, want the Bridge's clock", got)
	}
}

// StaleFiling ages a marker by the GitHub clock the Client learned (two hours
// behind the Bridge's here) and never asks for it: false until GitHub answers.
func TestStaleFiling(t *testing.T) {
	behind := 2 * time.Hour
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Date", time.Now().Add(-behind).UTC().Format(http.TimeFormat))
		io.WriteString(w, `{"number":1}`)
	})
	githubNow := func() time.Time { return time.Now().Add(-behind) }
	if c.StaleFiling(githubNow().Add(-FilingStale-time.Hour)) || calls != 0 {
		t.Fatalf("before any answer: stale, or %d calls", calls)
	}
	if _, err := c.GetIssue(context.Background(), testRepo, 1); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		name  string
		since time.Time
		stale bool
	}{
		{"under a day by GitHub's clock (over one by the Bridge's)", githubNow().Add(-FilingStale + time.Hour), false},
		{"over a day", githubNow().Add(-FilingStale - time.Hour), true},
		{"no marker", time.Time{}, false},
	} {
		if got := c.StaleFiling(m.since); got != m.stale {
			t.Errorf("%s: StaleFiling = %v", m.name, got)
		}
	}
}

// NothingFiled: only a refusal (4xx, rate limit) or no connection at all rules
// out that GitHub made it.
func TestNothingFiled(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&APIError{Status: http.StatusUnprocessableEntity}, true},
		{&RateLimitError{Status: http.StatusForbidden}, true},
		{fmt.Errorf("wrapped: %w", &APIError{Status: http.StatusNotFound}), true},
		{&APIError{Status: http.StatusBadGateway}, false},
		{context.DeadlineExceeded, false},
		{io.ErrUnexpectedEOF, false},
		// No connection: the request never left.
		{&url.Error{Op: "Post", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}}, true},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{&net.OpError{Op: "proxyconnect", Err: syscall.ECONNREFUSED}, true},
		// The connection broke after the request may have been written.
		{&url.Error{Op: "Post", Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}, false},
	} {
		if got := NothingFiled(c.err); got != c.want {
			t.Errorf("NothingFiled(%v) = %v", c.err, got)
		}
	}
}

// TestPersistent5xx: a 5xx on both attempts surfaces an *APIError (one retry
// only) carrying the status and trimmed body, never the token.
func TestPersistent5xx(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"message":"bad gateway"}`)
	})
	_, err := c.GetIssue(context.Background(), testRepo, 1)
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if api.Status != http.StatusBadGateway {
		t.Fatalf("status = %d", api.Status)
	}
	if calls != 2 {
		t.Fatalf("expected 1 retry then give up, calls=%d", calls)
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatalf("error leaked the token: %v", err)
	}
}

// TestUnauthenticatedClient: an empty token sends no Authorization header (the
// poller reads the public repo without a credential).
func TestUnauthenticatedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("unauthenticated client must send no Authorization header, got %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"number":1,"html_url":"u","state":"open","merged":false}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "")
	if _, err := c.GetPull(context.Background(), testRepo, 1); err != nil {
		t.Fatal(err)
	}
}
