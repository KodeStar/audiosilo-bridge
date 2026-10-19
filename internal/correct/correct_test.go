package correct

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
)

type fakeMeta struct {
	cov metaops.Coverage
	err error
}

func (f fakeMeta) FreshCoverageForWork(context.Context, string) (metaops.Coverage, error) {
	return f.cov, f.err
}

type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKV) GetSetting(_ context.Context, key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *memKV) SetSetting(_ context.Context, key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = value
	return nil
}

type tokens struct {
	token, from string
	err         error
	refused     []string
}

func (t *tokens) Resolve(context.Context) (string, string, error) { return t.token, t.from, t.err }
func (t *tokens) Refused(_ context.Context, from, _ string)       { t.refused = append(t.refused, from) }

// gh is a fake GitHub issues API: POST files, GET lists (newest first).
type gh struct {
	mu         sync.Mutex
	status     int
	listStatus int
	issues     []map[string]any
	auth       []string
}

func (g *gh) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/KodeStar/audiosilo-meta/issues" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.listStatus != 0 {
				w.WriteHeader(g.listStatus)
				return
			}
			var rows []map[string]any
			for i := len(g.issues) - 1; i >= 0; i-- {
				is := g.issues[i]
				rows = append(rows, map[string]any{"number": 42 + i, "html_url": "https://github.com/KodeStar/audiosilo-meta/issues/" + strconv.Itoa(42+i),
					"title": is["title"], "body": is["body"], "created_at": is["created_at"]})
			}
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		defer g.mu.Unlock()
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		if g.status != 0 {
			w.WriteHeader(g.status)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
			return
		}
		body["created_at"] = "2026-10-19T12:00:00Z"
		g.issues = append(g.issues, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 41 + len(g.issues), "html_url": "https://github.com/KodeStar/audiosilo-meta/issues/42", "state": "open"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *gh) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.issues)
}

const params = `{"work_id":"existing-work","asin":"B000000001","region":"us",
 "changes":[{"field":"title","current":"Existing Work","corrected":"The Existing Work"},
            {"field":"author","current":"Jane Doe","corrected":"Jane Doe, John Smith"},
            {"field":"published","current":"","corrected":"2020-03-04"}],
 "evidence":"Edited by the server admin.","source":"audiosilo-server/2.4.0"}`

func cov() metaops.Coverage {
	return metaops.Coverage{Available: true, Known: true, WorkID: "existing-work", WorkTitle: "Existing Work",
		Recordings: []metaops.RecordingRef{{ID: "john-smith-2020", ASINs: []metaops.RegionASIN{{Region: "us", ASIN: "B000000001"}}}}}
}

func newHandler(t *testing.T, g *gh, tok *tokens, kv *memKV, m Meta) *Handler {
	t.Helper()
	return New(Options{Meta: m, Tokens: tok, BaseURL: g.server(t).URL, CoreRepo: "KodeStar/audiosilo-meta", KV: kv,
		ServerID: func() string { return "srv-1" }})
}

func TestCorrectFilesOnceAndResumes(t *testing.T) {
	g, kv := &gh{}, &memKV{m: map[string]string{}}
	tok := &tokens{token: "ghu_user", from: contrib.FromDevice}
	h := newHandler(t, g, tok, kv, fakeMeta{cov: cov()})
	job := jobs.Job{ID: 7, Kind: Kind, Params: json.RawMessage(params)}
	out, err := h.Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	r := out.(Result)
	if r.IssueNumber != 42 || r.Changes != 3 || strings.Join(r.Auto, ",") != "title,published" || strings.Join(r.Maintainer, ",") != "author" {
		t.Fatalf("result = %+v", r)
	}
	g.mu.Lock()
	body := g.issues[0]["body"].(string)
	title := g.issues[0]["title"].(string)
	auth := g.auth[0]
	g.mu.Unlock()
	// The ASIN picked the recording; the source is the server's stamp.
	if title != "[corrections] Existing Work" || auth != "Bearer ghu_user" ||
		!strings.Contains(body, `"record":"data/works/existing-work/recordings/john-smith-2020","field":"release_date"`) ||
		!strings.Contains(body, `"source":"audiosilo-server/2.4.0"`) {
		t.Fatalf("issue %q (%s):\n%s", title, auth, body)
	}
	// A resumed (or repeated) run reports the issue it filed and files nothing.
	job.Resumed = true
	again, err := h.Run(context.Background(), job, jobs.LocalReporter())
	if err != nil || again.(Result).IssueNumber != 42 || g.count() != 1 {
		t.Fatalf("again = %+v %v, issues %d", again, err, g.count())
	}
	// Another server's job 7 is another job.
	h.o.ServerID = func() string { return "srv-2" }
	if _, err := h.Run(context.Background(), job, jobs.LocalReporter()); err != nil || g.count() != 2 {
		t.Fatalf("another server's job = %v, issues %d", err, g.count())
	}
}

func TestCorrectFailures(t *testing.T) {
	cases := map[string]struct {
		params string
		meta   Meta
		tok    *tokens
		status int
		want   string
	}{
		"unknown field":   {params: `{"work_id":"w","changes":[],"evidence":"e","extra":1}`, want: "bad params"},
		"unknown change":  {params: `{"work_id":"w","changes":[{"field":"title","corrected":"x","why":1}],"evidence":"e"}`, want: "bad params"},
		"no work":         {params: `{"changes":[],"evidence":"e"}`, want: "work_id is required"},
		"meta off":        {meta: fakeMeta{err: metaops.ErrDisabled}, want: "metadata service is off"},
		"no meta":         {want: "metadata service is off"},
		"work gone":       {meta: fakeMeta{err: metaops.ErrWorkNotFound}, want: "Match the book again"},
		"bad field":       {params: strings.Replace(params, `"field":"title"`, `"field":"subtitle"`, 1), want: "can't be filed"},
		"no sign-in":      {tok: &tokens{err: contrib.ErrNoCredential}, want: "Accounts tab"},
		"refused sign-in": {status: http.StatusUnauthorized, want: "sign in again"},
		"github says no":  {status: http.StatusUnprocessableEntity, want: "could not file"},
		"github errors":   {status: http.StatusBadGateway, want: "could not file"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			g := &gh{status: c.status}
			tok := c.tok
			if tok == nil {
				tok = &tokens{token: "ghu_user", from: contrib.FromDevice}
			}
			m := c.meta
			if m == nil && name != "no meta" {
				m = fakeMeta{cov: cov()}
			}
			p := c.params
			if p == "" {
				p = params
			}
			kv := &memKV{m: map[string]string{}}
			_, err := newHandler(t, g, tok, kv, m).Run(context.Background(), jobs.Job{ID: 1, Params: json.RawMessage(p)}, jobs.LocalReporter())
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), "ghu_user") {
				t.Fatal("the token reached the error")
			}
			if c.status == http.StatusUnauthorized && (len(tok.refused) != 1 || tok.refused[0] != contrib.FromDevice) {
				t.Fatalf("a 401 wasn't reported as a refusal: %v", tok.refused)
			}
			since, marked := filingSince(kv.m["meta.correct/srv-1/1"])
			marked = marked && !since.IsZero()
			if c.status >= http.StatusInternalServerError && !marked {
				t.Fatal("a server error may come after the issue is made: the filing marker must stay")
			}
			if c.status != 0 && c.status < http.StatusInternalServerError && marked {
				t.Fatal("GitHub's refusal left the filing marker: a retry couldn't file")
			}
		})
	}
}

// A run that stopped while filing (the marker left, no result) never files
// twice: it adopts the issue it finds among those made since, files when there
// is none, and leaves it to a person when it can't tell.
func TestCorrectNeverFilesTwice(t *testing.T) {
	ctx := context.Background()
	const key = "meta.correct/srv-1/3"
	marker := filingMarker + " 2026-10-19T12:00:30Z"
	job := jobs.Job{ID: 3, Params: json.RawMessage(params), Resumed: true}
	run := func(g *gh, kv *memKV) (any, error) {
		h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
		return h.Run(ctx, job, jobs.LocalReporter())
	}

	// A marker without a time (no way to look): a person decides.
	g, kv := &gh{}, &memKV{m: map[string]string{key: filingMarker}}
	if _, err := run(g, kv); !errors.Is(err, errFiledBefore) || g.count() != 0 {
		t.Fatalf("no time: err = %v, issues %d", err, g.count())
	}

	// The interrupted run had filed it: adopted, nothing filed again.
	g, kv = &gh{}, &memKV{m: map[string]string{}}
	if _, err := run(g, kv); err != nil || g.count() != 1 {
		t.Fatalf("first filing: %v, issues %d", err, g.count())
	}
	kv.m[key] = marker
	out, err := run(g, kv)
	if err != nil || g.count() != 1 || out.(Result).IssueNumber != 42 || out.(Result).Changes != 3 {
		t.Fatalf("adopt: %+v %v, issues %d", out, err, g.count())
	}
	if !strings.Contains(kv.m[key], `"issue_number":42`) {
		t.Fatalf("the adopted issue isn't recorded: %q", kv.m[key])
	}

	// It hadn't: filed once now.
	g, kv = &gh{}, &memKV{m: map[string]string{key: marker}}
	if _, err := run(g, kv); err != nil || g.count() != 1 {
		t.Fatalf("refile: %v, issues %d", err, g.count())
	}

	// A batch for the same record with another body, or a list that can't be
	// read: a person decides.
	g, kv = &gh{}, &memKV{m: map[string]string{key: marker}}
	g.issues = []map[string]any{{"title": "[corrections] Existing Work", "created_at": "2026-10-19T12:01:00Z",
		"body": "### Record\n\nhttps://meta.audiosilo.app/works/existing-work\n\n### Changes\n\nother"}}
	if _, err := run(g, kv); !errors.Is(err, errFiledBefore) || g.count() != 1 {
		t.Fatalf("other body: err = %v, issues %d", err, g.count())
	}
	g, kv = &gh{listStatus: http.StatusBadGateway}, &memKV{m: map[string]string{key: marker}}
	if _, err := run(g, kv); !errors.Is(err, errFiledBefore) || g.count() != 0 {
		t.Fatalf("list fails: err = %v, issues %d", err, g.count())
	}
}

func TestFilingSince(t *testing.T) {
	for v, want := range map[string]bool{"filing": true, "filing 2026-10-19T12:00:00Z": true, "filings": false, "": false, "{}": false} {
		if _, ok := filingSince(v); ok != want {
			t.Errorf("filingSince(%q) ok = %v", v, ok)
		}
	}
	if at, _ := filingSince("filing 2026-10-19T12:00:00.5Z"); !at.Equal(time.Date(2026, 10, 19, 12, 0, 0, 5e8, time.UTC)) {
		t.Fatal(at)
	}
}

func TestRecordingFor(t *testing.T) {
	c := cov()
	c.Recordings = append(c.Recordings, metaops.RecordingRef{ID: "other", ISBNs: []string{"978-1-4736-4763-3"},
		ASINs: []metaops.RegionASIN{{Region: "uk", ASIN: "B000000001"}}})
	for _, tc := range []struct {
		p    Params
		want string
	}{
		{Params{RecordingID: "given"}, "given"},
		{Params{ASIN: "b000000001", Region: "us"}, "john-smith-2020"},
		{Params{ASIN: "B000000001"}, ""}, // two regions carry it
		{Params{ISBN: "9781473647633"}, "other"},
		{Params{}, ""},
	} {
		if got := recordingFor(tc.p, c); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc.p, got, tc.want)
		}
	}
}
