package podcastjob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
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

func TestOwnedCleanupHonorsAgeLiveJobAndForeignFiles(t *testing.T) {
	for _, scenario := range []string{"old", "fresh", "live", "relocated", "foreign", "identity"} {
		t.Run(scenario, func(t *testing.T) {
			h, job := audioHarness(t)
			s := h.handler.s
			root, _ := os.OpenRoot(h.mount)
			defer root.Close()
			age := s.o.Now().Add(-25 * time.Hour)
			c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-owned", Offset: 6, Total: 100}
			root.WriteFile("Fixture Show/"+c.Temp, []byte("prefix"), 0600)
			root.WriteFile("Fixture Show/.audiosilo-tmp-foreign", []byte("keep"), 0600)
			if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
				t.Fatal(err)
			}
			if scenario == "fresh" {
				age = s.o.Now()
			}
			c.UpdatedAt = age.Format(time.RFC3339)
			if scenario == "identity" {
				c.ShowID = strings.Repeat("1", 16)
			}
			dir, _ := s.checkpointRoot()
			b, _ := json.Marshal(c)
			dir.WriteFile(s.checkpointName(job, c.ShowID), b, 0600)
			dir.Close()
			if scenario == "live" {
				if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Attempt: 1, ServerID: "server-1"}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "relocated" {
				// A fetch claimed under the folder's old name that found the show
				// here (Fetch's relocation): its row names a path the record does not.
				claimed := "Old Name/" + path.Base(*job.Path)
				if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: &claimed, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "foreign" {
				root.Remove("Fixture Show/" + c.Temp)
				os.Symlink(".audiosilo-tmp-foreign", root.Name()+"/Fixture Show/"+c.Temp)
			}
			// The calling retain job is itself running at the show's path; its own
			// row must not count as a live writer, or cleanup never runs.
			show := "Fixture Show"
			if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: 82, Kind: RetainKind, LibraryID: job.LibraryID, Path: &show, Attempt: 1, ServerID: "server-1"}); err != nil {
				t.Fatal(err)
			}
			s.cleanOwned(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, show, jobs.Job{ID: 82})
			_, err := root.Lstat("Fixture Show/" + c.Temp)
			if scenario == "old" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("old owned partial retained", err)
				}
			} else if err != nil {
				t.Fatal("protected partial removed", err)
			}
			if b, _ := root.ReadFile("Fixture Show/.audiosilo-tmp-foreign"); string(b) != "keep" {
				t.Fatal("foreign temp touched")
			}
		})
	}
}
func TestFileLockExclusiveAcrossHandlersAndCancellation(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	s.o.Wait = netguard.Wait
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	release, err := s.fileLock(context.Background(), root, *job.Path)
	if err != nil {
		t.Fatal(err)
	}
	other := New(s.o).s
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := other.fileLock(ctx, root, *job.Path); err == nil {
		t.Fatal("overlapping writer admitted")
	}
	release()
	release, err = other.fileLock(context.Background(), root, *job.Path)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
func TestFileLockRemovesItsFileAndStaysExclusive(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	s.o.Wait = netguard.Wait
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	lockFile := podcast.AttachmentPath(*job.Path, ".fetch.lock")
	release, err := s.fileLock(context.Background(), root, *job.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(lockFile); err != nil {
		t.Fatal("no lock file while held", err)
	}
	release()
	if _, err := root.Lstat(lockFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the lock file outlived its release", err)
	}
	// Jobs queued on the file the holder removes still take turns: one at a
	// time, and the last one leaves nothing behind.
	var mu sync.Mutex
	holders, most := 0, 0
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := New(s.o).s
			for range 15 {
				release, err := other.fileLock(context.Background(), root, *job.Path)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				holders++
				most = max(most, holders)
				mu.Unlock()
				time.Sleep(200 * time.Microsecond)
				mu.Lock()
				holders--
				mu.Unlock()
				release()
			}
		}()
	}
	wg.Wait()
	if most != 1 {
		t.Fatalf("%d jobs held the episode lock at once", most)
	}
	if _, err := root.Lstat(lockFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a lock file was left behind", err)
	}
}
func TestLostIdentityAbandonRemovesOnlyOwnedPartial(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-owned", Offset: 6, Total: 100}
	root.WriteFile(path.Dir(*job.Path)+"/"+c.Temp, []byte("prefix"), 0600)
	if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
		t.Fatal(err)
	}
	f := h.read("Fixture Show")
	f.ShowID = strings.Repeat("1", 16)
	podcast.Write(root, "Fixture Show", f, s.o.Now())
	if _, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter()); err == nil {
		t.Fatal("stale identity accepted")
	}
	if _, err := root.Lstat(path.Dir(*job.Path) + "/" + c.Temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("terminal partial retained", err)
	}
}

func TestOldCommittedTrashCleanupPreservesTombstone(t *testing.T) {
	h, _ := audioHarness(t)
	s := h.handler.s
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	var j podcast.Journal
	if err := json.Unmarshal(data(t, "publish-v1.json"), &j); err != nil {
		t.Fatal(err)
	}
	j.Fetch.FetchedAt = s.o.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	j.Chapters.GeneratedAt = j.Fetch.FetchedAt
	root.WriteFile("Fixture Show/"+j.AudioTemp, data(t, "chap-v24-reverse.mp3"), 0600)
	if err := podcast.Publish(root, nil, j, s.o.Now(), nil); err != nil {
		t.Fatal(err)
	}
	j.Op = "delete"
	j.AudioTemp = ""
	j.Chapters = nil
	j.Fetch.DeletedAt = s.o.Now().Add(-25 * time.Hour).Format(time.RFC3339)
	j.TrashDir = ".audiosilo-trash/podcasts/" + j.EpisodeID + "-g2-owned"
	if err := podcast.Publish(root, nil, j, s.o.Now(), nil); err != nil {
		t.Fatal(err)
	}
	show, lib := "Fixture Show", int64(3)
	if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: 82, Kind: RetainKind, LibraryID: &lib, Path: &show, Attempt: 1, ServerID: "server-1"}); err != nil {
		t.Fatal(err)
	}
	s.cleanOwned(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, show, jobs.Job{ID: 82})
	if _, err := root.Lstat("Fixture Show/" + j.TrashDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed trash retained", err)
	}
	if f := h.read("Fixture Show"); f.Episodes[0].Fetch.DeletedAt != j.Fetch.DeletedAt {
		t.Fatal("cleanup changed tombstone")
	}
}

// A record left by an identity the feed no longer has is ours to remove; the
// next save would otherwise overwrite it and orphan its partial for good.
func TestStaleIdentityCheckpointIsDropped(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: strings.Repeat("2", 16), Path: *job.Path, Temp: ".audiosilo-tmp-stale", Offset: 6, Total: 100}
	root.WriteFile("Fixture Show/"+c.Temp, []byte("prefix"), 0600)
	if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
		t.Fatal(err)
	}
	validator := c.Validator
	if err := s.dropStaleCheckpoint(job, root, "a1b2c3d4e5f60718", "1cc306d831645e2b"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat("Fixture Show/" + c.Temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale partial orphaned", err)
	}
	if v, _ := s.o.Vault.Get(validator); v != "" {
		t.Fatal("stale validator kept")
	}
	// The current identity's record is kept.
	c = checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-current", Offset: 6, Total: 100}
	root.WriteFile("Fixture Show/"+c.Temp, []byte("prefix"), 0600)
	s.saveCheckpoint(job, root, &c, `"one"`)
	if err := s.dropStaleCheckpoint(job, root, "a1b2c3d4e5f60718", "1cc306d831645e2b"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat("Fixture Show/" + c.Temp); err != nil {
		t.Fatal("current partial removed", err)
	}
}

// A proven partial swapped for a link after its check is never appended to:
// like one of another size, one gone or one barred to the Bridge (EACCES,
// EPERM: lasting faults), it is proven changed (errPartialChanged), which
// restarts the episode. Only a partial that could not be opened to tell (an
// I/O error) is errPartialUnreadable, kept.
func TestOpenPartialRefusesSwappedPartial(t *testing.T) {
	dir := t.TempDir()
	root, _ := os.OpenRoot(dir)
	defer root.Close()
	root.WriteFile("victim", []byte("keep"), 0644)
	root.WriteFile("partial", []byte("prefix"), 0600)
	f, err := openPartial(root, "partial", 6)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := openPartial(root, "partial", 5); !errors.Is(err, errPartialChanged) {
		t.Fatal("size check", err)
	}
	if os.Geteuid() != 0 {
		os.Chmod(filepath.Join(dir, "partial"), 0)
		if _, err := openPartial(root, "partial", 6); !errors.Is(err, errPartialChanged) {
			t.Fatal("a partial barred to the Bridge not taken for a lost one", err)
		}
		os.Chmod(filepath.Join(dir, "partial"), 0o600)
	}
	for _, lasting := range []error{syscall.EACCES, syscall.EPERM} {
		if err := partialError(&os.PathError{Op: "open", Path: "partial", Err: lasting}); !errors.Is(err, errPartialChanged) {
			t.Fatal("a lasting fault kept the partial", lasting, err)
		}
	}
	if err := partialError(&os.PathError{Op: "open", Path: "show/partial", Err: syscall.ENOTDIR}); !errors.Is(err, errPartialChanged) {
		t.Fatal("a partial whose show folder is a file now kept", err)
	}
	if err := partialError(&os.PathError{Op: "read", Path: "partial", Err: syscall.EIO}); !errors.Is(err, errPartialUnreadable) || errors.Is(err, errPartialChanged) {
		t.Fatal("a partial that could not be read taken for a changed one", err)
	}
	root.Remove("partial")
	if _, err := openPartial(root, "partial", 6); !errors.Is(err, errPartialChanged) {
		t.Fatal("a partial gone", err)
	}
	os.Symlink("victim", filepath.Join(dir, "partial"))
	if _, err := openPartial(root, "partial", 4); !errors.Is(err, errPartialChanged) {
		t.Fatal("link target accepted as the partial", err)
	}
}

// A partial the Bridge could not remove keeps its record and validator, so
// cleanOwned can find it again; once removable, the clear takes all three.
func TestClearCheckpointKeepsTheRecordOfAPartialItCouldNotRemove(t *testing.T) {
	requireNonRoot(t)
	// 0o500: the partial cannot be removed. 0o600: it cannot even be checked
	// (a share's hiccup), which proves nothing gone (partialGone).
	for name, mode := range map[string]os.FileMode{"cannot remove": 0o500, "cannot check": 0o600} {
		t.Run(name, func(t *testing.T) {
			h, job := audioHarness(t)
			s := h.handler.s
			root, _ := os.OpenRoot(h.mount)
			defer root.Close()
			c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-stuck", Offset: 6, Total: 100}
			partial := "Fixture Show/" + c.Temp
			root.WriteFile(partial, []byte("prefix"), 0600)
			if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
				t.Fatal(err)
			}
			show := filepath.Join(h.mount, "Fixture Show")
			if err := os.Chmod(show, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(show, 0o755) })
			stuck := c
			s.clearCheckpoint(job, root, &stuck)
			if saved, ok := s.readCheckpoint(job, fixtureShowID); !ok || saved.Temp != c.Temp {
				t.Fatal("the record of a partial still on disk was dropped")
			}
			if v, _ := s.o.Vault.Get(c.Validator); v == "" {
				t.Fatal("the validator of a partial still on disk was dropped")
			}
			os.Chmod(show, 0o755)
			s.clearCheckpoint(job, root, &c)
			if _, ok := s.readCheckpoint(job, fixtureShowID); ok {
				t.Fatal("record kept once the partial was removed")
			}
			if _, err := root.Lstat(partial); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial retained", err)
			}
		})
	}
}

// A record naming an earlier partial still on disk is not overwritten by a new
// download's checkpoint: it would orphan that partial. Once that partial can be
// removed, the next save takes it and records the new download.
func TestSaveCheckpointKeepsTheRecordOfAnEarlierPartial(t *testing.T) {
	requireNonRoot(t)
	h, job := audioHarness(t)
	s := h.handler.s
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	old := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-stuck", Offset: 6, Total: 100}
	partial := "Fixture Show/" + old.Temp
	root.WriteFile(partial, []byte("prefix"), 0600)
	if err := s.saveCheckpoint(job, root, &old, `"one"`); err != nil {
		t.Fatal(err)
	}
	show := filepath.Join(h.mount, "Fixture Show")
	if err := os.Chmod(show, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(show, 0o755) })
	next := old
	next.Temp, next.Validator = ".audiosilo-tmp-next", ""
	if err := s.saveCheckpoint(job, root, &next, `"two"`); err != nil {
		t.Fatal(err)
	}
	if saved, ok := s.readCheckpoint(job, fixtureShowID); !ok || saved.Temp != old.Temp || saved.Validator != old.Validator {
		t.Fatalf("the earlier record was overwritten: %+v", saved)
	}
	if next.Validator != "" {
		t.Fatal("a validator was sealed for an unrecorded download")
	}
	os.Chmod(show, 0o755)
	if err := s.saveCheckpoint(job, root, &next, `"two"`); err != nil {
		t.Fatal(err)
	}
	if saved, ok := s.readCheckpoint(job, fixtureShowID); !ok || saved.Temp != next.Temp {
		t.Fatalf("the new download stayed unrecorded once the earlier partial was removable: %+v", saved)
	}
	if _, err := root.Lstat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the earlier partial was not removed", err)
	}
	if v, _ := s.o.Vault.Get(old.Validator); v != "" {
		t.Fatal("the earlier validator was kept")
	}
}

// A record left under a folder that is gone (a fetch that ended early after
// its show folder was renamed by hand) or that holds another show now is read
// by no job: cleanOwned goes by a show's folder. The day-old sweep drops it
// and its validator, leaving its partial to sweepPartials. One a day old at
// its own show's folder (cleanOwned's), a fresh one, one whose download runs
// and one whose folder cannot be read stay.
func TestSweepRecordsDropsRecordsWhoseFolderIsGone(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir("Unreadable Show", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("Unreadable Show/feed.json", []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := path.Base(*job.Path)
	old := s.o.Now().Add(-25 * time.Hour)
	type record struct {
		c     checkpoint
		fresh bool
		run   bool
		swept bool
	}
	records := map[string]*record{
		"gone":       {c: checkpoint{ShowID: fixtureShowID, Path: "Gone Show/" + base}, swept: true},
		"other show": {c: checkpoint{ShowID: "0123456789abcdef", Path: "Fixture Show/" + base}, swept: true},
		"own folder": {c: checkpoint{ShowID: fixtureShowID, Path: "Fixture Show/" + base}},
		"fresh":      {c: checkpoint{ShowID: fixtureShowID, Path: "Fresh Gone/" + base}, fresh: true},
		"running":    {c: checkpoint{ShowID: fixtureShowID, Path: "Running Gone/" + base}, run: true},
		"unreadable": {c: checkpoint{ShowID: fixtureShowID, Path: "Unreadable Show/" + base}},
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for name, r := range records {
		// The running fetch's episode (its row's params) is the running one's
		// alone: a record of that episode counts as its download wherever.
		r.c.ServerHash, r.c.LibraryID, r.c.EpisodeID = s.pairHash(), 3, "2cc306d831645e2b"
		if r.run {
			r.c.EpisodeID = "1cc306d831645e2b"
		}
		r.c.Temp, r.c.Validator, r.c.Offset, r.c.Total = podcast.TempPrefix+nonce(), secrets.PodcastETagPrefix+nonce(), 6, 100
		r.c.UpdatedAt = old.Format(time.RFC3339)
		if r.fresh {
			r.c.UpdatedAt = s.o.Now().Format(time.RFC3339)
		}
		if err := s.o.Vault.Set(r.c.Validator, `"one"`); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(r.c)
		if err := dir.WriteFile(recordName(3, r.c.ShowID, r.c.Path), b, 0o600); err != nil {
			t.Fatal(name, err)
		}
		if r.run {
			p := r.c.Path
			if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: 99, Kind: FetchKind, LibraryID: job.LibraryID, Path: &p, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.sweepRecords(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, nil)
	for name, r := range records {
		_, err := dir.Lstat(recordName(3, r.c.ShowID, r.c.Path))
		v, _ := s.o.Vault.Get(r.c.Validator)
		if r.swept != errors.Is(err, os.ErrNotExist) || r.swept != (v == "") {
			t.Fatal(name, ": swept", r.swept, "record", err, "validator", v)
		}
	}
}

// A move that wrote a record at its new path and could not remove the old one
// leaves two records naming one validator (moveRecord). The sweep drops the
// one under the gone folder and keeps the validator the moved record still
// resumes by; alone, it goes with its record.
func TestSweepRecordsKeepsAValidatorAMovedRecordNames(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	dir, err := s.checkpointRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	left := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: fixtureShowID, EpisodeID: "2cc306d831645e2b", Path: "Gone Show/" + path.Base(*job.Path), Temp: podcast.TempPrefix + nonce(), Validator: secrets.PodcastETagPrefix + nonce(), Offset: 6, Total: 100, UpdatedAt: s.o.Now().Add(-25 * time.Hour).Format(time.RFC3339)}
	moved := left
	moved.Path = *job.Path
	if err := s.o.Vault.Set(left.Validator, `"one"`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []checkpoint{left, moved} {
		if err := writeRecord(dir, recordName(3, c.ShowID, c.Path), c); err != nil {
			t.Fatal(err)
		}
	}
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	sweep := func() (gone bool, validator string) {
		s.sweepRecords(context.Background(), root, lib, h.mount, []string{path.Dir(*job.Path)})
		_, err := dir.Lstat(recordName(3, left.ShowID, left.Path))
		v, _ := s.o.Vault.Get(left.Validator)
		return errors.Is(err, os.ErrNotExist), v
	}
	if gone, v := sweep(); !gone || v == "" {
		t.Fatal("the validator a moved record names went with the record left behind:", gone, v)
	}
	if _, ok := s.readOwnRecord(dir, 3, moved.ShowID, moved.Path); !ok {
		t.Fatal("the moved record was dropped")
	}
	// Left behind again, the moved record gone: nothing names the validator.
	if err := writeRecord(dir, recordName(3, left.ShowID, left.Path), left); err != nil {
		t.Fatal(err)
	}
	if err := dir.Remove(recordName(3, moved.ShowID, moved.Path)); err != nil {
		t.Fatal(err)
	}
	if gone, v := sweep(); !gone || v != "" {
		t.Fatal("a validator no record names was kept:", gone, v)
	}
}

// A record of a show whose folder was renamed by hand and not taken in yet
// is gone at its path but not orphaned: the show's take-in moves it, and the
// download resumes from the partial the folder took along. The sweep leaves
// it (and its partial recorded) until then, and drops it once the folder is
// taken in without it. Nothing is dropped while the library's marker is not
// there (a share unmounted since the mapping check).
func TestSweepRecordsLeavesARecordAwaitingItsTakeIn(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var f podcast.Feed
	if err := json.Unmarshal(data(t, "feed-fetch-v1.json"), &f); err != nil {
		t.Fatal(err)
	}
	const id = "fedcba9876543210"
	f.ShowID = id
	for i := range f.Episodes {
		f.Episodes[i].Path = "Away Old/" + path.Base(f.Episodes[i].Path)
	}
	if err := root.Mkdir("Away Old", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := podcast.Write(root, "Away Old", &f, s.o.Now()); err != nil {
		t.Fatal(err)
	}
	renameShow(t, h, "Away Old", "Away New")
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: id, EpisodeID: f.Episodes[0].ID, Path: f.Episodes[0].Path, Temp: podcast.TempPrefix + nonce(), Validator: secrets.PodcastETagPrefix + nonce(), Offset: 6, Total: 100, UpdatedAt: s.o.Now().Add(-25 * time.Hour).Format(time.RFC3339)}
	if err := root.WriteFile("Away New/"+c.Temp, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.o.Vault.Set(c.Validator, `"one"`); err != nil {
		t.Fatal(err)
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := writeRecord(dir, recordName(3, id, c.Path), c); err != nil {
		t.Fatal(err)
	}
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	// A copy of the partial in another show's folder, read first, never
	// answers for the folder awaiting its take-in.
	if err := root.WriteFile(path.Dir(*job.Path)+"/"+c.Temp, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	shows := []string{path.Dir(*job.Path), "Away New"}
	kept := func() bool {
		_, err := dir.Lstat(recordName(3, id, c.Path))
		v, _ := s.o.Vault.Get(c.Validator)
		return err == nil && v != ""
	}
	s.sweepRecords(context.Background(), root, lib, h.mount, shows)
	if !kept() {
		t.Fatal("the record of a show awaiting its take-in was dropped")
	}
	marker, err := root.ReadFile(".audiosilo/library.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(".audiosilo/library.json"); err != nil {
		t.Fatal(err)
	}
	// The folder taken in (its feed.json naming it), the record left behind.
	if err := podcast.Adopt(root, "Away New", "Away Old", &f, s.o.Now()); err != nil {
		t.Fatal(err)
	}
	s.sweepRecords(context.Background(), root, lib, h.mount, shows)
	if !kept() {
		t.Fatal("a record was dropped with the library's marker gone")
	}
	if err := root.WriteFile(".audiosilo/library.json", marker, 0o644); err != nil {
		t.Fatal(err)
	}
	s.sweepRecords(context.Background(), root, lib, h.mount, shows)
	if kept() {
		t.Fatal("the record a take-in left behind was kept")
	}
}

// A day-old record no job can resume or clear where it is goes with the
// hourly sweep, as an orphan's does, its partial left on disk unrecorded
// (sweepPartials' once old and removable) and a published episode beside it
// untouched: one under a show folder the Bridge may not search, one whose
// partial cannot be opened, a whole download never read back, and a partial
// of another size than its record's (a fetch's clear of it may keep failing
// where retain never runs). A day-old
// resumable record whose partial and folder read fine, and a fresh one under
// a barred folder, stay.
func TestSweepRecordsDropsDayOldRecordsNoJobCanClear(t *testing.T) {
	requireNonRoot(t)
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir("Barred Show", 0o755); err != nil {
		t.Fatal(err)
	}
	show := path.Dir(*job.Path)
	published := show + "/published.mp3"
	if err := root.WriteFile(published, []byte("episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	type record struct {
		c     checkpoint
		fresh bool
		swept bool
	}
	whole := sha256.Sum256([]byte("abcdef"))
	records := map[string]*record{
		"barred folder":  {c: checkpoint{Path: "Barred Show/one.mp3"}, swept: true},
		"barred partial": {c: checkpoint{Path: show + "/two.mp3"}, swept: true},
		"whole":          {c: checkpoint{Path: published, Offset: 6, SHA256: hex.EncodeToString(whole[:])}, swept: true},
		"wrong size":     {c: checkpoint{Path: show + "/six.mp3", Offset: 5, Total: 100}, swept: true},
		"resumable":      {c: checkpoint{Path: show + "/four.mp3"}},
		"fresh barred":   {c: checkpoint{Path: "Barred Show/five.mp3"}, fresh: true},
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	old := s.o.Now().Add(-25 * time.Hour)
	for name, r := range records {
		r.c.ServerHash, r.c.LibraryID, r.c.ShowID, r.c.EpisodeID = s.pairHash(), 3, fixtureShowID, "2cc306d831645e2b"
		r.c.Temp, r.c.Validator = podcast.TempPrefix+nonce(), secrets.PodcastETagPrefix+nonce()
		if r.c.Offset == 0 {
			r.c.Offset, r.c.Total = 6, 100
		} else if r.c.Total == 0 {
			r.c.Total = r.c.Offset
		}
		r.c.UpdatedAt = old.Format(time.RFC3339)
		if r.fresh {
			r.c.UpdatedAt = s.o.Now().Format(time.RFC3339)
		}
		if err := root.WriteFile(r.c.partialName(), []byte("abcdef"), 0o600); err != nil {
			t.Fatal(name, err)
		}
		if err := s.o.Vault.Set(r.c.Validator, `"one"`); err != nil {
			t.Fatal(err)
		}
		if err := writeRecord(dir, recordName(3, r.c.ShowID, r.c.Path), r.c); err != nil {
			t.Fatal(name, err)
		}
	}
	barredPartial := filepath.Join(h.mount, records["barred partial"].c.partialName())
	barred := filepath.Join(h.mount, "Barred Show")
	for p, mode := range map[string]os.FileMode{barredPartial: 0, barred: 0o600} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.Chmod(barred, 0o755) })
	s.sweepRecords(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, []string{show, "Barred Show"})
	os.Chmod(barred, 0o755)
	for name, r := range records {
		_, err := dir.Lstat(recordName(3, r.c.ShowID, r.c.Path))
		v, _ := s.o.Vault.Get(r.c.Validator)
		if r.swept != errors.Is(err, os.ErrNotExist) || r.swept != (v == "") {
			t.Fatal(name, ": swept", r.swept, "record", err, "validator", v)
		}
		if _, err := root.Lstat(r.c.partialName()); err != nil {
			t.Fatal(name, ": the partial was removed with its record", err)
		}
	}
	if b, err := root.ReadFile(published); err != nil || string(b) != "episode" {
		t.Fatal("a published episode was touched", err)
	}
}

// cleanOwned drops a day-old record whose partial it cannot remove with its
// validator, rather than keep it for good, and leaves the partial on disk,
// unrecorded. A show folder the Bridge may not write (its .audiosilo folder
// still writable for the episode's lock) stands in here for a partial a
// share's lock or a file flag keeps: retain's own write check refuses such a
// folder before it calls cleanOwned.
func TestOwnedCleanupDropsADayOldRecordWhosePartialItCannotRemove(t *testing.T) {
	requireNonRoot(t)
	h, job := audioHarness(t)
	s := h.handler.s
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	c := abandonRecord(t, s, root, job)
	partial := c.partialName()
	c.UpdatedAt = s.o.Now().Add(-25 * time.Hour).Format(time.RFC3339)
	dir, _ := s.checkpointRoot()
	defer dir.Close()
	if err := writeRecord(dir, s.checkpointName(job, c.ShowID), c); err != nil {
		t.Fatal(err)
	}
	if err := podcast.EnsureDir(root, "Fixture Show/.audiosilo"); err != nil {
		t.Fatal(err)
	}
	show := filepath.Join(h.mount, "Fixture Show")
	if err := os.Chmod(show, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(show, 0o755) })
	s.cleanOwned(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, "Fixture Show", jobs.Job{ID: 82})
	if _, ok := s.readCheckpoint(job, fixtureShowID); ok {
		t.Fatal("the day-old record of a partial that cannot be removed was kept")
	}
	if v, _ := s.o.Vault.Get(c.Validator); v != "" {
		t.Fatal("its validator was kept")
	}
	if _, err := root.Lstat(partial); err != nil {
		t.Fatal("the partial is gone", err)
	}
}

// A fetch claimed while the hourly sweep reads the library (after its first
// read of the open jobs) keeps the record it may have loaded already: the
// open jobs are read again just before a record goes, as the sweep takes no
// episode lock.
func TestSweepRecordsKeepsTheRecordOfAFetchClaimedMidSweep(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var p fetchParams
	if err := json.Unmarshal(job.Params, &p); err != nil {
		t.Fatal(err)
	}
	whole := sha256.Sum256([]byte("abcdef"))
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: p.ShowID, EpisodeID: p.EpisodeID, Path: *job.Path, Temp: podcast.TempPrefix + nonce(), Validator: secrets.PodcastETagPrefix + nonce(), Offset: 6, Total: 6, SHA256: hex.EncodeToString(whole[:]), UpdatedAt: s.o.Now().Add(-25 * time.Hour).Format(time.RFC3339)}
	if err := root.WriteFile(c.partialName(), []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	name := recordName(3, c.ShowID, c.Path)
	if err := writeRecord(dir, name, c); err != nil {
		t.Fatal(err)
	}
	// The sweep asks the library's mount once as it starts and again before
	// each drop: the fetch is claimed in between.
	mount, asked := s.o.Mount, 0
	s.o.Mount = func(id int64, r string) (string, bool) {
		if asked++; asked == 2 {
			if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Params: job.Params, Attempt: 1, ServerID: "server-1"}); err != nil {
				t.Error(err)
			}
		}
		return mount(id, r)
	}
	s.sweepRecords(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, []string{path.Dir(c.Path)})
	if asked < 2 {
		t.Fatal("the sweep never came to drop the record", asked)
	}
	if now, err := readRecord(dir, name); err != nil || now != c {
		t.Fatal("the record of a fetch claimed mid-sweep was dropped", err)
	}
}

// RealName lists the library's own folder: its failure, a permission refused
// included, says nothing of the show folder, and no record outlives its jobs
// for it (a share going away mid-sweep would otherwise cost every day-old
// record of the library its resume).
func TestOutlivedIgnoresTheLibraryFoldersOwnFailure(t *testing.T) {
	c := checkpoint{Path: "Show/one.mp3", Temp: podcast.TempPrefix + nonce(), Offset: 6, Total: 100}
	read := func(string) (*podcast.Feed, string, error) {
		t.Fatal("the show folder was read")
		return nil, "", nil
	}
	if outlived(nil, c, "Show", &os.PathError{Op: "lstat", Path: "Show", Err: syscall.EACCES}, read) {
		t.Fatal("a record outlived its jobs for the library folder's own failure")
	}
}
