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

func (k *memKV) DeleteSetting(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, key)
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
	// failFromPage fails the list from that page on (0: never); drop makes the
	// next issues but loses the answer (the connection closes).
	failFromPage int
	drop         bool
	issues       []map[string]any
	auth         []string
	// date is GitHub's clock (every answer's Date; fakeNow when zero), so a
	// marker's age doesn't depend on the day the tests run.
	date time.Time
}

// fakeNow is the fake GitHub's clock: shortly after the issues it made.
var fakeNow = time.Date(2026, 10, 19, 12, 10, 0, 0, time.UTC)

func (g *gh) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		date := g.date
		g.mu.Unlock()
		if date.IsZero() {
			date = fakeNow
		}
		w.Header().Set("Date", date.Format(http.TimeFormat))
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
			if page, _ := strconv.Atoi(r.URL.Query().Get("page")); g.failFromPage > 0 && page >= g.failFromPage {
				w.WriteHeader(http.StatusBadGateway)
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
		body["created_at"] = date.Format(time.RFC3339)
		g.issues = append(g.issues, body)
		if g.drop {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
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
		"github errors":   {status: http.StatusBadGateway, want: "didn't confirm"},
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
			marked := kv.m[filingKeyWork] != ""
			if c.status >= http.StatusInternalServerError && !marked {
				t.Fatal("a server error may come after the issue is made: the filing marker must stay")
			}
			if c.status != 0 && c.status < http.StatusInternalServerError && marked {
				t.Fatal("GitHub's refusal left the filing marker: a retry couldn't file")
			}
		})
	}
}

// filingKeyWork is the filing marker of params' book when the job names no
// library and path.
const filingKeyWork = "meta.correct.filing/srv-1/work/existing-work"

// otherBatch is a correction batch for the same record as params' with another
// body (the record as it was, or another admin's), made a minute after the
// marker's run.
func otherBatch() map[string]any {
	return map[string]any{"title": "[corrections] Existing Work", "created_at": "2026-10-19T12:01:00Z",
		"body": "### Record\n\nhttps://meta.audiosilo.app/works/existing-work\n\n### Changes\n\nother"}
}

// ownSum is contrib.IssueSum of the batch params file.
func ownSum(t *testing.T) string {
	t.Helper()
	p, err := ParseParams(json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	c := cov()
	title, body, _, _, err := contrib.CorrectionBatchIssue(contrib.CorrectionBatch{WorkID: c.WorkID, WorkTitle: c.WorkTitle,
		RecordingID: recordingFor(p, c), Changes: p.Changes, Evidence: p.Evidence, Source: p.Source})
	if err != nil {
		t.Fatal(err)
	}
	return contrib.IssueSum(title, body)
}

// markerAt is a filing marker of params' batch set at at.
func markerAt(t *testing.T, at time.Time) string {
	t.Helper()
	raw, err := json.Marshal(filing{Since: at, Sum: ownSum(t), Job: 3})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A filing that stopped (the marker left, no result) never files twice: it
// adopts the issue it finds among those made since, files when there is none,
// and leaves it to a person when it can't tell (under a day).
func TestCorrectNeverFilesTwice(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 19, 12, 0, 30, 0, time.UTC) // the interrupted run's marker
	marker := markerAt(t, at)
	job := jobs.Job{ID: 3, Params: json.RawMessage(params), Resumed: true}
	run := func(g *gh, kv *memKV) (any, error) {
		h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
		return h.Run(ctx, job, jobs.LocalReporter())
	}

	// A marker without a time (no way to look): a person decides.
	g, kv := &gh{}, &memKV{m: map[string]string{filingKeyWork: "{}"}}
	if _, err := run(g, kv); !errors.Is(err, errFiledBefore) || g.count() != 0 {
		t.Fatalf("no time: err = %v, issues %d", err, g.count())
	}

	// The interrupted run had filed it: adopted, nothing filed again, the
	// marker cleared.
	g, kv = &gh{}, &memKV{m: map[string]string{}}
	if _, err := run(g, kv); err != nil || g.count() != 1 || kv.m[filingKeyWork] != "" {
		t.Fatalf("first filing: %v, issues %d, marker %q", err, g.count(), kv.m[filingKeyWork])
	}
	delete(kv.m, "meta.correct/srv-1/3")
	kv.m[filingKeyWork] = marker
	out, err := run(g, kv)
	if err != nil || g.count() != 1 || out.(Result).IssueNumber != 42 || out.(Result).Changes != 3 {
		t.Fatalf("adopt: %+v %v, issues %d", out, err, g.count())
	}
	if !strings.Contains(kv.m["meta.correct/srv-1/3"], `"issue_number":42`) || kv.m[filingKeyWork] != "" {
		t.Fatalf("the adopted issue isn't recorded: %q, marker %q", kv.m["meta.correct/srv-1/3"], kv.m[filingKeyWork])
	}

	// It hadn't: filed once now.
	g, kv = &gh{}, &memKV{m: map[string]string{filingKeyWork: marker}}
	if _, err := run(g, kv); err != nil || g.count() != 1 {
		t.Fatalf("refile: %v, issues %d", err, g.count())
	}

	// A batch for the same record with another body, or a list that can't be
	// read: a person decides; a 401 is the sign-in refused.
	g, kv = &gh{issues: []map[string]any{otherBatch()}}, &memKV{m: map[string]string{filingKeyWork: marker}}
	if _, err := run(g, kv); !errors.Is(err, errFiledBefore) || g.count() != 1 {
		t.Fatalf("other body: err = %v, issues %d", err, g.count())
	}
	g, kv = &gh{listStatus: http.StatusBadGateway}, &memKV{m: map[string]string{filingKeyWork: marker}}
	if _, err := run(g, kv); !errors.Is(err, errFiledBefore) || g.count() != 0 {
		t.Fatalf("list fails: err = %v, issues %d", err, g.count())
	}
	g, kv = &gh{listStatus: http.StatusUnauthorized}, &memKV{m: map[string]string{filingKeyWork: marker}}
	if _, err := run(g, kv); !errors.Is(err, errRefused) || g.count() != 0 {
		t.Fatalf("list refused: err = %v, issues %d", err, g.count())
	}

	// The marker's age by GitHub's clock: just under a day, a lookup that can't
	// tell stops the job; just over (contrib.FilingStale), a list that can't be
	// read or can't reach back to the marker files again, while a batch for the
	// same record with another body (maybe the run's own) still stops it, even
	// on a page read before one that failed.
	others := func() []map[string]any { // a whole page of other issues made since, on every page
		rows := make([]map[string]any, contrib.IssuesPage)
		for i := range rows {
			rows[i] = map[string]any{"title": "other", "body": "", "created_at": "2026-10-19T12:05:00Z"}
		}
		return rows
	}
	for _, c := range []struct {
		name  string
		gh    func() *gh
		stale bool // files again over a day
	}{
		{"an unreadable list", func() *gh { return &gh{listStatus: http.StatusBadGateway} }, true},
		{"an incomplete list", func() *gh { return &gh{issues: others()} }, true},
		{"another body", func() *gh { return &gh{issues: []map[string]any{otherBatch()}} }, false},
		{"another body before a failed page", func() *gh {
			return &gh{issues: append(others(), otherBatch()), failFromPage: 2}
		}, false},
	} {
		for _, age := range []time.Duration{contrib.FilingStale - time.Hour, contrib.FilingStale + time.Hour} {
			g, kv := c.gh(), &memKV{m: map[string]string{filingKeyWork: marker}}
			g.date = at.Add(age)
			before := g.count()
			files := c.stale && age > contrib.FilingStale
			_, err := run(g, kv)
			switch {
			case files && (err != nil || g.count() != before+1):
				t.Fatalf("%s, %v old: err = %v, issues %d -> %d", c.name, age, err, before, g.count())
			case !files && (!errors.Is(err, errFiledBefore) || g.count() != before):
				t.Fatalf("%s, %v old: err = %v, issues %d -> %d", c.name, age, err, before, g.count())
			}
		}
	}
}

// The run's own issue on a page read before one that failed is adopted.
func TestCorrectAdoptsItsIssueBeforeAFailedPage(t *testing.T) {
	ctx := context.Background()
	g, kv := &gh{}, &memKV{m: map[string]string{}}
	h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	job := jobs.Job{ID: 3, Params: json.RawMessage(params)}
	if _, err := h.Run(ctx, job, jobs.LocalReporter()); err != nil || g.count() != 1 {
		t.Fatalf("first filing: %v, issues %d", err, g.count())
	}
	g.mu.Lock()
	for range contrib.IssuesPage {
		g.issues = append(g.issues, map[string]any{"title": "other", "body": "", "created_at": "2026-10-19T12:05:00Z"})
	}
	g.failFromPage = 2
	g.mu.Unlock()
	delete(kv.m, "meta.correct/srv-1/3")
	kv.m[filingKeyWork] = markerAt(t, time.Date(2026, 10, 19, 12, 0, 30, 0, time.UTC))
	out, err := h.Run(ctx, job, jobs.LocalReporter())
	if err != nil || out.(Result).IssueNumber != 42 || g.count() != 1+contrib.IssuesPage {
		t.Fatalf("%+v %v, issues %d", out, err, g.count())
	}
}

// A batch for the same record with another body listed before the run's own
// (newer: another admin's) doesn't hide it: the run's issue is adopted, whatever
// the marker's age.
func TestCorrectAdoptsItsIssuePastAnotherBatch(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 19, 12, 0, 30, 0, time.UTC)
	job := jobs.Job{ID: 3, Params: json.RawMessage(params), Resumed: true}
	for _, date := range []time.Time{fakeNow, at.Add(contrib.FilingStale + time.Hour)} {
		g, kv := &gh{}, &memKV{m: map[string]string{}}
		h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
		if _, err := h.Run(ctx, job, jobs.LocalReporter()); err != nil || g.count() != 1 {
			t.Fatalf("first filing: %v, issues %d", err, g.count())
		}
		g.mu.Lock()
		g.issues = append(g.issues, otherBatch()) // listed first (newest first)
		g.date = date
		g.mu.Unlock()
		delete(kv.m, "meta.correct/srv-1/3")
		kv.m[filingKeyWork] = markerAt(t, at)
		out, err := h.Run(ctx, job, jobs.LocalReporter())
		if err != nil || g.count() != 2 || out.(Result).IssueNumber != 42 {
			t.Fatalf("at %v: %+v %v, issues %d", date, out, err, g.count())
		}
	}
}

// The live repro: the issue POST's answer is lost (GitHub made it), the job
// fails in words, and the admin's NEW job for the same book adopts that issue.
// A new job with other corrections finds the first batch filed and files its own.
func TestCorrectNewJobForTheBookAdoptsTheLostIssue(t *testing.T) {
	ctx := context.Background()
	lib, path := int64(2), "Author/Book"
	const key = "meta.correct.filing/srv-1/2/Author/Book"
	g, kv := &gh{drop: true}, &memKV{m: map[string]string{}}
	h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	job := func(id int64, p string) jobs.Job {
		return jobs.Job{ID: id, LibraryID: &lib, Path: &path, Params: json.RawMessage(p)}
	}
	_, err := h.Run(ctx, job(10, params), jobs.LocalReporter())
	if !errors.Is(err, errNoAnswer) || g.count() != 1 || kv.m[key] == "" {
		t.Fatalf("lost answer: %v, issues %d, marker %q", err, g.count(), kv.m[key])
	}
	if strings.Contains(err.Error(), "contrib") || strings.Contains(err.Error(), "POST") {
		t.Fatalf("a raw error reached the console: %v", err)
	}
	g.mu.Lock()
	g.drop = false
	g.mu.Unlock()
	out, err := h.Run(ctx, job(11, params), jobs.LocalReporter())
	if err != nil || g.count() != 1 || out.(Result).IssueNumber != 42 || kv.m[key] != "" {
		t.Fatalf("new job: %+v %v, issues %d, marker %q", out, err, g.count(), kv.m[key])
	}
	if _, left := kv.m[key]; left {
		t.Fatal("a confirmed filing's marker is emptied, not deleted")
	}

	// Other corrections lost the same way, then a third job with others again:
	// the second batch is found filed (done), so the third files its own.
	other := strings.Replace(params, `"corrected":"The Existing Work"`, `"corrected":"Existing Work, The"`, 1)
	g.mu.Lock()
	g.drop = true
	g.mu.Unlock()
	if _, err := h.Run(ctx, job(12, other), jobs.LocalReporter()); !errors.Is(err, errNoAnswer) || g.count() != 2 {
		t.Fatalf("second lost answer: %v, issues %d", err, g.count())
	}
	g.mu.Lock()
	g.drop = false
	g.mu.Unlock()
	third := strings.Replace(params, `"corrected":"2020-03-04"`, `"corrected":"2020-03-05"`, 1)
	if _, err := h.Run(ctx, job(13, third), jobs.LocalReporter()); err != nil || g.count() != 3 || kv.m[key] != "" {
		t.Fatalf("third job: %v, issues %d, marker %q", err, g.count(), kv.m[key])
	}
}

// A lookup that finds nothing under contrib.FilingSettle after the marker
// waits: GitHub may still be making the issue.
func TestCorrectLookupSettles(t *testing.T) {
	ctx := context.Background()
	g, kv := &gh{}, &memKV{m: map[string]string{filingKeyWork: markerAt(t, fakeNow)}}
	h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	job := jobs.Job{ID: 3, Params: json.RawMessage(params)}
	if _, err := h.Run(ctx, job, jobs.LocalReporter()); !errors.Is(err, errSettling) || g.count() != 0 {
		t.Fatalf("moments after: %v, issues %d", err, g.count())
	}
	h = newHandler(t, &gh{date: fakeNow.Add(2 * contrib.FilingSettle)}, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	if _, err := h.Run(ctx, job, jobs.LocalReporter()); err != nil {
		t.Fatalf("settled: %v", err)
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
