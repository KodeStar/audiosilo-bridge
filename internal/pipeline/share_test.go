package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// hugeChars is a characters sidecar past the issue body limit (a gist carries it).
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

// A lost answer whose issue GitHub never made: the list is complete and holds
// none, so the next share files it, once.
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
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); err != nil {
		t.Fatal(err)
	}
	if gh.issueCount() != 1 || gh.listCalls != 1 {
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
	gh.issues = nil // the issue isn't on the page the list answers
	gh.listFull = true
	gh.mu.Unlock()
	if _, err := exe.Share(ctx, b.ID, "reacher-01", nil); !errors.Is(err, ErrFilingUnknown) {
		t.Fatalf("incomplete list = %v", err)
	}
	if gh.issueCount() != 0 {
		t.Fatalf("issues = %d, want none filed again", gh.issueCount())
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
