package correct

import (
	"cmp"
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
	// failSet fails each SetSetting of a key with this prefix ("": none).
	failSet string
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
	if k.failSet != "" && strings.HasPrefix(key, k.failSet) {
		return errors.New("the disk is full")
	}
	k.m[key] = value
	return nil
}

// markers is the filing markers kept (key -> value).
func (k *memKV) markers() map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]string{}
	for key, v := range k.m {
		if strings.HasPrefix(key, "meta.correct.filing/") {
			out[key] = v
		}
	}
	return out
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

// gh is a fake GitHub issues API: POST files, GET lists (newest first, paged as
// asked).
type gh struct {
	mu         sync.Mutex
	status     int
	listStatus int
	// failFromPage fails the list from that page on (0: never) with failStatus
	// (502 when 0); drop makes the next issues but loses the answer (the
	// connection closes).
	failFromPage int
	failStatus   int
	drop         bool
	// limited marks every answer as the rate limit's last (so a 403 is a rate
	// limit); endless lists other issues made since past the ones made here, so
	// the list never reaches back (it is incomplete).
	limited bool
	endless bool
	issues  []map[string]any
	auth    []string
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
		date, limited := g.date, g.limited
		g.mu.Unlock()
		if date.IsZero() {
			date = fakeNow
		}
		w.Header().Set("Date", date.Format(http.TimeFormat))
		if limited {
			w.Header().Set("X-RateLimit-Remaining", "0")
		}
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
				w.WriteHeader(cmp.Or(g.failStatus, http.StatusBadGateway))
				return
			}
			var rows []map[string]any
			for i := len(g.issues) - 1; i >= 0; i-- {
				is := g.issues[i]
				rows = append(rows, map[string]any{"number": 42 + i, "html_url": "https://github.com/KodeStar/audiosilo-meta/issues/" + strconv.Itoa(42+i),
					"title": is["title"], "body": is["body"], "created_at": is["created_at"]})
			}
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			for g.endless && len(rows) < page*per {
				rows = append(rows, map[string]any{"number": 9000 + len(rows), "title": "other", "body": "",
					"created_at": fakeNow.Format(time.RFC3339)})
			}
			_ = json.NewEncoder(w).Encode(rows[min((page-1)*per, len(rows)):min(page*per, len(rows))])
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

// setDrop sets drop (the handler reads it under mu).
func (g *gh) setDrop(on bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.drop = on
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
		params  string
		meta    Meta
		tok     *tokens
		status  int
		limited bool
		want    string
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
		"rate limited":    {status: http.StatusForbidden, limited: true, want: "rate limit"},
		"github errors":   {status: http.StatusBadGateway, want: "didn't confirm"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			g := &gh{status: c.status, limited: c.limited}
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
			marked := len(kv.markers()) != 0
			if c.status >= http.StatusInternalServerError && !marked {
				t.Fatal("a server error may come after the issue is made: the filing marker must stay")
			}
			if c.status != 0 && c.status < http.StatusInternalServerError && marked {
				t.Fatal("GitHub's refusal left the filing marker: a retry couldn't file")
			}
		})
	}
}

// keyOf is the filing marker of the request raw on srv-1.
func keyOf(t *testing.T, raw string) string {
	t.Helper()
	p, err := ParseParams(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{ServerID: func() string { return "srv-1" }}).filingKey(p)
}

// The filing marker is the request's: the record and the changes. The same
// corrections share it whichever job (or book, which may have moved) asks,
// however they are ordered, whatever the evidence, the source stamp or the
// community's current values say, and whatever picks the recording beside its
// id; other corrections, or the same ones for another record (another region
// picks another without an id), don't.
func TestCorrectFilingKey(t *testing.T) {
	key := keyOf(t, params)
	if !strings.HasPrefix(key, "meta.correct.filing/srv-1/") {
		t.Fatalf("key = %q", key)
	}
	reordered := `{"work_id":"existing-work","asin":"B000000001","region":"us",
 "changes":[{"field":"published","current":"","corrected":"2020-03-04"},
            {"field":"title","current":"Existing Work","corrected":"The Existing Work"},
            {"field":"author","current":"Jane Doe","corrected":"Jane Doe, John Smith"}],
 "evidence":"Edited by the server admin.","source":"audiosilo-server/2.4.0"}`
	for name, raw := range map[string]string{
		"another source stamp": strings.Replace(params, "audiosilo-server/2.4.0", "audiosilo-server/2.5.0", 1),
		"other evidence":       strings.Replace(params, "Edited by the server admin.", "Bearbeitet vom Admin des Servers 2.5.0.", 1),
		"another current":      strings.Replace(params, `"current":"Jane Doe"`, `"current":"Jane Doe, J. Smith"`, 1),
		"reordered":            reordered,
		"the ids padded":       strings.Replace(params, `"asin":"B000000001","region":"us"`, `"asin":" b000000001 ","region":"US"`, 1),
	} {
		if got := keyOf(t, raw); got != key {
			t.Errorf("%s: %q, want %q", name, got, key)
		}
	}
	named := strings.Replace(params, `"work_id":"existing-work",`, `"work_id":"existing-work","recording_id":"john-smith-2020",`, 1)
	for name, raw := range map[string]string{
		"another region": strings.Replace(named, `"region":"us"`, `"region":"uk"`, 1),
		"another ASIN":   strings.Replace(named, `"asin":"B000000001"`, `"asin":"B000000002"`, 1),
	} {
		if got := keyOf(t, raw); got != keyOf(t, named) {
			t.Errorf("beside the recording id, %s: %q, want %q", name, got, keyOf(t, named))
		}
	}
	for name, raw := range map[string]string{
		"another corrected value": strings.Replace(params, `"corrected":"2020-03-04"`, `"corrected":"2020-03-05"`, 1),
		"another field":           strings.Replace(params, `"field":"published"`, `"field":"isbn"`, 1),
		"another work":            strings.Replace(params, `"work_id":"existing-work"`, `"work_id":"other-work"`, 1),
		"another recording":       strings.Replace(params, `"asin":"B000000001"`, `"asin":"B000000002"`, 1),
		"another region":          strings.Replace(params, `"region":"us"`, `"region":"uk"`, 1),
		"a change fewer": strings.Replace(params, `,
            {"field":"published","current":"","corrected":"2020-03-04"}`, "", 1),
	} {
		if got := keyOf(t, raw); got == key {
			t.Errorf("%s shares the marker", name)
		}
	}
}

// The console fills the evidence afresh each time it opens (a default line
// naming the server's version, in the admin's language), so the same
// corrections suggested again after a lost answer usually carry other evidence,
// and may come in another order: the new job adopts the issue the lost answer
// made (the marker's sum), not a second one. Once that filing is confirmed its
// marker is gone, so suggesting them again files again; another corrected value
// is another request, filed beside a lost one's marker.
func TestCorrectAdoptsTheLostIssueWithOtherEvidence(t *testing.T) {
	ctx := context.Background()
	g, kv := &gh{drop: true}, &memKV{m: map[string]string{}}
	h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	if _, err := h.Run(ctx, jobs.Job{ID: 20, Params: json.RawMessage(params)}, jobs.LocalReporter()); !errors.Is(err, errNoAnswer) || g.count() != 1 {
		t.Fatalf("lost answer: %v, issues %d", err, g.count())
	}
	g.setDrop(false)
	again := `{"work_id":"existing-work","asin":"B000000001","region":"us",
 "changes":[{"field":"published","current":"","corrected":"2020-03-04"},
            {"field":"author","current":"Jane Doe","corrected":"Jane Doe, John Smith"},
            {"field":"title","current":"Existing Work","corrected":"The Existing Work"}],
 "evidence":"Corrigé par l'administrateur du serveur.","source":"audiosilo-server/2.5.0"}`
	out, err := h.Run(ctx, jobs.Job{ID: 21, Params: json.RawMessage(again)}, jobs.LocalReporter())
	if err != nil || g.count() != 1 || out.(Result).IssueNumber != 42 || len(kv.markers()) != 0 {
		t.Fatalf("other evidence: %+v %v, issues %d, markers %v", out, err, g.count(), kv.markers())
	}
	// Confirmed: the same corrections suggested once more file again.
	if out, err := h.Run(ctx, jobs.Job{ID: 22, Params: json.RawMessage(again)}, jobs.LocalReporter()); err != nil ||
		g.count() != 2 || out.(Result).IssueNumber != 43 {
		t.Fatalf("after a confirmed filing: %+v %v, issues %d", out, err, g.count())
	}
	// Another corrected value is another request: a lost one's marker doesn't stop it.
	g.setDrop(true)
	if _, err := h.Run(ctx, jobs.Job{ID: 23, Params: json.RawMessage(params)}, jobs.LocalReporter()); !errors.Is(err, errNoAnswer) || g.count() != 3 {
		t.Fatalf("second lost answer: %v, issues %d", err, g.count())
	}
	g.setDrop(false)
	changed := strings.Replace(params, `"corrected":"2020-03-04"`, `"corrected":"2020-03-05"`, 1)
	if out, err := h.Run(ctx, jobs.Job{ID: 24, Params: json.RawMessage(changed)}, jobs.LocalReporter()); err != nil ||
		g.count() != 4 || out.(Result).IssueNumber != 45 || kv.markers()[keyOf(t, params)] == "" {
		t.Fatalf("another corrected value: %+v %v, issues %d, markers %v", out, err, g.count(), kv.markers())
	}
}

// A day after a lost answer (contrib.FilingStale) the same corrections
// suggested by a new job (with a source the maintainers asked for, say) are no
// retry of it: the lost answer's issue, which may be closed by then, is not
// adopted for them but stops them once for the admin to look at, and the next
// suggestion files, with its evidence. The job that set the marker, resumed,
// still adopts it.
func TestCorrectStaleMarkerIsNoNewJobsToAdopt(t *testing.T) {
	ctx := context.Background()
	for _, resumed := range []bool{false, true} {
		g, kv := &gh{drop: true}, &memKV{m: map[string]string{}}
		h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
		if _, err := h.Run(ctx, jobs.Job{ID: 20, Params: json.RawMessage(params)}, jobs.LocalReporter()); !errors.Is(err, errNoAnswer) {
			t.Fatalf("lost answer: %v", err)
		}
		g.setDrop(false)
		g.mu.Lock()
		g.date = fakeNow.Add(contrib.FilingStale + time.Hour)
		g.mu.Unlock()
		sourced := strings.Replace(params, "Edited by the server admin.", "The publisher's page credits both authors.", 1)
		if resumed {
			c := cov()
			c.WorkTitle = "The Existing Work" // the lost issue's title is no longer what it composes
			out, err := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: c}).
				Run(ctx, jobs.Job{ID: 20, Params: json.RawMessage(params), Resumed: true}, jobs.LocalReporter())
			if err != nil || g.count() != 1 || out.(Result).IssueNumber != 42 {
				t.Fatalf("resumed a day later: %+v %v, issues %d", out, err, g.count())
			}
			continue
		}
		if _, err := h.Run(ctx, jobs.Job{ID: 21, Params: json.RawMessage(sourced)}, jobs.LocalReporter()); !errors.Is(err, errFiledBefore) ||
			g.count() != 1 || len(kv.markers()) != 0 {
			t.Fatalf("a new job a day later: %v, issues %d, markers %v", err, g.count(), kv.markers())
		}
		out, err := h.Run(ctx, jobs.Job{ID: 22, Params: json.RawMessage(sourced)}, jobs.LocalReporter())
		g.mu.Lock()
		body, _ := g.issues[len(g.issues)-1]["body"].(string)
		g.mu.Unlock()
		if err != nil || g.count() != 2 || out.(Result).IssueNumber != 43 || !strings.Contains(body, "credits both authors") {
			t.Fatalf("suggested again: %+v %v, issues %d", out, err, g.count())
		}
	}
}

// otherBatchIssue is a correction batch for the same record as params' with
// another body (another admin's, or the run's own edited since), made a minute
// after the marker's run.
func otherBatchIssue() map[string]any {
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
	key := keyOf(t, params)
	job := jobs.Job{ID: 3, Params: json.RawMessage(params), Resumed: true}
	run := func(g *gh, kv *memKV) (any, error) {
		h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
		return h.Run(ctx, job, jobs.LocalReporter())
	}

	// A marker without a time (no way to look): a person decides, once; the
	// next run files.
	g, kv := &gh{}, &memKV{m: map[string]string{key: "{}"}}
	if _, err := run(g, kv); !errors.Is(err, errFiledBefore) || g.count() != 0 || len(kv.markers()) != 0 {
		t.Fatalf("no time: err = %v, issues %d, markers %v", err, g.count(), kv.markers())
	}
	if _, err := run(g, kv); err != nil || g.count() != 1 {
		t.Fatalf("no time, then: err = %v, issues %d", err, g.count())
	}

	// The interrupted run had filed it: adopted, nothing filed again, the
	// marker cleared.
	g, kv = &gh{}, &memKV{m: map[string]string{}}
	if _, err := run(g, kv); err != nil || g.count() != 1 || len(kv.markers()) != 0 {
		t.Fatalf("first filing: %v, issues %d, markers %v", err, g.count(), kv.markers())
	}
	delete(kv.m, "meta.correct/srv-1/3")
	kv.m[key] = marker
	out, err := run(g, kv)
	if err != nil || g.count() != 1 || out.(Result).IssueNumber != 42 || out.(Result).Changes != 3 {
		t.Fatalf("adopt: %+v %v, issues %d", out, err, g.count())
	}
	if !strings.Contains(kv.m["meta.correct/srv-1/3"], `"issue_number":42`) || len(kv.markers()) != 0 {
		t.Fatalf("the adopted issue isn't recorded: %q, markers %v", kv.m["meta.correct/srv-1/3"], kv.markers())
	}

	// It hadn't: filed once now.
	g, kv = &gh{}, &memKV{m: map[string]string{key: marker}}
	if _, err := run(g, kv); err != nil || g.count() != 1 {
		t.Fatalf("refile: %v, issues %d", err, g.count())
	}

	// A batch for the same record with another body, the list read back to the
	// settled marker: a person decides, once (the marker goes); the next run
	// files.
	g, kv = &gh{issues: []map[string]any{otherBatchIssue()}}, &memKV{m: map[string]string{key: marker}}
	if _, err := run(g, kv); !errors.Is(err, errFiledBefore) || g.count() != 1 || len(kv.markers()) != 0 {
		t.Fatalf("other body: err = %v, issues %d, markers %v", err, g.count(), kv.markers())
	}
	if _, err := run(g, kv); err != nil || g.count() != 2 {
		t.Fatalf("other body, then: err = %v, issues %d", err, g.count())
	}

	// A list that can't be read: a person decides, and the marker stays for the
	// next look; a 401 is the sign-in refused and a rate limit the rate limit,
	// even past a batch for the same record on a page read before.
	others := func() []map[string]any { // a whole page of other issues made since
		rows := make([]map[string]any, contrib.IssuesPage)
		for i := range rows {
			rows[i] = map[string]any{"title": "other", "body": "", "created_at": "2026-10-19T12:05:00Z"}
		}
		return rows
	}
	for _, c := range []struct {
		name string
		gh   *gh
		want error
	}{
		{"list fails", &gh{listStatus: http.StatusBadGateway}, errFiledBefore},
		{"another body before a failed page", &gh{issues: append(others(), otherBatchIssue()), failFromPage: 2}, errFiledBefore},
		{"another body in an incomplete list", &gh{issues: []map[string]any{otherBatchIssue()}, endless: true}, errFiledBefore},
		{"list refused", &gh{listStatus: http.StatusUnauthorized}, errRefused},
		{"refused past another body", &gh{issues: append(others(), otherBatchIssue()), failFromPage: 2,
			failStatus: http.StatusUnauthorized}, errRefused},
		{"list rate limited", &gh{listStatus: http.StatusForbidden, limited: true}, errRateLimited},
		{"rate limited past another body", &gh{issues: append(others(), otherBatchIssue()), failFromPage: 2,
			failStatus: http.StatusForbidden, limited: true}, errRateLimited},
	} {
		kv := &memKV{m: map[string]string{key: marker}}
		before := c.gh.count()
		if _, err := run(c.gh, kv); !errors.Is(err, c.want) || c.gh.count() != before || kv.m[key] != marker {
			t.Fatalf("%s: err = %v, issues %d -> %d, marker %q", c.name, err, before, c.gh.count(), kv.m[key])
		}
	}

	// The marker's age by GitHub's clock: just under a day, a lookup that can't
	// tell stops the job; just over (contrib.FilingStale), a list that can't be
	// read or can't reach back to the marker files again, while a batch for the
	// same record with another body (maybe the run's own, edited) still stops it,
	// even on a page read before one that failed, and a rate limit waits.
	for _, c := range []struct {
		name  string
		gh    func() *gh
		stale bool  // files again over a day
		want  error // else (errFiledBefore when nil)
	}{
		{"an unreadable list", func() *gh { return &gh{listStatus: http.StatusBadGateway} }, true, nil},
		{"an incomplete list", func() *gh { return &gh{endless: true} }, true, nil},
		{"another body", func() *gh { return &gh{issues: []map[string]any{otherBatchIssue()}} }, false, nil},
		{"another body before a failed page", func() *gh {
			return &gh{issues: append(others(), otherBatchIssue()), failFromPage: 2}
		}, false, nil},
		{"a rate-limited list", func() *gh { return &gh{listStatus: http.StatusForbidden, limited: true} }, false,
			errRateLimited},
	} {
		want := cmp.Or(c.want, error(errFiledBefore))
		for _, age := range []time.Duration{contrib.FilingStale - time.Hour, contrib.FilingStale + time.Hour} {
			g, kv := c.gh(), &memKV{m: map[string]string{key: marker}}
			g.date = at.Add(age)
			before := g.count()
			files := c.stale && age > contrib.FilingStale
			_, err := run(g, kv)
			switch {
			case files && (err != nil || g.count() != before+1):
				t.Fatalf("%s, %v old: err = %v, issues %d -> %d", c.name, age, err, before, g.count())
			case !files && (!errors.Is(err, want) || g.count() != before):
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
	for range contrib.IssuesPage - 1 { // newer issues: the own one ends page 1
		g.issues = append(g.issues, map[string]any{"title": "other", "body": "", "created_at": "2026-10-19T12:05:00Z"})
	}
	g.failFromPage = 2
	g.mu.Unlock()
	delete(kv.m, "meta.correct/srv-1/3")
	kv.m[keyOf(t, params)] = markerAt(t, time.Date(2026, 10, 19, 12, 0, 30, 0, time.UTC))
	out, err := h.Run(ctx, job, jobs.LocalReporter())
	if err != nil || out.(Result).IssueNumber != 42 || g.count() != contrib.IssuesPage {
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
		g.issues = append(g.issues, otherBatchIssue()) // listed first (newest first)
		g.date = date
		g.mu.Unlock()
		delete(kv.m, "meta.correct/srv-1/3")
		kv.m[keyOf(t, params)] = markerAt(t, at)
		out, err := h.Run(ctx, job, jobs.LocalReporter())
		if err != nil || g.count() != 2 || out.(Result).IssueNumber != 42 {
			t.Fatalf("at %v: %+v %v, issues %d", date, out, err, g.count())
		}
	}
}

// The live repro: the issue POST's answer is lost (GitHub made it), the job
// fails in words, and the admin's NEW job for the same corrections adopts that
// issue, though the book was renamed since. Other corrections filed meanwhile
// leave the lost filing's marker alone: suggesting the lost ones again adopts
// their issue rather than filing them twice.
func TestCorrectNewJobAdoptsTheLostIssue(t *testing.T) {
	ctx := context.Background()
	lib := int64(2)
	key := keyOf(t, params)
	g, kv := &gh{drop: true}, &memKV{m: map[string]string{}}
	h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	job := func(id int64, path, p string) jobs.Job {
		return jobs.Job{ID: id, LibraryID: &lib, Path: &path, Params: json.RawMessage(p)}
	}
	_, err := h.Run(ctx, job(10, "Author/Book", params), jobs.LocalReporter())
	if !errors.Is(err, errNoAnswer) || g.count() != 1 || kv.m[key] == "" {
		t.Fatalf("lost answer: %v, issues %d, marker %q", err, g.count(), kv.m[key])
	}
	if strings.Contains(err.Error(), "contrib") || strings.Contains(err.Error(), "POST") {
		t.Fatalf("a raw error reached the console: %v", err)
	}
	g.setDrop(false)
	out, err := h.Run(ctx, job(11, "Author/Book (Unabridged)", params), jobs.LocalReporter())
	if err != nil || g.count() != 1 || out.(Result).IssueNumber != 42 {
		t.Fatalf("new job: %+v %v, issues %d", out, err, g.count())
	}
	if _, left := kv.m[key]; left {
		t.Fatal("a confirmed filing's marker is left")
	}

	// Other corrections lost the same way, a third set filed meanwhile, then the
	// second suggested again (as the lost answer's words invite).
	other := strings.Replace(params, `"corrected":"The Existing Work"`, `"corrected":"Existing Work, The"`, 1)
	third := strings.Replace(params, `"corrected":"2020-03-04"`, `"corrected":"2020-03-05"`, 1)
	g.setDrop(true)
	if _, err := h.Run(ctx, job(12, "Author/Book", other), jobs.LocalReporter()); !errors.Is(err, errNoAnswer) || g.count() != 2 {
		t.Fatalf("second lost answer: %v, issues %d", err, g.count())
	}
	g.setDrop(false)
	if _, err := h.Run(ctx, job(13, "Author/Book", third), jobs.LocalReporter()); err != nil || g.count() != 3 ||
		kv.m[keyOf(t, other)] == "" {
		t.Fatalf("third job: %v, issues %d, markers %v", err, g.count(), kv.markers())
	}
	out, err = h.Run(ctx, job(14, "Author/Book", other), jobs.LocalReporter())
	if err != nil || g.count() != 3 || out.(Result).IssueNumber != 43 || len(kv.markers()) != 0 {
		t.Fatalf("the second again: %+v %v, issues %d, markers %v", out, err, g.count(), kv.markers())
	}
}

// The record the batch was composed from moves once it is filed (meta's bot
// applies the title correction; another recording takes the ASIN): the lost
// issue is still adopted, by the job resumed or a new one, not filed again.
func TestCorrectAdoptsItsIssueAfterTheRecordMoved(t *testing.T) {
	ctx := context.Background()
	for name, move := range map[string]func(*metaops.Coverage){
		"the title corrected": func(c *metaops.Coverage) { c.WorkTitle = "The Existing Work" },
		"the recording moved": func(c *metaops.Coverage) { c.Recordings[0].ID = "john-smith-2021" },
	} {
		for _, resumed := range []bool{true, false} {
			g, kv := &gh{drop: true}, &memKV{m: map[string]string{}}
			tok := &tokens{token: "t", from: contrib.FromEnv}
			job := jobs.Job{ID: 3, Params: json.RawMessage(params)}
			if _, err := newHandler(t, g, tok, kv, fakeMeta{cov: cov()}).Run(ctx, job, jobs.LocalReporter()); !errors.Is(err, errNoAnswer) {
				t.Fatalf("%s: lost answer: %v", name, err)
			}
			g.setDrop(false)
			c := cov()
			move(&c)
			if resumed {
				job.Resumed = true
			} else {
				job.ID = 4
			}
			out, err := newHandler(t, g, tok, kv, fakeMeta{cov: c}).Run(ctx, job, jobs.LocalReporter())
			if err != nil || g.count() != 1 || out.(Result).IssueNumber != 42 || len(kv.markers()) != 0 {
				t.Fatalf("%s (resumed %v): %+v %v, issues %d, markers %v", name, resumed, out, err, g.count(), kv.markers())
			}
		}
	}
}

// A filing whose result can't be recorded keeps its marker: the job resumed
// adopts the issue rather than filing it again.
func TestCorrectKeepsTheMarkerUntilTheResultIsRecorded(t *testing.T) {
	ctx := context.Background()
	g, kv := &gh{}, &memKV{m: map[string]string{}, failSet: "meta.correct/"}
	h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	job := jobs.Job{ID: 3, Params: json.RawMessage(params)}
	out, err := h.Run(ctx, job, jobs.LocalReporter())
	if err != nil || out.(Result).IssueNumber != 42 || g.count() != 1 || kv.markers()[keyOf(t, params)] == "" {
		t.Fatalf("unrecorded: %+v %v, issues %d, markers %v", out, err, g.count(), kv.markers())
	}
	kv.mu.Lock()
	kv.failSet = ""
	kv.mu.Unlock()
	job.Resumed = true
	out, err = h.Run(ctx, job, jobs.LocalReporter())
	if err != nil || out.(Result).IssueNumber != 42 || g.count() != 1 || len(kv.markers()) != 0 {
		t.Fatalf("resumed: %+v %v, issues %d, markers %v", out, err, g.count(), kv.markers())
	}
}

// A marker is cleared only while it is the one the run wrote or adopted: an
// overlapping run's newer marker for the request stays.
func TestCorrectClearsOnlyItsMarker(t *testing.T) {
	ctx := context.Background()
	kv := &memKV{m: map[string]string{}}
	h := New(Options{KV: kv, ServerID: func() string { return "srv-1" }})
	key := keyOf(t, params)
	mine := filing{Since: fakeNow, Sum: "sum", Job: 3}
	theirs := filing{Since: fakeNow.Add(time.Minute), Sum: "sum", Job: 4}
	raw, err := json.Marshal(theirs)
	if err != nil {
		t.Fatal(err)
	}
	kv.m[key] = string(raw)
	h.clearFiling(ctx, key, mine)
	if kv.m[key] != string(raw) {
		t.Fatalf("another run's marker went: %q", kv.m[key])
	}
	h.clearFiling(ctx, key, theirs)
	if _, left := kv.m[key]; left {
		t.Fatal("the run's own marker is left")
	}
}

// A lookup that finds nothing under contrib.FilingSettle after the marker
// waits: GitHub may still be making the issue.
func TestCorrectLookupSettles(t *testing.T) {
	ctx := context.Background()
	g, kv := &gh{}, &memKV{m: map[string]string{keyOf(t, params): markerAt(t, fakeNow)}}
	h := newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	job := jobs.Job{ID: 3, Params: json.RawMessage(params)}
	if _, err := h.Run(ctx, job, jobs.LocalReporter()); !errors.Is(err, errSettling) || g.count() != 0 {
		t.Fatalf("moments after: %v, issues %d", err, g.count())
	}
	h = newHandler(t, &gh{date: fakeNow.Add(2 * contrib.FilingSettle)}, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	if _, err := h.Run(ctx, job, jobs.LocalReporter()); err != nil {
		t.Fatalf("settled: %v", err)
	}

	// A batch for the same record with another body moments after the marker:
	// a person decides, and the marker stays (the run's own may still come).
	other := otherBatchIssue()
	other["created_at"] = fakeNow.Format(time.RFC3339)
	marker := markerAt(t, fakeNow)
	g, kv = &gh{issues: []map[string]any{other}}, &memKV{m: map[string]string{keyOf(t, params): marker}}
	h = newHandler(t, g, &tokens{token: "t", from: contrib.FromEnv}, kv, fakeMeta{cov: cov()})
	if _, err := h.Run(ctx, job, jobs.LocalReporter()); !errors.Is(err, errFiledBefore) || g.count() != 1 ||
		kv.m[keyOf(t, params)] != marker {
		t.Fatalf("another body moments after: %v, issues %d, markers %v", err, g.count(), kv.markers())
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
