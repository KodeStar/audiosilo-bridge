package jobs

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// priorityServer is optional: older links keep the ordinary Claim contract.
type priorityServer interface {
	SupportsClaimMinPriority() bool
	WithdrawClaimMinPriority()
	ClaimMinPriority(context.Context, []string, time.Duration, int32) (*link.Job, error)
}

func supportsPriority(srv Server) bool {
	s, ok := srv.(priorityServer)
	return ok && s.SupportsClaimMinPriority()
}

func interactive(h Handler) bool {
	hook, ok := h.(InteractiveHandler)
	return ok && hook.Interactive()
}

// capacities is called under mu. A legacy pairing keeps both download slots.
func (r *Runner) capacities(srv Server) map[string]int {
	caps := maps.Clone(r.opts.Lanes)
	if !supportsPriority(srv) {
		caps[LaneDownload] += caps[LaneInteractive]
		caps[LaneInteractive] = 0
	}
	return caps
}

// A claim reserves one possible admission in every lane it requests. There is
// one long poll per dispatcher. A probe for a lane the poll omitted reserves
// its own admission. A refresh retains admission until the old call
// has returned, including a job racing its cancellation.
type claimRequest struct {
	srv         Server
	interactive bool
	kinds       []string
	lanes       []string
	capacities  map[string]int
	held        bool
	partial     bool // a lane with handlers was temporarily unavailable
	// fallback keeps the original claim identity but uses ordinary admission
	// when the server returned a job outside this request's eligibility.
	fallback bool
}

func (q *claimRequest) holds(lane string) int {
	if q != nil && q.held && slices.Contains(q.lanes, lane) {
		return 1
	}
	return 0
}

// roomLocked reports whether lane has a free slot under caps (capacities(srv)).
func (r *Runner) roomLocked(caps map[string]int, lane string, own *claimRequest) bool {
	used := func(l string) int { return r.use[l] + r.reserved[l] - own.holds(l) }
	if used(lane) >= caps[lane] {
		return false
	}
	// A capability change cannot admit a third enclosure while an earlier
	// pairing's ordinary or reserved jobs are still finishing.
	if lane == LaneDownload || lane == LaneInteractive {
		return used(LaneDownload)+used(LaneInteractive) < caps[LaneDownload]+caps[LaneInteractive]
	}
	return true
}

// An uncovered lane that answered empty is asked again recheckEvery later, at
// most maxRechecks times beside one long poll: work queued there since (an
// OPML import's next follows, an admin's check) would otherwise wait for the
// poll to return, up to its whole wait (link.MaxWait, 25 s). A free lane with
// nothing queued costs at most maxRechecks more requests per long poll.
const (
	recheckEvery = 4 * time.Second
	maxRechecks  = 3
)

// A narrow long poll keeps its reservations until the server answers. Beside
// it, probe only the lanes it missed. Remember an empty answer so unrelated
// wakes (including heartbeats and our own unreserve) issue no more requests
// until its bounded recheck.
type laneProbe struct {
	pending    *claimRequest
	emptyLanes []string
	recheck    *time.Timer // armed by an empty answer while rechecks remain
	rechecks   int
}

func (p *laneProbe) needed(q *claimRequest) bool {
	return sameTarget(q, p.pending) &&
		slices.ContainsFunc(q.lanes, func(lane string) bool { return !slices.Contains(p.emptyLanes, lane) })
}

// empty remembers lanes that answered empty and arms their recheck.
func (p *laneProbe) empty(lanes []string) {
	p.emptyLanes = lanes
	if p.rechecks < maxRechecks {
		p.rechecks++
		p.stop()
		p.recheck = time.NewTimer(recheckEvery)
	}
}

// keep forgets the empty lanes not in lanes: one that lost its room is asked
// again once it frees, and a snapshot that wasn't asked (another target)
// marks no lane empty.
func (p *laneProbe) keep(lanes []string) {
	p.emptyLanes = slices.DeleteFunc(slices.Clone(p.emptyLanes), func(lane string) bool { return !slices.Contains(lanes, lane) })
}

// due is the armed recheck while an empty lane waits for it, else nil.
func (p *laneProbe) due() <-chan time.Time {
	if p.recheck == nil || len(p.emptyLanes) == 0 {
		return nil
	}
	return p.recheck.C
}

func (p *laneProbe) stop() {
	if p.recheck != nil {
		p.recheck.Stop()
		p.recheck = nil
	}
}

// request snapshots eligibility and optionally reserves it under the same lock.
// own is excluded when checking whether an outstanding claim is still valid.
// requestLanes with a probe snapshots only the lanes its pending poll omitted;
// own and probe are never both set.
func (r *Runner) request(reserve, hold bool, own *claimRequest) (*claimRequest, <-chan struct{}) {
	return r.requestLanes(reserve, hold, own, nil)
}

func (r *Runner) requestLanes(reserve, hold bool, own *claimRequest, probe *laneProbe) (*claimRequest, <-chan struct{}) {
	// Read the external pairing without mu, but detect a wake during that read.
	// Otherwise a re-pair between server() and locking could be missed for a
	// whole long poll. The app changes its pairing before calling Wake.
	var srv Server
	for {
		changed := r.wakeChan()
		srv = r.server()
		r.mu.Lock()
		if changed == r.changed {
			break
		}
		r.mu.Unlock()
	}
	defer r.mu.Unlock()
	q := &claimRequest{srv: srv, interactive: reserve, capacities: r.capacities(srv)}
	if srv != nil && (!reserve || supportsPriority(srv)) && !r.strayWaitingLocked(reserve, probe != nil) {
		for kind, h := range r.handlers {
			lane := h.Lane()
			if reserve {
				if !interactive(h) {
					continue
				}
				lane = LaneInteractive
			}
			if probe != nil && slices.Contains(probe.pending.lanes, lane) {
				continue
			}
			if r.waiting[lane] == 0 && r.roomLocked(q.capacities, lane, own) {
				q.kinds = append(q.kinds, kind)
				if !slices.Contains(q.lanes, lane) {
					q.lanes = append(q.lanes, lane)
				}
			} else if q.capacities[lane] > 0 {
				q.partial = true
			}
		}
	}
	sort.Strings(q.kinds)
	if hold && len(q.kinds) != 0 && (probe == nil || probe.needed(q)) {
		for _, lane := range q.lanes {
			r.reserved[lane]++
		}
		q.held = true
	}
	return q, r.changed
}

// strayWaitingLocked reports whether the jobs this dispatcher took outside their
// admission, still waiting for their lane, stop it claiming. The reserve stops at
// one: a server that keeps advertising a filter it ignores holds one such waiter,
// not its queue. The ordinary dispatcher stops at two (bridge#13, #26): one waits
// in its own lane, which no claim asks for while a job waits there, and the other
// lanes go on claiming (it backs off after the answer); a second means the server
// keeps ignoring the kinds asked for, so it claims nothing until one has a slot.
// A probe can run beside an ordinary claim already in flight, so it stops at one.
func (r *Runner) strayWaitingLocked(reserve, probing bool) bool {
	limit := 2
	if reserve || probing {
		limit = 1
	}
	for _, ru := range r.running {
		if ru.stray && !ru.admitted && ru.strayReserve == reserve {
			if limit--; limit == 0 {
				return true
			}
		}
	}
	return false
}

func (r *Runner) unreserveLocked(q *claimRequest) {
	if q.held {
		for _, lane := range q.lanes {
			r.reserved[lane]--
		}
		q.held = false
		r.wakeLocked()
	}
}

func (r *Runner) unreserve(q *claimRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unreserveLocked(q)
}

// sameRequest reports whether a claim asked now would ask what a asks: the
// same link (so the same server: a link knows its server's id), the same
// kinds and capacities. A pause between claims (pauseClaims) ends early when
// it no longer does.
func sameRequest(a, b *claimRequest) bool {
	return sameTarget(a, b) && slices.Equal(a.kinds, b.kinds)
}

// sameTarget compares the link and the pool used by claims of one dispatcher.
// A change to either refreshes the poll and any probe on the old target.
func sameTarget(a, b *claimRequest) bool {
	return a.srv == b.srv && maps.Equal(a.capacities, b.capacities)
}

type claimResult struct {
	job *link.Job
	err error
}

// poll cancels and joins the call on a change of target or shutdown. Lane
// changes leave it open so a proxy that hides a hang-up cannot take a job
// whose answer the Bridge has already abandoned.
func (r *Runner) poll(ctx context.Context, q *claimRequest, changed <-chan struct{}, wait time.Duration) (claimResult, bool) {
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan claimResult, 1)
	go func() {
		var result claimResult
		if q.interactive {
			result.job, result.err = q.srv.(priorityServer).ClaimMinPriority(pollCtx, q.kinds, wait, InteractivePriority)
		} else {
			result.job, result.err = q.srv.Claim(pollCtx, q.kinds, wait)
		}
		done <- result
	}()
	for {
		select {
		case result := <-done:
			return result, false
		case <-ctx.Done():
			cancel()
			return <-done, true
		case <-changed:
			next, wake := r.request(q.interactive, false, q)
			if !sameTarget(q, next) {
				cancel()
				return <-done, true
			}
			changed = wake
		}
	}
}

// finishClaim handles every answer under the link and reservations that asked
// for it, including a job returned as the request was being cancelled.
func (r *Runner) finishClaim(ctx context.Context, q *claimRequest, job *link.Job, wg *sync.WaitGroup) {
	if job != nil {
		if !slices.Contains(q.kinds, job.Kind) || (q.interactive && job.Priority < int(InteractivePriority)) {
			// The lease is ours even if the server ignored the filter. Drop
			// the reserved admission and wait for the kind's ordinary lane.
			if q.interactive {
				q.srv.(priorityServer).WithdrawClaimMinPriority()
			}
			// Releasing the reservation (in launch, or below) wakes the dispatchers.
			q.fallback = true
		}
		r.start(ctx, q.srv, jobOf(job), wg, q)
	}
	r.unreserve(q)
}

// claimFreedLanes makes at most one zero-wait request at a time alongside
// the long poll. Stop taking new jobs when the poll returns. An outstanding
// probe can still bring a lease, so let it finish under its original target.
func (r *Runner) claimFreedLanes(ctx context.Context, pending *claimRequest, finished <-chan struct{}, wg *sync.WaitGroup) {
	probe := laneProbe{pending: pending}
	defer probe.stop()
	for ctx.Err() == nil {
		q, changed := r.requestLanes(pending.interactive, true, nil, &probe)
		select {
		case <-finished:
			r.unreserve(q)
			return
		default:
		}
		if !q.held {
			probe.keep(q.lanes)
			select {
			case <-finished:
				return
			case <-ctx.Done():
				return
			case <-changed:
			case <-probe.due():
				probe.recheck, probe.emptyLanes = nil, nil
			}
			continue
		}
		result, refreshed := r.poll(ctx, q, changed, 0)
		r.finishClaim(ctx, q, result.job, wg)
		switch {
		case refreshed:
		case errors.Is(result.err, link.ErrUnauthorized):
			// The idle long poll may not see the 401 until its wait ends.
			r.onUnauthorized(q.srv)
		case result.err != nil:
			// The long poll's own answer retries and logs a lasting failure;
			// until then a freed lane waits for it, as before these checks.
			r.log.Debug("bridge: checking a freed lane failed", "err", result.err)
		}
		if refreshed || result.err != nil || q.fallback {
			return // leave retries to the regular dispatcher
		}
		if result.job != nil {
			probe.emptyLanes = nil // the lanes may hold more
		} else {
			probe.empty(q.lanes)
		}
	}
}

func (r *Runner) dispatch(ctx context.Context, reserve bool, wg *sync.WaitGroup) {
	backoff := time.Second
	for ctx.Err() == nil {
		q, changed := r.request(reserve, true, nil)
		if len(q.kinds) == 0 {
			select {
			case <-ctx.Done():
			case <-changed:
			}
			continue
		}
		var probes sync.WaitGroup
		finished := make(chan struct{})
		if q.partial {
			probes.Go(func() { r.claimFreedLanes(ctx, q, finished, wg) })
		}
		start := time.Now()
		result, refreshed := r.poll(ctx, q, changed, r.opts.Wait)
		close(finished)
		r.finishClaim(ctx, q, result.job, wg)
		probes.Wait()
		if ctx.Err() != nil {
			return
		}
		if q.fallback && !q.interactive {
			// A server ignoring the kinds asked for (no negotiation to withdraw)
			// would otherwise hand out its queue as fast as it answers.
			r.pauseClaims(ctx, q, backoff)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		// A refresh is a scheduling event, not a failed network attempt.
		if refreshed || result.job != nil {
			backoff = time.Second
			continue
		}
		switch {
		case q.interactive && !supportsPriority(q.srv):
			// Negotiation was withdrawn after the request; the next one sees it.
		case errors.Is(result.err, link.ErrUnauthorized):
			r.onUnauthorized(q.srv)
		case errors.Is(result.err, link.ErrNoHeartbeat):
			r.pauseClaims(ctx, q, 2*time.Second)
		case result.err != nil:
			delay := retryAfter(backoff, result.err)
			r.log.Warn("bridge: claiming a job failed", "err", result.err, "retry_in", delay)
			r.pauseClaims(ctx, q, delay)
			backoff = min(backoff*2, time.Minute)
		default:
			backoff = time.Second
			if left := time.Second - time.Since(start); left > 0 {
				r.pauseClaims(ctx, q, left)
			}
		}
	}
}

func (r *Runner) pauseClaims(ctx context.Context, previous *claimRequest, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		next, changed := r.request(previous.interactive, false, nil)
		if !sameRequest(previous, next) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-timer.C:
			return
		}
	}
}

// Recovery, re-pair and out-of-admission claims use the ordinary lane, but a
// resumed job of interactive priority waits for the reserve while the pairing
// negotiates it (launch; rows from before the Bridge kept a job's priority read
// 0). A waiting job renews its lease through execute's reporter (only while the
// Bridge has a link to its server: Reporter.offLink) and takes a slot before any
// new claims for that lane. It is admitted only on such a link (a pairing with
// its server again included) and dropped once the Bridge is paired with
// another server (linkFor).
func (r *Runner) awaitAdmission(ctx context.Context, ru *run) bool {
	for ctx.Err() == nil {
		// The wake channel is taken before the pairing is read (as in request),
		// so a pairing or heartbeat between the two is not missed.
		r.mu.Lock()
		if ru.admitted {
			r.mu.Unlock()
			return true
		}
		changed := r.changed
		r.mu.Unlock()
		srv, other := r.linkFor(ru.job.serverID)
		if other {
			// Paired with another server since the job was launched: its claim
			// means nothing there, so it never runs (nor renews its lease).
			ru.cancel(errOtherServer)
			return false
		}
		r.mu.Lock()
		if srv != nil && ru.lane == LaneInteractive && !supportsPriority(srv) {
			// The reserve went (negotiation withdrawn) while a resumed job
			// waited for it: it waits in its kind's ordinary lane instead.
			r.waiting[ru.lane]--
			ru.lane = r.handlers[ru.job.Kind].Lane()
			r.waiting[ru.lane]++
		}
		if srv != nil && r.roomLocked(r.capacities(srv), ru.lane, nil) {
			r.waiting[ru.lane]--
			r.use[ru.lane]++
			ru.admitted = true
			r.wakeLocked()
			r.mu.Unlock()
			return true
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
	}
	return false
}
