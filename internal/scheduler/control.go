package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/scratch"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// ErrInvalidOp is returned when a control action does not apply to a book's
// current status (e.g. resuming a book that is not paused).
var ErrInvalidOp = errors.New("operation not valid for book's current status")

// ErrBusy is returned by Delete when a book is actively running a stage.
var ErrBusy = errors.New("book is running a stage")

// cancelInflight cancels a book's running stage if any (interrupting its
// executor); the worker then leaves the stage re-runnable.
func (s *Scheduler) cancelInflight(id int64) {
	s.mu.Lock()
	ib, ok := s.inflight[id]
	s.mu.Unlock()
	if ok {
		ib.cancel()
	}
}

// workerExitWait bounds how long a control change waits for a stage worker it
// cancelled to return (awaitWorker): one that ignores its cancel is left to it.
const workerExitWait = 30 * time.Second

// awaitWorker waits for the stage worker running on book id, which the caller
// cancelled, to return (up to workerExitWait, or ctx's end), so nothing it writes
// late, a sentinel its executor wrote as the cancel landed, lands after the
// caller's change (supersede_rerun would replay that sentinel rather than re-run
// the stage). A reservation has no worker to wait for.
func (s *Scheduler) awaitWorker(ctx context.Context, id int64) {
	s.mu.Lock()
	ib, ok := s.inflight[id]
	s.mu.Unlock()
	if !ok || ib.done == nil {
		return
	}
	t := time.NewTimer(workerExitWait)
	defer t.Stop()
	select {
	case <-ib.done:
	case <-ctx.Done():
	case <-t.C:
		slog.Warn("scheduler: a cancelled stage worker hasn't returned; the change goes on without it", "book_id", id)
	}
}

// Pause parks a running or queued book: it stops being dispatched. A stage
// already executing is allowed to finish (its result is kept), after which the
// book waits in its next state. Pausing an already-paused book is a no-op.
func (s *Scheduler) Pause(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	switch state.Status(b.Status) {
	case state.StatusPaused:
		return nil
	case state.StatusNone:
		// Preserve error/park_code (a pause is not a status clear); a pausable book
		// has no park code anyway, but mirror b.Error's preservation.
		if err := s.db.SetBookStatus(ctx, id, string(state.StatusPaused), b.Error, b.ParkCode); err != nil {
			return err
		}
		s.publishState(id, b.State, string(state.StatusPaused), b.Error, b.ParkCode, "")
		return nil
	default:
		return ErrInvalidOp
	}
}

// Resume clears a pause and re-admits the book to scheduling.
func (s *Scheduler) Resume(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	if state.Status(b.Status) != state.StatusPaused {
		return ErrInvalidOp
	}
	// Clearing the status clears the error and the typed park_code together.
	if err := s.db.SetBookStatus(ctx, id, string(state.StatusNone), "", ""); err != nil {
		return err
	}
	s.publishState(id, b.State, "", "", "", "")
	s.notify()
	return nil
}

// Retry re-admits a failed or needs_attention book by clearing its status and
// forcing the current stage to re-run: the stage's sentinel and its recorded
// success are dropped so the executor runs afresh (a failed stage has no sentinel
// anyway; a parked audit does, and must be re-executed to make progress).
func (s *Scheduler) Retry(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	return s.RetryAsRead(ctx, b)
}

// RetryAsRead is Retry for a book the caller has read already (b): it re-admits
// the book only while it still carries b's status and error, so a Cancel landing
// after the caller's read (a job's Retry that read the book parked) is never taken
// up as a Retry of the cancelled book: store.ErrStatusChanged, and nothing changed
// (bridge#65).
func (s *Scheduler) RetryAsRead(ctx context.Context, b store.Book) error {
	switch state.Status(b.Status) {
	case state.StatusFailed, state.StatusNeedsAttention:
		return s.readmit(ctx, b)
	default:
		return ErrInvalidOp
	}
}

// ReadmitParked re-admits every book parked with code (the Bridge: asr_worker_waiting
// once a worker is connected or the ASR fallback isn't a wait) through the same path
// as a manual Retry, and returns how many it re-admitted; a book busy elsewhere is
// skipped.
func (s *Scheduler) ReadmitParked(ctx context.Context, msg string, code state.ParkCode) int {
	books, err := s.db.ListBooksParkedWith(ctx, string(code))
	if err != nil {
		slog.Warn("readmit-parked: list books failed", "err", err)
		return 0
	}
	n := 0
	for _, b := range books {
		if ctx.Err() != nil {
			break // the Bridge stopping (its background tasks' context): no warning per book
		}
		if !s.reserve(b.ID) {
			continue
		}
		err := s.readmit(ctx, b)
		s.unreserve(b.ID)
		if err != nil {
			logReadmitFailed(slog.Default(), "readmit-parked", b.ID, err)
			continue
		}
		n++
		_ = s.hub.PublishBook("stage.note", b.ID, map[string]any{"book_id": b.ID, "stage": b.State, "msg": msg})
	}
	return n
}

// logReadmitFailed logs a batch re-admission (what) that didn't happen: at Debug
// when the book's status changed between the list and the write (a re-park or a
// Cancel landing there: the book is left as it is, nothing went wrong), at Warn
// otherwise (bridge#65).
func logReadmitFailed(log *slog.Logger, what string, bookID int64, err error) {
	if errors.Is(err, store.ErrStatusChanged) {
		log.Debug(what+": the book's status changed meanwhile; left as it is", "book_id", bookID)
		return
	}
	log.Warn(what+": readmit failed", "book_id", bookID, "err", err)
}

// readmit is the shared re-admission body: it clears the status/error/park_code
// (and the scheduled retry_at), forces the book's current stage to re-run (drops
// its sentinel), and wakes the dispatch loop. Retry (the manual control) gates on
// status first; the scheduler's timed auto-readmit calls it directly for a due
// transient-agent park. It takes b so a caller that already read the book does not
// re-query.
//
// Whether it SUPERSEDES the current stage's recorded successes is keyed on the PARK CODE,
// not merely the stage: superseding resets that stage's round/fix counter (CountStageSuccesses),
// which is the "grant one fresh round" reset the ROUND-CAP parks want (qa_no_converge resets
// the QA round counter; fix_loop_exhausted resets the audit round count). For every OTHER park
// code the current stage's successes are LEFT intact - an availability park (agent_rate_limited/
// agent_unavailable) or a budget park readmitted mid-stage (e.g. at auditing) must not wipe the
// convergence trajectory it had already built. A plain failed-book Retry has no success rows at
// the current stage anyway, so leaving them alone is a harmless no-op there.
//
// The status is cleared FIRST, and only while the book still carries the status
// and error read into b: a Cancel landing since (a supervisor tick racing it,
// bridge#37) is never undone, and then nothing else is changed either, the
// sentinel, the successes and the audit files included (bridge#65). The dispatch
// loop is held off the book meanwhile (holdDispatch), so it never starts the stage
// between the clear and the sentinel's removal (it would replay the sentinel). A
// change that fails after the clear puts the read status back. A crash between the
// two leaves the book admitted with its sentinel: the restart replays the stage's
// outcome, which parks a round-cap park again, for the next Retry.
func (s *Scheduler) readmit(ctx context.Context, b store.Book) error {
	release := s.holdDispatch(b.ID)
	defer release()
	s.readmitMu.Lock()
	defer s.readmitMu.Unlock()
	if err := s.db.ClearBookStatusIf(ctx, b.ID, b.Status, b.Error); err != nil {
		return err
	}
	if err := s.forceRerun(ctx, b); err != nil {
		s.restoreStatus(ctx, b)
		return err
	}
	s.publishState(b.ID, b.State, "", "", "", "")
	return nil
}

// forceRerun is readmit's change to an admitted book (see readmit): the store's
// writes first, which can fail, then the files.
func (s *Scheduler) forceRerun(ctx context.Context, b store.Book) error {
	id, stage := b.ID, b.State
	// Reset the current stage's round/fix counter ONLY for the round-cap parks (grant one
	// fresh round). Availability/budget parks keep their successes so their trajectory survives.
	if b.ParkCode == string(state.ParkQANoConverge) || b.ParkCode == string(state.ParkFixLoopExhausted) {
		if err := s.db.SupersedeStageSuccesses(ctx, id, stage); err != nil {
			return err
		}
	}
	// A book parked at auditing SPECIFICALLY because it exhausted its fix-loop budget
	// (park_code fix_loop_exhausted; FixAttempts counts the fixing successes) needs a
	// genuinely fresh loop: superseding only the auditing success would burn one audit
	// (~$4.50) and instantly re-park at the cap. Grant it by superseding the fixing
	// successes too and wiping the audit-loop trajectory artifacts so the re-run starts
	// from a clean round-1 history (audit_rounds.json / audit_accepted.json).
	if b.ParkCode == string(state.ParkFixLoopExhausted) {
		if err := s.db.SupersedeStageSuccesses(ctx, id, string(state.Fixing)); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(b.WorkDir, AuditRoundsFile))
		_ = os.Remove(filepath.Join(b.WorkDir, AuditAcceptedFile))
	}
	_ = os.Remove(SentinelPath(b.WorkDir, stage)) // best-effort; force a clean re-run
	return nil
}

// restoreStatus puts back the status, error, park code and retry_at a control
// change cleared from b (as read) when the change then failed, unless the book's
// status moved on since the clear (a Cancel).
func (s *Scheduler) restoreStatus(ctx context.Context, b store.Book) {
	err := s.db.SetBookStatusIf(context.WithoutCancel(ctx), b.ID, "", "", b.Status, b.Error, b.ParkCode, b.RetryAt)
	if err != nil && !errors.Is(err, store.ErrStatusChanged) {
		slog.Warn("scheduler: putting a book's status back failed", "book_id", b.ID, "err", err)
	}
}

// holdDispatch keeps the dispatch loop from starting a stage on book id while a
// control change rewrites it (readmit and supersede_rerun in steps, Cancel in
// one): no stage starts on it until release, nor in a pass that read its books
// before release (dispatchableLocked). release wakes the loop; calling it again
// does nothing.
func (s *Scheduler) holdDispatch(id int64) (release func()) {
	s.mu.Lock()
	s.held[id]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if s.held[id]--; s.held[id] <= 0 {
				delete(s.held, id)
			}
			s.released[id] = struct{}{}
			s.mu.Unlock()
			s.notify()
		})
	}
}

// beginPass marks where a dispatch pass starts reading its books: a hold released
// after this mark keeps its book out of the pass, and the ones released before it
// are forgotten.
func (s *Scheduler) beginPass() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.released)
}

// dispatchableLocked reports whether the pass may start a stage on book id: no
// control change holds it, and none released it after the pass read its books.
// Called under mu.
func (s *Scheduler) dispatchableLocked(id int64) bool {
	_, released := s.released[id]
	return s.held[id] == 0 && !released
}

// cancelledReason is the error a cancelled book carries (the status enum has no
// dedicated cancelled state); IsCancelled tells a cancel from a failure by it.
const cancelledReason = "cancelled by user"

// IsCancelled reports whether b was stopped by Cancel rather than failed by a
// stage.
func IsCancelled(b store.Book) bool {
	return state.Status(b.Status) == state.StatusFailed && b.Error == cancelledReason
}

// Cancel stops a book: it interrupts any running stage and marks the book failed
// with a "cancelled" reason (the status enum has no dedicated cancelled state).
// A cancelled book can later be retried or deleted.
//
// The dispatch loop is held off the book meanwhile (holdDispatch): a pass that
// read the book before the cancel was written never starts a stage on it after
// cancelInflight found none to stop (bridge#65).
func (s *Scheduler) Cancel(ctx context.Context, id int64) error {
	return s.cancel(ctx, id, nil)
}

// CancelUnlessFollowed is Cancel for a job's end that may no longer own the book:
// the store's cancel is written only while g holds (store.StopGuard, checked in
// the same statement), and nothing is stopped when it doesn't (store.ErrFollowed),
// nor when the book finished or a stage failed it since it was read
// (store.ErrSettled: its own error stays).
// A later claim of the job that reopens its mapping between the end's checks and
// the cancel is never stopped.
func (s *Scheduler) CancelUnlessFollowed(ctx context.Context, id int64, g store.StopGuard) error {
	return s.cancel(ctx, id, &g)
}

func (s *Scheduler) cancel(ctx context.Context, id int64, g *store.StopGuard) error {
	release := s.holdDispatch(id)
	defer release()
	if err := markCancelled(ctx, s.db, id, g); err != nil {
		return err
	}
	s.cancelInflight(id)
	s.publishCancelled(ctx, id)
	return nil
}

// publishCancelled publishes book id's cancel as stored, unless the book moved on
// since the cancel was written: a later claim of its job re-admitting it published
// its own state, which a stale cancelled frame would cover until the next event.
// The read and the publish hold readmitMu, as a re-admit's write and publish do.
func (s *Scheduler) publishCancelled(ctx context.Context, id int64) {
	s.readmitMu.Lock()
	defer s.readmitMu.Unlock()
	// The cancel is written: a caller's context ending now must not lose its frame.
	b, err := s.db.GetBook(context.WithoutCancel(ctx), id)
	if err != nil || !IsCancelled(b) {
		return
	}
	s.publishState(id, b.State, b.Status, b.Error, "", "")
}

// CancelStored is Cancel for a caller with no scheduler running (the Bridge's
// engine hasn't started yet): the store's write is the whole cancel, since no
// stage runs to interrupt, and the scheduler reads the book cancelled when it
// starts. ErrInvalidOp for a finished book, as Cancel.
func CancelStored(ctx context.Context, db *store.DB, id int64) error {
	return markCancelled(ctx, db, id, nil)
}

// CancelStoredUnlessFollowed is CancelStored written only while g holds, as
// CancelUnlessFollowed.
func CancelStoredUnlessFollowed(ctx context.Context, db *store.DB, id int64, g store.StopGuard) error {
	return markCancelled(ctx, db, id, &g)
}

// markCancelled marks book id cancelled in the store; with g, only while g holds
// (store.ErrFollowed otherwise), and never over a stage's failure or a finish:
// store.ErrSettled for a book read failed by other than a cancel, or one that
// failed or finished between the read and the write (a job's end read the book
// before this, and a stage may have failed it since). A book whose status
// changed otherwise meanwhile (a stage parked it) is read again and cancelled as
// it is now, so a park never outlives the job's end (a timed park would re-admit
// it with no job to follow it).
func markCancelled(ctx context.Context, db *store.DB, id int64, g *store.StopGuard) error {
	b, err := db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	if state.IsTerminal(state.State(b.State)) {
		return ErrInvalidOp
	}
	// Cancel is not a park (no typed reason), so the park_code stays empty.
	if g == nil {
		return db.SetBookStatus(ctx, id, string(state.StatusFailed), cancelledReason, "")
	}
	for range cancelRereads {
		if state.Status(b.Status) == state.StatusFailed && !IsCancelled(b) {
			return store.ErrSettled
		}
		err = db.SetBookStatusUnlessFollowed(ctx, id, b.Status, b.Error, string(state.StatusFailed), cancelledReason, *g)
		if !errors.Is(err, store.ErrSettled) {
			return err
		}
		if b, err = db.GetBook(ctx, id); err != nil {
			return err
		}
		if state.IsTerminal(state.State(b.State)) {
			return store.ErrSettled
		}
	}
	return store.ErrSettled
}

// cancelRereads bounds markCancelled's guarded write over a book whose status
// keeps changing under it.
const cancelRereads = 3

// Delete removes a book, its durable state, and its on-disk work dir. It refuses
// while the book is actively running a stage (cancel or pause first), so a worker
// never writes to a deleted book. The in-flight check and the DB delete happen
// under one lock so a worker can't start in the window between them; the books FK
// (ON DELETE CASCADE) still leaves a narrow residual race if a worker is mid-write
// - the user can simply re-delete.
func (s *Scheduler) Delete(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if _, busy := s.inflight[id]; busy {
		s.mu.Unlock()
		return ErrBusy
	}
	err = s.db.DeleteBook(ctx, id)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.removeWorkDir(b.WorkDir)
	return nil
}

// PurgeScratch reclaims a book's split chapters/ (the M2 heavy scratch) while
// keeping its durables (probe.json/manifest.json/transcripts). It is a manual,
// user-initiated reclaim: allowed only when the book is done, paused, or
// failed/cancelled - never mid-run (a running book still needs its chapters).
//
// It reserves the book id for the duration (a pseudo-inflight entry) so a
// concurrent Resume/Retry-triggered dispatch cannot start a stage that races the
// chapter removal, and a concurrent Delete sees it busy (ErrBusy) - the same
// window Delete already guards. The removal is confined to the work root by
// scratch.Purge.
func (s *Scheduler) PurgeScratch(ctx context.Context, id int64) error {
	b, err := s.db.GetBook(ctx, id)
	if err != nil {
		return err
	}
	if !purgeAllowed(b) {
		return ErrInvalidOp
	}
	// Reserve under the same lock dispatch/Delete use: fail if already in-flight,
	// otherwise hold the slot so no worker starts for this id until we release it.
	if !s.reserve(id) {
		return ErrBusy
	}
	defer s.unreserve(id)

	return s.purgeScratchInner(ctx, b)
}

// purgeScratchInner is the reclaim body shared by the manual PurgeScratch (which
// reserves the id first) and the auto-purge/startup-GC callers (which already hold, or
// do not need, the slot - so it must NOT reserve, or an in-flight worker calling it
// would deadlock/see itself busy). It reclaims the split scratch, invalidates the
// affected stage sentinels, re-accounts scratch_bytes, and reconciles a non-terminal
// book so a later Retry re-runs the invalidated stage. b must carry the book's CURRENT
// state (its terminal check gates the reconcile).
func (s *Scheduler) purgeScratchInner(ctx context.Context, b store.Book) error {
	kind := state.ParseKind(b.Kind)
	if err := scratch.Purge(s.workRoot, b.WorkDir, kind); err != nil {
		return err
	}
	// The reclaimed artifacts include a completed stage's output, so drop that
	// stage's sentinel (and its recorded success) - a later Retry/reconcile must
	// re-run it rather than skip it and feed the next stage an empty directory. The
	// list is DERIVED from the same table Purge deletes from, so a newly reclaimed
	// directory cannot be added without its stage coming with it.
	for _, st := range scratch.InvalidatedStages(kind) {
		_ = os.Remove(SentinelPath(b.WorkDir, string(st)))
	}
	// Re-account what remains (the durables) in one walk so scratch_bytes reflects
	// the reclaim without a read-side walk. If the walk itself fails, the pre-purge
	// value is now definitely wrong (we just deleted the chapters), so record 0
	// rather than leave a stale over-count. The gauge write uses a non-cancellable
	// context: the files are already deleted, so a shutdown-timed auto-purge/startup-GC
	// must not skip the accounting and leave scratch_bytes overstating disk that is gone.
	acctCtx := context.WithoutCancel(ctx)
	if n, derr := scratch.DirSize(b.WorkDir); derr == nil {
		_ = s.db.UpdateScratchBytes(acctCtx, b.ID, n)
	} else {
		slog.Warn("purge: re-accounting the work dir failed; recording 0 scratch",
			"book_id", b.ID, "work_dir", b.WorkDir, "err", derr)
		_ = s.db.UpdateScratchBytes(acctCtx, b.ID, 0)
	}
	// Reconcile the purged book WITHOUT waiting for a restart: dropping the split
	// sentinel above only re-runs the book if it is still AT splitting. A book past
	// splitting (e.g. failed at asr) would otherwise retry into an empty chapters/
	// and fail. reconcileBook rewinds it to the earliest completed stage whose
	// sentinel we just invalidated, so a following Retry re-splits. Terminal (done)
	// books are left untouched.
	if !state.IsTerminal(state.State(b.State)) {
		succeeded, serr := s.db.SucceededStages(ctx, b.ID)
		if serr != nil {
			return serr
		}
		if rerr := s.reconcileBook(ctx, b, succeeded); rerr != nil {
			return rerr
		}
	}
	return nil
}

// reserve inserts a pseudo-inflight entry for id (lane-less, cancel is a no-op) so
// dispatch/startWorker skip it and Delete treats it as busy. It returns false if
// the id is already in-flight (a real worker or another reservation). Callers pair
// it with unreserve.
func (s *Scheduler) reserve(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.inflight[id]; busy {
		return false
	}
	s.inflight[id] = &inflightBook{lane: state.LaneNone, cancel: func() {}}
	return true
}

// Hold reserves a book for work outside the scheduler (the Bridge's description
// step reads a finished book's notes and writes in its work dir) through the same
// reservation PurgeScratch takes: while it is held no stage is dispatched for it,
// Delete and PurgeScratch see it busy, the timed and parked re-admits and the
// startup GC skip it, and a supervisor terminate finds no worker to stop. ok is
// false when the book is busy already (a running stage, or another reservation).
// release ends the hold; calling it again does nothing.
func (s *Scheduler) Hold(id int64) (release func(), ok bool) {
	if !s.reserve(id) {
		return func() {}, false
	}
	var once sync.Once
	return func() { once.Do(func() { s.unreserve(id) }) }, true
}

// unreserve drops a reservation and wakes the dispatch loop, so a book made
// dispatchable during the reservation (e.g. a Resume that landed mid-purge) is
// picked up now that the slot is free.
func (s *Scheduler) unreserve(id int64) {
	s.mu.Lock()
	delete(s.inflight, id)
	s.mu.Unlock()
	s.notify()
}

// purgeAllowed reports whether a book is in a state where reclaiming its scratch is
// safe: terminal (done), or PARKED - paused, failed (cancel marks a book failed), or
// needs_attention. A parked book holds no worker, and a purge invalidates the
// sentinels of the stages whose inputs it removed, so a later Retry regenerates them.
// A running book (status none, non-terminal) still needs its scratch.
//
// needs_attention belongs here because it is the longest park there is: it waits on a
// human. An ebook sitting at chapter_mapping for weeks was otherwise unable to
// release its copyrighted source text by any means.
func purgeAllowed(b store.Book) bool {
	if state.IsTerminal(state.State(b.State)) {
		return true
	}
	switch state.Status(b.Status) {
	case state.StatusPaused, state.StatusFailed, state.StatusNeedsAttention:
		return true
	default:
		return false
	}
}

// removeWorkDir deletes a removed book's scratch dir, but ONLY when it resolves to
// a path inside the daemon's work root - a guard so a doctored or legacy WorkDir
// can never make delete rm an arbitrary location. It shares scratch.Confined with
// the purge path, so both destructive operations enforce the one guard. A missing
// dir is fine.
func (s *Scheduler) removeWorkDir(workDir string) {
	if wd, ok := scratch.Confined(s.workRoot, workDir); ok {
		_ = os.RemoveAll(wd)
	}
}
