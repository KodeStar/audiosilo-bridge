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
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
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
	Engine    func() Engine
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
	return &Handler{o: o, pokes: map[int64]chan struct{}{}}
}

func (h *Handler) now() time.Time { return h.o.Now() }

// Kind is sidecars.generate.
func (h *Handler) Kind() string { return Kind }

// Lane is the pipeline lane.
func (h *Handler) Lane() string { return jobs.LanePipeline }

// Result is what a done job reports (contract section 1).
type Result struct {
	LibraryID   int64             `json:"library_id"`
	Path        string            `json:"path"`
	Source      string            `json:"source"`
	Files       []string          `json:"files"`
	Characters  int               `json:"characters"`
	Recaps      int               `json:"recaps"`
	CostUSD     float64           `json:"cost_usd"`
	WorkID      string            `json:"work_id"`
	Contributed map[string]string `json:"contributed,omitempty"`
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
	// unchanged files aren't read again; written is the hash of the sidecars
	// last written ("" for none yet); writeErr the last failed write's error,
	// logged once.
	side     sidecars
	sideSig  string
	written  string
	writeErr string
	srv      Server
	// poke is this run's wake-up channel (Retry); ownPark says the job's park is
	// one of the Bridge's own (book_not_writable, library_not_writable), set by
	// h.park; resuming says the job resumes an engine book (decided before
	// prepare runs).
	poke     chan struct{}
	ownPark  bool
	resuming bool
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
	if job.LibraryID == nil || job.Path == nil || strings.TrimSpace(*job.Path) == "" {
		return nil, errors.New("sidecars.generate needs a library and a book path")
	}
	r := &run{job: job, rep: rep, params: params, rel: *job.Path, poke: make(chan struct{}, 1)}
	// One wake-up channel for the whole run: a Retry reaches whichever park the
	// job waits in. It is this run's own, so a later attempt of the same job
	// keeps its channel when this one ends.
	h.setPoke(job.ID, r.poke)
	defer h.dropPoke(job.ID, r.poke)
	rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Starting"})
	eng, err := h.waitEngine(ctx, r)
	if err != nil {
		return nil, err
	}
	mapped, merr := h.o.DB.GetGenerateBook(ctx, job.ID)
	if merr != nil && !errors.Is(merr, store.ErrNotFound) {
		return nil, merr
	}
	// A mapping is this job's own only for the same library and book: the ids
	// are the server's, so another server (the Bridge paired anew) or a
	// reinstalled one hands out an id an earlier job's mapping still holds, and
	// following that would write another book's characters and recaps into this
	// one. The path is the server's canonical one, known once prepare has asked.
	resumed := merr == nil && mapped.BookID != nil && mapped.LibraryID == *job.LibraryID
	r.resuming = resumed
	if err := h.prepare(ctx, r, !resumed); err != nil {
		if resumed && mapped.RelPath == r.rel {
			r.bookID = *mapped.BookID // its own book: end() stops it
		}
		return h.end(ctx, r, nil, err)
	}
	if resumed && mapped.RelPath != r.rel {
		// That run's job is gone (its id is this job's now), so nothing follows it.
		h.o.Log.Warn("generate: the job's id maps to another book's run; this job starts its own",
			"job", job.ID, "mapped", mapped.RelPath, "path", r.rel)
		h.stopBook(context.WithoutCancel(ctx), job.ID, *mapped.BookID)
		resumed = false
	}
	if resumed {
		r.bookID = *mapped.BookID
		if err := h.attach(ctx, eng, r, mapped); err != nil {
			return h.end(ctx, r, nil, err)
		}
	} else if err := h.create(ctx, eng, r); err != nil {
		return h.end(ctx, r, nil, err)
	}
	res, err := h.follow(ctx, eng, r)
	return h.end(ctx, r, res, err)
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
// failed: a cancel would overwrite its error).
func (h *Handler) stopBook(ctx context.Context, jobID, bookID int64) {
	eng := h.o.Engine()
	if eng == nil {
		return
	}
	b, err := h.o.DB.GetBook(ctx, bookID)
	if err != nil || finished(b.State, b.Status) {
		return
	}
	if err := eng.Cancel(ctx, bookID); err != nil && !errors.Is(err, scheduler.ErrInvalidOp) {
		h.o.Log.Warn("generate: cancelling the engine book failed", "job", jobID, "book", bookID, "err", err)
	}
}

// waitEngine waits for the engine to have started.
func (h *Handler) waitEngine(ctx context.Context, r *run) (Engine, error) {
	for {
		if eng := h.o.Engine(); eng != nil {
			return eng, nil
		}
		r.rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Waiting for the engine to start"})
		if err := agent.SleepCtx(ctx, h.o.Poll); err != nil {
			return nil, err
		}
	}
}

// prepare resolves the library and the book through the CHECKED mapping, fetches
// the book from the server, compares its files and makes sure the book's own
// folder can be written. strict (a new job) fails on a library that isn't mapped
// and writable; a resumed one waits for it, parked.
func (h *Handler) prepare(ctx context.Context, r *run, strict bool) error {
	srv := h.o.Server()
	if srv == nil {
		return errors.New("the Bridge isn't paired")
	}
	r.srv = srv
	lib, ok := h.o.Libraries.Library(*r.job.LibraryID)
	if !ok {
		return fmt.Errorf("library %d isn't one the server listed", *r.job.LibraryID)
	}
	r.lib = lib
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
			return fmt.Errorf("library %q %s, so the Bridge can't write the book's characters and recaps", r.lib.Name, why)
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
		t := time.NewTimer(h.o.Recheck)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-r.poke:
		case <-t.C:
		}
		t.Stop()
	}
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
		Extra: map[string]any{"source": r.source, "park": map[string]any{"code": code, "hint": ParkHint(code)}}})
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

// create picks the source and makes the engine book for a new job: a book the
// engine already has from an earlier run (the same source path) is deleted
// first, unless another open job follows it or a stage of it runs right now.
func (h *Handler) create(ctx context.Context, eng Engine, r *run) error {
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
	if old, err := h.o.DB.GetBookBySourcePath(ctx, sp); err == nil {
		open, err := h.o.DB.OpenGenerateJobsForBook(ctx, old.ID, r.job.ID)
		if err != nil {
			return err
		}
		if len(open) > 0 {
			return fmt.Errorf("job %d is already generating this book; cancel it first", open[0])
		}
		if err := eng.Delete(ctx, old.ID); errors.Is(err, scheduler.ErrBusy) {
			return errors.New("the engine is still running a stage of this book's earlier run; try again in a minute")
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("could not clear the book's earlier run: %w", err)
		}
		h.o.Log.Info("generate: the book's earlier run was cleared", "job", r.job.ID, "old_book", old.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	nb := h.newBook(r, sp, ebookPath)
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
	h.o.Log.Info("generate: queued in the engine", "job", r.job.ID, "book", b.ID, "source", r.source, "path", r.rel)
	eng.Notify()
	return nil
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
	if b.Kind == string(state.KindEbook) {
		r.source, r.epub = SourceEbook, b.EbookPath
	} else {
		r.source, r.epub = SourceAudio, ""
	}
	if scheduler.IsCancelled(b) {
		if err := eng.Retry(ctx, b.ID); err != nil && !errors.Is(err, scheduler.ErrInvalidOp) {
			return err
		}
		h.o.Log.Info("generate: the job is back, so its engine book runs again", "job", r.job.ID, "book", b.ID)
	}
	h.o.Log.Info("generate: following the engine book again", "job", r.job.ID, "book", b.ID, "state", b.State)
	return nil
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
		r.costUSD, _ = h.o.DB.SumStageRunCost(ctx, b.ID)
	}
	v := view{book: b, source: r.source, progress: r.progress, costUSD: r.costUSD}
	v.eta, v.hasETA = eng.ETASeconds(b.ID)
	st, status := state.State(b.State), state.Status(b.Status)
	switch {
	case st == state.Done:
		res, err := h.finish(ctx, r, b, v.costUSD)
		return res, true, err
	case status == state.StatusFailed:
		return nil, true, fmt.Errorf("the engine failed the book at %s: %s", b.State, b.Error)
	}
	r.rep.Progress(describe(v))
	if status == state.StatusNeedsAttention && st == state.Contributing {
		_, err := h.write(ctx, r, b)
		if msg := errText(err); msg != r.writeErr {
			r.writeErr = msg
			if err != nil {
				h.o.Log.Warn("generate: writing the attachments at a contributing park failed", "job", r.job.ID, "err", err)
			}
		}
		r.rep.Progress(describe(v)) // the park again, after the write's own steps
	}
	r.engineParked(status == state.StatusNeedsAttention, b.ParkCode, b.Error)
	return nil, false, nil
}

// finish writes the attachments of a done book, rescans, and composes the result.
func (h *Handler) finish(ctx context.Context, r *run, b store.Book, cost float64) (any, error) {
	r.engineParked(false, "", "")
	s, err := h.write(ctx, r, b)
	if err != nil {
		return nil, err
	}
	res := Result{LibraryID: r.lib.ID, Path: r.rel, Source: r.source, Files: attachmentNames(r.place.prefix, s),
		Characters: s.nChars, Recaps: s.nRecaps, CostUSD: Cents(cost), WorkID: cmp.Or(b.WorkID, s.workID)}
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
// checked mapped and writable at the mount the job uses, and the book's folder
// writable (else the job parks until it is).
func (h *Handler) write(ctx context.Context, r *run, b store.Book) (sidecars, error) {
	s, err := r.loadSidecars(b.WorkDir)
	if err != nil {
		return sidecars{}, err
	}
	if s.hash == r.written {
		return s, nil
	}
	if err := h.waitMapped(ctx, r, false); err != nil {
		return sidecars{}, err
	}
	p, err := locate(r.mount, r.rel)
	if err != nil {
		return sidecars{}, err
	}
	r.place = p
	r.rep.Progress(jobs.Progress{Step: stepWriting, Message: "Saving the characters and recaps with the book",
		Extra: map[string]any{"stage": b.State, "source": r.source}})
	workID := cmp.Or(b.WorkID, s.workID)
	for {
		prov := provenance(h.o.Version, r.job.ID, h.now(), r.source, workID, inputsOf(r.mount, r.files, r.epub))
		err := writeAttachments(r.place.dir, r.place.prefix, s, prov)
		if err == nil {
			break
		}
		if werr := mapping.ProbeWritable(r.place.dir); werr == nil {
			return sidecars{}, fmt.Errorf("could not write the book's characters and recaps: %w", err)
		}
		if err := h.waitWritable(ctx, r); err != nil {
			return sidecars{}, err
		}
	}
	r.written = s.hash
	h.o.Log.Info("generate: wrote the book's attachments", "job", r.job.ID, "path", r.rel, "files", attachmentNames(r.place.prefix, s))
	r.rep.Progress(jobs.Progress{Step: stepRescanning, Message: "Asking the server to read them",
		Extra: map[string]any{"stage": b.State, "source": r.source}})
	h.rescan(ctx, r)
	return s, nil
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
			h.o.Log.Info("generate: the server read the attachments", "job", r.job.ID, "characters", got.Characters, "recaps", got.Recaps)
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
			select {
			case c <- struct{}{}:
			default:
			}
		}
		return waiting
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
