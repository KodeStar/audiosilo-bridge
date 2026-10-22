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
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// priorityServer is optional: older links keep the ordinary Claim contract.
type priorityServer interface {
	SupportsClaimMinPriority() bool
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

// setCapacity changes a pool's capacity and refreshes affected claims.
func (r *Runner) setCapacity(lane string, capacity int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	capacity = max(0, capacity)
	if r.opts.Lanes[lane] != capacity {
		r.opts.Lanes[lane] = capacity
		r.wakeLocked()
	}
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
	serverID    string
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
	var serverID string
	for {
		changed := r.wakeChan()
		srv, serverID = r.server(), r.opts.ServerID()
		r.mu.Lock()
		if changed == r.changed {
			break
		}
		r.mu.Unlock()
	}
	defer r.mu.Unlock()
	q := &claimRequest{srv: srv, interactive: reserve, capacities: r.capacities(srv), serverID: serverID}
	if srv != nil && (!reserve || supportsPriority(srv)) {
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

func sameRequest(a, b *claimRequest) bool {
	return a.srv == b.srv && a.serverID == b.serverID && a.interactive == b.interactive && slices.Equal(a.kinds, b.kinds) && maps.Equal(a.capacities, b.capacities)
}

type claimResult struct {
	job *link.Job
	err error
}

// poll joins the call before returning, so a refreshed request can never leave
// a second old claim able to take a job behind the dispatcher's back.
func (r *Runner) poll(ctx context.Context, q *claimRequest, changed <-chan struct{}) (claimResult, bool) {
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan claimResult, 1)
	go func() {
		var result claimResult
		if q.interactive {
			result.job, result.err = q.srv.(priorityServer).ClaimMinPriority(pollCtx, q.kinds, r.opts.Wait, InteractivePriority)
		} else {
			result.job, result.err = q.srv.Claim(pollCtx, q.kinds, r.opts.Wait)
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
			if !sameRequest(q, next) {
				cancel()
				return <-done, true
			}
			changed = wake
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
		start := time.Now()
		result, refreshed := r.poll(ctx, q, changed)
		if result.job != nil {
			if slices.Contains(q.kinds, result.job.Kind) && (!q.interactive || result.job.Priority >= int(InteractivePriority)) {
				r.start(ctx, q.srv, jobOf(result.job), wg, q)
			} else {
				r.rejectClaim(ctx, q, result.job)
			}
		}
		r.unreserve(q)
		if ctx.Err() != nil {
			return
		}
		// A refresh is a scheduling event, not a failed network attempt.
		if refreshed || result.job != nil {
			backoff = time.Second
			continue
		}
		switch {
		case errors.Is(result.err, link.ErrUnauthorized):
			r.onUnauthorized(q.srv)
		case errors.Is(result.err, link.ErrNoHeartbeat):
			r.pauseClaims(ctx, q, 2*time.Second)
		case result.err != nil:
			r.log.Warn("bridge: claiming a job failed", "err", result.err, "retry_in", backoff)
			r.pauseClaims(ctx, q, backoff)
			backoff = min(backoff*2, time.Minute)
		default:
			backoff = time.Second
			if time.Since(start) < time.Second {
				r.pauseClaims(ctx, q, time.Second)
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

func (r *Runner) rejectClaim(ctx context.Context, q *claimRequest, j *link.Job) {
	// Never execute a kind or priority outside the reserved admission, even if
	// a server incorrectly ignores the negotiated filter. Do not persist params.
	job := jobOf(j)
	job.Params = nil
	if err := r.store.RecordClaim(ctx, store.ServerJob{ID: job.ID, Kind: job.Kind, Attempt: job.Attempt, ServerID: q.serverID}); err != nil {
		r.log.Warn("bridge: recording a refused claim failed", "job", job.ID, "err", err)
	}
	r.finish(ctx, q.srv, job, store.JobFailed, nil, nil, "the server returned a job outside the requested admission")
}

// Recovery and re-pair admission uses the ordinary lane, since old execution
// rows do not store priority. A waiting recovered job renews its lease through
// execute's reporter and takes a slot before any new claims for that lane.
func (r *Runner) awaitAdmission(ctx context.Context, ru *run) bool {
	for ctx.Err() == nil {
		srv := r.server()
		r.mu.Lock()
		if ru.admitted {
			r.mu.Unlock()
			return true
		}
		if srv != nil && r.roomLocked(r.capacities(srv), ru.lane, nil) {
			r.waiting[ru.lane]--
			r.use[ru.lane]++
			ru.admitted = true
			r.wakeLocked()
			r.mu.Unlock()
			return true
		}
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
	}
	return false
}
