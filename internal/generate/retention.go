package generate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scratch"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The transcript retention (decision 5) is policy here: which books, and when.
// What a purge deletes and keeps is scratch.PurgeTranscripts'.

// finished reports whether an engine book is out of the scheduler's hands: done,
// or failed (cancelled included). Only such a book's transcripts are swept.
func finished(st, status string) bool {
	return state.IsTerminal(state.State(st)) || state.Status(status) == state.StatusFailed
}

// purge deletes one finished book's transcripts for the sweep, holding the engine
// book while it does: one the engine holds (a description reading it) is left for
// a later sweep, and swept reports false.
func (h *Handler) purge(ctx context.Context, b store.SweepBook) (swept bool, err error) {
	release, ok := h.hold(b.ID)
	if !ok {
		return false, nil
	}
	defer release()
	return h.purgeHeld(ctx, b.ID, nil)
}

// purgeHeld deletes one held book's transcripts, updates its disk gauge and records
// the sweep, so the daily sweep never looks at it again. The book is looked at
// again under the hold: one a job follows again since it was found finished (a
// later claim of its job re-opening and re-admitting it while the purge waited
// for the hold) runs on its transcripts, so it is left alone, and swept reports
// false. ending, when set, is the job whose end purges: another claim of its id
// recorded since (store.OtherClaim: a later attempt, or another server's claim)
// follows the book before its attach reopens the mapping, so it keeps the
// transcripts too.
func (h *Handler) purgeHeld(ctx context.Context, bookID int64, ending *jobs.Job) (swept bool, err error) {
	b, err := h.o.DB.GetBook(ctx, bookID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil || !finished(b.State, b.Status) {
		return false, err
	}
	if followed, err := h.o.DB.GenerateBookFollowed(ctx, bookID); err != nil || followed {
		return false, err
	}
	if ending != nil {
		if other, err := h.o.DB.OtherClaim(ctx, stopGuard(*ending, false)); err != nil || other {
			return false, err
		}
	}
	if err := scratch.PurgeTranscripts(h.o.WorkRoot, b.WorkDir, state.ParseKind(b.Kind)); err != nil {
		return false, err
	}
	// The disk gauge follows what is left (the durables).
	if size, err := scratch.DirSize(b.WorkDir); err == nil {
		_ = h.o.DB.UpdateScratchBytes(ctx, b.ID, size)
	}
	return true, h.o.DB.MarkGenerateBookSwept(ctx, b.ID, h.now())
}

// purgeEnded deletes the transcripts of job's own engine book as job ends
// (transcripts.retention_days 0), unless another claim of job's id follows it
// (purgeHeld). It waits a few polls for the engine to let go of the book (a stage
// the job's end just stopped is still finishing); one it keeps is left for the
// sweep.
func (h *Handler) purgeEnded(ctx context.Context, job jobs.Job, bookID int64) error {
	b, err := h.o.DB.GetBook(ctx, bookID)
	if err != nil || !finished(b.State, b.Status) {
		return err
	}
	release, err := h.holdBook(ctx, b.ID)
	if errors.Is(err, errBookBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	_, err = h.purgeHeld(ctx, b.ID, &job)
	return err
}

// SweepTranscripts deletes the transcripts of every finished engine book whose
// job ended at least retention ago and that wasn't swept yet (one the engine
// holds is left for the next sweep), and returns how many it swept.
func (h *Handler) SweepTranscripts(ctx context.Context, retention time.Duration) (int, error) {
	books, err := h.o.DB.UnsweptGenerateBooks(ctx, h.now().Add(-retention))
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, b := range books {
		if ctx.Err() != nil {
			break
		}
		if !finished(b.State, b.Status) {
			continue
		}
		swept, err := h.purge(ctx, b)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if swept {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// fromKeptTranscript starts a fresh generate or chapters job after ASR. The
// finished run stays intact until its inputs have been checked and staged.
// A fresh book gives the new job its own costs, stage runs and filing state.
func (h *Handler) fromKeptTranscript(ctx context.Context, eng Engine, r *run, old store.Book, nb store.NewBook) (bool, error) {
	goal := old.Goal
	eligible := func(b store.Book) bool {
		return b.Goal == goal && b.State == string(state.Done) && b.Kind == string(state.KindAudio)
	}
	if h.o.RetentionDays == 0 || !eligible(old) {
		return false, nil
	}
	release, err := h.holdWithin(ctx, old.ID, giveWayTries, func() {
		r.rep.Progress(jobs.Progress{Step: stepWaiting, Code: jobs.SayEngineReleaseWait})
	})
	if errors.Is(err, errBookBusy) {
		return false, errEarlierRunBusy
	}
	if err != nil {
		return false, err
	}
	defer release()
	old, err = h.o.DB.GetBook(ctx, old.ID)
	if errors.Is(err, store.ErrNotFound) {
		// Gone while the job waited: nothing to reuse (create goes on without it).
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !eligible(old) {
		return false, errEarlierRunBusy
	}
	cutoff := h.now().Add(-time.Duration(h.o.RetentionDays) * 24 * time.Hour)
	if kept, err := h.o.DB.TranscriptRetained(ctx, old.ID, cutoff); err != nil || !kept {
		return false, err
	}
	// A run whose folder lies outside the work root (a data folder moved since)
	// is never reused nor removed here: the job clears it and starts over.
	wd, ok := scratch.Confined(h.o.WorkRoot, old.WorkDir)
	if !ok {
		return false, nil
	}
	m, err := audio.ReadManifest(wd)
	if err != nil {
		return false, nil
	}
	nb.State = string(state.Sanitizing)
	if nb.Goal == state.GoalChapters {
		// A read-along also uses windows for an unusable embedded marker
		// table. A chapters job only detects books inspect considers markerless.
		markers, duration, err := audio.ReadProbeMarkers(old.WorkDir)
		if m.Style != audio.StyleWindows || err != nil || !audio.Markerless(markers, duration) {
			return false, nil
		}
		nb.State = string(state.ChapterDetecting)
	} else if m.Style != audio.StyleMarkers && m.Style != audio.StyleFiles {
		return false, nil
	}
	current, err := pipeline.AudioCurrent(old.WorkDir, r.place.full)
	if err != nil {
		return false, fmt.Errorf("%w: %w", errAudioUnseen, err)
	}
	if !current {
		return false, nil
	}
	if kept, err := pipeline.KeptTranscription(ctx, old.WorkDir, m, h.o.ASR()); err != nil || !kept {
		return false, err
	}
	if err := h.newBatch(ctx, &nb); err != nil {
		return false, err
	}
	// Before the transaction the staged folder is disposable. The same
	// transaction that admits the new book switches cleanup to the old one.
	key := store.TranscriptCleanupPrefix + nb.BatchID
	if err := h.o.DB.SetSetting(ctx, key, nb.WorkDir); err != nil {
		return false, err
	}
	defer func() {
		if err := h.cleanTranscriptDir(context.WithoutCancel(ctx), key); err != nil {
			h.o.Log.Warn("generate: transcript cleanup needs another attempt", "batch", nb.BatchID, "err", err)
		}
	}()
	if err := pipeline.SeedTranscription(ctx, wd, nb.WorkDir, m); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	b, err := h.o.DB.ReplaceMappedBook(ctx, &old, nb, r.generateBook())
	if errors.Is(err, store.ErrNotContinuable) {
		// Another job follows the earlier run now (or another book shares its
		// folder): it isn't this job's to replace.
		return false, errEarlierRunBusy
	}
	if err != nil {
		return false, err
	}
	r.bookID = b.ID
	eng.Notify()
	return true, nil
}

// RecoverTranscriptDirs completes interrupted replacements before the engine
// accepts jobs. Only a folder explicitly recorded for cleanup is removed.
func (h *Handler) RecoverTranscriptDirs(ctx context.Context) error {
	keys, err := h.o.DB.TranscriptCleanupKeys(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, key := range keys {
		if err := h.cleanTranscriptDir(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *Handler) cleanTranscriptDir(ctx context.Context, key string) error {
	dir, found, err := h.o.DB.DisposableTranscriptDir(ctx, key)
	if err != nil || !found {
		return err
	}
	wd, ok := scratch.Confined(h.o.WorkRoot, dir)
	if !ok {
		return errors.New("transcript cleanup target is outside the work root")
	}
	// Confined answers an absolute path; a relative --data gives a relative root.
	workRoot, err := filepath.Abs(h.o.WorkRoot)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(workRoot, wd)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(workRoot)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(rel); err != nil {
		return err
	}
	return h.o.DB.DeleteSetting(ctx, key)
}
