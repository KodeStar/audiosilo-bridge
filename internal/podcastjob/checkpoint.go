package podcastjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	// SHA256 is the hex hash of a whole download (complete), as it was
	// written: what the next attempt verifies the partial against.
	SHA256 string `json:"sha256,omitempty"`
	// Source is the hex hash of the enclosure URL the partial was downloaded
	// from (never the URL: a private feed's carries its secret), and
	// SourceLength the length the feed gave it, when it gave one. A whole
	// download is published with no GET (complete): a feed that changed the
	// episode's audio under the same episode id since is told by these
	// (sameSource). A record from before them has neither, and is verified
	// by its hash alone.
	Source       string `json:"source,omitempty"`
	SourceLength int64  `json:"source_length,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

// complete reports whether c records a whole download, its partial holding
// every byte (Offset == Total) as hashed while it was written (SHA256): a
// fetch whose read-back failed keeps it (resume-at-total), and the next
// attempt verifies and publishes it with no GET (Fetch.run).
func (c *checkpoint) complete() bool {
	return c.Offset > 0 && c.Offset == c.Total && podcast.ValidSHA256(c.SHA256)
}

// setSource records enc as the enclosure c's partial is downloaded from.
func (c *checkpoint) setSource(enc podcast.Enclosure) {
	c.Source, c.SourceLength = podcast.FeedURLHash(enc.URL), max(enc.Length, 0)
}

// sameSource reports whether enc is the enclosure c's partial was downloaded
// from: the same URL and, when both lengths are known, the same length. A
// record with no Source (written before it was kept) cannot tell, and is.
func (c *checkpoint) sameSource(enc podcast.Enclosure) bool {
	if c.Source == "" {
		return true
	}
	return c.Source == podcast.FeedURLHash(enc.URL) && (c.SourceLength <= 0 || enc.Length <= 0 || c.SourceLength == enc.Length)
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
func (s *service) checkpointName(job jobs.Job, show string) string {
	return recordName(*job.LibraryID, show, *job.Path)
}

// recordName is the file name of library lib's download record for episode p
// of show id show. The show's id is part of it: two show folders whose names
// were swapped by hand, holding episodes of the same file name, would
// otherwise each find the other's record at its path, and neither record
// could move with its show (adoptCheckpoints never overwrites one).
func recordName(lib int64, show, p string) string {
	return podcast.FeedURLHash(strconv.FormatInt(lib, 10)+"|"+show+"|"+p) + ".json"
}

// legacyRecordName is the name an older Bridge gave a record, by its library
// and path alone (migrateRecords).
func legacyRecordName(lib int64, p string) string {
	return podcast.FeedURLHash(strconv.FormatInt(lib, 10)+"|"+p) + ".json"
}

func (s *service) checkpointRoot() (*os.Root, error) {
	if s.o.DataDir == "" {
		return nil, errors.New("missing checkpoint directory")
	}
	pairing := s.pairHash()
	dir := filepath.Join(s.o.DataDir, "podcast-fetch", pairing)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err == nil {
		s.migrateRecords(root, pairing)
	}
	return root, err
}

// migrateRecords renames, once per pairing and start, the records an older
// Bridge named by library and path alone (legacyRecordName) to their names by
// show (recordName), so an interrupted download still resumes after the
// upgrade and its record still moves with its show. A legacy record whose new
// name is taken already goes, with its validator when that is not the new
// record's: its partial, unrecorded then, is swept once old (sweepPartials).
//
// The pairing is done only once every legacy record moved (or went): a
// listing or a record that could not be read, a record that could not be
// written at its new name or an old name that could not be removed leaves it
// for the next caller, which goes through the folder again. Nothing a pass
// did is done twice: a record moved is no longer under a legacy name, and one
// written at its new name whose old name stayed is that taken name, with the
// same validator, so only the old name goes.
func (s *service) migrateRecords(dir *os.Root, pairing string) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	if s.migrated[pairing] {
		return
	}
	entries, err := readDir(dir)
	if err != nil {
		return
	}
	done := true
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		c, err := readRecord(dir, entry.Name())
		if err != nil {
			// Gone since the listing, or no record at all (malformed, not a
			// regular file): nothing to move. Any other failure may hide one.
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errBadRecord) && !errors.Is(err, podcast.ErrInvalidFile) {
				done = false
			}
			continue
		}
		if c.ServerHash != pairing || !podcast.ValidEpisodePath(c.Path) || entry.Name() != legacyRecordName(c.LibraryID, c.Path) {
			continue
		}
		name := recordName(c.LibraryID, c.ShowID, c.Path)
		there, err := readRecord(dir, name)
		switch {
		case err == nil:
			if dir.Remove(entry.Name()) != nil {
				done = false
			} else if there.Validator != c.Validator {
				s.dropValidator(c.Validator)
			}
			continue
		case !errors.Is(err, os.ErrNotExist):
			done = false // not known to be free: never written over
			continue
		}
		if writeRecord(dir, name, c) != nil || dir.Remove(entry.Name()) != nil {
			done = false
		}
	}
	if done {
		s.migrated[pairing] = true
	}
}

// writeRecord writes download record c under name in the records folder dir.
func writeRecord(dir *os.Root, name string, c checkpoint) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return fsutil.WriteRootAtomic(dir, ".", name, b, 0o600, nil)
}

// dropValidator removes validator v's sealed ETag from the vault, when v
// names one.
func (s *service) dropValidator(v string) {
	if strings.HasPrefix(v, secrets.PodcastETagPrefix) {
		_ = s.o.Vault.Delete(v)
	}
}
func (s *service) saveCheckpoint(job jobs.Job, root *os.Root, c *checkpoint, etag string) error {
	dir, err := s.checkpointRoot()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if saved, ok := s.readCheckpoint(job, c.ShowID); ok && saved.Temp != c.Temp && !s.dropPartial(root, &saved) {
		// An earlier partial at this path the Bridge still cannot remove: its
		// record stays (clearCheckpoint tries it again, cleanOwned after a day),
		// and this download goes unrecorded rather than orphan it (left behind,
		// sweepPartials takes it once old). One removed since (or already gone)
		// no longer holds the record.
		return nil
	}
	if c.Validator == "" {
		c.Validator = secrets.PodcastETagPrefix + nonce()
	}
	// A whole download is proven by its hash, not the host's ETag (no GET
	// resumes it): etag is not asked for, and a validator the download did
	// not seal one under names none.
	if !c.complete() {
		if !netguard.StrongETag(etag) {
			return errors.New("unproven validator")
		}
		if err := s.o.Vault.Set(c.Validator, etag); err != nil {
			return err
		}
	}
	c.UpdatedAt = s.o.Now().UTC().Format(time.RFC3339)
	return writeRecord(dir, s.checkpointName(job, c.ShowID), *c)
}

// readCheckpoint is the job's saved record of show id show when it is this
// pairing's, for the job's library and path, naming an owned temp and a
// sealed validator.
func (s *service) readCheckpoint(job jobs.Job, show string) (checkpoint, bool) {
	dir, err := s.checkpointRoot()
	if err != nil {
		return checkpoint{}, false
	}
	defer func() { _ = dir.Close() }()
	return s.readOwnRecord(dir, *job.LibraryID, show, *job.Path)
}

// readOwnRecord is readCheckpoint's record of library lib's episode p of show
// id show, read from the open records folder dir.
func (s *service) readOwnRecord(dir *os.Root, lib int64, show, p string) (checkpoint, bool) {
	saved, err := readRecord(dir, recordName(lib, show, p))
	ok := err == nil && saved.ServerHash == s.pairHash() && saved.LibraryID == lib && saved.ShowID == show && saved.Path == p && podcast.OwnedTemp(saved.Temp) && strings.HasPrefix(saved.Validator, secrets.PodcastETagPrefix)
	return saved, ok
}

// errBadRecord is a download record no job can read back: over the size
// limit, or not a record's JSON.
var errBadRecord = errors.New("podcastjob: malformed download record")

// readRecord parses the download record named name, unchecked: errBadRecord
// when it is malformed, or the error opening or reading it.
func readRecord(dir *os.Root, name string) (checkpoint, error) {
	var c checkpoint
	rd, err := podcast.OpenRegular(dir, name)
	if err != nil {
		return c, err
	}
	defer rd.Close()
	b, err := io.ReadAll(io.LimitReader(rd, 16385))
	if err != nil {
		return checkpoint{}, err
	}
	if len(b) > 16384 || json.Unmarshal(b, &c) != nil {
		return checkpoint{}, errBadRecord
	}
	return c, nil
}

// records is this pairing's download records for library lib, in name order:
// each one under its own show's and path's name (recordName), naming an
// episode path and an owned partial. An error means the records could not be listed.
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
		c, err := readRecord(dir, entry.Name())
		if err == nil && c.ServerHash == pairing && c.LibraryID == lib && podcast.ValidEpisodePath(c.Path) && podcast.OwnedTemp(c.Temp) && entry.Name() == recordName(lib, c.ShowID, c.Path) {
			out = append(out, c)
		}
	}
	return out, nil
}

// recordsOf is the episode paths of a fetch's download records: the job's
// path, and wherever adoptCheckpoints moved a record with a show folder
// renamed by hand (the runner abandons a job by the path the server named,
// the old one). A moved record is this show's, for this episode, under the
// same file name. Two can be left: one at the job's path (a folder put back
// there) beside one moved away. The job's path when there is none.
func (s *service) recordsOf(job jobs.Job, p fetchParams) []string {
	var out []string
	if saved, ok := s.readCheckpoint(job, p.ShowID); ok && saved.EpisodeID == p.EpisodeID {
		out = append(out, *job.Path)
	}
	if dir, err := s.checkpointRoot(); err == nil {
		records, _ := s.records(dir, *job.LibraryID)
		_ = dir.Close()
		for _, c := range records {
			if c.ShowID == p.ShowID && c.EpisodeID == p.EpisodeID && c.Path != *job.Path && path.Base(c.Path) == path.Base(*job.Path) {
				out = append(out, c.Path)
			}
		}
	}
	if len(out) == 0 {
		out = []string{*job.Path}
	}
	return out
}

// dropStaleCheckpoint removes this path's owned partial when it was made for
// an episode the show's feed no longer has there (an episode replaced at the
// same path): its record is about to be overwritten, which would orphan it.
// The caller holds the path's file lock and passes the feed's current IDs.
func (s *service) dropStaleCheckpoint(job jobs.Job, root *os.Root, showID, episodeID string) {
	if saved, ok := s.readCheckpoint(job, showID); ok && saved.EpisodeID != episodeID {
		s.clearCheckpoint(job, root, &saved)
	}
}

// loadCheckpoint is the job's saved record of its episode, with the sealed
// ETag a resume sends, when its partial is still the one it records: a
// resumable prefix (Offset < Total, a strong ETag) or a whole download
// (complete). A record whose partial is proven gone or changed, or that the
// Bridge may not open (partialLost: not there, not a regular file, no
// permission, another size), is cleared, and a new record answered; a
// partial the clear cannot remove fails the attempt write_failed
// (errPartialStuck) rather than restart beside it, its record left for the
// next attempt or the day-old cleanup (cleanOwned, sweepRecords). One whose partial could not be opened to tell (a network
// share's hiccup) is kept, partial and all, with errPartialUnreadable: only a
// definite absence, mismatch or lasting fault may cost a download its bytes.
func (s *service) loadCheckpoint(job jobs.Job, root *os.Root, showID, episodeID string) (checkpoint, string, error) {
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: *job.LibraryID, ShowID: showID, EpisodeID: episodeID, Path: *job.Path}
	saved, ok := s.readCheckpoint(job, showID)
	if !ok || saved.EpisodeID != episodeID {
		return c, "", nil
	}
	tag, _ := s.o.Vault.Get(saved.Validator)
	size, err := int64(0), s.o.Fault.Hit("load")
	if err == nil {
		size, err = partialSize(root, saved.partialName())
	}
	if err != nil && !partialLost(err) {
		return saved, "", fmt.Errorf("%w: %w", errPartialUnreadable, err)
	}
	resumable := saved.Offset > 0 && saved.Offset < saved.Total && netguard.StrongETag(tag)
	if err == nil && size == saved.Offset && saved.Total <= s.o.MaxEpisodeBytes && (saved.complete() || resumable) {
		return saved, tag, nil
	}
	if !s.clearCheckpoint(job, root, &saved) {
		if err == nil {
			err = errPartialNotRecorded
		}
		return c, "", fmt.Errorf("%w: %w", errPartialStuck, err)
	}
	return c, "", nil
}

// errPartialNotRecorded is why loadCheckpoint clears a partial it could read:
// not the one its record names (another size), or one no GET may resume.
var errPartialNotRecorded = errors.New("the episode partial is not the one recorded")

// partialSize is the size of the partial at name, opened as OpenRegular
// opens it (so a partial the Bridge cannot read is not taken for one it can
// resume).
func partialSize(root *os.Root, name string) (int64, error) {
	f, err := podcast.OpenRegular(root, name)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// partialGone reports whether err, a partial's open or check, proves it gone
// or not the Bridge's file any more: not there (ENOENT, or ENOTDIR: its show
// folder is a file now), or not a regular file (a link, a folder, a file
// swapped in since its check).
func partialGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, podcast.ErrInvalidFile)
}

// partialLost reports whether err, a partial's open or check, means its bytes
// cannot be resumed: gone (partialGone), or a permission the Bridge lacks on
// its own file (EACCES, EPERM), a lasting fault as the server's share-fault
// rule has it, not a hiccup that passes. The partial is cleared and the
// episode downloaded from 0. Any other error (a share's hiccup) does not
// tell, and keeps it.
func partialLost(err error) bool {
	return partialGone(err) || errors.Is(err, os.ErrPermission)
}

// clearCheckpoint removes the owned partial, its sealed validator and the
// record of c's show at the job's path (only under the pairing that made
// them), then resets c to no partial. A partial it cannot remove keeps its
// validator and the record, for cleanOwned once it is a day old, and the
// answer is false; an earlier such partial the record still names is tried
// again first, so no later clear in the job drops its record. A record of another show at the path (one
// adoptCheckpoints moved there for a show folder renamed by hand to this
// job's folder name, an episode file name both shows use) is another record,
// named by its own show (recordName), and never read here.
func (s *service) clearCheckpoint(job jobs.Job, root *os.Root, c *checkpoint) bool {
	defer func() { c.Temp, c.Offset, c.Total, c.Validator, c.SHA256 = "", 0, 0, "", "" }()
	if c.ServerHash != s.pairHash() {
		return true
	}
	if !s.dropPartial(root, c) {
		return false
	}
	if saved, ok := s.readCheckpoint(job, c.ShowID); ok && saved.Temp != c.Temp && !s.dropPartial(root, &saved) {
		return false
	}
	if dir, err := s.checkpointRoot(); err == nil {
		_ = dir.Remove(s.checkpointName(job, c.ShowID))
		_ = dir.Close()
	}
	return true
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
// it finds the folder (Fetch's relocation; adoptOwn, under the episode's lock,
// past another open row of its episode) or drops it as it ends. A record of
// another show (names swapped by hand) stays too: that show's take-in moves
// it, and as records are named by show (recordName) neither is ever in the
// other's way. An error means the records
// could not be listed (or the open jobs read); one that could not be read or
// moved is left as it was.
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
	running := s.runningRecords(rows, lib.ID, self)
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
		moveRecord(dir, lib.ID, c, show+"/"+path.Base(c.Path))
	}
	return nil
}

// moveRecord moves download record c of library lib to episode path to: its
// record there first, then the old one removed. Nothing moves when a record
// of its show is at to already (one a download at the new path made, never
// overwritten) or the new one could not be written.
func moveRecord(dir *os.Root, lib int64, c checkpoint, to string) {
	old := recordName(lib, c.ShowID, c.Path)
	c.Path = to
	name := recordName(lib, c.ShowID, to)
	if _, err := dir.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if writeRecord(dir, name, c) != nil {
		return
	}
	_ = dir.Remove(old)
}

// adoptOwn takes the download record of a fetch's episode left at from, its
// path before a show folder renamed by hand (the episode's partial moved
// with the folder), to the job's path where the show holds the episode now.
// The fetch holds the episode's lock there, so no other download of it runs
// and the record is its own, though adoptCheckpoints leaves it to any open
// row naming it (a duplicate claim of the episode): left at from, the fetch
// would download again from byte 0 and the record would stay for a day. A
// record of this episode at the new path already wins (adoptCheckpoints'),
// and the one at from is cleared with its partial, rather than left; never
// live, the partial the fetch is writing (follow's), whose next checkpoint
// records it there (saveCheckpoint). Its partial is dropped for no other
// record there: a stale one (of another episode once at the path) or one
// that cannot be read leaves the fetch's own record and partial as they
// were, as does a records folder that cannot say whether one is there.
// (Another show's record at the path, names swapped by hand and an episode
// file name both shows use, is named by its own show and never in the way.)
// A fetch's start calls it again once the stale record at its path is
// cleared (dropStaleCheckpoint), so its own then moves in and the download
// resumes; its end calls it for a record still at from, as nothing else
// reads one under a folder that is gone so soon (sweepRecords waits a day)
// and its partial, a recorded one, would not be swept meanwhile.
func (s *service) adoptOwn(job jobs.Job, root *os.Root, from string, p fetchParams, live string) {
	lib, to := *job.LibraryID, *job.Path
	if from == to {
		return
	}
	dir, err := s.checkpointRoot()
	if err != nil {
		return
	}
	defer func() { _ = dir.Close() }()
	saved, ok := s.readOwnRecord(dir, lib, p.ShowID, from)
	if !ok || saved.EpisodeID != p.EpisodeID || path.Base(from) != path.Base(to) {
		return
	}
	if _, err := dir.Lstat(recordName(lib, p.ShowID, to)); errors.Is(err, os.ErrNotExist) {
		moveRecord(dir, lib, saved, to)
		return
	} else if err != nil {
		// Not known to be there: the record and its partial stay as they are
		// (the fetch's end tries again), rather than go for a records folder
		// that could not be read just now.
		return
	}
	there, ok := s.readOwnRecord(dir, lib, p.ShowID, to)
	saved.Path = to // its partial is in the episode's folder now
	switch {
	case ok && there.Temp == saved.Temp, live != "" && saved.Temp == live:
		// The partial stays: for the record at the new path naming it too (a
		// move that wrote there and could not remove this one), or for the
		// download writing it. Only the record goes.
	case ok && there.EpisodeID == p.EpisodeID:
		if !s.dropPartial(root, &saved) {
			return
		}
	default:
		// Not this episode's record there: nothing of the fetch's own goes
		// for it.
		return
	}
	_ = dir.Remove(recordName(lib, p.ShowID, from))
}

// runningRecords reports, by the open job rows, whether a download record of
// library lib is a running download's (self, the calling job, aside): a job
// of this Bridge claimed at its path, or a fetch of its episode (one that
// found the show elsewhere since its claim, Fetch's relocation, works at a
// path its row does not name).
func (s *service) runningRecords(rows []store.ServerJob, lib, self int64) func(checkpoint) bool {
	paths, episodes := map[string]bool{}, map[fetchParams]bool{}
	for _, r := range rows {
		if r.ID == self || r.State != store.JobRunning || r.ServerID != s.o.ServerID() || r.LibraryID == nil || *r.LibraryID != lib {
			continue
		}
		if r.Path != nil {
			paths[*r.Path] = true
		}
		var p fetchParams
		if r.Kind == FetchKind && json.Unmarshal(r.Params, &p) == nil {
			episodes[p] = true
		}
	}
	return func(c checkpoint) bool {
		return paths[c.Path] || episodes[fetchParams{c.ShowID, c.EpisodeID}]
	}
}

// dropPartial removes c's owned partial, then its sealed validator; false, with
// both kept, when the partial is still on disk or could not be checked to tell
// (partialGone: a share's hiccup proves nothing gone). A partial an install journal
// stages is the journal's (podcast.RecoverShow installs it): it stays on disk
// and only the record's claim on it goes.
func (s *service) dropPartial(root *os.Root, c *checkpoint) bool {
	if podcast.OwnedTemp(c.Temp) {
		p := c.partialName()
		// Checked, not opened: a partial the Bridge can no longer read (the
		// restart from zero clears one) is still its own to remove.
		st, err := podcast.LstatRegular(root, p)
		switch {
		case err != nil && !partialGone(err):
			return false
		case err == nil && !journalStages(root, c, st):
			if err := root.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false
			}
		}
	}
	s.dropValidator(c.Validator)
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
	l, err := s.lockEpisode(ctx, root, episode)
	if err != nil {
		return nil, err
	}
	return l.release, nil
}

// heldLock is an episode's .fetch.lock a job holds (lockEpisode).
type heldLock struct {
	root *os.Root
	p    string
	f    *os.File
}

func (l *heldLock) release() { releaseLock(l.root, l.p, l.f) }

// held reports whether the file at l's path is still the one held: a show
// folder deleted and put back from a copy has a copy of the lock file there.
func (l *heldLock) held() bool { return lockedAt(l.root, l.p, l.f) }

// follow points l at episode's lock when the file there is the one held: a
// show folder renamed by hand took the lock with it, and letting go removes
// it there rather than leave it in the folder. false when the file there is
// not the one held (followLock).
func (l *heldLock) follow(episode string) bool {
	p := podcast.AttachmentPath(episode, ".fetch.lock")
	if !lockedAt(l.root, p, l.f) {
		return false
	}
	l.p = p
	return true
}

// followLock moves l, the lock a fetch holds on its episode, to episode, the
// path a show folder renamed by hand has it at now: the lock file moved with
// the folder (follow), or, when the file there is not the one held (a folder
// "renamed" by a copy and a delete, as a move across filesystems or a file
// manager's is, or a lock file removed by hand), the lock there is taken
// anew and the old one let go. Without it the fetch would go on, checkpoint,
// clear and publish at the new path holding no lock there, beside a second
// fetch of the episode or an Abandon clearing the partial it writes. It waits
// one ceiling for a holder there (another download of the episode, which this
// one would only repeat), then fails write_failed; l is unchanged on an error
// (ctx ended, or the lock could not be taken).
//
// A re-take never makes the episode's .audiosilo folder, and at the path it
// holds already (a folder put back there from a copy) never the lock file
// either: the caller saw the file there, so one gone now is a folder being
// deleted (an rm -rf would find the folder made again and fail to remove the
// show folder). Nothing is made there and l is left as it is. At a new path
// with no .audiosilo folder (a show moved without its hidden folders, or a
// folder being deleted) nothing is made either, and the follow fails
// write_failed with l unchanged: the fetch would otherwise go on there
// holding no lock.
func (s *service) followLock(ctx context.Context, l *heldLock, episode string) error {
	if l.follow(episode) {
		return nil
	}
	wait, stop := context.WithTimeout(ctx, ceiling)
	defer stop()
	moved := podcast.AttachmentPath(episode, ".fetch.lock") != l.p
	next, err := s.takeLock(wait, l.root, episode, moved)
	switch {
	case err == nil:
		l.release()
		*l = *next
		return nil
	case errors.Is(err, os.ErrNotExist) && !moved:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case l.follow(episode):
		// The file there was the one held after all (a check the library
		// failed just then): waited on, it could never be taken.
		return nil
	case wait.Err() != nil, errors.Is(err, os.ErrNotExist):
		return fail("write_failed", "The episode could not be locked where its show folder is now.")
	}
	return err
}

// lockEpisode takes episode's .fetch.lock (fileLock), as a heldLock a fetch
// can follow to a show folder renamed by hand.
func (s *service) lockEpisode(ctx context.Context, root *os.Root, episode string) (*heldLock, error) {
	if err := podcast.EnsureDir(root, path.Dir(episode)+"/.audiosilo"); err != nil {
		return nil, err
	}
	return s.takeLock(ctx, root, episode, true)
}

// takeLock takes episode's .fetch.lock in its .audiosilo folder, making the
// file only when create says so (a re-take, followLock, never makes the
// folder): os.ErrNotExist when the file (or the folder) is not there.
func (s *service) takeLock(ctx context.Context, root *os.Root, episode string, create bool) (*heldLock, error) {
	p := podcast.AttachmentPath(episode, ".fetch.lock")
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	if st, err := root.Lstat(p); err == nil && !st.Mode().IsRegular() {
		return nil, podcast.ErrInvalidFile
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raced := 0
	for {
		f, err := root.OpenFile(p, flags, 0o600)
		if errors.Is(err, os.ErrNotExist) && create && raced < 100 && folderThere(root, path.Dir(p)) {
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
				return &heldLock{root, p, f}, nil
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
		// A wait that does not watch ctx itself still ends with it.
		if err := ctx.Err(); err != nil {
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
// Only the held file is removed: one at p since (its show folder renamed by
// hand, the lock moving with it, and the name taken again) is another job's.
func releaseLock(root *os.Root, p string, f *os.File) {
	if lockedAt(root, p, f) {
		_ = root.Remove(p)
	}
	unlockFile(f)
	_ = f.Close()
}

// folderThere reports whether dir is there (a link or a file too).
func folderThere(root *os.Root, dir string) bool {
	_, err := root.Lstat(dir)
	return !errors.Is(err, os.ErrNotExist)
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
