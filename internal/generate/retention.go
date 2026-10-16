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

// purge deletes one finished book's transcripts, updates its disk gauge and
// records the sweep, so the daily sweep never looks at it again.
func (h *Handler) purge(ctx context.Context, b store.SweepBook) error {
	if err := scratch.PurgeTranscripts(h.o.WorkRoot, b.WorkDir, state.ParseKind(b.Kind)); err != nil {
		return err
	}
	// The disk gauge follows what is left (the durables).
	if size, err := scratch.DirSize(b.WorkDir); err == nil {
		_ = h.o.DB.UpdateScratchBytes(ctx, b.ID, size)
	}
	return h.o.DB.MarkGenerateBookSwept(ctx, b.ID, h.now())
}

// purgeEnded deletes the transcripts of the job's own engine book as its job
// ends (transcripts.retention_days 0).
func (h *Handler) purgeEnded(ctx context.Context, bookID int64) error {
	b, err := h.o.DB.GetBook(ctx, bookID)
	if err != nil || !finished(b.State, b.Status) {
		return err
	}
	return h.purge(ctx, store.SweepBook{ID: b.ID, WorkDir: b.WorkDir, Kind: b.Kind, State: b.State, Status: b.Status})
}

// SweepTranscripts deletes the transcripts of every finished engine book whose
// job ended at least retention ago and that wasn't swept yet, and returns how
// many it swept.
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
		if err := h.purge(ctx, b); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
