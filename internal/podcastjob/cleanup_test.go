package podcastjob

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
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
			if err := s.saveCheckpoint(job, &c, `"one"`); err != nil {
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
			s.cleanOwned(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, "Fixture Show")
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
	s.o.Wait = wait
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
func TestLostIdentityAbandonRemovesOnlyOwnedPartial(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: "a1b2c3d4e5f60718", EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-owned", Offset: 6, Total: 100}
	root.WriteFile(path.Dir(*job.Path)+"/"+c.Temp, []byte("prefix"), 0600)
	if err := s.saveCheckpoint(job, &c, `"one"`); err != nil {
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
	if err := podcast.Publish(root, j, s.o.Now(), nil); err != nil {
		t.Fatal(err)
	}
	j.Op = "delete"
	j.AudioTemp = ""
	j.Chapters = nil
	j.Fetch.DeletedAt = s.o.Now().Add(-25 * time.Hour).Format(time.RFC3339)
	j.TrashDir = ".audiosilo-trash/podcasts/" + j.EpisodeID + "-g2-owned"
	if err := podcast.Publish(root, j, s.o.Now(), nil); err != nil {
		t.Fatal(err)
	}
	s.cleanOwned(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, "Fixture Show")
	if _, err := root.Lstat("Fixture Show/" + j.TrashDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed trash retained", err)
	}
	if f := h.read("Fixture Show"); f.Episodes[0].Fetch.DeletedAt != j.Fetch.DeletedAt {
		t.Fatal("cleanup changed tombstone")
	}
}
