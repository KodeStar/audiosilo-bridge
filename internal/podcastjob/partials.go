package podcastjob

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// partialMaxAge is how old an unrecorded partial must be before the sweep
// takes it: longer than the longest episode download (EpisodeCeiling of the
// largest admitted size, about 14 hours, plus the mapping wait's ceiling), so
// no running download's file is ever this old.
const partialMaxAge = 24 * time.Hour

// ownedTempName matches the names the Bridge gives its own temp files in a
// show folder, and only those: podcast.TempPrefix and 24 hex digits (a
// download's partial, podcast.TempPrefix + nonce(), and an atomic write's
// temp, fsutil.WriteRootAtomic's). podcast.OwnedTemp only checks the prefix,
// which is enough for a name a record holds but not for a name found on disk.
var ownedTempName = regexp.MustCompile(`^` + regexp.QuoteMeta(podcast.TempPrefix) + `[0-9a-f]{24}$`)

// sweepEvery spaces the whole-library sweeps: a partial waits a day
// (partialMaxAge) before it may go, so reading every show folder on each
// mapping check (every ten minutes by default) would buy nothing.
const sweepEvery = time.Hour

// SweepPartials sweeps every show of every checked, mapped and writable
// podcast library (sweepPartials): the Bridge calls it after a mapping check,
// so at start and then at most once per sweepEvery, beside a retain's
// cleanOwned.
func (h *Subscribe) SweepPartials(ctx context.Context) {
	s := h.s
	now := s.o.Now()
	s.mu.Lock()
	// A sweep still running, or one just done, covers this call too. (A clock
	// set back since the last one sweeps now.)
	if since := now.Sub(s.swept); !s.swept.IsZero() && since >= 0 && since < sweepEvery {
		s.mu.Unlock()
		return
	}
	s.swept = now
	s.mu.Unlock()
	for _, st := range s.o.Libraries.Statuses() {
		if ctx.Err() != nil {
			return
		}
		lib, ok := s.o.Libraries.Library(st.LibraryID)
		if !ok || lib.LibraryKind() != "podcasts" {
			continue
		}
		mount, _ := s.o.Mount(lib.ID, lib.Root)
		if s.writable(lib, mount) != nil {
			continue
		}
		func() {
			root, err := os.OpenRoot(mount)
			if err != nil {
				return
			}
			defer func() { _ = root.Close() }()
			entries, err := readDir(root)
			if err != nil {
				return
			}
			var shows []string
			for _, e := range entries {
				if e.IsDir() && podcast.ValidShow(e.Name()) {
					shows = append(shows, e.Name())
				}
			}
			s.sweepPartials(ctx, root, lib, mount, shows, 0)
		}()
	}
}

// sweepPartials removes the old partials no checkpoint record names (issue
// #87): a download stopped before its first checkpoint, or one whose record a
// later download could not take over, whose file could not be removed then.
// Nothing else ever resumes or removes such a file. In each show (a folder
// holding a feed.json) with no running job of this Bridge (self, the calling
// job, aside) and no install or delete journal awaiting recovery, it removes
// the regular files directly in the show folder named as the Bridge names its
// temps (ownedTempName) that are older than partialMaxAge and that no record
// of this pairing names. A link, a folder, a file under a linked folder or one
// of any other name is never touched. A show holding no file of such a name
// costs one folder listing; for any other the open jobs and the records are
// read then, show by show, so a job claimed or a record written while an
// earlier show was swept counts. Nothing is swept when either can't be read.
func (s *service) sweepPartials(ctx context.Context, root *os.Root, lib link.Library, mount string, shows []string, self int64) {
	if s.o.DB == nil || s.o.DataDir == "" || s.writable(lib, mount) != nil {
		return
	}
	for _, show := range shows {
		if ctx.Err() != nil {
			return
		}
		var names []string
		entries, _ := listDir(root, show)
		for _, e := range entries {
			if ownedTempName.MatchString(e.Name()) {
				names = append(names, e.Name())
			}
		}
		if len(names) == 0 {
			continue
		}
		if _, err := podcast.LstatRegular(root, show+"/feed.json"); err != nil {
			continue // not a show (or a linked one): nothing in it is the Bridge's to sweep
		}
		if s.busy(ctx, lib.ID, show, self) || journalsPending(root, show) {
			continue
		}
		recorded, ok := s.recordedPartials()
		if !ok {
			return
		}
		now := s.o.Now()
		for _, name := range names {
			if recorded[name] {
				continue
			}
			p := show + "/" + name
			// No link in any component, and a regular file at the name.
			st, err := podcast.LstatRegular(root, p)
			if err != nil || now.Sub(st.ModTime()) <= partialMaxAge {
				continue
			}
			if s.writable(lib, mount) != nil {
				return
			}
			_ = root.Remove(p)
		}
	}
}

// busy reports whether a job of this Bridge other than self is running in
// library id's show (at the show's path or an episode's), or the open jobs
// can't be read to tell. A claimed job's row is running from its claim.
func (s *service) busy(ctx context.Context, id int64, show string, self int64) bool {
	rows, err := s.o.DB.OpenServerJobs(ctx)
	if err != nil {
		return true
	}
	for _, r := range rows {
		if r.ID != self && r.State == store.JobRunning && r.LibraryID != nil && *r.LibraryID == id && r.Path != nil && (*r.Path == show || path.Dir(*r.Path) == show) {
			return true
		}
	}
	return false
}

// recordedPartials is the names of the partials this pairing's checkpoint
// records name, in any library and whether or not the record is still valid: a
// partial's name is random, so the bare name tells it apart whatever the form
// of its show's name on disk. False when the records can't be read, or the
// pairing's token can't (pairHash would then name another, empty folder and
// every recorded partial would pass for unrecorded).
func (s *service) recordedPartials() (map[string]bool, bool) {
	if _, err := s.o.Vault.Get(secrets.ServerToken); err != nil {
		return nil, false
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		return nil, false
	}
	defer func() { _ = dir.Close() }()
	entries, err := readDir(dir)
	if err != nil {
		return nil, false
	}
	recorded := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		rd, err := podcast.OpenRegular(dir, entry.Name())
		if errors.Is(err, podcast.ErrInvalidFile) || errors.Is(err, os.ErrNotExist) {
			continue // not a regular file (never read as a record), or cleared since the listing
		}
		if err != nil {
			return nil, false
		}
		b, err := io.ReadAll(io.LimitReader(rd, 16385))
		_ = rd.Close()
		if err != nil {
			return nil, false
		}
		// A record no job can read back (readCheckpoint's limit) holds nothing.
		var c checkpoint
		if len(b) <= 16384 && json.Unmarshal(b, &c) == nil && podcast.OwnedTemp(c.Temp) {
			recorded[c.Temp] = true
		}
	}
	return recorded, true
}

// journalsPending reports whether show's .audiosilo/ holds an install or
// delete journal (podcast.RecoverShow's *.publish.json), whose staged audio
// the sweep must leave for the recovery, or can't be read to tell.
func journalsPending(root *os.Root, show string) bool {
	dir := show + "/.audiosilo"
	st, err := root.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return true
	}
	entries, err := listDir(root, dir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".publish.json") {
			return true
		}
	}
	return false
}

// listDir is the entries of dir inside root.
func listDir(root *os.Root, dir string) ([]os.DirEntry, error) {
	d, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	return d.ReadDir(-1)
}
