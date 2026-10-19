package correct

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

// gh is a fake GitHub issues API.
type gh struct {
	mu     sync.Mutex
	status int
	issues []map[string]any
	auth   []string
}

func (g *gh) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/KodeStar/audiosilo-meta/issues" {
			http.NotFound(w, r)
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
			if c.status != 0 && kv.m["meta.correct/srv-1/1"] == filingMarker {
				t.Fatal("GitHub's answer left the filing marker: a retry couldn't file")
			}
		})
	}
}

// A run that stopped while filing (the marker left, no result) never files
// again: the issue may be there.
func TestCorrectNeverFilesTwice(t *testing.T) {
	g, kv := &gh{}, &memKV{m: map[string]string{"meta.correct/srv-1/3": filingMarker}}
	h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	_, err := h.Run(context.Background(), jobs.Job{ID: 3, Params: json.RawMessage(params), Resumed: true}, jobs.LocalReporter())
	if !errors.Is(err, errFiledBefore) || g.count() != 0 {
		t.Fatalf("err = %v, issues %d", err, g.count())
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
