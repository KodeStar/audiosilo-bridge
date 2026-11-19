package podcastjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
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
		_, old := s.readCheckpoint(job, fixtureShowID)
		saved, now := s.readCheckpoint(there, fixtureShowID)
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
			if got, err := s.episodeAt(context.Background(), root, lib, h.mount, *job.Path, params, job.ID); err != nil || got != moved {
				t.Fatal("the running download did not find its episode", got)
			}
			if _, old = s.readCheckpoint(job, fixtureShowID); old {
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
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &moved}, fixtureShowID); !ok {
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
	// The server knows the episode only at the old path until it reads the
	// renamed folder: the fetch asks it to index the folder.
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	res, err := h.handler.FetchJob().Run(context.Background(), fetch, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	if got := res.(fetchResult); got.Path != moved || got.Fetch.Bytes != int64(len(audio)) {
		t.Fatal(got)
	}
	if asked := h.scan.last(); asked != "Renamed Show" {
		t.Fatal("the server was not asked to index the renamed folder", asked)
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
	for _, c := range []struct {
		handler jobs.Handler
		job     jobs.Job
	}{
		{h.handler.PollJob(), jobs.Job{ID: 40, Kind: PollKind, LibraryID: &lib, Path: &show, Params: params}},
		{h.handler.RetainJob(), jobs.Job{ID: 41, Kind: RetainKind, LibraryID: &lib, Path: &show, Params: params}},
	} {
		_, err := c.handler.Run(context.Background(), c.job, jobs.LocalReporter())
		var failure *netguard.Error
		if !errors.As(err, &failure) || failure.Code != "too_large" {
			t.Fatal(c.job.Kind, err)
		}
	}
}

// A show folder renamed by hand to the name of another show's folder (names
// swapped, or another show moved there) brings that show's download records
// to the paths under the name, one per episode file name both shows use. A
// download of the first show still running under the old name, or abandoned
// there by the runner, leaves that record and its partial alone: they are the
// other show's, in its folder now.
func TestADownloadUnderAnOldNameLeavesAnotherShowsRecord(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	theirs := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "0123456789abcdef", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: podcast.TempPrefix + strings.Repeat("a", 24), Offset: 6, Total: 100}
	mine := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: podcast.TempPrefix + strings.Repeat("b", 24), Offset: 6, Total: 100}
	for _, c := range []checkpoint{theirs, mine} {
		if err := root.WriteFile("Fixture Show/"+c.Temp, []byte("prefix"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.saveCheckpoint(job, root, &theirs, `"one"`); err != nil {
		t.Fatal(err)
	}
	kept := func(step string) {
		t.Helper()
		if saved, ok := s.readCheckpoint(job, theirs.ShowID); !ok || saved.ShowID != theirs.ShowID || saved.Temp != theirs.Temp {
			t.Fatal(step, ": the other show's record went", saved, ok)
		}
		if _, err := root.Lstat("Fixture Show/" + theirs.Temp); err != nil {
			t.Fatal(step, ": the other show's partial went", err)
		}
	}
	if err := s.saveCheckpoint(job, root, &mine, `"two"`); err != nil {
		t.Fatal(err)
	}
	kept("save")
	own := "Fixture Show/" + mine.Temp
	s.clearCheckpoint(job, root, &mine)
	kept("clear")
	if _, err := root.Lstat(own); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("its own partial is still the job's to remove", err)
	}
	h.handler.FetchJob().Abandon(context.Background(), job)
	kept("abandon")
}

// Records moved to a folder's new name by a take-in that went no further
// (podcast.Adopt refused or stopped before feed.json), then the folder renamed
// again by hand: the next take-in moves them from that gone earlier name too,
// or they would hold their partials in the folder for good.
func TestTakingInMovesRecordsLeftUnderAGoneEarlierName(t *testing.T) {
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
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	renameShow(t, h, "Fixture Show", "Middle Name")
	if err := s.adoptCheckpoints(context.Background(), root, lib, "Middle Name", "Fixture Show", c.ShowID, 0); err != nil {
		t.Fatal(err)
	}
	base := strings.TrimPrefix(*job.Path, "Fixture Show/")
	middle, third := "Middle Name/"+base, "Third Name/"+base
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &middle}, fixtureShowID); !ok {
		t.Fatal("the record did not move to the middle name")
	}
	renameShow(t, h, "Middle Name", "Third Name")
	if _, err := s.readShow(context.Background(), root, lib, h.mount, "Third Name"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &middle}, fixtureShowID); ok {
		t.Fatal("the record was left under the gone middle name")
	}
	if resumed, tag := s.loadCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &third}, root, c.ShowID, c.EpisodeID); tag != `"one"` || resumed.Temp != c.Temp {
		t.Fatal("the record did not follow the folder", resumed, tag)
	}
}

// A show folder renamed by hand, taken in, then renamed back while the server
// still names the old folder: a fetch there takes the folder in again where
// it is and fetches the episode, rather than failing as not found.
func TestFetchTakesInAFolderRenamedBack(t *testing.T) {
	requireProbe(t)
	h, fetch := audioHarness(t)
	audio := data(t, "chap-v24-reverse.mp3")
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"one"`)
		w.Write(audio)
	})
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	renameShow(t, h, "Fixture Show", "Renamed Show")
	if _, err := h.handler.s.readShow(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, "Renamed Show"); err != nil {
		t.Fatal(err)
	}
	renameShow(t, h, "Renamed Show", "Fixture Show")
	res, err := h.handler.FetchJob().Run(context.Background(), fetch, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	if got := res.(fetchResult); got.Path != *fetch.Path || got.Fetch.Bytes != int64(len(audio)) {
		t.Fatal(got)
	}
}

// A show folder that lost its feed.json while it still holds the job's episode
// file, or the partial its download record names, is the show's own, and a
// backup copy of it elsewhere (whose feed.json names it) is not a rename: the
// fetch is not moved to the copy, the copy is not taken in and the show's
// cache row stays, so the audio and the download are never left behind in
// the real folder. With the old folder renamed by hand and a new empty one
// made at its name, the fetch still finds the renamed folder.
func TestABackupOfAFolderThatLostItsFeedIsNotTakenIn(t *testing.T) {
	for _, left := range []string{"episode", "record", "nothing"} {
		t.Run(left, func(t *testing.T) {
			requireProbe(t)
			h, job := audioHarness(t)
			s := h.handler.s
			audio := data(t, "chap-v24-reverse.mp3")
			setAudio(h, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"one"`)
				w.Write(audio)
			})
			root, err := os.OpenRoot(h.mount)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			var params fetchParams
			if err := json.Unmarshal(job.Params, &params); err != nil {
				t.Fatal(err)
			}
			switch left {
			case "episode":
				if err := root.WriteFile(*job.Path, audio, 0o644); err != nil {
					t.Fatal(err)
				}
			case "record":
				c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: params.ShowID, EpisodeID: params.EpisodeID, Path: *job.Path, Temp: podcast.TempPrefix + nonce(), Offset: 1000, Total: int64(len(audio))}
				if err := root.WriteFile("Fixture Show/"+c.Temp, audio[:1000], 0o600); err != nil {
					t.Fatal(err)
				}
				if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.CopyFS(filepath.Join(h.mount, "Backup"), os.DirFS(filepath.Join(h.mount, "Fixture Show"))); err != nil {
				t.Fatal(err)
			}
			if left == "nothing" {
				renameShow(t, h, "Fixture Show", "Gone")
				if err := os.RemoveAll(filepath.Join(h.mount, "Gone")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(h.mount, "Fixture Show"), 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(filepath.Join(h.mount, "Fixture Show", "feed.json")); err != nil {
				t.Fatal(err)
			}
			res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if left == "nothing" {
				if got, ok := res.(fetchResult); err != nil || !ok || got.Path != "Backup/"+strings.TrimPrefix(*job.Path, "Fixture Show/") {
					t.Fatal("a folder renamed by hand with an empty one at its old name was not found", res, err)
				}
				return
			}
			if err == nil {
				t.Fatal("the fetch ran in the copy", res)
			}
			if f, err := podcast.Read(root, "Backup"); err == nil {
				t.Fatal("the copy was taken in", f.Episodes[0].Path)
			}
			if row, ok, err := h.db.PodcastShowByID(context.Background(), "server-1", params.ShowID); err != nil || ok && row.ShowPath == "Backup" {
				t.Fatal("the cache row moved to the copy", row, ok, err)
			}
		})
	}
}

// The runner abandons a fetch stopped at a resumable point by the path the
// server named. When the fetch had found its show in a folder renamed by hand
// (its record moved there), Abandon clears the record and partial where they
// are now, rather than leave them for the day-old cleanup; another episode's
// record that moved with the folder stays.
func TestAbandonClearsARecordMovedWithItsShow(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	otherPath := "Fixture Show/other.mp3"
	other := jobs.Job{LibraryID: job.LibraryID, Path: &otherPath}
	mine := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: podcast.TempPrefix + strings.Repeat("b", 24), Offset: 6, Total: 100}
	theirs := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: mine.ShowID, EpisodeID: "2cc306d831645e2b", Path: otherPath, Temp: podcast.TempPrefix + strings.Repeat("c", 24), Offset: 6, Total: 100}
	for _, c := range []struct {
		job jobs.Job
		c   *checkpoint
	}{{job, &mine}, {other, &theirs}} {
		if err := root.WriteFile("Fixture Show/"+c.c.Temp, []byte("prefix"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.saveCheckpoint(c.job, root, c.c, `"one"`); err != nil {
			t.Fatal(err)
		}
	}
	renameShow(t, h, "Fixture Show", "Renamed Show")
	var params fetchParams
	if err := json.Unmarshal(job.Params, &params); err != nil {
		t.Fatal(err)
	}
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	if got, err := s.episodeAt(context.Background(), root, lib, h.mount, *job.Path, params, job.ID); err != nil || got != moved {
		t.Fatal("the fetch did not find its episode", got, err)
	}
	h.handler.FetchJob().Abandon(context.Background(), job)
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &moved}, fixtureShowID); ok {
		t.Fatal("the moved record was left for the day-old cleanup")
	}
	if _, err := root.Lstat("Renamed Show/" + mine.Temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the moved partial was left behind", err)
	}
	if v, _ := s.o.Vault.Get(mine.Validator); v != "" {
		t.Fatal("the moved record's validator was left behind")
	}
	otherMoved := "Renamed Show/other.mp3"
	if saved, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &otherMoved}, fixtureShowID); !ok || saved.Temp != theirs.Temp {
		t.Fatal("another episode's record went", saved, ok)
	}
	if _, err := root.Lstat("Renamed Show/" + theirs.Temp); err != nil {
		t.Fatal("another episode's partial went", err)
	}
}

// A take-in while a fetch's row runs (the Bridge stopped at a resumable point)
// leaves that fetch's record at its old path. When the runner then abandons the
// fetch (its claim answered with a cancel or lost), the record under the gone
// folder goes rather than stay for good; its partial, moved with the folder,
// is unrecorded then, for sweepPartials once old.
func TestAbandonDropsARecordLeftUnderAGoneFolder(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: podcast.TempPrefix + strings.Repeat("b", 24), Offset: 6, Total: 100}
	if err := root.WriteFile("Fixture Show/"+c.Temp, []byte("prefix"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
		t.Fatal(err)
	}
	if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
		t.Fatal(err)
	}
	renameShow(t, h, "Fixture Show", "Renamed Show")
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	if _, err := s.readShow(context.Background(), root, lib, h.mount, "Renamed Show"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.readCheckpoint(job, fixtureShowID); !ok {
		t.Fatal("the take-in moved a running fetch's record")
	}
	h.handler.FetchJob().Abandon(context.Background(), job)
	if _, ok := s.readCheckpoint(job, fixtureShowID); ok {
		t.Fatal("the abandoned fetch's record stayed under the gone folder")
	}
	if v, _ := s.o.Vault.Get(c.Validator); v != "" {
		t.Fatal("the abandoned record's validator was left behind")
	}
	recorded, ok := s.recordedPartials()
	if !ok || recorded[c.Temp] {
		t.Fatal("the moved partial is still recorded", ok)
	}
}

// A poll the server names by a folder it cannot read finds its show where it
// is now, as it does a folder that is gone: here the folder holds an
// unreadable feed.json, or a copy of another show renamed by hand (refused,
// that show's own folder taken in). The folder is left as it is.
func TestPollFindsItsShowPastAFolderItCannotRead(t *testing.T) {
	lib := int64(3)
	poll := func(t *testing.T, h *harness, show, id string) (any, error) {
		t.Helper()
		params, _ := json.Marshal(map[string]string{"show_id": id, "etag": `"abc"`})
		return h.handler.PollJob().Run(context.Background(), jobs.Job{ID: h.claimID.Add(1), Kind: PollKind, LibraryID: &lib, Path: &show, Params: params}, jobs.LocalReporter())
	}
	t.Run("unreadable feed.json", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		show, id := followed(t, h)
		renameShow(t, h, show, "Renamed Show")
		if res, err := poll(t, h, show, id); err != nil || fields(t, res)["show_path"] != "Renamed Show" {
			t.Fatal(res, err)
		}
		if err := os.Mkdir(filepath.Join(h.mount, show), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.mount, show, "feed.json"), []byte("not a feed"), 0o644); err != nil {
			t.Fatal(err)
		}
		if res, err := poll(t, h, show, id); err != nil || fields(t, res)["show_path"] != "Renamed Show" {
			t.Fatal("the poll did not find its show past an unreadable folder", res, err)
		}
		if b, err := os.ReadFile(filepath.Join(h.mount, show, "feed.json")); err != nil || string(b) != "not a feed" {
			t.Fatal("the unreadable folder changed", string(b), err)
		}
	})
	t.Run("a copy of another show", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		a, idA := followed(t, h)
		res, err := h.handler.Run(context.Background(), h.claim(h.http.URL+"/another", false), jobs.LocalReporter())
		if err != nil {
			t.Fatal(res, err)
		}
		r := fields(t, res)
		b, idB := r["show_path"].(string), r["show_id"].(string)
		renameShow(t, h, a, "A Moved")
		if res, err := poll(t, h, a, idA); err != nil || fields(t, res)["show_path"] != "A Moved" {
			t.Fatal(res, err)
		}
		renameShow(t, h, b, "B Moved")
		if err := os.CopyFS(filepath.Join(h.mount, a), os.DirFS(filepath.Join(h.mount, "B Moved"))); err != nil {
			t.Fatal(err)
		}
		if res, err := poll(t, h, b, idB); err != nil || fields(t, res)["show_path"] != "B Moved" {
			t.Fatal(res, err)
		}
		if res, err := poll(t, h, a, idA); err != nil || fields(t, res)["show_path"] != "A Moved" {
			t.Fatal("the poll did not find its show past a refused copy", res, err)
		}
		root, err := os.OpenRoot(h.mount)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		if f, from, err := podcast.ReadMoved(root, a); err != nil || from != b || f.ShowID != idB {
			t.Fatal("the copy changed", from, err)
		}
	})
}

// A take-in after a fetch found its show in a folder renamed by hand leaves
// that fetch's record alone while it runs, though the fetch's row still names
// the server's old path: it is matched by its show and episode. Once the
// fetch ends, the record moves with the folder.
func TestATakeInLeavesARelocatedFetchsRecord(t *testing.T) {
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
	if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
		t.Fatal(err)
	}
	var params fetchParams
	if err := json.Unmarshal(job.Params, &params); err != nil {
		t.Fatal(err)
	}
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	renameShow(t, h, "Fixture Show", "Renamed Show")
	base := strings.TrimPrefix(*job.Path, "Fixture Show/")
	relocated, third := "Renamed Show/"+base, "Third Name/"+base
	if got, err := s.episodeAt(context.Background(), root, lib, h.mount, *job.Path, params, job.ID); err != nil || got != relocated {
		t.Fatal("the fetch did not find its episode", got, err)
	}
	renameShow(t, h, "Renamed Show", "Third Name")
	if _, err := s.readShow(context.Background(), root, lib, h.mount, "Third Name"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &relocated}, fixtureShowID); !ok {
		t.Fatal("a running fetch's record was moved")
	}
	if err := h.db.FinishServerJob(context.Background(), job.ID, 1, store.JobFailed, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.adoptCheckpoints(context.Background(), root, lib, "Third Name", "Renamed Show", c.ShowID, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &third}, fixtureShowID); !ok {
		t.Fatal("the ended fetch's record did not follow the folder")
	}
}

// A fetch that finds its show in a folder taken in elsewhere but cannot read
// the download records waits (write_failed), as a take-in does, rather than
// run at the new path and leave its record and partial under the old one.
func TestAFetchThatCannotMoveItsRecordWaits(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	renameShow(t, h, "Fixture Show", "Renamed Show")
	if _, err := s.readShow(context.Background(), root, lib, h.mount, "Renamed Show"); err != nil {
		t.Fatal(err)
	}
	var params fetchParams
	if err := json.Unmarshal(job.Params, &params); err != nil {
		t.Fatal(err)
	}
	data := s.o.DataDir
	blocked := filepath.Join(t.TempDir(), "a file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.o.DataDir = blocked
	got, err := s.episodeAt(context.Background(), root, lib, h.mount, *job.Path, params, job.ID)
	var failure *netguard.Error
	if !errors.As(err, &failure) || failure.Code != "write_failed" || got != *job.Path {
		t.Fatal("the fetch moved without its record", got, err)
	}
	s.o.DataDir = data
	moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	if got, err := s.episodeAt(context.Background(), root, lib, h.mount, *job.Path, params, job.ID); err != nil || got != moved {
		t.Fatal(got, err)
	}
}

// A show folder renamed by hand while its episode downloads (bridge#13): the
// partial, its record and the episode's lock moved with the folder. The fetch
// publishes the episode there from the bytes it has (one request, never from
// byte 0 again), clears the record and the lock there, and asks the server to
// index the show folder (the episode's new path it does not know yet).
func TestAFetchFollowsItsShowRenamedMidDownload(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
		t.Fatal(err)
	}
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	audio := data(t, "chap-v24-reverse.mp3")
	old := *job.Path
	// written waits until the partial in the show folder holds n bytes.
	written := func(n int64) bool { return partialAt(filepath.Join(h.mount, "Fixture Show"), n) }
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Range") != "" {
			t.Error("the episode was downloaded again", r.Header.Get("Range"))
		}
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		w.Write(audio[:1000])
		w.(http.Flusher).Flush()
		if !written(1000) {
			t.Error("the download did not start")
			return
		}
		// The next part's write saves the download record first.
		w.Write(audio[1000:2000])
		w.(http.Flusher).Flush()
		if !written(2000) {
			t.Error("the download did not go on")
			return
		}
		if _, ok := s.readCheckpoint(job, fixtureShowID); !ok {
			t.Error("no download record before the rename")
		}
		if err := os.Rename(filepath.Join(h.mount, "Fixture Show"), filepath.Join(h.mount, "Renamed Show")); err != nil {
			t.Error(err)
		}
		w.Write(audio[2000:])
	})
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	moved := "Renamed Show/" + strings.TrimPrefix(old, "Fixture Show/")
	got := res.(fetchResult)
	if got.Path != moved || got.Reused || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 1 {
		t.Fatal(got, calls.Load())
	}
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if !podcast.Matches(root, moved, &got.Fetch) {
		t.Fatal("the episode was not published in the renamed folder")
	}
	if _, err := os.Lstat(filepath.Join(h.mount, "Fixture Show")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old folder came back", err)
	}
	if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
		t.Fatal("a partial was left behind", left)
	}
	if _, err := root.Lstat(podcast.AttachmentPath(moved, ".fetch.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the episode's lock was left in the renamed folder", err)
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if records, err := s.records(dir, 3); err != nil || len(records) != 0 {
		t.Fatal("a download record was left", records, err)
	}
	if asked := h.scan.last(); asked != "Renamed Show" {
		t.Fatal("the server was not asked to index the renamed folder", asked)
	}
}

// cutAfterRename answers the first episode request with 2000 bytes of audio
// (a download record saved on the way), renames the show folder by hand and
// cuts the connection; every later request goes to then.
func cutAfterRename(t *testing.T, h *harness, job jobs.Job, audio []byte, calls *atomic.Int32, then http.HandlerFunc) {
	t.Helper()
	cutAfterMove(t, h, job, audio, calls, os.Rename, then)
}

// cutAfterMove is cutAfterRename with the show folder moved by move (given
// its path and the new one).
func cutAfterMove(t *testing.T, h *harness, job jobs.Job, audio []byte, calls *atomic.Int32, move func(from, to string) error, then http.HandlerFunc) {
	t.Helper()
	written := func(n int64) bool { return partialAt(filepath.Join(h.mount, "Fixture Show"), n) }
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			then(w, r)
			return
		}
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		w.Write(audio[:1000])
		w.(http.Flusher).Flush()
		if !written(1000) {
			t.Error("the download did not start")
			return
		}
		w.Write(audio[1000:2000])
		w.(http.Flusher).Flush()
		if !written(2000) {
			t.Error("the download did not go on")
			return
		}
		if _, ok := h.handler.s.readCheckpoint(job, fixtureShowID); !ok {
			t.Error("no download record before the rename")
		}
		if err := move(filepath.Join(h.mount, "Fixture Show"), filepath.Join(h.mount, "Renamed Show")); err != nil {
			t.Error(err)
		}
	})
}

// A show folder "renamed" by a copy and a delete while its episode downloads
// (a move across filesystems, a file manager's): the lock file in the new
// folder is a copy, not the file the fetch holds, so the lock cannot follow
// the folder. The fetch takes the episode's lock there before it goes on from
// the copied partial, rather than checkpoint, clear and publish there holding
// none, and lets it go (the file removed) at its end.
func TestAFetchFollowingACopiedFolderLocksItsEpisodeThere(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	audio := data(t, "chap-v24-reverse.mp3")
	moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	lockFile := filepath.Join(h.mount, podcast.AttachmentPath(moved, ".fetch.lock"))
	resume := resumeAt(t, audio)
	var calls atomic.Int32
	cutAfterMove(t, h, job, audio, &calls, func(from, to string) error {
		return copyThenDelete(t, from, to)
	}, func(w http.ResponseWriter, r *http.Request) {
		f, err := os.OpenFile(lockFile, os.O_RDWR, 0)
		if err != nil {
			t.Error("no lock file where the show is now", err)
		} else {
			if free, err := tryLock(f); err != nil || free {
				t.Error("the fetch went on holding no lock on its episode where the show is now", err)
			}
			f.Close()
		}
		resume(w, r)
	})
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if got, ok := res.(fetchResult); err != nil || !ok || got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 {
		t.Fatal(res, err, calls.Load())
	}
	if _, err := os.Lstat(lockFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the episode's lock was left in the copied folder", err)
	}
	if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
		t.Fatal("a partial was left behind", left)
	}
	noRecords(t, h.handler.s)
}

// resumeAt answers a ranged request for the rest of audio from 2000 bytes,
// as the host does; a request for the whole episode is an error.
func resumeAt(t *testing.T, audio []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=2000-" || r.Header.Get("If-Range") != `"one"` {
			t.Error("the episode was downloaded again from byte 0", r.Header.Get("Range"))
		}
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Range", "bytes 2000-"+strconv.Itoa(len(audio)-1)+"/"+strconv.Itoa(len(audio)))
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)-2000))
		w.WriteHeader(206)
		w.Write(audio[2000:])
	}
}

// noRecords fails t when any download record of the library is left.
func noRecords(t *testing.T, s *service) {
	t.Helper()
	dir, err := s.checkpointRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if records, err := s.records(dir, 3); err != nil || len(records) != 0 {
		t.Fatal("a download record was left", records, err)
	}
}

// A show folder renamed by hand while its episode downloads, then the host
// cutting the connection: the partial moved with the folder. The host's
// retry inside the download finds the show there and goes on from the bytes
// it has (a ranged request), never from byte 0; a host that then fails for
// good fails the job with the partial and its record cleared where they are
// now, not left for the day-old sweep.
func TestAFetchRenamedMidDownloadRetriesFromItsPartialThere(t *testing.T) {
	for _, fails := range []bool{false, true} {
		requireProbe(t)
		h, job := audioHarness(t)
		s := h.handler.s
		if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
			t.Fatal(err)
		}
		h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
		audio := data(t, "chap-v24-reverse.mp3")
		then := resumeAt(t, audio)
		var ranged atomic.Bool
		if fails {
			then = func(w http.ResponseWriter, r *http.Request) {
				ranged.Store(r.Header.Get("Range") == "bytes=2000-")
				w.WriteHeader(404)
			}
		}
		var calls atomic.Int32
		cutAfterRename(t, h, job, audio, &calls, then)
		res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
		if fails {
			if err == nil || calls.Load() != 2 || !ranged.Load() {
				t.Fatal("the failing host was not asked to resume", res, err, calls.Load())
			}
		} else if got, ok := res.(fetchResult); err != nil || !ok || got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 {
			t.Fatal(res, err, calls.Load())
		}
		if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
			t.Fatal("a partial was left behind", fails, left)
		}
		if _, err := os.Lstat(filepath.Join(h.mount, "Fixture Show")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the old folder came back", err)
		}
		noRecords(t, s)
	}
}

// Another open row of the same episode (a duplicate claim) does not keep a
// fetch from its own download record when its show folder was renamed by
// hand, before the download or while it ran: adoptCheckpoints leaves a record
// a running row names, but the fetch holds the episode's lock, so the record
// is its own. It goes on from the partial (never from byte 0) and leaves no
// record at the old path for the day-old cleanup.
func TestAFetchTakesItsOwnRecordPastADuplicateClaim(t *testing.T) {
	for _, during := range []bool{false, true} {
		requireProbe(t)
		h, job := audioHarness(t)
		s := h.handler.s
		for _, id := range []int64{job.ID, job.ID + 1} {
			if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: id, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
				t.Fatal(err)
			}
		}
		h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
		audio := data(t, "chap-v24-reverse.mp3")
		var calls atomic.Int32
		if during {
			cutAfterRename(t, h, job, audio, &calls, resumeAt(t, audio))
		} else {
			root, err := os.OpenRoot(h.mount)
			if err != nil {
				t.Fatal(err)
			}
			var params fetchParams
			if err := json.Unmarshal(job.Params, &params); err != nil {
				t.Fatal(err)
			}
			c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: params.ShowID, EpisodeID: params.EpisodeID, Path: *job.Path, Temp: podcast.TempPrefix + nonce(), Offset: 2000, Total: int64(len(audio))}
			if err := root.WriteFile("Fixture Show/"+c.Temp, audio[:2000], 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
				t.Fatal(err)
			}
			root.Close()
			renameShow(t, h, "Fixture Show", "Renamed Show")
			resume := resumeAt(t, audio)
			setAudio(h, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); resume(w, r) })
		}
		res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		want := int32(1)
		if during {
			want = 2
		}
		if got, ok := res.(fetchResult); err != nil || !ok || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != want {
			t.Fatal(during, res, err, calls.Load())
		}
		if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
			t.Fatal("a partial was left behind", during, left)
		}
		noRecords(t, s)
	}
}

// A fetch's own record left at the old path while one is at the new path
// already: the new one wins and the old one goes, its partial (in the
// renamed folder now) with it, unless the new record names the same partial
// (a move that wrote there and could not remove the old one) or the partial
// is the one the fetch is writing (live: a download that followed the folder,
// whose record at the new path may be a stale one or another show's).
func TestAdoptOwnClearsAnOldRecordPastANewOne(t *testing.T) {
	for _, tc := range []struct{ same, live bool }{{false, false}, {true, false}, {false, true}} {
		same, live := tc.same, tc.live
		h, job := audioHarness(t)
		s := h.handler.s
		root, err := os.OpenRoot(h.mount)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		var params fetchParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			t.Fatal(err)
		}
		old := *job.Path
		mk := func(at string, temp string) {
			c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: params.ShowID, EpisodeID: params.EpisodeID, Path: at, Temp: temp, Offset: 6, Total: 100}
			if err := root.WriteFile("Fixture Show/"+temp, []byte("prefix"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.saveCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &at}, root, &c, `"one"`); err != nil {
				t.Fatal(err)
			}
		}
		mk(old, podcast.TempPrefix+"old")
		moved := "Renamed Show/" + strings.TrimPrefix(old, "Fixture Show/")
		newTemp := podcast.TempPrefix + "new"
		if same {
			newTemp = podcast.TempPrefix + "old"
		}
		mk(moved, newTemp)
		renameShow(t, h, "Fixture Show", "Renamed Show")
		there := jobs.Job{LibraryID: job.LibraryID, Path: &moved}
		writing := ""
		if live {
			writing = podcast.TempPrefix + "old"
		}
		s.adoptOwn(there, root, old, params, writing)
		if _, ok := s.readCheckpoint(job, fixtureShowID); ok {
			t.Fatal("the old record stayed", tc)
		}
		if saved, ok := s.readCheckpoint(there, fixtureShowID); !ok || saved.Temp != newTemp {
			t.Fatal("the new record changed", tc, saved)
		}
		_, err = os.Lstat(filepath.Join(h.mount, "Renamed Show", podcast.TempPrefix+"old"))
		if (same || live) != (err == nil) {
			t.Fatal("the old partial", tc, err)
		}
	}
}

// A record at the new path that is not this episode's (another show's, the
// names of two folders swapped by hand and an episode file name both use, or
// one no job can read back) costs a fetch found elsewhere nothing of its own.
// Another show's is another record (named by its show, recordName), left for
// its show (or the day-old sweep, sweepRecords), and the fetch goes on from
// its own partial rather than from byte 0. Past one it
// cannot read the fetch downloads again, and its old record follows at its
// end, with its partial, rather than stay under the folder that is gone,
// where nothing would ever clear it or sweep the partial it names.
func TestAFetchKeepsItsPartialPastARecordNotItsOwn(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		requireProbe(t)
		h, job := audioHarness(t)
		s := h.handler.s
		h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
		audio := data(t, "chap-v24-reverse.mp3")
		root, err := os.OpenRoot(h.mount)
		if err != nil {
			t.Fatal(err)
		}
		var params fetchParams
		if err := json.Unmarshal(job.Params, &params); err != nil {
			t.Fatal(err)
		}
		own := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: params.ShowID, EpisodeID: params.EpisodeID, Path: *job.Path, Temp: podcast.TempPrefix + nonce(), Offset: 2000, Total: int64(len(audio))}
		if err := root.WriteFile("Fixture Show/"+own.Temp, audio[:2000], 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.saveCheckpoint(job, root, &own, `"one"`); err != nil {
			t.Fatal(err)
		}
		moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
		there := jobs.Job{LibraryID: job.LibraryID, Path: &moved}
		if unreadable {
			dir, err := s.checkpointRoot()
			if err != nil {
				t.Fatal(err)
			}
			if err := dir.WriteFile(recordName(3, params.ShowID, moved), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			dir.Close()
		} else {
			other := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "ffffffffffffffff", EpisodeID: params.EpisodeID, Path: moved, Temp: podcast.TempPrefix + nonce(), Offset: 6, Total: 100}
			if err := s.saveCheckpoint(there, root, &other, `"two"`); err != nil {
				t.Fatal(err)
			}
		}
		root.Close()
		renameShow(t, h, "Fixture Show", "Renamed Show")
		var calls atomic.Int32
		resume := resumeAt(t, audio)
		setAudio(h, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if unreadable {
				w.Header().Set("ETag", `"one"`)
				w.Write(audio)
				return
			}
			resume(w, r)
		})
		res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		if got, ok := res.(fetchResult); err != nil || !ok || got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 1 {
			t.Fatal(unreadable, res, err, calls.Load())
		}
		if _, ok := s.readCheckpoint(job, fixtureShowID); ok {
			t.Fatal("a record stayed under the folder that is gone", unreadable)
		}
		if !unreadable {
			if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
				t.Fatal("a partial was left behind", left)
			}
			dir, err := s.checkpointRoot()
			if err != nil {
				t.Fatal(err)
			}
			left, err := s.records(dir, 3)
			dir.Close()
			if err != nil || len(left) != 1 || left[0].ShowID != "ffffffffffffffff" {
				t.Fatal("the fetch's own record was left, or the other show's went", left, err)
			}
			continue
		}
		if saved, ok := s.readCheckpoint(there, fixtureShowID); !ok || saved.Temp != own.Temp {
			t.Fatal("the fetch's old record did not follow the show", saved, ok)
		}
		if _, err := os.Lstat(filepath.Join(h.mount, "Renamed Show", own.Temp)); err != nil {
			t.Fatal("the partial went for a record that could not be read", err)
		}
	}
}

// A show folder renamed by hand while the fetch waits to retry a host that
// failed before any audio arrived (no partial yet): the retry finds the show
// where it is now and makes its partial there, rather than fail in the folder
// that is gone.
func TestAFetchRenamedBeforeItsFirstBytesRetriesThere(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	audio := data(t, "chap-v24-reverse.mp3")
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if err := os.Rename(filepath.Join(h.mount, "Fixture Show"), filepath.Join(h.mount, "Renamed Show")); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"one"`)
		w.Write(audio)
	})
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	if got, ok := res.(fetchResult); err != nil || !ok || got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 {
		t.Fatal(res, err, calls.Load())
	}
	if _, err := os.Lstat(filepath.Join(h.mount, "Fixture Show")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old folder came back", err)
	}
	if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
		t.Fatal("a partial was left behind", left)
	}
	noRecords(t, h.handler.s)
}

// An episode already fetched whose show folder was renamed by hand since is
// taken as it is (reuse), and the server, which knows it only at the old
// path, is asked to index the folder; one that never moved is indexed by its
// own path.
func TestAReusedFetchOfARenamedShowIndexesItsFolder(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	audio := data(t, "chap-v24-reverse.mp3")
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"one"`)
		w.Write(audio)
	})
	if res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter()); err != nil {
		t.Fatal(res, err)
	}
	if asked := h.scan.last(); asked != *job.Path {
		t.Fatal("an episode that never moved was not indexed by its path", asked)
	}
	renameShow(t, h, "Fixture Show", "Renamed Show")
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	if got := res.(fetchResult); !got.Reused || got.Path != "Renamed Show/"+strings.TrimPrefix(*job.Path, "Fixture Show/") {
		t.Fatal(got)
	}
	if asked := h.scan.last(); asked != "Renamed Show" {
		t.Fatal("the server was not asked to index the renamed folder", asked)
	}
}

// A show folder renamed by hand mid-download, then the Bridge stopping: the
// download record stays though its partial is no longer at its path (it moved
// with the folder), and the resumed job finds the show, its record and the
// partial there and goes on from the bytes it has (a ranged request), never
// from byte 0 again.
func TestAFetchStoppedAfterItsShowWasRenamedResumesThere(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
		t.Fatal(err)
	}
	audio := data(t, "chap-v24-reverse.mp3")
	started := make(chan struct{})
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("ETag", `"one"`)
		if n == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
			w.WriteHeader(200)
			w.Write(audio[:1000])
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			return
		}
		if r.Header.Get("Range") != "bytes=1000-" || r.Header.Get("If-Range") != `"one"` {
			t.Error("the episode was downloaded again", r.Header)
		}
		w.Header().Set("Content-Range", "bytes 1000-"+strconv.Itoa(len(audio)-1)+"/"+strconv.Itoa(len(audio)))
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)-1000))
		w.WriteHeader(206)
		w.Write(audio[1000:])
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := h.handler.FetchJob().Run(ctx, job, jobs.LocalReporter()); done <- err }()
	<-started
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if matches := partials(filepath.Join(h.mount, "Fixture Show")); len(matches) == 1 {
			if st, err := os.Stat(matches[0]); err == nil && st.Size() == 1000 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the download did not start")
		}
	}
	renameShow(t, h, "Fixture Show", "Renamed Show")
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the stopped job succeeded")
	}
	if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 1 {
		t.Fatal("the partial did not stay in the renamed folder", left)
	}
	job.Resumed = true
	res, err := New(h.handler.s.o).FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	if got := res.(fetchResult); got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 {
		t.Fatal(got, calls.Load())
	}
	if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
		t.Fatal("a partial was left behind", left)
	}
	if _, err := os.Lstat(filepath.Join(h.mount, "Fixture Show")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old folder came back", err)
	}
}

// copyThenDelete "renames" folder from to to as a move across filesystems or
// a file manager does: a copy, then the original deleted. to may be from
// itself (the folder put back from a copy made elsewhere).
func copyThenDelete(t *testing.T, from, to string) error {
	stage := to
	if to == from {
		stage = filepath.Join(t.TempDir(), "copy")
	}
	if err := os.CopyFS(stage, os.DirFS(from)); err != nil {
		return err
	}
	if err := os.RemoveAll(from); err != nil {
		return err
	}
	if stage != to {
		return os.CopyFS(to, os.DirFS(stage))
	}
	return nil
}

// partialAt waits until the one partial in show holds at least n bytes.
func partialAt(show string, n int64) bool {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if matches := partials(show); len(matches) == 1 {
			if st, err := os.Stat(matches[0]); err == nil && st.Size() >= n {
				return true
			}
		}
	}
	return false
}

// A show folder "renamed" by a copy and a delete while its episode downloads
// and the host goes on sending: the partial the fetch writes is deleted, a
// copy of its start in the new folder. The fetch notices before it publishes
// and writes the whole episode where the show is now (one request), rather
// than fail on the stale copy and fetch again from byte 0; nothing is left,
// the lock it takes there (a copy, not the file held) included. A folder put
// back at its own name from a copy is the same case at the same path.
func TestAFetchFollowsItsShowCopiedAndDeletedMidDownload(t *testing.T) {
	for _, to := range []string{"Renamed Show", "Fixture Show"} {
		requireProbe(t)
		h, job := audioHarness(t)
		moved := to + "/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
		lockFile := filepath.Join(h.mount, podcast.AttachmentPath(moved, ".fetch.lock"))
		var checked atomic.Bool
		h.scan.knows = func(p string) bool {
			// The episode's rescan, after the publication: the fetch holds the
			// lock in the folder it followed, taken anew there (a copy of the
			// file it held) at the same path as at a new one.
			if p == moved {
				checked.Store(true)
				if f, err := os.OpenFile(lockFile, os.O_RDWR, 0); err != nil {
					t.Error(to, "no lock file where the show is now", err)
				} else {
					if free, err := tryLock(f); err != nil || free {
						t.Error(to, "the fetch published holding no lock where the show is now", err)
					}
					f.Close()
				}
			}
			return strings.HasPrefix(p, "Fixture Show/")
		}
		audio := data(t, "chap-v24-reverse.mp3")
		show := filepath.Join(h.mount, "Fixture Show")
		var calls atomic.Int32
		setAudio(h, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Header.Get("Range") != "" {
				t.Error("the episode was downloaded again", r.Header.Get("Range"))
			}
			w.Header().Set("ETag", `"one"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
			w.WriteHeader(200)
			w.Write(audio[:2000])
			w.(http.Flusher).Flush()
			if !partialAt(show, 2000) {
				t.Error("the download did not start")
				return
			}
			if err := copyThenDelete(t, show, filepath.Join(h.mount, to)); err != nil {
				t.Error(err)
			}
			w.Write(audio[2000:])
		})
		res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		got, ok := res.(fetchResult)
		if err != nil || !ok || got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 1 || !checked.Load() {
			t.Fatal(to, res, err, calls.Load(), checked.Load())
		}
		root, err := os.OpenRoot(h.mount)
		if err != nil {
			t.Fatal(err)
		}
		if !podcast.Matches(root, moved, &got.Fetch) {
			t.Fatal(to, "the episode was not published where the show is now")
		}
		root.Close()
		if left := partials(filepath.Join(h.mount, to)); len(left) != 0 {
			t.Fatal(to, "a partial was left behind", left)
		}
		if _, err := os.Lstat(filepath.Join(h.mount, podcast.AttachmentPath(moved, ".fetch.lock"))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(to, "the episode's lock was left in the copied folder", err)
		}
		noRecords(t, h.handler.s)
	}
}

// The same copy and delete caught on the way (the first checkpoint's check,
// or the once-a-second one): the bytes so far are written where the show is
// now, and the rest of the response after them, so when the host then
// cuts the connection the retry resumes from all of them there (a ranged
// request), not from the stale copy's size, which no longer matches the
// record, or from byte 0.
func TestAFetchCopiedAndDeletedMidDownloadResumesFromAllItsBytes(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	audio := data(t, "chap-v24-reverse.mp3")
	show := filepath.Join(h.mount, "Fixture Show")
	moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			if r.Header.Get("Range") != "bytes=4000-" || r.Header.Get("If-Range") != `"one"` {
				t.Error("the retry did not resume from the bytes the fetch held", r.Header.Get("Range"))
			}
			w.Header().Set("ETag", `"one"`)
			w.Header().Set("Content-Range", "bytes 4000-"+strconv.Itoa(len(audio)-1)+"/"+strconv.Itoa(len(audio)))
			w.Header().Set("Content-Length", strconv.Itoa(len(audio)-4000))
			w.WriteHeader(206)
			w.Write(audio[4000:])
			return
		}
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		w.Write(audio[:2000])
		w.(http.Flusher).Flush()
		if !partialAt(show, 2000) {
			t.Error("the download did not start")
			return
		}
		if err := copyThenDelete(t, show, filepath.Join(h.mount, "Renamed Show")); err != nil {
			t.Error(err)
		}
		w.Write(audio[2000:3000])
		w.(http.Flusher).Flush()
		time.Sleep(1100 * time.Millisecond) // past the once-a-second check, however the reads fell
		w.Write(audio[3000:4000])
		w.(http.Flusher).Flush()
		if !partialAt(filepath.Join(h.mount, "Renamed Show"), 4000) {
			t.Error("the partial was not written where the show is now")
		}
	})
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if got, ok := res.(fetchResult); err != nil || !ok || got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 {
		t.Fatal(res, err, calls.Load())
	}
	if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
		t.Fatal("a partial was left behind", left)
	}
	if _, err := os.Lstat(filepath.Join(h.mount, podcast.AttachmentPath(moved, ".fetch.lock"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the episode's lock was left in the copied folder", err)
	}
	noRecords(t, h.handler.s)
}

// A show folder renamed by hand while the fetch waits to retry a host that
// failed before any audio arrived, and another folder taking its old name
// meanwhile (a new one, or another show's: names swapped by hand). The retry
// reads the episode's feed.json, finds the show where it is now and makes its
// partial there, rather than in the folder at the old name, then fail.
func TestAFetchRetryFindsItsShowPastAFolderAtItsOldName(t *testing.T) {
	for _, other := range []bool{false, true} {
		requireProbe(t)
		h, job := audioHarness(t)
		h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
		audio := data(t, "chap-v24-reverse.mp3")
		var calls atomic.Int32
		setAudio(h, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				renameShow(t, h, "Fixture Show", "Renamed Show")
				if err := os.Mkdir(filepath.Join(h.mount, "Fixture Show"), 0o755); err != nil {
					t.Error(err)
				}
				if other {
					root, err := os.OpenRoot(h.mount)
					if err != nil {
						t.Error(err)
						return
					}
					defer root.Close()
					var f podcast.Feed
					if err := json.Unmarshal(data(t, "feed-fetch-v1.json"), &f); err != nil {
						t.Error(err)
						return
					}
					f.ShowID, f.Episodes[0].Fetch = "0123456789abcdef", nil
					if _, err := podcast.Write(root, "Fixture Show", &f, h.handler.s.o.Now()); err != nil {
						t.Error(err)
					}
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("ETag", `"one"`)
			w.Write(audio)
		})
		res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
		if got, ok := res.(fetchResult); err != nil || !ok || got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 {
			t.Fatal(other, res, err, calls.Load())
		}
		for _, show := range []string{"Fixture Show", "Renamed Show"} {
			if left := partials(filepath.Join(h.mount, show)); len(left) != 0 {
				t.Fatal(other, "a partial was left behind", left)
			}
		}
		if _, err := os.Lstat(filepath.Join(h.mount, podcast.AttachmentPath(*job.Path, ".fetch.lock"))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(other, "a lock was made in the folder at the old name", err)
		}
		noRecords(t, h.handler.s)
	}
}

// The same copy and delete with the host cutting the connection before the
// next check (within the second): the interruption writes the bytes held
// where the show is now before it records them, so the retry resumes from
// all of them there rather than restart from byte 0 on the stale copy.
func TestAFetchCopiedAndDeletedJustBeforeACutResumesFromAllItsBytes(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	audio := data(t, "chap-v24-reverse.mp3")
	show := filepath.Join(h.mount, "Fixture Show")
	moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			if r.Header.Get("Range") != "bytes=4000-" || r.Header.Get("If-Range") != `"one"` {
				t.Error("the retry did not resume from the bytes the fetch held", r.Header.Get("Range"))
			}
			w.Header().Set("ETag", `"one"`)
			w.Header().Set("Content-Range", "bytes 4000-"+strconv.Itoa(len(audio)-1)+"/"+strconv.Itoa(len(audio)))
			w.Header().Set("Content-Length", strconv.Itoa(len(audio)-4000))
			w.WriteHeader(206)
			w.Write(audio[4000:])
			return
		}
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		w.Write(audio[:2000])
		w.(http.Flusher).Flush()
		if !partialAt(show, 2000) {
			t.Error("the download did not start")
			return
		}
		// The first checkpoint (at 2000), then the copy: the next check is a
		// second away.
		w.Write(audio[2000:3000])
		w.(http.Flusher).Flush()
		if !partialAt(show, 3000) {
			t.Error("the download did not go on")
			return
		}
		if err := copyThenDelete(t, show, filepath.Join(h.mount, "Renamed Show")); err != nil {
			t.Error(err)
		}
		w.Write(audio[3000:4000])
		w.(http.Flusher).Flush()
	})
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if got, ok := res.(fetchResult); err != nil || !ok || got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 {
		t.Fatal(res, err, calls.Load())
	}
	if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
		t.Fatal("a partial was left behind", left)
	}
	noRecords(t, h.handler.s)
}

// A show folder deleted mid-download and another show's folder renamed to its
// name by hand, with an episode of the same file name and its lock file. The
// fetch, finding no folder that holds its show, never takes that lock: it
// would wait on the other show's fetch, or hold its lock, while it writes.
// Nor does it download the episode again there once the partial it wrote is
// found gone (the hash before the probe).
func TestAFetchNeverTakesAnotherShowsLockAtItsPath(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	audio := data(t, "chap-v24-reverse.mp3")
	show := filepath.Join(h.mount, "Fixture Show")
	other := filepath.Join(h.mount, podcast.AttachmentPath(*job.Path, ".fetch.lock"))
	var taken atomic.Bool
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		w.Write(audio[:2000])
		w.(http.Flusher).Flush()
		if !partialAt(show, 2000) {
			t.Error("the download did not start")
			return
		}
		if err := os.RemoveAll(show); err != nil {
			t.Error(err)
			return
		}
		if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
			t.Error(err)
			return
		}
		root, err := os.OpenRoot(h.mount)
		if err != nil {
			t.Error(err)
			return
		}
		defer root.Close()
		var f podcast.Feed
		if err := json.Unmarshal(data(t, "feed-fetch-v1.json"), &f); err != nil {
			t.Error(err)
			return
		}
		f.ShowID, f.Episodes[0].Fetch = "0123456789abcdef", nil
		if _, err := podcast.Write(root, "Fixture Show", &f, h.handler.s.o.Now()); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(other, nil, 0o600); err != nil {
			t.Error(err)
		}
		w.Write(audio[2000:3000])
		w.(http.Flusher).Flush()
		time.Sleep(1100 * time.Millisecond) // past the once-a-second check
		lf, err := os.OpenFile(other, os.O_RDWR, 0)
		if err != nil {
			t.Error(err)
			return
		}
		if ok, err := tryLock(lf); err != nil || !ok {
			taken.Store(true)
		} else {
			unlockFile(lf)
		}
		_ = lf.Close()
		w.Write(audio[3000:])
	})
	if res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter()); err == nil {
		t.Fatal("the episode was published without its show", res)
	}
	if taken.Load() {
		t.Fatal("the fetch took the other show's lock")
	}
	// Its partial gone with the folder, the episode is not downloaded a second
	// time into the other show's folder.
	if calls.Load() != 1 {
		t.Fatal("the episode was downloaded again where its show is not:", calls.Load())
	}
	if left := partials(show); len(left) != 0 {
		t.Fatal("a partial was left in the other show's folder", left)
	}
}

// A show folder renamed by hand only in case ("Fixture Show" to "fixture
// show") on a case-insensitive filesystem (macOS APFS, SMB), where the
// server's spelling still opens the folder: a fetch naming the old spelling
// works under the folder's name on disk (its feed.json taken in, the episode
// saved there, the server asked to index that folder), and a poll naming it
// does too, so no two jobs lock the folder under two names.
func TestAFetchResolvesACaseOnlyRename(t *testing.T) {
	requireProbe(t)
	h, fetch := audioHarness(t)
	renameShow(t, h, "Fixture Show", "fixture show")
	if _, err := os.Lstat(filepath.Join(h.mount, "Fixture Show")); err != nil {
		t.Skip("the filesystem is case-sensitive")
	}
	audio := data(t, "chap-v24-reverse.mp3")
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"one"`)
		w.Write(audio)
	})
	moved := "fixture show/" + strings.TrimPrefix(*fetch.Path, "Fixture Show/")
	res, err := h.handler.FetchJob().Run(context.Background(), fetch, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	if got := res.(fetchResult); got.Path != moved || got.Fetch.Bytes != int64(len(audio)) {
		t.Fatal(got)
	}
	if f := h.read("fixture show"); !strings.HasPrefix(f.Episodes[0].Path, "fixture show/") {
		t.Fatal("feed.json still names the old spelling", f.Episodes[0].Path)
	}
	if asked := h.scan.last(); !strings.HasPrefix(asked, "fixture show") {
		t.Fatal("the server was asked to index the old spelling", asked)
	}
	show := "Fixture Show"
	params, _ := json.Marshal(map[string]string{"show_id": "a1b2c3d4e5f60718", "etag": `"abc"`})
	poll := jobs.Job{ID: 90, Kind: PollKind, LibraryID: fetch.LibraryID, Path: &show, Params: params}
	h.handler.s.o.Fetcher = feedFetcher(func(context.Context, netguard.Request) (*netguard.Response, error) {
		return &netguard.Response{Status: 304}, nil
	})
	res, err = h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil {
		t.Fatal(res, err)
	}
	if r := fields(t, res); r["show_path"] != "fixture show" {
		t.Fatal(r)
	}
}

// Two show folders whose names were swapped by hand, holding episodes of the
// same file name, each with an interrupted download: a record is named by its
// show as well as its path (recordName), so taking each folder in moves its
// show's record to the other name, and each download resumes there. Named by
// path alone, each record found the other's at its new path and stayed.
func TestSwappedShowsSharingEpisodeNamesKeepTheirDownloads(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f := h.read("Fixture Show")
	f.ShowID = "0123456789abcdef"
	for i := range f.Episodes {
		f.Episodes[i].Path = "Other Show/" + filepath.Base(f.Episodes[i].Path)
	}
	if err := root.Mkdir("Other Show", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := podcast.Write(root, "Other Show", f, s.o.Now()); err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(*job.Path)
	mine := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: fixtureShowID, EpisodeID: "1cc306d831645e2b", Path: "Fixture Show/" + base, Temp: podcast.TempPrefix + nonce(), Offset: 6, Total: 100}
	theirs := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: f.ShowID, EpisodeID: "1cc306d831645e2b", Path: "Other Show/" + base, Temp: podcast.TempPrefix + nonce(), Offset: 6, Total: 100}
	for _, c := range []*checkpoint{&mine, &theirs} {
		if err := root.WriteFile(c.partialName(), []byte("prefix"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := c.Path
		if err := s.saveCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &at}, root, c, `"one"`); err != nil {
			t.Fatal(err)
		}
	}
	renameShow(t, h, "Fixture Show", "Swapping")
	renameShow(t, h, "Other Show", "Fixture Show")
	renameShow(t, h, "Swapping", "Other Show")
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	for _, show := range []string{"Fixture Show", "Other Show"} {
		if _, err := s.readShow(context.Background(), root, lib, h.mount, show); err != nil {
			t.Fatal(show, err)
		}
	}
	for _, c := range []struct {
		c    checkpoint
		show string
	}{{mine, "Other Show"}, {theirs, "Fixture Show"}} {
		at := c.show + "/" + base
		resumed, tag := s.loadCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &at}, root, c.c.ShowID, c.c.EpisodeID)
		if tag != `"one"` || resumed.Temp != c.c.Temp || resumed.Offset != 6 {
			t.Fatal("a show lost its download in the swap", c.c.ShowID, resumed, tag)
		}
	}
}

// A record an older Bridge named by library and path alone is renamed by its
// show once at the first read of the records, so the interrupted download
// resumes after the upgrade.
func TestARecordNamedByPathAloneIsMigrated(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: fixtureShowID, EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: podcast.TempPrefix + nonce(), Offset: 6, Total: 100, Validator: secrets.PodcastETagPrefix + nonce()}
	if err := root.WriteFile(c.partialName(), []byte("prefix"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.o.Vault.Set(c.Validator, `"one"`); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.o.DataDir, "podcast-fetch", s.pairHash())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(dir, legacyRecordName(3, c.Path)), b, 0o600); err != nil {
		t.Fatal(err)
	}
	resumed, tag := s.loadCheckpoint(job, root, c.ShowID, c.EpisodeID)
	if tag != `"one"` || resumed.Temp != c.Temp {
		t.Fatal("the record was not migrated", resumed, tag)
	}
	if _, err := os.Lstat(filepath.Join(dir, legacyRecordName(3, c.Path))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old name was left", err)
	}
}

// A copy of the show folder made over it in place (cp into the same folder)
// while its episode downloads rewrites the partial under the file the fetch
// holds open: the same file (no check by path sees it), the same size once
// the download ends, other bytes. The fetch hashes the partial again before
// it probes and publishes it and, finding it changed, downloads the episode
// again from byte 0, publishing the episode's own bytes, never the copy's.
func TestAFetchDownloadsAgainAPartialOverwrittenInPlace(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	audio := data(t, "chap-v24-reverse.mp3")
	show := filepath.Join(h.mount, "Fixture Show")
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		if calls.Add(1) > 1 {
			w.Write(audio)
			return
		}
		w.Write(audio[:1000])
		w.(http.Flusher).Flush()
		if !partialAt(show, 1000) {
			t.Error("the download did not start")
			return
		}
		left := partials(show)
		f, err := os.OpenFile(left[0], os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			t.Error(err)
			return
		}
		f.Write(bytes.Repeat([]byte{0x55}, 1000))
		f.Close()
		w.Write(audio[1000:])
	})
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if got, ok := res.(fetchResult); err != nil || !ok || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 {
		t.Fatal(res, err, calls.Load())
	}
	if saved, err := os.ReadFile(filepath.Join(h.mount, *job.Path)); err != nil || !bytes.Equal(saved, audio) {
		t.Fatal("the published episode is not the host's", err)
	}
	if left := partials(show); len(left) != 0 {
		t.Fatal("a partial was left behind", left)
	}
	noRecords(t, h.handler.s)
}

// A show folder deleted and put back from a copy between two host attempts
// (the host cut the download; the copy holds the partial as it was and a copy
// of the lock file, not the file held): the retry goes on from the partial
// there, holding the lock there, taken anew, rather than append under a
// copied lock it does not hold beside another download of the episode.
func TestAFetchRetakesItsLockInAFolderPutBackBetweenAttempts(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	audio := data(t, "chap-v24-reverse.mp3")
	lockFile := filepath.Join(h.mount, podcast.AttachmentPath(*job.Path, ".fetch.lock"))
	show := filepath.Join(h.mount, "Fixture Show")
	var put atomic.Bool
	s.o.Wait = func(context.Context, time.Duration) error {
		// The host's retry wait: the folder is put back from a copy once.
		if put.CompareAndSwap(false, true) {
			if err := copyThenDelete(t, show, show); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	resume := resumeAt(t, audio)
	var calls atomic.Int32
	cutAfterMove(t, h, job, audio, &calls, func(string, string) error { return nil }, func(w http.ResponseWriter, r *http.Request) {
		f, err := os.OpenFile(lockFile, os.O_RDWR, 0)
		if err != nil {
			t.Error("no lock file in the folder put back", err)
		} else {
			if free, err := tryLock(f); err != nil || free {
				t.Error("the retry appended under a copied lock it does not hold", err)
			}
			f.Close()
		}
		resume(w, r)
	})
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if got, ok := res.(fetchResult); err != nil || !ok || got.Path != *job.Path || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 || !put.Load() {
		t.Fatal(res, err, calls.Load(), put.Load())
	}
	if _, err := os.Lstat(lockFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the episode's lock was left", err)
	}
	if left := partials(show); len(left) != 0 {
		t.Fatal("a partial was left behind", left)
	}
	noRecords(t, s)
}

// A re-take of the episode's lock (followLock) at the path it holds already,
// with the show folder's .audiosilo removed meanwhile (an rm -rf of the show
// folder under way), makes nothing there: neither the folder (the rm -rf
// would then fail to remove the show folder) nor the lock file. One whose
// .audiosilo is there but whose lock file went makes no file either; at a
// new path the lock is taken, the file made in a .audiosilo there.
func TestALockRetakeNeverMakesTheFolder(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx := context.Background()
	for _, gone := range []string{"Fixture Show/.audiosilo", podcast.AttachmentPath(*job.Path, ".fetch.lock")} {
		l, err := s.lockEpisode(ctx, root, *job.Path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(h.mount, gone)); err != nil {
			t.Fatal(err)
		}
		if err := s.followLock(ctx, l, *job.Path); err != nil {
			t.Fatal(gone, err)
		}
		if _, err := root.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the re-take made", gone, err)
		}
		l.release()
	}
	l, err := s.lockEpisode(ctx, root, *job.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.release()
	if err := os.MkdirAll(filepath.Join(h.mount, "Moved Show", ".audiosilo"), 0o755); err != nil {
		t.Fatal(err)
	}
	// At a new path with no .audiosilo folder the follow fails, making
	// nothing there and keeping the lock it holds, rather than go on there
	// holding none.
	bare := "Bare Show/" + path.Base(*job.Path)
	if err := os.Mkdir(filepath.Join(h.mount, "Bare Show"), 0o755); err != nil {
		t.Fatal(err)
	}
	var failure *netguard.Error
	if err := s.followLock(ctx, l, bare); !errors.As(err, &failure) || failure.Code != "write_failed" || !l.held() || l.p != podcast.AttachmentPath(*job.Path, ".fetch.lock") {
		t.Fatal("a follow to a path with nothing to lock went on", err)
	}
	if _, err := root.Lstat("Bare Show/.audiosilo"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the follow made the .audiosilo folder", err)
	}
	moved := "Moved Show/" + path.Base(*job.Path)
	if err := s.followLock(ctx, l, moved); err != nil || !l.held() || l.p != podcast.AttachmentPath(moved, ".fetch.lock") {
		t.Fatal("the lock was not taken at the new path", err)
	}
}

// A show folder renamed by hand only in case on a case-insensitive
// filesystem, its cache row naming the old spelling (as the follow left it):
// the row names this very folder, not another holding the show, so a poll
// naming the old spelling takes the folder in under its spelling on disk
// rather than refuse it as a copy for good. A running fetch naming the old
// spelling finds its episode there too (episodeAt), never reading on under
// the old one, whose show lock no other job takes.
func TestACaseOnlyRenameWithACacheRowIsTakenIn(t *testing.T) {
	h := newHarness(t)
	show, id := followed(t, h)
	lower := strings.ToLower(show)
	renameShow(t, h, show, lower)
	if _, err := os.Lstat(filepath.Join(h.mount, show)); err != nil {
		t.Skip("the filesystem is case-sensitive")
	}
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f := h.read(show)
	episode := f.Episodes[0]
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	want := lower + "/" + path.Base(episode.Path)
	if got, err := h.handler.s.episodeAt(context.Background(), root, lib, h.mount, episode.Path, fetchParams{id, episode.ID}, 0); err != nil || got != want {
		t.Fatal("the fetch stayed under the old spelling", got, err)
	}
	if f := h.read(lower); !strings.HasPrefix(f.Episodes[0].Path, lower+"/") {
		t.Fatal("feed.json still names the old spelling", f.Episodes[0].Path)
	}
	params, _ := json.Marshal(map[string]string{"show_id": id, "etag": `"abc"`})
	poll := jobs.Job{ID: 20, Kind: PollKind, LibraryID: &lib.ID, Path: &show, Params: params}
	res, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil || fields(t, res)["show_path"] != lower {
		t.Fatal(res, err)
	}
}

// A show folder whose feed.json cannot be read, its show held by no other
// folder: the first job there searches the library for the show, and the
// next ones within a minute fail the same way without searching again (a
// fetch per episode would each read every show's feed.json).
func TestAnUnreadableShowIsSearchedForOnce(t *testing.T) {
	h, job := audioHarness(t)
	if err := os.WriteFile(filepath.Join(h.mount, "Fixture Show", "feed.json"), []byte("not a feed"), 0o644); err != nil {
		t.Fatal(err)
	}
	searches := 0
	searchHook = func() { searches++ }
	t.Cleanup(func() { searchHook = nil })
	show := "Fixture Show"
	params, _ := json.Marshal(map[string]string{"show_id": fixtureShowID})
	for i := range 3 {
		poll := jobs.Job{ID: int64(40 + i), Kind: PollKind, LibraryID: job.LibraryID, Path: &show, Params: params}
		res, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
		var failure *netguard.Error
		if !errors.As(err, &failure) || failure.Code != "write_failed" {
			t.Fatal(i, res, err)
		}
	}
	if searches != 1 {
		t.Fatal("library searches:", searches)
	}
}

// Abandon clears every download record of the episode: one moved with its
// show folder renamed by hand, and one still at the job's path beside it (a
// folder put back there), not only the first it finds.
func TestAbandonClearsBothRecordsOfTheEpisode(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.CopyFS(filepath.Join(h.mount, "Renamed Show"), os.DirFS(filepath.Join(h.mount, "Fixture Show"))); err != nil {
		t.Fatal(err)
	}
	moved := "Renamed Show/" + path.Base(*job.Path)
	var temps []string
	for _, at := range []string{*job.Path, moved} {
		c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: fixtureShowID, EpisodeID: "1cc306d831645e2b", Path: at, Temp: podcast.TempPrefix + nonce(), Offset: 6, Total: 100}
		if err := root.WriteFile(c.partialName(), []byte("prefix"), 0o600); err != nil {
			t.Fatal(err)
		}
		p := at
		if err := s.saveCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &p}, root, &c, `"one"`); err != nil {
			t.Fatal(err)
		}
		temps = append(temps, c.partialName())
	}
	h.handler.FetchJob().Abandon(context.Background(), job)
	noRecords(t, s)
	for _, p := range temps {
		if _, err := root.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a partial was left", p, err)
		}
	}
}

// renamingVault renames the show folder by hand at the first read of the
// pairing's token (the fetch's first, after it took the episode's lock).
type renamingVault struct {
	secrets.Store
	once sync.Once
	move func()
}

func (v *renamingVault) Get(name string) (string, error) {
	if name == secrets.ServerToken {
		v.once.Do(v.move)
	}
	return v.Store.Get(name)
}

// A show folder renamed by hand just after a fetch took the episode's lock,
// the fetch then failing before its download (the folder gone at the
// snapshot): the lock file moved with the folder, and the fetch lets it go
// there, its file removed, rather than leave it in the renamed folder.
func TestAFetchFailingEarlyLetsGoOfItsLockWhereTheShowIsNow(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	s.o.Vault = &renamingVault{Store: s.o.Vault, move: func() { renameShow(t, h, "Fixture Show", "Renamed Show") }}
	if res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter()); err == nil {
		t.Fatal("the fetch did not fail", res)
	}
	if _, err := os.Lstat(filepath.Join(h.mount, "Renamed Show")); err != nil {
		t.Fatal("the folder was not renamed", err)
	}
	lockFile := filepath.Join(h.mount, podcast.AttachmentPath("Renamed Show/"+path.Base(*job.Path), ".fetch.lock"))
	if _, err := os.Lstat(lockFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the episode's lock was left in the renamed folder", err)
	}
}

// A download read back before its probe (the hash that catches a partial
// overwritten in place) whose read fails, as a network share's does for a
// moment, is not a changed one: it is read again, and published from the
// bytes it holds. One request, never the whole episode again from byte 0.
func TestAFetchReadsItsDownloadAgainAfterAFailedRead(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	audio := data(t, "chap-v24-reverse.mp3")
	var calls, reads atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		w.Write(audio)
	})
	s.o.Fault = func(at string) error {
		if at == "rehash" && reads.Add(1) <= 2 {
			return syscall.EIO
		}
		return nil
	}
	var waits []time.Duration
	s.o.Wait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if got, ok := res.(fetchResult); err != nil || !ok || got.Fetch.Bytes != int64(len(audio)) {
		t.Fatal(res, err)
	}
	if calls.Load() != 1 || reads.Load() != 3 {
		t.Fatal("a read that failed cost the download:", calls.Load(), reads.Load())
	}
	if !slices.Equal(waits, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatal("waits:", waits)
	}
	if saved, err := os.ReadFile(filepath.Join(h.mount, *job.Path)); err != nil || !bytes.Equal(saved, audio) {
		t.Fatal("the published episode is not the host's", err)
	}
	noRecords(t, s)
}

// A download that cannot be read back at all is never taken for a changed
// one either: the fetch fails write_failed after its bounded reads, with no
// second request for the episode.
func TestAFetchNeverDownloadsAgainADownloadItCannotRead(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	audio := data(t, "chap-v24-reverse.mp3")
	var calls, reads atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		w.Write(audio)
	})
	s.o.Fault = func(at string) error {
		if at == "rehash" {
			reads.Add(1)
			return syscall.EIO
		}
		return nil
	}
	_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	var failure *netguard.Error
	if !errors.As(err, &failure) || failure.Code != "write_failed" {
		t.Fatal(err)
	}
	if calls.Load() != 1 || reads.Load() != rehashTries {
		t.Fatal("requests and reads:", calls.Load(), reads.Load())
	}
	if _, err := os.Lstat(filepath.Join(h.mount, *job.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an episode was published unread", err)
	}
}

// A show folder renamed by hand between the fetch's last follow and the read
// of its download leaves no partial at the path read: not a changed download.
// The fetch follows the show and reads the partial there, publishing it from
// the one request.
func TestAFetchFollowsAShowRenamedWhileItsDownloadIsReadBack(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
		t.Fatal(err)
	}
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	audio := data(t, "chap-v24-reverse.mp3")
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		w.Write(audio)
	})
	var once sync.Once
	s.o.Fault = func(at string) error {
		if at == "rehash" {
			once.Do(func() { renameShow(t, h, "Fixture Show", "Renamed Show") })
		}
		return nil
	}
	res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	moved := "Renamed Show/" + path.Base(*job.Path)
	got, ok := res.(fetchResult)
	if err != nil || !ok || got.Path != moved || got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 1 {
		t.Fatal(res, err, calls.Load())
	}
	if saved, err := os.ReadFile(filepath.Join(h.mount, moved)); err != nil || !bytes.Equal(saved, audio) {
		t.Fatal("the episode was not published in the renamed folder", err)
	}
	if left := partials(filepath.Join(h.mount, "Renamed Show")); len(left) != 0 {
		t.Fatal("a partial was left behind", left)
	}
	noRecords(t, s)
}
