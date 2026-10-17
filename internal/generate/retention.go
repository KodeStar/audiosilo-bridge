package generate

import (
	"context"
	"errors"
	"time"

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
	return h.purgeHeld(ctx, b.ID)
}

// purgeHeld deletes one held book's transcripts, updates its disk gauge and records
// the sweep, so the daily sweep never looks at it again. The book is looked at
// again under the hold: one a job follows again since it was found finished (a
// later claim of its job re-opening and re-admitting it while the purge waited
// for the hold) runs on its transcripts, so it is left alone, and swept reports
// false.
func (h *Handler) purgeHeld(ctx context.Context, bookID int64) (swept bool, err error) {
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
	if err := scratch.PurgeTranscripts(h.o.WorkRoot, b.WorkDir, state.ParseKind(b.Kind)); err != nil {
		return false, err
	}
	// The disk gauge follows what is left (the durables).
	if size, err := scratch.DirSize(b.WorkDir); err == nil {
		_ = h.o.DB.UpdateScratchBytes(ctx, b.ID, size)
	}
	return true, h.o.DB.MarkGenerateBookSwept(ctx, b.ID, h.now())
}

// purgeEnded deletes the transcripts of the job's own engine book as its job
// ends (transcripts.retention_days 0). It waits a few polls for the engine to let
// go of the book (a stage the job's end just stopped is still finishing); one it
// keeps is left for the sweep.
func (h *Handler) purgeEnded(ctx context.Context, bookID int64) error {
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
	_, err = h.purgeHeld(ctx, b.ID)
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
