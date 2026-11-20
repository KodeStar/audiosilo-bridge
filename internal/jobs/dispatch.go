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
// one ordinary dispatcher and one reserved dispatcher, never unbounded waiters
// holding newly claimed jobs. A refresh retains admission until the old call
// has returned, including a job racing its cancellation.
type claimRequest struct {
	srv         Server
	interactive bool
	kinds       []string
	lanes       []string
	capacities  map[string]int
	held        bool
	// leftOut: the light lane was left out (full, or a job waits there), so
	// the claim may wait briefly (leftOutWait): the room a quick job frees
	// there is asked for soon, not after a whole long poll.
	leftOut bool
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

// request snapshots eligibility and optionally reserves it under the same lock.
// own is excluded when checking whether an outstanding claim is still valid.
func (r *Runner) request(reserve, hold bool, own *claimRequest) (*claimRequest, <-chan struct{}) {
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
	if srv != nil && (!reserve || supportsPriority(srv)) && !r.strayWaitingLocked(reserve) {
		for kind, h := range r.handlers {
			lane := h.Lane()
			if reserve {
				if !interactive(h) {
					continue
				}
				lane = LaneInteractive
			}
			if r.waiting[lane] == 0 && r.roomLocked(q.capacities, lane, own) {
				q.kinds = append(q.kinds, kind)
				if !slices.Contains(q.lanes, lane) {
					q.lanes = append(q.lanes, lane)
				}
			} else if lane == LaneLight {
				q.leftOut = true
			}
		}
	}
	sort.Strings(q.kinds)
	if hold && len(q.kinds) != 0 {
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
func (r *Runner) strayWaitingLocked(reserve bool) bool {
	limit := 2
	if reserve {
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

// sameTarget reports whether an in-flight claim a still asks the right link
// for the right pool: the same link and the same capacities (a capability
// change: the reserve's negotiation withdrawn folds the interactive lane into
// the download one). b is always asked for a's dispatcher (the same
// interactive). Its kinds are left out: they change whenever a lane slot frees
// or fills, and the claim's own reservation keeps a slot for what it may bring
// in each lane it asks for, so it is let finish rather than cancelled
// (BR-CLAIM-HOLD); one that left the light lane out waits only briefly
// (leftOutWait), so a slot freeing there is asked for soon. A cancelled long
// poll a reverse proxy doesn't pass on as a hang-up still hands the job out,
// and the answer is lost until the job's 2-minute lease lapses.
func sameTarget(a, b *claimRequest) bool {
	return a.srv == b.srv && maps.Equal(a.capacities, b.capacities)
}

// leftOutWait is the long poll's wait while the claim leaves the light lane
// out (claimRequest.leftOut). A lane slot freeing doesn't end the claim in
// flight (sameTarget), so with the whole wait (link.MaxWait, 25 s) a queue of
// quick jobs, an OPML import's follows, would drain at a lane's worth per
// long poll. The download and pipeline lanes hold long jobs: a claim leaving
// them out waits the whole poll. maxShortPolls such short waits in a row that
// claim nothing go back to the whole wait until the claim asks for something
// else (a lane's room changed), so a light lane held by slow jobs doesn't
// keep the server answering a claim every few seconds.
const (
	leftOutWait   = 4 * time.Second
	maxShortPolls = 3
)

type claimResult struct {
	job *link.Job
	err error
}

// poll joins the call before returning, so a refreshed request can never leave
// a second old claim able to take a job behind the dispatcher's back. Only a
// changed target (sameTarget: the pairing, the negotiation, the capacities)
// or the runner's stop cancels the call; a lane slot freeing or filling lets
// it finish, which is soon when the claim left the light lane out (wait:
// leftOutWait).
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

func (r *Runner) dispatch(ctx context.Context, reserve bool, wg *sync.WaitGroup) {
	backoff := time.Second
	// shorts: the short waits in a row (leftOutWait) that claimed nothing for
	// the request asked last (last); maxShortPolls of them end short waits
	// until the request changes.
	var last *claimRequest
	shorts := 0
	for ctx.Err() == nil {
		q, changed := r.request(reserve, true, nil)
		if len(q.kinds) == 0 {
			select {
			case <-ctx.Done():
			case <-changed:
			}
			continue
		}
		if last == nil || !sameRequest(last, q) {
			shorts = 0
		}
		last = q
		wait := r.opts.Wait
		if q.leftOut && shorts < maxShortPolls {
			wait = min(wait, leftOutWait)
			shorts++
		}
		start := time.Now()
		result, refreshed := r.poll(ctx, q, changed, wait)
		if result.job != nil {
			shorts = 0
			if !slices.Contains(q.kinds, result.job.Kind) || (q.interactive && result.job.Priority < int(InteractivePriority)) {
				// The lease is ours even if the server ignored the filter. Drop
				// the reserved admission and wait for the kind's ordinary lane.
				if q.interactive {
					q.srv.(priorityServer).WithdrawClaimMinPriority()
				}
				// Releasing the reservation (in launch, or below) wakes the dispatchers.
				q.fallback = true
			}
			r.start(ctx, q.srv, jobOf(result.job), wg, q)
		}
		r.unreserve(q)
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
			delay := backoff
			if busy, limited := link.RateLimited(result.err); limited {
				delay = min(max(delay, busy.RetryAfter), time.Minute)
			}
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
