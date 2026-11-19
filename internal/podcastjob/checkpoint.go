package podcastjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

type checkpoint struct {
	ServerHash string `json:"server_hash"`
	LibraryID  int64  `json:"library_id"`
	ShowID     string `json:"show_id"`
	EpisodeID  string `json:"episode_id"`
	Path       string `json:"path"`
	Temp       string `json:"temp"`
	Offset     int64  `json:"offset"`
	Total      int64  `json:"total"`
	Validator  string `json:"validator"`
	UpdatedAt  string `json:"updated_at"`
}

// partialName is the root-relative name of the partial c records, beside its
// episode.
func (c *checkpoint) partialName() string {
	return path.Dir(c.Path) + "/" + c.Temp
}

func nonce() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
func (s *service) pairHash() string {
	token, _ := s.o.Vault.Get(secrets.ServerToken)
	return podcast.FeedURLHash(s.o.ServerID() + "|" + token)
}
func (s *service) checkpointName(job jobs.Job) string {
	return recordName(*job.LibraryID, *job.Path)
}

// recordName is the file name of library lib's download record for episode p.
func recordName(lib int64, p string) string {
	return podcast.FeedURLHash(strconv.FormatInt(lib, 10)+"|"+p) + ".json"
}
func (s *service) checkpointRoot() (*os.Root, error) {
	if s.o.DataDir == "" {
		return nil, errors.New("missing checkpoint directory")
	}
	dir := filepath.Join(s.o.DataDir, "podcast-fetch", s.pairHash())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}
func (s *service) saveCheckpoint(job jobs.Job, root *os.Root, c *checkpoint, etag string) error {
	dir, err := s.checkpointRoot()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if saved, ok := s.readCheckpoint(job); ok && saved.Temp != c.Temp && (saved.ShowID != c.ShowID || !s.dropPartial(root, &saved)) {
		// An earlier partial at this path the Bridge still cannot remove: its
		// record stays (clearCheckpoint tries it again, cleanOwned after a day),
		// and this download goes unrecorded rather than orphan it (left behind,
		// sweepPartials takes it once old). One removed since (or already gone)
		// no longer holds the record. Another show's record at this path stays
		// too (clearCheckpoint).
		return nil
	}
	if c.Validator == "" {
		c.Validator = secrets.PodcastETagPrefix + nonce()
	}
	if !netguard.StrongETag(etag) {
		return errors.New("unproven validator")
	}
	if err := s.o.Vault.Set(c.Validator, etag); err != nil {
		return err
	}
	c.UpdatedAt = s.o.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return fsutil.WriteRootAtomic(dir, ".", s.checkpointName(job), b, 0o600, nil)
}

// readCheckpoint is the job's saved record when it is this pairing's, for the
// job's library and path, naming an owned temp and a sealed validator.
func (s *service) readCheckpoint(job jobs.Job) (checkpoint, bool) {
	dir, err := s.checkpointRoot()
	if err != nil {
		return checkpoint{}, false
	}
	defer func() { _ = dir.Close() }()
	saved, ok := readRecord(dir, s.checkpointName(job))
	ok = ok && saved.ServerHash == s.pairHash() && saved.LibraryID == *job.LibraryID && saved.Path == *job.Path && podcast.OwnedTemp(saved.Temp) && strings.HasPrefix(saved.Validator, secrets.PodcastETagPrefix)
	return saved, ok
}

// readRecord parses the download record named name, unchecked.
func readRecord(dir *os.Root, name string) (checkpoint, bool) {
	var c checkpoint
	rd, err := podcast.OpenRegular(dir, name)
	if err != nil {
		return c, false
	}
	defer rd.Close()
	b, err := io.ReadAll(io.LimitReader(rd, 16385))
	if err != nil || len(b) > 16384 || json.Unmarshal(b, &c) != nil {
		return checkpoint{}, false
	}
	return c, true
}

// records is this pairing's download records for library lib, in name order:
// each one under its own path's name (recordName), naming an episode path and
// an owned partial. An error means the records could not be listed.
func (s *service) records(dir *os.Root, lib int64) ([]checkpoint, error) {
	entries, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	pairing := s.pairHash()
	var out []checkpoint
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		c, ok := readRecord(dir, entry.Name())
		if ok && c.ServerHash == pairing && c.LibraryID == lib && podcast.ValidEpisodePath(c.Path) && podcast.OwnedTemp(c.Temp) && entry.Name() == recordName(lib, c.Path) {
			out = append(out, c)
		}
	}
	return out, nil
}

// recordOf is the episode path of a fetch's download record: the job's path,
// or where adoptCheckpoints moved the record with a show folder renamed by
// hand (the runner abandons a job by the path the server named, the old
// one). A moved record is this show's, for this episode, under the same file
// name.
func (s *service) recordOf(job jobs.Job, p fetchParams) string {
	if saved, ok := s.readCheckpoint(job); ok && saved.ShowID == p.ShowID && saved.EpisodeID == p.EpisodeID {
		return *job.Path
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		return *job.Path
	}
	defer func() { _ = dir.Close() }()
	records, _ := s.records(dir, *job.LibraryID)
	for _, c := range records {
		if c.ShowID == p.ShowID && c.EpisodeID == p.EpisodeID && path.Base(c.Path) == path.Base(*job.Path) {
			return c.Path
		}
	}
	return *job.Path
}

// dropStaleCheckpoint removes this path's owned partial when it was made for an
// identity the current feed no longer has (a show or episode replaced at the
// same path): its record is about to be overwritten, which would orphan it.
// The caller holds the path's file lock and passes the feed's current IDs.
func (s *service) dropStaleCheckpoint(job jobs.Job, root *os.Root, showID, episodeID string) {
	if saved, ok := s.readCheckpoint(job); ok && (saved.ShowID != showID || saved.EpisodeID != episodeID) {
		s.clearCheckpoint(job, root, &saved)
	}
}

func (s *service) loadCheckpoint(job jobs.Job, root *os.Root, showID, episodeID string) (checkpoint, string) {
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: *job.LibraryID, ShowID: showID, EpisodeID: episodeID, Path: *job.Path}
	saved, ok := s.readCheckpoint(job)
	if !ok || saved.ShowID != showID || saved.EpisodeID != episodeID {
		return c, ""
	}
	tag, _ := s.o.Vault.Get(saved.Validator)
	partial, err := podcast.OpenRegular(root, saved.partialName())
	if err == nil {
		defer partial.Close()
		st, e := partial.Stat()
		if e == nil && st.Size() == saved.Offset && saved.Offset > 0 && saved.Offset < saved.Total && saved.Total <= s.o.MaxEpisodeBytes && netguard.StrongETag(tag) {
			return saved, tag
		}
	}
	s.clearCheckpoint(job, root, &saved)
	return c, ""
}

// clearCheckpoint removes the owned partial, its sealed validator and the
// record (only under the pairing that made them), then resets c to no partial.
// A partial it cannot remove keeps its validator and the record, for cleanOwned
// once it is a day old; an earlier such partial the record still names is tried
// again first, so no later clear in the job drops its record. A record of
// another show at the path is left with its partial: adoptCheckpoints put it
// there for a show folder renamed by hand to this job's folder name (names
// swapped, or another show moved to it, an episode file name both shows use),
// while this job still works under that name.
func (s *service) clearCheckpoint(job jobs.Job, root *os.Root, c *checkpoint) {
	defer func() { c.Temp, c.Offset, c.Total, c.Validator = "", 0, 0, "" }()
	if c.ServerHash != s.pairHash() || !s.dropPartial(root, c) {
		return
	}
	if saved, ok := s.readCheckpoint(job); ok && saved.Temp != c.Temp && (saved.ShowID != c.ShowID || !s.dropPartial(root, &saved)) {
		return
	}
	if dir, err := s.checkpointRoot(); err == nil {
		_ = dir.Remove(s.checkpointName(job))
		_ = dir.Close()
	}
}

// adoptCheckpoints moves this pairing's download records for the episodes of
// show id in a show folder taken in under show (adopt) from their paths under
// its old name from. A record names its episode's path twice (its own name is
// the path's hash), and its partial moved with the folder: left as it was, no
// fetch at the new path would resume it, cleanOwned (which reads a show's
// records by their folder) would never clear it and sweepPartials would leave
// its partial for good, as one a record names. adopt moves them before
// feed.json, so a stop between the two leaves a folder that is taken in again,
// moving what is left. The show's records under a gone folder move too: one
// such stop, or a take-in podcast.Adopt refused after the records moved, then
// the folder renamed again by hand, leaves them under that earlier name
// (podcast.Adopt's journals alike). A record whose download is still running
// (a fetch claimed at its path, or one of its episode that found the show
// elsewhere since its claim), other than self's, stays: that job moves it when
// it finds the folder (Fetch's relocation) or drops it as it ends. A record of
// another show (names swapped by hand) stays too. An error means the records
// could not be read; one that could not be moved is left as it was.
func (s *service) adoptCheckpoints(ctx context.Context, root *os.Root, lib link.Library, show, from, id string, self int64) error {
	if s.o.DB == nil || s.o.DataDir == "" {
		return nil
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	records, err := s.records(dir, lib.ID)
	if err != nil {
		return err
	}
	rows, err := s.o.DB.OpenServerJobs(ctx)
	if err != nil {
		return err
	}
	// running reports whether c's download still runs: a job claimed at its
	// path, or a fetch of its episode (one that found the show elsewhere since
	// its claim, Fetch's relocation, works at a path its row does not name).
	running := func(c checkpoint) bool {
		return slices.ContainsFunc(rows, func(r store.ServerJob) bool {
			if r.ID == self || r.State != store.JobRunning || r.ServerID != s.o.ServerID() || r.LibraryID == nil || *r.LibraryID != lib.ID {
				return false
			}
			if r.Path != nil && *r.Path == c.Path {
				return true
			}
			var p fetchParams
			return r.Kind == FetchKind && json.Unmarshal(r.Params, &p) == nil && p.ShowID == c.ShowID && p.EpisodeID == c.EpisodeID
		})
	}
	for _, c := range records {
		if c.ShowID != id {
			continue
		}
		if at := path.Dir(c.Path); at != from {
			if _, err := root.Lstat(at); at == show || !errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		if running(c) {
			continue
		}
		old := recordName(lib.ID, c.Path)
		c.Path = show + "/" + path.Base(c.Path)
		name := recordName(lib.ID, c.Path)
		if _, err := dir.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			continue // never over a record a download at the new path already made
		}
		b, err := json.Marshal(c)
		if err != nil {
			continue
		}
		if fsutil.WriteRootAtomic(dir, ".", name, b, 0o600, nil) == nil {
			_ = dir.Remove(old)
		}
	}
	return nil
}

// dropPartial removes c's owned partial, then its sealed validator; false, with
// both kept, when the partial is still on disk. A partial an install journal
// stages is the journal's (podcast.RecoverShow installs it): it stays on disk
// and only the record's claim on it goes.
func (s *service) dropPartial(root *os.Root, c *checkpoint) bool {
	if podcast.OwnedTemp(c.Temp) {
		p := c.partialName()
		// Checked, not opened: a partial the Bridge can no longer read (the
		// restart from zero clears one) is still its own to remove.
		if st, err := podcast.LstatRegular(root, p); err == nil && !journalStages(root, c, st) {
			if err := root.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false
			}
		}
	}
	if strings.HasPrefix(c.Validator, secrets.PodcastETagPrefix) {
		_ = s.o.Vault.Delete(c.Validator)
	}
	return true
}

// journalStages reports whether the episode's pending journal names c's partial
// (st, as LstatRegular saw it) as its staged audio, or is there but can't be
// read to tell. A name spelt otherwise for the same file (a case-insensitive
// or normalising filesystem) counts too: RecoverShow would install that file.
func journalStages(root *os.Root, c *checkpoint, st os.FileInfo) bool {
	j, err := podcast.ReadJournal(root, podcast.AttachmentPath(c.Path, ".publish.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil || j.AudioTemp == c.Temp {
		return true
	}
	if j.AudioTemp == "" {
		return false
	}
	staged, err := root.Lstat(path.Dir(c.Path) + "/" + j.AudioTemp)
	return err == nil && os.SameFile(st, staged)
}
func (s *service) fileLock(ctx context.Context, root *os.Root, episode string) (func(), error) {
	if err := podcast.EnsureDir(root, path.Dir(episode)+"/.audiosilo"); err != nil {
		return nil, err
	}
	p := podcast.AttachmentPath(episode, ".fetch.lock")
	if st, err := root.Lstat(p); err == nil && !st.Mode().IsRegular() {
		return nil, podcast.ErrInvalidFile
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raced := 0
	for {
		f, err := root.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrNotExist) && raced < 100 {
			// The holder removed the file as we opened it (macOS can say so
			// for an O_CREATE open): open it again.
			raced++
			continue
		}
		if err != nil {
			return nil, err
		}
		ok, err := tryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if ok {
			// The holder before us removes the file as it lets go, so the lock
			// we got may be on a file no longer at p (one another job has since
			// made again and locked): only the file still at p counts.
			if lockedAt(root, p, f) {
				return func() { releaseLock(root, p, f) }, nil
			}
			unlockFile(f)
			_ = f.Close()
			// Opened again at once, unless the job ended meanwhile or p is no
			// longer a lock file (a folder or link put there since the check
			// above would never be the file locked, and the loop would spin).
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if st, err := root.Lstat(p); err == nil && !st.Mode().IsRegular() {
				return nil, podcast.ErrInvalidFile
			}
			continue
		}
		_ = f.Close()
		if err := s.o.Wait(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

// releaseLock removes the episode's .fetch.lock while still holding it, then
// lets go: a job waiting on the removed file finds it gone (lockedAt) and
// opens the path again, so the files never pile up and no two jobs hold one.
// Windows too: os.Root opens files with FILE_SHARE_DELETE and removes them
// with POSIX semantics, so removing after letting go would let a waiter lock
// the file just before it goes while a third job makes and locks a new one.
func releaseLock(root *os.Root, p string, f *os.File) {
	_ = root.Remove(p)
	unlockFile(f)
	_ = f.Close()
}

// lockedAt reports whether f is still the file at p.
func lockedAt(root *os.Root, p string, f *os.File) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	now, err := root.Lstat(p)
	return err == nil && os.SameFile(held, now)
}
