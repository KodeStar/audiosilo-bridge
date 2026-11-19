package podcastjob

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/store"
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

// Taking in a renamed show folder moves the records of its interrupted
// downloads to the episodes' new paths, so a fetch there resumes the partial
// (which moved with the folder) and the day-old cleanup still finds it; left
// under the old path the record would hold its partial in the folder for good.
// A record whose download is still running stays where it is.
func TestTakingInARenamedShowMovesItsDownloadRecords(t *testing.T) {
	for _, running := range []bool{false, true} {
		h, job := audioHarness(t)
		s := h.handler.s
		root, err := os.OpenRoot(h.mount)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-owned", Offset: 6, Total: 100}
		if err := root.WriteFile("Fixture Show/"+c.Temp, []byte("prefix"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
			t.Fatal(err)
		}
		if running {
			if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Attempt: 1, ServerID: "server-1"}); err != nil {
				t.Fatal(err)
			}
		}
		renameShow(t, h, "Fixture Show", "Renamed Show")
		if _, err := s.readShow(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, "Renamed Show"); err != nil {
			t.Fatal(err)
		}
		moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
		there := jobs.Job{LibraryID: job.LibraryID, Path: &moved}
		_, old := s.readCheckpoint(job)
		saved, now := s.readCheckpoint(there)
		if running {
			if !old || now {
				t.Fatal("a running download's record was moved", old, now)
			}
			// That download, still naming the old folder, finds the episode
			// where it is now and takes its own record along.
			var params fetchParams
			if err := json.Unmarshal(job.Params, &params); err != nil {
				t.Fatal(err)
			}
			lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
			if got := s.episodeAt(context.Background(), root, lib, h.mount, *job.Path, params, job.ID); got != moved {
				t.Fatal("the running download did not find its episode", got)
			}
			if _, old = s.readCheckpoint(job); old {
				t.Fatal("the running download left its record at the old path")
			}
			if resumed, tag := s.loadCheckpoint(there, root, c.ShowID, c.EpisodeID); tag != `"one"` || resumed.Offset != 6 {
				t.Fatal("the running download's record did not follow it", resumed, tag)
			}
			continue
		}
		if old || !now || saved.Temp != c.Temp || saved.Validator != c.Validator {
			t.Fatal("the record did not follow the folder", old, now, saved)
		}
		if resumed, tag := s.loadCheckpoint(there, root, c.ShowID, c.EpisodeID); tag != `"one"` || resumed.Offset != 6 || resumed.Temp != c.Temp {
			t.Fatal("a fetch at the new path would not resume the partial", resumed, tag)
		}
	}
}

// The records move before feed.json does: an adoption that cannot read them
// (here, the Bridge's data folder) leaves the folder as it was, and the next
// read takes it in with them. Moved after feed.json, a stop between the two
// left the records at the old paths for good.
func TestATakeInThatCannotMoveTheRecordsWaits(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-owned", Offset: 6, Total: 100}
	if err := root.WriteFile("Fixture Show/"+c.Temp, []byte("prefix"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
		t.Fatal(err)
	}
	renameShow(t, h, "Fixture Show", "Renamed Show")
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	data := s.o.DataDir
	blocked := filepath.Join(t.TempDir(), "a file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.o.DataDir = blocked
	if _, err := s.readShow(context.Background(), root, lib, h.mount, "Renamed Show"); err == nil {
		t.Fatal("a folder was taken in without its download records")
	}
	s.o.DataDir = data
	if _, from, err := podcast.ReadMoved(root, "Renamed Show"); err != nil || from != "Fixture Show" {
		t.Fatal("feed.json moved without the records", from, err)
	}
	if _, err := s.readShow(context.Background(), root, lib, h.mount, "Renamed Show"); err != nil {
		t.Fatal(err)
	}
	moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &moved}); !ok {
		t.Fatal("the record did not follow the folder")
	}
}

// Two show folders whose names were swapped by hand: each old name now holds
// the other show, so each folder is a rename, not a copy. A poll naming one
// show by its old folder finds another show there, takes that folder in, then
// finds its own show in the other folder (never holding both locks) and polls
// it there.
func TestPollFollowsShowFoldersWhoseNamesWereSwapped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a, idA := followed(t, h)
	res, err := h.handler.Run(context.Background(), h.claim(h.http.URL+"/another", false), jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	r := fields(t, res)
	b, idB := r["show_path"].(string), r["show_id"].(string)
	renameShow(t, h, a, "Swapping")
	renameShow(t, h, b, a)
	renameShow(t, h, "Swapping", b)
	lib := int64(3)
	for i, c := range []struct{ show, id, want string }{{a, idA, b}, {b, idB, a}} {
		params, _ := json.Marshal(map[string]string{"show_id": c.id, "etag": `"abc"`})
		poll := jobs.Job{ID: int64(30 + i), Kind: PollKind, LibraryID: &lib, Path: &c.show, Params: params}
		res, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
		if err != nil {
			t.Fatal(c, res, err)
		}
		if r := fields(t, res); r["show_path"] != c.want || r["show_id"] != c.id {
			t.Fatal(c, r)
		}
	}
	for show, id := range map[string]string{a: idB, b: idA} {
		f := h.read(show)
		if f.ShowID != id || !strings.HasPrefix(f.Episodes[0].Path, show+"/") {
			t.Fatal("folder", show, f.ShowID, f.Episodes[0].Path)
		}
		if row, ok, err := h.db.PodcastShowByID(context.Background(), "server-1", id); err != nil || !ok || row.ShowPath != show {
			t.Fatal("cache row", row, ok, err)
		}
	}
}

// A fetch or a retention check the server still names by the old folder of a
// show folder renamed by hand runs where the show is now, as a poll does,
// rather than failing at a folder that is gone. The server's answers name the
// old folder until its rescan, so a retention check there deletes nothing.
func TestFetchAndRetainFindAShowFolderRenamedByHand(t *testing.T) {
	requireProbe(t)
	h, fetch := audioHarness(t)
	audio := data(t, "chap-v24-reverse.mp3")
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"one"`)
		w.Write(audio)
	})
	renameShow(t, h, "Fixture Show", "Renamed Show")
	moved := "Renamed Show/" + strings.TrimPrefix(*fetch.Path, "Fixture Show/")
	res, err := h.handler.FetchJob().Run(context.Background(), fetch, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	if got := res.(fetchResult); got.Path != moved || got.Fetch.Bytes != int64(len(audio)) {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(h.mount, moved)); err != nil {
		t.Fatal("the episode was not saved in the renamed folder", err)
	}
	if _, err := os.Lstat(filepath.Join(h.mount, "Fixture Show")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old folder came back", err)
	}
	keep := link.PodcastPolicy{Mode: "keep", Keep: 3, Days: 0}
	server := &retainServer{scanServer: h.scan, policy: keep}
	server.check = func(r link.PodcastRetentionRequest) *link.PodcastRetentionCheck {
		return eligibleCheck(r, keep, h.handler.s.o.Now()) // names "Fixture Show"
	}
	h.handler.s.o.Server = func() Server { return server }
	show := "Fixture Show"
	params, _ := json.Marshal(map[string]string{"show_id": "a1b2c3d4e5f60718"})
	retain := jobs.Job{ID: 82, Kind: RetainKind, LibraryID: fetch.LibraryID, Path: &show, Params: params, Attempt: 1}
	res, err = h.handler.RetainJob().Run(context.Background(), retain, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	if got := res.(retainResult); got.ShowPath != "Renamed Show" || got.Deleted != 0 {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(h.mount, moved)); err != nil {
		t.Fatal("an episode was deleted on an answer naming the old folder", err)
	}
}

// A saved feed.json over the reader's cap fails a poll and a retention check
// as too_large, as it did before either took in renamed folders, never
// write_failed.
func TestAnOversizedSavedFeedIsTooLarge(t *testing.T) {
	h, _ := audioHarness(t)
	if err := os.Truncate(filepath.Join(h.mount, "Fixture Show", "feed.json"), 32<<20+1); err != nil {
		t.Fatal(err)
	}
	server := &retainServer{scanServer: h.scan, policy: link.PodcastPolicy{Mode: "keep", Keep: 3}}
	h.handler.s.o.Server = func() Server { return server }
	show := "Fixture Show"
	params, _ := json.Marshal(map[string]string{"show_id": "a1b2c3d4e5f60718"})
	lib := int64(3)
	for _, job := range []jobs.Job{
		{ID: 40, Kind: PollKind, LibraryID: &lib, Path: &show, Params: params},
		{ID: 41, Kind: RetainKind, LibraryID: &lib, Path: &show, Params: params},
	} {
		var err error
		if job.Kind == PollKind {
			_, err = h.handler.PollJob().Run(context.Background(), job, jobs.LocalReporter())
		} else {
			_, err = h.handler.RetainJob().Run(context.Background(), job, jobs.LocalReporter())
		}
		var failure *netguard.Error
		if !errors.As(err, &failure) || failure.Code != "too_large" {
			t.Fatal(job.Kind, err)
		}
	}
}
