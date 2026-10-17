package generate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// DescribeKind is the description-only job (BRIDGE-PLAN.md Phase 1c): an
// own-words description for a book the Bridge already generated, from the notes
// its run kept (no new speech recognition), written as the book's
// .audiosilo/description.json.
const DescribeKind = "sidecars.describe"

// describeFailure is a job failure worded for the console as it is.
type describeFailure string

func (f describeFailure) Error() string { return string(f) }

// The ways a description-only job fails that need an admin's words.
const (
	errDescribeNoNotes describeFailure = "The Bridge has no notes for this book (it was not generated here, or they were removed). " +
		"Generate characters and recaps again to get a description."
	errDescribeGenerating describeFailure = "A generate job is still running for this book; it writes the description when it finishes."
	errDescribeChecks     describeFailure = "Couldn't write a description that passed the checks."
	errBookBusy           describeFailure = "The engine is busy with this book; try again in a minute."
	errDescribeOneChapter describeFailure = "This book has a single chapter, so no description could leave out its ending."
)

// holdTries bounds the wait for the engine's hold on a book (a stage just
// finishing or re-admitted, a purge): that many polls, then the description (or a
// job end's transcript purge) gives up.
const holdTries = 20

// DescribeJob runs sidecars.describe. It shares the generate handler's parts:
// the checked mapping, the book's folder, its parks, its Retry.
type DescribeJob struct{ h *Handler }

// DescribeJob is the sidecars.describe handler beside this one.
func (h *Handler) DescribeJob() *DescribeJob { return &DescribeJob{h: h} }

// Kind is sidecars.describe.
func (d *DescribeJob) Kind() string { return DescribeKind }

// Lane is the pipeline lane: a park (a rate limit, the budget) can hold the job
// for a long time.
func (d *DescribeJob) Lane() string { return jobs.LanePipeline }

// DescribeJobResult is what a done description-only job reports.
type DescribeJobResult struct {
	LibraryID      int64    `json:"library_id"`
	Path           string   `json:"path"`
	Description    bool     `json:"description"`
	Files          []string `json:"files"`
	CostUSD        float64  `json:"cost_usd"`
	CheckedAgainst string   `json:"checked_against"`
}

// ParseDescribeParams checks a description-only job's params: nothing is asked
// for, and a field a later server adds is ignored, so anything but a JSON object
// (or none) is refused.
func ParseDescribeParams(raw json.RawMessage) error { return parseObjectParams(raw) }

// parseObjectParams accepts no params or any JSON object (a kind that asks for
// nothing, read leniently so a field a later server adds is ignored).
func parseObjectParams(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("bad params: %w", err)
	}
	return nil
}

// Run checks the book as a generate job does, finds the engine book whose notes
// it reads, writes the description (parking for the agent's parks) and saves it
// with the book.
func (d *DescribeJob) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	h := d.h
	if err := ParseDescribeParams(job.Params); err != nil {
		return nil, err
	}
	r, stop, err := h.startRun(job, rep, DescribeKind)
	if err != nil {
		return nil, err
	}
	defer stop()
	if _, err := h.waitEngine(ctx, r); err != nil {
		return nil, err
	}
	desc := h.describer()
	if desc == nil {
		return nil, errors.New("the Bridge's engine can't write descriptions")
	}
	// Strict when new: nothing is spent yet, so a library that isn't mapped and
	// writable fails the job at once. A resumed one (its description may be paid
	// for already) parks library_not_writable and waits, as a resumed generate job
	// does.
	if err := h.prepare(ctx, r, !job.Resumed); err != nil {
		return nil, err
	}
	b, err := h.notesBook(ctx, r)
	if err != nil {
		return nil, err
	}
	// The job has its engine book now: from here a park of its own is lifted by
	// the step that clears it (the save that goes through), never between two tries
	// (unparkOwn), which the server would hear as a new park each time.
	r.bookID = b.ID
	got, spent, err := h.describeParked(ctx, r, desc, b.ID)
	if err != nil {
		return nil, err
	}
	files, err := h.saveDescription(ctx, r, got, spent)
	if err != nil {
		return nil, err
	}
	out := DescribeJobResult{LibraryID: r.lib.ID, Path: r.rel, Description: true, Files: files, CostUSD: Cents(spent),
		CheckedAgainst: got.res.CheckedAgainst}
	r.rep.Progress(jobs.Progress{Step: string(state.Done), Message: "Finished", Done: 1, Total: 1,
		Extra: map[string]any{"stage": string(state.Done), "source": r.source, "cost_usd": out.CostUSD}})
	return out, nil
}

// notesBook is the engine book whose notes describe the book: the latest run at
// its source path, else the latest generate job's book at its library path (a book
// that moved while that job ran), else the run its own bridge.json names (a book
// that moved after). None, or one still running, fails the job.
func (h *Handler) notesBook(ctx context.Context, r *run) (store.Book, error) {
	b, err := h.bookAtSource(ctx, r)
	if errors.Is(err, store.ErrNotFound) {
		b, err = h.latestGenerated(ctx, r)
	}
	if errors.Is(err, store.ErrNotFound) {
		b, err = h.namedRun(ctx, r)
	}
	if errors.Is(err, store.ErrNotFound) {
		return store.Book{}, errDescribeNoNotes
	}
	if err != nil {
		return store.Book{}, err
	}
	open, err := h.o.DB.OpenGenerateJobsForBook(ctx, b.ID, 0)
	if err != nil {
		return store.Book{}, err
	}
	// A book the engine still has queued or running (no park, not done) is
	// working too, whether or not a job follows it.
	if len(open) > 0 || (b.Status == "" && state.State(b.State) != state.Done) {
		return store.Book{}, errDescribeGenerating
	}
	r.sourceOf(b)
	return b, nil
}

// bookAtSource is the engine's book at the book's source path (store.ErrNotFound
// for none, or a path outside the library's folder).
func (h *Handler) bookAtSource(ctx context.Context, r *run) (store.Book, error) {
	sp, ok, err := metaops.AllowedPath(r.place.full, []string{r.mount})
	if err != nil || !ok {
		return store.Book{}, store.ErrNotFound
	}
	return h.o.DB.GetBookBySourcePath(ctx, sp)
}

// latestGenerated is the latest generate job's surviving engine book at the book's
// library path (store.ErrNotFound for none). A library id and path are the
// server's, and repeat across servers: a run made for another server's library
// (the Bridge paired anew) lies outside this library's folder, so it is none.
func (h *Handler) latestGenerated(ctx context.Context, r *run) (store.Book, error) {
	id, err := h.o.DB.LatestGenerateBookAt(ctx, r.lib.ID, r.rel)
	if err != nil {
		return store.Book{}, err
	}
	b, err := h.o.DB.GetBook(ctx, id)
	if err != nil {
		return store.Book{}, err
	}
	if _, ok, err := metaops.AllowedPath(b.SourcePath, []string{r.mount}); err != nil || !ok {
		return store.Book{}, store.ErrNotFound
	}
	return b, nil
}

// namedRun is the engine book of the generate job the book's own bridge.json
// names (store.ErrNotFound for none): a book that moved after its job ended took
// that file along, while the Bridge's records (the book's source path, the job's
// mapping) stayed at the old place. Only this library's run, at a source inside its
// folder, counts; the job's mapping follows the book from then on.
func (h *Handler) namedRun(ctx context.Context, r *run) (store.Book, error) {
	prov := readProvenance(filepath.Join(r.place.dir, AttachDir), r.place.prefix+provenanceName)
	var kind string
	var jobID int64
	if prov == nil || json.Unmarshal(prov["kind"], &kind) != nil || json.Unmarshal(prov["job_id"], &jobID) != nil ||
		kind != Kind || jobID == 0 {
		return store.Book{}, store.ErrNotFound
	}
	m, err := h.o.DB.GetGenerateBook(ctx, jobID)
	if err != nil {
		return store.Book{}, err
	}
	if m.BookID == nil || m.LibraryID != r.lib.ID {
		return store.Book{}, store.ErrNotFound
	}
	b, err := h.o.DB.GetBook(ctx, *m.BookID)
	if err != nil {
		return store.Book{}, err
	}
	if _, ok, err := metaops.AllowedPath(b.SourcePath, []string{r.mount}); err != nil || !ok {
		return store.Book{}, store.ErrNotFound
	}
	if err := h.o.DB.SetGenerateBookPath(ctx, jobID, r.rel); err != nil {
		h.o.Log.Warn("generate: recording the book's new path failed", "job", jobID, "err", err)
	}
	return b, nil
}

// described is a description the step wrote (or reused), read back from the
// engine's work dir while the book was held.
type described struct {
	res  pipeline.DescribeResult
	raw  []byte
	work string
}

// describeHeld runs the description step under the engine's hold on the book
// (waiting a few polls for it; errBookBusy when it never comes) and reads the
// description back before letting go, so no stage, delete, purge or sweep acts on
// the book meanwhile. The result carries the step's spend whatever happened.
func (h *Handler) describeHeld(ctx context.Context, d Describer, bookID int64, noWait bool) (described, error) {
	release, err := h.holdBook(ctx, bookID)
	if err != nil {
		return described{}, err
	}
	defer release()
	res, err := d.Describe(ctx, bookID, noWait)
	if err != nil {
		return described{res: res}, err
	}
	raw, desc, err := readDescription(res.Path)
	if err == nil && raw == nil {
		err = errors.New("the engine kept no description")
	}
	return described{res: res, raw: raw, work: desc.Work}, err
}

// holdBook takes the engine's hold on an engine book, trying again every poll
// for holdTries polls before giving up (errBookBusy).
func (h *Handler) holdBook(ctx context.Context, bookID int64) (func(), error) {
	for i := 1; ; i++ {
		if release, ok := h.hold(bookID); ok {
			return release, nil
		}
		if i == holdTries {
			return nil, errBookBusy
		}
		if err := agent.SleepCtx(ctx, h.o.Poll); err != nil {
			return nil, err
		}
	}
}

// describeParked runs the description step until it writes one: an agent park
// (a rate limit, the budget, a refused credential, no agent) parks the job and
// tries again at the park's retry_at or on Retry; the job stays parked while it
// tries, so a park that holds isn't announced again. It returns the spend across
// the tries.
func (h *Handler) describeParked(ctx context.Context, r *run, d Describer, bookID int64) (described, float64, error) {
	var spent float64
	parked := false
	for {
		if !parked {
			r.rep.Progress(jobs.Progress{Step: pipeline.DescribeStage, Message: "Writing a description", Total: 1,
				Extra: r.describeExtra(spent)})
		}
		got, err := h.describeHeld(ctx, d, bookID, false)
		spent += got.res.CostUSD
		var park *scheduler.ParkError
		switch {
		case err == nil:
			// Lifts this loop's park, and one the runner restored for a job that
			// resumed after a restart (nothing when there is none).
			r.rep.Unpark()
			return got, spent, nil
		case ctx.Err() != nil:
			return described{}, spent, ctx.Err()
		case errors.Is(err, errBookBusy):
			return described{}, spent, err
		case errors.Is(err, pipeline.ErrNoNotes):
			return described{}, spent, errDescribeNoNotes
		case errors.Is(err, pipeline.ErrOneChapter):
			return described{}, spent, errDescribeOneChapter
		case errors.As(err, &park) && park.Code == state.ParkAgentValidationExhausted:
			h.o.Log.Warn("generate: the description kept failing its checks", "job", r.job.ID, "err", err)
			return described{}, spent, errDescribeChecks
		case errors.As(err, &park):
			parked = true
			if !park.RetryAfter.IsZero() {
				// A Retry pressed while that try ran was answered by it: only one
				// pressed once this park is reported tries again before its
				// retry_at. (A park without one waits for a press, so a press made
				// meanwhile, after raising the budget say, is kept.)
				select {
				case <-r.poke:
				default:
				}
			}
			h.parkDescribe(r, park, spent)
			if err := h.recheckWait(ctx, r, park.RetryAfter); err != nil {
				return described{}, spent, err
			}
		default:
			return described{}, spent, fmt.Errorf("could not write a description: %w", err)
		}
	}
}

// parkDescribe reports an agent park as the job's, with its hint and retry_at.
func (h *Handler) parkDescribe(r *run, park *scheduler.ParkError, spent float64) {
	code, retryAt := string(park.Code), ""
	if !park.RetryAfter.IsZero() {
		retryAt = store.Timestamp(park.RetryAfter)
	}
	extra := r.describeExtra(spent)
	extra["park"] = parkExtra(code, retryAt)
	r.rep.Progress(jobs.Progress{Step: pipeline.DescribeStage, Message: "Needs attention: Writing a description", Total: 1,
		Extra: extra})
	r.rep.Park(code, park.Reason)
}

// describeExtra is a description-only job's progress Extra: the describing
// stage, the source and the spend so far.
func (r *run) describeExtra(spent float64) map[string]any {
	return map[string]any{"stage": pipeline.DescribeStage, "source": r.source, "cost_usd": Cents(spent)}
}

// saveDescription writes the description and bridge.json's description object
// into the book's .audiosilo/ at the path it is at now, through the mapping
// checked again, parking book_not_writable until it can; then asks the server to
// re-read the attachments. It returns the names written.
func (h *Handler) saveDescription(ctx context.Context, r *run, got described, spent float64) ([]string, error) {
	err := h.save(ctx, r, saveSpec{
		what: "the description",
		wait: true,
		progress: jobs.Progress{Step: stepWriting, Message: "Saving the description with the book", Done: 1, Total: 1,
			Extra: r.describeExtra(spent)},
		files: func(p place) ([]attachFile, error) {
			fresh := provenance(h.o.Version, DescribeKind, r.job.ID, h.now(), r.source, got.work, inputsOf(r.mount, r.files, r.epub))
			fresh.Description = descriptionProvenance(r.job.ID, got.res)
			prov, err := describedProvenance(p.dir, p.prefix, fresh)
			if err != nil {
				return nil, err
			}
			return []attachFile{{descriptionName, got.raw}, {provenanceName, prov}}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	h.o.Log.Info("generate: wrote the book's description", "job", r.job.ID, "path", r.rel)
	r.rep.Progress(jobs.Progress{Step: stepRescanning, Message: "Asking the server to read it", Done: 1, Total: 1,
		Extra: r.describeExtra(spent)})
	h.rescan(ctx, r)
	return attachmentNames(r.place.prefix, sidecars{description: got.raw}), nil
}

// describer is the engine's description step (nil until it started).
func (h *Handler) describer() Describer {
	if h.o.Describer == nil {
		return nil
	}
	return h.o.Describer()
}

// describeOnce runs the description step once per run of a generate job, when the
// engine's sidecars are final (done, or parked at contributing). It never fails,
// parks or holds the job: a rate limit is not waited out, an engine that keeps the
// book (a few polls) is given up on, and any error leaves the job to finish with
// its characters and recaps, its result saying why there is no description. A run
// that asked at a contributing park asks once more at done: the contributing
// stage may have resolved the work and sources since, and the step re-composes the
// kept text with them (nothing spent), so the description never ships beside
// characters and recaps that name another work; a try at the park that failed
// without spending (a rate limit, the budget, a busy engine) gets its second go.
// It reports whether it asked (the description may have changed).
func (h *Handler) describeOnce(ctx context.Context, r *run, b store.Book) bool {
	done := state.State(b.State) == state.Done
	if r.descTried && (!done || r.descDone || (r.desc == nil && r.descPaid)) {
		return false
	}
	r.descTried, r.descDone = true, done
	if r.desc == nil && h.describeFailedPaid(ctx, b.ID) {
		// The book's last try (the engine's at the start of contributing, or an
		// earlier run of this job's) spent and failed: the run doesn't pay twice,
		// and an ebook's second try at done would be checked against nothing.
		r.descErr, r.descPaid = "an earlier try at the description failed (see the Bridge's log)", true
		return false
	}
	d := h.describer()
	if d == nil {
		r.descErr = "the Bridge's engine can't write descriptions"
		return false
	}
	r.rep.Progress(jobs.Progress{Step: pipeline.DescribeStage, Message: "Writing a description",
		Extra: r.bookExtra(b)})
	got, err := h.describeHeld(ctx, d, b.ID, true)
	if err != nil && r.desc != nil {
		// The re-compose at done failed: the park's description stands (as the
		// park had its work).
		h.o.Log.Warn("generate: the description wasn't composed again at done; the park's stands", "job", r.job.ID, "err", err)
		return false
	}
	if err != nil {
		r.descErr, r.descPaid = descriptionErrorText(err), got.res.CostUSD > 0
		h.o.Log.Warn("generate: no description; the job finishes without one", "job", r.job.ID, "err", err)
		return true
	}
	r.desc, r.descRaw, r.descErr = &got.res, got.raw, ""
	return true
}

// describeFailedPaid reports whether the book's latest describing run failed
// after it spent.
func (h *Handler) describeFailedPaid(ctx context.Context, bookID int64) bool {
	runs, err := h.o.DB.ListStageRuns(ctx, bookID)
	if err != nil {
		return false
	}
	for _, run := range slices.Backward(runs) {
		if run.Stage != pipeline.DescribeStage {
			continue
		}
		if run.Ok == nil || *run.Ok {
			return false
		}
		cost, err := h.o.DB.StageRunBudgetCost(ctx, run.ID)
		return err == nil && cost > 0
	}
	return false
}

// descriptionErrorText is a generate result's short plain reason for having no
// description.
func descriptionErrorText(err error) string {
	var park *scheduler.ParkError
	switch {
	case errors.Is(err, pipeline.ErrNoNotes):
		return "the Bridge has no notes for this book"
	case errors.Is(err, pipeline.ErrOneChapter):
		return "the book has a single chapter, so no description leaves out its ending"
	case errors.Is(err, errBookBusy):
		return "the engine was busy with this book"
	case errors.As(err, &park):
		switch park.Code {
		case state.ParkAgentRateLimited:
			return "the agent account hit its usage limit"
		case state.ParkBudgetExceeded:
			return "the agent budget is spent"
		case state.ParkAgentCredentialRefused:
			return "the agent's credential was refused"
		case state.ParkAgentUnavailable:
			return "the agent isn't available"
		case state.ParkAgentValidationExhausted:
			return "the description kept failing its checks"
		}
		return "the agent stopped (" + string(park.Code) + ")"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "the job stopped while it was being written"
	}
	return "the description step failed (see the Bridge's log)"
}
