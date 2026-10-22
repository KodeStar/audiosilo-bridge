package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Extra rides in the same object as step/message/done/total, which win over an
// Extra key of the same name; an object over the server's 16 KiB drops Extra.
func TestProgressExtra(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Progress{Step: "asr", Message: "Transcribing", Done: 3, Total: 9,
		Extra: map[string]any{"stage": "asr", "eta_seconds": 60, "step": "nope", "park": map[string]string{"code": "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["step"] != "asr" || got["stage"] != "asr" || got["eta_seconds"] != float64(60) || got["done"] != float64(3) ||
		got["park"].(map[string]any)["code"] != "x" {
		t.Fatalf("merged = %s", b)
	}
	plain, _ := json.Marshal(Progress{Step: "stat"})
	if string(plain) != `{"step":"stat"}` {
		t.Fatalf("no extra = %s", plain)
	}
	big, _ := json.Marshal(Progress{Step: "asr", Extra: map[string]any{"blob": strings.Repeat("a", maxProgressBytes)}})
	if string(big) != `{"step":"asr"}` {
		t.Fatalf("an oversized extra was sent: %d bytes", len(big))
	}
}

// A parked job reports needs_attention with the park message as its error and
// goes on reporting (the server drops its lease, but a cancel still reaches
// it); Unpark sends running with the error cleared.
func TestParkAndUnpark(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1})
	h := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.Progress(Progress{Step: "fact_pass", Extra: map[string]any{"park": map[string]string{"code": "agent_unavailable"}}})
		rep.Park("agent_unavailable", "the agent container isn't answering")
		waitFor(t, func() bool { return countState(srv, 1, link.StateNeedsAttention) >= 2 })
		if parked, msg := rep.parkState(); !parked || msg != "the agent container isn't answering" {
			t.Errorf("parkState() = %v %q", parked, msg)
		}
		sj, _ := db.GetServerJob(ctx, 1)
		if sj.State != store.JobRunning || sj.Error != "the agent container isn't answering" {
			t.Errorf("stored while parked = %+v", sj)
		}
		rep.Unpark()
		rep.Progress(Progress{Step: "synthesizing"})
		waitFor(t, func() bool {
			return lastUpdate(srv, 1).Progress != nil && strings.Contains(string(lastUpdate(srv, 1).Progress), "synthesizing")
		})
		return map[string]int{"ok": 1}, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool { s, _ := srv.finalState(1); return s == link.StateDone })

	srv.mu.Lock()
	defer srv.mu.Unlock()
	var sawPark, sawClear bool
	for _, u := range srv.updates[1] {
		if u.State != nil && *u.State == link.StateNeedsAttention {
			sawPark = sawPark || (u.Error != nil && *u.Error == "the agent container isn't answering")
		}
		if sawPark && u.State != nil && *u.State == link.StateRunning && u.Error != nil && *u.Error == "" {
			sawClear = true
		}
	}
	if !sawPark || !sawClear {
		t.Fatalf("park sent %v, cleared %v: %+v", sawPark, sawClear, srv.updates[1])
	}
}

// A parked job the server takes back (an admin cancelled it: 409 not_held)
// stops, and its context says it doesn't resume.
func TestParkedJobCancelledOnTheServer(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 2, Kind: "k.v", Attempts: 1})
	var resumes atomic.Bool
	resumes.Store(true)
	h := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.Park("core_needed", "needs a core record")
		waitFor(t, func() bool { return countState(srv, 2, link.StateNeedsAttention) >= 1 })
		srv.mu.Lock()
		srv.notHeld[2] = true
		srv.mu.Unlock()
		<-ctx.Done()
		resumes.Store(Resumes(ctx))
		return nil, ctx.Err()
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool {
		sj, err := db.GetServerJob(context.Background(), 2)
		return err == nil && sj.State == store.JobLost
	})
	if resumes.Load() {
		t.Fatal("a job the server took back says it resumes")
	}
}

// A job parked when the Bridge stopped resumes parked: its first report after
// the restart is needs_attention, never a running in between. A stop says the
// job resumes.
func TestParkSurvivesARestart(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 3, Kind: "k.v", Attempts: 1})
	var resumes atomic.Bool
	first := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.Park("agent_rate_limited", "rate limited until 3pm")
		waitFor(t, func() bool { return countState(srv, 3, link.StateNeedsAttention) >= 1 })
		<-ctx.Done()
		resumes.Store(Resumes(ctx))
		return nil, ctx.Err()
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), first)
	runFor(t, r, func() bool { return countState(srv, 3, link.StateNeedsAttention) >= 1 })
	// runFor returns once the runner stopped, so the handler has returned.
	if !resumes.Load() {
		t.Fatal("a job the Bridge's stop ended says it doesn't resume")
	}

	srv.mu.Lock()
	before := len(srv.updates[3])
	srv.mu.Unlock()
	var sawParked atomic.Bool
	second := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		parked, msg := rep.parkState()
		sawParked.Store(parked && msg == "rate limited until 3pm")
		rep.Unpark()
		return nil, nil
	}}
	r2 := New(db, func() Server { return srv }, nil, testOpts(), second)
	runFor(t, r2, func() bool { s, _ := srv.finalState(3); return s == link.StateDone })
	if !sawParked.Load() {
		t.Fatal("the resumed job's reporter wasn't parked")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	after := srv.updates[3][before]
	if after.State == nil || *after.State != link.StateNeedsAttention || after.Error == nil || *after.Error != "rate limited until 3pm" {
		t.Fatalf("the first report after the restart = %+v", after)
	}
}

// A claim records the server's id; a start drops another server's open jobs
// (lost, nothing reported) instead of resuming or reporting them.
func TestServerIdentity(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	ctx := context.Background()
	srv.add(&link.Job{ID: 4, Kind: "k.v", Attempts: 1})
	opts := testOpts()
	opts.ServerID = func() string { return "srv-a" }
	r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.v", lane: LaneLight,
		fn: func(context.Context, Job, *Reporter) (any, error) { return nil, nil }})
	runFor(t, r, func() bool { s, _ := srv.finalState(4); return s == link.StateDone })
	if sj, _ := db.GetServerJob(ctx, 4); sj.ServerID != "srv-a" {
		t.Fatalf("recorded server = %q", sj.ServerID)
	}

	// Another server's jobs: one running, one finished and never reported.
	_ = db.RecordClaim(ctx, store.ServerJob{ID: 5, Kind: "k.v", Attempt: 1, ServerID: "srv-old"})
	_ = db.RecordClaim(ctx, store.ServerJob{ID: 6, Kind: "k.v", Attempt: 1, ServerID: "srv-old"})
	_ = db.FinishServerJob(ctx, 6, 1, store.JobDone, nil, "")
	var ran atomic.Bool
	r2 := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.v", lane: LaneLight,
		fn: func(context.Context, Job, *Reporter) (any, error) { ran.Store(true); return nil, nil }})
	runFor(t, r2, func() bool {
		a, _ := db.GetServerJob(ctx, 5)
		b, _ := db.GetServerJob(ctx, 6)
		return a.State == store.JobLost && b.Reported
	})
	if ran.Load() {
		t.Fatal("another server's job was resumed")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.updates[5]) != 0 || len(srv.updates[6]) != 0 {
		t.Fatalf("another server's jobs were reported here: %+v %+v", srv.updates[5], srv.updates[6])
	}
}

func countState(f *fakeServer, id int64, state string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, u := range f.updates[id] {
		if u.State != nil && *u.State == state {
			n++
		}
	}
	return n
}

func lastUpdate(f *fakeServer, id int64) link.Update {
	f.mu.Lock()
	defer f.mu.Unlock()
	ups := f.updates[id]
	if len(ups) == 0 {
		return link.Update{}
	}
	return ups[len(ups)-1]
}

// A handler that reports the same progress on every poll sends nothing new: only
// a change (Extra included) is a report, and the keepalive renews the lease.
func TestUnchangedProgressIsNotSent(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 8, Kind: "k.v", Attempts: 1})
	opts := testOpts()
	opts.Keepalive = time.Hour
	same := func() Progress {
		return Progress{Step: "asr", Message: "Transcribing", Extra: map[string]any{"park": map[string]any{"code": "x"}}}
	}
	r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.v", lane: LaneLight,
		fn: func(_ context.Context, _ Job, rep *Reporter) (any, error) {
			for range 30 {
				rep.Progress(same())
				time.Sleep(2 * time.Millisecond)
			}
			running := countState(srv, 8, link.StateRunning)
			// Changed Extra only: a report again.
			changed := same()
			changed.Extra["eta_seconds"] = 60
			rep.Progress(changed)
			time.Sleep(20 * time.Millisecond)
			if running != 2 || countState(srv, 8, link.StateRunning) != 3 {
				return nil, fmt.Errorf("running reports = %d then %d, want 2 (the start and the first progress) then 3",
					running, countState(srv, 8, link.StateRunning))
			}
			return nil, nil
		}})
	runFor(t, r, func() bool { s, _ := srv.finalState(8); return s != "" })
	if s, u := srv.finalState(8); s != link.StateDone {
		msg := ""
		if u.Error != nil {
			msg = *u.Error
		}
		t.Fatalf("final = %q: %s", s, msg)
	}
}

// abandoner is a handler that hears of the jobs the runner ends without it.
type abandoner struct {
	handler
	mu  sync.Mutex
	got []int64
	ran atomic.Bool
}

func (a *abandoner) Run(ctx context.Context, j Job, rep *Reporter) (any, error) {
	a.ran.Store(true)
	return a.handler.Run(ctx, j, rep)
}

func (a *abandoner) Abandon(_ context.Context, j Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, j.ID)
}

func (a *abandoner) abandoned() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.got)
}

// A job the runner ends without running its handler (its first report after a
// restart answered with a cancel or a 409, or another server's job dropped at a
// start) is handed to the handler's Abandon, so what an earlier run left is
// released. One that runs, or one that resumes later, is not.
func TestAbandonedJobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		serverID string
		setup    func(srv *fakeServer)
		abandon  bool
	}{
		"cancelled while down": {serverID: "srv", setup: func(srv *fakeServer) { srv.cancel[9] = true }, abandon: true},
		"lost while down":      {serverID: "srv", setup: func(srv *fakeServer) { srv.notHeld[9] = true }, abandon: true},
		"another server's":     {serverID: "srv-new", setup: func(*fakeServer) {}, abandon: true},
		"resumed and run":      {serverID: "srv", setup: func(*fakeServer) {}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, srv := openStore(t), newFakeServer()
			if err := db.RecordClaim(ctx, store.ServerJob{ID: 9, Kind: "k.v", Attempt: 1, ServerID: "srv"}); err != nil {
				t.Fatal(err)
			}
			tc.setup(srv)
			h := &abandoner{handler: handler{kind: "k.v", lane: LaneLight,
				fn: func(context.Context, Job, *Reporter) (any, error) { return nil, nil }}}
			opts := testOpts()
			opts.ServerID = func() string { return tc.serverID }
			r := New(db, func() Server { return srv }, nil, opts, h)
			runFor(t, r, func() bool {
				sj, err := db.GetServerJob(ctx, 9)
				return err == nil && sj.State != store.JobRunning
			})
			if got := h.abandoned(); tc.abandon != (len(got) == 1 && got[0] == 9) || (tc.abandon && h.ran.Load()) {
				t.Fatalf("abandoned %v (ran %v), want abandoned %v", got, h.ran.Load(), tc.abandon)
			}
		})
	}
}

// Every report of a job that isn't parked clears the error: a clear a restart
// interrupted (the store already cleared, the server not yet) still reaches the
// server.
func TestReportsClearTheErrorWhenNotParked(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	ctx := context.Background()
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 11, Kind: "k.v", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	h := handler{kind: "k.v", lane: LaneLight, fn: func(_ context.Context, _ Job, rep *Reporter) (any, error) {
		rep.Progress(Progress{Step: "asr"})
		waitFor(t, func() bool { return countState(srv, 11, link.StateRunning) >= 2 })
		return nil, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool { s, _ := srv.finalState(11); return s == link.StateDone })
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, u := range srv.updates[11] {
		if u.State != nil && *u.State == link.StateRunning && (u.Error == nil || *u.Error != "") {
			t.Fatalf("a running report without the cleared error: %+v", u)
		}
	}
}

// The job's path follows the server's answers: the server moves an open job with
// its book (a rename the scan noticed), and the handler reads where it is now.
func TestReporterPathFollowsTheServer(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	old := "Author/Old"
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1, Path: &old})
	var before, after string
	h := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		before = rep.Path() // the first report's answer carried none
		srv.mu.Lock()
		srv.paths = map[int64]string{1: "Author/New"}
		srv.mu.Unlock()
		waitFor(t, func() bool { return rep.Path() == "Author/New" })
		after = rep.Path()
		return map[string]int{"ok": 1}, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool { s, _ := srv.finalState(1); return s == link.StateDone })
	if before != "" || after != "Author/New" {
		t.Fatalf("Path() = %q then %q", before, after)
	}
	if LocalReporter().Path() != "" {
		t.Fatal("a local reporter has a path")
	}
}

func TestPublicationRequiresSuccessfulLease(t *testing.T) {
	for _, mode := range []string{"allowed", "network", "lost", "cancel", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			srv := newFakeServer()
			switch mode {
			case "network":
				srv.failing = true
			case "lost":
				srv.notHeld[81] = true
			case "cancel":
				srv.cancel[81] = true
			case "unauthorized":
				srv.unauthed = true
			}
			jobCtx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			unpaired := false
			rep := &Reporter{r: &Runner{onUnauthorized: func(Server) { unpaired = true }}, srv: srv, job: Job{ID: 81, Attempt: 2}, cancel: cancel}
			err := rep.ConfirmLease(context.Background())
			if (err == nil) != (mode == "allowed") {
				t.Fatal(mode, err)
			}
			// The publication check acts on the answer as the keepalive does.
			if unpaired != (mode == "unauthorized") {
				t.Fatal(mode, "unpaired", unpaired)
			}
			want := map[string]error{"lost": errLost, "cancel": errCancelRequested, "unauthorized": errUnpaired}[mode]
			if got := context.Cause(jobCtx); want != nil && !errors.Is(got, want) || want == nil && got != nil {
				t.Fatal(mode, "cause", got)
			}
		})
	}
	if got := DefaultLanes()[LaneDownload] + DefaultLanes()[LaneInteractive]; got != 2 {
		t.Fatal("episode concurrency", got)
	}
}
