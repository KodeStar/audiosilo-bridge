// Package generate is the sidecars.generate job (BRIDGE-PLAN.md Phase 1a): it
// turns a server job into an engine book (the sidecars pipeline imported from
// audiosilo-sidecars), follows that book to the end while reporting its stage,
// ETA, spend and parks to the server, then writes the validated characters and
// recaps into the book's .audiosilo/ folder and asks the server to re-read them.
//
// The engine's scheduler does the throttling: a job sits in the runner's
// pipeline lane for as long as its book is queued, running or parked. The job's
// server id maps to the engine book in generate_books, so a job resumed after a
// restart follows the same book.
package generate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Kind is the job kind.
const Kind = "sidecars.generate"

// Engine is the part of the engine's scheduler the job drives
// (*scheduler.Scheduler).
type Engine interface {
	Notify()
	Retry(ctx context.Context, id int64) error
	Cancel(ctx context.Context, id int64) error
	Delete(ctx context.Context, id int64) error
	ETASeconds(id int64) (int64, bool)
	// Hold reserves a book for work outside its stages (the description step, a
	// transcript purge); ok is false while a stage runs on it or another holds it.
	Hold(id int64) (release func(), ok bool)
}

// Describer writes a finished engine book's own-words description
// (*pipeline.Executor's Describe, Phase 1c).
type Describer interface {
	Describe(ctx context.Context, bookID int64, noWait bool) (pipeline.DescribeResult, error)
}

// Server is the part of the server link the job calls.
type Server interface {
	Book(ctx context.Context, libraryID int64, path string) (*link.Book, error)
	RescanAttachments(ctx context.Context, libraryID int64, path string) (*link.Attachments, error)
}

// Libraries are the server's libraries and their last mapping checks.
type Libraries interface {
	Library(id int64) (link.Library, bool)
	Status(id int64) (link.LibraryStatus, bool)
}

// Options wire the handler.
type Options struct {
	DB *store.DB
	// Engine is the scheduler, nil until the engine has started (the job waits).
	Engine func() Engine
	// Describer is the engine's description step, nil until the engine has
	// started (a nil func: no descriptions).
	Describer func() Describer
	// Sharer is the engine's share step for sidecars.contribute (nil until the
	// engine has started; a nil func: no shares).
	Sharer    func() Sharer
	Libraries Libraries
	Mount     mapping.MountFunc
	// Server is the server link, nil while unpaired.
	Server func() Server
	// WorkRoot is the engine's work folder (<data>/work).
	WorkRoot string
	// Version is the Bridge's, for bridge.json.
	Version string
	// RetentionDays is transcripts.retention_days: 0 deletes a finished book's
	// transcripts when its job ends (else the daily sweep does, that much later).
	RetentionDays int
	// ContribCredential reports why no GitHub token resolves (contribute=issue
	// needs one): nil when one does, else contrib.ErrNoCredential or
	// contrib.ErrSignInUnrefreshed. A nil func counts as one resolving.
	ContribCredential func(context.Context) error
	// Poll is how often a working engine book is looked at (default 3 s);
	// IdlePoll how often one that is queued, paused or parked is (default 30 s;
	// a Retry looks at once); Recheck how often a book folder that couldn't be
	// written is tried again (default 1 min; a Retry tries at once).
	Poll     time.Duration
	IdlePoll time.Duration
	Recheck  time.Duration
	Log      *slog.Logger
	Now      func() time.Time
}

// Handler runs sidecars.generate.
type Handler struct {
	o Options

	mu sync.Mutex
	// pokes wake a running job when its Retry is pressed (a park of the
	// Bridge's own, book_not_writable, checks again at once): each job id's
	// latest run's own channel.
	pokes map[int64]chan struct{}
	// describeParks is each describe job's park while its loop waits on it
	// (describeParked), with that run's wake-up channel, so WakeParked finds the
	// ones a change answers.
	describeParks map[int64]describePark
	// wakeSeq numbers WakeParked's calls and woken holds each park code's latest,
	// so a describe try that parks on a code woken while it ran tries again at
	// once (parkDescribe).
	wakeSeq uint64
	woken   map[state.ParkCode]uint64
	// doneJobs counts the jobs that finished done here (the heartbeat's
	// estimates are computed again when it moves).
	doneJobs atomic.Int64
}

// Finished is how many jobs finished done since the start.
func (h *Handler) Finished() int64 { return h.doneJobs.Load() }

// New returns the handler.
func New(o Options) *Handler {
	if o.Poll <= 0 {
		o.Poll = 3 * time.Second
	}
	if o.IdlePoll <= 0 {
		o.IdlePoll = 30 * time.Second
	}
	if o.Recheck <= 0 {
		o.Recheck = time.Minute
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ContribCredential == nil {
		o.ContribCredential = func(context.Context) error { return nil }
	}
	return &Handler{o: o, pokes: map[int64]chan struct{}{}, describeParks: map[int64]describePark{},
		woken: map[state.ParkCode]uint64{}}
}

// describePark is a describe run's park (describeParks): its code, and the
// run's wake-up channel.
type describePark struct {
	code state.ParkCode
	poke chan struct{}
}

// WakeParked wakes every sidecars.describe job parked with code to try again
// now, and returns how many it woke: a monthly budget raised on the page, or the
// agent's credential changed on the Accounts tab, re-admits the engine's parked
// books, while a describe job waits in its own loop (until its retry_at, the 1st
// for the monthly budget, a Retry, or the Bridge's next start, which runs the job
// again). A job whose try is running then tries again at once if that try parks
// with code.
func (h *Handler) WakeParked(code state.ParkCode) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wakeSeq++
	h.woken[code] = h.wakeSeq
	woke := 0
	for _, p := range h.describeParks {
		if p.code == code {
			nudge(p.poke)
			woke++
		}
	}
	return woke
}

// wakeMark is WakeParked's latest call (describeParked's before a try).
func (h *Handler) wakeMark() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wakeSeq
}

func (h *Handler) now() time.Time { return h.o.Now() }

// Kind is sidecars.generate.
func (h *Handler) Kind() string { return Kind }

// Lane is the pipeline lane.
func (h *Handler) Lane() string { return jobs.LanePipeline }

// Result is what a done job reports (contract section 1). Description says
// whether description.json was written with the others (Phase 1c); when it
// wasn't, DescriptionError says why in a few plain words.
type Result struct {
	LibraryID        int64             `json:"library_id"`
	Path             string            `json:"path"`
	Source           string            `json:"source"`
	Files            []string          `json:"files"`
	Characters       int               `json:"characters"`
	Recaps           int               `json:"recaps"`
	CostUSD          float64           `json:"cost_usd"`
	WorkID           string            `json:"work_id"`
	Contributed      map[string]string `json:"contributed,omitempty"`
	Description      bool              `json:"description"`
	DescriptionError string            `json:"description_error,omitempty"`
}

// run is one job's state as it goes.
type run struct {
	job    jobs.Job
	rep    *jobs.Reporter
	params Params
	lib    link.Library
	mount  string
	rel    string
	place  place
	files  []link.BookFile
	info   *link.BookInfo
	source string
	epub   string
	bookID int64
	// seen is the engine book row the progress and cost were last read for:
	// they are read again only when it changed or the book is working.
	seen     bookKey
	progress []store.Progress
	costUSD  float64
	// side is the sidecars last read and sideSig their files' signature, so
	// unchanged files aren't read again; written is the hash of the sidecars last
	// written ("" for none yet); writeErr the last failed write's error, logged
	// once.
	side     sidecars
	sideSig  string
	written  string
	writeErr string
	srv      Server
	// jobPath is the job's path as the server last gave it (the claim, then the
	// answers to the job's reports): when it changes, the server moved the job with
	// its book, and rel follows it.
	jobPath string
	// poke is this run's wake-up channel (Retry); ownPark says the job's park is
	// one of the Bridge's own (book_not_writable, library_not_writable), set by
	// h.park; resuming says the job resumes an engine book (decided before
	// prepare runs).
	poke     chan struct{}
	ownPark  bool
	resuming bool
	// descTried says this run asked for the description, descDone that it asked
	// with the book done (a run asks at most twice: at a contributing park, then at
	// done); desc is the description it got and descRaw its file as read back,
	// descErr why it got none and descPaid that the try which failed spent.
	descTried bool
	descDone  bool
	desc      *pipeline.DescribeResult
	descRaw   []byte
	descErr   string
	descPaid  bool
	// kind is the run's job kind (Kind, ChaptersKind, DescribeKind).
	kind string
	// chaptersProv is bridge.json's chapters object for the run's own chapters.json
	// (Phase 1d: the engine found the book's chapters in its narration; nil for no
	// file to write: none found, or a one-chapter book), made at chaptersAt for the
	// detected list chaptersSum identifies; the file itself is composed at each
	// write (chaptersJSON) from the book's files and path as they are then.
	// chaptersSaved is the list last saved early, chaptersRetryAt when an early save
	// that failed is tried again.
	chaptersProv    *ChaptersProvenance
	chaptersSum     string
	chaptersAt      time.Time
	chaptersSaved   string
	chaptersRetryAt time.Time
	// detected is the detected list last read (pipeline.DetectedStamp says when;
	// detectedOK that it was current then), so refreshChapters reads it again only
	// once it changed.
	detected      pipeline.DetectedChapters
	detectedOK    bool
	detectedStamp string
}

// bookKey is what changes on an engine book row when the scheduler moves it.
type bookKey struct{ updatedAt, state, status string }

func keyOf(b store.Book) bookKey { return bookKey{b.UpdatedAt, b.State, b.Status} }

// working reports whether the engine runs a stage of the book now (not queued,
// paused or parked): its progress moves without its row changing, so it is
// followed closely.
func working(b store.Book) bool {
	return b.Status == "" && state.State(b.State) != state.Queued
}

// Run does the job: check, create (or find again) the engine book, follow it,
// write the attachments, rescan.
func (h *Handler) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	params, err := ParseParams(job.Params)
	if err != nil {
		return nil, err
	}
	r, stop, err := h.startRun(job, rep, Kind)
	if err != nil {
		return nil, err
	}
	defer stop()
	r.params = params
	eng, err := h.waitEngine(ctx, r)
	if err != nil {
		return nil, err
	}
	mapped, resumed, err := h.resumeMapping(ctx, job)
	if err != nil {
		return nil, err
	}
	if !resumed {
		// A new job before prepare can wait on its library; a resumed one that
		// makes a new engine book after all is checked by create.
		if err := h.cantContribute(ctx, r); err != nil {
			return nil, err
		}
	}
	return h.runEngineBook(ctx, eng, r, mapped, resumed, nil)
}

// cantContribute fails a contribute=issue job before any spend when no GitHub
// token resolves: the contribution is the last stage, and without a token it
// could only park there after the whole run was paid for. nil for any other job.
func (h *Handler) cantContribute(ctx context.Context, r *run) error {
	if r.params.Contribute != ContributeIssue {
		return nil
	}
	return credFailure(h.o.ContribCredential(ctx), errCantContributeUnrefreshed, errCantContribute)
}

// resumeMapping is the job's mapping to an engine book and whether the job resumes
// it. A mapping is this job's own only for the same library (and book, which
// runEngineBook checks once prepare knows the path): the ids are the server's, so
// another server (the Bridge paired anew) or a reinstalled one hands out an id an
// earlier job's mapping still holds, and following that would write another book's
// characters and recaps into this one.
func (h *Handler) resumeMapping(ctx context.Context, job jobs.Job) (store.GenerateBook, bool, error) {
	mapped, err := h.o.DB.GetGenerateBook(ctx, job.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.GenerateBook{}, false, nil
	}
	if err != nil {
		return store.GenerateBook{}, false, err
	}
	return mapped, mapped.BookID != nil && mapped.LibraryID == *job.LibraryID, nil
}

// runEngineBook checks the book (prepare), finds a resumed job's engine book again
// (following a book that moved while the job was away) or makes a new one, and
// follows it to the end; both job kinds that run the engine share it. fresh, when
// set, runs before a new book is made and may end the job without one (done).
func (h *Handler) runEngineBook(ctx context.Context, eng Engine, r *run, mapped store.GenerateBook, resumed bool,
	fresh func() (res any, done bool, err error)) (any, error) {
	// The book is the mapping's when its path is the server's canonical one, known
	// once prepare has asked; the claim's own path is the job's too (the server
	// moved the job with its book since).
	ownBook := func() bool { return mapped.RelPath == r.rel || mapped.RelPath == *r.job.Path }
	r.resuming = resumed
	if err := h.prepare(ctx, r, !resumed); err != nil {
		if resumed && ownBook() {
			r.bookID = *mapped.BookID // its own book: end() stops it
		}
		return h.end(ctx, r, nil, err)
	}
	if resumed && ownBook() && mapped.RelPath != r.rel {
		h.o.Log.Info("generate: the book moved while the job was away; the job follows it", "job", r.job.ID,
			"from", mapped.RelPath, "to", r.rel)
		h.recordMove(ctx, r)
	}
	if resumed && !ownBook() {
		// That run's job is gone (its id is this job's now), so nothing follows it.
		h.o.Log.Warn("generate: the job's id maps to another book's run; this job starts its own",
			"job", r.job.ID, "mapped", mapped.RelPath, "path", r.rel)
		h.stopBook(context.WithoutCancel(ctx), r.job.ID, *mapped.BookID)
		resumed = false
	}
	if resumed {
		r.bookID = *mapped.BookID
		if err := h.attach(ctx, eng, r, mapped); err != nil {
			return h.end(ctx, r, nil, err)
		}
	} else {
		if fresh != nil {
			if res, done, err := fresh(); done || err != nil {
				return res, err
			}
		}
		if err := h.create(ctx, eng, r); err != nil {
			return h.end(ctx, r, nil, err)
		}
	}
	res, err := h.follow(ctx, eng, r)
	return h.end(ctx, r, res, err)
}

// startRun begins a run of either job kind: the run at the job's path (the server
// may have moved the job with its book since the claim; the first report's answer
// says where it is now), its wake-up channel registered and "Starting" reported.
// One wake-up channel serves the whole run, so a Retry reaches whichever park the
// job waits in; it is this run's own, so a later attempt of the same job keeps its
// channel when this one ends. stop drops it.
func (h *Handler) startRun(job jobs.Job, rep *jobs.Reporter, kind string) (r *run, stop func(), err error) {
	if job.LibraryID == nil || job.Path == nil || strings.TrimSpace(*job.Path) == "" {
		return nil, nil, fmt.Errorf("%s needs a library and a book path", kind)
	}
	if h.o.Libraries != nil {
		if lib, ok := h.o.Libraries.Library(*job.LibraryID); ok && lib.LibraryKind() != "books" {
			return nil, nil, errors.New("not_book_library: this job requires a books library")
		}
	}
	r = &run{job: job, rep: rep, kind: kind, rel: *job.Path, jobPath: *job.Path, poke: make(chan struct{}, 1)}
	if p := rep.Path(); p != "" {
		r.rel, r.jobPath = p, p
	}
	h.setPoke(job.ID, r.poke)
	rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Starting"})
	return r, func() { h.dropPoke(job.ID, r.poke) }, nil
}

// errCantContribute is a contribute=issue job on a Bridge with no GitHub
// credential; errCantContributeUnrefreshed one on a Bridge signed in to GitHub
// whose sign-in GitHub couldn't be reached to refresh.
var (
	errCantContribute = errors.New("this job asks to contribute the characters and recaps (contribute: issue), but " +
		"the Bridge isn't signed in to GitHub: sign in on the Bridge's Accounts tab, or generate the book with " +
		"contribute set to local")
	errCantContributeUnrefreshed = errors.New("this job asks to contribute the characters and recaps (contribute: " +
		"issue), but the Bridge's GitHub sign-in couldn't be refreshed because GitHub couldn't be reached: generate " +
		"the book again once it can, or with contribute set to local")
)

// credFailure words a missing contribution credential (err, from
// Options.ContribCredential): unrefreshed when the sign-in only couldn't be
// refreshed (contrib.ErrSignInUnrefreshed), else none; nil for nil.
func credFailure(err, unrefreshed, none error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, contrib.ErrSignInUnrefreshed):
		return unrefreshed
	}
	return none
}

// end records how the job ended, unless it resumes later (the Bridge stopping or
// its token refused). A job that ends any other way short of done (an admin
// cancelled it, the server took it back, it failed) stops its engine book, which
// nothing would follow any more.
func (h *Handler) end(ctx context.Context, r *run, res any, err error) (any, error) {
	if ctx.Err() != nil && jobs.Resumes(ctx) {
		return res, err
	}
	if r.bookID != 0 {
		h.release(context.WithoutCancel(ctx), r.job, r.bookID, err != nil)
	}
	return res, err
}

// Abandon is the jobs runner telling the handler it ended a job without running
// it (the first report after a restart was answered with a cancel or a lost
// claim, or the job was another server's): the engine book an earlier run of the
// job created is released as end() would, or it would go on with nothing to
// follow it.
func (h *Handler) Abandon(ctx context.Context, job jobs.Job) {
	m, err := h.o.DB.GetGenerateBook(ctx, job.ID)
	if err != nil || m.BookID == nil || m.EndedAt != "" {
		return
	}
	if h.o.Engine() != nil {
		h.release(ctx, job, *m.BookID, true)
		return
	}
	// A restart resumes its jobs at the first heartbeat, which can come before the
	// engine has started (its first start may download ffmpeg): the book is
	// released once it has.
	go func() {
		for h.o.Engine() == nil {
			if agent.SleepCtx(ctx, h.o.Poll) != nil {
				return
			}
		}
		h.release(ctx, job, *m.BookID, true)
	}()
}

// release ends a job's hold on its engine book: the end is recorded (the
// transcript retention counts from it, and the supervisor leaves a book no job
// follows alone), a job that didn't finish stops the book (nothing follows it
// any more), and with retention_days 0 the transcripts go now. A later claim of
// the same job may run here already (the server re-queued it after a lease ran
// out and handed it out again before this attempt heard it was lost): that
// attempt follows the book now, so it is left alone.
func (h *Handler) release(ctx context.Context, job jobs.Job, bookID int64, stop bool) {
	if sj, err := h.o.DB.GetServerJob(ctx, job.ID); err == nil && sj.Attempt > job.Attempt {
		return
	}
	if err := h.o.DB.EndGenerateBook(ctx, job.ID, h.now()); err != nil {
		h.o.Log.Warn("generate: recording the job's end failed", "job", job.ID, "err", err)
	}
	if stop {
		h.stopBook(ctx, job.ID, bookID)
	}
	if h.o.RetentionDays == 0 {
		if err := h.purgeEnded(ctx, bookID); err != nil {
			h.o.Log.Warn("generate: deleting the transcripts failed", "job", job.ID, "err", err)
		}
	}
}

// stopBook cancels a job's engine book unless it already ended (done, or
// failed: a cancel would overwrite its error) or another open job follows it now
// (a generate job continued the chapters run, so a late end of the chapters job,
// one Abandon releases once the engine starts, leaves it to that job).
func (h *Handler) stopBook(ctx context.Context, jobID, bookID int64) {
	eng := h.o.Engine()
	if eng == nil {
		return
	}
	b, err := h.o.DB.GetBook(ctx, bookID)
	if err != nil || finished(b.State, b.Status) {
		return
	}
	if open, err := h.o.DB.OpenGenerateJobsForBook(ctx, bookID, jobID); err != nil || len(open) > 0 {
		return
	}
	if err := eng.Cancel(ctx, bookID); err != nil && !errors.Is(err, scheduler.ErrInvalidOp) {
		h.o.Log.Warn("generate: cancelling the engine book failed", "job", jobID, "book", bookID, "err", err)
	}
}

// hold takes the engine's hold on an engine book for work outside its stages (the
// description step, a transcript purge): ok is false while a stage runs on it or
// something else holds it. With no engine nothing runs on it, so it holds.
func (h *Handler) hold(bookID int64) (release func(), ok bool) {
	var eng Engine
	if h.o.Engine != nil {
		eng = h.o.Engine()
	}
	if eng == nil {
		return func() {}, true
	}
	return eng.Hold(bookID)
}

// waitEngine waits for the engine to have started.
func (h *Handler) waitEngine(ctx context.Context, r *run) (Engine, error) {
	for {
		if eng := h.o.Engine(); eng != nil {
			return eng, nil
		}
		r.rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Waiting for the engine to start"})
		if err := agent.SleepCtx(ctx, h.o.Poll); err != nil {
			if !jobs.Resumes(ctx) {
				// Cancelled, or taken back, before the engine started: the run never
				// reaches end(), so the engine book an earlier run of the job made is
				// released as Abandon does, or a start would re-admit it with nothing
				// to follow it.
				h.Abandon(context.WithoutCancel(ctx), r.job)
			}
			return nil, err
		}
	}
}

// prepare resolves the library and the book through the CHECKED mapping, fetches
// the book from the server, compares its files and makes sure the book's own
// folder can be written. strict (a new job) fails on a library that isn't mapped
// and writable; a resumed one waits for it, parked.
func (h *Handler) prepare(ctx context.Context, r *run, strict bool) error {
	if err := h.serverLibrary(r); err != nil {
		return err
	}
	if err := h.waitMapped(ctx, r, strict); err != nil {
		return err
	}
	book, err := h.fetchBook(ctx, r)
	if err != nil {
		return err
	}
	r.files, r.info = book.Files, book.Info
	if book.Path != "" {
		r.rel = book.Path
	}
	if _, ok, problem := mapping.CompareFiles(r.mount, book.Files); !ok {
		return errors.New("the Bridge's copy of the book doesn't match the server's: " + problem)
	}
	p, err := locate(r.mount, r.rel)
	if err != nil {
		return err
	}
	r.place = p
	return h.waitWritable(ctx, r)
}

// serverLibrary sets the run's server and the job's library as the server
// listed it.
func (h *Handler) serverLibrary(r *run) error {
	srv := h.o.Server()
	if srv == nil {
		return errors.New("the Bridge isn't paired")
	}
	r.srv = srv
	lib, ok := h.o.Libraries.Library(*r.job.LibraryID)
	if !ok {
		return fmt.Errorf("library %d isn't one the server listed", *r.job.LibraryID)
	}
	if lib.LibraryKind() != "books" {
		return errors.New("not_book_library: this job requires a books library")
	}
	r.lib = lib
	return nil
}

// fetchBook asks the server for the book, waiting out an answer that may change
// (the server unreachable or restarting, a 5xx, a refused token the runner is
// dealing with): only a definite answer (no such book, another 4xx) fails the job,
// so a restart while the server is down doesn't end a long run.
func (h *Handler) fetchBook(ctx context.Context, r *run) (*link.Book, error) {
	wait := h.o.Poll
	for {
		r.rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Asking the server for the book"})
		book, err := r.srv.Book(ctx, r.lib.ID, r.rel)
		var apiErr *link.APIError
		switch {
		case err == nil:
			return book, nil
		case errors.Is(err, link.ErrNoBook):
			return nil, fmt.Errorf("the server has no indexed book at %s", r.rel)
		case errors.As(err, &apiErr) && apiErr.Status < 500:
			return nil, fmt.Errorf("could not get the book from the server: %w", err)
		}
		h.o.Log.Warn("generate: asking the server for the book failed; trying again", "job", r.job.ID, "err", err)
		if err := agent.SleepCtx(ctx, wait); err != nil {
			return nil, err
		}
		wait = min(wait*2, time.Minute)
	}
}

// waitMapped waits for the library's mapping check (a start checks in the
// background after the first heartbeat) and needs it mapped and writable at the
// mount the Bridge uses now. A new job fails otherwise; a resumed one parks
// library_not_writable and waits, its engine book going on meanwhile.
func (h *Handler) waitMapped(ctx context.Context, r *run, strict bool) error {
	for {
		mount, _ := h.o.Mount(r.lib.ID, r.lib.Root)
		st, checked := h.o.Libraries.Status(r.lib.ID)
		switch {
		case checked && st.Mapped && st.Writable && st.MountPath == mount:
			r.mount = mount
			r.unparkOwn()
			return nil
		case !checked || st.MountPath != mount:
			r.rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Waiting for the library's mapping check"})
		case strict:
			why := "isn't mapped on the Bridge"
			if st.Mapped {
				why = "is mapped read-only on the Bridge"
			}
			if st.Error != "" {
				why += " (" + st.Error + ")"
			}
			return fmt.Errorf("library %q %s, so the Bridge can't write into the book's folder", r.lib.Name, why)
		default:
			h.park(r, parkLibraryNotWritable, "library "+r.lib.Name+" isn't mapped and writable on the Bridge any more")
		}
		if err := agent.SleepCtx(ctx, h.o.Poll); err != nil {
			return err
		}
	}
}

// waitWritable checks the book's own folder can be written (a library root that
// is writable says nothing about a 0555 book folder, spike 3), parking
// book_not_writable and checking again every Recheck (or on Retry) until it can.
func (h *Handler) waitWritable(ctx context.Context, r *run) error {
	for {
		err := mapping.ProbeWritable(r.place.dir)
		if err == nil {
			r.unparkOwn()
			return nil
		}
		h.park(r, parkBookNotWritable, "the Bridge can't write in the book's folder ("+writeProblem(err)+")")
		if err := h.recheckWait(ctx, r, time.Now().Add(h.o.Recheck)); err != nil {
			return err
		}
	}
}

// recheckWait waits for the next look at a park: until at (the zero time: no
// timer, only a Retry), a Retry (which pokes), or the job's end. A park of the
// Bridge's own looks again every Recheck; a description's agent park at its
// retry_at.
func (h *Handler) recheckWait(ctx context.Context, r *run, at time.Time) error {
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

// writeProblem words a failed write probe without the Bridge's own paths.
func writeProblem(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrNotExist):
		return "the folder is gone"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// park parks the job with one of the Bridge's own codes.
func (h *Handler) park(r *run, code, msg string) {
	r.rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Needs attention: " + msg,
		Extra: map[string]any{"source": r.source, "park": parkExtra(code, "")}})
	r.rep.Park(code, msg)
	r.ownPark = true
}

// unparkOwn lifts a park of the Bridge's own once its reason is gone, while the
// job has no engine book to report on. A job that has one, or resumes one, stays
// parked until step() reports the book's own state, and a park restored after a
// restart (not the Bridge's own) stays too: the job never reports running between
// two parks, which the server would hear as a new one.
func (r *run) unparkOwn() {
	if r.ownPark && r.bookID == 0 && !r.resuming {
		r.rep.Unpark()
		r.ownPark = false
	}
}

// engineParked reports the engine book's park (or none) as the job's.
func (r *run) engineParked(parked bool, code, msg string) {
	if parked {
		r.rep.Park(code, msg)
	} else {
		r.rep.Unpark()
	}
	r.ownPark = false
}

// create picks the source and makes the engine book for a new job (a chapters job's
// is createChapters'): a chapters run of the book reading the same audio is
// continued (continueChapters), keeping its transcription, once a parked chapters
// job on it gave way (awaitChaptersGivingWay); any other book the engine already has from an earlier
// run (the same source path) is deleted first, unless another open job follows it
// or a stage of it runs right now.
func (h *Handler) create(ctx context.Context, eng Engine, r *run) error {
	if r.kind == ChaptersKind {
		return h.createChapters(ctx, eng, r)
	}
	if r.resuming {
		// A resumed job whose book is another job's (an id reused) or gone (deleted,
		// or swept since it was cancelled) starts over: a new spend, which Run's
		// check before any spend skipped.
		if err := h.cantContribute(ctx, r); err != nil {
			return err
		}
	}
	source, epub, note, err := chooseSource(r.place, r.params.Source)
	if err != nil {
		return err
	}
	r.source, r.epub = source, epub
	if note != "" {
		h.o.Log.Info("generate: "+note, "job", r.job.ID)
	}
	sp, ok, err := metaops.AllowedPath(r.place.full, []string{r.mount})
	if err != nil || !ok {
		return errors.New("the book's path leaves the library's folder")
	}
	ebookPath := ""
	if r.epub != "" {
		ep, ok, err := metaops.AllowedPath(r.epub, []string{r.mount})
		if err != nil || !ok {
			return errors.New("the book's .epub leaves the library's folder")
		}
		ebookPath = ep
	}
	old, err := h.o.DB.GetBookBySourcePath(ctx, sp)
	var openID int64
	var openKind string
	if err == nil {
		old, openID, openKind, err = h.awaitChaptersGivingWay(ctx, r, old)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No earlier run (or it went while this job waited): a book of its own.
	case err != nil:
		return err
	case openID != 0 && openKind == ChaptersKind:
		return fmt.Errorf("job %d is finding this book's chapters; let it finish (this job can then go on from its transcription) or cancel it first", openID)
	case openID != 0:
		return fmt.Errorf("job %d is already generating this book; cancel it first", openID)
	default:
		if ok, err := h.continueChapters(ctx, eng, r, old); ok || err != nil {
			return err
		}
		if err := h.clearEarlier(ctx, eng, r, old.ID); err != nil {
			return err
		}
	}
	return h.queueBook(ctx, eng, r, h.newBook(r, sp, ebookPath))
}

// errEarlierRunBusy is a new job's refusal while the engine stays busy with the
// book's earlier run, which it would continue or clear.
var errEarlierRunBusy = errors.New("the engine is still busy with this book's earlier run; try again in a minute")

// clearEarlier deletes the engine book of the book's earlier run (see
// deleteEarlier), in words for a person when it can't.
func (h *Handler) clearEarlier(ctx context.Context, eng Engine, r *run, id int64) error {
	if err := h.deleteEarlier(ctx, eng, r, id); errors.Is(err, scheduler.ErrBusy) {
		return errEarlierRunBusy
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("could not clear the book's earlier run: %w", err)
	}
	h.o.Log.Info("generate: the book's earlier run was cleared", "job", r.job.ID, "old_book", id)
	return nil
}

// queueBook makes the job's engine book nb in a batch of its own, maps the job to
// it and wakes the engine.
func (h *Handler) queueBook(ctx context.Context, eng Engine, r *run, nb store.NewBook) error {
	batch, err := h.o.DB.CreateBatch(ctx)
	if err != nil {
		return err
	}
	nb.BatchID = batch.ID
	b, err := h.o.DB.CreateBook(ctx, nb)
	if err != nil {
		return fmt.Errorf("could not queue the book in the engine: %w", err)
	}
	if err := h.o.DB.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: r.job.ID, BookID: &b.ID, LibraryID: r.lib.ID, RelPath: r.rel}); err != nil {
		_ = h.o.DB.DeleteBook(context.WithoutCancel(ctx), b.ID)
		return err
	}
	r.bookID = b.ID
	h.o.Log.Info("generate: queued in the engine", "job", r.job.ID, "kind", r.kind, "book", b.ID, "source", r.source, "path", r.rel)
	eng.Notify()
	return nil
}

// deleteEarlier deletes the engine book of the book's earlier run, waiting a few
// polls (holdTries) while the engine is busy with it: a description reading it,
// which the server asked to give way to this job and which lets go at its next
// report, or a stage the earlier job's end just stopped. scheduler.ErrBusy when it
// stays busy.
func (h *Handler) deleteEarlier(ctx context.Context, eng Engine, r *run, id int64) error {
	for i := 1; ; i++ {
		err := eng.Delete(ctx, id)
		if !errors.Is(err, scheduler.ErrBusy) || i == holdTries {
			return err
		}
		r.rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Waiting for the engine to let go of the book's earlier run"})
		if err := agent.SleepCtx(ctx, h.o.Poll); err != nil {
			return err
		}
	}
}

// newBook is the engine book for the job, as sidecars' intake built it from a
// scanned candidate: the identity from the server's book block, the work id from
// the params or else the server's metadata cache.
func (h *Handler) newBook(r *run, sourcePath, ebookPath string) store.NewBook {
	info := link.BookInfo{}
	if r.info != nil {
		info = *r.info
	}
	title := strings.TrimSpace(info.Title)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(r.rel), filepath.Ext(r.rel))
		if !r.place.single {
			title = filepath.Base(r.rel)
		}
	}
	sources := map[string]string{"title": "server"}
	for field, v := range map[string]string{"asin": info.ASIN, "isbn": info.ISBN, "series": info.Series} {
		if strings.TrimSpace(v) != "" {
			sources[field] = "server"
		}
	}
	workID := cmp.Or(r.params.WorkID, strings.TrimSpace(info.WorkID))
	kind := string(state.KindAudio)
	if ebookPath != "" {
		kind = string(state.KindEbook)
	}
	return store.NewBook{
		SourcePath: sourcePath, WorkDir: store.DeriveWorkDir(h.o.WorkRoot, sourcePath, title),
		Title: title, Authors: info.Authors, Narrators: info.Narrators,
		Series: strings.TrimSpace(info.Series), SeriesPos: strings.TrimSpace(info.SeriesIndex),
		ASIN: strings.TrimSpace(info.ASIN), ISBN: strings.TrimSpace(info.ISBN), IdentitySources: sources,
		WorkID: workID, Kind: kind, EbookPath: ebookPath, ContribMode: r.params.Contribute,
	}
}

// attach finds a resumed job's engine book again. One this job's own cancel
// stopped (the server took the job back, then handed it out again) is re-admitted,
// unless its transcripts went with that end (retention): the stages that wrote
// them still count as done, so it starts over instead, as a book that is gone is.
// The job's mapping is open again either way.
func (h *Handler) attach(ctx context.Context, eng Engine, r *run, m store.GenerateBook) error {
	b, err := h.o.DB.GetBook(ctx, r.bookID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && scheduler.IsCancelled(b) && m.SweptAt != "") {
		r.bookID = 0
		return h.create(ctx, eng, r)
	}
	if err != nil {
		return err
	}
	if m.EndedAt != "" {
		if err := h.o.DB.ReopenGenerateBook(ctx, r.job.ID); err != nil {
			return err
		}
	}
	r.sourceOf(b)
	if scheduler.IsCancelled(b) {
		if err := eng.Retry(ctx, b.ID); err != nil && !errors.Is(err, scheduler.ErrInvalidOp) {
			return err
		}
		h.o.Log.Info("generate: the job is back, so its engine book runs again", "job", r.job.ID, "book", b.ID)
	}
	h.o.Log.Info("generate: following the engine book again", "job", r.job.ID, "book", b.ID, "state", b.State)
	return nil
}

// bookExtra is a progress Extra for a step on the engine book b: its stage and
// the run's source, and its park while it is parked (a write at a contributing
// park keeps the park in what the job reports).
func (r *run) bookExtra(b store.Book) map[string]any {
	extra := map[string]any{"stage": b.State, "source": r.source}
	if state.Status(b.Status) == state.StatusNeedsAttention {
		extra["park"] = parkExtra(b.ParkCode, b.RetryAt)
	}
	return extra
}

// sourceOf takes the run's source (and its .epub) from the engine book whose notes
// it follows: an ebook-kind book read its .epub, any other the audio.
func (r *run) sourceOf(b store.Book) {
	if b.Kind == string(state.KindEbook) {
		r.source, r.epub = SourceEbook, b.EbookPath
	} else {
		r.source, r.epub = SourceAudio, ""
	}
}

// follow reports the engine book's progress until it is done (then writes and
// rescans), fails, or the job's context ends.
func (h *Handler) follow(ctx context.Context, eng Engine, r *run) (any, error) {
	for {
		wait := h.o.Poll
		b, err := h.o.DB.GetBook(ctx, r.bookID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return nil, errors.New("the engine book was deleted")
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil:
			h.o.Log.Warn("generate: reading the engine book failed", "job", r.job.ID, "err", err)
		default:
			res, done, err := h.step(ctx, eng, r, b)
			if done || err != nil {
				return res, err
			}
			if !working(b) {
				// Queued, paused or parked: nothing moves until the scheduler
				// (or a Retry, which pokes) moves the book.
				wait = h.o.IdlePoll
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.poke:
		case <-time.After(wait):
		}
	}
}

// step reports one look at the engine book and acts on it: done writes and
// finishes, failed fails, a contributing park (core_needed) still writes the
// local files first.
func (h *Handler) step(ctx context.Context, eng Engine, r *run, b store.Book) (any, bool, error) {
	if k := keyOf(b); k != r.seen || working(b) {
		r.seen = k
		r.progress, _ = h.o.DB.ListProgress(ctx, b.ID)
		// The budgets' basis (the provider's reported cost, else a complete
		// estimate), so the job's spend is the one the caps count.
		r.costUSD, _, _ = h.o.DB.SumStageRunBudgetCost(ctx, b.ID)
	}
	v := view{book: b, source: r.source, progress: r.progress, costUSD: r.costUSD}
	v.eta, v.hasETA = eng.ETASeconds(b.ID)
	st, status := state.State(b.State), state.Status(b.Status)
	switch {
	case st == state.Done && r.kind == ChaptersKind:
		res, err := h.finishChapters(ctx, r, b)
		return res, true, err
	case st == state.Done:
		res, err := h.finish(ctx, r, b, v.costUSD)
		return res, true, err
	}
	// A chapters-only book whose inspect found chapters of its own ends the job with
	// that sentence, failed or (the supervisor may have escalated the failure since)
	// parked.
	if r.kind == ChaptersKind && st == state.Inspecting &&
		(status == state.StatusFailed || status == state.StatusNeedsAttention) {
		if msg, ok := h.hasChaptersFailure(ctx, b.ID); ok {
			return nil, true, describeFailure(msg)
		}
	}
	if status == state.StatusFailed {
		return nil, true, fmt.Errorf("the engine failed the book at %s: %s", b.State, b.Error)
	}
	pr := describe(v)
	if r.kind == ChaptersKind {
		delete(pr.Extra, "cost_usd") // speech recognition and detection only: no agent spend
	}
	r.rep.Progress(pr)
	// The chapters go into the library as soon as they are found, long before the
	// characters and recaps; the stage is reported again after the save's own steps.
	if r.kind == Kind && h.saveChaptersEarly(ctx, r, b) {
		r.rep.Progress(pr)
	}
	parked := status == state.StatusNeedsAttention
	// The park is the job's before the write below, which can take a while (the
	// description step may wait for an agent slot): the server hears it at once.
	r.engineParked(parked, b.ParkCode, b.Error)
	if parked && st == state.Contributing {
		_, err := h.write(ctx, r, b, false)
		if msg := errText(err); msg != r.writeErr {
			r.writeErr = msg
			if err != nil {
				h.o.Log.Warn("generate: writing the attachments at a contributing park failed", "job", r.job.ID, "err", err)
			}
		}
		r.rep.Progress(describe(v)) // the park again, after the write's own steps
		// A park of the Bridge's own the write raised and lifted leaves the
		// engine's to report again.
		r.engineParked(true, b.ParkCode, b.Error)
	}
	return nil, false, nil
}

// finish writes the attachments of a done book, rescans, and composes the result.
func (h *Handler) finish(ctx context.Context, r *run, b store.Book, cost float64) (any, error) {
	r.engineParked(false, "", "")
	s, err := h.write(ctx, r, b, true)
	if err != nil {
		return nil, err
	}
	if r.descTried {
		// The description step may have spent after the cost was read (a try that
		// failed its checks is paid for too), and the budgets count it.
		if c, _, err := h.o.DB.SumStageRunBudgetCost(ctx, b.ID); err == nil {
			cost = c
		}
	}
	res := Result{LibraryID: r.lib.ID, Path: r.rel, Source: r.source, Files: attachmentNames(r.place.prefix, s),
		Characters: s.nChars, Recaps: s.nRecaps, CostUSD: Cents(cost), WorkID: cmp.Or(b.WorkID, s.workID),
		Description: s.description != nil}
	if !res.Description {
		res.DescriptionError = cmp.Or(r.descErr, "no description was written")
	}
	if r.params.Contribute == ContributeIssue {
		rows, err := h.o.DB.ListContributionsByBook(ctx, b.ID)
		if err == nil {
			for _, c := range rows {
				if c.URL != "" && (c.Kind == store.ContribKindCharacters || c.Kind == store.ContribKindRecaps) {
					if res.Contributed == nil {
						res.Contributed = map[string]string{}
					}
					res.Contributed[c.Kind] = c.URL
				}
			}
		}
	}
	h.doneJobs.Add(1)
	r.rep.Progress(jobs.Progress{Step: string(state.Done), Message: "Finished",
		Extra: map[string]any{"stage": string(state.Done), "source": r.source, "cost_usd": res.CostUSD}})
	return res, nil
}

// write writes the engine book's sidecars into the book's .audiosilo/ (once per
// content) and asks the server to re-read them. The mapping must still be
// checked mapped and writable at the mount the job uses, and the book is written
// where it is now (the server moves the job with its book). With wait (the book
// is done), a write that fails parks the job book_not_writable and tries again
// every Recheck (or on Retry) until it goes through: the sidecars stay in the
// engine's work dir meanwhile, never lost to a failed job. Without (a book parked
// at contributing, looked at every poll) the failure is returned.
//
// The characters and recaps go first, then the description step runs (once per
// run, see describeOnce) and a description it got follows in a second save: the
// step can wait a long time for an agent slot behind other books, and the
// optional description never holds back the files the job exists for.
func (h *Handler) write(ctx context.Context, r *run, b store.Book, wait bool) (sidecars, error) {
	s, err := h.writeSidecars(ctx, r, b, wait)
	if err != nil || !h.describeOnce(ctx, r, b) {
		return s, err
	}
	return h.writeSidecars(ctx, r, b, wait)
}

// writeSidecars is one write of the sidecars as they are now (the description the
// run has so far with them), skipped when that content was written already.
func (h *Handler) writeSidecars(ctx context.Context, r *run, b store.Book, wait bool) (sidecars, error) {
	s, err := r.loadSidecars(b.WorkDir)
	if err != nil {
		return sidecars{}, err
	}
	// Only the description this run's step read back, under the engine's hold (it
	// can say how that was made); none when it got none. The chapters the run found
	// go with them, composed for the book where it is when it is written.
	s.description = r.descRaw
	h.refreshChapters(r, b)
	if s.chapters, err = r.chaptersJSON(r.place.single); err != nil {
		return sidecars{}, err
	}
	sum := s.hash()
	if sum == r.written {
		return s, nil
	}
	workID := cmp.Or(b.WorkID, s.workID)
	err = h.save(ctx, r, saveSpec{
		what:   "the characters and recaps",
		wait:   wait,
		record: true,
		progress: jobs.Progress{Step: stepWriting, Message: "Saving the characters and recaps with the book",
			Extra: r.bookExtra(b)},
		files: func(p place) ([]attachFile, error) {
			prov := provenance(h.o.Version, Kind, r.job.ID, h.now(), r.source, workID, inputsOf(r.mount, r.files, r.epub))
			if s.description != nil {
				prov.Description = descriptionProvenance(r.job.ID, *r.desc)
			} else {
				// An earlier run's description.json stays in place; what vouches
				// for it stays with it.
				prov.Description = keptObject[DescriptionProvenance](p.dir, p.prefix, descriptionName, "description")
			}
			chapters, err := r.chaptersJSON(p.single)
			if err != nil {
				return nil, err
			}
			s.chapters = chapters
			prov.Chapters = r.chaptersProv
			if prov.Chapters == nil {
				prov.Chapters = keptObject[ChaptersProvenance](p.dir, p.prefix, chaptersName, "chapters")
			}
			return attachmentFiles(s, prov)
		},
	})
	if err != nil {
		return sidecars{}, err
	}
	r.written = sum
	h.o.Log.Info("generate: wrote the book's attachments", "job", r.job.ID, "path", r.rel, "files", attachmentNames(r.place.prefix, s))
	r.rep.Progress(jobs.Progress{Step: stepRescanning, Message: "Asking the server to read them",
		Extra: r.bookExtra(b)})
	h.rescan(ctx, r)
	return s, nil
}

// saveSpec is one save into the book's .audiosilo/ (the generate job's
// attachments, or a description-only job's two files).
type saveSpec struct {
	// what is what the save writes, as a park names it ("the description").
	what string
	// wait parks book_not_writable and tries again until the save goes through;
	// without it the failure is returned.
	wait bool
	// record records a move in the job's mapping (a description-only job has
	// none to record it in).
	record bool
	// progress is reported once the book is located, before the write.
	progress jobs.Progress
	// files composes what is written once the book is located (bridge.json names
	// the inputs where the book is now).
	files func(p place) ([]attachFile, error)
}

// save writes spec's files into the book's .audiosilo/ at the path it is at now
// (the server moves the job with its book), through the mapping checked again
// (mapped and writable at the mount the job uses). With spec.wait, a write that
// fails parks the job book_not_writable and tries again every Recheck (or on
// Retry) until it goes through.
func (h *Handler) save(ctx context.Context, r *run, spec saveSpec) error {
	for {
		if err := h.waitMapped(ctx, r, false); err != nil {
			return err
		}
		if h.adoptMove(ctx, r) && spec.record {
			h.recordMove(ctx, r)
		}
		err := h.saveOnce(r, spec)
		if err == nil {
			break
		}
		if !spec.wait {
			return err
		}
		h.park(r, parkBookNotWritable, "the Bridge can't save "+spec.what+" with the book: "+err.Error())
		if err := h.recheckWait(ctx, r, time.Now().Add(h.o.Recheck)); err != nil {
			return err
		}
	}
	if r.ownPark {
		r.rep.Unpark()
		r.ownPark = false
	}
	return nil
}

// saveOnce locates the book through the mount and writes spec's files, its error
// worded without the Bridge's own paths.
func (h *Handler) saveOnce(r *run, spec saveSpec) error {
	if err := r.relocate(); err != nil {
		return err
	}
	r.rep.Progress(spec.progress)
	files, err := spec.files(r.place)
	if err == nil {
		err = writeAttachFiles(r.place.dir, r.place.prefix, files)
	}
	if err != nil {
		return fmt.Errorf("its %s folder can't be written (%s)", AttachDir, writeProblem(err))
	}
	return nil
}

// relocate locates the book where the job has it now (r.place), and its .epub
// with it: a book that moved took its .epub along.
func (r *run) relocate() error {
	p, err := locate(r.mount, r.rel)
	if err != nil {
		return err
	}
	r.place = p
	if r.epub != "" && !fsutil.IsFile(r.epub) {
		if moved := filepath.Join(p.dir, filepath.Base(r.epub)); fsutil.IsFile(moved) {
			r.epub = moved
		}
	}
	return nil
}

// adoptMove adopts the job's path when the server moved the job with its book (a
// rename its scan noticed; the answer to a report says so): the book's files are
// asked for again there. It reports whether the book moved.
func (h *Handler) adoptMove(ctx context.Context, r *run) bool {
	cur := r.rep.Path()
	if cur == "" || cur == r.jobPath {
		return false
	}
	from := r.rel
	r.jobPath, r.rel = cur, cur
	if book, err := r.srv.Book(ctx, r.lib.ID, cur); err == nil {
		r.files = book.Files
		if book.Path != "" {
			r.rel = book.Path
		}
	} else {
		h.o.Log.Warn("generate: asking the server for the moved book failed", "job", r.job.ID, "err", err)
	}
	h.o.Log.Info("generate: the book moved; the job follows it", "job", r.job.ID, "from", from, "to", r.rel)
	return true
}

// recordMove records the job's book at r.rel in its mapping, so a restart finds
// the same book.
func (h *Handler) recordMove(ctx context.Context, r *run) {
	if err := h.o.DB.SetGenerateBookPath(ctx, r.job.ID, r.rel); err != nil {
		h.o.Log.Warn("generate: recording the book's new path failed", "job", r.job.ID, "err", err)
	}
}

// loadSidecars reads the engine's sidecars, unless their files are unchanged
// since the last read.
func (r *run) loadSidecars(workDir string) (sidecars, error) {
	sig := sidecarsSig(workDir)
	if sig != "" && sig == r.sideSig {
		return r.side, nil
	}
	s, err := readSidecars(workDir)
	if err != nil {
		return sidecars{}, err
	}
	r.side, r.sideSig = s, sig
	return s, nil
}

// attachmentNames are the names a write produces for s.
func attachmentNames(prefix string, s sidecars) []string {
	var out []string
	if s.characters != nil {
		out = append(out, prefix+charactersName)
	}
	if s.recaps != nil {
		out = append(out, prefix+recapsName)
	}
	if s.description != nil {
		out = append(out, prefix+descriptionName)
	}
	if s.chapters != nil {
		out = append(out, prefix+chaptersName)
	}
	return append(out, prefix+provenanceName)
}

// rescanTries bounds the attachments rescan: the files are on disk either way,
// and the server's next full scan reads them, so a rescan that keeps failing is
// logged, never a failed job.
const rescanTries = 3

func (h *Handler) rescan(ctx context.Context, r *run) {
	for i := 1; ; i++ {
		got, err := r.srv.RescanAttachments(ctx, r.lib.ID, r.rel)
		if err == nil {
			h.o.Log.Info("generate: the server read the attachments", "job", r.job.ID, "characters", got.Characters, "recaps", got.Recaps,
				"description", got.Description, "chapters", got.Chapters)
			return
		}
		if i == rescanTries || errors.Is(err, link.ErrNotIndexable) || ctx.Err() != nil {
			h.o.Log.Warn("generate: the server didn't re-read the attachments (its next scan will)", "job", r.job.ID, "err", err)
			return
		}
		if agent.SleepCtx(ctx, h.o.Poll) != nil {
			return
		}
	}
}

// ErrNotParked is a Retry for a job that isn't parked.
var ErrNotParked = errors.New("the job isn't waiting on anything a retry would help")

// Retry re-admits a parked job's engine book (the engine's own Retry), or wakes a
// job parked before the engine has its book (the book's folder wasn't writable)
// to check again now. store.ErrNotFound when the job isn't one of this handler's.
func (h *Handler) Retry(ctx context.Context, jobID int64) error {
	h.mu.Lock()
	c, waiting := h.pokes[jobID]
	h.mu.Unlock()
	// poke wakes the running job (to look at its book, or at its folder, now),
	// reporting whether one runs here.
	poke := func() bool {
		if waiting {
			nudge(c)
		}
		return waiting
	}
	// A description-only job's parks are all its own (they wait for the poke); its
	// id may still map to an engine book in generate_books, an earlier server's
	// generate job under the same id, which is not this job's to re-admit.
	if sj, err := h.o.DB.GetServerJob(ctx, jobID); err == nil && sj.Kind == DescribeKind {
		if poke() {
			return nil
		}
		return store.ErrNotFound
	}
	m, err := h.o.DB.GetGenerateBook(ctx, jobID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.BookID == nil) {
		if poke() {
			return nil
		}
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	eng := h.o.Engine()
	if eng == nil {
		return ErrNotParked
	}
	b, err := h.o.DB.GetBook(ctx, *m.BookID)
	if err != nil {
		return err
	}
	if state.Status(b.Status) != state.StatusNeedsAttention {
		if poke() {
			return nil
		}
		return ErrNotParked
	}
	if err := eng.Retry(ctx, b.ID); err != nil {
		if errors.Is(err, scheduler.ErrInvalidOp) {
			return ErrNotParked
		}
		return err
	}
	poke()
	return nil
}

// setPoke makes c the job's wake-up channel (its latest run's).
func (h *Handler) setPoke(jobID int64, c chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pokes[jobID] = c
}

// nudge sends a wake-up on c without waiting (one already pending is enough).
func nudge(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// dropPoke removes the job's wake-up channel if it is still c (a later run of
// the job may have its own by now).
func (h *Handler) dropPoke(jobID int64, c chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pokes[jobID] == c {
		delete(h.pokes, jobID)
	}
}

// errText is err's message, "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
