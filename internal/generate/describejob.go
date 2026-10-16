package generate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

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

// stepDescribing is the description step, as a job's progress names it.
const stepDescribing = pipeline.DescribeStage

// describeFailure is a job failure worded for the console as it is.
type describeFailure string

func (f describeFailure) Error() string { return string(f) }

// The ways a description-only job fails that need an admin's words.
const (
	errDescribeNoNotes describeFailure = "The Bridge has no notes for this book (it was not generated here, or they were removed). " +
		"Generate characters and recaps again to get a description."
	errDescribeGenerating describeFailure = "A generate job is still running for this book; it writes the description when it finishes."
	errDescribeChecks     describeFailure = "Couldn't write a description that passed the checks."
)

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
func ParseDescribeParams(raw json.RawMessage) error {
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
	if job.LibraryID == nil || job.Path == nil || strings.TrimSpace(*job.Path) == "" {
		return nil, errors.New("sidecars.describe needs a library and a book path")
	}
	r := &run{job: job, rep: rep, rel: *job.Path, jobPath: *job.Path, poke: make(chan struct{}, 1)}
	if p := rep.Path(); p != "" {
		r.rel, r.jobPath = p, p
	}
	h.setPoke(job.ID, r.poke)
	defer h.dropPoke(job.ID, r.poke)
	rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Starting"})
	if _, err := h.waitEngine(ctx, r); err != nil {
		return nil, err
	}
	desc := h.describer()
	if desc == nil {
		return nil, errors.New("the Bridge's engine can't write descriptions")
	}
	// Strict: nothing is spent yet, so a library that isn't mapped and writable
	// fails the job at once instead of waiting.
	if err := h.prepare(ctx, r, true); err != nil {
		return nil, err
	}
	b, err := h.notesBook(ctx, r)
	if err != nil {
		return nil, err
	}
	h.markDescribing(b.ID, 1)
	defer h.markDescribing(b.ID, -1)
	res, spent, err := h.describeParked(ctx, r, desc, b.ID)
	if err != nil {
		return nil, err
	}
	files, err := h.saveDescription(ctx, r, res, spent)
	if err != nil {
		return nil, err
	}
	out := DescribeJobResult{LibraryID: r.lib.ID, Path: r.rel, Description: true, Files: files, CostUSD: Cents(spent),
		CheckedAgainst: res.CheckedAgainst}
	r.rep.Progress(jobs.Progress{Step: string(state.Done), Message: "Finished", Done: 1, Total: 1,
		Extra: map[string]any{"stage": string(state.Done), "source": r.source, "cost_usd": out.CostUSD}})
	return out, nil
}

// notesBook is the engine book whose notes describe the book: the latest run at
// its source path, else the latest generate job's book at its library path (a book
// that moved since). None, or one still running, fails the job.
func (h *Handler) notesBook(ctx context.Context, r *run) (store.Book, error) {
	var b store.Book
	found := false
	if sp, ok, err := metaops.AllowedPath(r.place.full, []string{r.mount}); err == nil && ok {
		if got, err := h.o.DB.GetBookBySourcePath(ctx, sp); err == nil {
			b, found = got, true
		} else if !errors.Is(err, store.ErrNotFound) {
			return store.Book{}, err
		}
	}
	if !found {
		id, err := h.o.DB.LatestGenerateBookAt(ctx, r.lib.ID, r.rel)
		if errors.Is(err, store.ErrNotFound) {
			return store.Book{}, errDescribeNoNotes
		}
		if err != nil {
			return store.Book{}, err
		}
		if b, err = h.o.DB.GetBook(ctx, id); errors.Is(err, store.ErrNotFound) {
			return store.Book{}, errDescribeNoNotes
		} else if err != nil {
			return store.Book{}, err
		}
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
	if b.Kind == string(state.KindEbook) {
		r.source, r.epub = SourceEbook, b.EbookPath
	} else {
		r.source = SourceAudio
	}
	return b, nil
}

// describeParked runs the description step until it writes one: an agent park
// (a rate limit, the budget, a refused credential, no agent) parks the job and
// tries again at the park's retry_at or on Retry; the job stays parked while it
// tries, so a park that holds isn't announced again. It returns the spend across
// the tries.
func (h *Handler) describeParked(ctx context.Context, r *run, d Describer, bookID int64) (pipeline.DescribeResult, float64, error) {
	var spent float64
	parked := false
	for {
		if !parked {
			r.rep.Progress(jobs.Progress{Step: stepDescribing, Message: "Writing a description", Total: 1,
				Extra: map[string]any{"stage": stepDescribing, "source": r.source, "cost_usd": Cents(spent)}})
		}
		res, err := d.Describe(ctx, bookID, pipeline.DescribeReport{})
		spent += res.CostUSD
		var park *scheduler.ParkError
		switch {
		case err == nil:
			if parked {
				r.rep.Unpark()
			}
			return res, spent, nil
		case ctx.Err() != nil:
			return pipeline.DescribeResult{}, spent, ctx.Err()
		case errors.Is(err, pipeline.ErrNoNotes):
			return pipeline.DescribeResult{}, spent, errDescribeNoNotes
		case errors.As(err, &park) && park.Code == state.ParkAgentValidationExhausted:
			h.o.Log.Warn("generate: the description kept failing its checks", "job", r.job.ID, "err", err)
			return pipeline.DescribeResult{}, spent, errDescribeChecks
		case errors.As(err, &park):
			parked = true
			h.parkDescribe(r, park, spent)
			if err := h.parkWait(ctx, r, park.RetryAfter); err != nil {
				return pipeline.DescribeResult{}, spent, err
			}
		default:
			return pipeline.DescribeResult{}, spent, fmt.Errorf("could not write a description: %w", err)
		}
	}
}

// parkDescribe reports an agent park as the job's, with its hint and retry_at.
func (h *Handler) parkDescribe(r *run, park *scheduler.ParkError, spent float64) {
	code := string(park.Code)
	p := map[string]any{"code": code, "hint": ParkHint(code)}
	if !park.RetryAfter.IsZero() {
		p["retry_at"] = store.Timestamp(park.RetryAfter)
	}
	r.rep.Progress(jobs.Progress{Step: stepDescribing, Message: "Needs attention: Writing a description", Total: 1,
		Extra: map[string]any{"stage": stepDescribing, "source": r.source, "cost_usd": Cents(spent), "park": p}})
	r.rep.Park(code, park.Reason)
}

// parkWait waits out an agent park: until at (none: only a Retry), a Retry, or
// the job's end.
func (h *Handler) parkWait(ctx context.Context, r *run, at time.Time) error {
	var timer <-chan time.Time
	if !at.IsZero() {
		t := time.NewTimer(max(time.Until(at), 0))
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.poke:
	case <-timer:
	}
	return nil
}

// saveDescription writes the description and bridge.json's description object
// into the book's .audiosilo/ at the path it is at now, through the mapping
// checked again, parking book_not_writable until it can; then asks the server to
// re-read the attachments. It returns the names written.
func (h *Handler) saveDescription(ctx context.Context, r *run, res pipeline.DescribeResult, spent float64) ([]string, error) {
	raw, err := readAttachment(res.Path)
	if err == nil && raw == nil {
		err = errors.New("the engine kept no description")
	}
	if err != nil {
		return nil, err
	}
	var d model.Description
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("the engine's description.json doesn't decode: %w", err)
	}
	for {
		if err := h.waitMapped(ctx, r, false); err != nil {
			return nil, err
		}
		if from, moved := h.adoptMove(ctx, r); moved {
			h.o.Log.Info("generate: the book moved; the job follows it", "job", r.job.ID, "from", from, "to", r.rel)
		}
		err := h.writeDescriptionOnce(r, raw, d.Work, res, spent)
		if err == nil {
			break
		}
		h.park(r, parkBookNotWritable, "the Bridge can't save the description with the book: "+err.Error())
		if err := h.recheckWait(ctx, r); err != nil {
			return nil, err
		}
	}
	if r.ownPark {
		r.rep.Unpark()
		r.ownPark = false
	}
	h.o.Log.Info("generate: wrote the book's description", "job", r.job.ID, "path", r.rel)
	r.rep.Progress(jobs.Progress{Step: stepRescanning, Message: "Asking the server to read it", Done: 1, Total: 1,
		Extra: map[string]any{"stage": stepDescribing, "source": r.source, "cost_usd": Cents(spent)}})
	h.rescan(ctx, r)
	return []string{r.place.prefix + descriptionName, r.place.prefix + provenanceName}, nil
}

// writeDescriptionOnce locates the book and writes description.json and its
// bridge.json, its error worded without the Bridge's own paths.
func (h *Handler) writeDescriptionOnce(r *run, raw []byte, workID string, res pipeline.DescribeResult, spent float64) error {
	p, err := locate(r.mount, r.rel)
	if err != nil {
		return err
	}
	r.place = p
	r.rep.Progress(jobs.Progress{Step: stepWriting, Message: "Saving the description with the book", Done: 1, Total: 1,
		Extra: map[string]any{"stage": stepDescribing, "source": r.source, "cost_usd": Cents(spent)}})
	fresh := provenance(h.o.Version, DescribeKind, r.job.ID, h.now(), r.source, workID, inputsOf(r.mount, r.files, r.epub))
	fresh.Description = descriptionProvenance(r.job.ID, res)
	prov, err := describedProvenance(p.dir, p.prefix, fresh)
	if err == nil {
		err = writeAttachFiles(p.dir, p.prefix, []attachFile{{descriptionName, raw}, {provenanceName, prov}})
	}
	if err != nil {
		return fmt.Errorf("its %s folder can't be written (%s)", AttachDir, writeProblem(err))
	}
	return nil
}

// describer is the engine's description step (nil until it started).
func (h *Handler) describer() Describer {
	if h.o.Describer == nil {
		return nil
	}
	return h.o.Describer()
}

// markDescribing counts a description step on an engine book in (+1) or out (-1).
func (h *Handler) markDescribing(bookID int64, delta int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.describing[bookID] += delta; h.describing[bookID] <= 0 {
		delete(h.describing, bookID)
	}
}

// isDescribing reports whether a description step reads the engine book now.
func (h *Handler) isDescribing(bookID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.describing[bookID] > 0
}

// describeOnce runs the description step once per run of a generate job, when the
// engine's sidecars are final (done, or parked at contributing). It never fails,
// parks or holds the job: a rate limit is not waited out, and any error leaves the
// job to finish with its characters and recaps, its result saying why there is no
// description.
func (h *Handler) describeOnce(ctx context.Context, r *run, b store.Book) {
	if r.descTried {
		return
	}
	r.descTried = true
	d := h.describer()
	if d == nil {
		r.descErr = "the Bridge's engine can't write descriptions"
		return
	}
	r.rep.Progress(jobs.Progress{Step: stepDescribing, Message: "Writing a description",
		Extra: map[string]any{"stage": b.State, "source": r.source}})
	h.markDescribing(b.ID, 1)
	defer h.markDescribing(b.ID, -1)
	res, err := d.Describe(ctx, b.ID, pipeline.DescribeReport{NoWait: true})
	if err != nil {
		r.descErr = descriptionErrorText(err)
		h.o.Log.Warn("generate: no description; the job finishes without one", "job", r.job.ID, "err", err)
		return
	}
	r.desc = &res
}

// descriptionErrorText is a generate result's short plain reason for having no
// description.
func descriptionErrorText(err error) string {
	var park *scheduler.ParkError
	switch {
	case errors.Is(err, pipeline.ErrNoNotes):
		return "the Bridge has no notes for this book"
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
