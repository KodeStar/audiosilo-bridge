package generate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// ContributeKind is "Share with the community" (BRIDGE-PLAN.md Phase 2b, plan
// D2): the characters and recaps the Bridge generated for a book, kept with its
// engine book, filed to the community repository as intake issues with the
// Bridge's GitHub sign-in. It writes nothing into the library and runs no agent.
const ContributeKind = "sidecars.contribute"

// Sharer is the engine's share step (pipeline.Executor.ShareChecked: stopped,
// nil for never, is asked before each dimension is filed).
type Sharer interface {
	ShareChecked(ctx context.Context, bookID int64, workID string, dims []string,
		stopped func() (bool, error)) (pipeline.ShareResult, error)
}

// The ways a share fails that need an admin's words.
const (
	errShareCantContribute describeFailure = "The Bridge isn't signed in to GitHub: sign in on the Bridge's Accounts " +
		"tab, then share the book again."
	errShareUnrefreshed describeFailure = "The Bridge's GitHub sign-in couldn't be refreshed (" +
		contrib.UnrefreshedWhy + "): share the book again once GitHub answers."
	errShareRefused describeFailure = "GitHub refused the Bridge's sign-in: sign in again on the Bridge's Accounts " +
		"tab, then share the book again."
	errShareNothing describeFailure = "The Bridge keeps no characters or recaps for this book (it was not generated " +
		"here, or they were removed). Generate them first."
	errShareGenerating    describeFailure = "A generate job is still running for this book; share it once it finishes."
	errShareNotMatched    describeFailure = "Match the book first: it isn't matched to a work in the community database."
	errShareFilingUnknown describeFailure = "An earlier try at filing this book's sidecars stopped before GitHub " +
		"answered, and the Bridge can't tell yet whether GitHub made the issue (or the gist it links), so it filed " +
		"nothing more: share the book again in a few minutes, and if this keeps happening, look for the issue on " +
		"the community repository."
)

// ContributeParams are sidecars.contribute's params: the community work the
// results are for (else the book's own match) and the dimensions to share
// (both when none).
type ContributeParams struct {
	WorkID     string   `json:"work_id,omitempty"`
	Dimensions []string `json:"dimensions,omitempty"`
}

// ParseContributeParams checks the params: a field a later server adds is
// ignored, a dimension is characters or recaps (each once), a work id one line
// within checkWorkID's bounds.
func ParseContributeParams(raw json.RawMessage) (ContributeParams, error) {
	var p ContributeParams
	if len(raw) == 0 || string(raw) == "null" {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("bad params: %w", err)
	}
	p.WorkID = strings.TrimSpace(p.WorkID)
	if err := checkWorkID(p.WorkID); err != nil {
		return p, fmt.Errorf("bad params: %w", err)
	}
	for i, d := range p.Dimensions {
		if (d != pipeline.ShareCharacters && d != pipeline.ShareRecaps) || slices.Contains(p.Dimensions[:i], d) {
			return p, errors.New("bad params: dimensions are characters and recaps, each once")
		}
	}
	return p, nil
}

// ContributeResult is what a done share reports: each dimension's issue (one
// filed earlier included) and the dimensions the work already carries.
type ContributeResult struct {
	LibraryID   int64             `json:"library_id"`
	Path        string            `json:"path"`
	WorkID      string            `json:"work_id"`
	Contributed map[string]string `json:"contributed"`
	Covered     []string          `json:"covered"`
}

// ContributeJob runs sidecars.contribute beside the generate handler.
type ContributeJob struct{ h *Handler }

// ContributeJob is the sidecars.contribute handler beside this one.
func (h *Handler) ContributeJob() *ContributeJob { return &ContributeJob{h: h} }

// Kind is sidecars.contribute.
func (c *ContributeJob) Kind() string { return ContributeKind }

// Lane is the light lane: a few reads and HTTP calls.
func (c *ContributeJob) Lane() string { return jobs.LaneLight }

// Run finds the book's engine book (as sidecars.describe does), holds it, and
// files its sidecars.
func (c *ContributeJob) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	h := c.h
	p, err := ParseContributeParams(job.Params)
	if err != nil {
		return nil, err
	}
	r, stop, err := h.startRun(job, rep, ContributeKind)
	if err != nil {
		return nil, err
	}
	defer stop()
	if err := credFailure(h.o.ContribCredential(ctx), errShareUnrefreshed, errShareCantContribute); err != nil {
		return nil, err
	}
	if _, err := h.waitEngine(ctx, r); err != nil {
		return nil, err
	}
	var sharer Sharer
	if h.o.Sharer != nil {
		sharer = h.o.Sharer()
	}
	if sharer == nil {
		return nil, errors.New("the Bridge's engine can't share books")
	}
	if err := h.prepareRead(ctx, r); err != nil {
		return nil, err
	}
	b, err := h.notesBook(ctx, r)
	switch {
	case errors.Is(err, errDescribeNoNotes):
		return nil, errShareNothing
	case errors.Is(err, errDescribeGenerating):
		return nil, errShareGenerating
	case err != nil:
		return nil, err
	}
	rep.Progress(jobs.Progress{Step: stepSharing, Message: "Sharing with the community", Total: 1})
	res, err := h.shareConfirmed(ctx, r, sharer, b.ID, p, nil)
	if err != nil {
		// The console gets shareError's words; the log keeps the cause.
		h.o.Log.Warn("generate: the share failed", "job", job.ID, "path", r.rel, "err", err)
		return nil, shareError(err)
	}
	h.o.Log.Info("generate: shared the book's sidecars", "job", job.ID, "path", r.rel, "work", res.WorkID,
		"contributed", len(res.Contributed), "covered", len(res.Covered))
	rep.Progress(jobs.Progress{Step: string(state.Done), Message: "Shared", Done: 1, Total: 1})
	return ContributeResult{LibraryID: r.lib.ID, Path: r.rel, WorkID: res.WorkID, Contributed: res.Contributed,
		Covered: res.Covered}, nil
}

// stepSharing is a share's step while it files.
const stepSharing = "sharing"

// shareConfirmed holds the engine book and files its sidecars once the server
// confirms the share still stands: it is asked (Reporter.Refresh) just before
// filing, as the server stops a share whose Contributions module was turned
// off only in an answer to the job's next report, which can be a keepalive
// away. An answer that stopped the job, or carried share_off (errShareOffNow),
// files nothing (the job ends); so does stands (nil: none) answering false,
// asked under the hold after the server's answer (errShareOffNow; its error is
// returned). A server
// that couldn't be asked parks the job share_unconfirmed and asks again every
// Recheck, or at once on Retry, filing nothing meanwhile.
func (h *Handler) shareConfirmed(ctx context.Context, r *run, sharer Sharer, bookID int64,
	p ContributeParams, stands func(context.Context) (bool, error)) (pipeline.ShareResult, error) {
	for parked := false; ; parked = true {
		release, err := h.holdBook(ctx, bookID)
		if err != nil {
			return pipeline.ShareResult{}, err
		}
		err = r.rep.Refresh(ctx)
		off := err == nil && r.rep.ShareOff()
		if err == nil && !off && stands != nil {
			var ok bool
			if ok, err = stands(ctx); err != nil {
				release()
				return pipeline.ShareResult{}, err
			}
			off = !ok
		}
		if off {
			// A sidecars.generate run filing a share it owes (shareOwed) hears
			// share_off without being stopped: nothing is filed, and a run that
			// goes on to finish reports no park it no longer waits on.
			release()
			if parked {
				r.rep.Unpark()
			}
			return pipeline.ShareResult{}, errShareOffNow
		}
		if err == nil {
			if parked {
				r.rep.Unpark()
				r.rep.Progress(jobs.Progress{Step: stepSharing, Message: "Sharing with the community", Total: 1})
			}
			res, err := sharer.ShareChecked(ctx, bookID, p.WorkID, p.Dimensions, stoppedBy(ctx, r, stands))
			release()
			return res, err
		}
		release()
		if ctx.Err() != nil || errors.Is(err, jobs.ErrStopped) {
			// The answer stopped the job (a cancel, the module off): its context
			// has ended, and the runner reports how.
			return pipeline.ShareResult{}, cmp.Or(context.Cause(ctx), err)
		}
		h.o.Log.Warn("generate: the server couldn't be asked whether the share still stands; it waits",
			"job", r.job.ID, "err", err)
		at := time.Now().Add(h.o.Recheck)
		r.rep.Progress(jobs.Progress{Step: stepSharing, Message: "Needs attention: Sharing with the community", Total: 1,
			Extra: map[string]any{"park": parkExtra(string(state.ParkShareUnconfirmed), store.Timestamp(at))}})
		r.rep.Park(string(state.ParkShareUnconfirmed), pipeline.ShareUnconfirmedMsg)
		if err := h.recheckWait(ctx, r, at); err != nil {
			return pipeline.ShareResult{}, err
		}
	}
}

// stoppedBy is shareConfirmed's check before each dimension it files when
// stands is set (shareOwed: a done book's owed share, which sidecars.generate
// answers can turn off without stopping the job): the server asked again
// (Reporter.Refresh), its share_off or stands answering false stop the share
// (pipeline.ErrShareStopped), and a server that can't be asked is the error
// (nothing more is filed). nil (never) without stands: sidecars.contribute's
// stop comes as a cancel.
func stoppedBy(ctx context.Context, r *run, stands func(context.Context) (bool, error)) func() (bool, error) {
	if stands == nil {
		return nil
	}
	return func() (bool, error) {
		if err := r.rep.Refresh(ctx); err != nil {
			return false, err
		}
		if r.rep.ShareOff() {
			return true, nil
		}
		ok, err := stands(ctx)
		return !ok, err
	}
}

// errShareOffNow is shareConfirmed's answer when the server's answer carried
// share_off: nothing was filed.
var errShareOffNow = errors.New("the server turned the share off")

// shareError words a share's failure for the console.
func shareError(err error) error {
	var park *scheduler.ParkError
	switch {
	case errors.As(err, &park) && park.Code == state.ParkContribUnavailable:
		// The park's Cause is the credential's resolve error (pipeline's contribClient).
		if errors.Is(park.Cause, contrib.ErrSignInUnrefreshed) {
			return errShareUnrefreshed
		}
		return errShareCantContribute
	case contrib.Unauthorized(err):
		return errShareRefused
	case errors.Is(err, pipeline.ErrNotMatched):
		return errShareNotMatched
	case errors.Is(err, pipeline.ErrNoSidecars):
		return errShareNothing
	case errors.Is(err, pipeline.ErrFilingUnknown):
		return errShareFilingUnknown
	}
	return fmt.Errorf("could not share the book: %w", err)
}

// prepareRead finds the book through the library's mapping as the server lists
// it, for a job that reads and writes nothing in the library: the mapping need
// not be writable, only show the book.
func (h *Handler) prepareRead(ctx context.Context, r *run) error {
	if err := h.serverLibrary(r); err != nil {
		return err
	}
	// The mapping, else the server's own path (locate says when the book isn't
	// there).
	r.mount, _ = h.o.Mount(r.lib.ID, r.lib.Root)
	book, err := h.fetchBook(ctx, r)
	if err != nil {
		return err
	}
	r.files, r.info = book.Files, book.Info
	if book.Path != "" {
		r.rel = book.Path
	}
	p, err := locate(r.mount, r.rel)
	if err != nil {
		return err
	}
	r.place = p
	return nil
}
