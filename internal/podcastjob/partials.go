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

// SweepPartials sweeps every show of every checked, mapped and writable
// podcast library (sweepPartials): the Bridge runs it after a mapping check,
// so at start and on that check's cadence, beside a retain's cleanOwned.
func (h *Subscribe) SweepPartials(ctx context.Context) {
	s := h.s
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
// Nothing else ever resumes or removes such a file. In each show with no
// running job of this Bridge (self, the calling job, aside) and no install or
// delete journal awaiting recovery, it removes the regular files directly in
// the show folder named as the Bridge names its temps (ownedTempName) that are
// older than partialMaxAge and that no record of this pairing names. A link,
// a folder, a file under a linked folder or one of any other name is never
// touched. Nothing is swept when the open jobs or the records can't be read.
func (s *service) sweepPartials(ctx context.Context, root *os.Root, lib link.Library, mount string, shows []string, self int64) {
	if len(shows) == 0 || s.o.DB == nil || s.o.DataDir == "" || s.writable(lib, mount) != nil {
		return
	}
	rows, err := s.o.DB.OpenServerJobs(ctx)
	if err != nil {
		return
	}
	recorded, ok := s.recordedPartials(lib.ID)
	if !ok {
		return
	}
	busy := func(show string) bool {
		for _, r := range rows {
			if r.ID != self && r.State == store.JobRunning && r.LibraryID != nil && *r.LibraryID == lib.ID && r.Path != nil && (*r.Path == show || path.Dir(*r.Path) == show) {
				return true
			}
		}
		return false
	}
	now := s.o.Now()
	for _, show := range shows {
		if ctx.Err() != nil {
			return
		}
		if busy(show) || journalsPending(root, show) {
			continue
		}
		d, err := root.Open(show)
		if err != nil {
			continue
		}
		entries, err := d.ReadDir(-1)
		_ = d.Close()
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := show + "/" + e.Name()
			if !ownedTempName.MatchString(e.Name()) || recorded[p] {
				continue
			}
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

// recordedPartials is the root-relative partials this pairing's checkpoint
// records in library id name, whether or not the record is still valid: false
// when the records can't be read.
func (s *service) recordedPartials(id int64) (map[string]bool, bool) {
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
		if errors.Is(err, podcast.ErrInvalidFile) {
			continue // not a regular file: never read as a record
		}
		if err != nil {
			return nil, false
		}
		var c checkpoint
		err = json.NewDecoder(io.LimitReader(rd, 16384)).Decode(&c)
		_ = rd.Close()
		if err == nil && c.LibraryID == id && podcast.OwnedTemp(c.Temp) {
			recorded[c.partialName()] = true
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
	d, err := root.Open(dir)
	if err != nil {
		return true
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
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
