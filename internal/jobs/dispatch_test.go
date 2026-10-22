package jobs

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

type controlledClaim struct {
	kinds     []string
	min       *int32
	answer    chan *link.Job
	cancelled chan struct{}
	cancelJob atomic.Pointer[link.Job]
	hold      atomic.Bool
	join      chan struct{}
}

type controlledServer struct {
	*fakeServer
	accepts        atomic.Bool
	calls          chan *controlledClaim
	updated        chan int64
	reportPriority atomic.Int64
	backlog        []*controlledClaim // test goroutine only
}

func newControlledServer(accept bool) *controlledServer {
	s := &controlledServer{fakeServer: newFakeServer(), calls: make(chan *controlledClaim, 32), updated: make(chan int64, 128)}
	s.accepts.Store(accept)
	return s
}
func (s *controlledServer) SupportsClaimMinPriority() bool { return s.accepts.Load() }
func (s *controlledServer) Claim(ctx context.Context, kinds []string, _ time.Duration) (*link.Job, error) {
	return s.claim(ctx, kinds, nil)
}
func (s *controlledServer) ClaimMinPriority(ctx context.Context, kinds []string, _ time.Duration, priority int32) (*link.Job, error) {
	return s.claim(ctx, kinds, &priority)
}
func (s *controlledServer) claim(ctx context.Context, kinds []string, priority *int32) (*link.Job, error) {
	c := &controlledClaim{kinds: slices.Clone(kinds), min: priority, answer: make(chan *link.Job, 1), cancelled: make(chan struct{}), join: make(chan struct{})}
	select {
	case s.calls <- c:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case job := <-c.answer:
		return job, nil
	case <-ctx.Done():
		close(c.cancelled)
		if c.hold.Load() {
			<-c.join
		}
		return c.cancelJob.Load(), ctx.Err()
	}
}
func (s *controlledServer) UpdateJob(ctx context.Context, id int64, u link.Update) (*link.Job, error) {
	j, err := s.fakeServer.UpdateJob(ctx, id, u)
	if j != nil {
		j.Priority = int(s.reportPriority.Load())
	}
	select {
	case s.updated <- id:
	default:
	}
	return j, err
}
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for channel")
		var zero T
		return zero
	}
}
func (s *controlledServer) next(t *testing.T, reserve bool) *controlledClaim {
	t.Helper()
	for {
		for i, c := range s.backlog {
			if (c.min != nil) == reserve {
				s.backlog = slices.Delete(s.backlog, i, i+1)
				return c
			}
		}
		c := await(t, s.calls)
		s.backlog = append(s.backlog, c)
	}
}
func startControlled(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); await(t, done) })
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

type optedHandler struct{ handler }

func (optedHandler) Interactive() bool { return true }

func wirePriority(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/wire-2b.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Priority int `json:"priority_interactive"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Priority
}

func blockingHandler(kind, lane string, started chan<- Job, gates map[int64]chan struct{}) handler {
	return handler{kind: kind, lane: lane, fn: func(ctx context.Context, j Job, _ *Reporter) (any, error) {
		started <- j
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-gates[j.ID]:
			return nil, nil
		}
	}}
}
func claimed(id int64, kind string, priority int) *link.Job {
	return &link.Job{ID: id, Kind: kind, Priority: priority, Attempts: 1}
}
func assertPools(t *testing.T, r *Runner, expected map[int64]string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, pool := range expected {
		if ru := r.running[id]; ru == nil || ru.lane != pool || !ru.admitted {
			t.Fatalf("job %d admission = %+v, expected %s", id, ru, pool)
		}
	}
	if r.use[LaneDownload]+r.use[LaneInteractive] > 2 {
		t.Fatal("enclosure concurrency exceeded 2", r.use)
	}
}

func TestReservedSaturationAndRelease(t *testing.T) {
	srv := newControlledServer(true)
	started := make(chan Job, 8)
	gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
	h := optedHandler{blockingHandler("podcast.fetch", LaneDownload, started, gates)}
	r := New(openStore(t), func() Server { return srv }, nil, testOpts(), h,
		handler{kind: "not.opted", lane: LaneLight})
	startControlled(t, r)
	ordinary, reserve := srv.next(t, false), srv.next(t, true)
	priority := wirePriority(t)
	if *reserve.min != int32(priority) || !slices.Equal(reserve.kinds, []string{h.Kind()}) {
		t.Fatal("reserved eligibility", reserve)
	}
	ordinary.answer <- claimed(1, h.Kind(), 20)
	await(t, started)
	// While an ordinary download is busy, only the reserve can take the play.
	narrow := srv.next(t, false)
	if !slices.Equal(narrow.kinds, []string{"not.opted"}) {
		t.Fatal(narrow.kinds)
	}
	reserve.answer <- claimed(2, h.Kind(), priority)
	await(t, started)
	assertPools(t, r, map[int64]string{1: LaneDownload, 2: LaneInteractive})
	close(gates[1])
	await(t, narrow.cancelled)
	expanded := srv.next(t, false)
	if !slices.Contains(expanded.kinds, h.Kind()) {
		t.Fatal(expanded.kinds)
	}
	assertPools(t, r, map[int64]string{2: LaneInteractive})
}

func TestRefreshJoinsAndOwnsCancelRace(t *testing.T) {
	srv := newControlledServer(false)
	started := make(chan Job, 4)
	gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
	opts := testOpts()
	opts.Lanes = map[string]int{LaneDownload: 1, LaneLight: 1}
	r := New(openStore(t), func() Server { return srv }, nil, opts,
		blockingHandler("download", LaneDownload, started, gates), blockingHandler("light", LaneLight, started, gates))
	startControlled(t, r)
	first := srv.next(t, false)
	first.answer <- claimed(1, "download", 20)
	await(t, started)
	narrow := srv.next(t, false)
	if !slices.Equal(narrow.kinds, []string{"light"}) {
		t.Fatal(narrow.kinds)
	}
	narrow.hold.Store(true)
	narrow.cancelJob.Store(claimed(2, "light", 20))
	// Ensure cleanup can always release the deliberately held old call.
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(narrow.join) }) })
	close(gates[1])
	await(t, narrow.cancelled)
	select {
	case c := <-srv.calls:
		t.Fatal("reissued before old claim joined", c)
	default:
	}
	once.Do(func() { close(narrow.join) })
	if j := await(t, started); j.ID != 2 {
		t.Fatal("cancel-race job lost", j)
	}
	refreshed := srv.next(t, false)
	if !slices.Equal(refreshed.kinds, []string{"download"}) {
		t.Fatal(refreshed.kinds)
	}
}

func TestLegacyDownloadCapacityAndNegotiationChange(t *testing.T) {
	srv := newControlledServer(false)
	started := make(chan Job, 4)
	gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{}), 3: make(chan struct{})}
	h := optedHandler{blockingHandler("podcast.fetch", LaneDownload, started, gates)}
	r := New(openStore(t), func() Server { return srv }, nil, testOpts(), h)
	startControlled(t, r)
	for _, id := range []int64{1, 2} {
		c := srv.next(t, false)
		if c.min != nil {
			t.Fatal("legacy filter", c)
		}
		c.answer <- claimed(id, h.Kind(), 20)
		await(t, started)
	}
	assertPools(t, r, map[int64]string{1: LaneDownload, 2: LaneDownload})
	q, _ := r.request(true, false, nil)
	if len(q.kinds) != 0 {
		t.Fatal("legacy reserve enabled", q)
	}
	srv.accepts.Store(true)
	r.Wake()
	q, _ = r.request(true, false, nil)
	if len(q.kinds) != 0 {
		t.Fatal("negotiation overclaimed while legacy jobs active", q)
	}
	close(gates[1])
	reserve := srv.next(t, true)
	if *reserve.min != int32(wirePriority(t)) {
		t.Fatal(*reserve.min)
	}
	assertPools(t, r, map[int64]string{2: LaneDownload})
	// A later server downgrade cancels the reserved poll and restores the
	// second ordinary slot without borrowing background work into reserve.
	srv.accepts.Store(false)
	r.Wake()
	await(t, reserve.cancelled)
	ordinary := srv.next(t, false)
	ordinary.answer <- claimed(3, h.Kind(), 20)
	await(t, started)
	assertPools(t, r, map[int64]string{2: LaneDownload, 3: LaneDownload})
	q, _ = r.request(true, false, nil)
	if len(q.kinds) != 0 {
		t.Fatal("downgraded server still has a reserve", q)
	}
}

func TestReservedRefusesOutsideAdmission(t *testing.T) {
	for _, mode := range []string{"below threshold", "kind not opted in"} {
		t.Run(mode, func(t *testing.T) {
			srv := newControlledServer(true)
			started := make(chan Job, 1)
			h := optedHandler{blockingHandler("podcast.fetch", LaneDownload, started, nil)}
			r := New(openStore(t), func() Server { return srv }, nil, testOpts(), h,
				blockingHandler("not.opted", LaneLight, started, nil))
			startControlled(t, r)
			reserve := srv.next(t, true)
			job := claimed(9, h.Kind(), wirePriority(t)-1)
			if mode == "kind not opted in" {
				job.Kind, job.Priority = "not.opted", wirePriority(t)
			}
			answered := time.Now()
			reserve.answer <- job
			// A new reserved claim proves the denied response was processed,
			// and only after a backoff: a server ignoring the filter must not
			// see its queue refused in a tight loop.
			srv.next(t, true)
			if elapsed := time.Since(answered); elapsed < time.Second {
				t.Fatal("refusal did not back off", elapsed)
			}
			if state, _ := srv.finalState(9); state != link.StateFailed {
				t.Fatal("unfiltered server answer was not failed", state)
			}
			select {
			case j := <-started:
				t.Fatal("job outside admission ran", j)
			default:
			}
		})
	}
}

func TestUnchangedWakeAndCapacityRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := newControlledServer(true)
		r := New(nil, func() Server { return srv }, nil, testOpts(), handler{kind: "light", lane: LaneLight})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		q, changed := r.request(false, true, nil)
		done := make(chan bool, 1)
		go func() { _, refreshed := r.poll(ctx, q, changed); done <- refreshed }()
		call := await(t, srv.calls)
		for range 3 {
			r.Wake()
			synctest.Wait()
		}
		select {
		case <-call.cancelled:
			t.Fatal("unchanged wake cancelled poll")
		default:
		}
		r.setCapacity(LaneLight, 1)
		synctest.Wait()
		if !await(t, done) {
			t.Fatal("capacity change did not refresh")
		}
		await(t, call.cancelled)
		r.unreserve(q)
	})
}

func TestRepairRefreshOwnsOldPairingJob(t *testing.T) {
	old, next := newControlledServer(true), newControlledServer(true)
	var current atomic.Pointer[controlledServer]
	current.Store(old)
	started := make(chan Job, 2)
	opts := testOpts()
	opts.ServerID = func() string {
		if current.Load() == old {
			return "old"
		}
		return "new"
	}
	h := optedHandler{blockingHandler("podcast.fetch", LaneDownload, started, nil)}
	db := openStore(t)
	r := New(db, func() Server { return current.Load() }, nil, opts, h)
	startControlled(t, r)
	ordinary, reserve := old.next(t, false), old.next(t, true)
	reserve.cancelJob.Store(claimed(7, h.Kind(), wirePriority(t)))
	current.Store(next)
	r.Wake()
	await(t, ordinary.cancelled)
	await(t, reserve.cancelled)
	if j := await(t, started); j.ID != 7 {
		t.Fatal(j)
	}
	// The old response remains on its old link and identity after re-pair.
	sj, err := db.GetServerJob(context.Background(), 7)
	if err != nil || sj.ServerID != "old" {
		t.Fatal(sj, err)
	}
	next.next(t, false)
	old.mu.Lock()
	reported := len(old.updates[7]) != 0
	old.mu.Unlock()
	if !reported {
		t.Fatal("old job used the new link")
	}
}

func TestRecoveryAdmissionRenewsAndReservesInteractive(t *testing.T) {
	srv := newControlledServer(true)
	db := openStore(t)
	for _, id := range []int64{1, 2, 3} {
		if err := db.RecordClaim(context.Background(), store.ServerJob{ID: id, Kind: "podcast.fetch", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan Job, 8)
	gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{}), 3: make(chan struct{}), 4: make(chan struct{})}
	h := optedHandler{blockingHandler("podcast.fetch", LaneDownload, started, gates)}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	startControlled(t, r)
	first := await(t, started)
	if !first.Resumed {
		t.Fatal(first)
	}
	// All recovered claims keep their leases, including those waiting for room.
	seen := map[int64]bool{}
	for len(seen) < 3 {
		seen[await(t, srv.updated)] = true
	}
	reserve := srv.next(t, true)
	reserve.answer <- claimed(4, h.Kind(), wirePriority(t))
	if j := await(t, started); j.ID != 4 {
		t.Fatal("recovery over-admitted", j)
	}
	assertPools(t, r, map[int64]string{first.ID: LaneDownload, 4: LaneInteractive})
	close(gates[first.ID])
	second := await(t, started)
	if !second.Resumed || second.ID == first.ID {
		t.Fatal(second)
	}
	assertPools(t, r, map[int64]string{second.ID: LaneDownload, 4: LaneInteractive})
}

func TestOrdinaryInteractivePromotionKeepsPool(t *testing.T) {
	srv := newControlledServer(true)
	started := make(chan Job, 4)
	promote, confirmed := make(chan struct{}), make(chan struct{})
	h := optedHandler{handler{kind: "custom.interactive", lane: LaneDownload, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		started <- j
		if j.ID == 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-promote:
			}
			if err := rep.ConfirmLease(ctx); err != nil {
				return nil, err
			}
			close(confirmed)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	r := New(openStore(t), func() Server { return srv }, nil, testOpts(), h)
	startControlled(t, r)
	ordinary, reserve := srv.next(t, false), srv.next(t, true)
	ordinary.answer <- claimed(1, h.Kind(), 10)
	await(t, started)
	srv.reportPriority.Store(int64(wirePriority(t)))
	close(promote)
	await(t, confirmed)
	assertPools(t, r, map[int64]string{1: LaneDownload})
	reserve.answer <- claimed(2, h.Kind(), wirePriority(t))
	await(t, started)
	assertPools(t, r, map[int64]string{1: LaneDownload, 2: LaneInteractive})
}

func TestLiveParkKeepsSlotAndTerminalParkWakesDispatcher(t *testing.T) {
	srv := newControlledServer(true)
	parked, end := make(chan struct{}), make(chan struct{})
	h := optedHandler{handler{kind: "custom.interactive", lane: LaneDownload, fn: func(ctx context.Context, _ Job, rep *Reporter) (any, error) {
		rep.Park("waiting", "Waiting")
		close(parked)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-end:
		}
		return nil, &ParkError{Code: "waiting", Message: "Waiting"}
	}}}
	r := New(openStore(t), func() Server { return srv }, nil, testOpts(), h, handler{kind: "light", lane: LaneLight})
	startControlled(t, r)
	ordinary := srv.next(t, false)
	ordinary.answer <- claimed(1, h.Kind(), 10)
	await(t, parked)
	narrow := srv.next(t, false)
	if !slices.Equal(narrow.kinds, []string{"light"}) {
		t.Fatal(narrow.kinds)
	}
	assertPools(t, r, map[int64]string{1: LaneDownload})
	close(end)
	await(t, narrow.cancelled)
	expanded := srv.next(t, false)
	if !slices.Contains(expanded.kinds, h.Kind()) {
		t.Fatal(expanded.kinds)
	}
	if state, _ := srv.finalState(1); state != link.StateNeedsAttention {
		t.Fatal(state)
	}
}

func TestPairingWakeDuringSnapshotIsNotLost(t *testing.T) {
	old, next := newControlledServer(false), newControlledServer(false)
	var current atomic.Pointer[controlledServer]
	current.Store(old)
	read, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	r := New(nil, func() Server {
		srv := current.Load()
		if once.CompareAndSwap(false, true) {
			close(read)
			<-release
		}
		return srv
	}, nil, testOpts(), handler{kind: "light", lane: LaneLight})
	done := make(chan *claimRequest, 1)
	go func() { q, _ := r.request(false, true, nil); done <- q }()
	await(t, read)
	current.Store(next)
	r.Wake()
	close(release)
	q := await(t, done)
	if q.srv != next {
		t.Fatal("missed pairing change during snapshot")
	}
	r.unreserve(q)
}

func TestOrdinaryCanTakeInteractiveWork(t *testing.T) {
	srv := newControlledServer(true)
	started := make(chan Job, 1)
	h := optedHandler{blockingHandler("custom.interactive", LaneDownload, started, nil)}
	r := New(openStore(t), func() Server { return srv }, nil, testOpts(), h)
	startControlled(t, r)
	ordinary := srv.next(t, false)
	ordinary.answer <- claimed(1, h.Kind(), wirePriority(t))
	await(t, started)
	assertPools(t, r, map[int64]string{1: LaneDownload})
}

func TestWaitingRecoveryRunsOrAbandons(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "admitted", true: "cancelled"}[cancelled], func(t *testing.T) {
			db, srv := openStore(t), newControlledServer(false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := db.RecordClaim(ctx, store.ServerJob{ID: 9, Kind: "light", Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			h := &abandoner{handler: handler{kind: "light", lane: LaneLight, fn: func(context.Context, Job, *Reporter) (any, error) { return nil, nil }}}
			opts := testOpts()
			opts.Lanes = map[string]int{LaneLight: 1}
			r := New(db, func() Server { return srv }, nil, opts, h)
			r.use[LaneLight] = 1
			var wg sync.WaitGroup
			r.launch(ctx, srv, Job{ID: 9, Kind: h.Kind(), Attempt: 1, Resumed: true}, &wg, "", nil)
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			t.Cleanup(func() { cancel(); await(t, done) })
			await(t, srv.updated) // lease renewed while admission is blocked
			if cancelled {
				if !r.Cancel(9) {
					t.Fatal("missing waiting job")
				}
			} else {
				r.mu.Lock()
				r.use[LaneLight] = 0
				r.wakeLocked()
				r.mu.Unlock()
			}
			await(t, done)
			if h.ran.Load() == cancelled || (len(h.abandoned()) == 1) != cancelled {
				t.Fatal("waiting job disposition", h.ran.Load(), h.abandoned())
			}
		})
	}
}

func TestWaitingAdmissionSeesWakeDuringPairingRead(t *testing.T) {
	db, srv := openStore(t), newControlledServer(false)
	ran := make(chan struct{})
	h := handler{kind: "light", lane: LaneLight, fn: func(context.Context, Job, *Reporter) (any, error) {
		close(ran)
		return nil, nil
	}}
	read, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r := New(db, func() Server {
		if calls.Add(1) == 1 {
			close(read)
			<-release
			return nil // the link becomes ready during this read
		}
		return srv
	}, nil, testOpts(), h)
	r.use[LaneLight] = 2
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	r.launch(ctx, srv, Job{ID: 4, Kind: h.Kind(), Attempt: 1, Resumed: true}, &wg, "", nil)
	await(t, read)
	r.mu.Lock()
	r.use[LaneLight] = 0
	r.mu.Unlock()
	r.Wake()
	close(release)
	await(t, ran)
}

func TestClaimRacingStopIsRecordedForResume(t *testing.T) {
	db, srv := openStore(t), newControlledServer(false)
	h := handler{kind: "light", lane: LaneLight, fn: func(context.Context, Job, *Reporter) (any, error) {
		t.Error("a claim that raced the stop ran")
		return nil, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	ctx, stop := context.WithCancelCause(context.Background())
	stop(errStopping)
	var wg sync.WaitGroup
	r.start(ctx, srv, Job{ID: 3, Kind: h.Kind(), Attempt: 1}, &wg, nil)
	wg.Wait()
	sj, err := db.GetServerJob(context.Background(), 3)
	if err != nil || sj.State != store.JobRunning {
		t.Fatal("claim racing the stop was not kept for the next start", sj, err)
	}
}

func TestClaimRefreshReissuesWithoutTimeAdvance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := newControlledServer(false)
		opts := testOpts()
		opts.Wait = 25 * time.Second
		r := New(nil, func() Server { return srv }, nil, opts, handler{kind: "light", lane: LaneLight})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		var wg sync.WaitGroup
		go func() { r.dispatch(ctx, false, &wg); close(done) }()
		first := srv.next(t, false)
		start := time.Now()
		r.setCapacity(LaneLight, 1)
		await(t, first.cancelled)
		srv.next(t, false)
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatal("refresh waited or backed off", elapsed)
		}
		cancel()
		await(t, done)
	})
}

func TestClaimBackoffIgnoresUnchangedWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := newControlledServer(true)
		r := New(nil, func() Server { return srv }, nil, testOpts(), handler{kind: "light", lane: LaneLight})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		q, _ := r.request(false, false, nil)
		done := make(chan struct{})
		go func() { r.pauseClaims(ctx, q, time.Hour); close(done) }()
		synctest.Wait()
		for range 3 {
			r.Wake()
			synctest.Wait()
		}
		select {
		case <-done:
			t.Fatal("unchanged wake bypassed idle/network backoff")
		default:
		}
		start := time.Now()
		r.setCapacity(LaneLight, 1)
		await(t, done)
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatal("eligibility change did not bypass backoff", elapsed)
		}
	})
}

func TestUnpairCancelsOutstandingClaims(t *testing.T) {
	srv := newControlledServer(true)
	var current atomic.Pointer[controlledServer]
	current.Store(srv)
	h := optedHandler{handler{kind: "podcast.fetch", lane: LaneDownload}}
	r := New(openStore(t), func() Server {
		if s := current.Load(); s != nil {
			return s
		}
		return nil
	}, nil, testOpts(), h)
	startControlled(t, r)
	ordinary, reserve := srv.next(t, false), srv.next(t, true)
	current.Store(nil)
	r.Wake()
	await(t, ordinary.cancelled)
	await(t, reserve.cancelled)
	q, _ := r.request(false, false, nil)
	if q.srv != nil || len(q.kinds) != 0 {
		t.Fatal("unpaired eligibility", q)
	}
	current.Store(srv)
	r.Wake()
	srv.next(t, false)
	srv.next(t, true)
}
