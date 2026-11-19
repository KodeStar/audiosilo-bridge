package podcastjob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

const FetchKind = "podcast.fetch"
const RetainKind = "podcast.retain"

type Streamer interface {
	Stream(context.Context, netguard.StreamRequest) (*netguard.StreamResponse, error)
}
type Fetch struct{ s *service }

func (h *Subscribe) FetchJob() *Fetch { return &Fetch{s: h.s} }
func (h *Fetch) Kind() string         { return FetchKind }
func (h *Fetch) Lane() string         { return jobs.LaneDownload }
func (h *Fetch) Interactive() bool    { return true }

type fetchParams struct {
	ShowID    string `json:"show_id"`
	EpisodeID string `json:"episode_id"`
}
type fetchResult struct {
	LibraryID int64         `json:"library_id"`
	Path      string        `json:"path"`
	ShowID    string        `json:"show_id"`
	EpisodeID string        `json:"episode_id"`
	Fetch     podcast.Fetch `json:"fetch"`
	Chapters  int           `json:"chapters"`
	Reused    bool          `json:"reused"`
}

// EpisodeCeiling uses overflow-safe division; all admitted sizes are <=4096 MiB.
func EpisodeCeiling(bytes int64) time.Duration {
	if bytes < 0 || bytes > 4096<<20 {
		bytes = 4096 << 20
	}
	return 5*time.Minute + time.Duration(3*((bytes+262143)/262144)+6)*time.Second
}

type Timer interface {
	Reset(time.Duration) bool
	Stop() bool
}
type budget struct {
	start, deadline time.Time
	now             func() time.Time
	timer           Timer
	cancel          context.CancelCauseFunc
}

// errOverBudget is the cause of a download stopped at its ceiling: the episode
// host's slowness, reported as timeout, not as the Bridge stopping (bridge_lost).
var errOverBudget = errors.New("the episode download ran past its ceiling")

// errPartialUnreadable is loadCheckpoint's, download's and consume's when the
// proven partial a resume appends to could not be opened or read back to
// tell whether it is still the one recorded (a network share's hiccup): it is
// kept, record and all, and the attempt fails or is tried again, never
// restarted from zero. A permission the Bridge lacks is not one
// (partialLost).
var errPartialUnreadable = errors.New("the episode partial could not be read")

// errPartialChanged is a partial proven no longer the one a resume appends
// to, or one the Bridge may no longer open (partialLost): gone, not a
// regular file, no permission, another size, or shorter than its prefix when
// read back. download starts the episode again from zero, once.
var errPartialChanged = errors.New("the episode partial changed on disk")

// errPartialStuck is a partial that must be cleared for the episode to start
// again from zero (partialLost, another size, another enclosure) but that the
// clear could not remove (a show folder the Bridge may not write): the
// attempt fails write_failed rather than restart beside it, and the record
// stays for the next attempt or the day-old cleanup (cleanOwned,
// sweepRecords).
var errPartialStuck = errors.New("the episode partial could not be removed")

func (s *service) budget(parent context.Context) (context.Context, *budget) {
	ctx, cancel := context.WithCancelCause(parent)
	start := s.o.BudgetNow()
	d := EpisodeCeiling(s.o.MaxEpisodeBytes)
	b := &budget{start: start, deadline: start.Add(d), now: s.o.BudgetNow, cancel: cancel}
	b.timer = s.o.AfterFunc(d, b.expire)
	return ctx, b
}
func (b *budget) expire() { b.cancel(errOverBudget) }
func (b *budget) tighten(total int64) {
	d := b.start.Add(EpisodeCeiling(total))
	if d.Before(b.deadline) {
		b.deadline = d
		left := d.Sub(b.now())
		if left <= 0 {
			b.expire()
		} else {
			b.timer.Reset(left)
		}
	}
}
func (b *budget) close() { b.timer.Stop(); b.cancel(nil) }

// overBudget reports whether ctx (a budget's) was stopped by its ceiling.
func overBudget(ctx context.Context) bool { return errors.Is(context.Cause(ctx), errOverBudget) }

// failure is err as the job reports it: a stop the ceiling made (ctx is the
// budget's) is the episode's timeout wherever it surfaced, a request it ended
// too (netguard's own timeout, which words it as the feed's).
func (b *budget) failure(ctx context.Context, err error) error {
	var guard *netguard.Error
	stopped := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &guard) && guard.Code == "timeout"
	if stopped && overBudget(ctx) {
		return fail("timeout", "The episode download did not finish in time.")
	}
	return err
}

func (h *Fetch) Run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	kept := ""
	result, err := (&Fetch{s: h.s.forJob(job)}).run(parent, &job, rep, &kept)
	if err != nil && (parent.Err() == nil || !jobs.Resumes(parent)) {
		h.abandon(parent, job, kept)
	}
	return outcome(result, err)
}

// run fetches claimed's episode, moving claimed.Path to where the episode is
// now when its show folder was renamed by hand (episodeAt), before the
// download or while it ran, so Abandon clears the record there. kept is set
// to the partial a failure keeps recorded for the next attempt (a whole
// download whose read-back failed, or a partial that could not be read to
// tell whether it changed), which Abandon then leaves.
func (h *Fetch) run(parent context.Context, claimed *jobs.Job, rep *jobs.Reporter, kept *string) (_ any, err error) {
	s := h.s
	job := *claimed
	var params fetchParams
	if job.Path == nil || !podcast.ValidEpisodePath(*job.Path) || json.Unmarshal(job.Params, &params) != nil || !podcast.ValidID(params.ShowID) || !podcast.ValidID(params.EpisodeID) {
		return nil, fail("episode_not_found", "The podcast episode was not found.")
	}
	waitCtx, cancel := context.WithTimeout(parent, ceiling)
	defer cancel()
	lib, mount, err := s.library(waitCtx, job, rep)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	// Read the show before creating lock artifacts, so invalid paths write
	// nothing; one the server still names by a show folder renamed by hand is
	// fetched where it is now. It is read again even at the same path:
	// episodeAt may have taken the folder in there (renamed back by hand after
	// a take-in elsewhere, while the server still names it).
	claimedAt := *job.Path
	// The folder's name as it is spelt on disk: on a case-insensitive
	// filesystem a show folder renamed by hand only in case still opens under
	// the server's spelling, and the locks below would be keyed apart from a
	// job naming the new one (realPath).
	if p := realPath(root, *job.Path); p != *job.Path {
		job.Path, claimed.Path = &p, &p
	}
	if _, _, err = s.readEpisode(root, *job.Path, params); err != nil {
		var p string
		if p, err = s.episodeAt(waitCtx, root, lib, mount, *job.Path, params, job.ID); err != nil {
			return nil, err
		}
		if p != *job.Path {
			job.Path, claimed.Path = &p, &p
		}
		if _, _, err = s.readEpisode(root, *job.Path, params); err != nil {
			return nil, err
		}
	}
	unlock, err := s.acquire(parent, s.o.ServerID()+"|fetch|"+mount+"/"+*job.Path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.beforeWrite(lib, mount, path.Dir(*job.Path)); err != nil {
		return nil, err
	}
	lock, err := s.lockEpisode(parent, root, *job.Path)
	if err != nil {
		return nil, err
	}
	// settled: the exit below followed the show already (settle), the lock with
	// it, so letting go does not search the library for the show a second time.
	settled := false
	defer func() {
		// A show folder renamed by hand since the lock was taken took its file
		// along, and a failure before the download (which follows the show)
		// leaves it there: it is let go where the show is now, its file
		// removed, rather than left in the folder.
		if !settled && !lock.held() && parent.Err() == nil {
			sctx, stop := context.WithTimeout(parent, ceiling)
			if p, err := s.episodeAt(sctx, root, lib, mount, *job.Path, params, job.ID); err == nil {
				lock.follow(p)
			}
			stop()
		}
		lock.release()
	}()
	// Found elsewhere: its record, under the episode's lock now, is taken
	// along even when another open row names it (adoptOwn).
	s.adoptOwn(job, root, claimedAt, params, "")
	pairing := s.pairHash()
	f, e, err := s.fetchSnapshot(parent, root, lib, mount, *job.Path, params)
	if err != nil {
		return nil, err
	}
	if e.Fetch.Valid() && e.Fetch.DeletedAt == "" && podcast.Matches(root, e.Path, e.Fetch) {
		return s.reuse(parent, root, lib, e, f.ShowID)
	}
	if _, err := root.Lstat(e.Path); !errors.Is(err, os.ErrNotExist) {
		return nil, fail("write_failed", "An unverified episode file needs administrator repair.")
	}
	// An invalid saved Fetch grants no new generation (NextFetch refuses it): say
	// so before any enclosure GET rather than after a whole download.
	if e.Fetch != nil && !e.Fetch.Valid() {
		return nil, fail("write_failed", "The saved episode record needs administrator repair.")
	}
	ctx, b := s.budget(parent)
	defer b.close()
	defer func() { err = b.failure(ctx, err) }()
	// Another episode's partial at the path that cannot be removed fails the
	// attempt as the fetch's own would (errPartialStuck): never a download
	// beside it, unrecorded.
	if err := s.dropStaleCheckpoint(job, root, f.ShowID, e.ID); err != nil {
		return nil, err
	}
	// Again now that a stale record at the new path (another episode's) is
	// gone: the fetch's own, left at the old path for it, moves in and the
	// download resumes from its partial.
	s.adoptOwn(job, root, claimedAt, params, "")
	c, tag, err := s.loadCheckpoint(job, root, f.ShowID, e.ID)
	if errors.Is(err, errPartialStuck) {
		return nil, stuckPartial(c.partialName(), err)
	}
	if err != nil {
		// Not known to have changed: the partial and its record stay for the
		// next attempt (a share's hiccup passes), and no GET is spent.
		*kept = c.Temp
		return nil, keptPartial(&c, err)
	}
	// follow moves the fetch to its episode where a show folder renamed by
	// hand has it now (episodeAt), taking the episode's lock and its own
	// record along (adoptOwn, never dropping the partial it writes): the
	// partial moved with the folder. The lock first (followLock): a fetch
	// that cannot hold it there stays where it was, and fails.
	// At the same path, a folder put back there from a copy has a copy of the
	// lock file, not the one held: the lock there is taken anew (followLock).
	// No lock file there (a folder being deleted) is left so, never made again.
	follow := func(ctx context.Context) error {
		p, err := s.episodeAt(ctx, root, lib, mount, e.Path, params, job.ID)
		if err != nil {
			return err
		}
		if p == e.Path {
			if _, err := root.Lstat(podcast.AttachmentPath(p, ".fetch.lock")); err != nil || lock.follow(p) {
				return nil
			}
			// episodeAt says p too when no folder holds the show: a lock there
			// in another show's folder (an episode file name both use) is
			// never taken.
			if f, err := podcast.Read(root, path.Dir(p)); err != nil || f.ShowID != params.ShowID {
				return nil
			}
			return s.followLock(ctx, lock, p)
		}
		if err := s.followLock(ctx, lock, p); err != nil {
			return err
		}
		from := e.Path
		e.Path, c.Path = p, p
		job.Path, claimed.Path = &p, &p
		s.adoptOwn(job, root, from, params, c.Temp)
		return nil
	}
	// settle follows the fetch (follow) when its partial is no longer at its
	// path, so a host's retry goes on from the partial where it is now rather
	// than from byte 0, and a clear removes it there rather than drop its
	// record and leave it for the day-old sweep. With no partial yet (a retry
	// that cleared it) it always asks follow, which reads the episode's
	// feed.json once: the next attempt makes its partial where the show is now,
	// not in a folder that is gone or that another folder took the name of
	// meanwhile (a new one, or names swapped by hand). So it does when the lock
	// file at the path is not the one held: a folder deleted and put back from
	// a copy between two attempts holds a copy of the lock (and of the
	// partial), and the next attempt would append under a lock it does not
	// hold; follow takes the lock there anew (followLock).
	settle := func(ctx context.Context) error {
		if c.Temp != "" && lock.held() {
			if _, err := root.Lstat(c.partialName()); !errors.Is(err, os.ErrNotExist) {
				return nil
			}
		}
		return follow(ctx)
	}
	committed := false
	// unresolved: the episode's enclosure could not be resolved for a failure
	// that may pass (resolve), before any GET of this attempt touched the
	// partial it loaded.
	unresolved := false
	defer func() {
		if s.writable(lib, mount) != nil || pairing != s.pairHash() {
			return
		}
		keep := false
		switch {
		case committed || c.Temp == "" || parent.Err() != nil && !jobs.Resumes(parent):
			// Published, nothing held, or cancelled (its claim lost): cleared.
		case c.complete(), errors.Is(err, errPartialUnreadable), unresolved:
			// A whole download whose read-back failed (resume-at-total), a
			// partial that could not be read to tell whether it changed, or
			// one whose enclosure a private feed that did not answer just now
			// could not give (unresolved): kept for the next attempt, which
			// verifies it (or resumes it) where its record is now. One proven
			// gone or another size goes.
			keep = s.keepsPartial(job, root, &c)
		case parent.Err() != nil:
			// Read, not loaded: loadCheckpoint drops a record whose partial is
			// not at its path, and a show folder renamed by hand mid-download
			// took the partial with it. The resumed job finds the show there
			// (episodeAt, its record following) and goes on from the partial.
			if saved, ok := s.readCheckpoint(job, params.ShowID); ok && saved.EpisodeID == params.EpisodeID {
				validator, _ := s.o.Vault.Get(saved.Validator)
				keep = saved.Temp == c.Temp && saved.Offset == c.Offset && saved.Total == c.Total && netguard.StrongETag(validator)
			}
		}
		if keep {
			*kept = c.Temp
		} else {
			if !committed && parent.Err() == nil {
				// Bounded as the fetch's first search for its show is: the
				// job has failed already (its own ceiling may be what ended
				// it), and a show lock another job holds, or a library slow
				// to read, must not keep its slot and locks for good.
				sctx, stop := context.WithTimeout(parent, ceiling)
				_ = settle(sctx)
				stop()
				settled = true
			}
			s.clearCheckpoint(job, root, &c)
		}
		// Its own record still at the path it was claimed at (the start could
		// not take it past a record it could not read at the new path): it
		// follows now, for a retry to resume or the day-old cleanup to clear
		// where the show is, rather than stay under a folder that is gone.
		s.adoptOwn(job, root, claimedAt, params, c.Temp)
	}()
	rep.Progress(jobs.Progress{Step: "resolving", Message: "Resolving the episode"})
	// The episode's enclosure, resolved once per job (a private feed's is
	// fetched): before the loop for a kept partial its record names the
	// source of, else when a download needs it. A failure that may pass (the
	// private feed's fetch: DNS, the network, a timeout, a 429 or 5xx) says
	// nothing of the partial the attempt loaded, which the exit keeps for the
	// next attempt to resume (unresolved); any other (the feed gone, no
	// enclosure for the episode in it) leaves nothing to resume it from.
	var enc podcast.Enclosure
	resolve := func() error {
		if enc.URL != "" {
			return nil
		}
		got, err := s.enclosure(ctx, f, e)
		enc = got
		var failure *netguard.Error
		unresolved = errors.As(err, &failure) && netguard.Retryable(failure)
		return err
	}
	if c.Temp != "" && c.Source != "" {
		if err := resolve(); err != nil {
			return nil, err // a whole download is kept (the exit) for the next attempt, and a partial when the failure may pass
		}
		// The feed changed the episode's audio under the same episode id
		// since it was downloaded (another enclosure URL or length): the old
		// download is never published, nor a partial resumed against
		// another URL with its old ETag, and the episode is downloaded again
		// from byte 0 (the loop's one GET).
		if !c.resumes(enc) {
			slog.Info("podcasts: an episode's enclosure changed since its partial was kept; it is downloaded again", "partial", c.partialName())
			_ = settle(ctx) // the clear finds the partial where it is now (a private feed's fetch may take a while)
			if err := s.restartPartial(job, root, &c, errors.New("the episode's enclosure changed")); err != nil {
				return nil, err
			}
			tag = ""
		}
	}
	var received podcast.Fetch
	for again := false; ; again = true {
		if c.complete() {
			// Kept whole by an attempt whose read-back failed: verified below
			// against the hash it was written with, and published with no GET.
			// One proven changed is downloaded again from byte 0, once.
			received = podcast.Fetch{Bytes: c.Offset, SHA256: c.SHA256}
		} else if err = resolve(); err != nil {
			return nil, err
		} else if received, err = s.download(ctx, b, &job, root, enc, &c, tag, settle, follow, rep); err != nil {
			return nil, err
		}
		// A show folder renamed by hand while the episode downloaded took the
		// partial and the episode's lock with it: the episode is published where
		// the show is now, its record following it, never downloaded again.
		//
		// A copy of the show folder made over it in place (cp into the same
		// folder) rewrites the partial under the file the download holds open:
		// the same file, so nothing above sees it, with other bytes. The
		// partial is hashed again before the probe and the publication read it,
		// and one proven not to be what was written is downloaded again from
		// byte 0 (once), rather than probed as broken audio or refused at
		// publication (written, which follows the show first).
		held, err := s.written(ctx, root, &c, received, follow)
		unread := errors.Is(err, errPartialUnread)
		if err != nil && !unread {
			return nil, err
		}
		if held {
			break
		}
		// Only where the show still is: a show folder deleted outright (follow
		// found it nowhere) has no partial to hash either, and a second
		// download would be written into whatever is at its name now, another
		// show's folder taking that name included. It fails as the publication
		// would.
		if _, _, err := s.readEpisode(root, e.Path, params); err != nil {
			return nil, err
		}
		// A partial that could not be read back is not known to have changed:
		// the whole download is never thrown away for a read that failed. Its
		// record says it is whole (resume-at-total), with the hash it was
		// written with, so the exit keeps it and the next attempt verifies and
		// publishes it with no GET.
		// One kept so already keeps its record as it is: saved again, its
		// age would start over at each attempt, and a partial that is never
		// read back (a lasting fault) would never be a day old for cleanOwned,
		// failing every attempt with no download for good.
		if unread {
			slog.Warn("podcasts: an episode's download could not be read back to check it; it is kept for the next attempt", "partial", c.partialName(), "err", err)
			if !c.complete() {
				c.Total, c.SHA256 = c.Offset, received.SHA256
				if err := s.saveCheckpoint(job, root, &c, ""); err != nil {
					c.SHA256 = "" // unrecorded: the exit clears it
				}
			}
			return nil, fail("write_failed", "The episode download could not be read back.")
		}
		if again {
			return nil, fail("write_failed", "The episode download was changed on disk while it was saved.")
		}
		slog.Warn("podcasts: an episode's partial changed on disk while it downloaded; it is downloaded again", "partial", c.partialName())
		if err := s.restartPartial(job, root, &c, errPartialChanged); err != nil {
			return nil, err
		}
		tag = ""
	}
	rep.Progress(jobs.Progress{Step: "probing", Message: "Checking episode audio"})
	rd, err := podcast.OpenRegular(root, c.partialName())
	if err != nil {
		return nil, err
	}
	probe, err := s.o.Probe(ctx, rd)
	if err != nil {
		_ = rd.Close()
		return nil, err
	}
	if probe.DurationMS <= 0 || probe.Codec == "" || probe.ContentType == "" {
		_ = rd.Close()
		c.SHA256 = "" // a whole download kept before is not kept again: no attempt publishes it
		return nil, fail("invalid_audio", "The episode is not supported audio.")
	}
	received.DurationMS = probe.DurationMS
	received.Codec = probe.Codec
	received.ContentType = probe.ContentType
	received.FetchedAt = s.o.Now().UTC().Format(time.RFC3339)
	received, err = podcast.NextFetch(e.Fetch, received)
	if err != nil {
		_ = rd.Close()
		return nil, err
	}
	_, err = rd.Seek(0, io.SeekStart)
	if err != nil {
		_ = rd.Close()
		return nil, err
	}
	chapters := podcast.ReadCHAP(rd, e.Path, received)
	_ = rd.Close()
	rep.Progress(jobs.Progress{Step: "writing", Message: "Saving the episode"})
	// A case-only rename by hand since the follow above: the show lock is
	// taken under the folder's spelling on disk, as every other job takes it.
	if realPath(root, e.Path) != e.Path {
		if err := follow(ctx); err != nil {
			return nil, err
		}
	}
	unlockShow, err := s.acquire(ctx, s.showKey(mount, path.Dir(e.Path)))
	if err != nil {
		return nil, err
	}
	err = func() error {
		defer unlockShow()
		if err := rep.ConfirmLease(ctx); err != nil {
			if overBudget(ctx) {
				return ctx.Err() // the ceiling passed: the episode's timeout (b.failure), not a lost claim
			}
			return leaseFailure(ctx, err, "The episode job no longer owns its claim.")
		}
		if pairing != s.pairHash() {
			return fail("identity_changed", "The Bridge pairing changed.")
		}
		if err := s.beforeWrite(lib, mount, path.Dir(e.Path)); err != nil {
			return err
		}
		if err := podcast.RecoverShow(root, path.Dir(e.Path), s.o.Now(), s.o.Fault); err != nil {
			return err
		}
		feed, latest, err := s.readEpisode(root, e.Path, params)
		if err != nil {
			return err
		}
		if !sameFetch(latest.Fetch, e.Fetch) {
			return fail("identity_changed", "The episode generation changed.")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j := podcast.Journal{Version: 1, Op: "install", ShowID: params.ShowID, EpisodeID: e.ID, Path: e.Path, AudioTemp: c.Temp, Fetch: received, Chapters: chapters}
		// Once a journal exists recovery owns the candidate, including on a failed
		// write: the exit's clear drops the record but leaves the file
		// (dropPartial), for RecoverShow to install.
		err = podcast.Publish(root, feed, j, s.o.Now(), s.o.Fault)
		if _, e := root.Lstat(podcast.AttachmentPath(j.Path, ".publish.json")); e == nil {
			c.Temp = ""
		}
		return err
	}()
	if err != nil {
		return nil, err
	}
	committed = true
	s.setIndexed(lib.ID, path.Dir(e.Path), false)
	rep.Progress(jobs.Progress{Step: "indexing", Message: "Indexing the episode"})
	if err := s.episodeRescan(ctx, lib.ID, e.Path); err != nil {
		return nil, err
	}
	n := 0
	if chapters != nil {
		n = len(chapters.Chapters)
	}
	return fetchResult{lib.ID, e.Path, f.ShowID, e.ID, received, n, false}, nil
}

// leaseFailure words a refused publication check. A lost, cancelled or refused
// claim no longer owns the job; a server that could not be reached denies the
// commit too, but as a retryable failure rather than a changed identity:
// bridge_lost (the Bridge and its server lost each other), never network, which
// the server reads as the episode's host or the feed's being out of reach.
func leaseFailure(ctx context.Context, err error, lost string) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, link.ErrNotHeld) || errors.Is(err, link.ErrUnauthorized) {
		return fail("identity_changed", lost)
	}
	return fail("bridge_lost", "The server could not confirm the job before saving.")
}

// episodeAt is where a fetch's episode p is now. The server names p's folder
// until its rescan, so after a rename by hand that folder is gone, holds
// another show or cannot be read: the episode is then the same file name in
// the folder holding the show (lockShow, which takes it in when renamed), and
// the records of the show's interrupted downloads still under the old folder,
// this job's own included, follow it (adoptCheckpoints). p when its folder
// holds the show (the episode itself is missing, or lockShow just took the
// folder in there) or none does. write_failed when the records cannot be
// read: the fetch waits, as a take-in does (adopt), rather than leave its
// record and partial behind. ctx's error when it ended while the show lock
// was awaited. Before a download its show lock is taken and released before
// the fetch's own locks; during one (settle) and after it, under them (the
// episode's lock before the show's, as the publication takes them), the
// episode's lock then following to the new path (followLock).
func (s *service) episodeAt(ctx context.Context, root *os.Root, lib link.Library, mount, p string, params fetchParams, self int64) (string, error) {
	from := path.Dir(p)
	// Only under its spelling on disk: on a case-insensitive filesystem the
	// old spelling of a folder renamed by hand only in case still reads its
	// feed.json (not taken in yet), and the fetch would publish under a show
	// lock keyed apart from every other job's (lockShow, realPath).
	if f, err := podcast.Read(root, from); err == nil && f.ShowID == params.ShowID {
		if real, err := podcast.RealName(root, from); err != nil || real == from {
			return p, nil
		}
	}
	show, _, unlock, err := s.lockShow(ctx, root, lib, mount, from, params.ShowID, nil)
	if err != nil {
		if ctx.Err() != nil {
			return p, ctx.Err() // a stop or the ceiling, not a show that is gone
		}
		return p, nil
	}
	defer unlock()
	if show == from {
		return p, nil
	}
	if err := s.adoptCheckpoints(ctx, root, lib, show, from, params.ShowID, self); err != nil {
		slog.Warn("podcasts: the download records of a show folder renamed by hand could not be read; the fetch waits", "library", lib.ID, "show", show, "err", err)
		return p, fail("write_failed", "The podcast download records could not be read.")
	}
	return show + "/" + path.Base(p), nil
}

// realPath is episode p with its show folder spelt as it is on disk
// (podcast.RealName): p when it is, or its folder is not there.
func realPath(root *os.Root, p string) string {
	show, err := podcast.RealName(root, path.Dir(p))
	if err != nil {
		return p
	}
	return show + "/" + path.Base(p)
}

// keepsPartial reports whether a failed fetch's exit keeps c's partial and
// record for the next attempt: the record at the job's path still names c's
// partial (a whole one with c's hash), and the partial is not proven gone,
// barred to the Bridge or another size (a check that cannot tell keeps it,
// partialLost).
func (s *service) keepsPartial(job jobs.Job, root *os.Root, c *checkpoint) bool {
	saved, ok := s.readCheckpoint(job, c.ShowID)
	if !ok || saved.EpisodeID != c.EpisodeID || saved.Temp != c.Temp || saved.SHA256 != c.SHA256 {
		return false
	}
	st, err := podcast.LstatRegular(root, c.partialName())
	if err != nil {
		return !partialLost(err)
	}
	return st.Size() == saved.Offset
}

// rehashTries and rehashWait bound how long a download that cannot be read
// back is asked again (written): four reads, 1 s, 2 s and 4 s apart.
const (
	rehashTries = 4
	rehashWait  = time.Second
)

// errPartialUnread is written's answer for a partial it could not read back
// to tell whether it holds what the download wrote.
var errPartialUnread = errors.New("podcastjob: the episode download could not be read back")

// written reports whether the partial c names holds what the download wrote
// (received: its size and hash), where its show folder is now (follow, run's,
// first). false with no error only for a partial proven to hold other content
// (podcast.Verify). One that could not be read to tell (a read the library
// failed for a moment, as a network share does, or a show folder renamed by
// hand between the follow and the read, its partial gone from the path) is
// not a changed one: the show is followed and the partial read again, up to
// rehashTries times, then errPartialUnread (wrapping the read's error).
// follow's error, or ctx's when it ended in a wait.
func (s *service) written(ctx context.Context, root *os.Root, c *checkpoint, received podcast.Fetch, follow func(context.Context) error) (bool, error) {
	for try := 1; ; try++ {
		if err := follow(ctx); err != nil {
			return false, err
		}
		err := s.o.Fault.Hit("rehash")
		if err == nil {
			var ok bool
			if ok, err = podcast.Verify(root, c.partialName(), received.Bytes, received.SHA256); err == nil {
				return ok, nil
			}
		}
		if try == rehashTries {
			return false, fmt.Errorf("%w: %w", errPartialUnread, err)
		}
		if err := s.o.Wait(ctx, rehashWait<<(try-1)); err != nil {
			return false, err
		}
	}
}

func (s *service) showKey(mount, show string) string {
	return s.o.ServerID() + "|" + mount + "/" + show
}
func (s *service) readEpisode(root *os.Root, p string, params fetchParams) (*podcast.Feed, podcast.Episode, error) {
	f, err := podcast.Read(root, path.Dir(p))
	if err != nil {
		return nil, podcast.Episode{}, fail("episode_not_found", "The podcast episode was not found.")
	}
	if f.ShowID != params.ShowID {
		return nil, podcast.Episode{}, fail("identity_changed", "The podcast show identity changed.")
	}
	for _, e := range f.Episodes {
		if e.Path == p {
			if e.ID != params.EpisodeID {
				return nil, e, fail("identity_changed", "The podcast episode identity changed.")
			}
			return f, e, nil
		}
	}
	return nil, podcast.Episode{}, fail("episode_not_found", "The podcast episode was not found.")
}
func (s *service) fetchSnapshot(ctx context.Context, root *os.Root, lib link.Library, mount, p string, params fetchParams) (*podcast.Feed, podcast.Episode, error) {
	unlock, err := s.acquire(ctx, s.showKey(mount, path.Dir(p)))
	if err != nil {
		return nil, podcast.Episode{}, err
	}
	defer unlock()
	if err := s.beforeWrite(lib, mount, path.Dir(p)); err != nil {
		return nil, podcast.Episode{}, err
	}
	if err := podcast.RecoverShow(root, path.Dir(p), s.o.Now(), s.o.Fault); err != nil {
		return nil, podcast.Episode{}, err
	}
	return s.readEpisode(root, p, params)
}
func sameFetch(a, b *podcast.Fetch) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func (s *service) reuse(ctx context.Context, root *os.Root, lib link.Library, e podcast.Episode, showID string) (any, error) {
	if err := s.episodeRescan(ctx, lib.ID, e.Path); err != nil {
		return nil, err
	}
	n := 0
	// Chapters are counted only from the owned attachment matching this pin, never publisher tags.
	if list := podcast.ReadChapters(root, e.Path, e.Fetch); list != nil {
		n = len(list.Chapters)
	}
	return fetchResult{lib.ID, e.Path, showID, e.ID, *e.Fetch, n, true}, nil
}

// rescanTries, rescanMaxWait and rescanBackoff bound how long a rescan waits
// out the server's 429s (askRescan).
const (
	rescanTries   = 3
	rescanMaxWait = 30 * time.Second
	rescanBackoff = 2 * time.Second
)

var rescanLimit = link.Backoff{Tries: rescanTries, First: rescanBackoff, Max: rescanMaxWait}

// askRescan asks the server to index target (a show folder or an episode),
// waiting out its 429s as audiblejob's enrichment does (rescanLimit): the
// server's Retry-After (an explicit 0 asks again at once, none waits
// rescanBackoff, doubling), rescanTries asks in all, each wait at most
// rescanMaxWait. The last answer's error: still a 429 (link.RateLimited)
// past those bounds or after a stop meanwhile.
func (s *service) askRescan(ctx context.Context, srv Server, id int64, target string) error {
	for limited := 1; ; limited++ {
		_, err := srv.RescanPodcast(ctx, id, target)
		busy, ok := link.RateLimited(err)
		if !ok {
			return err
		}
		wait, ok := rescanLimit.Wait(busy, limited)
		if !ok || ctx.Err() != nil || s.o.Wait(ctx, wait) != nil {
			return err
		}
	}
}

// episodeRescan asks the server to index episode p. One whose show folder was
// renamed by hand since the server last read it is a path the server does not
// know (it answers not_indexable), so the show folder is rescanned instead.
// A 429 askRescan could not wait out leaves the episode, saved already, for
// the show's next poll, which rescans a show not indexed since (isIndexed),
// rather than fail the job index_failed and fetch it again.
func (s *service) episodeRescan(ctx context.Context, id int64, p string) error {
	srv := s.o.Server()
	if srv == nil {
		return fail("index_failed", "The episode could not be indexed. Retry the job.")
	}
	show := path.Dir(p)
	err := s.askRescan(ctx, srv, id, p)
	if errors.Is(err, link.ErrNotIndexable) {
		err = s.askRescan(ctx, srv, id, show)
	}
	if _, limited := link.RateLimited(err); limited {
		s.setIndexed(id, show, false)
		slog.Info("podcasts: the server asked the Bridge to slow down; the episode is indexed at its show's next poll", "library", id, "show", show)
		return nil
	}
	if err != nil {
		return fail("index_failed", "The episode could not be indexed. Retry the job.")
	}
	s.setIndexed(id, show, true)
	return nil
}

// enclosure is episode e's enclosure: from feed.json, or for a private feed
// (whose feed.json keeps no URL) from the feed itself, fetched again.
func (s *service) enclosure(ctx context.Context, f *podcast.Feed, e podcast.Episode) (podcast.Enclosure, error) {
	if !f.Private {
		if e.Enclosure.URL == "" {
			return podcast.Enclosure{}, fail("enclosure_missing", "The episode has no available enclosure.")
		}
		return e.Enclosure, nil
	}
	raw, err := s.feedURL("", true, secrets.PodcastFeedName(f.ShowID), "The private feed address is invalid.")
	if err != nil {
		return podcast.Enclosure{}, err
	}
	resp, err := s.o.Fetcher.Fetch(ctx, netguard.Request{URL: raw, MaxBytes: s.o.MaxFeedBytes})
	if err != nil {
		return podcast.Enclosure{}, err
	}
	parsed, err := podcast.ParseExisting(resp.Body, resp.URL)
	if err != nil {
		return podcast.Enclosure{}, err
	}
	var exact, weak []podcast.Episode
	for _, candidate := range parsed.Episodes {
		guid := candidate.GUID
		if guid != "" && e.GUID == "sha256:"+podcast.FeedURLHash(guid) {
			guid = e.GUID
		}
		if podcast.EpisodeID(candidate) == e.ID || e.GUID != "" && e.GUID == guid {
			exact = append(exact, candidate)
		}
		if podcast.RedactText(candidate.Title) == e.Title && candidate.Published == e.Published {
			weak = append(weak, candidate)
		}
	}
	if len(exact) == 1 && exact[0].Enclosure.URL != "" {
		return exact[0].Enclosure, nil
	}
	if len(exact) == 0 && len(weak) == 1 && weak[0].Enclosure.URL != "" {
		return weak[0].Enclosure, nil
	}
	return podcast.Enclosure{}, fail("enclosure_missing", "The episode enclosure could not be identified.")
}

// download fetches the episode's enclosure enc (run's, resolved once per job
// and held in memory only) into its partial (c), retrying the host. settle
// (run's) follows the fetch, job and c, to a show folder renamed by hand that
// took the partial: checked before each attempt and before a retry's clear.
// follow (run's) is consume's, for a partial no longer at its path while it
// is written (keepHome).
func (s *service) download(ctx context.Context, b *budget, job *jobs.Job, root *os.Root, enc podcast.Enclosure, c *checkpoint, tag string, settle, follow func(context.Context) error, rep *jobs.Reporter) (podcast.Fetch, error) {
	if s.o.Streamer == nil {
		return podcast.Fetch{}, fail("write_failed", "The episode download is unavailable.")
	}
	// Every attempt GETs the same enclosure, and the record names it, so a
	// whole download kept for the next attempt is published only while the
	// feed still has it (sameSource).
	raw := enc.URL
	c.setSource(enc)
	var last error
	// A partial proven changed (gone, not a regular file, another size) is
	// started again from zero, once per job: the episode may well be fine,
	// only the Bridge's own copy of its start is not. A second one fails the
	// job write_failed. One that could not be read to tell (a network share's
	// hiccup, errPartialUnreadable) is never cleared: the attempt is tried
	// again as a host's failure is, and the last one fails write_failed with
	// the partial and its record kept (run's exit) for the next attempt.
	restarted := false
	// onPartial answers attempt's partial failure err with the job's error,
	// or nil to go on: from zero for one proven changed (errPartialChanged),
	// else with the attempt tried again.
	onPartial := func(err error, attempt int) error {
		if errors.Is(err, errPartialChanged) {
			if restarted {
				return fail("write_failed", "The episode partial is incomplete.")
			}
			restarted = true
			if err := s.restartPartial(*job, root, c, err); err != nil {
				return err
			}
			tag = ""
			return nil
		}
		last = keptPartial(c, err)
		if attempt == 2 || s.o.Wait(ctx, netguard.RetryDelay(attempt, 0)) != nil {
			return last
		}
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return podcast.Fetch{}, ctx.Err()
		}
		if err := settle(ctx); err != nil {
			return podcast.Fetch{}, err
		}
		// Checked before the ranged GET, so an unreadable partial costs no request:
		// the attempt goes out from zero instead, or waits for one that could
		// not be read to tell.
		if c.Offset > 0 {
			if err := s.partialOK(root, c); err != nil {
				if err := onPartial(err, attempt); err != nil {
					return podcast.Fetch{}, err
				}
				if !errors.Is(err, errPartialChanged) {
					continue
				}
			}
		}
		response, err := s.o.Streamer.Stream(ctx, netguard.StreamRequest{URL: raw, Offset: c.Offset, ETag: tag})
		if err == nil {
			action := "restart_with_200_body"
			if c.Offset > 0 {
				action = netguard.ResumeAction(c.Offset, c.Total, tag, response)
			} else if response.Status == 206 {
				action = "fresh_get"
			}
			var fetch podcast.Fetch
			switch {
			case response.Status != 200 && response.Status != 206 && (response.Status != 416 || c.Offset == 0):
				err = netguard.StatusError("episode", response.Status, response.RetryAfterSeconds)
			case action == "fresh_get":
				_ = response.Body.Close()
				if err := s.restartPartial(*job, root, c, errors.New("the episode resume could not be verified")); err != nil {
					return podcast.Fetch{}, err
				}
				tag = ""
				last = fail("network", "The episode resume could not be verified.")
				continue
			default:
				fetch, tag, err = s.consume(ctx, b, job, root, c, tag, response, action, follow, rep)
			}
			_ = response.Body.Close()
			if err == nil {
				return fetch, nil
			}
			// The partial failed between the check and the append: proven
			// changed, it restarts (attempt-- keeps the host's retries); not
			// read to tell, the attempt is tried again with it kept.
			if errors.Is(err, errPartialChanged) || errors.Is(err, errPartialUnreadable) {
				if err := onPartial(err, attempt); err != nil {
					return podcast.Fetch{}, err
				}
				if errors.Is(err, errPartialChanged) {
					attempt--
				}
				continue
			}
			// A local file failure (a full disk, a partial the restart from
			// byte 0 could not remove) is not a network interruption: never
			// retried or reported as one.
			var guard *netguard.Error
			if errors.Is(err, errPartialStuck) || !errors.As(err, &guard) && ctx.Err() == nil {
				return podcast.Fetch{}, err
			}
		}
		failure := netguard.Classify(ctx, err)
		last = failure
		again := attempt < 2 && netguard.Retryable(failure) && failure.RetryAfterSeconds <= 10
		// The job's end (a cancel, a lost lease, the Bridge stopping, the
		// ceiling) with another try still to come is that end (stopped),
		// never what the host last answered: a cancelled job's http_503
		// would count against the feed (the server's cancelledFailureCodes),
		// though the next try might have had the episode. So is a request the
		// end itself cut short. An answer no try would have followed (the
		// last one's, a 404) stays the host's.
		if ctx.Err() != nil && (again || failure.Code == "timeout") {
			return podcast.Fetch{}, stopped()
		}
		if !again {
			break
		}
		if !netguard.StrongETag(tag) || c.Offset <= 0 || c.Total <= c.Offset {
			_ = settle(ctx) // the clear finds the partial where it is now
			if err := s.restartPartial(*job, root, c, failure); err != nil {
				return podcast.Fetch{}, err
			}
			tag = ""
		}
		// A wait the job's end cuts short is that end too.
		if s.o.Wait(ctx, netguard.RetryDelay(attempt, failure.RetryAfterSeconds)) != nil {
			return podcast.Fetch{}, stopped()
		}
	}
	return podcast.Fetch{}, last
}

// consume writes response r into the partial c, from its offset (an append)
// or from byte 0. job is run's, so a checkpoint goes under the path follow
// (run's) moved the fetch to: the partial is checked against its path at most
// once a second and at the end (keepHome), and one a copy and a delete of its
// show folder left behind is written where the show is now.
func (s *service) consume(ctx context.Context, b *budget, job *jobs.Job, root *os.Root, c *checkpoint, tag string, r *netguard.StreamResponse, action string, follow func(context.Context) error, rep *jobs.Reporter) (podcast.Fetch, string, error) {
	if r.Encoding != "" && r.Encoding != "identity" {
		return podcast.Fetch{}, "", fail("unsupported_encoding", "The episode uses an unsupported encoding.")
	}
	total := c.Total
	if action != "append" {
		// A body from zero never reuses the old partial's inode: its name is
		// unlinked and a fresh exclusive temp is made, so nothing swapped in at
		// that name (a link to another file) is ever truncated or written.
		// One the clear cannot remove fails the attempt (errPartialStuck),
		// never a download beside it.
		if c.Temp != "" {
			if err := s.restartPartial(*job, root, c, errors.New("the episode host sent it from byte 0")); err != nil {
				return podcast.Fetch{}, "", err
			}
		}
		total = r.ContentLength
		c.Offset = 0
		tag = r.ETag
	}
	if total > s.o.MaxEpisodeBytes {
		return podcast.Fetch{}, "", fail("too_large", "The episode exceeds its size limit.")
	}
	if total > 0 {
		b.tighten(total)
	}
	// Only an append keeps c.Temp (any other action cleared it above).
	appending := c.Temp != ""
	if !appending {
		c.Temp = podcast.TempPrefix + nonce()
	}
	name := c.partialName()
	var file *os.File
	var err error
	if appending {
		if err = s.o.Fault.Hit("append"); err != nil {
			err = partialError(err)
		} else {
			file, err = openPartial(root, name, c.Offset)
		}
	} else {
		file, err = root.OpenFile(name, os.O_RDWR|os.O_EXCL|os.O_CREATE, 0o600)
	}
	if err != nil {
		return podcast.Fetch{}, tag, err // a partial kept is still the one tag validates
	}
	defer func() { _ = file.Close() }() // file, as keepHome last replaced it
	if err := file.Chmod(0o600); err != nil {
		return podcast.Fetch{}, "", err
	}
	h := sha256.New()
	if appending {
		if _, err := io.CopyN(h, file, c.Offset); err != nil {
			// The Bridge's own partial file: a local fault, not the episode host's.
			// Shorter than its prefix, it changed (download restarts the episode
			// once, then fails write_failed); a read that failed tells nothing.
			if errors.Is(err, io.EOF) {
				return podcast.Fetch{}, tag, errPartialChanged
			}
			return podcast.Fetch{}, tag, partialError(err)
		}
	}
	c.Total = total
	out := &downloadWriter{file: file, hash: h, rep: rep, received: c.Offset, total: total}
	var lastCheck, lastCheckpoint, lastFollow time.Time
	// home keeps the partial written the one at its path (keepHome), following
	// at most every 10 s while it is nowhere to be found, then less often
	// (twice the wait each time a follow finds no new place, up to
	// followMaxEvery): a show folder deleted outright makes each follow search
	// the whole library for its show. The end clears lastFollow, so it follows
	// then.
	every := followEvery
	home := func(received int64) error {
		next, err := s.keepHome(ctx, root, c, file, received, func(ctx context.Context) error {
			if time.Since(lastFollow) < every {
				return nil
			}
			lastFollow = time.Now()
			at := c.partialName()
			if err := follow(ctx); err != nil {
				return err
			}
			if c.partialName() == at {
				every = min(2*every, followMaxEvery)
			} else {
				every = followEvery
			}
			return nil
		})
		if err != nil {
			return err
		}
		file, out.file = next, next
		return nil
	}
	// refused is a check's failure: a mapping or pairing changed, a show not
	// followed. Nothing more is written to the library after it.
	var refused error
	out.check = func(received int64) (err error) {
		defer func() { refused = err }()
		if err := ctx.Err(); err != nil {
			return err
		}
		checkpointDue := received > 0 && received < total && netguard.StrongETag(tag) && time.Since(lastCheckpoint) >= 10*time.Second
		// The mapping and pairing rechecks read the mount and the vault: at most once
		// a second, and always before a checkpoint. Commit rechecks both under the lock.
		if !checkpointDue && time.Since(lastCheck) < time.Second {
			return nil
		}
		lastCheck = time.Now()
		lib, ok := s.o.Libraries.Library(*job.LibraryID)
		if !ok {
			return fail("library_not_writable", "The podcast mapping changed.")
		}
		if err := s.writable(lib, root.Name()); err != nil {
			return err
		}
		if c.ServerHash != s.pairHash() {
			return fail("identity_changed", "The Bridge pairing changed.")
		}
		if err := home(received); err != nil {
			return err
		}
		if _, err := mapping.SafeJoin(root.Name(), c.partialName()); err != nil {
			return err
		}
		if checkpointDue {
			if err := file.Sync(); err != nil {
				return err
			}
			c.Offset = received
			if err := s.saveCheckpoint(*job, root, c, tag); err != nil {
				return err
			}
			lastCheckpoint = time.Now()
		}
		return ctx.Err()
	}
	out.progress()
	_, copyErr := io.CopyBuffer(out, io.LimitReader(r.Body, s.o.MaxEpisodeBytes-c.Offset+1), make([]byte, 64<<10))
	c.Offset = out.received
	if err := file.Sync(); err != nil {
		return podcast.Fetch{}, "", err
	}
	if c.Offset > s.o.MaxEpisodeBytes {
		return podcast.Fetch{}, "", fail("too_large", "The episode exceeds its size limit.")
	}
	if copyErr == nil && (c.Offset == 0 || total >= 0 && c.Offset != total) {
		copyErr = io.ErrUnexpectedEOF
	}
	// The whole episode is in file, and it is published from the partial at
	// its path: a show folder copied and deleted since the last check must not
	// leave only the copy of its start there.
	if copyErr == nil {
		lastFollow = time.Time{}
		if err := home(c.Offset); err != nil {
			return podcast.Fetch{}, "", err
		}
	}
	if copyErr != nil {
		var guard *netguard.Error
		var disk *diskError
		isGuard := errors.As(copyErr, &guard)
		if !isGuard && errors.As(copyErr, &disk) {
			return podcast.Fetch{}, "", fail("write_failed", "The episode could not be saved.")
		}
		resumable := c.Offset > 0 && c.Offset < total && netguard.StrongETag(tag)
		if resumable && refused == nil && ctx.Err() == nil {
			// A show folder copied and deleted since the last check: the bytes
			// held go where the show is now first, so the retry (or the record)
			// resumes from all of them rather than find a stale copy there. A
			// failure here leaves the record for loadCheckpoint to judge.
			lastFollow = time.Time{}
			_ = home(c.Offset)
		}
		if isGuard {
			return podcast.Fetch{}, tag, guard
		}
		if resumable {
			if err := s.saveCheckpoint(*job, root, c, tag); err != nil {
				return podcast.Fetch{}, "", err
			}
		}
		if ctx.Err() != nil {
			return podcast.Fetch{}, tag, ctx.Err()
		}
		return podcast.Fetch{}, tag, fail("network", "The episode download was interrupted.")
	}
	return podcast.Fetch{Bytes: c.Offset, SHA256: hex.EncodeToString(h.Sum(nil))}, tag, nil
}

// followEvery and followMaxEvery bound how often a download whose partial is
// nowhere to be found follows its show (consume's home).
const (
	followEvery    = 10 * time.Second
	followMaxEvery = 5 * time.Minute
)

// partialOK checks that the proven partial c resumes can still be opened for
// the append and is the regular file it was (openPartial's errors). A read
// failing later in the prefix is consume's to find.
func (s *service) partialOK(root *os.Root, c *checkpoint) error {
	if err := s.o.Fault.Hit("partial"); err != nil {
		return partialError(err)
	}
	file, err := openPartial(root, c.partialName(), c.Offset)
	if err != nil {
		return err
	}
	return file.Close()
}

// keepHome checks that file, the partial a download writes (received bytes so
// far), is still the file at its path (c.partialName()), and returns the file
// to go on writing. A show folder renamed by hand took it along: follow moves
// the fetch (and c) there, the same file. One "renamed" by a copy and a delete
// (a move across filesystems, a file manager's) left file unlinked, written
// to no folder, and a stale copy of its start where the show is now: the bytes
// file holds replace that copy (a fresh exclusive file at its name, never the
// copy written over), so the download goes on, and publishes, there. file
// itself when nothing is at the path (a folder deleted outright, the job's end
// finds it so) or it is not a regular file. An error is follow's, or a
// failure writing the new partial.
func (s *service) keepHome(ctx context.Context, root *os.Root, c *checkpoint, file *os.File, received int64, follow func(context.Context) error) (*os.File, error) {
	if lockedAt(root, c.partialName(), file) {
		return file, nil
	}
	held, err := file.Stat()
	if err != nil {
		return file, nil
	}
	if err := follow(ctx); err != nil {
		return file, err
	}
	name := c.partialName()
	st, err := podcast.LstatRegular(root, name)
	if err != nil || os.SameFile(held, st) {
		return file, nil
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return file, fail("write_failed", "The episode could not be saved.")
	}
	next, err := root.OpenFile(name, os.O_RDWR|os.O_EXCL|os.O_CREATE, 0o600)
	if err != nil {
		return file, fail("write_failed", "The episode could not be saved.")
	}
	// A large buffer: os.File cannot copy from a SectionReader in the kernel,
	// and this may be the whole episode.
	if _, err := io.CopyBuffer(next, io.NewSectionReader(file, 0, received), make([]byte, 1<<20)); err != nil || next.Sync() != nil {
		_ = next.Close()
		_ = root.Remove(name)
		return file, fail("write_failed", "The episode could not be saved.")
	}
	slog.Info("podcasts: a download's partial was copied with its show folder and the original deleted; it goes on where the show is now", "partial", name, "bytes", received)
	_ = file.Close()
	return next, nil
}

// openPartial opens the proven partial at name for the append. A partial
// proven gone, replaced by a link or another file, barred to the Bridge (no
// permission) or of another size since it was proven is errPartialChanged;
// any other failure (a share's hiccup) errPartialUnreadable. Neither is a
// reason to write to whatever is there.
func openPartial(root *os.Root, name string, size int64) (*os.File, error) {
	file, err := podcast.OpenRegularFile(root, name, os.O_RDWR)
	if err != nil {
		return nil, partialError(err)
	}
	st, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, partialError(err)
	}
	if st.Size() != size {
		_ = file.Close()
		return nil, errPartialChanged
	}
	return file, nil
}

// keptPartial logs err, why c's partial could not be read to resume it (not
// known to have changed, so kept with its record), and is the job's failure.
func keptPartial(c *checkpoint, err error) error {
	slog.Warn("podcasts: an episode's partial could not be read to resume it; it is kept", "partial", c.partialName(), "err", err)
	return fmt.Errorf("%w: %w", err, fail("write_failed", "The episode's partial download could not be read."))
}

// stopped is the failure of an episode download its job's end (a cancel, a
// lost lease, the Bridge stopping) cut short between two tries or in one: a
// timeout, as netguard reads an ended context (never a code the server
// counts against the feed for a cancelled job), worded as the stop rather
// than as a feed that did not respond. The ceiling's is the episode's own
// timeout (budget.failure).
func stopped() *netguard.Error {
	return fail("timeout", "The episode download was stopped before it finished.")
}

// restartPartial clears c's partial for a download from byte 0, or answers
// stuckPartial when the clear cannot remove it: never a download
// beside it, its record left for the next attempt or the day-old cleanup.
func (s *service) restartPartial(job jobs.Job, root *os.Root, c *checkpoint, cause error) error {
	partial := ""
	if c.Temp != "" {
		partial = c.partialName() // the clear resets c, removed or not
	}
	if !s.clearCheckpoint(job, root, c) {
		return stuckPartial(partial, cause)
	}
	return nil
}

// stuckPartial is the job's failure for a partial the restart from zero could
// not remove (partial, its name when known, logged so it can be found; err
// wraps the clear's cause).
func stuckPartial(partial string, err error) error {
	slog.Warn("podcasts: an episode's partial could not be removed to download it again", "partial", partial, "err", err)
	return fmt.Errorf("%w: %w", errPartialStuck, fail("write_failed", "The episode's partial download could not be removed."))
}

// partialError is err, a partial's open, check or read, as errPartialChanged
// when it proves the partial gone, replaced or not the Bridge's to open any
// more (partialLost), else errPartialUnreadable.
func partialError(err error) error {
	if partialLost(err) {
		return fmt.Errorf("%w: %w", errPartialChanged, err)
	}
	return fmt.Errorf("%w: %w", errPartialUnreadable, err)
}

type downloadWriter struct {
	check           func(int64) error
	file            *os.File
	hash            hash.Hash
	rep             *jobs.Reporter
	received, total int64
	last            time.Time
}

func (w *downloadWriter) Write(p []byte) (int, error) {
	if err := w.check(w.received); err != nil {
		return 0, err
	}
	n, err := w.file.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.received += int64(n)
		if time.Since(w.last) >= time.Second {
			w.progress()
		}
	}
	if err != nil {
		return n, &diskError{err}
	}
	return n, nil
}

// diskError marks a failed write to the staged audio, telling it apart from a
// failed read of the response body.
type diskError struct{ err error }

func (e *diskError) Error() string { return e.err.Error() }
func (e *diskError) Unwrap() error { return e.err }
func (w *downloadWriter) progress() {
	var total any
	if w.total >= 0 {
		total = w.total
	}
	w.rep.Progress(jobs.Progress{Step: "fetching", Message: "Fetching episode", Extra: map[string]any{"bytes_received": w.received, "bytes_total": total}})
	w.last = time.Now()
}

// abandonWait is how long Abandon waits for an episode's lock another job
// holds: long enough for a holder that lets go at once (a cleanup, a clear),
// never a download's length.
var abandonWait = 10 * time.Second

// Abandon handles a terminal cancel/lost claim after a resumable stop. Mapping
// and identities must still match before any owned partial is removed. The
// runner names the job by the server's path: a record moved with a show folder
// renamed by hand (the job's own relocation, or a take-in since) is cleared
// where it is now (recordsOf), as is one still at the job's path beside it,
// and one left under a folder that is gone goes too.
//
// The record of a download still running is never cleared: another open row
// of the episode (a duplicate claim) shares this job's record, and that fetch
// resumes from it. Its lock, held for the whole download, is waited on for
// abandonWait at most (the failed job holds its runner slot meanwhile), and a
// record another running row names stays whoever holds the lock (running: a
// fetch stopped at a resumable point holds none, and neither does one whose
// folder is gone). The fetch's own end clears it, or the day-old cleanup.
func (h *Fetch) Abandon(ctx context.Context, job jobs.Job) { h.abandon(ctx, job, "") }

// abandon is Abandon, leaving the record of partial kept (a failed fetch's
// own, run's exit kept it for the next attempt), and any record whose
// partial could not be read to tell whether it changed (loadCheckpoint).
func (h *Fetch) abandon(ctx context.Context, job jobs.Job, kept string) {
	if job.Path == nil || job.LibraryID == nil || !podcast.ValidEpisodePath(*job.Path) {
		return
	}
	var p fetchParams
	if json.Unmarshal(job.Params, &p) != nil {
		return
	}
	s := h.s.forJob(job)
	lib, ok := s.o.Libraries.Library(*job.LibraryID)
	if !ok {
		return
	}
	mount, _ := s.o.Mount(lib.ID, lib.Root)
	if s.writable(lib, mount) != nil {
		return
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	running := func(checkpoint) bool { return false }
	if s.o.DB != nil {
		// Read even when ctx has ended (a cancelled job's own Run calls this).
		rows, err := s.o.DB.OpenServerJobs(context.WithoutCancel(ctx))
		if err != nil {
			return // not known: nothing is cleared (the day-old cleanup's then)
		}
		running = s.runningRecords(rows, lib.ID, job.ID)
	}
	for _, at := range s.recordsOf(job, p) {
		job.Path = &at
		if running(checkpoint{ShowID: p.ShowID, EpisodeID: p.EpisodeID, Path: at}) {
			continue
		}
		wait, stop := context.WithTimeout(ctx, abandonWait)
		release, err := s.fileLock(wait, root, at)
		stop()
		if err != nil {
			// Its folder is gone (renamed by hand while this job's row ran, so
			// the take-in left its record here): no download works at this
			// path, so the record goes rather than stay for good. Its partial,
			// moved with the folder, is unrecorded then, and sweepPartials takes
			// it once old. A lock still held after the wait is a download's,
			// and so is the record.
			if folderThere(root, path.Dir(at)) {
				continue
			}
			release = func() {}
		}
		if c, _, err := s.loadCheckpoint(job, root, p.ShowID, p.EpisodeID); err == nil && (kept == "" || c.Temp != kept) {
			s.clearCheckpoint(job, root, &c)
		}
		release()
	}
}

var _ jobs.Abandoner = (*Fetch)(nil)
