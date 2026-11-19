package podcastjob

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

func TestOwnedCleanupHonorsAgeLiveJobAndForeignFiles(t *testing.T) {
	for _, scenario := range []string{"old", "fresh", "live", "foreign", "identity"} {
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
			dir.WriteFile(s.checkpointName(job), b, 0600)
			dir.Close()
			if scenario == "live" {
				if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path, Attempt: 1, ServerID: "server-1"}); err != nil {
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
			s.cleanOwned(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, show, 82)
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
	s.cleanOwned(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, show, 82)
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
	s.dropStaleCheckpoint(job, root, "a1b2c3d4e5f60718", "1cc306d831645e2b")
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
	s.dropStaleCheckpoint(job, root, "a1b2c3d4e5f60718", "1cc306d831645e2b")
	if _, err := root.Lstat("Fixture Show/" + c.Temp); err != nil {
		t.Fatal("current partial removed", err)
	}
}

// A proven partial swapped for a link after its check is never appended to.
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
	if _, err := openPartial(root, "partial", 5); !errors.Is(err, errPartialUnreadable) {
		t.Fatal("size check", err)
	}
	root.Remove("partial")
	os.Symlink("victim", filepath.Join(dir, "partial"))
	if _, err := openPartial(root, "partial", 4); !errors.Is(err, errPartialUnreadable) {
		t.Fatal("link target accepted as the partial", err)
	}
}

// A partial the Bridge could not remove keeps its record and validator, so
// cleanOwned can find it again; once removable, the clear takes all three.
func TestClearCheckpointKeepsTheRecordOfAPartialItCouldNotRemove(t *testing.T) {
	requireNonRoot(t)
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
	if err := os.Chmod(show, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(show, 0o755) })
	stuck := c
	s.clearCheckpoint(job, root, &stuck)
	if saved, ok := s.readCheckpoint(job); !ok || saved.Temp != c.Temp {
		t.Fatal("the record of a partial still on disk was dropped")
	}
	if v, _ := s.o.Vault.Get(c.Validator); v == "" {
		t.Fatal("the validator of a partial still on disk was dropped")
	}
	os.Chmod(show, 0o755)
	s.clearCheckpoint(job, root, &c)
	if _, ok := s.readCheckpoint(job); ok {
		t.Fatal("record kept once the partial was removed")
	}
	if _, err := root.Lstat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial retained", err)
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
	if saved, ok := s.readCheckpoint(job); !ok || saved.Temp != old.Temp || saved.Validator != old.Validator {
		t.Fatalf("the earlier record was overwritten: %+v", saved)
	}
	if next.Validator != "" {
		t.Fatal("a validator was sealed for an unrecorded download")
	}
	os.Chmod(show, 0o755)
	if err := s.saveCheckpoint(job, root, &next, `"two"`); err != nil {
		t.Fatal(err)
	}
	if saved, ok := s.readCheckpoint(job); !ok || saved.Temp != next.Temp {
		t.Fatalf("the new download stayed unrecorded once the earlier partial was removable: %+v", saved)
	}
	if _, err := root.Lstat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the earlier partial was not removed", err)
	}
	if v, _ := s.o.Vault.Get(old.Validator); v != "" {
		t.Fatal("the earlier validator was kept")
	}
}
