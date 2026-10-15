// Package jobs is the Bridge's generic job runner (BRIDGE-PLAN.md Phase 0). It
// long-polls the server for jobs of the kinds it has room for, runs each with
// the kind's Handler in that kind's lane, reports progress (which renews the
// server's lease and carries an admin's cancel back), and reports the end.
//
// Its own server_jobs rows (internal/store) are the execution truth: a job
// still running when the Bridge starts again is resumed under the same claim,
// or given up when the server says the claim no longer holds it (its lease ran
// out, so the server queued it again), and a report that never reached the
// server is sent again.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Job is a claimed server job as a Handler sees it.
type Job struct {
	ID        int64
	Kind      string
	LibraryID *int64
	Path      *string
	Params    json.RawMessage
	Attempt   int
}

// Handler runs one job kind.
type Handler interface {
	// Kind is the job kind ("bridge.verify"), as the server knows it.
	Kind() string
	// Lane is the lane the kind runs in (LaneLight, ...).
	Lane() string
	// Run does the job. Its ctx ends when an admin cancels it, the server takes
	// it back or the Bridge stops; result may be returned beside an error (a
	// verify that found a mismatch). Return a *ParkError to park the job.
	Run(ctx context.Context, job Job, rep *Reporter) (result any, err error)
}

// Lanes: how many jobs of a kind can run at once. Light jobs are quick checks;
// the ASR and agent lanes arrive with Phase 1.
const (
	LaneLight = "light"
)

// DefaultLanes are the lanes' capacities.
func DefaultLanes() map[string]int { return map[string]int{LaneLight: 2} }

// ParkError parks a job (needs_attention) with a typed code.
type ParkError struct {
	Code    string
	Message string
}

func (e *ParkError) Error() string { return e.Code + ": " + e.Message }

// Server is the part of the server link the runner calls.
type Server interface {
	Claim(ctx context.Context, kinds []string, wait time.Duration) (*link.Job, error)
	UpdateJob(ctx context.Context, id int64, u link.Update) (*link.Job, error)
}

// Store is the part of the store the runner writes.
type Store interface {
	RecordClaim(ctx context.Context, j store.ServerJob) error
	SetServerJobProgress(ctx context.Context, id int64, attempt int, progress json.RawMessage) error
	FinishServerJob(ctx context.Context, id int64, attempt int, state string, result json.RawMessage, errText string) error
	MarkServerJobReported(ctx context.Context, id int64, attempt int) error
	OpenServerJobs(ctx context.Context) ([]store.ServerJob, error)
	GetServerJob(ctx context.Context, id int64) (*store.ServerJob, error)
	PruneServerJobs(ctx context.Context, keep int) (int64, error)
}

// Why a running job's context ended.
var (
	errCancelRequested = errors.New("an admin cancelled the job")
	errLost            = errors.New("the server no longer holds the job for this claim")
	errStopping        = errors.New("the Bridge is stopping")
	errUnpaired        = errors.New("the server refused the Bridge's token")
)

// Options tunes a Runner; zero values take the defaults.
type Options struct {
	Lanes map[string]int
	// Keepalive is how often a running job reports even with no new progress,
	// renewing the server's 2-minute lease and picking up a cancel.
	Keepalive time.Duration
	// ProgressEvery is the shortest gap between two progress reports of a job
	// (a new step is sent at once).
	ProgressEvery time.Duration
	// Wait is the claim's long-poll wait.
	Wait time.Duration
	// RetryEvery is how often a report that didn't reach the server is retried.
	RetryEvery time.Duration
	// Keep is how many finished jobs the store keeps.
	Keep int
	Log  *slog.Logger
}

// Runner claims and runs jobs.
type Runner struct {
	opts     Options
	store    Store
	handlers map[string]Handler
	// server returns the current server link, nil while the Bridge is unpaired.
	server func() Server
	// onUnauthorized is called with the link whose token the server refused (a
	// 401), which may be one a re-pair has since replaced.
	onUnauthorized func(Server)
	log            *slog.Logger

	mu      sync.Mutex
	running map[int64]*run
	use     map[string]int
	// stranded are the jobs a refused token stopped (errUnpaired): the claim
	// loop resumes them under their claim once a link to their server works
	// again.
	stranded []strandedJob
	// changed is closed (and replaced) when a lane frees up or the link
	// changes, waking the claim loop.
	changed chan struct{}
	// retry is set when a final report didn't reach the server (or a start
	// found unreported ones), so retryReports looks again.
	retry atomic.Bool
}

// New returns a Runner over the handlers. server returns the server link (nil
// while unpaired); onUnauthorized is called with the link whose token the
// server refused.
func New(st Store, server func() Server, onUnauthorized func(Server), opts Options, handlers ...Handler) *Runner {
	if opts.Lanes == nil {
		opts.Lanes = DefaultLanes()
	}
	if opts.Keepalive <= 0 {
		opts.Keepalive = 10 * time.Second
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 2 * time.Second
	}
	if opts.Wait <= 0 {
		opts.Wait = link.MaxWait
	}
	if opts.RetryEvery <= 0 {
		opts.RetryEvery = 30 * time.Second
	}
	if opts.Keep <= 0 {
		opts.Keep = 500
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if onUnauthorized == nil {
		onUnauthorized = func(Server) {}
	}
	r := &Runner{opts: opts, store: st, handlers: map[string]Handler{}, server: server, onUnauthorized: onUnauthorized,
		log: opts.Log, running: map[int64]*run{}, use: map[string]int{}, changed: make(chan struct{})}
	for _, h := range handlers {
		r.handlers[h.Kind()] = h
	}
	return r
}

// Kinds are the job kinds the Bridge runs, sorted (the heartbeat's kinds).
func (r *Runner) Kinds() []string {
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lane is one lane's capacity and use.
type Lane struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
	Running  int    `json:"running"`
}

// Lanes reports every lane's use, sorted by name.
func (r *Runner) Lanes() []Lane {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Lane, 0, len(r.opts.Lanes))
	for name, c := range r.opts.Lanes {
		out = append(out, Lane{Name: name, Capacity: c, Running: r.use[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Wake makes the claim loop look again now (the link was paired, or the first
// heartbeat went through).
func (r *Runner) Wake() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wakeLocked()
}

func (r *Runner) wakeLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// freeKinds are the kinds whose lane has room, with the channel the next
// change closes.
func (r *Runner) freeKinds() ([]string, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for kind, h := range r.handlers {
		if r.use[h.Lane()] < r.opts.Lanes[h.Lane()] {
			out = append(out, kind)
		}
	}
	sort.Strings(out)
	return out, r.changed
}

// Run claims and runs jobs until ctx ends. It first picks up the jobs a
// previous run left open, once the server link is up. On return every job it
// started has stopped.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(errStopping)
	wg.Add(1)
	go func() { defer wg.Done(); r.retryReports(ctx) }()
	backoff := time.Second
	recovered := false
	for ctx.Err() == nil {
		srv := r.server()
		kinds, changed := r.freeKinds()
		if srv == nil || len(kinds) == 0 {
			select {
			case <-changed:
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
			continue
		}
		if !recovered {
			recovered = r.recover(ctx, srv, &wg)
			continue
		}
		r.resumeStranded(ctx, srv, &wg)
		start := time.Now()
		job, err := srv.Claim(ctx, kinds, r.opts.Wait)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, link.ErrUnauthorized):
			r.onUnauthorized(srv)
			continue
		case errors.Is(err, link.ErrNoHeartbeat):
			sleep(ctx, 2*time.Second)
			continue
		case err != nil:
			r.log.Warn("bridge: claiming a job failed", "err", err, "retry_in", backoff)
			sleep(ctx, backoff)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		if job == nil {
			// An answer much quicker than the wait (nothing to wait for): don't spin.
			if time.Since(start) < time.Second {
				sleep(ctx, time.Second)
			}
			continue
		}
		r.start(ctx, srv, jobOf(job), &wg, false)
	}
}

// recover picks up what the last run left: jobs still running are resumed
// under their claim (a kind this Bridge no longer runs is failed), finished
// jobs wait for retryReports. It reports whether it could read them.
func (r *Runner) recover(ctx context.Context, srv Server, wg *sync.WaitGroup) bool {
	open, err := r.store.OpenServerJobs(ctx)
	if err != nil {
		r.log.Warn("bridge: reading the open jobs failed", "err", err)
		sleep(ctx, 5*time.Second)
		return false
	}
	for _, sj := range open {
		if sj.State != store.JobRunning {
			r.retry.Store(true)
			continue
		}
		job := storedJob(sj)
		if _, ok := r.handlers[job.Kind]; !ok {
			msg := "the Bridge restarted and no longer runs " + job.Kind + " jobs"
			if err := r.store.FinishServerJob(ctx, job.ID, job.Attempt, store.JobFailed, nil, msg); err != nil {
				r.log.Warn("bridge: recording a job failed", "job", job.ID, "err", err)
			}
			r.retry.Store(true) // retryReports sends it
			continue
		}
		r.log.Info("bridge: resuming a job after a restart", "job", job.ID, "kind", job.Kind, "attempt", job.Attempt)
		r.start(ctx, srv, job, wg, true)
	}
	return true
}

// resumeStranded resumes, under their claim, the jobs a refused token stopped,
// now that a link works again (the refusal ended, or the Bridge was paired
// again), as a restart would: when the server took one back meanwhile, its
// first report is a 409 and it is lost. One a later claim already runs here is
// left to that claim.
func (r *Runner) resumeStranded(ctx context.Context, srv Server, wg *sync.WaitGroup) {
	r.mu.Lock()
	stranded := r.stranded
	r.stranded = nil
	r.mu.Unlock()
	for _, s := range stranded {
		job := s.job
		r.mu.Lock()
		_, running := r.running[job.ID]
		r.mu.Unlock()
		sj, err := r.store.GetServerJob(ctx, job.ID)
		if running || err != nil || sj.State != store.JobRunning || sj.Attempt != job.Attempt {
			continue // a later claim took its row, or it ended
		}
		if !sameServer(s.srv, srv) {
			// Paired with another server since: its job ids are another
			// server's, and the claim can't be reported anywhere now.
			if err := r.store.FinishServerJob(ctx, job.ID, job.Attempt, store.JobLost, nil, "the Bridge was paired with another server"); err != nil {
				r.log.Warn("bridge: recording a job failed", "job", job.ID, "err", err)
			}
			continue
		}
		r.log.Info("bridge: resuming a job its refused token stopped", "job", job.ID, "kind", job.Kind, "attempt", job.Attempt)
		r.start(ctx, srv, job, wg, true)
	}
}

// sameServer reports whether two links reach one server: the same link, or
// links to one address (a new pairing with the same server makes a new link).
func sameServer(a, b Server) bool {
	if a == b {
		return true
	}
	ab, aok := a.(interface{ BaseURL() string })
	bb, bok := b.(interface{ BaseURL() string })
	return aok && bok && ab.BaseURL() == bb.BaseURL()
}

// jobOf is a claimed job as a Handler sees it.
func jobOf(j *link.Job) Job {
	return Job{ID: j.ID, Kind: j.Kind, LibraryID: j.LibraryID, Path: j.Path, Params: j.Params, Attempt: j.Attempts}
}

// storedJob is a job the store recorded, as a Handler sees it.
func storedJob(sj store.ServerJob) Job {
	return Job{ID: sj.ID, Kind: sj.Kind, LibraryID: sj.LibraryID, Path: sj.Path, Params: sj.Params, Attempt: sj.Attempt}
}

// run is one running job, and the link it runs on.
type run struct {
	job    Job
	srv    Server
	lane   string
	cancel context.CancelCauseFunc
}

// strandedJob is a job a refused token stopped, with the link it ran on.
type strandedJob struct {
	job Job
	srv Server
}

// start runs a job in its lane (resumed: picked up after a restart, already
// recorded). A job of a kind the Bridge doesn't run is failed at once.
func (r *Runner) start(ctx context.Context, srv Server, job Job, wg *sync.WaitGroup, resumed bool) {
	h, ok := r.handlers[job.Kind]
	if !resumed {
		if err := r.store.RecordClaim(ctx, store.ServerJob{ID: job.ID, Kind: job.Kind, LibraryID: job.LibraryID, Path: job.Path,
			Params: job.Params, Attempt: job.Attempt}); err != nil {
			r.log.Warn("bridge: recording a claim failed", "job", job.ID, "err", err)
		}
	}
	if !ok {
		r.finish(ctx, srv, job, store.JobFailed, nil, nil, "this Bridge doesn't run "+job.Kind+" jobs")
		return
	}
	rctx, cancel := context.WithCancelCause(ctx)
	ru := &run{job: job, srv: srv, lane: h.Lane(), cancel: cancel}
	r.mu.Lock()
	r.running[job.ID] = ru
	r.use[ru.lane]++
	r.mu.Unlock()
	wg.Add(1)
	go func() {
		defer wg.Done()
		stranded := false
		defer func() { r.release(ru, stranded) }()
		stranded = r.execute(rctx, ctx, srv, h, ru)
	}()
}

// release frees a job's lane slot, keeps a job a refused token stopped for
// resumeStranded, and wakes the claim loop.
func (r *Runner) release(ru *run, stranded bool) {
	ru.cancel(nil)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[ru.job.ID] == ru { // a later claim of the same job may run under its id
		delete(r.running, ru.job.ID)
	}
	r.use[ru.lane]--
	if stranded {
		r.stranded = append(r.stranded, strandedJob{job: ru.job, srv: ru.srv})
	}
	r.wakeLocked()
}

// execute runs the handler, keeps the lease alive beside it, and records and
// reports how it ended. base is the runner's context (for the final report,
// which must go out after the job's own context ended). It reports whether a
// refused token stopped the job (left running, to resume under its claim).
func (r *Runner) execute(ctx, base context.Context, srv Server, h Handler, ru *run) (stranded bool) {
	rep := &Reporter{r: r, srv: srv, job: ru.job, cancel: ru.cancel, ctx: ctx, nudge: make(chan struct{}, 1)}
	if !rep.send() {
		return r.settle(base, srv, ru, rep, nil, context.Cause(ctx))
	}
	done := make(chan struct{})
	reporting := make(chan struct{})
	go func() {
		defer close(reporting)
		rep.loop(done)
	}()
	result, err := h.Run(ctx, ru.job, rep)
	close(done)
	<-reporting
	return r.settle(base, srv, ru, rep, result, firstErr(context.Cause(ctx), err))
}

// firstErr prefers why the job's context ended over the handler's error (a
// handler stopped by a cancel usually returns ctx.Err()).
func firstErr(cause, err error) error {
	if cause != nil {
		return cause
	}
	return err
}

// settle records and reports a job's end. It reports whether a refused token
// stopped the job (left running, for resumeStranded).
func (r *Runner) settle(base context.Context, srv Server, ru *run, rep *Reporter, result any, err error) (stranded bool) {
	job := ru.job
	var raw json.RawMessage
	if result != nil {
		b, merr := json.Marshal(result)
		switch {
		case merr != nil:
			r.log.Warn("bridge: a job's result isn't JSON", "job", job.ID, "err", merr)
		case string(b) == "null":
		case len(b) > maxResultBytes || b[0] != '{':
			// The server takes a JSON object of at most 64 KiB and refuses any
			// other report for good, which would be retried until the lease
			// ran out and the job ran again: report none, and say why.
			r.log.Warn("bridge: a job's result can't be reported", "job", job.ID, "bytes", len(b))
			if err == nil {
				err = errUnreportable
			}
		default:
			raw = b
		}
	}
	// The last progress goes with the final report (one still waiting to be sent
	// never was).
	var progress json.RawMessage
	if last := rep.Last(); last.Step != "" {
		progress, _ = json.Marshal(last)
	}
	var park *ParkError
	switch {
	case base.Err() != nil, errors.Is(err, errStopping):
		// Left running: the next start resumes it under its claim.
		r.log.Info("bridge: a job was interrupted; it resumes at the next start", "job", job.ID, "why", err)
	case errors.Is(err, errUnpaired):
		// Left running: resumed under its claim once a link works again (or at
		// the next start).
		r.log.Info("bridge: the server refused a job's token; it resumes once the link works again", "job", job.ID)
		return true
	case errors.Is(err, errLost):
		r.log.Info("bridge: the server took a job back", "job", job.ID, "kind", job.Kind)
		if ferr := r.store.FinishServerJob(base, job.ID, job.Attempt, store.JobLost, raw, err.Error()); ferr != nil {
			r.log.Warn("bridge: recording a job failed", "job", job.ID, "err", ferr)
		}
	case errors.Is(err, errCancelRequested):
		r.finish(base, srv, job, store.JobCancelled, progress, raw, "")
	case err == nil:
		r.finish(base, srv, job, store.JobDone, progress, raw, "")
	case errors.As(err, &park):
		r.finish(base, srv, job, store.JobNeedsAttention, progress, raw, park.Error())
	default:
		r.finish(base, srv, job, store.JobFailed, progress, raw, err.Error())
	}
	return false
}

// Bounds on what the Bridge reports, inside the server's (its error limit is
// 2 KiB, a progress object's 16 KiB and a result object's 64 KiB): a report it
// refuses is refused for good.
const (
	maxErrorText   = 1024
	maxStepText    = 128
	maxMessageText = 1024
	maxResultBytes = 64 << 10
)

// errUnreportable fails a job whose result the server would refuse.
var errUnreportable = errors.New("the job's result can't be reported: it must be a JSON object of at most 64 KiB")

// finish records a job's end and reports it; retryReports resends a report
// that didn't get through.
func (r *Runner) finish(ctx context.Context, srv Server, job Job, state string, progress, result json.RawMessage, errText string) {
	errText = clip(errText, maxErrorText)
	if progress != nil {
		if err := r.store.SetServerJobProgress(ctx, job.ID, job.Attempt, progress); err != nil {
			r.log.Warn("bridge: recording progress failed", "job", job.ID, "err", err)
		}
	}
	if err := r.store.FinishServerJob(ctx, job.ID, job.Attempt, state, result, errText); err != nil {
		r.log.Warn("bridge: recording a job failed", "job", job.ID, "err", err)
	}
	r.log.Info("bridge: job finished", "job", job.ID, "kind", job.Kind, "state", state, "error", errText)
	r.report(ctx, srv, store.ServerJob{ID: job.ID, Attempt: job.Attempt, State: state, Progress: progress, Result: result, Error: errText})
}

// report sends a finished job's end to the server, and marks it reported once
// the server took it (or no longer holds the job for this claim).
func (r *Runner) report(ctx context.Context, srv Server, sj store.ServerJob) {
	state := sj.State
	u := link.Update{Attempt: sj.Attempt, State: &state}
	if len(sj.Progress) > 0 && string(sj.Progress) != "{}" {
		u.Progress = sj.Progress
	}
	if len(sj.Result) > 0 && string(sj.Result) != "{}" {
		u.Result = sj.Result
	}
	if sj.Error != "" {
		e := sj.Error
		u.Error = &e
	}
	_, err := srv.UpdateJob(ctx, sj.ID, u)
	switch {
	case err == nil, errors.Is(err, link.ErrNotHeld):
		if errors.Is(err, link.ErrNotHeld) {
			r.log.Info("bridge: the server had moved on from a job; its report is dropped", "job", sj.ID)
		}
		if merr := r.store.MarkServerJobReported(ctx, sj.ID, sj.Attempt); merr != nil {
			r.log.Warn("bridge: recording a report failed", "job", sj.ID, "err", merr)
		}
	case errors.Is(err, link.ErrUnauthorized):
		r.retry.Store(true)
		r.onUnauthorized(srv)
	default:
		r.retry.Store(true)
		r.log.Warn("bridge: reporting a job failed; retrying", "job", sj.ID, "err", err)
	}
}

// pruneEvery is how often old finished jobs are pruned.
const pruneEvery = time.Hour

// retryReports resends, every RetryEvery while one is owed, the reports that
// didn't reach the server, and prunes old finished jobs hourly.
func (r *Runner) retryReports(ctx context.Context) {
	t := time.NewTicker(r.opts.RetryEvery)
	defer t.Stop()
	var pruned time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Since(pruned) >= pruneEvery {
			pruned = time.Now()
			if _, err := r.store.PruneServerJobs(ctx, r.opts.Keep); err != nil {
				r.log.Warn("bridge: pruning old jobs failed", "err", err)
			}
		}
		srv := r.server()
		if srv == nil || !r.retry.Swap(false) {
			continue
		}
		open, err := r.store.OpenServerJobs(ctx)
		if err != nil {
			r.retry.Store(true)
			continue
		}
		for _, sj := range open {
			if sj.State != store.JobRunning && !sj.Reported {
				r.report(ctx, srv, sj)
			}
		}
	}
}

// Cancel stops a running job the Bridge's own page cancelled, as an admin's
// cancel would (reported as cancelled). false when it isn't running here.
func (r *Runner) Cancel(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ru, ok := r.running[id]
	if ok {
		ru.cancel(errCancelRequested)
	}
	return ok
}

// Progress is a job's progress as the server shows it: a step, a sentence, and
// a count where there is one.
type Progress struct {
	Step    string `json:"step"`
	Message string `json:"message,omitempty"`
	Done    int    `json:"done,omitempty"`
	Total   int    `json:"total,omitempty"`
}

// Reporter is how a Handler reports progress. It is safe for concurrent use.
// Progress only records: the job's reporting goroutine (loop) sends, so a
// handler never waits on the network.
type Reporter struct {
	r      *Runner
	srv    Server
	job    Job
	cancel context.CancelCauseFunc
	ctx    context.Context //nolint:containedctx // the job's own context, for its sends
	// nudge wakes loop when the step changed (nil for a LocalReporter).
	nudge chan struct{}

	mu       sync.Mutex
	progress Progress
	// dirty: progress changed since it was last sent (and stored).
	dirty  bool
	sentAt time.Time
}

// LocalReporter returns a Reporter that records progress and sends nothing (a
// handler's tests).
func LocalReporter() *Reporter { return &Reporter{} }

// Last is the progress last recorded.
func (p *Reporter) Last() Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.progress
}

// Progress records the job's progress; a new step is sent at once, anything
// else within ProgressEvery.
func (p *Reporter) Progress(pr Progress) {
	pr.Step, pr.Message = clip(pr.Step, maxStepText), clip(pr.Message, maxMessageText)
	p.mu.Lock()
	newStep := pr.Step != p.progress.Step
	p.progress, p.dirty = pr, true
	p.mu.Unlock()
	if newStep && p.nudge != nil {
		select {
		case p.nudge <- struct{}{}:
		default:
		}
	}
}

// loop sends the job's progress until done closes or the job's context ends: a
// new step at once, other changes at most every ProgressEvery, and a keepalive
// every Keepalive when nothing changed (renewing the lease).
func (p *Reporter) loop(done <-chan struct{}) {
	t := time.NewTicker(p.r.opts.ProgressEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-p.ctx.Done():
			return
		case <-p.nudge:
			// A step the previous send already carried (it was recorded while
			// that send was in flight) needs no second report.
			p.mu.Lock()
			dirty := p.dirty
			p.mu.Unlock()
			if !dirty {
				continue
			}
		case <-t.C:
			p.mu.Lock()
			since, dirty := time.Since(p.sentAt), p.dirty
			p.mu.Unlock()
			changed := dirty && since >= p.r.opts.ProgressEvery
			if !changed && since < p.r.opts.Keepalive {
				continue
			}
		}
		if !p.send() {
			return
		}
	}
}

// send reports the job as running with its current progress (renewing the
// lease) and acts on the answer: a cancel request or a lost claim ends the
// job's context. It reports whether the job goes on.
func (p *Reporter) send() bool {
	ctx := p.ctx
	p.mu.Lock()
	pr, dirty := p.progress, p.dirty
	p.sentAt, p.dirty = time.Now(), false
	p.mu.Unlock()
	raw, _ := json.Marshal(pr)
	if dirty {
		if err := p.r.store.SetServerJobProgress(ctx, p.job.ID, p.job.Attempt, raw); err != nil && ctx.Err() == nil {
			p.r.log.Warn("bridge: recording progress failed", "job", p.job.ID, "err", err)
		}
	}
	state := link.StateRunning
	u := link.Update{Attempt: p.job.Attempt, State: &state}
	if pr.Step != "" {
		u.Progress = raw
	}
	got, err := p.srv.UpdateJob(ctx, p.job.ID, u)
	switch {
	case err == nil && got.CancelRequested:
		p.cancel(errCancelRequested)
		return false
	case err == nil:
		return true
	case errors.Is(err, link.ErrNotHeld):
		p.cancel(errLost)
		return false
	case errors.Is(err, link.ErrUnauthorized):
		p.cancel(errUnpaired)
		p.r.onUnauthorized(p.srv)
		return false
	case ctx.Err() != nil:
		return false
	default:
		// The server is unreachable for now: the job goes on, and its lease
		// lasts two minutes from the last report that got through.
		p.r.log.Warn("bridge: reporting progress failed", "job", p.job.ID, "err", err)
		return true
	}
}

// clip bounds s to n bytes, cut at a character's boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
