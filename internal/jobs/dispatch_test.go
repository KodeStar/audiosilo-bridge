package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/testwait"
)

type controlledClaim struct {
	kinds     []string
	wait      time.Duration
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
func (s *controlledServer) WithdrawClaimMinPriority()      { s.accepts.Store(false) }
func (s *controlledServer) Claim(ctx context.Context, kinds []string, wait time.Duration) (*link.Job, error) {
	return s.claim(ctx, kinds, wait, nil)
}
func (s *controlledServer) ClaimMinPriority(ctx context.Context, kinds []string, wait time.Duration, priority int32) (*link.Job, error) {
	return s.claim(ctx, kinds, wait, &priority)
}
func (s *controlledServer) claim(ctx context.Context, kinds []string, wait time.Duration, priority *int32) (*link.Job, error) {
	c := &controlledClaim{kinds: slices.Clone(kinds), wait: wait, min: priority, answer: make(chan *link.Job, 1), cancelled: make(chan struct{}), join: make(chan struct{})}
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
	case <-time.After(testwait.Scaled(3 * time.Second)):
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
	// The freed slot leaves the narrow claim in flight (BR-CLAIM-HOLD); the
	// next claim asks for the download again.
	narrow.answer <- nil
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
	r.setCapacity(LaneDownload, 2) // a capacity change cancels an in-flight claim
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
	close(gates[1])
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

func TestRollbackBeforeHeartbeatUsesOrdinaryAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, db := newControlledServer(true), openStore(t)
		started := make(chan Job, 4)
		gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{}), 3: make(chan struct{}), 4: make(chan struct{})}
		var active atomic.Int32
		base := blockingHandler("podcast.fetch", LaneDownload, started, gates)
		block := base.fn
		base.fn = func(ctx context.Context, j Job, rep *Reporter) (any, error) {
			if n := active.Add(1); n > 2 {
				t.Error("enclosure concurrency exceeded 2", n)
			}
			defer active.Add(-1)
			return block(ctx, j, rep)
		}
		h := optedHandler{base}
		srv.id = "rollback"
		r := New(db, func() Server { return srv }, nil, testOpts(), h)
		startControlled(t, r)
		ordinary, reserve := srv.next(t, false), srv.next(t, true)
		ordinary.answer <- claimed(1, h.Kind(), 20)
		await(t, started)
		// An older server now occupies the same address, before any heartbeat.
		job := claimed(2, h.Kind(), 20)
		job.Params = json.RawMessage(`{"episode":"kept"}`)
		reserve.answer <- job
		if got := await(t, started); got.ID != 2 || got.Attempt != 1 || string(got.Params) != string(job.Params) {
			t.Fatal("rollback claim lost its identity or params", got)
		}
		assertPools(t, r, map[int64]string{1: LaneDownload, 2: LaneDownload})
		if srv.SupportsClaimMinPriority() {
			t.Fatal("ignored filter left negotiation enabled")
		}
		for range 3 {
			r.Wake() // unchanged scheduling events must not restore negotiation
			synctest.Wait()
		}
		// Finishing a job immediately reissues an ordinary claim, without a
		// refusal backoff or any more min_priority requests.
		before := time.Now()
		close(gates[1])
		c := await(t, srv.calls)
		if c.min != nil || time.Since(before) != 0 {
			t.Fatal("rollback did not reissue ordinary claim immediately", c)
		}
		c.answer <- claimed(3, h.Kind(), 20)
		await(t, started)
		assertPools(t, r, map[int64]string{2: LaneDownload, 3: LaneDownload})
		// A later heartbeat advertises the accept again. Two legacy downloads
		// must finish down to one before the reserve can claim or execute.
		srv.accepts.Store(true)
		r.Wake()
		synctest.Wait()
		select {
		case c := <-srv.calls:
			t.Fatal("renegotiation overclaimed two active downloads", c)
		default:
		}
		close(gates[2])
		reserve = srv.next(t, true)
		reserve.answer <- claimed(4, h.Kind(), wirePriority(t))
		if got := await(t, started); got.ID != 4 {
			t.Fatal(got)
		}
		assertPools(t, r, map[int64]string{3: LaneDownload, 4: LaneInteractive})
		if state, _ := srv.finalState(2); state != link.StateDone {
			t.Fatal("rollback job did not complete successfully", state)
		}
		sj, err := db.GetServerJob(context.Background(), 2)
		if err != nil || sj.Attempt != 1 || sj.ServerID != "rollback" {
			t.Fatal("rollback claim changed attempt or server", sj, err)
		}
	})
}

func TestOutsideReservedKindWaitsAndRenewsOrdinaryLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := newControlledServer(true)
		started := make(chan Job, 4)
		gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
		opts := testOpts()
		opts.Lanes = DefaultLanes()
		opts.Lanes[LaneLight] = 1
		r := New(openStore(t), func() Server { return srv }, nil, opts,
			optedHandler{blockingHandler("podcast.fetch", LaneDownload, started, gates)},
			blockingHandler("not.opted", LaneLight, started, gates))
		startControlled(t, r)
		ordinary, reserve := srv.next(t, false), srv.next(t, true)
		ordinary.answer <- claimed(1, "not.opted", 20)
		await(t, started)
		narrow := srv.next(t, false)
		reserve.answer <- claimed(2, "not.opted", wirePriority(t))
		// Capability withdrawal cancels the other claim too, since its
		// capacity snapshot changed, and no filtered claim replaces it.
		await(t, narrow.cancelled)
		refreshed := srv.next(t, false)
		for range 2 {
			for await(t, srv.updated) != 2 {
			}
		}
		r.mu.Lock()
		ru := r.running[2]
		waiting := ru != nil && ru.lane == LaneLight && !ru.admitted && r.waiting[LaneLight] == 1
		r.mu.Unlock()
		if !waiting {
			t.Fatal("outside kind did not wait in its ordinary lane")
		}
		if state, _ := srv.finalState(2); state != "" {
			t.Fatal("waiting claim was ended", state)
		}
		select {
		case j := <-started:
			t.Fatal("outside kind bypassed ordinary admission", j)
		case c := <-srv.calls:
			t.Fatal("claim looped after filter withdrawal", c)
		default:
		}
		close(gates[1])
		if j := await(t, started); j.ID != 2 {
			t.Fatal(j)
		}
		assertPools(t, r, map[int64]string{2: LaneLight})
		select {
		case <-refreshed.cancelled:
			t.Fatal("ordinary waiter admission changed unrelated eligibility")
		default:
		}
	})
}

func TestIgnoredFilterHoldsOneStrayWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := newControlledServer(true)
		started := make(chan Job, 4)
		gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
		opts := testOpts()
		opts.Lanes = DefaultLanes()
		opts.Lanes[LaneLight] = 1
		r := New(openStore(t), func() Server { return srv }, nil, opts,
			optedHandler{blockingHandler("podcast.fetch", LaneDownload, started, gates)},
			blockingHandler("not.opted", LaneLight, started, gates))
		startControlled(t, r)
		ordinary, reserve := srv.next(t, false), srv.next(t, true)
		ordinary.answer <- claimed(1, "not.opted", 20)
		await(t, started)
		reserve.answer <- claimed(2, "not.opted", wirePriority(t))
		synctest.Wait()
		// A server that re-advertises the filter on every heartbeat but keeps
		// ignoring it must not move its queue into waiters, one beat at a time.
		for range 3 {
			srv.accepts.Store(true)
			r.Wake()
			synctest.Wait()
			for drained := false; !drained; {
				select {
				case c := <-srv.calls:
					if c.min != nil {
						t.Fatal("reserve claimed again while its stray waits")
					}
					srv.backlog = append(srv.backlog, c)
				default:
					drained = true
				}
			}
		}
		close(gates[1])
		if j := await(t, started); j.ID != 2 {
			t.Fatal(j)
		}
		srv.next(t, true) // the stray's admission frees the reserve again
	})
}

func TestOrdinaryOutsideKindsBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := newControlledServer(false)
		started := make(chan Job, 1)
		r := New(openStore(t), func() Server { return srv }, nil, testOpts(),
			blockingHandler("podcast.fetch", LaneDownload, started, nil))
		startControlled(t, r)
		answered := time.Now()
		srv.next(t, false).answer <- claimed(5, "unknown.kind", 20)
		srv.next(t, false)
		if elapsed := time.Since(answered); elapsed < time.Second {
			t.Fatal("an answer outside the requested kinds did not back off", elapsed)
		}
		if state, _ := srv.finalState(5); state != link.StateFailed {
			t.Fatal("unknown kind was not failed", state)
		}
	})
}

// bridge#13, #26: an ordinary claim answered outside its kinds waits in its own
// lane, which the dispatcher stops asking for, but the other lanes go on claiming
// (after the back-off); a second such answer while one waits stops the
// dispatcher until one has its slot.
func TestOrdinaryStrayPausesOnlyItsLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := newControlledServer(false)
		started := make(chan Job, 4)
		gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{}), 3: make(chan struct{})}
		opts := testOpts()
		opts.Lanes = map[string]int{LaneDownload: 1, LaneLight: 2}
		r := New(openStore(t), func() Server { return srv }, nil, opts,
			blockingHandler("podcast.fetch", LaneDownload, started, gates),
			blockingHandler("light", LaneLight, started, gates))
		startControlled(t, r)
		srv.next(t, false).answer <- claimed(1, "podcast.fetch", 20)
		await(t, started) // the download lane is full now
		lightOnly := srv.next(t, false)
		if !slices.Equal(lightOnly.kinds, []string{"light"}) {
			t.Fatal(lightOnly.kinds)
		}
		lightOnly.answer <- claimed(2, "podcast.fetch", 20) // outside the kinds asked for
		again := srv.next(t, false)
		if !slices.Equal(again.kinds, []string{"light"}) {
			t.Fatal("a stray waiting for the download lane stopped the light lane's claims", again.kinds)
		}
		again.answer <- claimed(3, "podcast.fetch", 20) // and again: the server ignores the kinds
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case c := <-srv.calls:
			t.Fatal("claimed with two strays waiting", c.kinds)
		default:
		}
		close(gates[1])
		if j := await(t, started); j.ID != 2 && j.ID != 3 {
			t.Fatal(j)
		}
		srv.next(t, false) // one has its slot: claims go on
	})
}

func TestUnchangedWakeAndCapacityRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := newControlledServer(true)
		r := New(nil, func() Server { return srv }, nil, testOpts(), handler{kind: "light", lane: LaneLight})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		q, changed := r.request(false, true, nil)
		done := make(chan bool, 1)
		go func() { _, refreshed := r.poll(ctx, q, changed, r.opts.Wait); done <- refreshed }()
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

// A claim the old pairing's link answers once the Bridge is paired with another
// server is dropped whole (denied): never recorded, so the new server's job of
// the same id keeps its row, never run nor handed to its handler, and reported
// to neither link. Its own server's lease lapses it.
func TestRepairDropsTheOldPairingsAnswer(t *testing.T) {
	old, next := newControlledServer(true), newControlledServer(true)
	old.id, next.id = "old", "new"
	var current atomic.Pointer[controlledServer]
	current.Store(old)
	started := make(chan Job, 2)
	h := &abandoner{handler: blockingHandler("podcast.fetch", LaneDownload, started, nil)}
	db := openStore(t)
	r := New(db, func() Server { return current.Load() }, nil, testOpts(), h)
	startControlled(t, r)
	ordinary := old.next(t, false)
	ordinary.cancelJob.Store(claimed(7, h.Kind(), 0)) // answered as the re-pair cancels it
	// The new server's job of the same id already holds the row (its own
	// claim, taken by the other dispatcher) when the old answer is read.
	ctx := context.Background()
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 7, Kind: h.Kind(), Attempt: 1, ServerID: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetServerJobProgress(ctx, 7, 1, json.RawMessage(`{"step":"downloading"}`)); err != nil {
		t.Fatal(err)
	}
	current.Store(next)
	r.Wake()
	await(t, ordinary.cancelled)
	next.next(t, false) // the dispatcher has read the old answer and asks the new server
	sj, err := db.GetServerJob(ctx, 7)
	if err != nil || sj.ServerID != "new" || sj.State != store.JobRunning || !strings.Contains(string(sj.Progress), "downloading") {
		t.Fatalf("the new server's row after the old pairing's answer: %+v %v, want it untouched", sj, err)
	}
	select {
	case j := <-started:
		t.Fatal("another server's job ran", j)
	default:
	}
	if got := h.abandoned(); len(got) != 0 {
		t.Fatal("a claim that was never recorded was handed to its handler", got)
	}
	r.mu.Lock()
	_, running := r.running[7]
	r.mu.Unlock()
	if running {
		t.Fatal("the old pairing's answer is a run here")
	}
	for _, s := range []*controlledServer{old, next} {
		if n := s.triesOf(7); n != 0 {
			t.Fatal("the dropped job was reported on", s.id, n)
		}
	}
}

// An unpair on the Bridge's own page stops the jobs here (allowed: cancelled
// locally, for good, their work released) and sends nothing more to that
// server (denied), a job waiting for its lane slot and one a refused token
// had stopped included; each is recorded lost, and none is resumed, reported
// or recorded as failed once the Bridge is paired again, with that server or
// another.
func TestUnpairStopsTheJobsAndSendsNothingMore(t *testing.T) {
	for name, nextID := range map[string]string{"paired again with it": "a", "paired with another": "b"} {
		t.Run(name, func(t *testing.T) {
			srv, next := newControlledServer(false), newControlledServer(false)
			srv.id, next.id = "a", nextID
			var current atomic.Pointer[controlledServer]
			current.Store(srv)
			started := make(chan Job, 4)
			var resumes atomic.Bool
			h := &abandoner{handler: handler{kind: "podcast.fetch", lane: LaneDownload, fn: func(ctx context.Context, j Job, _ *Reporter) (any, error) {
				started <- j
				<-ctx.Done()
				resumes.Store(Resumes(ctx))
				return nil, ctx.Err()
			}}}
			db := openStore(t)
			opts := testOpts()
			opts.Lanes = map[string]int{LaneDownload: 1}
			r := New(db, func() Server {
				if s := current.Load(); s != nil {
					return s
				}
				return nil
			}, nil, opts, h)
			ctx := context.Background()
			startControlled(t, r)
			var wg sync.WaitGroup
			defer wg.Wait()
			runCtx, stop := context.WithCancel(ctx)
			defer stop()
			// Job 1 runs, job 2 waits for the lane's one slot (its lease renewed).
			srv.next(t, false).answer <- claimed(1, h.Kind(), 0)
			if j := await(t, started); j.ID != 1 {
				t.Fatal(j)
			}
			r.start(runCtx, srv, Job{ID: 2, Kind: h.Kind(), Attempt: 1}, &wg, nil)
			waitFor(t, func() bool { return srv.triesOf(1) > 0 && srv.triesOf(2) > 0 })

			current.Store(nil) // the app ends the pairing first
			// Job 3 is one a refused token had stopped (stranded, its row running).
			if err := db.RecordClaim(ctx, store.ServerJob{ID: 3, Kind: h.Kind(), Attempt: 1, ServerID: "a"}); err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			r.stranded = []Job{{ID: 3, Kind: h.Kind(), Attempt: 1, serverID: "a"}}
			r.mu.Unlock()
			r.Unpaired(srv)
			for _, id := range []int64{1, 2, 3} {
				waitFor(t, func() bool {
					sj, err := db.GetServerJob(ctx, id)
					return err == nil && sj.State == store.JobLost && sj.ServerID == "a"
				})
			}
			if resumes.Load() {
				t.Fatal("the running job's handler was told it resumes")
			}
			if got := h.abandoned(); !slices.Contains(got, 2) || !slices.Contains(got, 3) || slices.Contains(got, 1) {
				t.Fatal("the jobs that never ran weren't released (or the one that ran was)", got)
			}
			waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.running) == 0 })
			sent := map[int64]int{1: srv.triesOf(1), 2: srv.triesOf(2), 3: srv.triesOf(3)}
			if s, _ := srv.finalState(1); s != "" || sent[3] != 0 {
				t.Fatal("an end was reported to the unpaired server", s, sent)
			}
			// A claim the old link answers after the unpair is dropped whole.
			r.start(runCtx, srv, Job{ID: 4, Kind: h.Kind(), Attempt: 1}, &wg, nil)
			if _, err := db.GetServerJob(ctx, 4); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("a claim answered after the unpair was recorded", err)
			}
			current.Store(next)
			r.Wake()
			time.Sleep(10 * opts.Keepalive) // several keepalives and report retries
			for id, n := range sent {
				if got := srv.triesOf(id); got != n {
					t.Fatalf("job %d: %d more reports went to the unpaired server", id, got-n)
				}
				if got := next.triesOf(id); got != 0 {
					t.Fatalf("job %d: %d reports went to the next pairing's server", id, got)
				}
				if sj, err := db.GetServerJob(ctx, id); err != nil || sj.State != store.JobLost || sj.ServerID != "a" {
					t.Fatalf("job %d after the next pairing: %+v %v, want lost, of server a", id, sj, err)
				}
			}
			select {
			case j := <-started:
				t.Fatal("a job the unpair stopped ran again", j)
			default:
			}
		})
	}
}

// linkPairing is a test's Options.Pairing over the link cur holds: its
// PairingID and server, none while cur holds no link (unpaired).
func linkPairing(cur *atomic.Pointer[controlledServer]) func() Pairing {
	return func() Pairing {
		if s := cur.Load(); s != nil {
			return Pairing{ID: s.pairing, ServerID: s.id}
		}
		return Pairing{}
	}
}

// linkOf is a test's server func over cur (nil while it holds no link).
func linkOf(cur *atomic.Pointer[controlledServer]) func() Server {
	return func() Server {
		if s := cur.Load(); s != nil {
			return s
		}
		return nil
	}
}

// An unpair on the page drops the rows the store still has running that no run
// here holds (the Bridge unpaired before its first link after a restart): the
// pairing is gone, so they are another pairing's, released and recorded lost
// by the claim loop, and a later pairing with their server never resumes them
// (denied). A finished row waiting for its report is never sent (denied).
func TestUnpairDropsRowsNoRunHolds(t *testing.T) {
	srv := newControlledServer(false)
	srv.pairing = "p1"
	h := &abandoner{handler: handler{kind: "podcast.fetch", lane: LaneDownload, fn: func(context.Context, Job, *Reporter) (any, error) {
		return nil, nil
	}}}
	db := openStore(t)
	ctx := context.Background()
	for _, id := range []int64{5, 6} {
		if err := db.RecordClaim(ctx, store.ServerJob{ID: id, Kind: h.Kind(), Attempt: 1, ServerID: testServer, Pairing: "p1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.FinishServerJob(ctx, 6, 1, store.JobDone, nil, ""); err != nil {
		t.Fatal(err)
	}
	var paired, cur atomic.Pointer[controlledServer]
	cur.Store(srv) // paired, with no link yet since the start
	opts := testOpts()
	opts.Pairing = linkPairing(&cur)
	r := New(db, linkOf(&paired), nil, opts, h)
	startControlled(t, r)
	cur.Store(nil)
	r.Unpaired(srv)
	// Each row is waited on for its own end: the running row's release and the
	// finished row's mark land in either order.
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(ctx, 5)
		return err == nil && sj.State == store.JobLost && sj.Error == errPairingEnded.Error()
	})
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(ctx, 6)
		return err == nil && sj.State == store.JobDone && sj.Reported
	})
	if got := h.abandoned(); len(got) != 1 || got[0] != 5 {
		t.Fatal("the dropped row's work wasn't released once", got)
	}
	next := newControlledServer(false) // paired again with the same server: a new pairing
	next.pairing = "p2"
	cur.Store(next)
	paired.Store(next)
	r.Wake()
	next.next(t, false) // the runner claims anew
	time.Sleep(5 * opts.RetryEvery)
	if h.ran.Load() || next.triesOf(5) != 0 || next.triesOf(6) != 0 || srv.triesOf(6) != 0 {
		t.Fatal("the earlier pairing's jobs were resumed or reported on the next pairing")
	}
}

// Unpaired never waits for a job's work to be released (the unpair page's
// request is answered at once): an Abandon that blocks runs off the claim
// loop (releaseDropped), and the row is recorded lost once it returns.
func TestUnpairedDoesNotWaitForAbandon(t *testing.T) {
	srv := newControlledServer(false)
	srv.pairing = "p1"
	release := make(chan struct{})
	h := newCountingAbandoner(nil, release, 1)
	db := openStore(t)
	ctx := context.Background()
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 5, Kind: h.Kind(), Attempt: 1, ServerID: testServer, Pairing: "p1"}); err != nil {
		t.Fatal(err)
	}
	var none, cur atomic.Pointer[controlledServer]
	cur.Store(srv)
	opts := testOpts()
	opts.Pairing = linkPairing(&cur)
	r := New(db, linkOf(&none), nil, opts, h)
	startControlled(t, r)
	cur.Store(nil)
	returned := make(chan struct{})
	go func() { r.Unpaired(srv); close(returned) }()
	await(t, returned)
	await(t, h.entered) // the claim loop releases the row's work, still blocked
	if sj, err := db.GetServerJob(ctx, 5); err != nil || sj.State != store.JobRunning {
		t.Fatalf("recorded lost before its work was released: %+v %v", sj, err)
	}
	close(release)
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(ctx, 5)
		return err == nil && sj.State == store.JobLost
	})
}

// A run dropped by an unpair before its handler ran (it waited for the lane's
// one slot) whose work takes long to release gives its place in the lane back
// at once: a new pairing's claim starts there while the release still waits
// (denied: released on the run's goroutine, the run first in line for the
// slot until it returns, which a release tried again round after round may
// never do), and its row stays running and held until the release returns,
// then is recorded lost (allowed).
func TestDroppedRunFreesItsLaneWhileItsReleaseWaits(t *testing.T) {
	srv := newControlledServer(false)
	srv.pairing = "p1"
	var cur atomic.Pointer[controlledServer]
	cur.Store(srv)
	started := make(chan Job, 4)
	release := make(chan struct{})
	h := newCountingAbandoner(func(ctx context.Context, j Job, _ *Reporter) (any, error) {
		started <- j
		<-ctx.Done()
		return nil, ctx.Err()
	}, release, 4)
	db := openStore(t)
	opts := testOpts()
	opts.Lanes = map[string]int{LaneDownload: 1}
	opts.Pairing = linkPairing(&cur)
	r := New(db, linkOf(&cur), nil, opts, h)
	startControlled(t, r)
	var wg sync.WaitGroup
	defer wg.Wait()
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	// Job 1 runs in the lane's one slot; job 2 waits for it.
	srv.next(t, false).answer <- claimed(1, h.Kind(), 0)
	if j := await(t, started); j.ID != 1 {
		t.Fatal(j)
	}
	r.start(runCtx, srv, Job{ID: 2, Kind: h.Kind(), Attempt: 1}, &wg, nil)
	waitFor(t, func() bool { _, waiting := laneCounts(r, LaneDownload); return waiting == 1 })
	cur.Store(nil) // the app ends the pairing first
	r.Unpaired(srv)
	if id := await(t, h.entered); id != 2 {
		t.Fatal("released:", id)
	}
	next := newControlledServer(false) // paired again with the same server: a new pairing
	next.pairing = "p2"
	cur.Store(next)
	r.Wake()
	next.next(t, false).answer <- claimed(10, h.Kind(), 0)
	if j := await(t, started); j.ID != 10 {
		t.Fatal("the new pairing's claim didn't run", j)
	}
	ctx := context.Background()
	if sj, err := db.GetServerJob(ctx, 2); err != nil || sj.State != store.JobRunning || !r.holds(2) || h.inFlight.Load() != 1 {
		t.Fatalf("the dropped run whose release waits: %+v %v, held %v; want running and held", sj, err, r.holds(2))
	}
	close(release)
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(ctx, 2)
		return err == nil && sj.State == store.JobLost && sj.Error == errPairingEnded.Error() && !r.holds(2)
	})
	if got := h.abandoned(); !slices.Equal(got, []int64{2}) {
		t.Fatal("released:", got)
	}
}

// countingAbandoner's Abandon sends the job's id on entered, waits for release
// (nil: for its context to end) and counts how many wait at once, and how many
// were told under a context that had ended already (toldEnded).
type countingAbandoner struct {
	abandoner
	release          <-chan struct{}
	entered          chan int64
	inFlight, maxers atomic.Int32
	toldEnded        atomic.Int32
}

// newCountingAbandoner is a podcast.fetch countingAbandoner running fn (nil:
// one that ends at once), with room on entered for n jobs.
func newCountingAbandoner(fn func(context.Context, Job, *Reporter) (any, error), release <-chan struct{}, n int) *countingAbandoner {
	if fn == nil {
		fn = func(context.Context, Job, *Reporter) (any, error) { return nil, nil }
	}
	return &countingAbandoner{abandoner: abandoner{handler: handler{kind: "podcast.fetch", lane: LaneDownload, fn: fn}},
		release: release, entered: make(chan int64, n)}
}

func (a *countingAbandoner) Abandon(ctx context.Context, j Job) {
	if ctx.Err() != nil {
		a.toldEnded.Add(1)
	}
	n := a.inFlight.Add(1)
	for m := a.maxers.Load(); n > m && !a.maxers.CompareAndSwap(m, n); m = a.maxers.Load() {
	}
	a.entered <- j.ID
	select {
	case <-a.release:
	case <-ctx.Done():
	}
	a.inFlight.Add(-1)
	a.abandoner.Abandon(ctx, j)
}

// droppedRows records running rows 1..n of kind for pairing p1, a pairing the
// Bridge no longer has.
func droppedRows(t *testing.T, db *store.DB, kind string, n int64) {
	t.Helper()
	for id := int64(1); id <= n; id++ {
		if err := db.RecordClaim(context.Background(), store.ServerJob{ID: id, Kind: kind, Attempt: 1, ServerID: testServer, Pairing: "p1"}); err != nil {
			t.Fatal(err)
		}
	}
}

// rowsIn reports whether rows 1..n are all in state.
func rowsIn(db *store.DB, n int64, state string) bool {
	for id := int64(1); id <= n; id++ {
		if sj, err := db.GetServerJob(context.Background(), id); err != nil || sj.State != state {
			return false
		}
	}
	return true
}

// A start that drops several jobs of an earlier pairing whose work takes long
// to release holds up neither its own resumes nor the claiming (denied: the
// releases one after another on the claim loop, each up to 2 minutes): they
// are released dropConcurrency at a time beside the claims, and each row is
// recorded lost once its release is done, never before.
func TestDroppedJobsReleaseOffTheClaimLoop(t *testing.T) {
	const n = 6
	release := make(chan struct{})
	h := newCountingAbandoner(nil, release, n)
	db := openStore(t)
	droppedRows(t, db, h.Kind(), n)
	srv := newControlledServer(false)
	srv.pairing = "p2" // paired again with the same server: a new pairing
	var cur atomic.Pointer[controlledServer]
	cur.Store(srv)
	opts := testOpts()
	opts.Pairing = linkPairing(&cur)
	r := New(db, linkOf(&cur), nil, opts, h)
	startControlled(t, r)
	for range dropConcurrency {
		await(t, h.entered)
	}
	srv.next(t, false) // claiming while every release waits
	if got := h.inFlight.Load(); got != dropConcurrency {
		t.Fatal("releases at once:", got)
	}
	if !rowsIn(db, n, store.JobRunning) {
		t.Fatal("a row was recorded before its work was released")
	}
	close(release)
	waitFor(t, func() bool { return rowsIn(db, n, store.JobLost) })
	if got := h.abandoned(); len(got) != n {
		t.Fatal("released:", got)
	}
	if got := h.maxers.Load(); got != dropConcurrency {
		t.Fatal("most releases at once:", got)
	}
}

// A round of dropped jobs shares one deadline, and a job is recorded lost only
// once its release returned inside it. Releases still waiting at the deadline
// give up together and are tried again, their rows still running and held;
// the jobs the round never reached are told in the next one, ahead of those
// (denied: a deadline each, the batch taking one per job; denied: recorded
// lost at the deadline with the work unreleased, where generate's engine book
// would run on beside the job that waited for the row's end; denied: told
// under a context that had ended already, where a release that reads the
// store first releases nothing; and denied: the ones that gave up told first
// again, the rest never reached).
func TestDroppedJobsAreRecordedOnlyOnceReleased(t *testing.T) {
	const n = dropConcurrency + 2
	release := make(chan struct{})
	h := newCountingAbandoner(nil, release, 1024) // each waits for release, or its round's deadline
	db := openStore(t)
	droppedRows(t, db, h.Kind(), n)
	logs := &logCounter{n: map[string]int{}}
	opts := testOpts()
	opts.Log = slog.New(logs)
	r := New(db, func() Server { return nil }, nil, opts, h) // unpaired since
	r.dropDeadline = 300 * time.Millisecond
	startControlled(t, r)
	// The first round's releases wait until its deadline, so the last two rows
	// are reached only in a later round: told there, though the first ones
	// give up again in every round.
	told := map[int64]int{}
	for told[n-1] == 0 || told[n] == 0 {
		told[await(t, h.entered)]++
	}
	if got := logs.count("WARN bridge: releasing a dropped job's work gave up at its deadline"); got == 0 {
		t.Fatal("no release gave up at a round's deadline")
	}
	if got := h.toldEnded.Load(); got != 0 {
		t.Fatal("releases told under a context that had ended:", got)
	}
	if !rowsIn(db, n, store.JobRunning) {
		t.Fatal("a row was recorded lost before its work was released")
	}
	for id := int64(1); id <= n; id++ {
		if !r.holds(id) {
			t.Fatal("a job whose work isn't released yet is no longer held:", id)
		}
	}
	close(release)
	waitFor(t, func() bool { return rowsIn(db, n, store.JobLost) && !r.holds(1) && !r.holds(n) })
	if got := h.maxers.Load(); got > dropConcurrency {
		t.Fatal("most releases at once:", got)
	}
}

// A dropped job waits its turn behind its batch's slower releases while the
// new pairing claims: one whose row a claim took under its id meanwhile (job
// ids are each server's own) is read again as its turn comes and left to that
// claim (denied: its work released for the job listed, by a handler whose
// state is keyed by the job's id, and allowed: the rows still the dropped
// claims' are released and recorded lost).
func TestDroppedJobIsReadAgainAtItsTurn(t *testing.T) {
	const n = dropConcurrency + 1
	release := make(chan struct{})
	h := newCountingAbandoner(nil, release, n)
	db := openStore(t)
	droppedRows(t, db, h.Kind(), n)
	r := New(db, func() Server { return nil }, nil, testOpts(), h) // unpaired since
	startControlled(t, r)
	for range dropConcurrency {
		if id := await(t, h.entered); id == n {
			t.Fatal("the last row was released before the ones listed ahead of it")
		}
	}
	// Row n waits its turn: another server's claim takes its id and attempt.
	ctx := context.Background()
	if err := db.RecordClaim(ctx, store.ServerJob{ID: n, Kind: h.Kind(), Attempt: 1, ServerID: "other", Pairing: "p2"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, func() bool { return rowsIn(db, n-1, store.JobLost) && !r.holds(n) })
	if got := h.abandoned(); len(got) != n-1 || slices.Contains(got, n) {
		t.Fatal("released:", got)
	}
	if sj, err := db.GetServerJob(ctx, n); err != nil || sj.State != store.JobRunning || sj.Pairing != "p2" || sj.ServerID != "other" {
		t.Fatalf("the new claim's row: %+v %v", sj, err)
	}
}
// A dropped job whose id another server's claim of another kind took while
// it waited its turn still has its work released (denied: left alone as a
// claim of its own kind is, its work then released by nothing, as that
// claim's handler keeps none of the job's state), and the new claim's row is
// left alone (allowed: never recorded lost for the dropped job).
func TestDroppedJobWhoseIDAnotherKindTookIsStillReleased(t *testing.T) {
	const n = dropConcurrency + 1
	release := make(chan struct{})
	h := newCountingAbandoner(nil, release, n)
	db := openStore(t)
	droppedRows(t, db, h.Kind(), n)
	r := New(db, func() Server { return nil }, nil, testOpts(), h) // unpaired since
	startControlled(t, r)
	for range dropConcurrency {
		if id := await(t, h.entered); id == n {
			t.Fatal("the last row was released before the ones listed ahead of it")
		}
	}
	// Row n waits its turn: another server's claim of another kind takes its id.
	ctx := context.Background()
	if err := db.RecordClaim(ctx, store.ServerJob{ID: n, Kind: "sidecars.generate", Attempt: 1, ServerID: "other", Pairing: "p2"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, func() bool { return rowsIn(db, n-1, store.JobLost) && !r.holds(n) })
	if got := h.abandoned(); len(got) != n || !slices.Contains(got, n) {
		t.Fatal("released:", got)
	}
	if sj, err := db.GetServerJob(ctx, n); err != nil || sj.State != store.JobRunning || sj.Kind != "sidecars.generate" || sj.ServerID != "other" {
		t.Fatalf("the new claim's row: %+v %v", sj, err)
	}
}


// A stop interrupts a job whose pairing is ended on the page just after its
// end was settled (left running to resume): the stop never waits for its work
// to be released (denied: a release on a context the stop doesn't end, up to
// 2 minutes), and the next start, its pairing gone from the store, drops it
// (released, recorded lost) rather than resuming it.
func TestStopNeverWaitsForAnUnpairedJobsRelease(t *testing.T) {
	srv := newControlledServer(false)
	srv.pairing = "p1"
	h := newCountingAbandoner(func(ctx context.Context, _ Job, _ *Reporter) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil, 1) // waits for its context to end
	db := openStore(t)
	var cur atomic.Pointer[controlledServer]
	cur.Store(srv)
	var r *Runner
	// The unpair lands as settle leaves the job running (its log line), before
	// release.
	hook := &hookLog{on: "bridge: a job was interrupted; it resumes at the next start", fn: func() {
		cur.Store(nil)
		r.Unpaired(srv)
	}}
	opts := testOpts()
	opts.Pairing = linkPairing(&cur)
	opts.Log = slog.New(hook)
	r = New(db, linkOf(&cur), nil, opts, h)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	srv.next(t, false).answer <- claimed(1, h.Kind(), 0)
	waitFor(t, func() bool { return h.ran.Load() && srv.triesOf(1) > 0 })
	stop()
	await(t, done)
	if !hook.fired.Load() {
		t.Fatal("the job wasn't interrupted")
	}
	if sj, err := db.GetServerJob(context.Background(), 1); err != nil || sj.State != store.JobRunning {
		t.Fatalf("a job the stop interrupted: %+v %v, want left running for the next start", sj, err)
	}
	// The next start: unpaired, the row of the ended pairing is dropped.
	next := &abandoner{handler: h.handler}
	runFor(t, New(db, func() Server { return nil }, nil, testOpts(), next), func() bool {
		sj, err := db.GetServerJob(context.Background(), 1)
		return err == nil && sj.State == store.JobLost && sj.Error == errPairingEnded.Error()
	})
	if got := next.abandoned(); len(got) != 1 || got[0] != 1 {
		t.Fatal("the next start didn't release its work once", got)
	}
	if s, _ := srv.finalState(1); s != "" {
		t.Fatal("the unpaired server was sent its end", s)
	}
}

// hookLog calls fn, once, when message on is logged.
type hookLog struct {
	on    string
	fn    func()
	fired atomic.Bool
}

func (h *hookLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *hookLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.on && h.fired.CompareAndSwap(false, true) {
		h.fn()
	}
	return nil
}
func (h *hookLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *hookLog) WithGroup(string) slog.Handler      { return h }

// A start drops the open rows of an earlier pairing before it resumes
// anything, as the pairing is the store's, not this process's: unpaired since
// (the Bridge unpaired, maybe paired again with the same server: a new
// pairing) or paired with another server, a running row is recorded lost with
// its drop's reason and released, never resumed (denied), and a finished one
// is never reported; the pairing's own row resumes (allowed: a plain restart,
// or an upgrade whose rows the app gave the pairing in place).
func TestEarlierPairingsRowsAreDroppedAtStart(t *testing.T) {
	for name, tc := range map[string]struct {
		pairing, server string // the current pairing (pairing "": unpaired)
		resumes         bool
		why             error
	}{
		"unpaired since":                {why: errPairingEnded},
		"paired again with the server":  {pairing: "p2", server: testServer, why: errPairingEnded},
		"paired with another server":    {pairing: "p2", server: "other", why: errOtherServer},
		"the same pairing (or upgrade)": {pairing: "p1", server: testServer, resumes: true},
	} {
		t.Run(name, func(t *testing.T) {
			started := make(chan Job, 2)
			h := &abandoner{handler: blockingHandler("podcast.fetch", LaneDownload, started, nil)}
			db := openStore(t)
			ctx := context.Background()
			for _, id := range []int64{5, 6} {
				if err := db.RecordClaim(ctx, store.ServerJob{ID: id, Kind: h.Kind(), Attempt: 1, ServerID: testServer, Pairing: "p1"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.FinishServerJob(ctx, 6, 1, store.JobDone, nil, ""); err != nil {
				t.Fatal(err)
			}
			var cur atomic.Pointer[controlledServer]
			var srv *controlledServer
			if tc.pairing != "" {
				srv = newControlledServer(false)
				srv.id, srv.pairing = tc.server, tc.pairing
				cur.Store(srv)
			}
			opts := testOpts()
			opts.Pairing = linkPairing(&cur)
			r := New(db, linkOf(&cur), nil, opts, h)
			startControlled(t, r)
			if tc.resumes {
				if j := await(t, started); j.ID != 5 {
					t.Fatal("resumed", j.ID)
				}
				waitFor(t, func() bool { return srv.triesOf(6) > 0 }) // its end is reported
				return
			}
			waitFor(t, func() bool {
				sj, err := db.GetServerJob(ctx, 5)
				return err == nil && sj.State == store.JobLost && sj.Error == tc.why.Error()
			})
			if got := h.abandoned(); len(got) != 1 || got[0] != 5 {
				t.Fatal("the dropped row's work wasn't released once", got)
			}
			// Marked after row 5 is recorded lost (the sweep goes in id order).
			waitFor(t, func() bool {
				sj, err := db.GetServerJob(ctx, 6)
				return err == nil && sj.Reported
			})
			if srv != nil {
				srv.next(t, false) // claiming: past the start's look
				time.Sleep(5 * opts.RetryEvery)
				if srv.triesOf(5) != 0 || srv.triesOf(6) != 0 {
					t.Fatal("an earlier pairing's job was reported", srv.triesOf(5), srv.triesOf(6))
				}
			}
			select {
			case j := <-started:
				t.Fatal("an earlier pairing's job resumed", j)
			default:
			}
		})
	}
}

// A job whose pairing the app has forgotten (an unpair on the page, before
// Unpaired reaches its run) reports nothing more: its end is dropped, lost
// and never sent (denied), and a claim its link answers then is never
// recorded or run.
func TestRunOfAForgottenPairingIsDropped(t *testing.T) {
	srv := newControlledServer(false)
	srv.pairing = "p1"
	started := make(chan Job, 2)
	gates := map[int64]chan struct{}{1: make(chan struct{})}
	h := &abandoner{handler: blockingHandler("podcast.fetch", LaneDownload, started, gates)}
	db := openStore(t)
	var cur atomic.Pointer[controlledServer]
	cur.Store(srv)
	opts := testOpts()
	opts.Pairing = linkPairing(&cur)
	r := New(db, func() Server { return srv }, nil, opts, h)
	startControlled(t, r)
	srv.next(t, false).answer <- claimed(1, h.Kind(), 0)
	await(t, started)
	waitFor(t, func() bool { return srv.triesOf(1) > 0 })
	cur.Store(nil)  // the pairing is forgotten; Unpaired hasn't come yet
	close(gates[1]) // the job ends well
	ctx := context.Background()
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(ctx, 1)
		return err == nil && sj.State == store.JobLost && sj.Error == errPairingEnded.Error()
	})
	if s, _ := srv.finalState(1); s != "" {
		t.Fatal("the end of a job of a forgotten pairing was reported", s)
	}
	var wg sync.WaitGroup
	r.start(ctx, srv, Job{ID: 2, Kind: h.Kind(), Attempt: 1}, &wg, nil)
	wg.Wait()
	if _, err := db.GetServerJob(ctx, 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a claim answered on a forgotten pairing's link was recorded", err)
	}
}

// unreadableStore is a store whose open jobs can't be read while fail is set.
type unreadableStore struct {
	*store.DB
	fail   atomic.Bool
	failed atomic.Int32 // the reads failed so far
}

func (s *unreadableStore) OpenServerJobs(ctx context.Context) ([]store.ServerJob, error) {
	if s.fail.Load() {
		s.failed.Add(1)
		return nil, errors.New("database is locked")
	}
	return s.DB.OpenServerJobs(ctx)
}

// The look for an earlier pairing's rows that can't read the store looks again
// once the store reads, though the Bridge is paired again with the same server
// meanwhile: the row is dropped, released once and reported to neither link
// (denied: the look skipped once paired, the row left for the next start to
// resume). The new pairing's rows are left alone (allowed): its run, and a row
// no run holds (a run whose end couldn't be recorded), which only a start
// resumes.
func TestPairingLookRunsOnceTheStoreReads(t *testing.T) {
	first, second := newControlledServer(false), newControlledServer(false)
	first.pairing, second.pairing = "p1", "p2"
	started := make(chan Job, 4)
	h := &abandoner{handler: blockingHandler("podcast.fetch", LaneDownload, started, map[int64]chan struct{}{})}
	db := &unreadableStore{DB: openStore(t)}
	var cur atomic.Pointer[controlledServer]
	cur.Store(first)
	opts := testOpts()
	opts.Pairing = linkPairing(&cur)
	r := New(db, linkOf(&cur), nil, opts, h)
	startControlled(t, r)
	first.next(t, false) // past recover: the claim loop runs
	ctx := context.Background()
	// A row no run holds (its run's end couldn't be recorded).
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 5, Kind: h.Kind(), Attempt: 1, ServerID: testServer, Pairing: "p1"}); err != nil {
		t.Fatal(err)
	}
	db.fail.Store(true)
	cur.Store(nil)
	r.Unpaired(first)
	waitFor(t, func() bool { return db.failed.Load() > 0 }) // the look tried
	if sj, err := db.GetServerJob(ctx, 5); err != nil || sj.State != store.JobRunning {
		t.Fatalf("a row dropped though the store couldn't be read: %+v %v", sj, err)
	}
	cur.Store(second) // paired again with the same server, the store still unreadable
	r.Wake()
	second.next(t, false).answer <- claimed(7, h.Kind(), 0)
	await(t, started)
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 8, Kind: h.Kind(), Attempt: 1, ServerID: testServer, Pairing: "p2"}); err != nil {
		t.Fatal(err)
	}
	db.fail.Store(false)
	r.Wake()
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(ctx, 5)
		return err == nil && sj.State == store.JobLost
	})
	if got := h.abandoned(); len(got) != 1 || got[0] != 5 {
		t.Fatal("the dropped row's work wasn't released once", got)
	}
	for _, id := range []int64{7, 8} {
		if sj, err := db.GetServerJob(ctx, id); err != nil || sj.State != store.JobRunning {
			t.Fatalf("the new pairing's row %d: %+v %v, want running", id, sj, err)
		}
	}
	if first.triesOf(5) != 0 || second.triesOf(5) != 0 {
		t.Fatal("the dropped job was reported")
	}
}

// laggingListStore answers its second list of the open jobs (a start's
// recover, after its look) as read before the look's release of row id was
// recorded: it lets that release go on (release) once it has read the rows,
// and answers only once the row is recorded lost.
type laggingListStore struct {
	*store.DB
	id      int64
	release chan struct{}
	n       atomic.Int32
}

func (s *laggingListStore) OpenServerJobs(ctx context.Context) ([]store.ServerJob, error) {
	list, err := s.DB.OpenServerJobs(ctx)
	if s.n.Add(1) != 2 {
		return list, err
	}
	close(s.release)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if sj, gerr := s.GetServerJob(ctx, s.id); gerr == nil && sj.State == store.JobLost {
			break
		}
	}
	return list, err
}

// A start's recover never drops again a row its look is releasing, even one
// recorded lost (and no longer held) after recover read the rows (denied: its
// work released twice, its row recorded lost again).
func TestRecoverLeavesARowTheLookReleased(t *testing.T) {
	release := make(chan struct{})
	h := newCountingAbandoner(nil, release, 2)
	db := &laggingListStore{DB: openStore(t), id: 1, release: release}
	droppedRows(t, db.DB, h.Kind(), 1)
	srv := newControlledServer(false)
	srv.pairing = "p2" // paired again with the same server: a new pairing
	var cur atomic.Pointer[controlledServer]
	cur.Store(srv)
	opts := testOpts()
	opts.Pairing = linkPairing(&cur)
	opts.RetryEvery = time.Hour // retryReports never lists the rows
	r := New(db, linkOf(&cur), nil, opts, h)
	startControlled(t, r)
	srv.next(t, false) // recover is done: the dispatchers claim
	waitFor(t, func() bool { return !r.holds(1) })
	if got := h.abandoned(); len(got) != 1 {
		t.Fatal("released:", got)
	}
}

// staleListStore lists the open jobs as an earlier read had them (list), while
// its rows are what claims wrote since: the first list only (the start's
// look), none after it. listed hears each list.
type staleListStore struct {
	*store.DB
	list   []store.ServerJob
	n      atomic.Int32
	listed chan int32
}

func (s *staleListStore) OpenServerJobs(context.Context) ([]store.ServerJob, error) {
	n := s.n.Add(1)
	select {
	case s.listed <- n:
	default:
	}
	if n > 1 {
		return nil, nil
	}
	return s.list, nil
}

// The look for an earlier pairing's rows reads each row again before it drops
// it: a claim of the new pairing (another server's, job ids are each server's
// own) that took the row under the listed id since the list was read, and ran
// and ended, is never Abandoned, recorded lost or marked reported for the
// listed one (denied).
func TestPairingLookLeavesARowANewClaimTook(t *testing.T) {
	db := &staleListStore{DB: openStore(t), listed: make(chan int32, 8)}
	h := &abandoner{handler: handler{kind: "podcast.fetch", lane: LaneDownload, fn: func(context.Context, Job, *Reporter) (any, error) {
		return nil, nil
	}}}
	ctx := context.Background()
	for _, id := range []int64{5, 6} {
		if err := db.RecordClaim(ctx, store.ServerJob{ID: id, Kind: h.Kind(), Attempt: 1, ServerID: "other", Pairing: "p2"}); err != nil {
			t.Fatal(err)
		}
		db.list = append(db.list, store.ServerJob{ID: id, Kind: h.Kind(), Attempt: 1, ServerID: testServer, Pairing: "p1", State: store.JobRunning})
	}
	if err := db.FinishServerJob(ctx, 6, 1, store.JobDone, nil, ""); err != nil {
		t.Fatal(err)
	}
	db.list[1].State = store.JobDone
	next := newControlledServer(false)
	next.id, next.pairing = "other", "p2"
	var cur atomic.Pointer[controlledServer]
	cur.Store(next)
	opts := testOpts()
	opts.Pairing = linkPairing(&cur)
	opts.RetryEvery = time.Hour // retryReports never lists the rows
	r := New(db, linkOf(&cur), nil, opts, h)
	startControlled(t, r)
	// The handshake: the claim loop lists the rows again to resume them (recover)
	// only once the start's look is done, a drop it decided held (holdsLocked).
	if n := await(t, db.listed); n != 1 {
		t.Fatal("the first list was", n)
	}
	await(t, db.listed)
	if got := h.abandoned(); len(got) != 0 || r.holds(5) {
		t.Fatal("the new claim's work was released, or is being released, for the listed row", got)
	}
	if sj, err := db.GetServerJob(ctx, 5); err != nil || sj.State != store.JobRunning || sj.Pairing != "p2" {
		t.Fatalf("the new claim's running row: %+v %v", sj, err)
	}
	if sj, err := db.GetServerJob(ctx, 6); err != nil || sj.State != store.JobDone || sj.Reported {
		t.Fatalf("the new claim's end: %+v %v, want done and still to be reported", sj, err)
	}
}

// A job the Bridge's stop interrupts, its pairing ended on the page before its
// end is settled, is never released on its run's goroutine, which the stop
// waits for (denied: a release on a context the stop doesn't end, up to 2
// minutes): it is left running, and the next start, its pairing gone from the
// store, drops it (released, recorded lost) rather than resuming it (denied);
// one the stop leaves running with no unpair is kept to resume (allowed:
// TestStopAndResume's rule).
func TestUnpairWhileStoppingDropsTheJob(t *testing.T) {
	srv := newControlledServer(false)
	srv.pairing = "p1"
	var cur atomic.Pointer[controlledServer]
	cur.Store(srv)
	stopped, settle := make(chan struct{}), make(chan struct{})
	h := newCountingAbandoner(func(ctx context.Context, _ Job, _ *Reporter) (any, error) {
		<-ctx.Done()
		close(stopped)
		<-settle // the unpair comes as the stop is under way
		return nil, ctx.Err()
	}, nil, 1) // a release waits for its context to end
	db := openStore(t)
	opts := testOpts()
	opts.Pairing = linkPairing(&cur)
	r := New(db, linkOf(&cur), nil, opts, h)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	srv.next(t, false).answer <- claimed(1, h.Kind(), 0)
	waitFor(t, func() bool { return h.ran.Load() && srv.triesOf(1) > 0 })
	stop()
	await(t, stopped)
	cur.Store(nil) // the unpair forgets the pairing first
	r.Unpaired(srv)
	close(settle)
	await(t, done)
	if got := h.abandoned(); len(got) != 0 {
		t.Fatal("its work was released as the Bridge stopped", got)
	}
	if sj, err := db.GetServerJob(context.Background(), 1); err != nil || sj.State != store.JobRunning {
		t.Fatalf("a job unpaired as the Bridge stopped: %+v %v, want left running for the next start", sj, err)
	}
	// The next start: unpaired, the row of the ended pairing is dropped.
	next := &abandoner{handler: h.handler}
	runFor(t, New(db, func() Server { return nil }, nil, testOpts(), next), func() bool {
		sj, err := db.GetServerJob(context.Background(), 1)
		return err == nil && sj.State == store.JobLost && sj.Error == errPairingEnded.Error()
	})
	if got := next.abandoned(); len(got) != 1 || got[0] != 1 || next.ran.Load() {
		t.Fatal("the next start didn't release its kept work once, or resumed it", got)
	}
	if s, _ := srv.finalState(1); s != "" {
		t.Fatal("the unpaired server was sent its end", s)
	}
}

// A job on its way here when its pairing is ended on the page (a claim read
// just before, a resume racing the unpair) is launched already ended: it
// never runs and reports nothing (denied), and is recorded lost.
func TestJobLaunchedOnAnUnpairedLinkEndsAtOnce(t *testing.T) {
	srv := newControlledServer(false)
	h := &abandoner{handler: handler{kind: "podcast.fetch", lane: LaneDownload, fn: func(context.Context, Job, *Reporter) (any, error) {
		return nil, nil
	}}}
	db := openStore(t)
	r := New(db, func() Server { return nil }, nil, testOpts(), h)
	ctx := context.Background()
	r.Unpaired(srv)
	// The claim passed start's look before the unpair and is recorded after it
	// (a row there before it is the claim loop's: TestUnpairDropsRowsNoRunHolds).
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 5, Kind: h.Kind(), Attempt: 1, ServerID: testServer}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	r.launch(ctx, srv, Job{serverID: testServer, ID: 5, Kind: h.Kind(), Attempt: 1, Resumed: true}, &wg, "", nil, nil)
	wg.Wait()
	r.drops.Wait() // its release, off its run's goroutine
	if sj, err := db.GetServerJob(ctx, 5); err != nil || sj.State != store.JobLost {
		t.Fatalf("the job launched on an unpaired link: %+v %v, want lost", sj, err)
	}
	if h.ran.Load() || srv.triesOf(5) != 0 {
		t.Fatal("a job launched on an unpaired link ran or reported", h.ran.Load(), srv.triesOf(5))
	}
	if got := h.abandoned(); len(got) != 1 || got[0] != 5 {
		t.Fatal("the job that never ran wasn't released", got)
	}
}

func TestRecoveryAdmissionRenewsAndReservesInteractive(t *testing.T) {
	srv := newControlledServer(true)
	db := openStore(t)
	for _, id := range []int64{1, 2, 3} {
		if err := db.RecordClaim(context.Background(), store.ServerJob{ServerID: testServer, ID: id, Kind: "podcast.fetch", Attempt: 1}); err != nil {
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

// bridge#13: a job claimed into the reserve (interactive priority) resumes there
// after a restart, its priority recorded with the claim, where it used to wait in
// the ordinary lane behind background work; one of lower priority resumes in the
// ordinary lane, and a pairing without the reserve takes both there.
func TestRecoveredInteractiveJobResumesInTheReserve(t *testing.T) {
	for _, negotiated := range []bool{true, false} {
		t.Run(map[bool]string{true: "negotiated", false: "legacy"}[negotiated], func(t *testing.T) {
			srv := newControlledServer(negotiated)
			db := openStore(t)
			for id, priority := range map[int64]int{1: wirePriority(t), 2: 20} {
				if err := db.RecordClaim(context.Background(), store.ServerJob{ServerID: testServer, ID: id, Kind: "podcast.fetch", Attempt: 1,
					Priority: priority}); err != nil {
					t.Fatal(err)
				}
			}
			if sj, err := db.GetServerJob(context.Background(), 1); err != nil || sj.Priority != wirePriority(t) {
				t.Fatalf("the claim's priority wasn't recorded: %+v, %v", sj, err)
			}
			started := make(chan Job, 4)
			gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
			opts := testOpts()
			opts.Lanes = DefaultLanes()
			r := New(db, func() Server { return srv }, nil, opts,
				optedHandler{blockingHandler("podcast.fetch", LaneDownload, started, gates)})
			startControlled(t, r)
			await(t, started)
			await(t, started)
			want := map[int64]string{1: LaneInteractive, 2: LaneDownload}
			if !negotiated {
				want[1] = LaneDownload // a legacy pairing has no reserve
			}
			assertPools(t, r, want)
		})
	}
}

// A resumed job waiting for the reserve when the negotiation is withdrawn waits
// in its kind's ordinary lane instead, where it runs once there is room.
func TestRecoveredInteractiveJobFallsBackWhenTheReserveGoes(t *testing.T) {
	srv := newControlledServer(true)
	db := openStore(t)
	if err := db.RecordClaim(context.Background(), store.ServerJob{ServerID: testServer, ID: 1, Kind: "podcast.fetch", Attempt: 1,
		Priority: wirePriority(t)}); err != nil {
		t.Fatal(err)
	}
	started := make(chan Job, 2)
	h := optedHandler{blockingHandler("podcast.fetch", LaneDownload, started, nil)}
	opts := testOpts()
	opts.Lanes = DefaultLanes()
	r := New(db, func() Server { return srv }, nil, opts, h)
	r.use[LaneInteractive] = 1 // the reserve is busy: the resumed job waits for it
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	r.launch(ctx, srv, Job{serverID: testServer, ID: 1, Kind: h.Kind(), Attempt: 1, Priority: wirePriority(t), Resumed: true}, &wg, "", nil, nil)
	await(t, srv.updated)
	if _, waiting := laneCounts(r, LaneInteractive); waiting != 1 {
		t.Fatal("the resumed job doesn't wait for the reserve")
	}
	srv.WithdrawClaimMinPriority()
	r.Wake()
	await(t, started)
	assertPools(t, r, map[int64]string{1: LaneDownload})
	if _, waiting := laneCounts(r, LaneInteractive); waiting != 0 {
		t.Fatal("the reserve still counts the job waiting", waiting)
	}
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
	narrow.answer <- nil // the freed slot leaves it in flight (BR-CLAIM-HOLD)
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
			if err := db.RecordClaim(ctx, store.ServerJob{ServerID: testServer, ID: 9, Kind: "light", Attempt: 1}); err != nil {
				t.Fatal(err)
			}
			h := &abandoner{handler: handler{kind: "light", lane: LaneLight, fn: func(context.Context, Job, *Reporter) (any, error) { return nil, nil }}}
			opts := testOpts()
			opts.Lanes = map[string]int{LaneLight: 1}
			r := New(db, func() Server { return srv }, nil, opts, h)
			r.use[LaneLight] = 1
			var wg sync.WaitGroup
			r.launch(ctx, srv, Job{serverID: testServer, ID: 9, Kind: h.Kind(), Attempt: 1, Resumed: true}, &wg, "", nil, nil)
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

// bridge#13: a job waiting for its first slot renews its lease only while the
// Bridge has a link to its server. Unpaired, it stops renewing (the server may
// hand it out again). Paired again with the same server (a new link), it
// renews there and runs there once a slot frees (allowed); paired with
// another server, it is dropped, never reported there (denied).
func TestWaitingJobLetsGoOfAnOldLink(t *testing.T) {
	for name, nextID := range map[string]string{"same server": "a", "another server": "b"} {
		t.Run(name, func(t *testing.T) {
			db, old, next := openStore(t), newControlledServer(false), newControlledServer(false)
			old.id, next.id = "a", nextID
			if err := db.RecordClaim(context.Background(), store.ServerJob{ID: 9, Kind: "light", Attempt: 1, ServerID: "a"}); err != nil {
				t.Fatal(err)
			}
			var current atomic.Pointer[controlledServer]
			current.Store(old)
			h := &abandoner{handler: handler{kind: "light", lane: LaneLight, fn: func(context.Context, Job, *Reporter) (any, error) { return nil, nil }}}
			opts := testOpts()
			opts.Lanes = map[string]int{LaneLight: 1}
			r := New(db, func() Server {
				if srv := current.Load(); srv != nil {
					return srv
				}
				return nil
			}, nil, opts, h)
			r.use[LaneLight] = 1 // the lane is full: the job waits
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			t.Cleanup(func() { cancel(); wg.Wait() })
			r.launch(ctx, old, Job{ID: 9, Kind: h.Kind(), Attempt: 1, Resumed: true, serverID: "a"}, &wg, "", nil, nil)
			for range 2 {
				await(t, old.updated) // the first report, then a renewal while it waits
			}
			current.Store(nil) // unpaired
			r.Wake()
			time.Sleep(5 * opts.Keepalive) // a renewal already under way may still land
			for drained := false; !drained; {
				select {
				case <-old.updated:
				default:
					drained = true
				}
			}
			time.Sleep(5 * opts.Keepalive)
			select {
			case <-old.updated:
				t.Fatal("an unpaired Bridge renewed a waiting job's lease")
			default:
			}
			current.Store(next) // paired again
			r.Wake()
			if nextID == "b" {
				wg.Wait()
				r.drops.Wait() // its release, off its run's goroutine
				if sj, _ := db.GetServerJob(context.Background(), 9); sj.State != store.JobLost || h.ran.Load() ||
					!slices.Equal(h.abandoned(), []int64{9}) {
					t.Fatal("another server's waiting job wasn't dropped", sj.State, h.ran.Load(), h.abandoned())
				}
				select {
				case <-next.updated:
					t.Fatal("the old server's job reported on the new server's link")
				default:
				}
				return
			}
			await(t, next.updated) // renewed on the new link to its server
			r.mu.Lock()
			r.use[LaneLight] = 0
			r.mu.Unlock()
			r.Wake()
			wg.Wait()
			if state, _ := next.finalState(9); state != link.StateDone || !h.ran.Load() {
				t.Fatal("the waiting job didn't run on the new link to its server", state, h.ran.Load())
			}
			select {
			case <-old.updated:
				t.Fatal("the job went on reporting on the old link")
			default:
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
	r.launch(ctx, srv, Job{serverID: testServer, ID: 4, Kind: h.Kind(), Attempt: 1, Resumed: true}, &wg, "", nil, nil)
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

// A legacy pairing folds the interactive reserve into the download lane, and
// the page doesn't list an "interactive 0 of 0"; a negotiated one lists both.
func TestLanesLeaveOutAnEmptyFoldedReserve(t *testing.T) {
	t.Parallel()
	lanes := func(srv Server) map[string]int {
		opts := testOpts()
		opts.Lanes = DefaultLanes()
		r := New(openStore(t), func() Server { return srv }, nil, opts)
		out := map[string]int{}
		for _, l := range r.Lanes() {
			out[l.Name] = l.Capacity
		}
		return out
	}
	legacy := lanes(newFakeServer())
	if _, ok := legacy[LaneInteractive]; ok || legacy[LaneDownload] != DefaultLanes()[LaneDownload]+DefaultLanes()[LaneInteractive] {
		t.Fatalf("legacy lanes %v", legacy)
	}
	negotiated := lanes(newControlledServer(true))
	if negotiated[LaneInteractive] != DefaultLanes()[LaneInteractive] || negotiated[LaneDownload] != DefaultLanes()[LaneDownload] {
		t.Fatalf("negotiated lanes %v", negotiated)
	}
}

// laneCounts reads a lane's use and waiting counts.
func laneCounts(r *Runner, lane string) (use, waiting int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.use[lane], r.waiting[lane]
}

// A job that parks and yields gives its download slot to the next download
// job; its Reclaim waits for that job to end and queues first; a cancel while
// it is yielded or reclaiming leaves the lane's counts right.
func TestYieldFreesSlotAndReclaimWaits(t *testing.T) {
	for _, mode := range []string{"reclaim", "cancel_yielded", "cancel_reclaiming"} {
		t.Run(mode, func(t *testing.T) {
			srv := newControlledServer(false)
			yielded, goOn, reclaimed, finish := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan struct{})
			parker := handler{kind: "lane.parker", lane: LaneDownload, fn: func(ctx context.Context, _ Job, rep *Reporter) (any, error) {
				rep.Park("not_writable", "Waiting for the mapping")
				rep.Yield()
				rep.Yield() // a second Yield changes nothing
				if !rep.Yielded() {
					t.Error("not yielded after Yield")
				}
				close(yielded)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-goOn:
				}
				rep.Unpark()
				err := rep.Reclaim(ctx)
				if err == nil && rep.Yielded() {
					t.Error("still yielded after Reclaim")
				}
				reclaimed <- err
				if err != nil {
					return nil, err
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-finish:
				}
				return nil, nil
			}}
			started := make(chan Job, 4)
			gate := map[int64]chan struct{}{2: make(chan struct{})}
			other := blockingHandler("lane.other", LaneDownload, started, gate)
			opts := testOpts()
			opts.Lanes = map[string]int{LaneDownload: 1, LaneLight: 1}
			r := New(openStore(t), func() Server { return srv }, nil, opts, parker, other, handler{kind: "light", lane: LaneLight})
			startControlled(t, r)
			first := srv.next(t, false)
			if !slices.Contains(first.kinds, "lane.other") {
				t.Fatal(first.kinds)
			}
			first.answer <- claimed(1, parker.Kind(), 0)
			await(t, yielded)
			if use, waiting := laneCounts(r, LaneDownload); use != 0 || waiting != 0 {
				t.Fatalf("yielded: use %d waiting %d", use, waiting)
			}
			if mode == "cancel_yielded" {
				if !r.Cancel(1) {
					t.Fatal("no job 1")
				}
				waitFor(t, func() bool { // the cancelled job's end
					r.mu.Lock()
					defer r.mu.Unlock()
					return len(r.running) == 0 && r.use[LaneDownload] == 0 && r.waiting[LaneDownload] == 0
				})
				return
			}
			// The lane takes another download job meanwhile.
			var c *controlledClaim
			for c == nil || !slices.Contains(c.kinds, "lane.other") {
				if c != nil {
					c.answer <- nil // the yield leaves a narrow claim in flight (BR-CLAIM-HOLD)
				}
				c = srv.next(t, false)
			}
			c.answer <- claimed(2, other.Kind(), 0)
			await(t, started)
			close(goOn)
			waitFor(t, func() bool { // the reclaim queued
				use, waiting := laneCounts(r, LaneDownload)
				return use == 1 && waiting == 1
			})
			select {
			case err := <-reclaimed:
				t.Fatalf("reclaimed while the other job holds the slot (%v)", err)
			case <-time.After(50 * time.Millisecond):
			}
			if mode == "cancel_reclaiming" {
				r.Cancel(1)
				if err := await(t, reclaimed); err == nil {
					t.Fatal("a cancelled reclaim succeeded")
				}
				waitFor(t, func() bool { // the cancelled job's end
					use, waiting := laneCounts(r, LaneDownload)
					return use == 1 && waiting == 0
				})
				close(gate[2])
				waitFor(t, func() bool { // the lane empty
					use, waiting := laneCounts(r, LaneDownload)
					return use == 0 && waiting == 0
				})
				return
			}
			close(gate[2])
			if err := await(t, reclaimed); err != nil {
				t.Fatal(err)
			}
			if use, waiting := laneCounts(r, LaneDownload); use != 1 || waiting != 0 {
				t.Fatalf("reclaimed: use %d waiting %d", use, waiting)
			}
			close(finish)
			waitFor(t, func() bool { // the lane empty
				use, waiting := laneCounts(r, LaneDownload)
				return use == 0 && waiting == 0
			})
		})
	}
}

// A stray (a claim answered outside its admission) holds its dispatcher only
// while it waits for its first slot: once it yielded, neither the yield nor
// its wait in Reclaim behind another download stops the claims for the other
// lanes.
func TestYieldedStrayFreesItsDispatcher(t *testing.T) {
	srv := newControlledServer(false)
	opts := testOpts()
	opts.Lanes = map[string]int{LaneDownload: 1, LaneLight: 1}
	r := New(openStore(t), func() Server { return srv }, nil, opts)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ru := &run{job: Job{serverID: testServer, ID: 1}, lane: LaneDownload, cancel: cancel, stray: true, strayReserve: true}
	r.mu.Lock()
	r.running[1] = ru
	r.waiting[LaneDownload]++
	if !r.strayWaitingLocked(true) {
		t.Error("a waiting stray doesn't hold its dispatcher")
	}
	r.mu.Unlock()
	if !r.awaitAdmission(ctx, ru) {
		t.Fatal("not admitted")
	}
	rep := &Reporter{r: r, ru: ru}
	rep.Yield()
	r.mu.Lock()
	held := r.strayWaitingLocked(true)
	r.use[LaneDownload]++ // another download took the slot
	r.mu.Unlock()
	if held {
		t.Fatal("a yielded stray holds its dispatcher")
	}
	reclaimed := make(chan error, 1)
	go func() { reclaimed <- rep.Reclaim(ctx) }()
	waitFor(t, func() bool {
		_, waiting := laneCounts(r, LaneDownload)
		return waiting == 1
	})
	r.mu.Lock()
	held = r.strayWaitingLocked(true)
	r.mu.Unlock()
	if held {
		t.Fatal("a reclaiming stray holds its dispatcher")
	}
	cancel(errCancelRequested)
	if err := await(t, reclaimed); err == nil {
		t.Fatal("a cancelled reclaim succeeded")
	}
	r.release(ru, false)
	if use, waiting := laneCounts(r, LaneDownload); use != 1 || waiting != 0 {
		t.Fatalf("released: use %d waiting %d", use, waiting)
	}
}

// A LocalReporter's Yield and Reclaim only record it, for Yielded.
func TestLocalReporterYield(t *testing.T) {
	rep := LocalReporter()
	if rep.Yielded() {
		t.Fatal("yielded before Yield")
	}
	rep.Yield()
	if !rep.Yielded() {
		t.Fatal("Yield unrecorded")
	}
	if err := rep.Reclaim(context.Background()); err != nil || rep.Yielded() {
		t.Fatal("Reclaim", err)
	}
}

// BR-CLAIM-HOLD: a lane slot freeing or filling leaves the claim in flight
// alone (cancelled, a reverse proxy that hides the hang-up lets the server
// hand the job out anyway, its answer lost until the 2-minute lease lapses);
// the job it brings runs in the slot the claim reserved, and the runner's stop
// still cancels a claim at once. A claim that leaves a full lane out waits only
// leftOutWait (denied: the whole long poll, so a queue in that lane drained a
// slot's worth per 25 s), one that leaves none out the whole wait (allowed).
func TestLaneChangesLeaveAnInFlightClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, db := newControlledServer(false), openStore(t)
		started := make(chan Job, 4)
		gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{}), 5: make(chan struct{})}
		opts := testOpts()
		opts.Wait = link.MaxWait
		opts.Lanes = map[string]int{LaneLight: 1, LaneDownload: 1}
		r := New(db, func() Server { return srv }, nil, opts,
			blockingHandler("download", LaneDownload, started, gates), blockingHandler("light", LaneLight, started, gates))
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		done := make(chan struct{})
		go func() { r.dispatch(ctx, false, &wg); close(done) }()
		first := srv.next(t, false)
		if first.wait != link.MaxWait {
			t.Fatalf("a claim with every lane free waits %v, want %v", first.wait, link.MaxWait)
		}
		first.answer <- claimed(1, "light", 0)
		await(t, started)
		inFlight := srv.next(t, false)
		if !slices.Equal(inFlight.kinds, []string{"download"}) {
			t.Fatal(inFlight.kinds)
		}
		if inFlight.wait != leftOutWait {
			t.Fatalf("a claim leaving the full light lane out waits %v, want %v", inFlight.wait, leftOutWait)
		}
		close(gates[1]) // the light slot frees
		synctest.Wait()
		// The download slot fills: a resumed job waits behind the claim's reservation.
		r.launch(ctx, srv, Job{serverID: testServer, ID: 5, Kind: "download", Attempt: 1, Resumed: true}, &wg, "", nil, nil)
		synctest.Wait()
		select {
		case <-inFlight.cancelled:
			t.Fatal("a lane change cancelled the claim in flight")
		default:
		}
		inFlight.answer <- claimed(2, "download", 0)
		if j := await(t, started); j.ID != 2 {
			t.Fatal("the held claim's job didn't run in its reserved slot", j)
		}
		if use, waiting := laneCounts(r, LaneDownload); use != 1 || waiting != 1 {
			t.Fatalf("download use %d waiting %d, want the claimed job running and the resumed one waiting", use, waiting)
		}
		stopped := srv.next(t, false)
		cancel()
		await(t, stopped.cancelled)
		await(t, done)
		wg.Wait()
	})
}

// The short wait is the light lane's alone, and bounded: a claim leaving the
// full download lane out waits the whole long poll (denied: a long download
// kept the server answering a claim every 4 s for its whole run), and
// maxShortPolls short waits in a row that claim nothing go back to the whole
// wait (denied: a light lane held by slow jobs did the same) until the light
// lane's room changes, which short waits again after its next job (allowed).
func TestShortClaimWaitsAreTheLightLanesAndCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, db := newControlledServer(false), openStore(t)
		started := make(chan Job, 4)
		gates := map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{}), 3: make(chan struct{})}
		opts := testOpts()
		opts.Wait = link.MaxWait
		opts.Lanes = map[string]int{LaneLight: 1, LaneDownload: 1}
		r := New(db, func() Server { return srv }, nil, opts,
			blockingHandler("download", LaneDownload, started, gates), blockingHandler("light", LaneLight, started, gates))
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		done := make(chan struct{})
		go func() { r.dispatch(ctx, false, &wg); close(done) }()
		srv.next(t, false).answer <- claimed(1, "download", 0)
		await(t, started)
		c := srv.next(t, false)
		if !slices.Equal(c.kinds, []string{"light"}) || c.wait != link.MaxWait {
			t.Fatalf("a claim leaving the full download lane out asks %v for %v, want the whole wait", c.kinds, c.wait)
		}
		c.answer <- claimed(2, "light", 0)
		await(t, started)
		close(gates[1]) // the download slot frees; the light lane stays full
		synctest.Wait()
		c = srv.next(t, false)
		if !slices.Equal(c.kinds, []string{"download"}) {
			t.Fatal(c.kinds)
		}
		for i := range maxShortPolls {
			if c.wait != leftOutWait {
				t.Fatalf("short poll %d waits %v, want %v", i+1, c.wait, leftOutWait)
			}
			c.answer <- nil
			c = srv.next(t, false)
		}
		if c.wait != link.MaxWait {
			t.Fatalf("after %d empty short polls the claim waits %v, want the whole wait", maxShortPolls, c.wait)
		}
		close(gates[2]) // the light slot frees under the claim in flight
		synctest.Wait()
		c.answer <- nil
		c = srv.next(t, false)
		if !slices.Equal(c.kinds, []string{"download", "light"}) || c.wait != link.MaxWait {
			t.Fatalf("a claim with every lane free asks %v for %v", c.kinds, c.wait)
		}
		c.answer <- claimed(3, "light", 0)
		await(t, started)
		c = srv.next(t, false)
		if c.wait != leftOutWait {
			t.Fatalf("the light lane full again: the claim waits %v, want %v", c.wait, leftOutWait)
		}
		cancel()
		await(t, c.cancelled)
		await(t, done)
		wg.Wait()
	})
}

// BR-CLAIM-HOLD: a job that arrives with no room left in its lane (the claim
// answered as a capacity change cancelled it) is held, its lease renewed while
// it waits, and runs once a slot frees; it is neither dropped nor run over the
// lane's capacity.
func TestClaimedJobWithoutRoomIsHeld(t *testing.T) {
	srv := newControlledServer(false)
	started := make(chan Job, 4)
	gates := map[int64]chan struct{}{2: make(chan struct{})}
	opts := testOpts()
	opts.Lanes = map[string]int{LaneLight: 1}
	r := New(openStore(t), func() Server { return srv }, nil, opts, blockingHandler("light", LaneLight, started, gates))
	startControlled(t, r)
	inFlight := srv.next(t, false)
	inFlight.cancelJob.Store(claimed(2, "light", 0)) // answered as the change cancels it
	r.setCapacity(LaneLight, 0)
	await(t, inFlight.cancelled)
	waitFor(t, func() bool { _, waiting := laneCounts(r, LaneLight); return waiting == 1 })
	for range 2 {
		if id := await(t, srv.updated); id != 2 { // its first report, then a renewal
			t.Fatal("update for", id)
		}
	}
	select {
	case j := <-started:
		t.Fatal("ran over the lane's capacity", j)
	default:
	}
	r.setCapacity(LaneLight, 1)
	if j := await(t, started); j.ID != 2 {
		t.Fatal(j)
	}
	close(gates[2])
	waitFor(t, func() bool { s, _ := srv.finalState(2); return s == link.StateDone })
}
