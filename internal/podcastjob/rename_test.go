package podcastjob

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
			if got, err := s.episodeAt(context.Background(), root, lib, h.mount, *job.Path, params, job.ID); err != nil || got != moved {
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
		if saved, ok := s.readCheckpoint(job); !ok || saved.ShowID != theirs.ShowID || saved.Temp != theirs.Temp {
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
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &middle}); !ok {
		t.Fatal("the record did not move to the middle name")
	}
	renameShow(t, h, "Middle Name", "Third Name")
	if _, err := s.readShow(context.Background(), root, lib, h.mount, "Third Name"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &middle}); ok {
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
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &moved}); ok {
		t.Fatal("the moved record was left for the day-old cleanup")
	}
	if _, err := root.Lstat("Renamed Show/" + mine.Temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the moved partial was left behind", err)
	}
	if v, _ := s.o.Vault.Get(mine.Validator); v != "" {
		t.Fatal("the moved record's validator was left behind")
	}
	otherMoved := "Renamed Show/other.mp3"
	if saved, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &otherMoved}); !ok || saved.Temp != theirs.Temp {
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
	if _, ok := s.readCheckpoint(job); !ok {
		t.Fatal("the take-in moved a running fetch's record")
	}
	h.handler.FetchJob().Abandon(context.Background(), job)
	if _, ok := s.readCheckpoint(job); ok {
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
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &relocated}); !ok {
		t.Fatal("a running fetch's record was moved")
	}
	if err := h.db.FinishServerJob(context.Background(), job.ID, 1, store.JobFailed, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.adoptCheckpoints(context.Background(), root, lib, "Third Name", "Renamed Show", c.ShowID, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.readCheckpoint(jobs.Job{LibraryID: job.LibraryID, Path: &third}); !ok {
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
	written := func(n int64) bool {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			matches := partials(filepath.Join(h.mount, "Fixture Show"))
			if len(matches) == 1 {
				if st, err := os.Stat(matches[0]); err == nil && st.Size() >= n {
					return true
				}
			}
		}
		return false
	}
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
		if _, ok := s.readCheckpoint(job); !ok {
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
