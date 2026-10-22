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
	"maps"
	"reflect"
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
	// Resumed: the runner resumed the job under its claim (after a restart, or
	// once a refused token works again) instead of claiming it now, so an earlier
	// run of it may have spent already.
	Resumed bool
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

// InteractiveHandler opts a kind into the reserved pool. Ordinary workers can
// still run any priority of this kind in its normal lane.
type InteractiveHandler interface {
	Interactive() bool
}

// ParamSealer removes sensitive claim params before any execution row is
// written. A handler resumes from these stored params and its sealed secrets.
// A failure fails the job without running it: a *SealFailure is reported as it
// is, any other error only as a generic failure (its text may hold the secret).
type ParamSealer interface {
	SealParams(job Job) (stored json.RawMessage, err error)
}

// SealFailure is a ParamSealer's failure that is safe to report: it becomes
// the failed job's result ({"code","message"}) and its message the error.
type SealFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *SealFailure) Error() string { return e.Message }

// Lanes: how many jobs of a kind can run at once. Light jobs are quick checks.
// The pipeline lane holds the jobs that follow an engine book (sidecars.generate):
// the engine's own scheduler does the throttling, so a job waits there while its
// book is queued or parked, and the lane only bounds how many are followed.
// The interactive pool is carved from the download lane: a legacy pairing folds
// it back into download, and the two share one combined bound (roomLocked).
const (
	LaneLight       = "light"
	LaneDownload    = "download"
	LanePipeline    = "pipeline"
	LaneInteractive = "interactive"
)

const InteractivePriority int32 = 100

// DefaultLanes are the lanes' capacities.
func DefaultLanes() map[string]int {
	return map[string]int{LaneLight: 2, LaneDownload: 1, LaneInteractive: 1, LanePipeline: 25}
}

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
	SetServerJobReport(ctx context.Context, id int64, attempt int, progress json.RawMessage, parkMsg string) error
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

// Abandoner is a Handler whose jobs hold work beyond Run (sidecars.generate: an
// engine book). The runner calls Abandon when it ends such a job without running
// the handler (the first report of a resumed job answered with a cancel or a lost
// claim, or another server's job dropped), so the work is released as the
// handler's own end would.
type Abandoner interface {
	Abandon(ctx context.Context, job Job)
}

// abandon tells the job's handler, when it is an Abandoner, that the job ended
// without it.
func (r *Runner) abandon(ctx context.Context, job Job) {
	if a, ok := r.handlers[job.Kind].(Abandoner); ok {
		a.Abandon(ctx, job)
	}
}

// Resumes reports whether a job whose context ended will run again under its
// claim: the Bridge is stopping (the next start resumes it) or the server refused
// the token (resumed once a link works again). False when an admin cancelled it
// or the server took it back, which end it here for good.
func Resumes(ctx context.Context) bool {
	cause := context.Cause(ctx)
	return !errors.Is(cause, errCancelRequested) && !errors.Is(cause, errLost)
}

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
	// ServerID is the paired server's id ("" when unknown), recorded on every
	// claim: a start treats an open job of another server as lost.
	ServerID func() string
	Log      *slog.Logger
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

	mu       sync.Mutex
	running  map[int64]*run
	use      map[string]int
	reserved map[string]int
	waiting  map[string]int
	// stranded are the jobs a refused token stopped (errUnpaired): the claim
	// loop resumes them under their claim once a link to their server works
	// again.
	stranded []strandedJob
	// changed is closed and replaced when admission or pairing may have
	// changed. Dispatchers compare eligibility before refreshing a claim.
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
	opts.Lanes = maps.Clone(opts.Lanes)
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
	if opts.ServerID == nil {
		opts.ServerID = func() string { return "" }
	}
	if onUnauthorized == nil {
		onUnauthorized = func(Server) {}
	}
	r := &Runner{opts: opts, store: st, handlers: map[string]Handler{}, server: server, onUnauthorized: onUnauthorized,
		log: opts.Log, running: map[int64]*run{}, use: map[string]int{}, reserved: map[string]int{}, waiting: map[string]int{}, changed: make(chan struct{})}
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
	srv := r.server()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Lane, 0, len(r.opts.Lanes))
	for name, c := range r.capacities(srv) {
		out = append(out, Lane{Name: name, Capacity: c, Running: r.use[name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Wake asks dispatchers to compare eligibility after a pairing or heartbeat.
func (r *Runner) Wake() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wakeLocked()
}

// wakeChan is the channel the next Wake closes.
func (r *Runner) wakeChan() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

func (r *Runner) wakeLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
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
	for ctx.Err() == nil {
		changed := r.wakeChan()
		srv := r.server()
		if srv == nil {
			select {
			case <-changed:
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
			continue
		}
		if r.recover(ctx, srv, &wg) {
			break
		}
	}
	wg.Add(2)
	go func() { defer wg.Done(); r.dispatch(ctx, false, &wg) }()
	go func() { defer wg.Done(); r.dispatch(ctx, true, &wg) }()
	for ctx.Err() == nil {
		changed := r.wakeChan()
		if srv := r.server(); srv != nil {
			r.resumeStranded(ctx, srv, &wg)
		}
		select {
		case <-ctx.Done():
		case <-changed:
		}
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
		if r.otherServer(sj) {
			// Another server's job (the Bridge was paired with this one since):
			// its id means nothing here, so nothing is reported for it.
			r.dropOtherServers(ctx, sj)
			continue
		}
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
		job.Resumed = true
		r.launch(ctx, srv, job, wg, sj.Error, nil)
	}
	return true
}

// dropOtherServers ends what a start found open for another server: a running
// job is lost, an unreported end is never sent.
func (r *Runner) dropOtherServers(ctx context.Context, sj store.ServerJob) {
	r.log.Info("bridge: a job of another server is dropped", "job", sj.ID, "server", sj.ServerID)
	var err error
	if sj.State == store.JobRunning {
		err = r.store.FinishServerJob(ctx, sj.ID, sj.Attempt, store.JobLost, nil, "the Bridge was paired with another server")
		r.abandon(ctx, storedJob(sj))
	} else {
		err = r.store.MarkServerJobReported(ctx, sj.ID, sj.Attempt)
	}
	if err != nil {
		r.log.Warn("bridge: recording a job failed", "job", sj.ID, "err", err)
	}
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
		if !sameServer(s.srv, srv) || r.otherServer(*sj) {
			// Paired with another server since: its job ids are another
			// server's, and the claim can't be reported anywhere now.
			if err := r.store.FinishServerJob(ctx, job.ID, job.Attempt, store.JobLost, nil, "the Bridge was paired with another server"); err != nil {
				r.log.Warn("bridge: recording a job failed", "job", job.ID, "err", err)
			}
			r.abandon(ctx, job)
			continue
		}
		r.log.Info("bridge: resuming a job its refused token stopped", "job", job.ID, "kind", job.Kind, "attempt", job.Attempt)
		job.Resumed = true
		r.launch(ctx, srv, job, wg, sj.Error, nil)
	}
}

// otherServer reports whether a recorded job came from a server other than the
// one the Bridge is paired with now (unknown on either side counts as the same).
func (r *Runner) otherServer(sj store.ServerJob) bool {
	cur := r.opts.ServerID()
	return sj.ServerID != "" && cur != "" && sj.ServerID != cur
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

// run is one running job, and the link it runs on. parked is the park message a
// resumed job had (its first report keeps it parked).
type run struct {
	job      Job
	srv      Server
	lane     string
	cancel   context.CancelCauseFunc
	parked   string
	admitted bool
	// stray: a claim answered outside its admission (claimRequest.fallback).
	// While it waits, the dispatcher that took it (strayReserve) claims no more.
	stray, strayReserve bool
}

// strandedJob is a job a refused token stopped, with the link it ran on.
type strandedJob struct {
	job Job
	srv Server
}

// start records a claimed job and runs it. claim is the dispatcher's request
// that took it (nil in tests that start a job directly).
func (r *Runner) start(ctx context.Context, srv Server, job Job, wg *sync.WaitGroup, claim *claimRequest) {
	serverID := r.opts.ServerID()
	if claim != nil {
		serverID = claim.serverID
	}
	var sealErr error
	sealer, sealing := r.handlers[job.Kind].(ParamSealer)
	if sealing {
		job.Params, sealErr = sealer.SealParams(job)
		if sealErr != nil {
			job.Params = json.RawMessage(`{}`)
		}
	}
	// Recorded even when the runner is stopping: a claim that raced the stop
	// is then resumed under its lease at the next start, not left to lapse.
	if err := r.store.RecordClaim(context.WithoutCancel(ctx), store.ServerJob{ID: job.ID, Kind: job.Kind, LibraryID: job.LibraryID, Path: job.Path,
		Params: job.Params, Attempt: job.Attempt, ServerID: serverID}); err != nil {
		r.log.Warn("bridge: recording a claim failed", "job", job.ID, "err", err)
		if sealing {
			// Fail closed: without its row the sealed secret could be neither
			// resumed nor released later. The server hands the job out again.
			r.abandon(ctx, job)
			return
		}
	}
	if sealErr != nil {
		r.abandon(ctx, job)
		var failure *SealFailure
		if !errors.As(sealErr, &failure) {
			failure = &SealFailure{Code: "write_failed", Message: "The job's private parameters could not be sealed."}
		}
		result, _ := json.Marshal(failure)
		r.finish(ctx, srv, job, store.JobFailed, nil, result, failure.Message)
		return
	}
	r.launch(ctx, srv, job, wg, "", claim)
}

// launch runs a recorded job in its lane, parked with message parked when a
// resumed one was ("" when it wasn't). A job of a kind the Bridge doesn't run is
// failed at once. claim is nil for a recovered or stranded job, which waits for
// ordinary admission; a claimed job takes its reservation unless the server
// ignored the requested filter, in which case it waits for ordinary admission.
func (r *Runner) launch(ctx context.Context, srv Server, job Job, wg *sync.WaitGroup, parked string, claim *claimRequest) {
	h, ok := r.handlers[job.Kind]
	if !ok {
		r.finish(ctx, srv, job, store.JobFailed, nil, nil, "this Bridge doesn't run "+job.Kind+" jobs")
		return
	}
	rctx, cancel := context.WithCancelCause(ctx)
	ru := &run{job: job, srv: srv, lane: h.Lane(), cancel: cancel, parked: parked}
	r.mu.Lock()
	if claim != nil {
		r.unreserveLocked(claim)
		ru.stray, ru.strayReserve = claim.fallback, claim.fallback && claim.interactive
	}
	if claim != nil && !claim.fallback {
		if claim.interactive {
			ru.lane = LaneInteractive
		}
		ru.admitted = true
	} else {
		// Behind any job already waiting in the lane (first come, first admitted).
		ru.admitted = r.waiting[ru.lane] == 0 && r.roomLocked(r.capacities(srv), ru.lane, nil)
	}
	r.running[job.ID] = ru
	if ru.admitted {
		r.use[ru.lane]++
	} else {
		r.waiting[ru.lane]++
	}
	r.wakeLocked()
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
	if ru.admitted {
		r.use[ru.lane]--
	} else {
		r.waiting[ru.lane]--
	}
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
	if ru.parked != "" {
		rep.parked, rep.parkMsg = true, ru.parked
	}
	if !rep.send() {
		// Ended before its handler ran: a cancel or a lost claim ends it for good,
		// so what an earlier run of it left (an engine book) is released.
		cause := context.Cause(ctx)
		if errors.Is(cause, errCancelRequested) || errors.Is(cause, errLost) {
			r.abandon(base, ru.job)
		}
		return r.settle(base, srv, ru, rep, nil, cause)
	}
	done := make(chan struct{})
	reporting := make(chan struct{})
	go func() {
		defer close(reporting)
		rep.loop(done)
	}()
	var result any
	var err error
	if r.awaitAdmission(ctx, ru) {
		result, err = h.Run(ctx, ru.job, rep)
	} else if cause := context.Cause(ctx); errors.Is(cause, errCancelRequested) || errors.Is(cause, errLost) {
		// A recovered job can lose its claim while waiting for a slot, after
		// its first report succeeded but before its handler ever ran.
		r.abandon(base, ru.job)
	}
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
	maxErrorText     = 1024
	maxStepText      = 128
	maxMessageText   = 1024
	maxResultBytes   = 64 << 10
	maxProgressBytes = 16 << 10
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
	// The error goes out even when empty: it clears the message a park left.
	e := sj.Error
	u.Error = &e
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
			// Another server's end is never sent here (recover drops it; it may
			// not have yet when this looks).
			if sj.State != store.JobRunning && !sj.Reported && !r.otherServer(sj) {
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
// a count where there is one. Extra carries a kind's own fields (sidecars.generate's
// stage, eta_seconds, cost_usd, source, park), sent in the same object beside
// them; the four fields above always win over an Extra key of the same name.
type Progress struct {
	Step    string         `json:"step"`
	Message string         `json:"message,omitempty"`
	Done    int            `json:"done,omitempty"`
	Total   int            `json:"total,omitempty"`
	Extra   map[string]any `json:"-"`
}

// MarshalJSON writes the progress as one flat object: Extra's keys beside step,
// message, done and total. An object over the server's 16 KiB is sent without
// Extra, which the server would otherwise refuse for good.
func (p Progress) MarshalJSON() ([]byte, error) {
	type base Progress
	if len(p.Extra) == 0 {
		return json.Marshal(base(p))
	}
	out := make(map[string]any, len(p.Extra)+4)
	for k, v := range p.Extra {
		out[k] = v
	}
	delete(out, "message")
	delete(out, "done")
	delete(out, "total")
	out["step"] = p.Step
	if p.Message != "" {
		out["message"] = p.Message
	}
	if p.Done != 0 {
		out["done"] = p.Done
	}
	if p.Total != 0 {
		out["total"] = p.Total
	}
	b, err := json.Marshal(out)
	if err != nil || len(b) > maxProgressBytes {
		return json.Marshal(base(p))
	}
	return b, nil
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
	// parked: the job reports needs_attention with parkMsg as its error, until
	// Unpark. Unparked, every report clears the error (one a park left, even
	// across a restart that came before the clear got through).
	parked  bool
	parkMsg string
	// path is the job's book path as the server's last answer gave it ("" until
	// one did): the server moves an open job with its book (a rename the scan
	// noticed), and the job hears it here.
	path string
}

// LocalReporter returns a Reporter that records progress and sends nothing (a
// handler's tests).
func LocalReporter() *Reporter { return &Reporter{} }

// Path is the job's book path as the server's last answer to a report gave it,
// "" when no answer carried one: a book the server saw move since the claim is
// at this path now.
func (p *Reporter) Path() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.path
}

// Last is the progress last recorded.
func (p *Reporter) Last() Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.progress
}

// Progress records the job's progress; a new step is sent at once, anything
// else within ProgressEvery. The same progress again (a handler reporting on
// every poll) changes nothing, so it is sent only with the keepalive.
func (p *Reporter) Progress(pr Progress) {
	pr.Step, pr.Message = clip(pr.Step, maxStepText), clip(pr.Message, maxMessageText)
	p.mu.Lock()
	if reflect.DeepEqual(pr, p.progress) {
		p.mu.Unlock()
		return
	}
	newStep := pr.Step != p.progress.Step
	p.progress, p.dirty = pr, true
	p.mu.Unlock()
	if newStep {
		p.wake()
	}
}

// Park reports the job as needs_attention with message as its error (code is
// the park's typed reason, which the handler also puts in its progress) while it
// keeps running here. The server drops a parked job's lease (needs_attention has
// none) and renews it once Unpark reports it running again; meanwhile the job
// goes on reporting, so a cancel from the server still ends it. If the Bridge
// disappears for good, the job stays needs_attention until an admin cancels it.
// Both are sent at once, and a job resumed after a restart keeps its park.
func (p *Reporter) Park(code, message string) {
	message = clip(strings.TrimSpace(message), maxErrorText)
	if message == "" {
		message = code
	}
	p.mu.Lock()
	changed := !p.parked || p.parkMsg != message
	p.parked, p.parkMsg = true, message
	if changed {
		p.dirty = true
	}
	p.mu.Unlock()
	if changed {
		p.wake()
	}
}

// Unpark reports the job running again after a Park (nothing when it isn't
// parked).
func (p *Reporter) Unpark() {
	p.mu.Lock()
	was := p.parked
	if was {
		p.parked, p.parkMsg, p.dirty = false, "", true
	}
	p.mu.Unlock()
	if was {
		p.wake()
	}
}

// parkState reports whether the job is parked, and with what message.
func (p *Reporter) parkState() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.parked, p.parkMsg
}

// wake asks the reporting goroutine to send now.
func (p *Reporter) wake() {
	if p.nudge == nil {
		return
	}
	select {
	case p.nudge <- struct{}{}:
	default:
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

// send reports the job's state (running, renewing the lease, or needs_attention
// while parked, which has none) with its current progress and acts on the
// answer: a cancel request or a lost claim ends the job's context. It reports
// whether the job goes on.
func (p *Reporter) send() bool {
	ctx := p.ctx
	p.mu.Lock()
	pr, dirty := p.progress, p.dirty
	parked, parkMsg := p.parked, p.parkMsg
	p.sentAt, p.dirty = time.Now(), false
	p.mu.Unlock()
	raw, _ := json.Marshal(pr)
	if dirty {
		if err := p.r.store.SetServerJobReport(ctx, p.job.ID, p.job.Attempt, raw, parkMsg); err != nil && ctx.Err() == nil {
			p.r.log.Warn("bridge: recording progress failed", "job", p.job.ID, "err", err)
		}
	}
	// The error always goes out: the park's message while parked, else "", which
	// clears the one a park left.
	state := link.StateRunning
	u := link.Update{Attempt: p.job.Attempt, State: &state, Error: &parkMsg}
	if parked {
		state = link.StateNeedsAttention
	}
	if pr.Step != "" {
		u.Progress = raw
	}
	got, err := p.srv.UpdateJob(ctx, p.job.ID, u)
	switch {
	case p.answer(got, err):
		return false
	case err == nil:
		return true
	case ctx.Err() != nil:
		return false
	default:
		// The server is unreachable for now: the job goes on, and its lease
		// (a running job's) lasts two minutes from the last report that got
		// through.
		p.r.log.Warn("bridge: reporting progress failed", "job", p.job.ID, "err", err)
		return true
	}
}

// answer acts on the server's answer to an update: the job follows a moved path,
// and a cancel request, a lost claim or a refused token ends its context. It
// reports whether the answer stopped the job.
func (p *Reporter) answer(got *link.Job, err error) bool {
	if err == nil && got.Path != nil && *got.Path != "" {
		p.mu.Lock()
		p.path = *got.Path
		p.mu.Unlock()
	}
	switch {
	case err == nil && got.CancelRequested:
		p.cancel(errCancelRequested)
	case errors.Is(err, link.ErrNotHeld):
		p.cancel(errLost)
	case errors.Is(err, link.ErrUnauthorized):
		p.cancel(errUnpaired)
		p.r.onUnauthorized(p.srv)
	default:
		return false
	}
	return true
}

// ConfirmLease proves ownership immediately before a filesystem publication. It
// acts on the answer as the keepalive does, and also refuses a moved path: the
// caller is about to write to the one it claimed.
func (p *Reporter) ConfirmLease(ctx context.Context) error {
	if p.r == nil {
		return nil
	}
	state := link.StateRunning
	got, err := p.srv.UpdateJob(ctx, p.job.ID, link.Update{Attempt: p.job.Attempt, State: &state})
	if p.answer(got, err) {
		if err != nil {
			return err
		}
		return context.Canceled
	}
	if err != nil {
		return err
	}
	if p.job.Path != nil && got.Path != nil && *p.job.Path != *got.Path {
		return link.ErrNotHeld
	}
	return ctx.Err()
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
