package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// fakeServer hands out queued jobs and records every update.
type fakeServer struct {
	mu       sync.Mutex
	queue    []*link.Job
	updates  map[int64][]link.Update
	claims   [][]string
	cancel   map[int64]bool   // answer cancel_requested for these
	notHeld  map[int64]bool   // answer 409 not_held for these
	failing  bool             // every update fails with a network error
	unauthed bool             // every call is a 401
	hang     chan struct{}    // updates wait for it to close (nil: none)
	changed  chan struct{}    // closed on every update
	paths    map[int64]string // the job's path in an update's answer (a book that moved)
	tries    map[int64]int    // every update asked, failed ones included
}

func newFakeServer() *fakeServer {
	return &fakeServer{updates: map[int64][]link.Update{}, cancel: map[int64]bool{}, notHeld: map[int64]bool{}, changed: make(chan struct{})}
}

func (f *fakeServer) add(j *link.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, j)
}

func (f *fakeServer) Claim(ctx context.Context, kinds []string, wait time.Duration) (*link.Job, error) {
	f.mu.Lock()
	f.claims = append(f.claims, kinds)
	if f.unauthed {
		f.mu.Unlock()
		return nil, link.ErrUnauthorized
	}
	for i, j := range f.queue {
		if slices.Contains(kinds, j.Kind) {
			f.queue = append(f.queue[:i], f.queue[i+1:]...)
			f.mu.Unlock()
			return j, nil
		}
	}
	f.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(20 * time.Millisecond):
	}
	return nil, nil
}

func (f *fakeServer) UpdateJob(ctx context.Context, id int64, u link.Update) (*link.Job, error) {
	f.mu.Lock()
	if hang := f.hang; hang != nil {
		f.mu.Unlock()
		select {
		case <-hang:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		f.mu.Lock()
	}
	defer f.mu.Unlock()
	if f.tries == nil {
		f.tries = map[int64]int{}
	}
	f.tries[id]++
	if f.unauthed {
		return nil, link.ErrUnauthorized
	}
	if f.failing {
		return nil, errors.New("connection refused")
	}
	f.updates[id] = append(f.updates[id], u)
	close(f.changed)
	f.changed = make(chan struct{})
	if f.notHeld[id] {
		return nil, link.ErrNotHeld
	}
	state := ""
	if u.State != nil {
		state = *u.State
	}
	j := &link.Job{ID: id, State: state, CancelRequested: f.cancel[id]}
	if p, ok := f.paths[id]; ok {
		j.Path = &p
	}
	return j, nil
}

// finalState is the last state reported for id ("" for none).
func (f *fakeServer) finalState(id int64) (string, link.Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ups := f.updates[id]
	for i := len(ups) - 1; i >= 0; i-- {
		if s := ups[i].State; s != nil && *s != link.StateRunning {
			return *s, ups[i]
		}
	}
	return "", link.Update{}
}

// handler is a test Handler: it runs fn.
type handler struct {
	kind, lane string
	fn         func(ctx context.Context, j Job, rep *Reporter) (any, error)
}

func (h handler) Kind() string { return h.kind }
func (h handler) Lane() string { return h.lane }
func (h handler) Run(ctx context.Context, j Job, rep *Reporter) (any, error) {
	return h.fn(ctx, j, rep)
}

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testOpts() Options {
	return Options{Keepalive: 20 * time.Millisecond, ProgressEvery: time.Millisecond, Wait: 10 * time.Millisecond,
		RetryEvery: 20 * time.Millisecond, Log: quietLog()}
}

// runFor runs r until cond holds (or fails the test after 5 s), then stops it.
func runFor(t *testing.T, r *Runner, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func lib(id int64) *int64 { return &id }

func TestRunsAJobToDone(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 1, Kind: "bridge.verify", LibraryID: lib(2), Params: json.RawMessage(`{"x":1}`), Attempts: 1})
	var got Job
	h := handler{kind: "bridge.verify", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		got = j
		rep.Progress(Progress{Step: "stat", Done: 1, Total: 2})
		return map[string]int{"files": 2}, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	if k := r.Kinds(); len(k) != 1 || k[0] != "bridge.verify" {
		t.Fatalf("kinds = %v", k)
	}
	// Wait for the report to be recorded too: the server hears it a moment before
	// the store marks it reported, and stopping in between would cut that short.
	runFor(t, r, func() bool {
		s, _ := srv.finalState(1)
		sj, err := db.GetServerJob(context.Background(), 1)
		return s == link.StateDone && err == nil && sj.Reported
	})
	if got.ID != 1 || got.Attempt != 1 || *got.LibraryID != 2 || string(got.Params) != `{"x":1}` {
		t.Fatalf("the handler saw %+v", got)
	}
	_, final := srv.finalState(1)
	if string(final.Result) != `{"files":2}` || final.Attempt != 1 || !strings.Contains(string(final.Progress), `"done":1`) {
		t.Fatalf("final report = %+v", final)
	}
	var sawProgress bool
	for _, u := range srv.updates[1] {
		if strings.Contains(string(u.Progress), `"step":"stat"`) {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Fatalf("no progress was sent: %+v", srv.updates[1])
	}
	sj, err := db.GetServerJob(context.Background(), 1)
	if err != nil || sj.State != store.JobDone || !sj.Reported {
		t.Fatalf("stored = %+v, %v", sj, err)
	}
}

func TestEndings(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		fn        func(ctx context.Context, j Job, rep *Reporter) (any, error)
		cancel    bool
		want      string
		wantError string
	}{
		"failure": {fn: func(context.Context, Job, *Reporter) (any, error) {
			return map[string]bool{"ok": false}, errors.New("2 files differ")
		}, want: link.StateFailed, wantError: "2 files differ"},
		"park": {fn: func(context.Context, Job, *Reporter) (any, error) {
			return nil, &ParkError{Code: "not_writable", Message: "the book folder is read-only"}
		}, want: link.StateNeedsAttention, wantError: "not_writable: the book folder is read-only"},
		"cancel requested": {cancel: true, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, want: link.StateCancelled},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, srv := openStore(t), newFakeServer()
			srv.cancel[1] = c.cancel
			srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1})
			r := New(db, func() Server { return srv }, nil, testOpts(), handler{kind: "k.v", lane: LaneLight, fn: c.fn})
			runFor(t, r, func() bool { s, _ := srv.finalState(1); return s != "" })
			state, final := srv.finalState(1)
			if state != c.want || (c.wantError != "" && (final.Error == nil || *final.Error != c.wantError)) {
				t.Fatalf("final = %s %+v", state, final)
			}
		})
	}
}

// The server takes a job back (its lease ran out): the job stops, nothing more
// is sent, and the row is lost. Here the first report already finds it gone, so
// the handler never runs.
func TestLostClaim(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.notHeld[1] = true
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1})
	var ran atomic.Bool
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		ran.Store(true)
		return nil, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool {
		sj, err := db.GetServerJob(context.Background(), 1)
		return err == nil && sj.State == store.JobLost
	})
	if ran.Load() {
		t.Fatal("the handler ran a job the server no longer held")
	}
	if s, _ := srv.finalState(1); s != "" {
		t.Fatalf("a lost job reported %q", s)
	}
}

// A job taken back mid-run: the next keepalive finds it gone and stops it.
func TestLostMidRun(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 2, Kind: "k.v", Attempts: 1})
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		srv.mu.Lock()
		srv.notHeld[2] = true
		srv.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool {
		sj, err := db.GetServerJob(context.Background(), 2)
		return err == nil && sj.State == store.JobLost
	})
	if s, _ := srv.finalState(2); s != "" {
		t.Fatalf("a lost job reported %q", s)
	}
}

// A job running when the Bridge stops stays running and is resumed under the
// same claim at the next start, without claiming it again.
func TestStopAndResume(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 7, Kind: "k.v", LibraryID: lib(1), Attempts: 2})
	started := make(chan struct{}, 1)
	block := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), block)
	runFor(t, r, func() bool { return len(started) == 1 })
	sj, _ := db.GetServerJob(context.Background(), 7)
	if sj.State != store.JobRunning || sj.Attempt != 2 {
		t.Fatalf("after a stop = %+v", sj)
	}
	if s, _ := srv.finalState(7); s != "" {
		t.Fatalf("a stop reported %q", s)
	}

	var resumed Job
	done := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		resumed = j
		return nil, nil
	}}
	srv.mu.Lock()
	claimsBefore := len(srv.claims)
	srv.mu.Unlock()
	r2 := New(db, func() Server { return srv }, nil, testOpts(), done)
	runFor(t, r2, func() bool { s, _ := srv.finalState(7); return s == link.StateDone })
	if resumed.ID != 7 || resumed.Attempt != 2 || *resumed.LibraryID != 1 {
		t.Fatalf("resumed = %+v", resumed)
	}
	_, final := srv.finalState(7)
	if final.Attempt != 2 {
		t.Fatalf("the resumed job reported attempt %d", final.Attempt)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.queue) != 0 || len(srv.claims) < claimsBefore {
		t.Fatalf("queue = %+v", srv.queue)
	}
}

// A job of a kind this Bridge no longer runs fails at the next start.
func TestResumeOfAnUnknownKindFails(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	if err := db.RecordClaim(context.Background(), store.ServerJob{ID: 3, Kind: "gone.kind", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	r := New(db, func() Server { return srv }, nil, testOpts(), handler{kind: "k.v", lane: LaneLight})
	runFor(t, r, func() bool { s, _ := srv.finalState(3); return s == link.StateFailed })
}

// A report that couldn't reach the server is sent again later.
func TestReportRetried(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.failing = true
	srv.add(&link.Job{ID: 5, Kind: "k.v", Attempts: 1})
	r := New(db, func() Server { return srv }, nil, testOpts(), handler{kind: "k.v", lane: LaneLight,
		fn: func(context.Context, Job, *Reporter) (any, error) { return nil, nil }})
	go func() {
		// Let the job finish while the server is unreachable, then bring it back.
		for {
			sj, err := db.GetServerJob(context.Background(), 5)
			if err == nil && sj.State == store.JobDone {
				srv.mu.Lock()
				srv.failing = false
				srv.mu.Unlock()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	runFor(t, r, func() bool {
		sj, err := db.GetServerJob(context.Background(), 5)
		return err == nil && sj.Reported
	})
	if s, _ := srv.finalState(5); s != link.StateDone {
		t.Fatalf("retried report = %q", s)
	}
}

// A full lane takes no more of its kinds: the claim asks only for kinds with
// room, and none while every lane is full.
func TestLanes(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 1, Kind: "slow.one", Attempts: 1})
	release := make(chan struct{})
	slow := handler{kind: "slow.one", lane: "slow", fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	}}
	fast := handler{kind: "fast.one", lane: LaneLight, fn: func(context.Context, Job, *Reporter) (any, error) { return nil, nil }}
	opts := testOpts()
	opts.Lanes = map[string]int{"slow": 1, LaneLight: 1}
	r := New(db, func() Server { return srv }, nil, opts, slow, fast)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool {
		for _, l := range r.Lanes() {
			if l.Name == "slow" && l.Running == 1 {
				return true
			}
		}
		return false
	})
	srv.mu.Lock()
	srv.claims = nil
	srv.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	srv.mu.Lock()
	for _, k := range srv.claims {
		if slices.Contains(k, "slow.one") || !slices.Contains(k, "fast.one") {
			t.Errorf("a claim with the slow lane full asked for %v", k)
		}
	}
	srv.mu.Unlock()
	close(release)
	waitFor(t, func() bool { s, _ := srv.finalState(1); return s == link.StateDone })
}

// A 401 tells the app which link the server refused (it may be one a re-pair
// replaced, which says nothing about the current pairing).
func TestUnauthorized(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.unauthed = true
	var once sync.Once
	called := make(chan Server, 1)
	r := New(db, func() Server { return srv }, func(s Server) { once.Do(func() { called <- s }) }, testOpts(), handler{kind: "k.v", lane: LaneLight})
	runFor(t, r, func() bool { return len(called) == 1 })
	if got := <-called; got != Server(srv) {
		t.Fatalf("the refused link = %v, want the one that got the 401", got)
	}
}

// A job a refused token stopped is resumed under its claim once a link to its
// server works again (a refusal ending, a re-pair), not left "running" until
// the next start.
func TestRefusedJobResumes(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 4, Kind: "k.v", Attempts: 1})
	var paired atomic.Bool
	paired.Store(true)
	var runs atomic.Int32
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		if runs.Add(1) == 1 {
			<-ctx.Done() // stopped by the refusal
			return nil, ctx.Err()
		}
		return nil, nil
	}}
	r := New(db, func() Server {
		if paired.Load() {
			return srv
		}
		return nil
	}, func(Server) { paired.Store(false) }, testOpts(), h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return runs.Load() == 1 })
	srv.mu.Lock()
	srv.unauthed = true
	srv.mu.Unlock()
	waitFor(t, func() bool { return !paired.Load() })
	srv.mu.Lock()
	srv.unauthed = false
	srv.mu.Unlock()
	paired.Store(true)
	r.Wake()
	waitFor(t, func() bool { s, _ := srv.finalState(4); return s == link.StateDone })
	if _, final := srv.finalState(4); final.Attempt != 1 || runs.Load() != 2 {
		t.Fatalf("resumed under attempt %d after %d runs, want attempt 1 and 2 runs", final.Attempt, runs.Load())
	}
}

// A job a refused token stopped is never resumed through a link to another
// server (its job ids are the old server's): it is recorded as lost.
func TestRefusedJobOfAnotherServerIsLost(t *testing.T) {
	t.Parallel()
	db, a, b := openStore(t), newFakeServer(), newFakeServer()
	a.add(&link.Job{ID: 6, Kind: "k.v", Attempts: 1})
	var mu sync.Mutex
	current := Server(a)
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := New(db, func() Server { mu.Lock(); defer mu.Unlock(); return current }, func(Server) {
		mu.Lock()
		current = b // paired with another server
		mu.Unlock()
	}, testOpts(), h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(context.Background(), 6)
		return err == nil && sj.State == store.JobRunning
	})
	a.mu.Lock()
	a.unauthed = true
	a.mu.Unlock()
	waitFor(t, func() bool {
		sj, _ := db.GetServerJob(context.Background(), 6)
		return sj != nil && sj.State == store.JobLost
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.updates[6]) != 0 {
		t.Fatalf("the other server got the job's reports: %+v", b.updates[6])
	}
}

// release frees only its own run: a later claim of the same job, already
// running under its id, keeps its entry (and the page can cancel it).
func TestReleaseKeepsALaterRun(t *testing.T) {
	t.Parallel()
	r := New(openStore(t), func() Server { return nil }, nil, testOpts())
	_, c1 := context.WithCancelCause(context.Background())
	_, c2 := context.WithCancelCause(context.Background())
	old := &run{job: Job{ID: 5, Attempt: 1}, lane: LaneLight, cancel: c1, admitted: true}
	later := &run{job: Job{ID: 5, Attempt: 2}, lane: LaneLight, cancel: c2, admitted: true}
	r.mu.Lock()
	r.running[5], r.use[LaneLight] = later, 2
	r.mu.Unlock()
	r.release(old, false)
	if !r.Cancel(5) {
		t.Fatal("the later run lost its entry")
	}
	r.release(later, false)
	if r.Cancel(5) {
		t.Fatal("a released run is still listed")
	}
}

// A result the server would refuse for good (not a JSON object, or too large)
// isn't reported: the job fails saying why, instead of a report retried until
// its lease runs out.
func TestUnreportableResultFails(t *testing.T) {
	t.Parallel()
	for name, result := range map[string]any{
		"not an object": []int{1, 2},
		"too large":     map[string]string{"x": strings.Repeat("a", maxResultBytes)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, srv := openStore(t), newFakeServer()
			srv.add(&link.Job{ID: 2, Kind: "k.v", Attempts: 1})
			r := New(db, func() Server { return srv }, nil, testOpts(), handler{kind: "k.v", lane: LaneLight,
				fn: func(context.Context, Job, *Reporter) (any, error) { return result, nil }})
			runFor(t, r, func() bool { s, _ := srv.finalState(2); return s != "" })
			s, final := srv.finalState(2)
			if s != link.StateFailed || final.Result != nil || final.Error == nil || *final.Error != errUnreportable.Error() {
				t.Fatalf("final = %s %+v", s, final)
			}
		})
	}
}

// Unpaired (no server), the runner waits without claiming until woken.
func TestWaitsWhileUnpaired(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1})
	var mu sync.Mutex
	var current Server
	r := New(db, func() Server { mu.Lock(); defer mu.Unlock(); return current }, nil, testOpts(),
		handler{kind: "k.v", lane: LaneLight, fn: func(context.Context, Job, *Reporter) (any, error) { return nil, nil }})
	go func() {
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		current = srv
		mu.Unlock()
		r.Wake()
	}()
	runFor(t, r, func() bool { s, _ := srv.finalState(1); return s == link.StateDone })
}

// The Bridge's own page can cancel a running job.
func TestLocalCancel(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 9, Kind: "k.v", Attempts: 1})
	started := make(chan struct{})
	r := New(db, func() Server { return srv }, nil, testOpts(), handler{kind: "k.v", lane: LaneLight,
		fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}})
	go func() {
		<-started
		if !r.Cancel(9) {
			t.Error("Cancel found no running job")
		}
		if r.Cancel(99) {
			t.Error("Cancel stopped a job that isn't running")
		}
	}()
	runFor(t, r, func() bool { s, _ := srv.finalState(9); return s == link.StateCancelled })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A handler's Progress never waits on the network: with the server hanging,
// reporting steps returns at once.
func TestProgressNeverBlocks(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1})
	hang := make(chan struct{})
	took := make(chan time.Duration, 1)
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		srv.mu.Lock()
		srv.hang = hang
		srv.mu.Unlock()
		start := time.Now()
		for i := range 20 {
			rep.Progress(Progress{Step: fmt.Sprintf("step-%d", i)})
		}
		took <- time.Since(start)
		close(hang)
		return nil, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool { s, _ := srv.finalState(1); return s == link.StateDone })
	if d := <-took; d > 500*time.Millisecond {
		t.Fatalf("20 progress steps took %v with the server hanging", d)
	}
}
