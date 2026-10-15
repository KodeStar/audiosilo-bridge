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
	"fmt"
	"log/slog"
	"sort"
	"sync"
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
	// onUnauthorized is called when the server refuses the token.
	onUnauthorized func()
	log            *slog.Logger

	mu      sync.Mutex
	running map[int64]*run
	use     map[string]int
	// changed is closed (and replaced) when a lane frees up or the link
	// changes, waking the claim loop.
	changed chan struct{}
	// recovered is set once the open jobs of the last run were picked up.
	recovered bool
}

// New returns a Runner over the handlers. server returns the server link (nil
// while unpaired); onUnauthorized is called when the server refuses the token.
func New(st Store, server func() Server, onUnauthorized func(), opts Options, handlers ...Handler) *Runner {
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
		onUnauthorized = func() {}
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
		if !r.recoveredYet() {
			r.recover(ctx, srv, &wg)
			continue
		}
		start := time.Now()
		job, err := srv.Claim(ctx, kinds, r.opts.Wait)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, link.ErrUnauthorized):
			r.onUnauthorized()
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

func (r *Runner) recoveredYet() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recovered
}

// recover picks up what the last run left: jobs still running are resumed
// under their claim (a kind this Bridge no longer runs is failed), finished
// jobs wait for retryReports.
func (r *Runner) recover(ctx context.Context, srv Server, wg *sync.WaitGroup) {
	open, err := r.store.OpenServerJobs(ctx)
	if err != nil {
		r.log.Warn("bridge: reading the open jobs failed", "err", err)
		sleep(ctx, 5*time.Second)
		return
	}
	r.mu.Lock()
	r.recovered = true
	r.mu.Unlock()
	for _, sj := range open {
		if sj.State != store.JobRunning {
			continue
		}
		job := Job{ID: sj.ID, Kind: sj.Kind, LibraryID: sj.LibraryID, Path: sj.Path, Params: sj.Params, Attempt: sj.Attempt}
		if _, ok := r.handlers[job.Kind]; !ok {
			msg := "the Bridge restarted and no longer runs " + job.Kind + " jobs"
			if err := r.store.FinishServerJob(ctx, job.ID, job.Attempt, store.JobFailed, nil, msg); err != nil {
				r.log.Warn("bridge: recording a job failed", "job", job.ID, "err", err)
			}
			continue
		}
		r.log.Info("bridge: resuming a job after a restart", "job", job.ID, "kind", job.Kind, "attempt", job.Attempt)
		r.start(ctx, srv, job, wg, true)
	}
}

func jobOf(j *link.Job) Job {
	return Job{ID: j.ID, Kind: j.Kind, LibraryID: j.LibraryID, Path: j.Path, Params: j.Params, Attempt: j.Attempts}
}

// run is one running job.
type run struct {
	job    Job
	lane   string
	cancel context.CancelCauseFunc
}

// start runs a job in its lane (resumed: picked up after a restart, already
// recorded). A job of a kind the Bridge doesn't run is failed at once.
func (r *Runner) start(ctx context.Context, srv Server, job Job, wg *sync.WaitGroup, resumed bool) {
	h, ok := r.handlers[job.Kind]
	if !resumed {
		lib, path := job.LibraryID, job.Path
		if err := r.store.RecordClaim(ctx, store.ServerJob{ID: job.ID, Kind: job.Kind, LibraryID: lib, Path: path,
			Params: job.Params, Attempt: job.Attempt}); err != nil {
			r.log.Warn("bridge: recording a claim failed", "job", job.ID, "err", err)
		}
	}
	if !ok {
		r.finish(ctx, srv, job, store.JobFailed, nil, nil, "this Bridge doesn't run "+job.Kind+" jobs")
		return
	}
	rctx, cancel := context.WithCancelCause(ctx)
	ru := &run{job: job, lane: h.Lane(), cancel: cancel}
	r.mu.Lock()
	r.running[job.ID] = ru
	r.use[ru.lane]++
	r.mu.Unlock()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer r.release(ru)
		r.execute(rctx, ctx, srv, h, ru)
	}()
}

// release frees a job's lane slot and wakes the claim loop.
func (r *Runner) release(ru *run) {
	ru.cancel(nil)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, ru.job.ID)
	r.use[ru.lane]--
	r.wakeLocked()
}

// execute runs the handler, keeps the lease alive beside it, and records and
// reports how it ended. base is the runner's context (for the final report,
// which must go out after the job's own context ended).
func (r *Runner) execute(ctx, base context.Context, srv Server, h Handler, ru *run) {
	rep := &Reporter{r: r, srv: srv, job: ru.job, cancel: ru.cancel, ctx: ctx}
	if !rep.send(ctx, true) {
		r.settle(base, srv, ru, rep, nil, context.Cause(ctx))
		return
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(r.opts.Keepalive)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				rep.send(ctx, true)
			}
		}
	}()
	result, err := h.Run(ctx, ru.job, rep)
	close(done)
	r.settle(base, srv, ru, rep, result, firstErr(context.Cause(ctx), err))
}

// firstErr prefers why the job's context ended over the handler's error (a
// handler stopped by a cancel usually returns ctx.Err()).
func firstErr(cause, err error) error {
	if cause != nil {
		return cause
	}
	return err
}

// settle records and reports a job's end.
func (r *Runner) settle(base context.Context, srv Server, ru *run, rep *Reporter, result any, err error) {
	job := ru.job
	var raw json.RawMessage
	if result != nil {
		b, merr := json.Marshal(result)
		if merr != nil {
			r.log.Warn("bridge: a job's result isn't JSON", "job", job.ID, "err", merr)
		} else {
			raw = b
		}
	}
	// The last progress goes with the final report (a throttled one may never
	// have been sent).
	var progress json.RawMessage
	if last := rep.Last(); last.Step != "" {
		progress, _ = json.Marshal(last)
	}
	var park *ParkError
	switch {
	case base.Err() != nil, errors.Is(err, errStopping), errors.Is(err, errUnpaired):
		// Left running: the next start (or pairing) resumes it under its claim.
		r.log.Info("bridge: a job was interrupted; it resumes at the next start", "job", job.ID, "why", err)
		return
	case errors.Is(err, errLost):
		r.log.Info("bridge: the server took a job back", "job", job.ID, "kind", job.Kind)
		if ferr := r.store.FinishServerJob(base, job.ID, job.Attempt, store.JobLost, raw, err.Error()); ferr != nil {
			r.log.Warn("bridge: recording a job failed", "job", job.ID, "err", ferr)
		}
		return
	case errors.Is(err, errCancelRequested):
		r.finish(base, srv, job, store.JobCancelled, progress, raw, "")
	case err == nil:
		r.finish(base, srv, job, store.JobDone, progress, raw, "")
	case errors.As(err, &park):
		r.finish(base, srv, job, store.JobNeedsAttention, progress, raw, park.Error())
	default:
		r.finish(base, srv, job, store.JobFailed, progress, raw, err.Error())
	}
}

// maxErrorText bounds an error sent to the server (its limit is 2 KiB).
const maxErrorText = 1024

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
		r.onUnauthorized()
	default:
		r.log.Warn("bridge: reporting a job failed; retrying", "job", sj.ID, "err", err)
	}
}

// retryReports resends, every RetryEvery, the reports that didn't reach the
// server, and prunes old finished jobs.
func (r *Runner) retryReports(ctx context.Context) {
	t := time.NewTicker(r.opts.RetryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		srv := r.server()
		if srv == nil {
			continue
		}
		open, err := r.store.OpenServerJobs(ctx)
		if err != nil {
			continue
		}
		for _, sj := range open {
			if sj.State != store.JobRunning && !sj.Reported {
				r.report(ctx, srv, sj)
			}
		}
		if _, err := r.store.PruneServerJobs(ctx, r.opts.Keep); err != nil {
			r.log.Warn("bridge: pruning old jobs failed", "err", err)
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
type Reporter struct {
	r      *Runner
	srv    Server
	job    Job
	cancel context.CancelCauseFunc
	ctx    context.Context //nolint:containedctx // the job's own context, for its keepalive sends

	mu       sync.Mutex
	progress Progress
	sentAt   time.Time
	sentStep string
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

// Progress records the job's progress and sends it when its step changed or
// ProgressEvery passed since the last report.
func (p *Reporter) Progress(pr Progress) {
	p.mu.Lock()
	p.progress = pr
	if p.r == nil {
		p.mu.Unlock()
		return
	}
	due := pr.Step != p.sentStep || time.Since(p.sentAt) >= p.r.opts.ProgressEvery
	p.mu.Unlock()
	if due {
		p.send(p.ctx, false)
	}
}

// send reports the job as running with its current progress (renewing the
// lease) and acts on the answer: a cancel request or a lost claim ends the
// job's context. force sends even when nothing changed (the keepalive). It
// reports whether the job goes on.
func (p *Reporter) send(ctx context.Context, force bool) bool {
	p.mu.Lock()
	pr := p.progress
	if !force && pr.Step == p.sentStep && time.Since(p.sentAt) < p.r.opts.ProgressEvery {
		p.mu.Unlock()
		return true
	}
	p.sentAt, p.sentStep = time.Now(), pr.Step
	p.mu.Unlock()
	raw, _ := json.Marshal(pr)
	if err := p.r.store.SetServerJobProgress(ctx, p.job.ID, p.job.Attempt, raw); err != nil && ctx.Err() == nil {
		p.r.log.Warn("bridge: recording progress failed", "job", p.job.ID, "err", err)
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
		p.r.onUnauthorized()
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

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// String names a job in logs.
func (j Job) String() string { return fmt.Sprintf("%s#%d", j.Kind, j.ID) }
