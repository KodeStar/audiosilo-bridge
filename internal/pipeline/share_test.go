package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
