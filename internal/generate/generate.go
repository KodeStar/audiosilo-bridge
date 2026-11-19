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
	"maps"
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
	// ContinueStored runs write, a continue clearing a stopped run's cancel, under
	// book id's publish lock and publishes the book as stored once it succeeded, so
	// a failed Retry or rewind of the book never puts its status back over it.
	ContinueStored(ctx context.Context, id int64, write func() error) error
	// RetryAsRead re-admits book b as the caller read it: store.ErrStatusChanged
	// when its status or error changed since (a Cancel landing between).
	RetryAsRead(ctx context.Context, b store.Book) error
	// CancelUnlessFollowed stops book id while g holds (store.StopGuard, checked
	// in the store's cancel write): store.ErrFollowed, and nothing stopped, when a
	// later claim of g's job or another open job follows the book; store.ErrSettled
	// when the book finished or failed since the cancel read it.
	CancelUnlessFollowed(ctx context.Context, id int64, g store.StopGuard) error
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
	// Background starts a task that outlives the call starting it (Abandon's wait
	// for the engine, stopStartedStage) with a context that ends when the Bridge
	// stops, which then waits for the task before it closes the store (the app's
	// bgTasks). Nil: a goroutine nothing waits for, with a context that never ends.
	Background func(func(context.Context))
	Log        *slog.Logger
	Now        func() time.Time
}

// Handler runs sidecars.generate.
type Handler struct {
	o Options

	mu sync.Mutex
	// pokes wake a running job when its Retry is pressed (a park of the
	// Bridge's own, book_not_writable, checks again at once): each job id's
	// latest run's own channel, with its Reporter (another job asks it to
	// report now when it stands in that job's way, awaitOpenJobEnd).
	pokes map[int64]runHandle
	// loopParks is each park a job waits on in its own loop, not as an engine
	// book (a describe job's, describeParked; a generate job's owed share,
	// shareOwed), with that run's wake-up channel, so WakeParked finds the ones
	// a change answers.
	loopParks map[int64]loopPark
	// wakeSeq numbers WakeParked's calls and woken holds each park code's latest,
	// so a try that parks on a code woken while it ran tries again at once
	// (parkInLoop).
	wakeSeq uint64
	woken   map[state.ParkCode]uint64
	// asker is the runner ShareOff asks the jobs through (AskThrough).
	asker ShareAsker
	// doneJobs counts the jobs that finished done here (the heartbeat's
	// estimates are computed again when it moves).
	doneJobs atomic.Int64
	// ended is closed (and replaced) at each job's end here (release): a job
	// waiting for another to give way wakes on it, not at its next poll
	// (bridge#24). endMu guards it.
	endMu sync.Mutex
	ended chan struct{}
	// beforeEnd, a seam (tests), runs in release between its first stop and the
	// job's recorded end: the window another job's end can land in.
	beforeEnd func(jobID int64)
	// readMapping, a seam (tests), stands in for the store's GetGenerateBook in
	// resumeMapping and Abandon (mappingOf).
	readMapping func(ctx context.Context, serverJobID int64) (store.GenerateBook, error)
	// unusableFor is how long a book read keeps asking through answers that
	// can't be used (mapping.BookReadBudget; shorter in tests).
	unusableFor time.Duration
}

// endSignal is the channel the next job end here closes. A waiter takes it
// before it reads what it waits on, so an end in between still wakes it.
func (h *Handler) endSignal() <-chan struct{} {
	h.endMu.Lock()
	defer h.endMu.Unlock()
	if h.ended == nil {
		h.ended = make(chan struct{})
	}
	return h.ended
}

// signalEnd wakes every waiter on endSignal.
func (h *Handler) signalEnd() {
	h.endMu.Lock()
	defer h.endMu.Unlock()
	if h.ended != nil {
		close(h.ended)
		h.ended = nil
	}
}

// pollOrEnd sleeps one poll, or less when a job here ends first (ended, from
// endSignal).
func (h *Handler) pollOrEnd(ctx context.Context, ended <-chan struct{}) error {
	t := time.NewTimer(h.o.Poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
	case <-ended:
	}
	return nil
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
	if o.Background == nil {
		o.Background = func(fn func(context.Context)) { go fn(context.Background()) }
	}
	if o.ContribCredential == nil {
		o.ContribCredential = func(context.Context) error { return nil }
	}
	return &Handler{o: o, pokes: map[int64]runHandle{}, loopParks: map[int64]loopPark{},
		woken: map[state.ParkCode]uint64{}, unusableFor: mapping.BookReadBudget}
}

// loopPark is a park a run waits on in its own loop (loopParks): its code, and
// the run's wake-up channel.
type loopPark struct {
	code state.ParkCode
	poke chan struct{}
}

// WakeParked wakes every job parked with code in its own loop (loopParks) to
// try again now, and returns how many it woke: a monthly budget raised on the
// page, the agent's credential changed or a GitHub sign-in on the Accounts tab,
// re-admits the engine's parked books, while a describe job (or a generate
// job's owed share) waits in its own loop (until its retry_at, the 1st for the
// monthly budget, a Retry, or the Bridge's next start, which runs the job
// again). A job whose try is running then tries again at once if that try parks
// with code.
func (h *Handler) WakeParked(code state.ParkCode) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wakeSeq++
	h.woken[code] = h.wakeSeq
	woke := 0
	for _, p := range h.loopParks {
		if p.code == code {
			nudge(p.poke)
			woke++
		}
	}
	return woke
}

// wakeMark is WakeParked's latest call (a loop's before a try).
func (h *Handler) wakeMark() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wakeSeq
}

// reportPark reports a park as the job's: pr (its step and "Needs attention"
// message) with the park's code and retry_at (zero: none) in its Extra, then
// the park itself with reason.
func reportPark(r *run, pr jobs.Progress, code state.ParkCode, reason string, retryAt time.Time) {
	at := ""
	if !retryAt.IsZero() {
		at = store.Timestamp(retryAt)
	}
	extra := maps.Clone(pr.Extra)
	if extra == nil {
		extra = map[string]any{}
	}
	extra["park"] = parkExtra(string(code), at)
	pr.Extra = extra
	r.rep.Progress(pr)
	r.rep.Park(string(code), reason)
}

// parkInLoop reports park as the job's (reportPark, with pr) for a run that
// waits on it in its own loop, and registers it for WakeParked; woken says
// WakeParked woke its code after mark (wakeMark, taken before the try), so the
// park was answered while the try ran and the loop tries again at once. A Retry
// pressed while that try ran was answered by it: a park with a retry_at drains
// it first, so only one pressed once this park is reported tries again before
// its retry_at (a park without one waits for a press, so a press made meanwhile
// is kept).
func (h *Handler) parkInLoop(r *run, park *scheduler.ParkError, pr jobs.Progress, mark uint64) (woken bool) {
	if !park.RetryAfter.IsZero() {
		select {
		case <-r.poke:
		default:
		}
	}
	reportPark(r, pr, park.Code, park.Reason, park.RetryAfter)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loopParks[r.job.ID] = loopPark{code: park.Code, poke: r.poke}
	return h.woken[park.Code] > mark
}

// dropLoopPark drops r's parkInLoop registration (a later run of the job may
// have its own by then).
func (h *Handler) dropLoopPark(r *run) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loopParks[r.job.ID].poke == r.poke {
		delete(h.loopParks, r.job.ID)
	}
}

func (h *Handler) now() time.Time { return h.o.Now() }

// Kind is sidecars.generate.
func (h *Handler) Kind() string { return Kind }

// Lane is the pipeline lane.
func (h *Handler) Lane() string { return jobs.LanePipeline }

// NeedsClaimRow is true for a share job (contribute issue, or params that
// don't read): its share_off is kept on the claim's row, which turns its books
// local (jobs.ClaimRowNeeder), so it never runs on a claim the store couldn't
// record.
func (h *Handler) NeedsClaimRow(job jobs.Job) bool {
	p, err := ParseParams(job.Params)
	return err != nil || p.Contribute == ContributeIssue
}

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
	// ShareSkipped is why a job that was to share kept its results local
	// (ShareSkippedContributionsOff, ShareSkippedNoShareJob); omitted otherwise.
	ShareSkipped string `json:"share_skipped,omitempty"`
}

// Result.ShareSkipped values (the console words them, and words one it
// doesn't know plainly).
const (
	// ShareSkippedContributionsOff is a job whose share the server turned off
	// (its Contributions module off: jobs.Reporter.ShareOff, server#218).
	ShareSkippedContributionsOff = store.ShareSkippedContributionsOff
	// ShareSkippedNoShareJob is a job whose engine book reached its share with
	// no run of the job following it (ShareOff: a fault, such as a lapsed
	// lease, left the book running alone), so the book was kept local.
	ShareSkippedNoShareJob = store.ShareSkippedNoShareJob
)

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
	// once it changed. detectedUnseen says the last look couldn't look at the
	// book's audio (a mount gone for a moment): the list is neither current nor
	// stale, and is looked at again at the next look.
	detected       pipeline.DetectedChapters
	detectedOK     bool
	detectedUnseen bool
	detectedStamp  string
	// readalongAsked says the run handed its engine book the read-along request
	// (readalong.align: the ebook and the server's files, written once a run).
	readalongAsked bool
	// shareSkipped is why the run kept the results it was to share local
	// (keepShareLocal), "" while it shares as asked; shareLifted says it ran a
	// share park's stage again in local mode (once a run).
	shareSkipped string
	shareLifted  bool
}

// wakeOnShareOff has a contribute=issue sidecars.generate run look at its
// engine book at once when the job hears share_off (jobs.Reporter.OnShareOff),
// not at its next look, which can be an idle poll away: the store has the
// book local by then (store.DB.SetServerJobShareOff), and keepShareLocal, at
// the look, lifts a share park and records why the share was skipped. Once a
// run: the server says share_off in every answer while its module is off, and
// a wake at each would have a park that waits on the same wake-up as a Retry
// (book_not_writable's save) try again at every report, each try reporting
// twice more. stop drops the call.
func (h *Handler) wakeOnShareOff(r *run) (stop func()) {
	if r.kind != Kind || r.params.Contribute != ContributeIssue {
		return func() {}
	}
	var once sync.Once
	r.rep.OnShareOff(func() { once.Do(func() { nudge(r.poke) }) })
	return func() { r.rep.OnShareOff(nil) }
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
	defer h.wakeOnShareOff(r)()
	eng, err := h.waitEngine(ctx, r)
	if err != nil {
		return nil, err
	}
	mapped, resumed, err := h.resumeMapping(ctx, job)
	if err != nil {
		return nil, err
	}
	// Asked after the wait for the engine, which a share_off can come during: a
	// share the server turned off needs no GitHub sign-in.
	h.keepShareLocal(ctx, nil, r, nil)
	if !resumed {
		// A new job before prepare can wait on its library; a resumed one that
		// makes a new engine book after all is checked by create.
		if err := h.cantContribute(ctx, r); err != nil {
			return nil, err
		}
	}
	return h.runEngineBook(ctx, eng, r, mapped, resumed, nil)
}

// keepShareLocal keeps the results of a contribute=issue job local once the
// server says its share is off (jobs.Reporter.ShareOff: its Contributions
// module was turned off, server#218), or once it finds its engine book kept
// local (by an earlier run of the job, a restart since and the module maybe on
// again, or by the contributing stage, which found no run of the job to ask:
// ShareOff): the run goes on, its share dropped for good, and its result says
// why (the book's own store.Book.ShareSkipped when it has one, else
// ShareSkippedContributionsOff). Before the engine book exists (b nil) the
// book is simply made in local mode. The book's own mode is the store's to
// turn (store.DB.SetServerJobShareOff and the job's mapping: a book the run
// takes up later, a resumed job's, included), so the contributing stage files
// nothing; one whose write failed is still asked about before anything is
// filed (ShareOff). A book parked at that stage over the share (shareParks)
// runs it again, once a run: a stage that was running in issue mode when the
// answer came parks there after it. A book done already shared as asked, and a
// filing the stage had under way when the answer came is not taken back
// (finish reports what was filed). A lift that failed is tried again at the
// next look. It reports whether it lifted a park (b is then as read before the
// lift). A book kept local only because no share job followed it (no_share_job:
// a lease that lapsed, a job's end that couldn't stop it) is shared again once
// its job follows it again with its share on (owesShare): one not done yet is
// turned back to issue mode (store.DB.ResumeShare), so its contributing stage
// files it; a done one is filed by finish (shareOwed). A share_off still wins.
func (h *Handler) keepShareLocal(ctx context.Context, eng Engine, r *run, b *store.Book) (lifted bool) {
	if r.owesShare(b) {
		if state.State(b.State) == state.Done {
			return false // finish files it (shareOwed)
		}
		// Held, so no stage runs on the book meanwhile: a contributing stage that
		// read it local would file it locally after the turn (and finish would
		// then owe nothing). A stage running now leaves it to the next look.
		release, ok := h.hold(b.ID)
		if !ok {
			return false
		}
		// Read again under the hold: a stage that ended since the look (the
		// contributing stage filed locally, the book done) leaves it to finish,
		// which a turn now would leave owing nothing.
		now, err := h.o.DB.GetBook(ctx, b.ID)
		turned := false
		if err == nil && state.State(now.State) != state.Done {
			turned, err = h.o.DB.ResumeShare(ctx, r.job.ID, r.job.Attempt, b.ID)
		}
		release()
		switch {
		case err == nil && state.State(now.State) == state.Done:
			return false // finish files it (shareOwed)
		case err != nil:
			// The share isn't dropped: the next look tries again.
			h.o.Log.Warn("generate: sharing the engine book again failed", "job", r.job.ID, "book", b.ID, "err", err)
			return false
		case turned:
			h.o.Log.Info("generate: the job follows its engine book again, so it is shared again", "job", r.job.ID, "book", b.ID)
			return false
		}
		// A share_off on the book (another job's, or this job's since) still wins.
	}
	if r.shareSkipped == "" {
		keptLocal := b != nil && b.ContribMode == store.ContribModeLocal
		if r.params.Contribute != ContributeIssue || (!keptLocal && !r.rep.ShareOff()) {
			return false
		}
	}
	if b != nil && b.ContribMode != store.ContribModeLocal && state.State(b.State) == state.Done {
		return false // it shared as asked
	}
	if r.shareSkipped == "" {
		h.dropShare(r, b)
	}
	if b == nil || r.shareLifted || !state.IsParkedWith(b.Status, b.ParkCode, shareParks...) {
		return false
	}
	switch err := eng.RetryAsRead(ctx, *b); {
	case errors.Is(err, store.ErrStatusChanged):
		return false // the book moved meanwhile: the next look sees it as it is
	case err != nil:
		// Nothing was lifted (the park is as it was), so the next look tries
		// again: giving up here would leave the job waiting on a share it no
		// longer makes.
		h.o.Log.Warn("generate: running the contributing stage again in local mode failed", "job", r.job.ID,
			"book", b.ID, "err", err)
		return false
	}
	r.shareLifted = true
	return true
}

// dropShare drops the run's share for good: its results stay local, and its
// result says why (b's own store.Book.ShareSkipped when it has one and no
// share_off was heard, else ShareSkippedContributionsOff).
func (h *Handler) dropShare(r *run, b *store.Book) {
	why := ShareSkippedContributionsOff
	if b != nil && b.ShareSkipped != "" && !r.rep.ShareOff() {
		why = b.ShareSkipped
	}
	r.params.Contribute, r.shareSkipped = ContributeLocal, why
	h.o.Log.Info("generate: this job's share is off; its results stay local", "job", r.job.ID, "why", why)
}

// owesShare reports whether b was kept local only because no share job
// followed it (store.ShareSkippedNoShareJob) while this run shares it: a
// sidecars.generate run with contribute=issue whose share is still on (no
// share_off heard, nothing dropped yet).
func (r *run) owesShare(b *store.Book) bool {
	return b != nil && r.kind == Kind && r.shareSkipped == "" && r.params.Contribute == ContributeIssue &&
		!r.rep.ShareOff() && b.ContribMode == store.ContribModeLocal && b.ShareSkipped == store.ShareSkippedNoShareJob
}

// shareOwed files the share of a done engine book that was kept local only
// because no share job followed it, now that this run does (owesShare): its
// contributing stage ran in local mode, so the run files the sidecars itself as
// sidecars.contribute does (shareConfirmed: the server asked just before),
// once the store agrees the share stands (store.DB.ShareResumable: a share_off
// on the book still wins), then records the book shared (store.DB.ResumeShare).
// It reports whether it filed; a book whose share doesn't stand is dropped as
// keepShareLocal drops it.
func (h *Handler) shareOwed(ctx context.Context, r *run, b store.Book) (bool, error) {
	ok, err := h.o.DB.ShareResumable(ctx, r.job.ID, r.job.Attempt, b.ID)
	if err != nil {
		return false, err
	}
	var sharer Sharer
	if h.o.Sharer != nil {
		sharer = h.o.Sharer()
	}
	if !ok || sharer == nil {
		h.dropShare(r, &b)
		return false, nil
	}
	h.o.Log.Info("generate: the job follows its engine book again, so it files the share", "job", r.job.ID, "book", b.ID)
	r.rep.Progress(jobs.Progress{Step: stepSharing, Message: "Sharing with the community", Total: 1})
	// Asked again under the hold, after the server's answer: a share_off stored
	// since the look above (another job's on the book) still wins.
	stands := func(ctx context.Context) (bool, error) {
		return h.o.DB.ShareResumable(ctx, r.job.ID, r.job.Attempt, b.ID)
	}
	if err := h.shareOwedParked(ctx, r, sharer, b, stands); err != nil {
		if errors.Is(err, errShareOffNow) || errors.Is(err, pipeline.ErrShareStopped) {
			h.dropShare(r, &b)
			return false, nil
		}
		return false, err
	}
	if _, err := h.o.DB.ResumeShare(ctx, r.job.ID, r.job.Attempt, b.ID); err != nil {
		h.o.Log.Warn("generate: recording the engine book shared failed", "job", r.job.ID, "book", b.ID, "err", err)
	}
	return true, nil
}

// shareOwedParked files an owed share (shareConfirmed) and waits out the parks
// the contributing stage would wait on in the same case, so a done book's share
// ends as a stage's would: a GitHub sign-in missing or not refreshed
// (contrib_unavailable), a server that couldn't be asked while it filed
// (share_unconfirmed). The job parks with the stage's code, reason and
// retry_at, stays parked while it tries again, and tries again at the
// retry_at, on Retry (which pokes the job, its book being done), or on a
// WakeParked for its code (a GitHub sign-in). Any other failure is the
// job's, as before.
func (h *Handler) shareOwedParked(ctx context.Context, r *run, sharer Sharer, b store.Book,
	stands func(context.Context) (bool, error)) error {
	defer h.dropLoopPark(r)
	for {
		mark := h.wakeMark()
		_, err := h.shareConfirmed(ctx, r, sharer, b.ID, ContributeParams{WorkID: b.WorkID}, stands)
		var park *scheduler.ParkError
		if !errors.As(err, &park) || ctx.Err() != nil {
			// Lifts this loop's park (nothing when there is none); the job ends or
			// goes on to finish.
			r.rep.Unpark()
			return err
		}
		// A share_off heard while the try ran, or before parkInLoop drained its
		// wake-up (wakeOnShareOff, once a run), would leave the job parked over
		// a share that is off until the retry_at: shareConfirmed drops it now.
		if r.rep.ShareOff() {
			continue
		}
		h.o.Log.Warn("generate: the share it owes waits", "job", r.job.ID, "book", b.ID, "code", park.Code, "err", err)
		if h.parkInLoop(r, park, sharingParked, mark) || r.rep.ShareOff() {
			continue // woken while the try ran, or share_off heard since
		}
		if err := h.recheckWait(ctx, r, park.RetryAfter); err != nil {
			return err
		}
	}
}

// shareParks are the contributing stage's parks over the share itself (a
// GitHub sign-in, the server's word on the share, a core record the community
// lacks), which local mode doesn't wait on.
var shareParks = []state.ParkCode{state.ParkContribUnavailable, state.ParkShareUnconfirmed, state.ParkCoreNeeded,
	state.ParkCorePending}

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
// characters and recaps into this one. A read that fails is tried again
// (stopTries, a poll apart): a job that failed on it would leave the engine book
// an earlier run made going on with no job to follow or stop it. A job that
// ends here anyway (the reads kept failing, or it was cancelled meanwhile)
// releases that book as Abandon does (when Abandon's own read of the mapping
// works by then), unless it resumes later; a book nothing can stop is kept
// local by its contributing stage all the same (ShareOff: no_share_job).
func (h *Handler) resumeMapping(ctx context.Context, job jobs.Job) (store.GenerateBook, bool, error) {
	for i := 1; ; i++ {
		mapped, err := h.mappingOf(ctx, job.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return store.GenerateBook{}, false, nil
		case err == nil:
			return mapped, mapped.BookID != nil && mapped.LibraryID == *job.LibraryID, nil
		}
		if i < stopTries {
			h.o.Log.Warn("generate: reading the job's earlier run failed; trying again", "job", job.ID, "err", err)
			if err = agent.SleepCtx(ctx, h.o.Poll); err == nil {
				continue
			}
		}
		if ctx.Err() == nil || !jobs.Resumes(ctx) {
			h.Abandon(context.WithoutCancel(ctx), job)
		}
		return store.GenerateBook{}, false, err
	}
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
		h.stopBook(context.WithoutCancel(ctx), stopGuard(r.job, false), *mapped.BookID, false)
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
	h.setPoke(job.ID, r.poke, rep)
	rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Starting"})
	return r, func() { h.dropPoke(job.ID, r.poke) }, nil
}

// errCantContribute is a contribute=issue job on a Bridge with no GitHub
// credential; errCantContributeUnrefreshed one on a Bridge signed in to GitHub
// whose sign-in couldn't be refreshed.
var (
	errCantContribute = errors.New("this job asks to contribute the characters and recaps (contribute: issue), but " +
		"the Bridge isn't signed in to GitHub: sign in on the Bridge's Accounts tab, or generate the book with " +
		"contribute set to local")
	errCantContributeUnrefreshed = errors.New("this job asks to contribute the characters and recaps (contribute: " +
		"issue), but the Bridge's GitHub sign-in couldn't be refreshed (" + contrib.UnrefreshedWhy + "): generate " +
		"the book again once GitHub answers, or with contribute set to local")
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
		// The engine made (or took up) the book, so it is up: the transcripts of
		// a book whose retention is 0 days go now.
		endCtx := context.WithoutCancel(ctx)
		if h.release(endCtx, r.job, r.bookID, err != nil) {
			h.purgeAtEnd(endCtx, r.job, r.bookID)
		}
	}
	return res, err
}

// Abandon is the jobs runner telling the handler it ended a job without running
// it (the first report after a restart was answered with a cancel or a lost
// claim, or the job was another server's): the engine book an earlier run of the
// job created is released as end() would, or it would go on with nothing to
// follow it.
func (h *Handler) Abandon(ctx context.Context, job jobs.Job) {
	m, err := h.mappingOf(ctx, job.ID)
	if err != nil || m.BookID == nil || m.EndedAt != "" {
		return
	}
	bookID := *m.BookID
	engineUp := h.o.Engine() != nil
	// Released before Abandon returns, even with no engine yet (its first start
	// may download ffmpeg): the runner records the job lost once Abandon returns,
	// and a job waiting for this one's end must find the book stopped (bridge#65).
	ended := h.release(ctx, job, bookID, true)
	if engineUp {
		if ended {
			h.purgeAtEnd(ctx, job, bookID)
		}
		return
	}
	// The rest waits for the engine, among the Bridge's background tasks: it stops
	// with the Bridge, never on a store that has closed (ctx may never end: a job
	// cancelled while it waited for the engine abandons with one that doesn't).
	h.o.Background(func(ctx context.Context) { h.stopStartedStage(ctx, job, bookID) })
}

// stopStartedStage waits for the engine to start, then stops a stage it began on
// book bookID before the store's cancel landed (its scheduler starts beside the
// Bridge's jobs), or a status such a stage wrote over the cancel (stopBook,
// cancelled books included): the engine's guarded cancel, which also keeps a dispatch
// pass that read the book before the store's cancel from starting a stage on it
// afterwards. The transcripts of a book whose retention is 0 days go only now
// (purgeAtEnd): with no engine nothing held the book against such a stage. A
// later claim of the same job that follows the book now (one already running
// here, which release left the book to, or one handed out since, whose attach
// reopened the job's mapping) keeps it, as in release: the checks below skip the
// common case, and the cancel is written only while the job's mapping is still
// ended and no later attempt is recorded, in the one statement (store.StopGuard),
// so a claim landing between the checks and the cancel is never stopped.
func (h *Handler) stopStartedStage(ctx context.Context, job jobs.Job, bookID int64) {
	for h.o.Engine() == nil {
		if agent.SleepCtx(ctx, h.o.Poll) != nil {
			return
		}
	}
	if sj, err := h.o.DB.GetServerJob(ctx, job.ID); err == nil && sj.Attempt > job.Attempt {
		return
	}
	if m, err := h.o.DB.GetGenerateBook(ctx, job.ID); err != nil || m.EndedAt == "" {
		return
	}
	if h.stopBook(ctx, stopGuard(job, true), bookID, true) {
		return
	}
	h.purgeAtEnd(ctx, job, bookID)
}

// release ends a job's hold on its engine book: a job that didn't finish stops
// the book (nothing follows it any more), then the end is recorded (the
// transcript retention counts from it, and the supervisor leaves a book no job
// follows alone); the caller then deletes the transcripts of a book whose
// retention is 0 days (purgeAtEnd), once the engine is up. The stop
// comes first so a job waiting for this one's end (awaitOpenJobEnd) starts only
// once the engine's cancel has returned. A stop left to another open job is
// tried again once this end is recorded: two jobs on the book ending at once
// would each leave it to the other, and the later end then stops it. A later
// claim of the same job may run here already (the server re-queued it after a
// lease ran out and handed it out again before this attempt heard it was lost):
// that attempt follows the book now, so it is left alone, and ended is false. The
// stop and the end are written only while no later attempt is recorded, each in
// its one statement (store.StopGuard): a claim landing after the check is never
// stopped, nor its mapping ended.
func (h *Handler) release(ctx context.Context, job jobs.Job, bookID int64, stop bool) (ended bool) {
	if sj, err := h.o.DB.GetServerJob(ctx, job.ID); err == nil && sj.Attempt > job.Attempt {
		return false
	}
	g := stopGuard(job, false)
	followed := stop && h.stopBook(ctx, g, bookID, false)
	if h.beforeEnd != nil {
		h.beforeEnd(job.ID)
	}
	switch err := h.o.DB.EndGenerateBook(ctx, job.ID, job.Attempt, h.now()); {
	case errors.Is(err, store.ErrFollowed):
		return false
	case err != nil:
		h.o.Log.Warn("generate: recording the job's end failed", "job", job.ID, "err", err)
	}
	if followed {
		h.stopBook(ctx, g, bookID, false)
	}
	h.signalEnd()
	return true
}

// purgeAtEnd deletes the transcripts of a job's engine book as the job ends, when
// transcripts.retention_days is 0 (purgeEnded). It needs the engine up: the
// engine's hold on the book is what keeps a stage from starting on it meanwhile
// (hold takes none without an engine, and its scheduler may be starting).
func (h *Handler) purgeAtEnd(ctx context.Context, job jobs.Job, bookID int64) {
	if h.o.RetentionDays != 0 {
		return
	}
	if err := h.purgeEnded(ctx, job, bookID); err != nil {
		h.o.Log.Warn("generate: deleting the transcripts failed", "job", job.ID, "err", err)
	}
}

// stopBook cancels a job's engine book unless it already ended (done, or
// failed: a cancel would overwrite its error; one that ends between the read and
// the cancel's write is left too, store.ErrSettled) or another open job follows it now
// (a generate job continued the chapters run, so a late end of the chapters job
// leaves it to that job). With no engine yet the cancel is the store's alone
// (scheduler.CancelStoredUnlessFollowed): nothing runs the book to interrupt.
// recancel stops a book already cancelled too (stopStartedStage: a stage the
// engine began before the cancel landed). The cancel is written only while g
// holds (store.StopGuard: no later attempt of the job, no other open job on the
// book, the job's mapping still ended when g.Ended), in the store's one
// statement. A cancel that fails is tried again (stopTries, a poll apart); one
// that still fails turns the book local (store.DB.KeepBookLocal, why
// ShareSkippedNoShareJob), so a book that runs on with nothing following it
// never files. It reports whether it left the book to another open job or a
// later claim.
func (h *Handler) stopBook(ctx context.Context, g store.StopGuard, bookID int64, recancel bool) (followed bool) {
	b, err := h.o.DB.GetBook(ctx, bookID)
	if err != nil || (finished(b.State, b.Status) && (!recancel || !scheduler.IsCancelled(b))) {
		return false
	}
	for i := 1; ; i++ {
		if eng := h.o.Engine(); eng != nil {
			err = eng.CancelUnlessFollowed(ctx, bookID, g)
		} else {
			err = scheduler.CancelStoredUnlessFollowed(ctx, h.o.DB, bookID, g)
		}
		switch {
		case errors.Is(err, store.ErrFollowed):
			return true
		case err == nil || errors.Is(err, scheduler.ErrInvalidOp) || errors.Is(err, store.ErrSettled) ||
			errors.Is(err, store.ErrNotFound):
			// Stopped, settled, or deleted since the read: nothing runs on.
			return false
		}
		if i == stopTries || agent.SleepCtx(ctx, h.o.Poll) != nil {
			break
		}
	}
	h.o.Log.Warn("generate: cancelling the engine book failed; it is kept local", "job", g.JobID, "book", bookID, "err", err)
	// Written even when the Bridge is stopping (ctx ended the tries): the book
	// may run on at the next start.
	if err := h.o.DB.KeepBookLocal(context.WithoutCancel(ctx), bookID, ShareSkippedNoShareJob); err != nil {
		h.o.Log.Warn("generate: keeping the engine book local failed", "job", g.JobID, "book", bookID, "err", err)
	}
	return false
}

// stopTries is how many times stopBook tries a cancel that fails, and
// resumeMapping a read.
const stopTries = 3

// mappingOf reads server job id's mapping (store.DB.GetGenerateBook, or the
// readMapping seam): resumeMapping and Abandon read it alike, so a read that
// fails fails both.
func (h *Handler) mappingOf(ctx context.Context, id int64) (store.GenerateBook, error) {
	if h.readMapping != nil {
		return h.readMapping(ctx, id)
	}
	return h.o.DB.GetGenerateBook(ctx, id)
}

// stopGuard is the StopGuard of job's end: its id and attempt, and whether the
// end read the job's mapping ended already.
func stopGuard(job jobs.Job, ended bool) store.StopGuard {
	return store.StopGuard{JobID: job.ID, Attempt: job.Attempt, Ended: ended}
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
// (the server unreachable or restarting, a 5xx, a 408 or 429, a refused token the
// runner is dealing with: mapping.BookReadSettled): only a definite answer (no
// such book, another 4xx) fails the job,
// so a restart while the server is down doesn't end a long run. The one answer
// with an end to its wait is a 2xx the Bridge can't use (link.ErrUnusableAnswer:
// a proxy's sign-in page at the server's address, which no wait turns into the
// book): unusableFor of nothing else, the last ask at its end, fails the job.
func (h *Handler) fetchBook(ctx context.Context, r *run) (*link.Book, error) {
	wait := h.o.Poll
	// unusable is when the answers now coming back unusable began (zero: the
	// last one wasn't).
	var unusable time.Time
	for {
		r.rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Asking the server for the book"})
		book, err := r.srv.Book(ctx, r.lib.ID, r.rel)
		switch {
		case err == nil:
			return book, nil
		case errors.Is(err, link.ErrNoBook):
			return nil, fmt.Errorf("the server has no indexed book at %s", r.rel)
		case mapping.BookReadSettled(err):
			return nil, fmt.Errorf("could not get the book from the server: %w", err)
		}
		sleep := wait
		if !errors.Is(err, link.ErrUnusableAnswer) {
			unusable = time.Time{}
		} else {
			if unusable.IsZero() {
				unusable = time.Now()
			}
			left := h.unusableFor - time.Since(unusable)
			if left <= 0 {
				return nil, fmt.Errorf("the server kept giving an answer for the book this Bridge couldn't use (%w): "+
					"check the server's address is the AudioSilo server's, not a proxy's sign-in page", err)
			}
			sleep = min(wait, left)
		}
		h.o.Log.Warn("generate: asking the server for the book failed; trying again", "job", r.job.ID, "err", err)
		if err := agent.SleepCtx(ctx, sleep); err != nil {
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
	switch r.kind {
	case ChaptersKind:
		return h.createChapters(ctx, eng, r)
	case ReadalongKind:
		return h.createReadalong(ctx, eng, r)
	}
	// The server may have said share_off since Run asked (prepare can wait a long
	// time, and a resumed job's first report may not have got through): the book
	// is then made in local mode, with no GitHub check.
	h.keepShareLocal(ctx, nil, r, nil)
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
	old, err := h.earlierRun(ctx, r, sp)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	found := err == nil
	// Read from the audio, the job also takes on the audio run beside an ebook run
	// (a read-along's or a chapters job's, store.AudioRunKey), the ebook run
	// cleared: the whole run, the book's notes, is the audio's now. It is cleared
	// only once the job is sure to go on: inside a continue, under the audio run's
	// hold, or after the audio run when neither continue takes it, so a refused
	// continue keeps both runs as they were (bridge#34). Read from the ebook, the
	// job leaves the audio run alone.
	ebookRun := int64(0)
	if r.source == SourceAudio {
		beside, err := h.earlierRun(ctx, r, store.AudioRunKey(sp))
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil {
			if found {
				ebookRun = old.ID
			}
			old, found = beside, true
		}
	}
	if found {
		if ok, err := h.continueReadalongRun(ctx, eng, r, old, ebookRun); ok || err != nil {
			return err
		}
		if ok, err := h.continueChapters(ctx, eng, r, old, ebookRun); ok || err != nil {
			return err
		}
		if err := h.clearEarlier(ctx, eng, r, old.ID); err != nil {
			return err
		}
		if err := h.clearEbookRun(ctx, eng, r, ebookRun); err != nil {
			return err
		}
	}
	return h.queueBook(ctx, eng, r, h.newBook(r, sp, ebookPath))
}

// earlierRun is the engine book at source path key (store.ErrNotFound for none,
// or one that went while the job waited), once a chapters job following it gave
// way; any other open job on it refuses the job.
func (h *Handler) earlierRun(ctx context.Context, r *run, key string) (store.Book, error) {
	old, err := h.o.DB.GetBookBySourcePath(ctx, key)
	if err != nil {
		return store.Book{}, err
	}
	old, openID, openKind, err := h.awaitChaptersGivingWay(ctx, r, old)
	if err == nil && openID != 0 {
		// A job the server cancelled a moment ago ends first (BRIDGE-01), and a
		// chapters run it followed is then given the chance to give way.
		if openID, openKind, err = h.awaitOpenJobEnd(ctx, r, old.ID); err == nil && openID == 0 {
			if old, err = h.o.DB.GetBook(ctx, old.ID); err == nil {
				old, openID, openKind, err = h.awaitChaptersGivingWay(ctx, r, old)
			}
		}
	}
	switch {
	case err != nil:
		return store.Book{}, err
	case openID != 0 && openKind == ChaptersKind:
		return store.Book{}, fmt.Errorf("job %d is finding this book's chapters; let it finish (this job can then go on from its transcription) or cancel it first", openID)
	case openID != 0 && openKind == ReadalongKind:
		return store.Book{}, fmt.Errorf("job %d is making this book's read-along; let it finish or cancel it first", openID)
	case openID != 0:
		return store.Book{}, fmt.Errorf("job %d is already generating this book; cancel it first", openID)
	}
	return old, nil
}

// openJobWait bounds a new job's wait for an open job on the same engine book
// that the server has stopped to end here (BRIDGE-01): an admin cancels a parked
// job and asks again at once, before that job's next keepalive (up to 30 s) has
// told the Bridge. The new job asks it to report now (jobs.Reporter.Refresh), so
// the wait is usually one round trip and the job's end.
const openJobWait = 30 * time.Second

// awaitOpenJobEnd returns the first open job other than r's following engine
// book bookID (firstOpenJob, id 0 for none) once the ones the server has stopped
// have ended here. Each is asked to report now: one the server still holds open
// is returned at once, for the caller to refuse; one its answer stopped, or one
// that couldn't be asked (not running here, the server not reached), is waited
// for, up to openJobWait.
func (h *Handler) awaitOpenJobEnd(ctx context.Context, r *run, bookID int64) (int64, string, error) {
	asked := map[int64]bool{}
	deadline := time.Now().Add(openJobWait)
	for {
		ended := h.endSignal()
		id, kind, err := h.firstOpenJob(ctx, bookID, r.job.ID)
		if err != nil || id == 0 {
			return id, kind, err
		}
		if !asked[id] {
			asked[id] = true
			if h.refreshJob(ctx, id) {
				return id, kind, nil
			}
		}
		if !time.Now().Before(deadline) {
			return id, kind, nil
		}
		r.rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Waiting for the book's earlier job to end"})
		if err := h.pollOrEnd(ctx, ended); err != nil {
			return 0, "", err
		}
	}
}

// refreshJob asks open job id, when it runs here, to report now (acting on the
// answer: a cancel ends it), and reports whether the server still holds it open.
func (h *Handler) refreshJob(ctx context.Context, id int64) bool {
	rep := h.reporter(id)
	return rep != nil && rep.Refresh(ctx) == nil
}

// reporter is the Reporter of open job id's run here, nil when none is up.
func (h *Handler) reporter(id int64) *jobs.Reporter {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pokes[id].rep
}

// ShareAsker asks a job's run here about its share (jobs.Runner.ShareOff).
type ShareAsker interface {
	ShareOff(ctx context.Context, id int64, ask bool) (bool, error)
}

// AskThrough has ShareOff ask the jobs' runs through a (the runner the
// handler's jobs run in), set before the runner and the engine start. Without
// one ShareOff can ask no job, so a share job's book waits.
func (h *Handler) AskThrough(a ShareAsker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asker = a
}

// ShareOff answers the contributing stage for engine book bookID in issue mode
// (pipeline.Config.ShareOff): why the book is to stay local, "" while an open
// generate-and-share job that follows it still shares it. The job hears
// share_off only in an answer to one of its reports, which can be a keepalive
// away (or an idle poll, while its run waits for the book), so with ask each
// such job reports now (through the runner, a job still waiting for its lane
// slot included), and the answer acts as any other: the store turns the book
// local. Without ask only what is known here answers, with no request: the
// stage asks so before its lookups, to wait at once on a job that can't be
// asked. The first job with its share off answers ShareSkippedContributionsOff
// (the book is local whatever the others say, one that couldn't be asked
// included): one that stored or heard share_off already needs no asking, and
// an answer that stopped the job counts when it carried share_off (heard
// before the stop). Otherwise an error when one couldn't be asked (the server
// not reached, the Bridge unpaired or its token refused, the answer stopped
// the job, or no run of it here), for the stage to wait and file nothing.
// Other jobs (contribute local, or another kind) are not asked; params that
// don't read are asked as a share's. A book that no open share job follows
// answers ShareSkippedNoShareJob (fail closed): a fault left it running with
// nothing to ask (a crash between its creation and its mapping, a job's end
// that couldn't stop it, a lease that lapsed), and nothing can say the share
// it was made for still stands.
func (h *Handler) ShareOff(ctx context.Context, bookID int64, ask bool) (string, error) {
	open, err := h.o.DB.OpenGenerateJobsForBook(ctx, bookID, 0)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	asker := h.asker
	h.mu.Unlock()
	shares := false
	var unasked error // the first job that couldn't be asked, once no other is off
	for _, id := range open {
		sj, err := h.o.DB.GetServerJob(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		if sj.Kind != Kind {
			continue
		}
		if p, err := ParseParams(sj.Params); err == nil && p.Contribute != ContributeIssue {
			continue
		}
		shares = true
		if sj.ShareOff {
			return ShareSkippedContributionsOff, nil
		}
		var off bool
		if asker == nil {
			err = fmt.Errorf("job %d can't be asked here", id)
		} else if off, err = asker.ShareOff(ctx, id, ask); err != nil {
			err = fmt.Errorf("job %d: %w", id, err)
		}
		switch {
		case off:
			return ShareSkippedContributionsOff, nil
		case err != nil && unasked == nil:
			unasked = err
		}
	}
	if !shares {
		return ShareSkippedNoShareJob, nil
	}
	return "", unasked
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

// clearEbookRun clears the ebook run (id; 0 for none) beside the audio run a job
// reading the audio takes on (create), once it is sure to.
func (h *Handler) clearEbookRun(ctx context.Context, eng Engine, r *run, id int64) error {
	if id == 0 {
		return nil
	}
	return h.clearEarlier(ctx, eng, r, id)
}

// queueBook makes the job's engine book nb in a batch of its own, mapped to the
// job in the same write (a crash between the two would leave a book running
// that no job follows), and wakes the engine.
func (h *Handler) queueBook(ctx context.Context, eng Engine, r *run, nb store.NewBook) error {
	batch, err := h.o.DB.CreateBatch(ctx)
	if err != nil {
		return err
	}
	nb.BatchID = batch.ID
	b, err := h.o.DB.CreateMappedBook(ctx, nb, r.generateBook())
	if err != nil {
		return fmt.Errorf("could not queue the book in the engine: %w", err)
	}
	r.bookID = b.ID
	h.o.Log.Info("generate: queued in the engine", "job", r.job.ID, "kind", r.kind, "book", b.ID, "source", r.source, "path", r.rel)
	eng.Notify()
	return nil
}

// deleteEarlier deletes the engine book of the book's earlier run, waiting a few
// polls (giveWayTries) while the engine is busy with it: a description reading it,
// which the server asked to give way to this job and which lets go at its next
// report, or a stage the earlier job's end just stopped. scheduler.ErrBusy when it
// stays busy.
func (h *Handler) deleteEarlier(ctx context.Context, eng Engine, r *run, id int64) error {
	for i := 1; ; i++ {
		err := eng.Delete(ctx, id)
		if !errors.Is(err, scheduler.ErrBusy) || i == giveWayTries {
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
		// The cancel read here is the one taken up: a book whose status changed
		// since (re-admitted already, or parked by a stage the cancel interrupted)
		// is followed as it is (bridge#65).
		switch err := eng.RetryAsRead(ctx, b); {
		case err == nil:
			h.o.Log.Info("generate: the job is back, so its engine book runs again", "job", r.job.ID, "book", b.ID)
		case !errors.Is(err, scheduler.ErrInvalidOp) && !errors.Is(err, store.ErrStatusChanged):
			return err
		}
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
	if r.kind == Kind && h.keepShareLocal(ctx, eng, r, &b) {
		// The share's park lifted: the look goes on with the book as it is now,
		// never reporting the park it no longer has, and the next look comes at
		// once (follow read the book parked, and would wait an idle poll).
		if now, err := h.o.DB.GetBook(ctx, b.ID); err == nil {
			b = now
		}
		nudge(r.poke)
	}
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
	if r.kind == ReadalongKind {
		if err := h.readalongAsk(ctx, r, b); err != nil {
			return nil, true, err
		}
	}
	switch {
	case st == state.Done && r.kind == ChaptersKind:
		res, err := h.finishChapters(ctx, r, b)
		return res, true, err
	case st == state.Done && r.kind == ReadalongKind:
		res, err := h.finishReadalong(ctx, r, b)
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
	if r.kind == ChaptersKind || r.kind == ReadalongKind {
		delete(pr.Extra, "cost_usd") // speech recognition, detection and alignment only: no agent spend
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
	if r.owesShare(&b) {
		if _, err := h.shareOwed(ctx, r, b); err != nil {
			if ctx.Err() != nil || errors.Is(err, jobs.ErrStopped) {
				return nil, err
			}
			h.o.Log.Warn("generate: the share failed", "job", r.job.ID, "path", r.rel, "err", err)
			return nil, shareError(err)
		}
	} else if r.shareSkipped == "" && r.params.Contribute == ContributeIssue && b.ContribMode == store.ContribModeLocal {
		// A share it owed (keepShareLocal left a done no_share_job book to this
		// point) that a share_off heard since (the write can wait a long time)
		// dropped: the result says so.
		h.dropShare(r, &b)
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
	// A share kept local (keepShareLocal) may have filed before the server's
	// answer came: what was filed is reported, and then it wasn't skipped.
	if r.params.Contribute == ContributeIssue || r.shareSkipped != "" {
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
	if len(res.Contributed) == 0 && b.ContribMode == store.ContribModeLocal {
		// A book that ran its share in issue mode (done before the answer came)
		// shared as asked, whatever it filed. One the contributing stage kept
		// local on an answer only this run had (the Reporter's share_off, its
		// store write failed) is local in the store too: the stage records it
		// (store.DB.KeepBookLocal).
		res.ShareSkipped = r.shareSkipped
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
	// Never without the run's chapters only because their audio is out of sight
	// for a moment: the characters written alone would keep a chapters job off.
	if _, _, err := h.awaitChapters(ctx, r, b, wait); err != nil {
		return sidecars{}, err
	}
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
			// What vouches for the chapters.json left in place when this write has
			// none (no current list, or none to write over the book's files now).
			if s.chapters = chapters; chapters != nil {
				prov.Chapters = r.chaptersProv
			} else {
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
		if book.Info != nil {
			r.info = book.Info
		}
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
				"description", got.Description, "chapters", got.Chapters, "readalong", got.Readalong)
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

// ErrBookChanged is a Retry that met the book parked again, or cancelled,
// between its read and its write (store.ErrStatusChanged): nothing was done.
var ErrBookChanged = errors.New("the book changed meanwhile; try again")

// Retry re-admits a parked job's engine book (the engine's own Retry), or wakes a
// job parked before the engine has its book (the book's folder wasn't writable)
// to check again now. store.ErrNotFound when the job isn't one of this handler's.
func (h *Handler) Retry(ctx context.Context, jobID int64) error {
	h.mu.Lock()
	rh, waiting := h.pokes[jobID]
	h.mu.Unlock()
	c := rh.poke
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
	// The engine re-admits the book only as read here: a Cancel landing since (the
	// job's end) is never taken up as a Retry of the cancelled book (bridge#65).
	if err := eng.RetryAsRead(ctx, b); err != nil {
		switch {
		case errors.Is(err, scheduler.ErrInvalidOp):
			return ErrNotParked
		case errors.Is(err, store.ErrStatusChanged):
			return ErrBookChanged
		}
		return err
	}
	poke()
	return nil
}

// runHandle is a running job's wake-up channel and Reporter (Handler.pokes).
type runHandle struct {
	poke chan struct{}
	rep  *jobs.Reporter
}

// setPoke makes c the job's wake-up channel (its latest run's), with the run's
// Reporter.
func (h *Handler) setPoke(jobID int64, c chan struct{}, rep *jobs.Reporter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pokes[jobID] = runHandle{poke: c, rep: rep}
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
	if h.pokes[jobID].poke == c {
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
