package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Share files a finished book's sidecars that a local run kept, for the work the
// job names (recorded on the book), skipping a dimension the work carries; a
// second share keeps the filed issue.
func TestShareFilesOverALocalRun(t *testing.T) {
	ctx := context.Background()
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{"reacher-01": {title: "Killing Floor", hasRecaps: true}})
	b := contribBook(t, db, store.NewBook{}, baseChars("x"), baseRecaps("x"))
	if _, err := db.UpsertContribution(ctx, store.Contribution{BookID: b.ID, Kind: store.ContribKindCharacters,
		Mode: store.ContribModeLocal, Status: store.ContribStatusLocal}); err != nil {
		t.Fatal(err)
	}
	exe := NewExecutor(contribConfig(t, db, contribModeLocal, gh.srv.URL, meta.srv.URL, t.TempDir(),
		fakeTokenResolver{token: "ghu_x", from: contrib.FromDevice}))

	got, err := exe.Share(ctx, b.ID, "reacher-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkID != "reacher-01" || got.Contributed["characters"] == "" || len(got.Contributed) != 1 ||
		strings.Join(got.Covered, ",") != "recaps" {
		t.Fatalf("share = %+v", got)
	}
	if gh.issueCount() != 1 {
		t.Fatalf("issues = %d, want the characters only", gh.issueCount())
	}
	if cur, _ := db.GetBook(ctx, b.ID); cur.WorkID != "reacher-01" {
		t.Fatalf("book work = %q", cur.WorkID)
	}
	again, err := exe.Share(ctx, b.ID, "", []string{ShareCharacters})
	if err != nil || gh.issueCount() != 1 || again.Contributed["characters"] != got.Contributed["characters"] {
		t.Fatalf("again = %+v %v, issues %d", again, err, gh.issueCount())
	}
}

func TestShareRefusals(t *testing.T) {
	ctx := context.Background()
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{"reacher-01": {title: "Killing Floor"}})
	b := contribBook(t, db, store.NewBook{}, baseChars("x"), baseRecaps("x"))
	exe := NewExecutor(contribConfig(t, db, contribModeLocal, gh.srv.URL, meta.srv.URL, t.TempDir(), fakeTokenResolver{token: "t"}))
	if _, err := exe.Share(ctx, b.ID, "", nil); !errors.Is(err, ErrNotMatched) {
		t.Fatalf("unmatched = %v", err)
	}
	if _, err := exe.Share(ctx, b.ID, "no-such-work", nil); !errors.Is(err, ErrNotMatched) {
		t.Fatalf("an unknown work = %v", err)
	}
	none := NewExecutor(contribConfig(t, db, contribModeLocal, gh.srv.URL, meta.srv.URL, t.TempDir(),
		fakeTokenResolver{err: contrib.ErrNoCredential}))
	var park *scheduler.ParkError
	if _, err := none.Share(ctx, b.ID, "reacher-01", nil); !errors.As(err, &park) || park.Code != state.ParkContribUnavailable {
		t.Fatalf("no credential = %v", err)
	}
	empty := contribBook(t, db, store.NewBook{SourcePath: "/src/empty"}, nil, nil)
	if err := os.RemoveAll(filepath.Join(empty.WorkDir, sidecarsDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := exe.Share(ctx, empty.ID, "reacher-01", nil); !errors.Is(err, ErrNoSidecars) {
		t.Fatalf("no sidecars = %v", err)
	}
	if gh.issueCount() != 0 {
		t.Fatalf("issues = %d", gh.issueCount())
	}
}

// shareFixture is a book whose characters a share files (its recaps covered).
func shareFixture(t *testing.T, chars *model.Characters) (*store.DB, *fakeGitHub, *Executor, store.Book) {
	t.Helper()
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{"reacher-01": {title: "Killing Floor", hasRecaps: true}})
	t.Cleanup(meta.srv.Close)
	b := contribBook(t, db, store.NewBook{}, chars, baseRecaps("x"))
	exe := NewExecutor(contribConfig(t, db, contribModeIssue, gh.srv.URL, meta.srv.URL, t.TempDir(),
		fakeTokenResolver{token: "ghu_x", from: contrib.FromDevice}))
	return db, gh, exe, b
}

// hugeChars is a characters sidecar past the issue body limit (~75 KB > 60 KB),
// so a gist carries it.
func hugeChars() *model.Characters {
	c := baseChars("x")
	c.Characters[0].Description = strings.Repeat("word ", 15000)
	return c
}

// An issue POST whose answer is lost (GitHub made it, the client timed out)
// fails the share; the next share adopts that issue rather than filing another.
func TestShareAdoptsAnIssueWhoseAnswerWasLost(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, baseChars("x"))
	gh.lostIssues = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost answer should fail the share")
	}
	if gh.issueCount() != 1 {
		t.Fatalf("issues = %d", gh.issueCount())
	}
	got, err := exe.Share(ctx, b.ID, "reacher-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if gh.issueCount() != 1 || got.Contributed["characters"] != gh.srv.URL+"/issue/1" {
		t.Fatalf("issues = %d, share = %+v", gh.issueCount(), got)
	}
	// Recorded: a third share neither files nor looks again.
	calls := gh.listCalls
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err != nil || gh.issueCount() != 1 || gh.listCalls != calls {
		t.Fatalf("third share: %v, issues %d, list calls %d -> %d", err, gh.issueCount(), calls, gh.listCalls)
	}
}

// backdateFiling moves a dimension's pending filing marker d into the past, as if
// the interrupted submit were that long ago.
func backdateFiling(t *testing.T, exe *Executor, bookID int64, kind string, d time.Duration) {
	t.Helper()
	ctx := context.Background()
	f, err := exe.readFiling(ctx, bookID, kind)
	if err != nil || (f.IssueSince.IsZero() && f.GistSince.IsZero()) {
		t.Fatalf("no pending marker: %+v %v", f, err)
	}
	if !f.IssueSince.IsZero() {
		f.IssueSince = f.IssueSince.Add(-d)
	}
	if !f.GistSince.IsZero() {
		f.GistSince = f.GistSince.Add(-d)
	}
	if err := exe.writeFiling(ctx, bookID, kind, f); err != nil {
		t.Fatal(err)
	}
}

// A lost answer whose issue GitHub never made: moments later GitHub may still be
// making it, so the share refuses; once the list can be trusted it is complete
// and holds none, so the next share files it, once.
func TestShareFilesWhenTheInterruptedIssueWasNotMade(t *testing.T) {
	ctx := context.Background()
	db, gh, exe, b := shareFixture(t, baseChars("x"))
	gh.lostIssues = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost answer should fail the share")
	}
	gh.mu.Lock()
	gh.issues = nil // the request never reached GitHub
	gh.mu.Unlock()
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); !errors.Is(err, ErrFilingUnknown) || !errors.Is(err, errFilingSettling) {
		t.Fatalf("a share moments after = %v", err)
	}
	backdateFiling(t, exe, b.ID, store.ContribKindCharacters, 2*time.Minute)
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err != nil {
		t.Fatal(err)
	}
	if gh.issueCount() != 1 || gh.listCalls != 2 {
		t.Fatalf("issues = %d, list calls = %d", gh.issueCount(), gh.listCalls)
	}
	if _, ok, _ := db.GetSetting(ctx, filingKey(b.ID, store.ContribKindCharacters)); ok {
		t.Fatal("the marker outlived the recorded issue")
	}
}

// A list that can't reach back to the marker: the share refuses to file again.
func TestShareRefusesWhenTheListIsIncomplete(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, baseChars("x"))
	gh.lostIssues = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost answer should fail the share")
	}
	gh.mu.Lock()
	gh.issues = nil // the issue isn't on the pages the list answers
	gh.listFull = true
	gh.mu.Unlock()
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); !errors.Is(err, ErrFilingUnknown) || !errors.Is(err, contrib.ErrListIncomplete) {
		t.Fatalf("incomplete list = %v", err)
	}
	if gh.issueCount() != 0 {
		t.Fatalf("issues = %d, want none filed again", gh.issueCount())
	}
}

// The sidecar changed since the lost answer (another source reference, an edit):
// the issue the earlier share sent is still the dimension's, and is adopted.
func TestShareAdoptsTheIssueSentForEarlierContent(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, baseChars("x"))
	gh.lostIssues = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost answer should fail the share")
	}
	other := baseChars("x")
	other.Characters[0].Description = "Another description."
	writeJSON(t, filepath.Join(b.WorkDir, sidecarsDir, charactersFileName), other)
	got, err := exe.Share(ctx, b.ID, "reacher-01", nil)
	if err != nil || gh.issueCount() != 1 || got.Contributed["characters"] != gh.srv.URL+"/issue/1" {
		t.Fatalf("share = %+v %v, issues %d", got, err, gh.issueCount())
	}
}

// The Bridge was signed in to another GitHub account since the lost answer: the
// issue is looked for among the first account's, and adopted.
func TestShareAdoptsAnIssueFiledAsAnotherAccount(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, baseChars("x"))
	gh.lostIssues = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost answer should fail the share")
	}
	gh.mu.Lock()
	gh.login = "someone-else"
	gh.mu.Unlock()
	got, err := exe.Share(ctx, b.ID, "reacher-01", nil)
	if err != nil || gh.issueCount() != 1 || got.Contributed["characters"] != gh.srv.URL+"/issue/1" {
		t.Fatalf("share = %+v %v, issues %d", got, err, gh.issueCount())
	}
}

// GitHub's clock runs minutes behind the Bridge's: the lookup reaches back in
// GitHub's time, and the lost issue is adopted.
func TestShareAdoptsAnIssueAcrossClockSkew(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, baseChars("x"))
	gh.offset = -5 * time.Minute
	gh.lostIssues = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost answer should fail the share")
	}
	got, err := exe.Share(ctx, b.ID, "reacher-01", nil)
	if err != nil || gh.issueCount() != 1 || got.Contributed["characters"] != gh.srv.URL+"/issue/1" {
		t.Fatalf("share = %+v %v, issues %d", got, err, gh.issueCount())
	}
}

// A closed issue is filed again even past a marker left from its filing (its
// clear failed): the issue the row names is never the one adopted.
func TestShareFilesAClosedIssueAgainPastAStaleMarker(t *testing.T) {
	ctx := context.Background()
	db, gh, exe, b := shareFixture(t, baseChars("x"))
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err != nil {
		t.Fatal(err)
	}
	row := rowsByKind(t, db, b.ID)[store.ContribKindCharacters]
	if err := db.SetContributionStatus(ctx, row.ID, store.ContribStatusClosed, 0, "", ""); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	first := gh.issues[0]
	gh.mu.Unlock()
	if err := exe.writeFiling(ctx, b.ID, store.ContribKindCharacters, filing{Login: "kodestar",
		IssueSince: time.Now().Add(-time.Hour).UTC(), Issue: issueSum(first.title, first.body)}); err != nil {
		t.Fatal(err)
	}
	got, err := exe.Share(ctx, b.ID, "reacher-01", nil)
	if err != nil || gh.issueCount() != 2 || got.Contributed["characters"] != gh.srv.URL+"/issue/2" {
		t.Fatalf("share = %+v %v, issues %d", got, err, gh.issueCount())
	}
}

// GitHub couldn't be reached at all: nothing was sent, so the next share files
// without looking.
func TestShareFilesWithoutLookingWhenGitHubWasUnreachable(t *testing.T) {
	ctx := context.Background()
	db := openContribDB(t)
	gh := newFakeGitHub(t)
	meta := newFakeMeta(t, nil, map[string]metaWorkFixture{"reacher-01": {title: "Killing Floor", hasRecaps: true}})
	b := contribBook(t, db, store.NewBook{}, baseChars("x"), baseRecaps("x"))
	executor := func(ghURL string) *Executor {
		return NewExecutor(contribConfig(t, db, contribModeIssue, ghURL, meta.srv.URL, t.TempDir(),
			fakeTokenResolver{token: "ghu_x", from: contrib.FromDevice}))
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // its port refuses connections now
	if _, err := executor(dead.URL).Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("an unreachable GitHub should fail the share")
	}
	if _, err := executor(gh.srv.URL).Share(ctx, b.ID, "reacher-01", nil); err != nil {
		t.Fatal(err)
	}
	if gh.issueCount() != 1 || gh.listCalls != 0 {
		t.Fatalf("issues = %d, list calls = %d", gh.issueCount(), gh.listCalls)
	}
}

// A gist POST whose answer is lost: the next share adopts that gist, and an issue
// POST lost after it is adopted too; one gist and one issue in all.
func TestShareAdoptsALostGistAndIssue(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, hugeChars())
	gh.lostGists = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost gist answer should fail the share")
	}
	gh.lostIssues = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost issue answer should fail the share")
	}
	got, err := exe.Share(ctx, b.ID, "reacher-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	if gh.gists != 1 || gh.issueCount() != 1 || got.Contributed["characters"] != gh.srv.URL+"/issue/1" {
		t.Fatalf("gists = %d, issues = %d, share = %+v", gh.gists, gh.issueCount(), got)
	}
	if !strings.Contains(gh.issues[0].body, gh.srv.URL+"/gist/1/characters.json") {
		t.Fatalf("the issue doesn't link the first gist:\n%s", truncate(gh.issues[0].body))
	}
}

// A lost gist answer whose gist GitHub never made: the gist list is complete and
// holds none, so the next share makes it, and files one issue linking it.
func TestShareMakesTheGistWhenTheLostOneWasNotMade(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, hugeChars())
	gh.lostGists = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost gist answer should fail the share")
	}
	gh.mu.Lock()
	gh.gistList = nil // the request never reached GitHub
	gh.mu.Unlock()
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err != nil {
		t.Fatal(err)
	}
	if gh.gists != 2 || len(gh.gistList) != 1 || gh.issueCount() != 1 ||
		!strings.Contains(gh.issues[0].body, gh.srv.URL+"/gist/2/characters.json") {
		t.Fatalf("gists = %d (%d listed), issues = %d", gh.gists, len(gh.gistList), gh.issueCount())
	}
}

// A gist made since with the same file, but described for other content (another
// book's, or an edit's), isn't the lost one: the share makes its own.
func TestShareNeverAdoptsAnotherContentsGist(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, hugeChars())
	gh.lostGists = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost gist answer should fail the share")
	}
	gh.mu.Lock()
	gh.gistList[0].desc = "AudioSilo characters for another-work (sha256 0)"
	gh.mu.Unlock()
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err != nil {
		t.Fatal(err)
	}
	if gh.gists != 2 || gh.issueCount() != 1 || !strings.Contains(gh.issues[0].body, gh.srv.URL+"/gist/2/characters.json") {
		t.Fatalf("gists = %d, issues = %d", gh.gists, gh.issueCount())
	}
}

// A gist list that can't reach back to the marker: the share refuses to make the
// gist or file the issue.
func TestShareRefusesWhenTheGistListIsIncomplete(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, hugeChars())
	gh.lostGists = 1
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a lost gist answer should fail the share")
	}
	gh.mu.Lock()
	gh.gistFull = true
	gh.mu.Unlock()
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); !errors.Is(err, ErrFilingUnknown) || !errors.Is(err, contrib.ErrListIncomplete) {
		t.Fatalf("incomplete gist list = %v", err)
	}
	if gh.gists != 1 || gh.issueCount() != 0 {
		t.Fatalf("gists = %d, issues = %d", gh.gists, gh.issueCount())
	}
}

// GitHub refusing the gist outright made nothing: the next share makes it
// without looking.
func TestShareAfterAGistRefusalMakesItWithoutLooking(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, hugeChars())
	gh.rateLimit = true
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a rate limit should fail the share")
	}
	gh.rateLimit = false
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err != nil {
		t.Fatal(err)
	}
	if gh.gists != 1 || gh.issueCount() != 1 || gh.gistCalls != 0 || gh.listCalls != 0 {
		t.Fatalf("gists = %d, issues = %d, gist list calls = %d, issue list calls = %d", gh.gists, gh.issueCount(),
			gh.gistCalls, gh.listCalls)
	}
}

// GitHub refusing the issue outright made nothing: the next share files without
// looking.
func TestShareAfterARefusalFilesWithoutLooking(t *testing.T) {
	ctx := context.Background()
	_, gh, exe, b := shareFixture(t, baseChars("x"))
	gh.rateLimit = true
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err == nil {
		t.Fatal("a rate limit should fail the share")
	}
	gh.rateLimit = false
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err != nil {
		t.Fatal(err)
	}
	if gh.issueCount() != 1 || gh.listCalls != 0 {
		t.Fatalf("issues = %d, list calls = %d", gh.issueCount(), gh.listCalls)
	}
}

// A marker's age by GitHub's clock: just under a day, a lookup that can't rule
// what it made out stops the share; just over (contrib.FilingStale), it files
// again: an issue, or another gist (then the issue).
func TestShareStaleMarkerFilesAgain(t *testing.T) {
	ages := []struct {
		name  string
		age   time.Duration
		files bool
	}{
		{"under a day", contrib.FilingStale - time.Hour, false},
		{"over a day", contrib.FilingStale + time.Hour, true},
	}
	for _, m := range []struct {
		name  string
		chars func() *model.Characters
		// mark leaves a marker of the given age, then lists nothing that clears it.
		mark func(t *testing.T, gh *fakeGitHub, exe *Executor, bookID int64, age time.Duration)
		// lost gists made before (the marker's), gists all made once it files again.
		lost, gists int
	}{
		{"issue", func() *model.Characters { return baseChars("x") }, func(t *testing.T, gh *fakeGitHub, exe *Executor, bookID int64, age time.Duration) {
			if err := exe.writeFiling(context.Background(), bookID, store.ContribKindCharacters, filing{Login: "kodestar",
				IssueSince: time.Now().Add(-age).UTC(), Issue: "an issue the list can't reach"}); err != nil {
				t.Fatal(err)
			}
			gh.mu.Lock()
			gh.listFull = true
			gh.mu.Unlock()
		}, 0, 0},
		{"gist", hugeChars, func(t *testing.T, gh *fakeGitHub, exe *Executor, bookID int64, age time.Duration) {
			ctx := context.Background()
			gh.lostGists = 1
			if _, err := exe.Share(ctx, bookID, "reacher-01", nil); err == nil {
				t.Fatal("a lost gist answer should fail the share")
			}
			f, err := exe.readFiling(ctx, bookID, store.ContribKindCharacters)
			if err != nil || f.GistSince.IsZero() {
				t.Fatalf("marker %+v %v", f, err)
			}
			f.GistSince = time.Now().Add(-age).UTC()
			if err := exe.writeFiling(ctx, bookID, store.ContribKindCharacters, f); err != nil {
				t.Fatal(err)
			}
			gh.mu.Lock()
			gh.gistList = nil // the lost gist isn't on the page the list answers
			gh.gistFull = true
			gh.mu.Unlock()
		}, 1, 2},
	} {
		for _, c := range ages {
			t.Run(m.name+" "+c.name, func(t *testing.T) {
				_, gh, exe, b := shareFixture(t, m.chars())
				m.mark(t, gh, exe, b.ID, c.age)
				_, err := exe.Share(context.Background(), b.ID, "reacher-01", nil)
				switch {
				case c.files && (err != nil || gh.gists != m.gists || gh.issueCount() != 1):
					t.Fatalf("err = %v, gists %d, issues %d", err, gh.gists, gh.issueCount())
				case !c.files && (!errors.Is(err, ErrFilingUnknown) || gh.gists != m.lost || gh.issueCount() != 0):
					t.Fatalf("err = %v, gists %d, issues %d", err, gh.gists, gh.issueCount())
				}
			})
		}
	}
}
