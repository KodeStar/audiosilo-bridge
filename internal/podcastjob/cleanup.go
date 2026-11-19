package podcastjob

import (
	"context"
	"errors"
	"log/slog"
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
// (sweepPartials). A day-old record whose partial cannot be removed (one a
// share's lock or a file flag keeps, or whose check keeps failing) goes all
// the same, its partial left unrecorded and logged: kept, it would stay for
// good. A show folder the Bridge may not write never gets here: retain's own
// write check (lockShow's beforeWrite) refuses it first, and its records stay
// until the folder may be written again. self is the calling job, whose own running row (at the
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
				listed := c
				if !s.clearCheckpoint(job, root, &c) && s.forgetRecord(checkpoints, records, listed) {
					slog.Warn("podcasts: a day-old download's partial could not be removed; its record is dropped and the partial left unrecorded", "partial", listed.partialName())
				}
			})
		}
		_ = checkpoints.Close()
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
// job reads or clears any more: a day old (cleanOwned's age), of no running
// download (runningRecords), whose folder is gone, holds another show or
// holds the show taken in under another spelling of its name (orphaned;
// podcast.ReadMoved, so a folder renamed by hand to the record's folder name
// and not taken in yet is still its show's), or that outlived every job that
// could (outlived: a show folder the Bridge may not search, a partial that
// cannot be opened, a whole download never read back). A fetch that ends
// early (its episode found installed, a file in the way) or a take-in that
// could not move a record leaves it under a gone folder, and cleanOwned reads
// a show's records by the show's folder: nothing else would ever clear it,
// and the partial it names would never be swept. A record whose partial is in one of shows, a folder
// of its show renamed by hand and not taken in yet, stays: that take-in moves
// it (adoptCheckpoints) and the download resumes there. Only the record and
// its validator go: its partial is not at the record's path or not its
// show's, or cannot be reached, and sweepPartials takes it once old and
// removable, unrecorded (never a published episode, nor audio a journal
// stages). A folder that cannot be read for any other cause (a feed.json
// being written) keeps its records, as does a record saved again since the
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
	// Each record's folder spelt as it is on disk (podcast.RealName lists the
	// library's root), found once per folder.
	type spelling struct {
		real string
		err  error
	}
	spelt := map[string]spelling{}
	for _, c := range records {
		if ctx.Err() != nil {
			return
		}
		if !s.dayOld(c.UpdatedAt) || running(c) {
			continue
		}
		show := path.Dir(c.Path)
		sp, ok := spelt[show]
		if !ok {
			sp.real, sp.err = podcast.RealName(root, show)
			spelt[show] = sp
		}
		gone := orphaned(c, sp.real, sp.err, read)
		if !gone && !outlived(root, c, sp.real, sp.err, read) || awaitsTakeIn(root, shows, c, read) {
			continue
		}
		if s.writable(lib, mount) != nil || !markerThere(root) {
			return
		}
		// The open jobs read again just before the drop: no episode lock is
		// taken here, and a fetch claimed since they were first read may have
		// loaded the record already (a whole download, or a partial in a show
		// folder it can read). One claimed after this read still has its
		// library, show and episode lock to find before it reads the record,
		// and the drop is next.
		if rows, err = s.o.DB.OpenServerJobs(ctx); err != nil {
			return
		}
		if running = s.runningRecords(rows, lib.ID, 0); running(c) {
			continue
		}
		if s.forgetRecord(dir, records, c) && !gone {
			slog.Warn("podcasts: a day-old download record no job could resume or clear is dropped; its partial is left unrecorded", "partial", c.partialName())
		}
	}
}

// forgetRecord removes record c, as listed among records, when it is still
// so (one saved again since stays), and its validator unless another of them
// names it (validatorNamed). It reports whether the record went.
func (s *service) forgetRecord(dir *os.Root, records []checkpoint, c checkpoint) bool {
	name := recordName(c.LibraryID, c.ShowID, c.Path)
	if now, err := readRecord(dir, name); err != nil || now != c {
		return false
	}
	if dir.Remove(name) != nil {
		return false
	}
	if !validatorNamed(dir, records, c) {
		s.dropValidator(c.Validator)
	}
	return true
}

// outlived reports whether record c, a day old, is past every job that could
// resume or clear it where it is: a whole download kept because it could not
// be read back (complete: a day on, still not published), a show folder the
// Bridge may not search (no job reads its feed.json or takes the episode's
// lock there), a partial that cannot be opened to tell (EIO, ESTALE, no
// permission: loadCheckpoint keeps the first two for good, and dropPartial
// cannot check them), or one of another size than the record's (no fetch
// resumes it, and one whose clear keeps failing, a share's lock or a file
// flag, fails every fetch errPartialStuck; retain's cleanOwned may never run
// for the show). A partial proven gone is not: the next fetch clears its
// record. real and err are podcast.RealName's answer for c's folder: an
// error there is the library folder's own (RealName lists it), never the
// show folder's, so it tells nothing, a permission refused included.
func outlived(root *os.Root, c checkpoint, real string, err error, read func(string) (*podcast.Feed, string, error)) bool {
	if c.complete() {
		return true
	}
	if err != nil {
		return false
	}
	if _, _, err := read(real); err != nil {
		return errors.Is(err, os.ErrPermission)
	}
	size, err := partialSize(root, c.partialName())
	if err != nil {
		return !partialGone(err)
	}
	return size != c.Offset
}

// validatorNamed reports whether another of records (as listed, read again
// now) names record c's validator: a move that wrote the record at its new
// path and could not remove the old one (moveRecord) leaves two naming one
// validator and one partial, and dropping it with the old record would cost
// the moved one its resume (loadCheckpoint clears a partial with no
// validator), as migrateRecords never does either.
func validatorNamed(dir *os.Root, records []checkpoint, c checkpoint) bool {
	for _, o := range records {
		if o.Validator != c.Validator || o.ShowID == c.ShowID && o.Path == c.Path {
			continue
		}
		if now, err := readRecord(dir, recordName(o.LibraryID, o.ShowID, o.Path)); err == nil && now.Validator == c.Validator {
			return true
		}
	}
	return false
}

// markerThere reports whether root holds its library's marker (or cannot
// say): a mount point whose share is gone holds none.
func markerThere(root *os.Root) bool { return folderThere(root, mapping.MarkerPath) }

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
// reads the record at its path). One that cannot be read is not. real and
// err are podcast.RealName's answer for c's folder.
func orphaned(c checkpoint, real string, err error, read func(string) (*podcast.Feed, string, error)) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	f, from, err := read(real)
	return err == nil && (f.ShowID != c.ShowID || real != path.Dir(c.Path) && from == "")
}

// awaitsTakeIn reports whether record c's partial is in one of shows whose
// folder holds c's show renamed by hand and not taken in yet (or cannot be
// read to tell): its take-in moves the record.
func awaitsTakeIn(root *os.Root, shows []string, c checkpoint, read func(string) (*podcast.Feed, string, error)) bool {
	for _, show := range shows {
		if _, err := podcast.LstatRegular(root, show+"/"+c.Temp); err != nil {
			continue
		}
		// Every folder holding a file of the partial's name is asked: a copy
		// of the folder (taken in, or another show's) may hold one too.
		if f, from, err := read(show); err != nil || f.ShowID == c.ShowID && from != "" {
			return true
		}
	}
	return false
}
