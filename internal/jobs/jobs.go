// Package jobs is the Bridge's generic job runner (BRIDGE-PLAN.md Phase 0). It
// long-polls the server for jobs of the kinds it has room for, runs each with
// the kind's Handler in that kind's lane, reports progress (which renews the
// server's lease and carries a cancel back, an admin's or the server's own),
// and reports the end.
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
	"maps"
	"reflect"
	"slices"
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
	// Priority is the priority the server gave the job (0 for one recorded
	// before the Bridge kept it): a resumed job of interactive priority waits
	// for the reserve it was claimed for.
	Priority int
	// Resumed: the runner resumed the job under its claim (after a restart, or
	// once a refused token works again) instead of claiming it now, so an earlier
	// run of it may have spent already.
	Resumed bool
	// shareOff: the claim's answer said share_off, or, for a job resumed after
	// a restart or a refused token, the claim or an answer since did
	// (store.ServerJob.ShareOff, or the stopped run's Reporter): Reporter.ShareOff.
	// Its Reporter writes it to the store at its first report (keepShareOff).
	shareOff bool
	// serverID is the id of the server that handed the job out (the claim's
	// link's, or the recorded row's): the one rule for which link the job
	// reports on (Runner.linkFor), and what a handler keys its durable state
	// and filings by (ServerID, Link).
	serverID string
	// pairing is the Bridge's pairing the job was claimed under (the claim's
	// link's PairingID, or the recorded row's): a job of another pairing is
	// dropped, never resumed or reported (Runner.sweepPairings, pairingGone).
	pairing string
}

// ServerID is the id of the server that handed the job out, never "" for a job
// the runner claimed or resumed. A handler keys the job's durable state and
// its filings by it, not by the Bridge's pairing now: a job of a server the
// Bridge is no longer paired with must never write under, or file for, the new
// one (BR-LINK-RULE). "" only for a job no runner claimed (a test's, a local
// run's), which keys by the pairing (KeyServer).
func (j Job) ServerID() string { return j.serverID }

// ClaimedFrom is job as server serverID handed it out (its ServerID), as the
// runner sets it on every job it claims: for a handler's tests.
func ClaimedFrom(job Job, serverID string) Job {
	job.serverID = serverID
	return job
}

// ClaimedUnder is ClaimedFrom for a job claimed under pairing (its Pairing):
// for a handler's tests.
func ClaimedUnder(job Job, serverID, pairing string) Job {
	job.serverID, job.pairing = serverID, pairing
	return job
}

// Pairing is the Bridge's pairing the job was claimed under, "" for a job no
// runner claimed. With the job's server, id and attempt it names one claim:
// a server hands a job out again under a later attempt (to a later pairing
// too, once its lease on the earlier one lapsed), and job ids are each
// server's own, so the id alone tells neither apart.
func (j Job) Pairing() string { return j.pairing }

// OwnRow reports whether sj is the row of this claim of the job: its id and,
// for a job the runner claimed, its attempt, server and pairing. A row is
// keyed by the job's id alone, so by the time a job looks (its work still
// being released after its pairing ended) the row under its id may be a later
// claim's, running here: that row is another job's to a handler, never its
// own. A job no runner claimed (a test's, a local run's) owns the row of its
// id.
func (j Job) OwnRow(sj store.ServerJob) bool {
	if sj.ID != j.ID {
		return false
	}
	return j.serverID == "" || sj.Attempt == j.Attempt && sj.ServerID == j.serverID && sj.Pairing == j.pairing
}

// KeyServer is the server id the job's durable state and filings are keyed by:
// its own (ServerID), or current (the pairing's) for a job no runner claimed.
func (j Job) KeyServer(current string) string {
	if j.serverID != "" {
		return j.serverID
	}
	return current
}

// Link returns srv when it may carry job's work: srv reaches the job's server
// (its ServerID() is the job's) and was made for the job's pairing (its
// PairingID(), when the job was claimed under one), or the job is no runner's.
// Otherwise (the Bridge paired with another server, unpaired and paired again
// with the job's server, or a link that names no server) it returns S's zero
// value, as for no link at all, so nothing of the job's reaches another server
// or pairing.
func Link[S any](job Job, srv S) S {
	if job.serverID == "" {
		return srv
	}
	l, ok := any(srv).(interface{ ServerID() string })
	if !ok || l.ServerID() != job.serverID {
		var none S
		return none
	}
	if job.pairing != "" {
		if p, ok := any(srv).(interface{ PairingID() string }); !ok || p.PairingID() != job.pairing {
			var none S
			return none
		}
	}
	return srv
}

// Handler runs one job kind.
type Handler interface {
	// Kind is the job kind ("bridge.verify"), as the server knows it.
	Kind() string
	// Lane is the lane the kind runs in (LaneLight, ...).
	Lane() string
	// Run does the job. Its ctx ends when it is cancelled (by an admin or the
	// server itself), the server takes it back or the Bridge stops; result
	// may be returned beside an error (a verify that found a mismatch). Return
	// a *ParkError to park the job.
	Run(ctx context.Context, job Job, rep *Reporter) (result any, err error)
}

// InteractiveHandler opts a kind into the reserved pool. Ordinary workers can
// still run any priority of this kind in its normal lane.
type InteractiveHandler interface {
	Interactive() bool
}

// ServerQueuedHandler marks a kind the server queues and schedules again
// itself (podcasts, Audible, LibriVox). After a 429 that outlasts the link's
// retries its job fails with the handler's own error, so the server can
// schedule the next run; a park would leave the job open without a lease.
// Any other kind's job needs attention, for an admin to queue it again.
type ServerQueuedHandler interface {
	ServerQueued() bool
}

// ParamSealer removes sensitive claim params before any execution row is
// written. A handler resumes from these stored params and its sealed secrets.
// A failure fails the job without running it: a *SealFailure is reported as it
// is, any other error only as a generic failure (its text may hold the secret).
type ParamSealer interface {
	SealParams(job Job) (stored json.RawMessage, err error)
}

// ClaimRowNeeder is a handler that runs a job only on a claim the store
// recorded (NeedsClaimRow): a sidecars.generate share job's share_off lives on
// its row, which keeps its books local (store.DB.SetServerJobShareOff), so a
// share the server turned off after the claim could otherwise still be filed.
// Such a claim whose write failed is abandoned like a sealed one, for the
// server to hand out again. A ParamSealer needs its row unless it says
// otherwise here (a podcast.fetch seals no secret, only its claim's pairing).
type ClaimRowNeeder interface {
	NeedsClaimRow(job Job) bool
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

// Server is the part of the server link the runner calls. ServerID is the id
// of the server the link reaches (the pairing's server_id, "" when unknown),
// and PairingID the Bridge's pairing the link was made for (recorded with
// every job claimed on it): read with the link itself, so neither ever comes
// from another pairing.
type Server interface {
	Claim(ctx context.Context, kinds []string, wait time.Duration) (*link.Job, error)
	UpdateJob(ctx context.Context, id int64, u link.Update) (*link.Job, error)
	ServerID() string
	PairingID() string
}

// Store is the part of the store the runner writes.
type Store interface {
	RecordClaim(ctx context.Context, j store.ServerJob) error
	SetServerJobProgress(ctx context.Context, id int64, attempt int, progress json.RawMessage) error
	SetServerJobReport(ctx context.Context, id int64, attempt int, progress json.RawMessage, parkMsg string) error
	SetServerJobShareOff(ctx context.Context, id int64, attempt int) error
	FinishServerJob(ctx context.Context, id int64, attempt int, state string, result json.RawMessage, errText string) error
	LoseServerJob(ctx context.Context, j store.ServerJob, errText string) (bool, error)
	MarkServerJobReported(ctx context.Context, id int64, attempt int) error
	OpenServerJobs(ctx context.Context) ([]store.ServerJob, error)
	GetServerJob(ctx context.Context, id int64) (*store.ServerJob, error)
	PruneServerJobs(ctx context.Context, keep int) (int64, error)
}

// Why a running job's context ended. errCancelRequested is any cancel; a
// cancel that says by whom wraps it (errCancelledOnPage, cancelCause): the
// server's 409 not_held with reason "cancelled" doesn't say, and is sent for
// the server's own cancels as for an admin's (bridge#80).
var (
	errCancelRequested = errors.New("the job was cancelled")
	errLost            = errors.New("the server no longer holds the job for this claim")
	errStopping        = errors.New("the Bridge is stopping")
	errUnpaired        = errors.New("the server refused the Bridge's token")
	// errOtherServer ends a job of a server the Bridge is no longer paired with
	// (Runner.linkFor): it is dropped here, recorded lost and reported nowhere,
	// and its own server's lease lapses it.
	errOtherServer = errors.New("the Bridge was paired with another server")
	// errPairingEnded ends a job whose pairing was ended on the Bridge's own
	// page (Runner.Unpaired): it stops here, recorded lost, and nothing more is
	// sent to its server, whose lease lapses it. The page names the reason by
	// these two errors' text (web/src/lib/jobs.ts DROPPED): change both together.
	errPairingEnded = errors.New("the Bridge was unpaired on its page")
	// errRelinked stops a running job whose server the Bridge was paired with
	// again (a new link: a new token, often a new address): its handler may
	// hold the old link, which the server revoked. It is left running and
	// resumed under its claim on the new link (resumeStranded), with a fresh
	// handler run, as a refused token's job is.
	errRelinked = errors.New("the Bridge was paired with the job's server again")

	// errCancelledOnPage is a cancel on the Bridge's own page (Runner.Cancel);
	// not link.ErrCancelledHere, the server's 409 for a job cancelled there.
	errCancelledOnPage = fmt.Errorf("%w on the Bridge's page", errCancelRequested)
)

// cancelCause is the cause of a cancel the server asked for in its answer
// (cancel_requested): the server's own reason when it recorded one as the
// job's error (its module turned off, an Audible account removed or its sync
// turned off: the Bridge's reports carry no error while the job runs, so any
// is the server's), else an admin's.
func cancelCause(reason string) error {
	if reason != "" {
		return fmt.Errorf("%w by the server: %s", errCancelRequested, reason)
	}
	return fmt.Errorf("%w by an admin", errCancelRequested)
}

// Abandoner is a Handler whose jobs hold work beyond Run (sidecars.generate: an
// engine book). The runner calls Abandon when it ends such a job without running
// the handler (the first report of a resumed job answered with a cancel or a lost
// claim, or a job dropped for another server's pairing or an unpair), so the
// work is released as the handler's own end would.
type Abandoner interface {
	Abandon(ctx context.Context, job Job)
}

// abandon tells the job's handler, when it is an Abandoner, that the job ended
// without it.
func (r *Runner) abandon(ctx context.Context, job Job) { r.abandonFor(ctx, job, nil) }

// abandonFor is abandon for a job whose context ended with cause: Abandon's
// context carries it, for Lost.
func (r *Runner) abandonFor(ctx context.Context, job Job, cause error) {
	if a, ok := r.handlers[job.Kind].(Abandoner); ok {
		if cause != nil {
			ctx = context.WithValue(ctx, abandonCauseKey{}, cause)
		}
		a.Abandon(ctx, job)
	}
}

// abandonCauseKey is where abandonFor puts why the job ended.
type abandonCauseKey struct{}

// Lost reports whether the job ended because the server no longer holds its
// claim (a lease that lapsed while the Bridge was down or cut off): the server
// may hand the same job out again, to this Bridge, so work it can resume (a
// partial download) is worth keeping. For a running job's context, or the
// context Abandon gets. Resumes is false for it all the same.
func Lost(ctx context.Context) bool {
	if errors.Is(context.Cause(ctx), errLost) {
		return true
	}
	cause, _ := ctx.Value(abandonCauseKey{}).(error)
	return errors.Is(cause, errLost)
}

// Resumes reports whether a job whose context ended will run again under its
// claim: the Bridge is stopping (the next start resumes it) or the server refused
// the token (resumed once a link works again). False when it was cancelled (by
// an admin or the server itself), the server took it back, or the Bridge was
// paired with another server or unpaired on its page, which end it here for
// good.
func Resumes(ctx context.Context) bool {
	return !endsForGood(context.Cause(ctx))
}

// endsForGood reports whether a job's context ended with a cause that ends it
// here for good (Resumes is false): a cancel, a lost claim, or a drop (a
// pairing with another server, an unpair on the Bridge's page). Such a job's
// handler, when it never ran, is told (abandon).
func endsForGood(cause error) bool {
	return errors.Is(cause, errCancelRequested) || errors.Is(cause, errLost) || dropped(cause)
}

// dropped reports whether cause drops a job: it ends here, recorded lost and
// reported to no server (the Bridge was paired with another one, or unpaired
// on its own page).
func dropped(cause error) bool {
	return errors.Is(cause, errOtherServer) || errors.Is(cause, errPairingEnded)
}

// DefaultKeepalive is how often a running job with no new progress reports by
// default. Each report renews the server's 2-minute lease (and is a write
// there), so 30 s keeps four reports inside one lease (one that didn't get
// through is tried again sooner, DefaultReportRetry), while an idle job moves
// the server's write generation twice a minute rather than six times. A
// changed progress is still sent within ProgressEvery and a new step at once;
// only a cancel waits up to this long to be heard.
const DefaultKeepalive = 30 * time.Second

// DefaultReportRetry is how soon a running job reports again after a report
// that didn't reach the server, until one does (then Keepalive again). At the
// keepalive alone, one report through and three failed would let the 2-minute
// lease lapse; at 10 s about ten attempts fit, as before the 30 s keepalive.
const DefaultReportRetry = 10 * time.Second

// Options tunes a Runner; zero values take the defaults.
type Options struct {
	Lanes map[string]int
	// Keepalive is how often a running job reports even with no new progress,
	// renewing the server's 2-minute lease and picking up a cancel (default
	// DefaultKeepalive).
	Keepalive time.Duration
	// ReportRetry is the usual delay after a transient report failure. A 429
	// can ask for a longer delay before the next idle report.
	ReportRetry time.Duration
	// ProgressEvery is the shortest gap between two progress reports of a job
	// (a new step is sent at once).
	ProgressEvery time.Duration
	// Wait is the claim's long-poll wait.
	Wait time.Duration
	// RetryEvery is how often a report that didn't reach the server is retried.
	RetryEvery time.Duration
	// Keep is how many finished jobs the store keeps.
	Keep int
	// Pairing is the Bridge's pairing as the app keeps it, its link or none
	// (a refused token, before the first heartbeat). A row recorded under
	// another pairing is dropped (sweepPairings). nil is always the zero
	// Pairing (tests whose links name no pairing).
	Pairing func() Pairing
	Log     *slog.Logger
}

// Pairing names the Bridge's pairing. ID is its identity (the app's
// pairing.id setting, recorded with every job claimed under it, Server's
// PairingID): "" while the Bridge is unpaired, new after an unpair on its
// page or a pairing with another server, kept while the token is refused and
// when it is paired again with the same server without an unpair. ServerID is
// the id of the server it reaches ("" when unknown).
type Pairing struct {
	ID, ServerID string
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
	stranded []Job
	// ended are the links whose pairing was ended on the Bridge's own page
	// (Unpaired): nothing is launched on one, and a claim one answers late is
	// dropped. unpaired are the jobs a refused token had stopped when their
	// pairing was ended, which the claim loop drops (dropUnpaired).
	ended    map[Server]struct{}
	unpaired []Job
	// starting are the claims start is recording, from before their row is
	// written until launch holds them in running: the look for another
	// pairing's rows (sweepPairings) never takes such a row.
	starting map[int64]int
	// dropping are the dropped jobs whose work is being released, until each
	// is recorded lost (releaseDropped): no look drops one twice.
	dropping map[int64]int
	// dropDeadline bounds one round of dropped jobs' releases (abandonTimeout;
	// tests shorten it).
	dropDeadline time.Duration
	// drops are the batches releaseDropped runs; Run waits for them.
	drops sync.WaitGroup
	// changed is closed and replaced when admission or pairing may have
	// changed. Dispatchers compare a claim's target before refreshing it
	// (sameTarget), and eligibility before ending a pause (sameRequest).
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
		opts.Keepalive = DefaultKeepalive
	}
	if opts.ReportRetry <= 0 {
		opts.ReportRetry = DefaultReportRetry
	}
	opts.ReportRetry = min(opts.ReportRetry, opts.Keepalive)
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
	if opts.Pairing == nil {
		opts.Pairing = func() Pairing { return Pairing{} }
	}
	if onUnauthorized == nil {
		onUnauthorized = func(Server) {}
	}
	r := &Runner{opts: opts, store: st, handlers: map[string]Handler{}, server: server, onUnauthorized: onUnauthorized,
		log: opts.Log, running: map[int64]*run{}, use: map[string]int{}, reserved: map[string]int{}, waiting: map[string]int{},
		ended: map[Server]struct{}{}, starting: map[int64]int{}, dropping: map[int64]int{}, dropDeadline: abandonTimeout,
		changed: make(chan struct{})}
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

// Lanes reports every lane's use, sorted by name. A lane with no capacity and
// nothing running is left out: a server that can't take priority claims folds
// the interactive reserve into the download lane, and "0 of 0" says nothing.
func (r *Runner) Lanes() []Lane {
	srv := r.server()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Lane, 0, len(r.opts.Lanes))
	for name, c := range r.capacities(srv) {
		if c == 0 && r.use[name] == 0 {
			continue
		}
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
	// Then the dropped jobs' releases (releaseDropped, started on this goroutine
	// or on a run's, before the run is done: none starts once this waits).
	defer r.drops.Wait()
	defer wg.Wait()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(errStopping)
	wg.Add(1)
	go func() { defer wg.Done(); r.retryReports(ctx) }()
	// swept is the pairing whose look for the other pairings' rows went
	// through (sweepPairings): the claim loop looks again whenever the pairing
	// changes, and until a look can read the store.
	var swept string
	var sweptOK bool
	sweep := func() bool {
		cur := r.opts.Pairing().ID
		if sweptOK && swept == cur {
			return true
		}
		if !r.sweepPairings(ctx) {
			return false
		}
		swept, sweptOK = cur, true
		return true
	}
	for ctx.Err() == nil {
		changed := r.wakeChan()
		if !sweep() {
			// Nothing of an earlier pairing is resumed before it is dropped.
			sleep(ctx, 5*time.Second)
			continue
		}
		srv, id := r.paired()
		if srv == nil {
			select {
			case <-changed:
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
			continue
		}
		if r.recover(ctx, srv, id, &wg) {
			break
		}
	}
	wg.Add(2)
	go func() { defer wg.Done(); r.dispatch(ctx, false, &wg) }()
	go func() { defer wg.Done(); r.dispatch(ctx, true, &wg) }()
	for ctx.Err() == nil {
		changed := r.wakeChan()
		r.dropUnpaired(ctx)
		// Paired again or not: a new pairing's claims and runs are held
		// (holdsLocked) or its own, and its stranded jobs resume only below.
		var retry <-chan time.Time
		if !sweep() {
			retry = time.After(5 * time.Second)
		}
		if srv, id := r.paired(); srv != nil {
			r.dropOtherServers(id)
			r.resumeStranded(ctx, srv, id, &wg)
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-retry:
		}
	}
}

// recover picks up what the last run left: jobs still running are resumed
// under their claim (a kind this Bridge no longer runs is failed), finished
// jobs wait for retryReports. srv is the current link, id its server's. It
// reports whether it could read them.
func (r *Runner) recover(ctx context.Context, srv Server, id string, wg *sync.WaitGroup) bool {
	open, err := r.store.OpenServerJobs(ctx)
	if err != nil {
		r.log.Warn("bridge: reading the open jobs failed", "err", err)
		sleep(ctx, 5*time.Second)
		return false
	}
	var drops []drop
	for _, sj := range open {
		if !sameServer(sj.ServerID, id) || sj.Pairing != srv.PairingID() {
			// Another server's job (the Bridge was paired with this one since):
			// its id means nothing here, so nothing is reported for it. Or an
			// earlier pairing's, one the sweep couldn't record: the same. One
			// the sweep is releasing is left to it; one it recorded lost since
			// the list was read (no longer held, still running in the list) is
			// found ended when its turn comes (dropStands), never released
			// twice.
			if r.holds(sj.ID) {
				continue
			}
			if d, ok := r.dropRecorded(ctx, sj); ok {
				drops = append(drops, d)
			}
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
		r.launch(ctx, srv, job, wg, sj.Error, sj.Progress, nil)
	}
	r.releaseDropped(ctx, drops)
	return true
}

// dropRecorded ends what a start found open for another server or an earlier
// pairing (dropCause): an unreported end is never sent, and a running job is
// returned (ok), for the caller to release and record lost (releaseDropped).
func (r *Runner) dropRecorded(ctx context.Context, sj store.ServerJob) (d drop, ok bool) {
	why := r.dropCause(sj.ServerID)
	r.log.Info("bridge: a job of another pairing is dropped", "job", sj.ID, "server", sj.ServerID, "why", why)
	if sj.State == store.JobRunning {
		return drop{job: storedJob(sj), why: why}, true
	}
	if err := r.store.MarkServerJobReported(ctx, sj.ID, sj.Attempt); err != nil {
		r.log.Warn("bridge: recording a job failed", "job", sj.ID, "err", err)
	}
	return drop{}, false
}

// dropCause is why a job of server serverID that isn't the current pairing's
// is dropped: the Bridge is paired with another server now (errOtherServer),
// else its pairing was ended on the Bridge's page (errPairingEnded: unpaired,
// or paired again with the same server since).
func (r *Runner) dropCause(serverID string) error {
	if _, other := r.linkFor(serverID); other {
		return errOtherServer
	}
	if p := r.opts.Pairing(); p.ID != "" && !sameServer(serverID, p.ServerID) {
		return errOtherServer
	}
	return errPairingEnded
}

// abandonTimeout bounds one round of the release of dropped jobs' work
// (releaseDropped), all of the round's jobs together. A handler's release may
// wait on a lock its own run holds (podcast.fetch's file lock).
const abandonTimeout = 2 * time.Minute

// dropConcurrency is how many of a batch's dropped jobs release their work at
// once (releaseDropped).
const dropConcurrency = 4

// drop is a running job no run here holds (or a run handing its release over,
// releaseRun), dropped for why (a server the Bridge is no longer paired with,
// or a pairing ended on its page). raw is the result its run had, recorded
// with it. end, when set, records the job's end in place of the lost record
// (a run that a cancel ended before its handler ran: settle's owed).
type drop struct {
	job Job
	why error
	raw json.RawMessage
	end func(context.Context)
}

// abandonWithin releases the work of a job dropped for why under actx, a
// deadline's context, and logs a slow release and one that gave up at the
// deadline. A job whose handler holds no work (not an Abandoner) has nothing
// to release.
func (r *Runner) abandonWithin(actx context.Context, job Job, why error) {
	if !r.holdsWork(job) {
		return
	}
	start := time.Now()
	r.abandonFor(actx, job, why)
	switch took := time.Since(start); {
	case errors.Is(actx.Err(), context.DeadlineExceeded):
		r.log.Warn("bridge: releasing a dropped job's work gave up at its deadline", "job", job.ID, "kind", job.Kind,
			"took", took.Round(time.Second))
	case took > r.dropDeadline/4:
		r.log.Warn("bridge: releasing a dropped job's work was slow", "job", job.ID, "kind", job.Kind, "took", took.Round(time.Second))
	}
}

// holdsWork reports whether job's handler holds work beyond Run that a drop
// releases (an Abandoner).
func (r *Runner) holdsWork(job Job) bool {
	_, ok := r.handlers[job.Kind].(Abandoner)
	return ok
}

// releaseDropped releases the work of drops (Abandon) and then records each
// lost, so a job waiting for one to end (generate's awaitOpenJobEnd reads the
// row's running state) starts only once its engine book is stopped, as the
// runner's own ends order it (bridge#37). It runs off the caller's goroutine
// (a start's look or recover, the claim loop, a dropped run's: releaseRun),
// dropConcurrency at a time, in
// rounds of one deadline each (the drop deadline, releaseRound), so N slow
// releases hold up neither the start nor the claiming, nor take N deadlines.
// A job is recorded lost only once its release returned inside its round's
// deadline: one the deadline cut short, or that the round never reached, is
// still running and held, and goes to the next round, never told under a
// context that had ended already (generate's Abandon reads the job's engine
// book first: told so, it would release nothing and the book would run on
// beside the job that waited for this one's end). One the Bridge's stop
// reaches first, or cuts short, is left running for the next start's look
// (sweepPairings), never told past the stop. Each job is held (holdsLocked)
// until it is recorded, so no look drops it twice, and its row is read again
// as its turn comes (dropStands). Run waits for the rounds.
func (r *Runner) releaseDropped(ctx context.Context, drops []drop) {
	if len(drops) == 0 || ctx.Err() != nil {
		return // a stop's: left running for the next start's look
	}
	r.mu.Lock()
	for _, d := range drops {
		r.dropping[d.job.ID]++
	}
	r.mu.Unlock()
	r.drops.Add(1)
	go func() {
		defer r.drops.Done()
		for len(drops) > 0 && ctx.Err() == nil {
			if drops = r.releaseRound(ctx, drops); len(drops) > 0 && ctx.Err() == nil {
				r.log.Info("bridge: dropped jobs whose work isn't released yet are tried again", "jobs", len(drops))
			}
		}
		r.unhold(drops) // a stop's
	}()
}

// releaseRound is one round of releaseDropped: each of drops has its turn
// (releaseTurn), dropConcurrency at a time, all under one deadline. It returns
// the jobs to try again in the next round, still held: first those the round
// never reached, in their order, then those whose release the deadline cut
// short, so a release that keeps giving up never keeps the others waiting.
func (r *Runner) releaseRound(ctx context.Context, drops []drop) []drop {
	actx, cancel := context.WithTimeout(ctx, r.dropDeadline)
	defer cancel()
	// outcomes[i] is drops[i]'s: each written by the one worker that took it.
	outcomes := make([]turn, len(drops))
	queue := make(chan int, len(drops))
	for i := range drops {
		queue <- i
	}
	close(queue)
	var workers sync.WaitGroup
	for range min(dropConcurrency, len(drops)) {
		workers.Go(func() {
			for i := range queue {
				if outcomes[i] = r.releaseTurn(ctx, actx, drops[i]); outcomes[i] == turnDone {
					r.unhold(drops[i : i+1])
				}
			}
		})
	}
	workers.Wait()
	var unreached, cut []drop
	for i, d := range drops {
		switch outcomes[i] {
		case turnUnreached:
			unreached = append(unreached, d)
		case turnCut:
			cut = append(cut, d)
		}
	}
	return append(unreached, cut...)
}

// turn is how a dropped job's turn in a round ended (releaseTurn).
type turn int

const (
	// turnDone: released and recorded lost, or no longer the claim it was
	// dropped for (dropStands). Nothing more is done for it.
	turnDone turn = iota
	// turnUnreached: the round's deadline passed (or the Bridge stopped)
	// before its turn came. Its release was never told.
	turnUnreached
	// turnCut: its release was told and the round's deadline (or the Bridge's
	// stop) ended it. Its work may not be released.
	turnCut
)

// releaseTurn is one dropped job's turn in a round (releaseRound), whose
// deadline is actx's: its work released, then the job recorded lost. A job
// that holds work (holdsWork) is recorded only when its release returned
// inside the deadline, and is never told once it passed. Nothing is done for
// a row that is no longer the job's claim (dropStands), nor once the Bridge
// stops (ctx, the runner's).
func (r *Runner) releaseTurn(ctx, actx context.Context, d drop) turn {
	holds := r.holdsWork(d.job)
	if holds && actx.Err() != nil {
		return turnUnreached
	}
	if ctx.Err() != nil || !r.dropStands(ctx, d.job) {
		return turnDone
	}
	if holds {
		if r.abandonWithin(actx, d.job, d.why); actx.Err() != nil {
			return turnCut
		}
	}
	if d.end != nil {
		d.end(ctx)
	} else {
		r.recordLost(ctx, d.job, d.raw, d.why)
	}
	return turnDone
}

// unhold ends releaseDropped's hold of drops (dropping).
func (r *Runner) unhold(drops []drop) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range drops {
		unholdLocked(r.dropping, d.job.ID)
	}
}

// dropStands reads a dropped job's row again as its turn to be released comes
// (releaseDropped): it may have waited minutes behind the batch's slower
// releases while a new pairing claims beside them, and job ids are each
// server's own. It reports whether the job's work is still its own to release:
// the row is still the running claim the job was dropped for, or another
// server's or pairing's claim of another kind took the id (that claim's
// handler keeps no state of the job's kind, so nothing else would release the
// job's work, and the job's handler guards what it shares under the id by its
// claim: generate's StopGuard). One a later claim of the same kind took, one
// that ended (a release recorded it since a start's recover listed it) or one
// that is gone is left alone, its work never released for the job listed (a
// handler's state may be keyed by the job's id alone: generate's engine book,
// which that claim follows). A row that can't be read stands as it was listed
// (recordLost checks the claim again in its write, and leaves another claim's
// row alone).
func (r *Runner) dropStands(ctx context.Context, job Job) bool {
	sj, err := r.store.GetServerJob(ctx, job.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		return true
	case sj.Kind != job.Kind:
		return true
	}
	return sj.State == store.JobRunning && sj.Attempt == job.Attempt && sj.ServerID == job.serverID && sj.Pairing == job.pairing
}

// unholdLocked ends one hold of job id in holds (starting, dropping).
func unholdLocked(holds map[int64]int, id int64) {
	if holds[id]--; holds[id] <= 0 {
		delete(holds, id)
	}
}

// recordLost records a job lost for why (its claim taken back, or the job
// dropped), with its result so far. Job ids are each server's own and the row
// is keyed by id and attempt only: once the Bridge works for another server or
// pairing, a claim of that one's may already hold the row under the same id
// and attempt (it claims as the old pairing's runs end), and is left alone
// (store.LoseServerJob checks it in the write).
func (r *Runner) recordLost(ctx context.Context, job Job, raw json.RawMessage, why error) {
	if dropped(why) {
		own, err := r.store.LoseServerJob(ctx, store.ServerJob{ID: job.ID, Attempt: job.Attempt, ServerID: job.serverID,
			Pairing: job.pairing, Result: raw}, why.Error())
		switch {
		case err != nil:
			r.log.Warn("bridge: recording a job failed", "job", job.ID, "err", err)
		case !own:
			r.log.Info("bridge: a dropped job's row is another pairing's claim now; it is left alone", "job", job.ID)
		}
		return
	}
	if err := r.store.FinishServerJob(ctx, job.ID, job.Attempt, store.JobLost, raw, why.Error()); err != nil {
		r.log.Warn("bridge: recording a job failed", "job", job.ID, "err", err)
	}
}

// resumeStranded resumes, under their claim, the jobs a refused token stopped,
// now that a link works again (the refusal ended, or the Bridge was paired
// again with their server), as a restart would: when the server took one back
// meanwhile, its first report is a 409 and it is lost. srv is the current
// link, id its server's: a job of another server is dropped (linkFor's rule).
// One a later claim already runs here is left to that claim. One whose row
// can't be read now stays stranded for the next look (a heartbeat wakes the
// runner): dropped, nothing would resume it before a restart, and the book of a
// share job would wait on it until then (ShareOff answers for a stranded job,
// never for one no run here knows).
func (r *Runner) resumeStranded(ctx context.Context, srv Server, id string, wg *sync.WaitGroup) {
	r.mu.Lock()
	stranded := r.stranded
	r.stranded = nil
	r.mu.Unlock()
	var drops []drop
	for _, job := range stranded {
		r.mu.Lock()
		_, running := r.running[job.ID]
		r.mu.Unlock()
		if running {
			continue // a later claim of it runs here under its id
		}
		sj, err := r.store.GetServerJob(ctx, job.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			r.log.Warn("bridge: reading a job its refused token stopped failed; it is tried again", "job", job.ID, "err", err)
			r.mu.Lock()
			r.stranded = append(r.stranded, job)
			r.mu.Unlock()
			continue
		}
		if err != nil || sj.State != store.JobRunning || sj.Attempt != job.Attempt || sj.ServerID != job.serverID || sj.Pairing != job.pairing {
			// Its row is gone, a later claim took it (another server's or
			// pairing's claim of the same id included: its state isn't this
			// job's), or it ended.
			continue
		}
		if !sameServer(job.serverID, id) || job.pairing != srv.PairingID() {
			// Paired with another server since (its job ids are another
			// server's), or the pairing was ended and made again: the claim
			// can't be reported anywhere now.
			drops = append(drops, drop{job: job, why: r.dropCause(job.serverID)})
			continue
		}
		r.log.Info("bridge: resuming a job its refused token stopped", "job", job.ID, "kind", job.Kind, "attempt", job.Attempt)
		job.Resumed = true
		// An answer since the claim may have said share_off: the store has it,
		// or the stopped run heard it and its write failed (the resumed run's
		// Reporter writes it at its first report).
		job.shareOff = job.shareOff || sj.ShareOff
		r.launch(ctx, srv, job, wg, sj.Error, sj.Progress, nil)
	}
	r.releaseDropped(ctx, drops)
}

// Unpaired tells the runner that the pairing was ended on the Bridge's own
// page; srv is the link it had (nil for none). Every job here stops: cancelled
// locally and recorded lost (errPairingEnded), with nothing more sent to that
// server, whose leases lapse them, and none of them is ever recorded or
// reported for a server the Bridge pairs with next. A claim srv answers after
// this is dropped (start) and nothing is launched on srv again. The caller
// ends the pairing first (server returns nil and Pairing names none from then
// on: the rows the store still has of it, that no run here holds, are another
// pairing's, which the claim loop drops, sweepPairings), then calls this. It
// doesn't wait: the runs end, and their work is released, on their own
// goroutines and releaseDropped's, never on the caller's (the unpair page's
// request).
func (r *Runner) Unpaired(srv Server) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if srv != nil {
		r.ended[srv] = struct{}{}
	}
	for _, ru := range r.running {
		ru.ended.Store(true)
		ru.cancel(errPairingEnded)
	}
	r.unpaired = append(r.unpaired, r.stranded...)
	r.stranded = nil
	r.wakeLocked()
}

// holdsLocked reports whether job id is one the runner ends through a path of
// its own: a run (settle, or release for one a stop left running), a job a
// refused token stopped (dropUnpaired, or resumeStranded for a later
// pairing's), a claim start is recording (launch) or a dropped job whose work
// is being released (releaseDropped).
func (r *Runner) holdsLocked(id int64) bool {
	_, running := r.running[id]
	has := func(job Job) bool { return job.ID == id }
	return running || r.starting[id] > 0 || r.dropping[id] > 0 || slices.ContainsFunc(r.stranded, has) || slices.ContainsFunc(r.unpaired, has)
}

// holds is holdsLocked under r.mu.
func (r *Runner) holds(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.holdsLocked(id)
}

// sweepPairings drops the open rows of a pairing other than the current one
// (Options.Pairing): the jobs of a pairing ended on the Bridge's page (a
// restart since included: the pairing is the store's, never only this
// process's) or of a server the Bridge is no longer paired with. A running
// one no run here holds (holdsLocked: a run, a claim being recorded, a job a
// refused token stopped, one being released) is released and recorded lost
// (releaseDropped, off the claim loop), so a later
// pairing, with its server or another, never resumes it; a finished one's end
// is never reported. A held one is ended by its own path (settle,
// dropUnpaired, resumeStranded, releaseDropped). Each row is read again before
// it is dropped:
// the list is read once while the new pairing claims beside the look, and
// job ids are each server's own, so a claim of
// the new pairing may hold the row under the listed id by then, run and
// ended (no longer held): that claim's row is never dropped, nor its end
// marked reported, for the one listed. It reports whether it could read the
// store, and stops once ctx ends (a stop leaves the rows for the next start's
// look).
func (r *Runner) sweepPairings(ctx context.Context) bool {
	open, err := r.store.OpenServerJobs(ctx)
	if err != nil {
		r.log.Warn("bridge: reading the jobs of an earlier pairing failed; it is tried again", "err", err)
		return false
	}
	read := true
	var drops []drop
	defer func() { r.releaseDropped(ctx, drops) }()
	for _, sj := range open {
		if ctx.Err() != nil {
			return false
		}
		if sj.Pairing == r.opts.Pairing().ID { // read again: a pairing made since keeps its rows
			continue
		}
		if r.holds(sj.ID) {
			continue
		}
		now, err := r.store.GetServerJob(ctx, sj.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			r.log.Warn("bridge: reading a job of an earlier pairing failed; it is tried again", "job", sj.ID, "err", err)
			read = false
		case now.Pairing != sj.Pairing || now.ServerID != sj.ServerID || now.Attempt != sj.Attempt:
			// Another claim's row now (the current pairing's).
		case now.State == store.JobRunning || !now.Reported:
			if d, ok := r.dropRecorded(ctx, *now); ok {
				drops = append(drops, d)
			}
		}
	}
	return read && ctx.Err() == nil
}

// dropUnpaired drops the jobs a refused token had stopped when their pairing
// was ended on the Bridge's page (Unpaired): released and recorded lost
// (releaseDropped), reported nowhere. As in resumeStranded, one a later claim
// runs or holds the row of (or that ended) is left to it, and one whose row
// can't be read now waits for the next look. ctx is the runner's: once the
// Bridge stops they wait for the next start's look (sweepPairings), their
// pairing gone from the store.
func (r *Runner) dropUnpaired(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	unpaired := r.unpaired
	r.unpaired = nil
	r.mu.Unlock()
	var drops []drop
	for _, job := range unpaired {
		r.mu.Lock()
		_, running := r.running[job.ID]
		r.mu.Unlock()
		if running {
			continue
		}
		sj, err := r.store.GetServerJob(ctx, job.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			r.log.Warn("bridge: reading a job the unpair stopped failed; it is tried again", "job", job.ID, "err", err)
			r.mu.Lock()
			r.unpaired = append(r.unpaired, job)
			r.mu.Unlock()
		case sj.State == store.JobRunning && sj.Attempt == job.Attempt && sj.ServerID == job.serverID && sj.Pairing == job.pairing:
			drops = append(drops, drop{job: job, why: errPairingEnded})
		}
	}
	r.releaseDropped(ctx, drops)
}

// stale reports whether srv, a link that just answered a claim, is no longer
// the Bridge's to run a job on: its pairing was ended on the Bridge's page
// (Unpaired), or the Bridge is paired with another server now. Such an answer
// is dropped whole (start): job ids are each server's own, so recording it
// could overwrite the row of the new server's job of the same id, and running
// it would report to a server the Bridge has left. That server's lease lapses
// the claim.
func (r *Runner) stale(srv Server) bool {
	r.mu.Lock()
	_, ended := r.ended[srv]
	r.mu.Unlock()
	if ended || srv.PairingID() != r.opts.Pairing().ID {
		return true
	}
	_, other := r.linkFor(srv.ServerID())
	return other
}

// pairingGone reports whether job's pairing is no longer the Bridge's
// (Options.Pairing): unpaired on its page (before Unpaired has reached the
// job's run, too: the app forgets the pairing first) or paired with another
// server. Its run reports nothing more and is dropped however it ends.
func (r *Runner) pairingGone(job Job) bool { return job.pairing != r.opts.Pairing().ID }

// sameServer reports whether server ids a and b are one server. Unknown ("")
// on either side is never the same: nothing of a job whose server isn't known
// is reported, shared or filed on a link, nor a job run on a link whose server
// isn't known (the app learns a stored pairing's id before its link counts).
func sameServer(a, b string) bool { return a != "" && a == b }

// paired is the Bridge's current link and the id of the server it reaches
// (nil, "" while it has none).
func (r *Runner) paired() (Server, string) {
	srv := r.server()
	if srv == nil {
		return nil, ""
	}
	return srv, srv.ServerID()
}

// linkFor is THE ONE RULE for whether a job belongs to the Bridge's current
// link: the id of the server that handed it out (serverID: the claim's link's,
// recorded on its row) against the id of the server the current link reaches.
// It returns the current link while it reaches that server; other while it
// reaches another one; neither while the Bridge has no link (unpaired, the
// token refused, or before a new link's first heartbeat: nothing is decided
// yet). A pairing with the same server, at a new address or with a new token,
// keeps its jobs (the server holds a claim by its job id and attempt, not by
// the token), and they report on the new link; a pairing with another server
// drops them here, recorded lost and reported nowhere (errOtherServer), and
// their own server's lease lapses them. Unknown on either side (a row from
// before the Bridge recorded it, or a link whose pairing answer named none) is
// never the same (sameServer): such a job is dropped.
func (r *Runner) linkFor(serverID string) (cur Server, other bool) {
	srv, id := r.paired()
	switch {
	case srv == nil:
		return nil, false
	case !sameServer(serverID, id):
		return nil, true
	}
	return srv, false
}

// dropOtherServers ends every run of a server other than id's (the current
// link's), waiting for its slot or running: each is dropped (errOtherServer),
// never reported to the server the Bridge is paired with now. A run's own
// reports drop it too, at its next look; this does it as soon as a pairing
// with another server is up.
func (r *Runner) dropOtherServers(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ru := range r.running {
		if !sameServer(ru.job.serverID, id) {
			ru.cancel(errOtherServer)
		}
	}
}

// jobOf is a claimed job as a Handler sees it.
func jobOf(j *link.Job) Job {
	return Job{ID: j.ID, Kind: j.Kind, LibraryID: j.LibraryID, Path: j.Path, Params: j.Params, Attempt: j.Attempts, Priority: j.Priority,
		shareOff: j.ShareOff}
}

// storedJob is a job the store recorded, as a Handler sees it.
func storedJob(sj store.ServerJob) Job {
	return Job{ID: sj.ID, Kind: sj.Kind, LibraryID: sj.LibraryID, Path: sj.Path, Params: sj.Params, Attempt: sj.Attempt,
		Priority: sj.Priority, shareOff: sj.ShareOff, serverID: sj.ServerID, pairing: sj.Pairing}
}

// run is one running job. parked is the park message a resumed job had (its
// first report keeps it parked), progress the progress it last recorded (a
// parked one's first report keeps its park's code too). The link it reports on
// is its Reporter's (linkFor).
type run struct {
	job      Job
	lane     string
	cancel   context.CancelCauseFunc
	parked   string
	progress json.RawMessage
	admitted bool
	// yielded: the job gave its slot back while it waits on something outside
	// the Bridge (Reporter.Yield): counted neither in use nor in waiting, so the
	// lane admits another job meanwhile; Reporter.Reclaim queues it again.
	yielded bool
	// stray: a claim answered outside its admission (claimRequest.fallback).
	// While it waits for its first slot, the dispatcher that took it
	// (strayReserve) claims less (strayWaitingLocked); Yield clears it.
	stray, strayReserve bool
	// rep is the run's Reporter (nil until execute makes it): ShareOff asks
	// through it, a job waiting for its slot included. Written under r.mu.
	rep *Reporter
	// ended: the job's pairing was ended on the Bridge's page (Unpaired, set
	// under r.mu): it reports nothing more (Reporter.link) and is recorded lost
	// however it ends (settle), never kept to resume (release).
	ended atomic.Bool
}

// start records a claimed job and runs it. claim is the dispatcher's request
// that took it (nil in tests that start a job directly). A claim answered on a
// link the Bridge has left since (stale) is dropped instead.
func (r *Runner) start(ctx context.Context, srv Server, job Job, wg *sync.WaitGroup, claim *claimRequest) {
	// The claim's link's: its server handed the job out, under its pairing.
	job.serverID, job.pairing = srv.ServerID(), srv.PairingID()
	if r.stale(srv) {
		// An answer from a pairing the Bridge has left: neither recorded (the
		// row of its id may be the new server's job) nor run nor reported.
		r.log.Info("bridge: a claim answered after its pairing ended is dropped", "job", job.ID, "kind", job.Kind, "server", job.serverID)
		return
	}
	// Held (starting) from before its row is written until launch holds it in
	// running: a look for the rows an unpair left never drops it meanwhile, as
	// a new pairing's claim or as one launch ends at once (its link ended).
	r.mu.Lock()
	r.starting[job.ID]++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		unholdLocked(r.starting, job.ID)
		r.mu.Unlock()
	}()
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
	if err := r.recordClaim(context.WithoutCancel(ctx), store.ServerJob{ID: job.ID, Kind: job.Kind, LibraryID: job.LibraryID, Path: job.Path,
		Params: job.Params, Attempt: job.Attempt, ServerID: job.serverID, Pairing: job.pairing, Priority: job.Priority, ShareOff: job.shareOff}); err != nil {
		r.log.Warn("bridge: recording a claim failed", "job", job.ID, "err", err)
		needs := sealing
		if needer, ok := r.handlers[job.Kind].(ClaimRowNeeder); ok {
			needs = needer.NeedsClaimRow(job) // a sealer may answer for itself
		}
		if needs {
			// Fail closed: without its row the sealed secret could be neither
			// resumed nor released later, and a share job's share_off could
			// keep nothing local. The server hands the job out again.
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
	r.launch(ctx, srv, job, wg, "", nil, claim)
}

// claimRecordTries is how many times recordClaim tries the store's write, and
// claimRecordBackoff the wait before the first retry (doubled before each
// next).
const (
	claimRecordTries   = 3
	claimRecordBackoff = 100 * time.Millisecond
)

// recordClaim records a claim (store RecordClaim), trying a write that fails
// again a few times, a short backoff apart: a claim that can't be recorded is
// abandoned when its handler needs the row (start), which spends one of the
// server's attempts at the job, so a passing blip (a locked database) must not.
func (r *Runner) recordClaim(ctx context.Context, sj store.ServerJob) error {
	wait := claimRecordBackoff
	for i := 1; ; i++ {
		err := r.store.RecordClaim(ctx, sj)
		if err == nil || i == claimRecordTries {
			return err
		}
		r.log.Debug("bridge: recording a claim failed; trying again", "job", sj.ID, "err", err)
		sleep(ctx, wait)
		wait *= 2
	}
}

// launch runs a recorded job in its lane, parked with message parked when a
// resumed one was ("" when it wasn't). A job of a kind the Bridge doesn't run is
// failed at once. claim is nil for a recovered or stranded job, which waits for
// admission in its kind's ordinary lane, or in the reserve when it is of
// interactive priority, its kind opts in and the pairing negotiated the reserve
// (as it was claimed, bridge#13); a claimed job takes its reservation unless the
// server ignored the requested filter, in which case it waits for ordinary
// admission. A claimed job whose lane has no room for it even so (the claim
// answered as a capacity change cancelled it) is held, waiting for a slot with
// its lease renewed, never dropped (BR-CLAIM-HOLD).
func (r *Runner) launch(ctx context.Context, srv Server, job Job, wg *sync.WaitGroup, parked string, progress json.RawMessage, claim *claimRequest) {
	h, ok := r.handlers[job.Kind]
	if !ok {
		r.finish(ctx, srv, job, store.JobFailed, nil, nil, "this Bridge doesn't run "+job.Kind+" jobs")
		return
	}
	rctx, cancel := context.WithCancelCause(ctx)
	ru := &run{job: job, lane: h.Lane(), cancel: cancel, parked: parked, progress: progress}
	r.mu.Lock()
	if claim != nil {
		r.unreserveLocked(claim)
		ru.stray, ru.strayReserve = claim.fallback, claim.fallback && claim.interactive
	}
	reserved := claim != nil && !claim.fallback
	switch {
	case reserved && claim.interactive:
		ru.lane = LaneInteractive
	case claim == nil && job.Priority >= int(InteractivePriority) && interactive(h) && supportsPriority(srv):
		ru.lane = LaneInteractive
	}
	// A claim's reservation, just let go, is its slot: ahead of any job that
	// started waiting in the lane since the claim went out. Anything else waits
	// behind any job already waiting there (first come, first admitted).
	ru.admitted = (reserved || r.waiting[ru.lane] == 0) && r.roomLocked(r.capacities(srv), ru.lane, nil)
	if _, ended := r.ended[srv]; ended {
		// Unpaired on the page while the job was on its way here: it ends
		// before its first report, as the jobs Unpaired found running.
		ru.ended.Store(true)
		cancel(errPairingEnded)
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
// resumeStranded, and wakes the claim loop. One a refused token left running
// as the pairing was ended on the page (after settle looked, while it was
// held) waits in unpaired for the claim loop's dropUnpaired. One the Bridge's
// stop left running is in no list (the claim loop has ended): the next start
// resumes it, or drops it when its pairing was ended meanwhile (sweepPairings),
// so it never holds up the stop.
func (r *Runner) release(ru *run, stranded bool) {
	ru.cancel(nil)
	heard := ru.rep != nil && ru.rep.ShareOff() // set on this goroutine (execute)
	r.mu.Lock()
	r.releaseLocked(ru, stranded, heard)
	r.mu.Unlock()
}

// releaseLocked is release under r.mu.
func (r *Runner) releaseLocked(ru *run, stranded, heard bool) {
	if r.running[ru.job.ID] == ru { // a later claim of the same job may run under its id
		delete(r.running, ru.job.ID)
	}
	switch {
	case ru.admitted:
		r.use[ru.lane]--
	case !ru.yielded:
		r.waiting[ru.lane]--
	}
	if stranded {
		job := ru.job
		job.shareOff = job.shareOff || heard // carried to resumeStranded
		if ru.ended.Load() {
			// Unpaired on the page since it stopped: dropped, not resumed.
			r.unpaired = append(r.unpaired, job)
		} else {
			r.stranded = append(r.stranded, job)
		}
	}
	r.wakeLocked()
}

// execute runs the handler, keeps the lease alive beside it, and records and
// reports how it ended. base is the runner's context (for the final report,
// which must go out after the job's own context ended). It reports whether a
// refused token stopped the job (left running, to resume under its claim).
func (r *Runner) execute(ctx, base context.Context, srv Server, h Handler, ru *run) (stranded bool) {
	// A job claimed or resumed with share_off mostly has it stored already
	// (RecordClaim, or the row it was resumed from); its first report writes it
	// all the same (keepShareOff, the one write after the claim), so one whose
	// write failed, or that a refused token stopped, has it stored again.
	rep := &Reporter{r: r, ru: ru, srv: srv, job: ru.job, cancel: ru.cancel, ctx: ctx, nudge: make(chan struct{}, 1),
		shareOff: ru.job.shareOff}
	if ru.parked != "" {
		rep.parked, rep.parkMsg = true, ru.parked
		// The server hears a park whose code changes as a new park (a bell
		// event): the first report sends the stored progress, park and all.
		rep.progress = decodeProgress(ru.progress)
	}
	// Published once its park is set: Runner.ShareOff can Refresh it from now on,
	// and a report without the park would be heard as an unpark.
	r.mu.Lock()
	ru.rep = rep
	r.mu.Unlock()
	if !rep.send() {
		// Ended before its handler ran: a cancel, a lost claim or a drop ends it
		// for good, so what an earlier run of it left (an engine book) is
		// released, off this goroutine (settle's owed).
		return r.settle(base, ru, rep, nil, context.Cause(ctx))
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
		rep.started.Store(true)
		if r.serverQueued(ru.job.Kind) {
			ctx = link.WithLastError(ctx)
		}
		result, err = h.Run(ctx, ru.job, rep)
	}
	// A recovered job can lose its claim (or be dropped for another server's
	// pairing) while waiting for a slot, after its first report succeeded but
	// before its handler ever ran: settle releases what it holds (owed).
	close(done)
	<-reporting
	return r.settle(base, ru, rep, result, firstErr(context.Cause(ctx), err))
}

// firstErr prefers why the job's context ended over the handler's error (a
// handler stopped by a cancel usually returns ctx.Err()).
func firstErr(cause, err error) error {
	if cause != nil {
		return cause
	}
	return err
}

// serverQueued reports whether kind's handler is a ServerQueuedHandler.
func (r *Runner) serverQueued(kind string) bool {
	h, ok := r.handlers[kind].(ServerQueuedHandler)
	return ok && h.ServerQueued()
}

// settle records and reports a job's end, on the link its Reporter reports on
// now (linkFor). It reports whether a refused token stopped the job (left
// running, for resumeStranded).
func (r *Runner) settle(base context.Context, ru *run, rep *Reporter, result any, err error) (stranded bool) {
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
	// nil once the Bridge is paired with another server or was unpaired on its
	// page: the end is reported nowhere.
	srv, _ := rep.link()
	// owed: ended for good before its handler ran (a cancel, a lost claim or
	// a drop), so what an earlier run of it left is still to be released: off
	// this goroutine (releaseDropped, directly or through releaseRun), so a
	// release that blocks never keeps the job's lane slot, or its place first
	// in line for one, from other work. The row stays running and held until
	// the release returns, then the end is recorded (a cancel's through
	// drop.end), so a job that waits for the row's end (generate's
	// awaitOpenJobEnd) starts only once the work is released. Never past a
	// stop: the job is left running, and the next start resumes it, its first
	// report hearing the cancel or the lost claim again.
	owed := !rep.started.Load() && endsForGood(context.Cause(rep.ctx)) && r.holdsWork(job)
	var park *ParkError
	_, rateLimited := link.RateLimited(err)
	switch {
	case ru.ended.Load(), r.pairingGone(job):
		// Unpaired on the Bridge's page (or paired with another server since),
		// however the job ended (done, failed, cancelled, its token refused,
		// the Bridge stopping): nothing more goes to its server, and it is no
		// job of the server the Bridge pairs with next. One that stopped to
		// resume kept its work for that, and one dropped before its handler ran
		// holds what an earlier run left: released off this goroutine
		// (releaseRun), never past a stop.
		why := errPairingEnded
		if !ru.ended.Load() {
			why = r.dropCause(job.serverID)
		}
		cause := context.Cause(rep.ctx)
		if owed || cause != nil && !endsForGood(cause) && r.holdsWork(job) {
			r.releaseRun(base, job, raw, why)
			return false
		}
		r.log.Info("bridge: a job of an ended pairing is dropped", "job", job.ID, "kind", job.Kind, "why", why)
		r.recordLost(context.WithoutCancel(base), job, raw, why)
	case base.Err() != nil, errors.Is(err, errStopping):
		r.interrupted(rep, ru, err)
	case errors.Is(err, errUnpaired), errors.Is(err, errRelinked):
		// Left running: resumed under its claim once a link works again (or at
		// the next start; a relinked job's link already does), with the
		// share_off it heard (release carries it to resumeStranded, whose run
		// writes it again if this write fails too).
		rep.keepShareOff()
		r.log.Info("bridge: a job's link ended; it resumes under its claim on the next one", "job", job.ID, "why", err)
		return true
	case srv == nil, errors.Is(err, errOtherServer):
		if owed {
			r.releaseRun(base, job, raw, errOtherServer)
			return false
		}
		r.log.Info("bridge: a job of another server is dropped", "job", job.ID, "kind", job.Kind)
		r.recordLost(base, job, raw, errOtherServer)
	case errors.Is(err, errLost):
		r.log.Info("bridge: the server took a job back", "job", job.ID, "kind", job.Kind)
		if owed {
			r.releaseDropped(base, []drop{{job: job, why: err, raw: raw}})
			return false
		}
		r.recordLost(base, job, raw, err)
	case errors.Is(err, errCancelRequested):
		r.log.Info("bridge: a job was cancelled", "job", job.ID, "kind", job.Kind, "why", err)
		if owed {
			r.releaseDropped(base, []drop{{job: job, why: err, end: func(ctx context.Context) {
				if r.pairingGone(job) {
					// Unpaired, or paired with another server, while its work
					// was released: its end goes to no server.
					r.recordLost(ctx, job, raw, r.dropCause(job.serverID))
					return
				}
				r.finish(ctx, srv, job, store.JobCancelled, progress, raw, "")
			}}})
			return false
		}
		r.finish(base, srv, job, store.JobCancelled, progress, raw, "")
	case err == nil:
		r.finish(base, srv, job, store.JobDone, progress, raw, "")
	case rateLimited && !r.serverQueued(job.Kind):
		r.finish(base, srv, job, store.JobNeedsAttention, progress, raw, err.Error()+". Cancel and queue the job again when the server is ready.")
	case errors.As(err, &park):
		r.finish(base, srv, job, store.JobNeedsAttention, progress, raw, park.Error())
	default:
		// A cancel asked for since the last report, which the failure beat to
		// the next keepalive, ends it cancelled: the failure may be the stop
		// itself, and the admin asked for the job to end anyway. A failure
		// with no cancel held stays a failure. A Bridge that stopped while it
		// asked leaves the job running, as one stopped before settle does.
		cause := rep.heldCancel(base)
		switch {
		case base.Err() != nil:
			r.interrupted(rep, ru, err)
		case cause != nil:
			r.log.Info("bridge: a job was cancelled", "job", job.ID, "kind", job.Kind, "why", cause, "err", err)
			r.finish(base, srv, job, store.JobCancelled, progress, raw, "")
		default:
			r.finish(base, srv, job, store.JobFailed, progress, raw, err.Error())
		}
	}
	return false
}

// releaseRun drops a run whose handler still holds work (it stopped to resume,
// or never ran: settle's owed), for why: the release goes to releaseDropped,
// which records the job lost once it is done, the row running and held
// (dropping, taken before release lets go of the run) until then. So the run
// frees its lane slot as it returns, and a release that blocks, tried again
// round after round, never keeps a new pairing's claims out of the lane. Never
// past a stop (the Bridge's stop waits for this goroutine): a job the stop
// ended, or whose release it cuts short, is left running for the next start's
// look (sweepPairings), its pairing gone from the store.
func (r *Runner) releaseRun(base context.Context, job Job, raw json.RawMessage, why error) {
	if base.Err() != nil {
		r.log.Info("bridge: a job of an ended pairing is left for the next start to drop", "job", job.ID, "kind", job.Kind, "why", why)
		return
	}
	r.log.Info("bridge: a dropped job's work is released before it is recorded lost", "job", job.ID, "kind", job.Kind, "why", why)
	r.releaseDropped(base, []drop{{job: job, why: why, raw: raw}})
}

// interrupted leaves a job the stopping Bridge ended running: the next start
// resumes it under its claim, with the share_off it heard if the store takes it
// now (unless the pairing is ended on the page meanwhile: that start's look
// drops it, sweepPairings).
func (r *Runner) interrupted(rep *Reporter, ru *run, why error) {
	rep.keepShareOff()
	r.log.Info("bridge: a job was interrupted; it resumes at the next start", "job", ru.job.ID, "why", why)
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
	got, err := srv.UpdateJob(ctx, sj.ID, u)
	switch {
	case err == nil, errors.Is(err, link.ErrNotHeld):
		// The server ends a failure or a park it had asked to stop as
		// cancelled: its answer is the job as it stored it (a cancel
		// heldCancel missed, or got no answer about; a park never asks), and
		// its 409 cancelled a job it had ended first (a retried report among
		// them).
		cancelled := errors.Is(err, link.ErrCancelledHere) || (err == nil && got != nil && got.State == link.StateCancelled)
		if (sj.State == store.JobFailed || sj.State == store.JobNeedsAttention) && cancelled {
			// A failure or a park is recorded cancelled, as the server has
			// it: a parked job waits for no one now. A job that got its work
			// done stays done here. Its error stays in the log here.
			r.log.Info("bridge: the server had cancelled a job that failed or parked; it is recorded cancelled", "job", sj.ID, "state", sj.State, "err", sj.Error)
			if ferr := r.store.FinishServerJob(ctx, sj.ID, sj.Attempt, store.JobCancelled, sj.Result, ""); ferr != nil {
				// Still failed or parked and unreported here: sent again,
				// which the server answers 409 cancelled.
				r.log.Warn("bridge: recording a job failed", "job", sj.ID, "err", ferr)
				r.retry.Store(true)
				return
			}
		} else if errors.Is(err, link.ErrNotHeld) {
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
		srv, id := r.paired()
		if srv == nil || !r.retry.Swap(false) {
			continue
		}
		open, err := r.store.OpenServerJobs(ctx)
		if err != nil {
			r.retry.Store(true)
			continue
		}
		for _, sj := range open {
			// Another server's or an earlier pairing's end is never sent here
			// (recover and sweepPairings drop it; they may not have yet when
			// this looks).
			if sj.State != store.JobRunning && !sj.Reported && sameServer(sj.ServerID, id) && sj.Pairing == srv.PairingID() {
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
		ru.cancel(errCancelledOnPage)
	}
	return ok
}

// Why ShareOff couldn't ask the server for a job: no run of it is up here (a
// resumed job not launched yet, or one that ended), its link isn't the one the
// Bridge uses now (unpaired, refused, or paired again), or its reports don't
// get through.
var (
	ErrShareNotRunning   = errors.New("the job isn't running here")
	ErrShareOffLink      = errors.New("the Bridge isn't linked to the job's server now (unpaired, or its token refused)")
	ErrShareNotReachable = errors.New("the job's reports aren't reaching the server")
)

// ShareOff reports whether the server turned job id's share off
// (Reporter.ShareOff), for the contributing stage before it files
// (generate.Handler.ShareOff). It finds the job's run here, one still waiting
// for its lane slot included (its Reporter keeps reporting), and one a refused
// token stopped (what it heard). With ask, the job reports now and the answer
// acts as any other (Reporter.AskShareOff: true once it says share_off, and an
// answer from a link the Bridge no longer uses confirms nothing). Without,
// only what is known here: true when heard, false when the job could be asked,
// and an error, with no request made, when it couldn't be (ErrShareNotRunning,
// ErrShareOffLink, ErrShareNotReachable), so the stage waits before its
// lookups rather than after them.
func (r *Runner) ShareOff(ctx context.Context, id int64, ask bool) (bool, error) {
	r.mu.Lock()
	var rep *Reporter
	ru, running := r.running[id]
	if running {
		rep = ru.rep
	}
	stranded, heard := false, false
	for _, s := range r.stranded {
		// A run of the job up here is a later claim (resumeStranded takes a
		// stranded job off the list before it launches it): what the stranded
		// attempt heard isn't that claim's.
		if s.ID == id && !running {
			stranded, heard = true, s.shareOff
		}
	}
	r.mu.Unlock()
	switch {
	case rep == nil && heard:
		return true, nil
	case rep == nil && stranded:
		return false, ErrShareOffLink
	case rep == nil:
		return false, ErrShareNotRunning
	case rep.ShareOff():
		return true, nil
	case ask:
		return rep.AskShareOff(ctx)
	case !rep.OnCurrentLink():
		return false, ErrShareOffLink
	case rep.unreached():
		return false, ErrShareNotReachable
	}
	return false, nil
}

// OnCurrentLink reports whether the Bridge's current link reaches the job's
// server (linkFor: a new pairing with it, at a new address or with a new
// token, included). Unpaired, refused, or paired with another server, nothing
// the job's server says confirms a share here. True for a LocalReporter.
func (p *Reporter) OnCurrentLink() bool {
	if p.r == nil {
		return true
	}
	cur, _ := p.r.linkFor(p.job.serverID)
	return cur != nil
}

// AskShareOff asks the server now whether the job's share is off (Refresh),
// for a handler about to file it to the community: true once the job heard
// share_off (an answer that stopped the job counts when it carried it). It asks
// only while the Bridge's current link reaches the job's server, and looks
// again once the answer is in, as a pairing can change while it is on its way:
// with no link then it confirms nothing (ErrShareOffLink), and paired with
// another server the job is dropped (ErrStopped, its context ended). A report
// that didn't get through is its error (ErrStopped for an answer that stopped
// the job).
func (p *Reporter) AskShareOff(ctx context.Context) (bool, error) {
	if p.ShareOff() {
		return true, nil
	}
	if err := p.onLink(); err != nil {
		return false, err
	}
	err := p.Refresh(ctx)
	switch {
	case p.ShareOff():
		return true, nil
	case err != nil:
		return false, err
	}
	return false, p.onLink()
}

// onLink is AskShareOff's look at the link (link): nil while the current link
// reaches the job's server, ErrShareOffLink while the Bridge has none, and
// ErrStopped once link dropped the job (paired with another server, or
// unpaired on the Bridge's page).
func (p *Reporter) onLink() error {
	if p.r == nil {
		return nil
	}
	srv, current := p.link()
	switch {
	case srv == nil:
		return ErrStopped
	case !current:
		return ErrShareOffLink
	}
	return nil
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

// decodeProgress reads a stored progress object back (MarshalJSON's flat shape);
// one it can't read is the zero Progress.
func decodeProgress(raw json.RawMessage) Progress {
	type base Progress
	var p Progress
	var m map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, (*base)(&p)) != nil || json.Unmarshal(raw, &m) != nil {
		return Progress{}
	}
	for _, k := range []string{"step", "message", "done", "total"} {
		delete(m, k)
	}
	if len(m) > 0 {
		p.Extra = m
	}
	return p
}

// Reporter is how a Handler reports progress. It is safe for concurrent use.
// Progress only records: the job's reporting goroutine (loop) sends, so a
// handler never waits on the network.
type Reporter struct {
	r *Runner
	// ru is the job's run (nil for a LocalReporter): Yield and Reclaim move its
	// lane slot.
	ru     *run
	job    Job
	cancel context.CancelCauseFunc
	ctx    context.Context //nolint:containedctx // the job's own context, for its sends
	// nudge wakes loop when the step changed (nil for a LocalReporter).
	nudge chan struct{}
	// sendMu orders state snapshots and their HTTP attempts. A 429 releases
	// it before any wait so newer progress or a finish can go through.
	sendMu sync.Mutex

	// started: the handler runs (or ran): it may hold the link it started on,
	// so a new link to its server restarts it there (link, errRelinked).
	started atomic.Bool

	mu sync.Mutex
	// srv is the link the job last reported on (link): the one it was
	// launched on until the Bridge has another link to its server.
	srv      Server
	progress Progress
	// dirty: progress changed since it was last sent (and stored).
	dirty  bool
	sentAt time.Time
	// failing records a transient failure. Any successful report, including a
	// ConfirmLease or a Refresh, clears it. warned keeps subsequent failures
	// quiet until a report gets through (bridge#31).
	failing  bool
	warned   bool
	nextIdle time.Time // next report due if the job makes no progress
	// parked: the job reports needs_attention with parkMsg as its error, until
	// Unpark. Unparked, every report clears the error (one a park left, even
	// across a restart that came before the clear got through).
	parked  bool
	parkMsg string
	// carried: progress.Extra's "park" is one Progress kept from the progress
	// before it (a parked job's report that named no park), which Unpark drops.
	carried bool
	// path is the job's book path as the server's last answer gave it ("" until
	// one did): the server moves an open job with its book (a rename the scan
	// noticed), and the job hears it here.
	path string
	// shareOff: the claim or an answer since said share_off (ShareOff);
	// shareOffKept that this run wrote it to the store (keepShareOff;
	// shareOffWarned: a failed write was logged, so the next are quiet);
	// onShareOff is the handler's OnShareOff.
	shareOff       bool
	shareOffKept   bool
	shareOffWarned bool
	onShareOff     func()
	// localYielded is a LocalReporter's Yield, for Yielded (a handler's tests).
	localYielded bool
}

// LocalReporter returns a Reporter that records progress and sends nothing (a
// handler's tests).
func LocalReporter() *Reporter { return &Reporter{} }

// CancelContext returns a child of parent and a func that ends it as an
// admin's cancel does (Resumes is false after it): a handler's tests.
func CancelContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	return ctx, func() { cancel(errCancelRequested) }
}

// LoseContext returns a child of parent and a func that ends it as a lost
// claim does (Resumes is false, Lost true after it): a handler's tests.
func LoseContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	return ctx, func() { cancel(errLost) }
}

// DropContext returns a child of parent and a func that ends it as an unpair
// on the Bridge's page does (Resumes is false after it): a handler's tests.
func DropContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	return ctx, func() { cancel(errPairingEnded) }
}

// AbandonContext is the context the runner hands Abandon for a job whose claim
// was lost (lost true) or cancelled: a handler's tests.
func AbandonContext(parent context.Context, lost bool) context.Context {
	if lost {
		return context.WithValue(parent, abandonCauseKey{}, errLost)
	}
	return context.WithValue(parent, abandonCauseKey{}, errCancelRequested)
}

// Path is the job's book path as the server's last answer to a report gave it,
// "" when no answer carried one: a book the server saw move since the claim is
// at this path now.
func (p *Reporter) Path() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.path
}

// ShareOff reports whether the server told the job to keep its results local,
// filing nothing to the community (link.Job.ShareOff): its claim's answer or
// any answer to its reports since. It stays heard for the run: a share dropped
// is not taken up again if the server's module is turned back on.
func (p *Reporter) ShareOff() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shareOff
}

// OnShareOff has fn called the moment the job hears share_off (ShareOff): at
// once when it has already, then at every answer that carries it, so a
// handler that acts on it from there (its next look) need not wait for an idle
// poll. The store has the job's books local by then when its write worked
// (keepShareOff). fn runs on the goroutine that read the answer (the
// keepalive's, a ConfirmLease's, or a Refresh's: another job's, or the
// engine's contributing stage asking before it files), after the report's lock
// is let go and before a cancel the same answer carries ends the job's
// context: it must be quick, safe to run beside the handler and more than
// once, and must not report.
func (p *Reporter) OnShareOff(fn func()) {
	p.mu.Lock()
	p.onShareOff = fn
	heard := p.shareOff
	p.mu.Unlock()
	if heard && fn != nil {
		fn()
	}
}

// HearShareOff acts as an answer that carries share_off does (ShareOff,
// OnShareOff), without recording it: a handler's tests, on a LocalReporter.
func HearShareOff(p *Reporter) {
	p.mu.Lock()
	p.shareOff = true
	fn := p.onShareOff
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Last is the progress last recorded.
func (p *Reporter) Last() Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.progress
}

// Progress records the job's progress; a new step is sent at once, anything
// else within ProgressEvery. The same progress again (a handler reporting on
// every poll) changes nothing, so it is sent only with the keepalive. While the
// job is parked, a progress that names no park keeps the one before it (a job
// resumed after a restart reports its first steps before it hears its engine's
// park again): the server would hear a park without its code as a new one.
func (p *Reporter) Progress(pr Progress) {
	pr.Step, pr.Message = clip(pr.Step, maxStepText), clip(pr.Message, maxMessageText)
	p.mu.Lock()
	// A null park names none, as the server reads it (bridge.parkOf).
	named := pr.Extra["park"] != nil
	carried := false
	if park := p.progress.Extra["park"]; park != nil && p.parked && !named {
		pr.Extra = maps.Clone(pr.Extra)
		if pr.Extra == nil {
			pr.Extra = map[string]any{}
		}
		pr.Extra["park"], carried = park, true
	}
	p.carried = carried
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
		if p.carried {
			p.progress.Extra = maps.Clone(p.progress.Extra)
			delete(p.progress.Extra, "park")
			p.carried = false
		}
	}
	p.mu.Unlock()
	if was {
		p.wake()
	}
}

// Yield gives the job's lane slot back while it waits on something outside
// the Bridge for an unknown time (a parked librivox.fetch waiting for its
// library's mapping), so the lane admits another job meanwhile. The job keeps
// running and reporting; it holds no slot until Reclaim. Opt-in: a park alone
// keeps the slot. Nothing for a job that holds none (a LocalReporter only
// records it for Yielded).
func (p *Reporter) Yield() {
	if p.ru == nil {
		p.mu.Lock()
		p.localYielded = true
		p.mu.Unlock()
		return
	}
	r, ru := p.r, p.ru
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ru.admitted {
		return
	}
	// A stray's hold on its dispatcher (strayWaitingLocked) is for its first
	// wait only: it had its slot, so neither the yield nor the wait in Reclaim
	// keeps that dispatcher from claiming for the other lanes.
	ru.admitted, ru.yielded, ru.stray = false, true, false
	r.use[ru.lane]--
	r.wakeLocked()
}

// Reclaim takes a lane slot again after Yield before the job goes on: it
// queues first in line (no new claim asks for the lane meanwhile, though a
// claim already in flight keeps the slot it reserved: BR-CLAIM-HOLD) and waits
// for the job holding the slot to end. The context's error when it ended first; nil at
// once when the job didn't yield.
func (p *Reporter) Reclaim(ctx context.Context) error {
	if p.ru == nil {
		p.mu.Lock()
		p.localYielded = false
		p.mu.Unlock()
		return nil
	}
	r, ru := p.r, p.ru
	r.mu.Lock()
	if !ru.yielded {
		r.mu.Unlock()
		return nil
	}
	ru.yielded = false
	r.waiting[ru.lane]++
	r.wakeLocked()
	r.mu.Unlock()
	if !r.awaitAdmission(ctx, ru) {
		return ctx.Err()
	}
	return nil
}

// Yielded reports whether the job gave its slot back and hasn't reclaimed one
// (a LocalReporter records its Yield and Reclaim too, for a handler's tests).
func (p *Reporter) Yielded() bool {
	if p.ru == nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.localYielded
	}
	p.r.mu.Lock()
	defer p.r.mu.Unlock()
	return p.ru.yielded
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
// at nextIdle when nothing changed (renewing the lease).
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
			changed := p.dirty && time.Since(p.sentAt) >= p.r.opts.ProgressEvery
			idleDue := !time.Now().Before(p.nextIdle)
			p.mu.Unlock()
			if !changed && !idleDue {
				continue
			}
		}
		if p.offLink() {
			continue
		}
		if !p.send() {
			return
		}
	}
}

// link is the link the job reports on now, by the one rule (Runner.linkFor):
// the Bridge's current link while it reaches the job's server (a new pairing
// with it included: the job moves onto it), with current true; while the
// Bridge has no link (unpaired, refused, before a new link's first heartbeat),
// the one it last reported on, as nothing says yet that the job is another
// server's (a running job goes on renewing its lease there). nil once the
// Bridge is paired with another server: the job is dropped (errOtherServer
// ends its context) and reports nothing more. A job whose handler started on
// another link to its server is stopped (errRelinked) to run again on the new
// one: the handler's own calls may still use the old link, which the server
// revoked. nil too once the job's pairing was ended on the Bridge's page
// (Runner.Unpaired, or the app's pairing forgotten before that reaches the
// run: pairingGone, which ends its context): nothing more is sent to its
// server, a new pairing with it included. Not for a LocalReporter.
func (p *Reporter) link() (srv Server, current bool) {
	if p.ended() {
		return nil, false
	}
	if p.ru != nil && p.r.pairingGone(p.job) {
		// The app forgot the job's pairing (Unpaired reaches the run next),
		// or the Bridge was paired with another server since.
		p.cancel(p.r.dropCause(p.job.serverID))
		return nil, false
	}
	cur, other := p.r.linkFor(p.job.serverID)
	if other {
		p.cancel(errOtherServer)
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur != nil {
		if p.srv != cur && p.srv != nil && p.started.Load() {
			p.cancel(errRelinked)
		}
		p.srv = cur
	}
	return p.srv, cur != nil
}

// ended reports whether the job's pairing was ended on the Bridge's page
// (Runner.Unpaired).
func (p *Reporter) ended() bool { return p.ru != nil && p.ru.ended.Load() }

// offLink reports whether the job still waits for its first slot while the
// Bridge has no link to its server (unpaired, refused, or paired with another
// server, which drops it): its lease isn't renewed, so the server can hand the
// job out again rather than wait on a claim nothing here will run (bridge#13).
// A link to its server again (a new pairing with it included) renews it.
func (p *Reporter) offLink() bool {
	p.r.mu.Lock()
	waiting := !p.ru.admitted && !p.ru.yielded
	p.r.mu.Unlock()
	return waiting && !p.OnCurrentLink()
}

// send reports the job's state (running, renewing the lease, or needs_attention
// while parked, which has none) with its current progress and acts on the
// answer: a cancel request or a lost claim ends the job's context. It reports
// whether the job goes on (false too for one link dropped).
func (p *Reporter) send() bool {
	goOn, srv, got, stop, err := p.sendLocked()
	if srv != nil {
		p.acted(srv, got, stop, err)
	}
	return goOn
}

// sendLocked is send under sendMu; it returns the link it reported on (nil for
// a job link dropped: nothing was sent), the answer and the cause that stops
// the job (answer) too, for acted (which runs after it, outside sendMu: heard
// writes the store).
func (p *Reporter) sendLocked() (goOn bool, srv Server, got *link.Job, stop, err error) {
	ctx := p.ctx
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	if srv, _ = p.link(); srv == nil {
		return false, nil, nil, nil, nil
	}
	p.mu.Lock()
	pr, dirty := p.progress, p.dirty
	parked, parkMsg := p.parked, p.parkMsg
	p.sentAt, p.dirty = time.Now(), false
	p.mu.Unlock()
	u, raw := p.updateOf(pr, parked, parkMsg)
	if dirty {
		if err := p.r.store.SetServerJobReport(ctx, p.job.ID, p.job.Attempt, raw, parkMsg); err != nil && ctx.Err() == nil {
			p.r.log.Warn("bridge: recording progress failed", "job", p.job.ID, "err", err)
		}
	}
	got, err = srv.UpdateJob(link.SingleAttempt(ctx), p.job.ID, u)
	if stop = p.answer(got, err, parked); stop != nil {
		return false, srv, got, stop, err
	}
	switch {
	case err == nil:
		p.reached()
		return true, srv, got, nil, nil
	case ctx.Err() != nil:
		return false, srv, got, nil, err
	default:
		p.failed(err)
		return true, srv, got, nil, err
	}
}

// updateOf is the report of progress pr, parked (with parkMsg) or not. The
// error always goes out: the park's message while parked, else "", which clears
// the one a park left.
func (p *Reporter) updateOf(pr Progress, parked bool, parkMsg string) (link.Update, json.RawMessage) {
	if parked {
		pr = withParkMessage(pr, parkMsg)
	}
	raw, _ := json.Marshal(pr)
	state := link.StateRunning
	if parked {
		state = link.StateNeedsAttention
	}
	u := link.Update{Attempt: p.job.Attempt, State: &state, Error: &parkMsg}
	if pr.Step != "" {
		u.Progress = raw
	}
	return u, raw
}

// withParkMessage puts a parked job's message for people beside its park in pr
// (progress park.message, bridge#28), so the page reads it there rather than
// taking the code off the job's error: msg without the "<code>: " the engine
// may lead it with, none when that leaves only the code (a message an earlier
// report left there, a park carried over or restored after a restart, goes). A
// progress naming no park is left as it is; pr's own maps are never changed.
func withParkMessage(pr Progress, msg string) Progress {
	named, ok := pr.Extra["park"].(map[string]any)
	if !ok {
		return pr
	}
	code, _ := named["code"].(string)
	if code != "" {
		msg = strings.TrimSpace(strings.TrimPrefix(msg, code+":"))
	}
	none := msg == "" || msg == code
	if _, had := named["message"]; none && !had {
		return pr
	}
	park := maps.Clone(named)
	if none {
		delete(park, "message")
	} else {
		park["message"] = msg
	}
	pr.Extra = maps.Clone(pr.Extra)
	pr.Extra["park"] = park
	return pr
}

// failed schedules the next idle report from the time of the refusal. A 429
// may extend ReportRetry by asking the Bridge to wait, up to one minute.
// The first failure warns and later failures log at debug until one gets through.
func (p *Reporter) failed(err error) {
	p.mu.Lock()
	p.failing = unreachable(err)
	delay := p.r.opts.Keepalive
	if p.failing {
		delay = p.r.opts.ReportRetry
	}
	p.nextIdle = time.Now().Add(retryAfter(delay, err))
	first := !p.warned
	p.warned = true
	p.mu.Unlock()
	if first {
		p.r.log.Warn("bridge: reporting progress failed", "job", p.job.ID, "err", err)
	} else {
		p.r.log.Debug("bridge: reporting progress failed again", "job", p.job.ID, "err", err)
	}
}

// retryAfter stretches delay to a 429's Retry-After, at most one minute (a
// claim's pause and a report's alike).
func retryAfter(delay time.Duration, err error) time.Duration {
	if busy, ok := link.RateLimited(err); ok {
		return min(max(delay, busy.RetryAfter), time.Minute)
	}
	return delay
}

// unreached reports whether the job's last report didn't reach the server
// (failed: no answer, a 5xx, a 408 or a 429), until one does.
func (p *Reporter) unreached() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failing
}

// reached notes a report that got through: the next goes after the Keepalive,
// and a failure logged before it gets its recovery line.
func (p *Reporter) reached() {
	p.mu.Lock()
	warned := p.warned
	p.failing, p.warned = false, false
	p.nextIdle = time.Now().Add(p.r.opts.Keepalive)
	p.mu.Unlock()
	if warned {
		p.r.log.Info("bridge: reporting progress again", "job", p.job.ID)
	}
}

// unreachable reports whether a failed report may get through if sent again
// soon: no answer at all (the network, a timeout), a 5xx, a 408 or a 429. Any
// other 4xx (a bad update, a job the server no longer has) is refused the same
// way however soon it goes.
func unreachable(err error) bool {
	var refused *link.APIError
	return !errors.As(err, &refused) || refused.Transient()
}

// answer reads the server's answer to an update: the job follows a moved path,
// and a cancel request, a lost claim or a refused token is the cause it returns
// (nil while the job goes on), which acted ends the job's context with. Its
// share_off is heard's. parked: the update reported the job parked (or, sending
// no error of its own, went out while it was).
func (p *Reporter) answer(got *link.Job, err error, parked bool) (stop error) {
	if err == nil && got.Path != nil && *got.Path != "" {
		p.mu.Lock()
		p.path = *got.Path
		p.mu.Unlock()
	}
	switch {
	case err == nil && got.CancelRequested:
		// A cancel asked for: by whom, from the job's error (cancelCause).
		// A parked report's answer says neither: the job's error is then
		// the park's message that report sent (it replaces the server's
		// reason too), as the server cleaned it, so it is never compared.
		if parked {
			return errCancelRequested
		}
		return cancelCause(got.Error)
	case errors.Is(err, link.ErrCancelledHere):
		// Already cancelled on the server, by an admin or by the server
		// itself: its answer doesn't say which.
		return errCancelRequested
	case errors.Is(err, link.ErrNotHeld):
		return errLost
	case errors.Is(err, link.ErrUnauthorized):
		return errUnpaired
	}
	return nil
}

// acted acts on an answer srv gave once answer has read it, outside sendMu:
// heard first, then stop (answer's cause, nil for none) ends the job's
// context, so a handler's OnShareOff runs for an answer that carries share_off
// and a cancel while the job's context still works (the Reporter's own write,
// keepShareOff, doesn't need it). A refused token is srv's (onUnauthorized).
// It reports whether the answer stopped the job.
func (p *Reporter) acted(srv Server, got *link.Job, stop, err error) bool {
	p.heard(got, err)
	if stop == nil {
		return false
	}
	p.cancel(stop)
	if errors.Is(stop, errUnpaired) {
		p.r.onUnauthorized(srv)
	}
	return true
}

// heard acts on an answer's share_off, after answer and outside sendMu (the
// write below and the handler's OnShareOff touch the store, and a report must
// not wait on them): the job keeps it for the run, the store keeps it (and the
// job's books local) for a restart, then the handler's func runs. A write that
// failed is tried again at every report, answered or not (a Bridge stopped
// while the server is out of reach must not lose it), and logged once a job.
func (p *Reporter) heard(got *link.Job, err error) {
	said := err == nil && got != nil && got.ShareOff
	p.mu.Lock()
	p.shareOff = p.shareOff || said
	fn := p.onShareOff
	p.mu.Unlock()
	p.keepShareOff()
	if said && fn != nil {
		fn()
	}
}

// keepShareOff writes the job's share_off to the store
// (store.DB.SetServerJobShareOff, which turns the job's engine books local in
// the same write) once the run heard it and hasn't written it yet, so the
// contributing stage files nothing for those books whether or not a run of the
// job is up, and a job resumed before the server answers again keeps its
// results local all the same. The first failure is a warning, the rest debug.
func (p *Reporter) keepShareOff() {
	p.mu.Lock()
	keep := p.shareOff && !p.shareOffKept
	p.mu.Unlock()
	if !keep {
		return
	}
	err := p.r.store.SetServerJobShareOff(context.WithoutCancel(p.ctx), p.job.ID, p.job.Attempt)
	p.mu.Lock()
	first := !p.shareOffWarned
	if err == nil {
		p.shareOffKept = true
	} else {
		p.shareOffWarned = true
	}
	p.mu.Unlock()
	switch {
	case err == nil:
	case first:
		p.r.log.Warn("bridge: recording a job's share_off failed", "job", p.job.ID, "err", err)
	default:
		p.r.log.Debug("bridge: recording a job's share_off failed again", "job", p.job.ID, "err", err)
	}
}

// ConfirmLease proves ownership immediately before a filesystem publication. It
// acts on the answer as the keepalive does, and also refuses a moved path: the
// caller is about to write to the one it claimed.
func (p *Reporter) ConfirmLease(ctx context.Context) error {
	if p.r == nil {
		return nil
	}
	srv, _ := p.link()
	if srv == nil {
		// Dropped: nothing is published for it.
		if p.ended() {
			return errPairingEnded
		}
		return errOtherServer
	}
	state := link.StateRunning
	got, err := srv.UpdateJob(ctx, p.job.ID, link.Update{Attempt: p.job.Attempt, State: &state})
	// It sends no error, so a parked job's stays its park's message.
	parked, _ := p.parkState()
	if p.acted(srv, got, p.answer(got, err, parked), err) {
		if err != nil {
			return err
		}
		return context.Canceled
	}
	if err != nil {
		return err
	}
	p.reached()
	if p.job.Path != nil && got.Path != nil && *p.job.Path != *got.Path {
		return link.ErrNotHeld
	}
	return ctx.Err()
}

// ErrStopped is Refresh's answer for a job the server's answer stopped: a cancel
// it holds for the job, a lost claim or a refused token. The job's context has
// ended; its run is on its way out.
var ErrStopped = errors.New("the server stopped the job")

// Refresh reports the job now, as its next keepalive would (parked or not, with
// its progress), and acts on the answer as that report would. Another job asks
// for it when this one stands in its way: the server may have cancelled it since
// its last report, and would otherwise say so only at the next keepalive; the
// contributing stage asks it just before it files, for a share_off since. nil
// when the server holds the job open; ErrStopped when the answer stopped it; the
// error when the report didn't get through, which counts as any failed report
// (failing, unless the caller's own context ended): a check that doesn't ask
// (Runner.ShareOff) then says the job can't be reached. A retry takes a new
// snapshot after the wait, as another report can go through meanwhile.
func (p *Reporter) Refresh(ctx context.Context) error {
	if p.r == nil {
		return nil
	}
	return link.Retry(ctx, p.refreshOnce)
}

func (p *Reporter) refreshOnce(ctx context.Context) error {
	srv, got, stop, err := func() (Server, *link.Job, error, error) {
		p.sendMu.Lock()
		defer p.sendMu.Unlock()
		srv, _ := p.link()
		if srv == nil {
			return nil, nil, nil, nil
		}
		p.mu.Lock()
		pr, parked, parkMsg := p.progress, p.parked, p.parkMsg
		p.mu.Unlock()
		u, _ := p.updateOf(pr, parked, parkMsg)
		got, err := srv.UpdateJob(ctx, p.job.ID, u)
		stop := p.answer(got, err, parked)
		// Noted under sendMu, as send does, so an older report's outcome never
		// lands over a newer one's.
		switch {
		case stop != nil:
		case err == nil:
			p.reached()
		case ctx.Err() == nil:
			p.failed(err)
		}
		return srv, got, stop, err
	}()
	if srv == nil || p.acted(srv, got, stop, err) {
		return ErrStopped // srv nil: link dropped the job (another server, an unpair)
	}
	return err
}

// sendNow reports the job now, as its next keepalive would (parked or not, with
// its progress), and returns the answer with the cause it carries (answer),
// without acting on it.
func (p *Reporter) sendNow(ctx context.Context) (got *link.Job, stop, err error) {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	p.mu.Lock()
	pr, parked, parkMsg := p.progress, p.parked, p.parkMsg
	p.mu.Unlock()
	u, _ := p.updateOf(pr, parked, parkMsg)
	got, err = p.srv.UpdateJob(link.SingleAttempt(ctx), p.job.ID, u)
	return got, p.answer(got, err, parked), err
}

// heldCancelWait bounds heldCancel's ask, so a server that doesn't answer holds
// a failed job's end (and its lane's slot) only briefly.
const heldCancelWait = 5 * time.Second

// heldCancel asks the server, before a failed job's end is reported, whether
// it holds a cancel the job never heard (one asked for since its last report,
// which a failure, the stop itself perhaps, beat to the next keepalive). It
// returns the cancel's cause, which says by whom as a keepalive's would, or nil:
// no cancel, or no answer within heldCancelWait (the failure stands; a cancel
// the server holds or ends it with all the same is in its end report's answer,
// stored cancelled or a 409 cancelled, which report reads).
func (p *Reporter) heldCancel(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, heldCancelWait)
	defer cancel()
	if _, stop, _ := p.sendNow(ctx); errors.Is(stop, errCancelRequested) {
		return stop
	}
	return nil
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
