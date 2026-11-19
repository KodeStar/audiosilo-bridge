package podcastjob

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// cleanOwned considers named checkpoint records and committed trash manifests
// older than a day, then sweeps the show's unrecorded partials
// (sweepPartials). self is the calling job, whose own running row (at the
// show's path) is not a live writer of these records. A record a running
// fetch relocated to the show (runningRecords) stays too.
func (s *service) cleanOwned(ctx context.Context, root *os.Root, lib link.Library, mount, show string, self int64) {
	if s.o.DB == nil || s.o.DataDir == "" || s.writable(lib, mount) != nil {
		return
	}
	rows, err := s.o.DB.OpenServerJobs(ctx)
	if err != nil {
		return
	}
	live := func(p string) bool {
		for _, r := range rows {
			if r.ID != self && r.ServerID == s.o.ServerID() && r.LibraryID != nil && *r.LibraryID == lib.ID && r.Path != nil && (*r.Path == p || *r.Path == show) && r.State == store.JobRunning {
				return true
			}
		}
		return false
	}
	old := func(stamp string) bool {
		t, err := time.Parse(time.RFC3339, stamp)
		return err == nil && s.o.Now().Sub(t) > 24*time.Hour
	}
	locked := func(p string, fn func()) {
		wait, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		release, err := s.fileLock(wait, root, p)
		if err != nil {
			return
		}
		defer release()
		if s.writable(lib, mount) == nil {
			fn()
		}
	}
	if checkpoints, err := s.checkpointRoot(); err == nil {
		records, _ := s.records(checkpoints, lib.ID)
		_ = checkpoints.Close()
		running := s.runningRecords(rows, lib.ID, self)
		for _, c := range records {
			if path.Dir(c.Path) != show || !old(c.UpdatedAt) || live(c.Path) || running(c) || !podcast.ValidID(c.ShowID) || !podcast.ValidID(c.EpisodeID) {
				continue
			}
			if _, _, err := s.readEpisode(root, c.Path, fetchParams{c.ShowID, c.EpisodeID}); err != nil {
				continue
			}
			job := jobs.Job{LibraryID: &lib.ID, Path: &c.Path}
			locked(c.Path, func() {
				// The records were listed before the loop: one a download at
				// this path saved since is not this one, and stays.
				if saved, ok := s.readCheckpoint(job, c.ShowID); ok && saved.Temp != c.Temp {
					return
				}
				s.clearCheckpoint(job, root, &c)
			})
		}
	}
	s.sweepPartials(ctx, root, lib, mount, []string{show}, self)
	dir := show + "/.audiosilo-trash/podcasts"
	st, err := root.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return
	}
	// OpenRegular below checks every ancestor before a manifest can authorize IO.
	entries, err := listDir(root, dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		j, err := podcast.ReadJournal(root, dir+"/"+entry.Name()+"/transaction.json")
		if err != nil || !podcast.ValidEpisodePath(j.Path) || path.Dir(j.Path) != show || j.TrashDir != ".audiosilo-trash/podcasts/"+entry.Name() || !old(j.Fetch.DeletedAt) || live(j.Path) {
			continue
		}
		locked(j.Path, func() { _ = podcast.CleanupTrash(root, j) })
	}
}

// sweepRecords drops this pairing's download records of library lib that no
// job reads any more: a day old (cleanOwned's age), of no running download
// (self, the calling job, aside; runningRecords), whose folder is gone or
// holds another show (podcast.ReadMoved, so a folder renamed by hand and not
// taken in yet is still its show's). A fetch that ends early (its episode
// found installed, a file in the way) or a take-in that could not move a
// record leaves it under a gone folder, and cleanOwned reads a show's records
// by the show's folder: nothing else would ever clear it, and the partial it
// names would never be swept. Only the record and its validator go: its
// partial is not at the record's path or not its show's, and sweepPartials
// takes it once old, unrecorded. A folder that cannot be read keeps its
// records, as does a record saved again since the listing.
func (s *service) sweepRecords(ctx context.Context, root *os.Root, lib link.Library, mount string, self int64) {
	if s.o.DB == nil || s.o.DataDir == "" || s.writable(lib, mount) != nil {
		return
	}
	rows, err := s.o.DB.OpenServerJobs(ctx)
	if err != nil {
		return
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		return
	}
	defer func() { _ = dir.Close() }()
	records, err := s.records(dir, lib.ID)
	if err != nil {
		return
	}
	running := s.runningRecords(rows, lib.ID, self)
	for _, c := range records {
		if ctx.Err() != nil {
			return
		}
		t, err := time.Parse(time.RFC3339, c.UpdatedAt)
		if err != nil || s.o.Now().Sub(t) <= 24*time.Hour || running(c) || !orphaned(root, c) {
			continue
		}
		name := recordName(lib.ID, c.ShowID, c.Path)
		if now, err := readRecord(dir, name); err != nil || now != c {
			continue
		}
		if s.writable(lib, mount) != nil {
			return
		}
		if dir.Remove(name) == nil && strings.HasPrefix(c.Validator, secrets.PodcastETagPrefix) {
			_ = s.o.Vault.Delete(c.Validator)
		}
	}
}

// orphaned reports whether record c's folder is gone, or holds a show other
// than c's.
func orphaned(root *os.Root, c checkpoint) bool {
	show := path.Dir(c.Path)
	if _, err := root.Lstat(show); errors.Is(err, os.ErrNotExist) {
		return true
	}
	f, _, err := podcast.ReadMoved(root, show)
	return err == nil && f.ShowID != c.ShowID
}
