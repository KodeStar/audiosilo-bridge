package podcastjob

import (
	"context"
	"errors"
	"os"
	"path"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
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
	old := s.dayOld
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
// (runningRecords), whose folder is gone, holds another show or holds the
// show taken in under another spelling of its name (orphaned; podcast.ReadMoved,
// so a folder renamed by hand to the record's folder name and not taken in
// yet is still its show's). A fetch that ends early (its episode found
// installed, a file in the way) or a take-in that could not move a record
// leaves it under a gone folder, and cleanOwned reads a show's records by the
// show's folder: nothing else would ever clear it, and the partial it names
// would never be swept. A record whose partial is in one of shows, a folder
// of its show renamed by hand and not taken in yet, stays: that take-in moves
// it (adoptCheckpoints) and the download resumes there. Only the record and
// its validator go: its partial is not at the record's path or not its
// show's, and sweepPartials takes it once old, unrecorded. A folder that
// cannot be read keeps its records, as does a record saved again since the
// listing, and nothing goes once the library's marker is not there (a share
// unmounted since the mapping check, its empty mount point holding no show).
func (s *service) sweepRecords(ctx context.Context, root *os.Root, lib link.Library, mount string, shows []string) {
	if s.o.DB == nil || s.o.DataDir == "" || s.writable(lib, mount) != nil || !markerThere(root) {
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
	running := s.runningRecords(rows, lib.ID, 0)
	read := movedReader(root)
	for _, c := range records {
		if ctx.Err() != nil {
			return
		}
		if !s.dayOld(c.UpdatedAt) || running(c) || !orphaned(root, c, read) || awaitsTakeIn(root, shows, c, read) {
			continue
		}
		name := recordName(lib.ID, c.ShowID, c.Path)
		if now, err := readRecord(dir, name); err != nil || now != c {
			continue
		}
		if s.writable(lib, mount) != nil || !markerThere(root) {
			return
		}
		if dir.Remove(name) == nil {
			s.dropValidator(c.Validator)
		}
	}
}

// markerThere reports whether root holds its library's marker (or cannot
// say): a mount point whose share is gone holds none.
func markerThere(root *os.Root) bool {
	_, err := root.Lstat(mapping.MarkerPath)
	return !errors.Is(err, os.ErrNotExist)
}

// movedReader is podcast.ReadMoved in root, each show folder read once.
func movedReader(root *os.Root) func(string) (*podcast.Feed, string, error) {
	type answer struct {
		f    *podcast.Feed
		from string
		err  error
	}
	read := map[string]answer{}
	return func(show string) (*podcast.Feed, string, error) {
		a, ok := read[show]
		if !ok {
			a.f, a.from, a.err = podcast.ReadMoved(root, show)
			read[show] = a
		}
		return a.f, a.from, a.err
	}
}

// dayOld reports whether RFC 3339 stamp is over a day old: the age at which
// cleanOwned and sweepRecords may clear a record or a trashed episode.
func (s *service) dayOld(stamp string) bool {
	t, err := time.Parse(time.RFC3339, stamp)
	return err == nil && s.o.Now().Sub(t) > 24*time.Hour
}

// orphaned reports whether record c's folder is gone, holds a show other
// than c's, or holds c's show taken in under another spelling of its name (a
// case-only rename by hand: every job resolves the spelling on disk, so none
// reads the record at its path). One that cannot be read is not.
func orphaned(root *os.Root, c checkpoint, read func(string) (*podcast.Feed, string, error)) bool {
	show := path.Dir(c.Path)
	real, err := podcast.RealName(root, show)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	f, from, err := read(real)
	return err == nil && (f.ShowID != c.ShowID || real != show && from == "")
}

// awaitsTakeIn reports whether record c's partial is in one of shows whose
// folder holds c's show renamed by hand and not taken in yet (or cannot be
// read to tell): its take-in moves the record.
func awaitsTakeIn(root *os.Root, shows []string, c checkpoint, read func(string) (*podcast.Feed, string, error)) bool {
	for _, show := range shows {
		if _, err := podcast.LstatRegular(root, show+"/"+c.Temp); err != nil {
			continue
		}
		f, from, err := read(show)
		return err != nil || f.ShowID == c.ShowID && from != ""
	}
	return false
}
