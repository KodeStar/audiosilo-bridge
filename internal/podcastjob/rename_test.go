package podcastjob

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
)

// followed subscribes the harness feed and answers its show folder and id.
func followed(t *testing.T, h *harness) (string, string) {
	t.Helper()
	res, err := h.handler.Run(context.Background(), h.claim(h.http.URL+"/feed", false), jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	r := fields(t, res)
	return r["show_path"].(string), r["show_id"].(string)
}

func renameShow(t *testing.T, h *harness, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(h.mount, from), filepath.Join(h.mount, to)); err != nil {
		t.Fatal(err)
	}
}

// A show folder renamed by hand (bridge#13): the server's next poll still
// names the old folder. The poll finds the show by its id, takes the folder
// in (its episodes move to the new name), keeps its episodes, artwork and
// cache row, and asks the server to index the new folder; nothing is created
// at the old name.
func TestPollFindsAShowFolderRenamedByHand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	show, id := followed(t, h)
	before := h.read(show)
	renameShow(t, h, show, "Renamed Show")
	scans := h.scan.calls.Load()
	params, _ := json.Marshal(map[string]string{"show_id": id, "etag": `"abc"`})
	lib := int64(3)
	poll := jobs.Job{ID: 20, Kind: PollKind, LibraryID: &lib, Path: &show, Params: params}
	res, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	r := fields(t, res)
	if r["show_path"] != "Renamed Show" || r["show_id"] != id || r["status"] != float64(304) {
		t.Fatal(r)
	}
	after := h.read("Renamed Show")
	if len(after.Episodes) != len(before.Episodes) || after.Show.Image == nil || *after.Show.Image != *before.Show.Image {
		t.Fatal("the renamed show lost its episodes or artwork", after)
	}
	for i, e := range after.Episodes {
		if e.ID != before.Episodes[i].ID || e.Path != "Renamed Show/"+strings.TrimPrefix(before.Episodes[i].Path, show+"/") {
			t.Fatal("episode", e.Path)
		}
	}
	if _, err := os.Stat(filepath.Join(h.mount, "Renamed Show", after.Show.Image.File)); err != nil {
		t.Fatal("artwork", err)
	}
	if h.scan.calls.Load() == scans {
		t.Fatal("the server was not asked to index the renamed folder")
	}
	if _, err := os.Lstat(filepath.Join(h.mount, show)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old folder came back", err)
	}
	row, ok, err := h.db.PodcastShowByID(context.Background(), "server-1", id)
	if err != nil || !ok || row.ShowPath != "Renamed Show" {
		t.Fatal("cache row", row, ok, err)
	}
	// The next poll at the old name finds it through the cache row, and one
	// for another show id finds nothing there.
	if res, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter()); err != nil || fields(t, res)["show_path"] != "Renamed Show" {
		t.Fatal(res, err)
	}
	other, _ := json.Marshal(map[string]string{"show_id": "0123456789abcdef"})
	poll.Params = other
	res, err = h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	var failure *netguard.Error
	if !errors.As(err, &failure) || failure.Code != "write_failed" {
		t.Fatal("a poll for another show found this one", res, err)
	}
}

// Following the feed again after a rename by hand joins the renamed folder,
// never starting the show again as new in "Example Show" or "Example Show (2)".
func TestFollowAgainJoinsAShowFolderRenamedByHand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	show, id := followed(t, h)
	renameShow(t, h, show, "Renamed Show")
	got, again := followed(t, h)
	if got != "Renamed Show" || again != id {
		t.Fatal(got, again)
	}
	entries, _ := os.ReadDir(h.mount)
	for _, e := range entries {
		if e.Name() != "Renamed Show" && e.Name() != ".audiosilo" {
			t.Fatal("a second show folder was made", e.Name())
		}
	}
	if f := h.read("Renamed Show"); !strings.HasPrefix(f.Episodes[0].Path, "Renamed Show/") {
		t.Fatal(f.Episodes[0].Path)
	}
}

// The cache row decides which folder a follow joins: of two folders holding
// the same feed (a show copied by hand and made its own), the one the cache
// names is joined without reading the others first.
func TestFollowAgainAsksTheCacheFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	show, _ := followed(t, h)
	f := h.read(show)
	for i := range f.Episodes {
		f.Episodes[i].Path = "A Copy/" + strings.TrimPrefix(f.Episodes[i].Path, show+"/")
	}
	os.Mkdir(filepath.Join(h.mount, "A Copy"), 0o755)
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := podcast.Write(root, "A Copy", f, h.handler.s.o.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := followed(t, h); got != show {
		t.Fatal("the follow joined", got, "not the cached", show)
	}
}

// A copy of a show folder renamed by hand, made before either was taken in,
// names the same gone folder: the poll takes in one, and the copy is then
// refused (two folders would hold one show) and left as it is.
func TestACopyOfARenamedShowFolderIsNotTakenIn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	show, id := followed(t, h)
	renameShow(t, h, show, "Renamed Show")
	if err := os.CopyFS(filepath.Join(h.mount, "Some Copy"), os.DirFS(filepath.Join(h.mount, "Renamed Show"))); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(map[string]string{"show_id": id, "etag": `"abc"`})
	lib := int64(3)
	poll := jobs.Job{ID: 20, Kind: PollKind, LibraryID: &lib, Path: &show, Params: params}
	res, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil || fields(t, res)["show_path"] != "Renamed Show" {
		t.Fatal(res, err)
	}
	copied := "Some Copy"
	poll.ID, poll.Path = 21, &copied
	res, err = h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	var failure *netguard.Error
	if !errors.As(err, &failure) || failure.Code != "write_failed" {
		t.Fatal("a copy of a renamed show was taken in", res, err)
	}
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, from, err := podcast.ReadMoved(root, copied); err != nil || from != show {
		t.Fatal("the copy changed", from, err)
	}
	if row, ok, err := h.db.PodcastShowByID(context.Background(), "server-1", id); err != nil || !ok || row.ShowPath != "Renamed Show" {
		t.Fatal("cache row", row, ok, err)
	}
}

// A new show never takes the name a folder renamed by hand came from while
// that folder is not taken in yet: a show made there would leave the renamed
// folder a copy (its feed.json names a folder that exists), refused for good.
// The renamed show is still found by its id afterwards.
func TestANewShowNeverTakesTheNameARenamedFolderCameFrom(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	show, id := followed(t, h)
	renameShow(t, h, show, "Renamed Show")
	// Another feed of the same title (the harness answers every path alike).
	res, err := h.handler.Run(context.Background(), h.claim(h.http.URL+"/another", false), jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	r := fields(t, res)
	if r["created"] != true || r["show_path"] == show || r["show_path"] == "Renamed Show" || r["show_id"] == id {
		t.Fatal("the new show took the renamed folder's old name, or joined it", r)
	}
	if _, err := os.Lstat(filepath.Join(h.mount, show)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a folder was made at the old name", err)
	}
	params, _ := json.Marshal(map[string]string{"show_id": id, "etag": `"abc"`})
	lib := int64(3)
	poll := jobs.Job{ID: 20, Kind: PollKind, LibraryID: &lib, Path: &show, Params: params}
	if res, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter()); err != nil || fields(t, res)["show_path"] != "Renamed Show" {
		t.Fatal("the renamed show was not taken in", res, err)
	}
}
