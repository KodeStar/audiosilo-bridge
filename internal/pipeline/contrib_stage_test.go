package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/canonical"
	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// --- fakes -------------------------------------------------------------------

// fakeTokenResolver is a deterministic TokenResolver (the real gh-auth fallback would
// make a no-credential test flaky on a host with gh installed).
type fakeTokenResolver struct {
	token string
	err   error
	from  string // "" is contrib.FromEnv
}

func (f fakeTokenResolver) Resolve(context.Context) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	if f.from != "" {
		return f.token, f.from, nil
	}
	return f.token, contrib.FromEnv, nil
}

// createdIssue records an issue the fake GitHub server was asked to open.
type createdIssue struct {
	repo    string // owner/name the issue was opened on
	number  int
	title   string
	body    string
	labels  []string
	author  string // the login that opened it
	created time.Time
}

// createdGist is a gist the fake made.
type createdGist struct {
	id      string
	desc    string
	file    string
	raw     string
	author  string
	created time.Time
}

// fakeGitHub stands in for api.github.com for the contribution client: the intake
// issue and gist calls issue mode makes.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	issues     []createdIssue
	gists      int // gist creations (lost ones included)
	gistList   []createdGist
	dropLabels bool // GET issue omits the routing labels (non-collaborator drop)
	rateLimit  bool // creation calls return a 403 rate-limit
	// lostIssues / lostGists: the next N creations are made, but the connection
	// drops before the answer (a POST that outlives the client's timeout).
	lostIssues int
	lostGists  int
	login      string        // the signed-in user ("kodestar" when unset)
	offset     time.Duration // GitHub's clock against the test's (Date, created_at)
	listCalls  int           // issue list pages read
	gistCalls  int           // gist list pages read
	// listFull / gistFull: the list holds more made since than the Bridge reads
	// back (every page is full of others).
	listFull bool
	gistFull bool
	// listStatus: the issue and gist lists answer this status (0 as usual).
	listStatus int
	// failFromPage: the issue list fails from that page on (0: never);
	// userStatus: GET /user answers this status (0 as usual).
	failFromPage int
	userStatus   int
}

// testCommunityRepo is the repository the stage contributes sidecars to.
const testCommunityRepo = "KodeStar/audiosilo-meta-community"

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{t: t}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.srv.Close)
	return g
}

// now is the fake GitHub's clock.
func (g *fakeGitHub) now() time.Time { return time.Now().Add(g.offset).UTC() }

// user is the signed-in user's login.
func (g *fakeGitHub) user() string {
	if g.login == "" {
		return "kodestar"
	}
	return g.login
}

// paged is the page of rows a list request asks for (per_page, page).
func paged(r *http.Request, rows []map[string]any) []map[string]any {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	per, page = max(per, 1), max(page, 1)
	return rows[min((page-1)*per, len(rows)):min(page*per, len(rows))]
}

// fullPage is a whole page of rows the Bridge looks past (row(i) the i-th).
func fullPage(r *http.Request, row func(i int) map[string]any) []map[string]any {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	rows := make([]map[string]any, 0, per)
	for i := range per {
		rows = append(rows, row(i))
	}
	return rows
}

func (g *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	path := r.URL.Path
	w.Header().Set("Date", g.now().Format(http.TimeFormat))

	// Rate-limit creation calls (never GETs) when configured.
	if g.rateLimit && r.Method == http.MethodPost {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		return
	}

	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/issues"):
		var req struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		g.decode(r, &req)
		num := len(g.issues) + 1
		repo := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/issues")
		g.issues = append(g.issues, createdIssue{repo: repo, number: num, title: req.Title, body: req.Body,
			labels: req.Labels, author: g.user(), created: g.now()})
		if g.lostIssues > 0 {
			g.lostIssues--
			g.drop(w)
			return
		}
		g.writeIssue(w, http.StatusCreated, num, req.Labels)

	case r.Method == http.MethodGet && path == "/user":
		if g.userStatus != 0 {
			w.WriteHeader(g.userStatus)
			return
		}
		g.writeJSON(w, http.StatusOK, map[string]string{"login": g.user()})

	case r.Method == http.MethodGet && strings.HasSuffix(path, "/issues"):
		g.listCalls++
		creator := r.URL.Query().Get("creator")
		if creator == "" {
			g.t.Errorf("fakeGitHub: issue list without the creator: %s", r.URL.RawQuery)
		}
		if g.listStatus != 0 {
			w.WriteHeader(g.listStatus)
			return
		}
		if page, _ := strconv.Atoi(r.URL.Query().Get("page")); g.failFromPage > 0 && page >= g.failFromPage {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if g.listFull {
			g.writeJSON(w, http.StatusOK, fullPage(r, func(i int) map[string]any {
				return map[string]any{"number": 1000 + i, "title": "other", "body": "",
					"created_at": g.now().Format(time.RFC3339), "html_url": "x"}
			}))
			return
		}
		rows := []map[string]any{}
		for i := len(g.issues) - 1; i >= 0; i-- {
			if is := g.issues[i]; is.author == creator {
				rows = append(rows, map[string]any{"number": is.number, "title": is.title, "body": is.body,
					"created_at": is.created.Format(time.RFC3339), "html_url": g.srv.URL + fmt.Sprintf("/issue/%d", is.number)})
			}
		}
		g.writeJSON(w, http.StatusOK, paged(r, rows))

	case r.Method == http.MethodGet && path == "/gists":
		g.gistCalls++
		if g.listStatus != 0 {
			w.WriteHeader(g.listStatus)
			return
		}
		if g.gistFull {
			g.writeJSON(w, http.StatusOK, fullPage(r, func(i int) map[string]any {
				return map[string]any{"id": fmt.Sprintf("other-%d", i), "created_at": g.now().Format(time.RFC3339),
					"files": map[string]any{}}
			}))
			return
		}
		rows := []map[string]any{}
		for i := len(g.gistList) - 1; i >= 0; i-- {
			if gs := g.gistList[i]; gs.author == g.user() {
				rows = append(rows, map[string]any{"id": gs.id, "description": gs.desc, "created_at": gs.created.Format(time.RFC3339),
					"files": map[string]any{gs.file: map[string]any{"raw_url": gs.raw}}})
			}
		}
		g.writeJSON(w, http.StatusOK, paged(r, rows))

	case r.Method == http.MethodGet && strings.Contains(path, "/issues/"):
		num := lastInt(path)
		var labels []string
		if num >= 1 && num <= len(g.issues) {
			labels = g.issues[num-1].labels
		}
		if g.dropLabels {
			labels = keepOnly(labels, "data") // routing labels silently dropped
		}
		g.writeIssue(w, http.StatusOK, num, labels)

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/gists"):
		var req struct {
			Description string `json:"description"`
			Files       map[string]struct {
				Content string `json:"content"`
			} `json:"files"`
		}
		g.decode(r, &req)
		g.gists++
		files := map[string]any{}
		for name := range req.Files {
			raw := fmt.Sprintf("%s/gist/%d/%s", g.srv.URL, g.gists, name)
			g.gistList = append(g.gistList, createdGist{id: strconv.Itoa(g.gists), desc: req.Description, file: name, raw: raw,
				author: g.user(), created: g.now()})
			files[name] = map[string]string{"raw_url": raw}
		}
		if g.lostGists > 0 {
			g.lostGists--
			g.drop(w)
			return
		}
		g.writeJSON(w, http.StatusCreated, map[string]any{"files": files})

	default:
		g.t.Errorf("fakeGitHub: unhandled %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (g *fakeGitHub) writeIssue(w http.ResponseWriter, status, num int, labels []string) {
	ls := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		ls = append(ls, map[string]string{"name": l})
	}
	g.writeJSON(w, status, map[string]any{
		"number":   num,
		"html_url": g.srv.URL + fmt.Sprintf("/issue/%d", num),
		"labels":   ls,
	})
}

// drop closes the connection without an answer.
func (g *fakeGitHub) drop(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		g.t.Fatalf("fakeGitHub: hijack: %v", err)
	}
	_ = conn.Close()
}

func (g *fakeGitHub) decode(r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		g.t.Fatalf("fakeGitHub: decode body: %v", err)
	}
}

func (g *fakeGitHub) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *fakeGitHub) issueCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.issues)
}

// fakeMeta stands in for meta.audiosilo.app (the metaops client's upstream).
type fakeMeta struct {
	srv *httptest.Server
	// lookups maps "asin:<v>"/"isbn:<v>" -> work id.
	lookups map[string]string
	// works maps work id -> (title, hasChars, hasRecaps); absent id => 404.
	works map[string]metaWorkFixture
	// redirects maps a RETIRED work id -> its survivor (answered 301, as meta does).
	redirects map[string]string
}

type metaWorkFixture struct {
	title      string
	hasChars   bool
	hasRecaps  bool
	recordings []map[string]any
}

func newFakeMeta(t *testing.T, lookups map[string]string, works map[string]metaWorkFixture) *fakeMeta {
	m := &fakeMeta{lookups: lookups, works: works}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMeta) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v1/lookup":
		id := ""
		if a := r.URL.Query().Get("asin"); a != "" {
			id = m.lookups["asin:"+a]
		}
		if id == "" {
			if b := r.URL.Query().Get("isbn"); b != "" {
				id = m.lookups["isbn:"+b]
			}
		}
		if id == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"work": nil})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"work": map[string]string{"id": id}})

	case strings.HasPrefix(r.URL.Path, "/api/v1/works/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/works/")
		if to, ok := m.redirects[id]; ok {
			http.Redirect(w, r, "/api/v1/works/"+to, http.StatusMovedPermanently)
			return
		}
		wf, ok := m.works[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		out := map[string]any{"id": id, "title": wf.title}
		if len(wf.recordings) > 0 {
			out["recordings"] = wf.recordings
		}
		if wf.hasChars {
			out["characters"] = []map[string]string{{"id": "x"}}
		}
		if wf.hasRecaps {
			out["recaps"] = []map[string]any{{"through": map[string]int{"chapter": 1}}}
		}
		_ = json.NewEncoder(w).Encode(out)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// --- helpers -----------------------------------------------------------------

func openContribDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sidecars.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// contribBook creates a book row whose work dir is populated with a valid sidecar pair.
func contribBook(t *testing.T, db *store.DB, nb store.NewBook, chars *model.Characters, recs *model.Recaps) store.Book {
	t.Helper()
	work := t.TempDir()
	nb.WorkDir = work
	if nb.SourcePath == "" {
		nb.SourcePath = "/src/" + work
	}
	if nb.Title == "" {
		nb.Title = "The Test Book"
	}
	seedWorkSidecars(t, work, chars, recs)
	b, err := db.CreateBook(context.Background(), nb)
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	return b
}

func contribConfig(t *testing.T, db *store.DB, mode, ghURL, metaURL, exportRoot string, tok TokenResolver) Config {
	t.Helper()
	var meta MetaCoverage
	if metaURL != "" {
		meta = metaops.NewClient(metaURL)
	}
	return Config{
		DB:                   db,
		DataDir:              t.TempDir(),
		Fallback:             scheduler.NewStubExecutor(0, 0),
		Meta:                 meta,
		TokenSource:          tok,
		ContribMode:          mode,
		ContribCommunityRepo: testCommunityRepo,
		ContribBaseURL:       ghURL,
		ExportRoot:           exportRoot,
	}
}

func rowsByKind(t *testing.T, db *store.DB, bookID int64) map[string]store.Contribution {
	t.Helper()
	rows, err := db.ListContributionsByBook(context.Background(), bookID)
	if err != nil {
		t.Fatalf("list contributions: %v", err)
	}
	m := map[string]store.Contribution{}
	for _, r := range rows {
		m[r.Kind] = r
	}
	return m
}

func lastInt(path string) int {
	seg := path[strings.LastIndexByte(path, '/')+1:]
	n := 0
	for _, c := range seg {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func keepOnly(labels []string, keep ...string) []string {
	set := map[string]bool{}
	for _, k := range keep {
		set[k] = true
	}
	var out []string
	for _, l := range labels {
		if set[l] {
			out = append(out, l)
		}
	}
	return out
}

// --- tests -------------------------------------------------------------------

func TestContributeIssueHappyPath(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{
		"reacher-01": {
			title: "Killing Floor",
			recordings: []map[string]any{{
				"id":          "jeff-harding-2015",
				"runtime_min": 530,
				"narrators":   []map[string]string{{"name": "Jeff Harding"}},
				"asin":        []map[string]string{{"region": "us", "asin": "B012345678"}},
			}},
		}, // no sidecars yet
	})

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01", ASIN: "B012345678"}, baseChars("placeholder"), baseRecaps("placeholder"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	exe := NewExecutor(cfg)

	res, err := exe.Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	if err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if res.RateSample == nil {
		t.Error("expected a whole-book RateSample")
	}
	if !scheduler.SentinelExists(b.WorkDir, string(state.Contributing)) {
		t.Error("contributing sentinel not written")
	}
	if gh.issueCount() != 2 {
		t.Fatalf("issues created = %d, want 2", gh.issueCount())
	}

	// The on-disk sidecars were reconciled to the real slug + stamped source, canonical.
	for _, name := range []string{charactersFileName, recapsFileName} {
		raw, err := os.ReadFile(filepath.Join(b.WorkDir, sidecarsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"work": "reacher-01"`) {
			t.Errorf("%s: work not rewritten to slug:\n%s", name, raw)
		}
		if !strings.Contains(string(raw), `"ref": "audible:us:B012345678"`) {
			t.Errorf("%s: audiobook-edition source not stamped:\n%s", name, raw)
		}
		if strings.Count(string(raw), `"type": "community"`) != 1 {
			t.Errorf("%s: contribution should carry exactly one community source:\n%s", name, raw)
		}
		if f, _ := canonical.Format(raw); !bytes.Equal(raw, f) {
			t.Errorf("%s: not canonical on disk", name)
		}
	}

	// Issue bodies carry the fenced payload with the rewritten slug + the routing labels.
	gotChars, gotRecaps := false, false
	for _, iss := range gh.issues {
		if !strings.Contains(iss.body, "```json") {
			t.Errorf("issue %q missing fenced payload", iss.title)
		}
		if !strings.Contains(iss.body, `"work": "reacher-01"`) {
			t.Errorf("issue %q missing rewritten work slug", iss.title)
		}
		if iss.repo != testCommunityRepo {
			t.Errorf("issue %q opened on %q, want the community repo %q (the core bot refuses sidecars)", iss.title, iss.repo, testCommunityRepo)
		}
		switch {
		case contains(iss.labels, "data:characters"):
			gotChars = true
		case contains(iss.labels, "data:recaps"):
			gotRecaps = true
		}
	}
	if !gotChars || !gotRecaps {
		t.Errorf("expected both characters+recaps labelled issues (chars=%v recaps=%v)", gotChars, gotRecaps)
	}

	// Both rows recorded submitted against the community repo, no note.
	rows := rowsByKind(t, db, b.ID)
	for _, k := range []string{store.ContribKindCharacters, store.ContribKindRecaps} {
		r := rows[k]
		if r.Status != store.ContribStatusSubmitted || r.URL == "" || r.Note != "" || r.Repo != testCommunityRepo {
			t.Errorf("%s row = %+v, want submitted with url and no note", k, r)
		}
	}
}

func TestBestRecordingRef(t *testing.T) {
	recordings := []metaops.RecordingRef{
		{ID: "other", Narrators: []string{"Other Narrator"}, RuntimeMin: 400},
		{ID: "match", Narrators: []string{"Jeff Harding"}, RuntimeMin: 530},
	}
	got := bestRecordingRef(store.Book{Narrators: []string{"Jeff Harding"}, DurationSec: 530 * 60}, recordings)
	if got == nil || got.ID != "match" {
		t.Fatalf("best recording = %+v, want match", got)
	}
	if got := bestRecordingRef(store.Book{}, recordings); got != nil {
		t.Fatalf("ambiguous recording = %+v, want nil", got)
	}
}

func TestContributeIssueLabelsDropped(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	gh.dropLabels = true

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	rows := rowsByKind(t, db, b.ID)
	if note := rows[store.ContribKindCharacters].Note; !strings.Contains(note, "labels missing") {
		t.Errorf("characters note = %q, want a labels-missing note", note)
	}
}

// An issue filed through the GitHub App (the Accounts tab's sign-in) is labelled
// by meta's route workflow after it opens: no labels-missing note, no extra read.
func TestContributeIssueAppFiledSkipsTheLabelCheck(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	gh.dropLabels = true

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghu_x", from: contrib.FromDevice})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	for kind, row := range rowsByKind(t, db, b.ID) {
		if strings.Contains(row.Note, "labels missing") || row.Number == 0 {
			t.Errorf("%s row = %+v, want filed with no labels-missing note", kind, row)
		}
	}
}

func TestContributeIssueOversizePayloadUsesGist(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, hugeChars(), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if gh.gists != 1 {
		t.Fatalf("gists created = %d, want 1", gh.gists)
	}
	// The characters issue links the gist raw URL instead of inlining the payload.
	var charsBody string
	for _, iss := range gh.issues {
		if contains(iss.labels, "data:characters") {
			charsBody = iss.body
		}
	}
	if !strings.Contains(charsBody, gh.srv.URL+"/gist/1/characters.json") {
		t.Errorf("characters issue did not link the gist:\n%s", truncate(charsBody))
	}
	if strings.Contains(charsBody, "```json") {
		t.Error("characters issue inlined an oversize payload instead of linking a gist")
	}
}

func TestContributeSkipsCoveredDimension(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{
		"reacher-01": {title: "Killing Floor", hasChars: true}, // characters already upstream
	})

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if gh.issueCount() != 1 {
		t.Fatalf("issues created = %d, want 1 (recaps only)", gh.issueCount())
	}
	rows := rowsByKind(t, db, b.ID)
	if rows[store.ContribKindCharacters].Status != store.ContribStatusAlreadyCovered {
		t.Errorf("characters row = %+v, want already_covered", rows[store.ContribKindCharacters])
	}
	if rows[store.ContribKindRecaps].Status != store.ContribStatusSubmitted {
		t.Errorf("recaps row = %+v, want submitted", rows[store.ContribKindRecaps])
	}
}

// TestContributeLocalMode: a local export is the bare sidecar FILE per dimension,
// <export>/<slug>/<name>.json - exactly what an intake-issue attachment takes - with
// no repository layout.
func TestContributeLocalMode(t *testing.T) {
	db := openContribDB(t)
	export := t.TempDir()

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeLocal, "", "", export, nil)
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	for _, name := range []string{charactersFileName, recapsFileName} {
		p := filepath.Join(export, "reacher-01", name)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("local export missing %s: %v", p, err)
		}
		src, _ := os.ReadFile(filepath.Join(b.WorkDir, sidecarsDir, name))
		if !bytes.Equal(raw, src) {
			t.Errorf("%s: export is not the sidecar file byte for byte", name)
		}
	}
	if _, err := os.Stat(filepath.Join(export, "works")); !os.IsNotExist(err) {
		t.Errorf("the retired works/<shard>/ layout was written (stat err %v)", err)
	}
	rows := rowsByKind(t, db, b.ID)
	if rows[store.ContribKindCharacters].Status != store.ContribStatusLocal {
		t.Errorf("characters row = %+v, want local", rows[store.ContribKindCharacters])
	}
}

// TestContributeCarriesAuditAcceptanceNote: a book whose sidecars were accepted on a
// converging audit trajectory (audit_accepted.json present) carries the residual-nits
// note on its contribution rows, so the acceptance surfaces in the UI.
func TestContributeCarriesAuditAcceptanceNote(t *testing.T) {
	db := openContribDB(t)
	export := t.TempDir()

	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	if err := writeAuditAccepted(b.WorkDir, auditAccepted{Round: 2, Fix: 1, Nit: 3}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeLocal, "", "", export, nil)
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	rows := rowsByKind(t, db, b.ID)
	for kind, row := range rows {
		if !strings.Contains(row.Note, "converged after 2 rounds") || !strings.Contains(row.Note, "3 residual nit") {
			t.Errorf("%s row note = %q, want the acceptance line", kind, row.Note)
		}
	}
}

func TestContributeLocalModeUnresolvedSlugUsesPlaceholder(t *testing.T) {
	db := openContribDB(t)
	export := t.TempDir()

	// No WorkID and no meta => local mode falls back to a title-derived placeholder slug.
	b := contribBook(t, db, store.NewBook{Title: "Some Unknown Book"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeLocal, "", "", export, nil)
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	slug := "some-unknown-book"
	p := filepath.Join(export, slug, charactersFileName)
	if _, err := os.Stat(p); err != nil {
		t.Errorf("placeholder export missing %s: %v", p, err)
	}
	rows := rowsByKind(t, db, b.ID)
	if note := rows[store.ContribKindCharacters].Note; note == "" {
		t.Error("expected a placeholder note on the local row")
	}
}

func TestContributeStaleWorkIDFallsToAsinLookup(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	// The recorded WorkID is stale (404 upstream); the ASIN resolves to the real work.
	meta := newFakeMeta(t,
		map[string]string{"asin:B01": "reacher-01"},
		map[string]metaWorkFixture{"reacher-01": {title: "Killing Floor"}},
	)

	b := contribBook(t, db, store.NewBook{WorkID: "stale-slug", ASIN: "B01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	// The resolved work id is persisted back onto the book.
	got, _ := db.GetBook(context.Background(), b.ID)
	if got.WorkID != "reacher-01" {
		t.Errorf("work_id = %q, want reacher-01 (asin match persisted)", got.WorkID)
	}
	if gh.issueCount() != 2 {
		t.Errorf("issues = %d, want 2", gh.issueCount())
	}
}

func TestContributeNoMatchParksCoreNeeded(t *testing.T) {
	db := openContribDB(t)
	meta := newFakeMeta(t, nil, nil) // nothing resolves

	b := contribBook(t, db, store.NewBook{Title: "Mystery Book", ASIN: "BZZ"}, baseChars("x"), baseRecaps("x"))
	// Seed the language + runtime the core proposal prefills from.
	if err := writeASRProvenance(b.WorkDir, asrProvenance{Language: "en"}); err != nil {
		t.Fatal(err)
	}
	if err := audio.WriteManifest(b.WorkDir, audio.Manifest{Style: audio.StyleMarkers, Duration: 3600, ChapterCount: 1}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, "", meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})

	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	assertPark(t, err, state.ParkCoreNeeded)

	// A prefilled proposal is written with the language + runtime.
	raw, rerr := os.ReadFile(filepath.Join(b.WorkDir, contribDir, coreProposalName))
	if rerr != nil {
		t.Fatalf("core proposal not written: %v", rerr)
	}
	var p contrib.CoreProposal
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if p.Title != "Mystery Book" || p.Language != "en" || p.RuntimeMin != 60 {
		t.Errorf("core proposal prefill = %+v, want title/language=en/runtime=60", p)
	}
	if len(p.ASINs) != 1 || p.ASINs[0].ASIN != "BZZ" || p.ASINs[0].Region != "" {
		t.Errorf("core proposal ASINs = %+v, want one BZZ with empty region", p.ASINs)
	}
}

// TestContributeEbookCoreProposalUsesEpubIdentity pins the two ways an ebook's
// identity differs from an audiobook's in a PUBLIC add-work proposal.
//
// The proposal is a factual claim in the community database, and nothing downstream
// re-checks it: an ISBN read from an epub's OPF names the print/ebook edition, so
// filing it under audiobook_isbns states something false; and the language, which is
// REQUIRED, comes from ASR on the audio path - a stage an ebook never runs - so it
// has to come from the OPF the extract stage already opened.
func TestContributeEbookCoreProposalUsesEpubIdentity(t *testing.T) {
	db := openContribDB(t)
	meta := newFakeMeta(t, nil, nil) // nothing resolves -> core_needed

	epub := filepath.Join(t.TempDir(), "book.epub")
	buildTestEpub(t, epub, [][2]string{{"Chapter 1", "one"}}) // its OPF states language "en"

	b := contribBook(t, db, store.NewBook{
		Title: "A Test Book", ISBN: "9780857521405",
		Kind: "ebook", EbookPath: epub,
		IdentitySources: map[string]string{"title": metaops.IdentitySourceEpub, "isbn": metaops.IdentitySourceEpub},
	}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, "", meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})

	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	assertPark(t, err, state.ParkCoreNeeded)

	raw, rerr := os.ReadFile(filepath.Join(b.WorkDir, contribDir, coreProposalName))
	if rerr != nil {
		t.Fatalf("core proposal not written: %v", rerr)
	}
	var p contrib.CoreProposal
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.AudiobookISBNs) != 0 {
		t.Errorf("audiobook_isbns = %v, want empty: the ISBN came from the epub's OPF", p.AudiobookISBNs)
	}
	if len(p.PrintISBNs) != 1 || p.PrintISBNs[0] != "9780857521405" {
		t.Errorf("print_isbns = %v, want the epub's ISBN", p.PrintISBNs)
	}
	if p.Language != "en" {
		t.Errorf("language = %q, want en from the epub's OPF (an ebook writes no asr.json, "+
			"and Validate rejects an empty language)", p.Language)
	}
}

func TestContributeCorePendingWhenProposalSubmitted(t *testing.T) {
	db := openContribDB(t)
	b := contribBook(t, db, store.NewBook{Title: "Mystery Book"}, baseChars("x"), baseRecaps("x"))
	// A core proposal was already submitted (the confirm endpoint's row).
	if _, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCore, Mode: store.ContribModeIssue,
		Status: store.ContribStatusSubmitted, Number: 7, URL: "https://x/7",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, "", "", "", fakeTokenResolver{token: "ghp_x"})
	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	assertPark(t, err, state.ParkCorePending)
}

func TestContributeNoCredentialParks(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))

	// Denied: no credential parks contrib_unavailable and creates no issue; a
	// sign-in GitHub couldn't be reached to refresh parks the same code, worded for
	// waiting on GitHub rather than signing in. Either park's Cause is the resolve
	// error, which the share job words the two apart by.
	for _, c := range []struct {
		err    error
		reason string
	}{{contrib.ErrNoCredential, ContribUnavailableMsg}, {contrib.ErrSignInUnrefreshed, ContribUnrefreshedMsg}} {
		cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{err: c.err})
		_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
		if pe := assertPark(t, err, state.ParkContribUnavailable); pe.Reason != c.reason || pe.Cause != c.err {
			t.Errorf("park = %q (cause %v), want %q (cause %v)", pe.Reason, pe.Cause, c.reason, c.err)
		}
		if gh.issueCount() != 0 {
			t.Errorf("%v: issues created = %d, want 0", c.err, gh.issueCount())
		}
	}

	// Allowed: a PAT proceeds.
	b2 := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg2 := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_ok"})
	if _, err := NewExecutor(cfg2).Execute(context.Background(), b2, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute with PAT: %v", err)
	}
	if gh.issueCount() != 2 {
		t.Errorf("issues created = %d, want 2 with a PAT", gh.issueCount())
	}
}

func TestContributeResumeSkipsAlreadyPostedRows(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	// Both dimensions already posted (a crash between submit and sentinel).
	for _, k := range []string{store.ContribKindCharacters, store.ContribKindRecaps} {
		if _, err := db.UpsertContribution(context.Background(), store.Contribution{
			BookID: b.ID, Kind: k, Mode: store.ContribModeIssue,
			Status: store.ContribStatusSubmitted, Number: 1, URL: "https://x/1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if gh.issueCount() != 0 {
		t.Errorf("resume created %d issues, want 0", gh.issueCount())
	}
}

func TestContributeRateLimitIsTransientNotPark(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	gh.rateLimit = true
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghp_x"})

	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	if err == nil {
		t.Fatal("expected a rate-limit error")
	}
	var rl *contrib.RateLimitError
	if !errors.As(err, &rl) {
		t.Errorf("err = %v, want a *contrib.RateLimitError", err)
	}
	var pe *scheduler.ParkError
	if errors.As(err, &pe) {
		t.Errorf("rate limit must NOT be a park, got %v", pe)
	}
}

// TestContributeSettledRowNotOverwrittenByCovered proves a settled (submitted) row is
// never clobbered by a later upstream-covered verdict: the rowSettled check runs BEFORE
// the covered branch.
func TestContributeSettledRowNotOverwrittenByCovered(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{
		"reacher-01": {title: "Killing Floor", hasChars: true}, // characters now covered upstream
	})
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	// A prior run already submitted the characters dimension.
	if _, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCharacters, Mode: store.ContribModeIssue,
		Status: store.ContribStatusSubmitted, Number: 5, URL: "https://x/5",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	rows := rowsByKind(t, db, b.ID)
	if r := rows[store.ContribKindCharacters]; r.Status != store.ContribStatusSubmitted || r.URL != "https://x/5" {
		t.Errorf("settled characters row was overwritten by the covered verdict: %+v", r)
	}
	if rows[store.ContribKindRecaps].Status != store.ContribStatusSubmitted {
		t.Errorf("recaps row = %+v, want submitted", rows[store.ContribKindRecaps])
	}
}

// TestContributeMergedCoreWaitsForRelease is the release gate: a book whose work id
// came from a MERGED add-work PR but 404s in the published catalogue has a work no
// data release holds yet. The community intake verifies a sidecar's key against the
// newest release, so contributing now would be refused - the book parks core_pending
// with the release-wait message (the poller re-admits it once the work is live),
// submits nothing, and does NOT re-park needs-core.
func TestContributeMergedCoreWaitsForRelease(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, nil) // every work id 404s
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	if _, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCore, Mode: store.ContribModeIssue,
		Status: store.ContribStatusMerged, Number: 9, URL: "https://x/9",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	assertPark(t, err, state.ParkCorePending)
	var pe *scheduler.ParkError
	if errors.As(err, &pe) && pe.Reason != contrib.ReleaseWaitMsg {
		t.Errorf("park reason = %q, want the release-wait message", pe.Reason)
	}
	if gh.issueCount() != 0 {
		t.Errorf("issues = %d, want 0 before the work is released", gh.issueCount())
	}
}

// TestContributeAdoptsSurvivorSlug: a recorded work id a merge has RETIRED is answered
// by the catalogue with a 301 to its survivor; the sidecars attach to the survivor and
// the book remembers it.
func TestContributeAdoptsSurvivorSlug(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{"reacher-01": {title: "Killing Floor"}})
	meta.redirects = map[string]string{"killing-floor-old": "reacher-01"}
	b := contribBook(t, db, store.NewBook{WorkID: "killing-floor-old"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if got, _ := db.GetBook(context.Background(), b.ID); got.WorkID != "reacher-01" {
		t.Errorf("work_id = %q, want the survivor reacher-01", got.WorkID)
	}
	raw, _ := os.ReadFile(filepath.Join(b.WorkDir, sidecarsDir, charactersFileName))
	if !strings.Contains(string(raw), `"work": "reacher-01"`) {
		t.Errorf("sidecar not keyed to the survivor:\n%s", raw)
	}
	for _, iss := range gh.issues {
		if !strings.Contains(iss.body, `"work": "reacher-01"`) {
			t.Errorf("issue %q not keyed to the survivor", iss.title)
		}
	}
}

// TestContributeAdoptsSurvivorPastAWarmCache: the work was read live (and cached)
// just before a merge retired its slug. The stage's adoption lookup is fresh, so it
// follows the new 301 instead of keying the sidecars to the retired slug.
func TestContributeAdoptsSurvivorPastAWarmCache(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{
		"killing-floor-old": {title: "Killing Floor"},
		"reacher-01":        {title: "Killing Floor"},
	})
	b := contribBook(t, db, store.NewBook{WorkID: "killing-floor-old"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	client := metaops.NewClient(meta.srv.URL)
	cfg.Meta = client
	if cov, err := client.CoverageForWork(context.Background(), "killing-floor-old"); err != nil || cov.WorkID != "killing-floor-old" {
		t.Fatalf("warm read = %+v, %v", cov, err)
	}
	meta.redirects = map[string]string{"killing-floor-old": "reacher-01"} // the merge lands

	if _, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contribute: %v", err)
	}
	if got, _ := db.GetBook(context.Background(), b.ID); got.WorkID != "reacher-01" {
		t.Errorf("work_id = %q, want the survivor reacher-01 despite the warm cache", got.WorkID)
	}
}

// TestContributeCoreDuplicateAsksForWork: the add-work proposal was answered as a
// duplicate (the core row is already_covered) and no identifier reaches the work - a
// human sets it, rather than a proposal being re-submitted into the same answer.
func TestContributeCoreDuplicateAsksForWork(t *testing.T) {
	db := openContribDB(t)
	meta := newFakeMeta(t, nil, nil)
	b := contribBook(t, db, store.NewBook{Title: "Dup Book"}, baseChars("x"), baseRecaps("x"))
	if _, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCore, Mode: store.ContribModeIssue,
		Status: store.ContribStatusAlreadyCovered, Number: 9, URL: "https://x/9", Note: store.ContribNoteIntakeDuplicate,
	}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, "", meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	assertPark(t, err, state.ParkCoreNeeded)
	var pe *scheduler.ParkError
	if errors.As(err, &pe) && pe.Reason != CoreDuplicateMsg {
		t.Errorf("park reason = %q, want the duplicate message", pe.Reason)
	}
}

// TestContributeCoreVerdictKeepsItsMessage: a Retry of a book whose submitted core
// row carries a needs-human verdict re-parks core_pending WITH the verdict message
// (the poller writes it once; the generic "waiting to merge" would hide it).
func TestContributeCoreVerdictKeepsItsMessage(t *testing.T) {
	db := openContribDB(t)
	meta := newFakeMeta(t, nil, nil)
	b := contribBook(t, db, store.NewBook{Title: "Verdict Book"}, baseChars("x"), baseRecaps("x"))
	if _, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCore, Mode: store.ContribModeIssue,
		Status: store.ContribStatusSubmitted, Number: 9, URL: "https://x/9",
		Note: store.ContribNoteIntakeNeedsHuman + " - a maintainer must look",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := contribConfig(t, db, contribModeIssue, "", meta.srv.URL, "", fakeTokenResolver{token: "ghp_x"})
	_, err := NewExecutor(cfg).Execute(context.Background(), b, state.Contributing, scheduler.StageReport{})
	assertPark(t, err, state.ParkCorePending)
	var pe *scheduler.ParkError
	if errors.As(err, &pe) && (pe.Reason == CorePendingMsg || !strings.Contains(pe.Reason, "https://x/9")) {
		t.Errorf("park reason = %q, want the verdict message naming the issue", pe.Reason)
	}
}

// --- small assertion helpers -------------------------------------------------

func assertPark(t *testing.T, err error, want state.ParkCode) *scheduler.ParkError {
	t.Helper()
	var pe *scheduler.ParkError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a ParkError with code %s", err, want)
	}
	if pe.Code != want {
		t.Fatalf("park code = %s, want %s", pe.Code, want)
	}
	return pe
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// TestContribModeForBook: a book's own mode (a sidecars.generate job's
// contribute) wins over the configured one; an empty or unknown one falls back.
func TestContribModeForBook(t *testing.T) {
	e := &Executor{contribMode: contribModeIssue}
	for mode, want := range map[string]string{"local": "local", "issue": "issue", "": "issue", "pr": "issue"} {
		if got := e.contribModeFor(store.Book{ContribMode: mode}); got != want {
			t.Errorf("book mode %q = %q, want %q", mode, got, want)
		}
	}
	e.contribMode = contribModeLocal
	if got := e.contribModeFor(store.Book{ContribMode: "issue"}); got != "issue" {
		t.Errorf("a book's issue under a local Bridge = %q", got)
	}
	if got := e.contribModeFor(store.Book{}); got != "local" {
		t.Errorf("no book mode under a local Bridge = %q", got)
	}
}

// The contributing stage files through the same marker: a retry after a lost
// answer adopts the issue rather than filing a second.
func TestContributeRetryAdoptsALostIssue(t *testing.T) {
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	gh.lostIssues = 1
	b := contribBook(t, db, store.NewBook{WorkID: "reacher-01"}, baseChars("x"), baseRecaps("x"))
	cfg := contribConfig(t, db, contribModeIssue, gh.srv.URL, "", "", fakeTokenResolver{token: "ghu_x", from: contrib.FromDevice})
	exe := NewExecutor(cfg)
	if _, err := exe.Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err == nil {
		t.Fatal("a lost answer should fail the stage")
	}
	if _, err := exe.Execute(context.Background(), b, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if gh.issueCount() != 2 {
		t.Fatalf("issues = %d, want one per dimension", gh.issueCount())
	}
	rows := rowsByKind(t, db, b.ID)
	if rows[store.ContribKindCharacters].Number == rows[store.ContribKindRecaps].Number {
		t.Fatalf("rows = %+v", rows)
	}
}
