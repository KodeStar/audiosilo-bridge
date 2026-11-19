package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
		Extra: map[string]any{"stage": "asr", "eta_seconds": 60, "step": "nope", "park": map[string]any{"code": "x"}}})
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
		rep.Progress(Progress{Step: "fact_pass", Extra: map[string]any{"park": map[string]any{"code": "agent_unavailable"}}})
		rep.Park("agent_unavailable", "the agent container isn't answering")
		waitFor(t, func() bool { return countState(srv, 1, link.StateNeedsAttention) >= 2 })
		if parked, msg := rep.parkState(); !parked || msg != "the agent container isn't answering" {
			t.Errorf("parkState() = %v %q", parked, msg)
		}
		sj, _ := db.GetServerJob(ctx, 1)
		if sj.State != store.JobRunning || sj.Error != "the agent container isn't answering" {
			t.Errorf("stored while parked = %+v", sj)
		}
		// The park carries its message for people (bridge#28), stored and sent.
		if !strings.Contains(string(sj.Progress), `"message":"the agent container isn't answering"`) ||
			!strings.Contains(string(lastUpdate(srv, 1).Progress), `"message":"the agent container isn't answering"`) {
			t.Errorf("park without its message: stored %s, sent %s", sj.Progress, lastUpdate(srv, 1).Progress)
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

// withParkMessage puts the message beside the park it names, without the
// "<code>: " an engine message may lead with, and adds none for a message that
// is only the code or a progress that names no park (bridge#28).
func TestWithParkMessage(t *testing.T) {
	t.Parallel()
	park := map[string]any{"code": "asr_worker_waiting", "hint": "Add a worker."}
	pr := Progress{Step: "asr", Extra: map[string]any{"stage": "asr", "park": park}}
	for msg, want := range map[string]string{
		"asr_worker_waiting: speech recognition waits for a worker": "speech recognition waits for a worker",
		"speech recognition waits for a worker":                     "speech recognition waits for a worker",
		"asr_worker_waiting":                                        "",
		"asr_worker_waiting:":                                       "",
	} {
		got := withParkMessage(pr, msg)
		gotMsg, _ := got.Extra["park"].(map[string]any)["message"].(string)
		if gotMsg != want {
			t.Errorf("%q: message %q, want %q", msg, gotMsg, want)
		}
	}
	if _, ok := park["message"]; ok || len(pr.Extra) != 2 {
		t.Fatalf("the caller's maps changed: %v", pr.Extra)
	}
	// A message an earlier report left (a park carried over, or restored after
	// a restart) goes when the park's message is now only its code.
	stale := Progress{Step: "asr", Extra: map[string]any{"park": map[string]any{"code": "asr_worker_waiting", "message": "older"}}}
	if got := withParkMessage(stale, "asr_worker_waiting"); got.Extra["park"].(map[string]any)["message"] != nil {
		t.Errorf("a stale message stayed: %v", got.Extra["park"])
	}
	if stale.Extra["park"].(map[string]any)["message"] != "older" {
		t.Fatalf("the caller's park changed: %v", stale.Extra["park"])
	}
	plain := Progress{Step: "asr", Extra: map[string]any{"stage": "asr"}}
	if got := withParkMessage(plain, "a message"); len(got.Extra) != 1 {
		t.Fatalf("a progress with no park gained one: %v", got.Extra)
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

// A parked job an admin cancels on a server that says so (409 not_held, reason
// cancelled, bridge#35) ends cancelled, not lost: its context says it neither
// resumes nor was lost (so a handler keeps nothing for a later claim).
func TestParkedJobCancelledByAnAdmin(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 2, Kind: "k.v", Attempts: 1})
	var resumes, lost atomic.Bool
	resumes.Store(true)
	lost.Store(true)
	h := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.Park("asr_worker_waiting", "waiting for an ASR worker")
		waitFor(t, func() bool { return countState(srv, 2, link.StateNeedsAttention) >= 1 })
		srv.mu.Lock()
		srv.gone[2] = true
		srv.mu.Unlock()
		<-ctx.Done()
		resumes.Store(Resumes(ctx))
		lost.Store(Lost(ctx))
		return nil, ctx.Err()
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool {
		sj, err := db.GetServerJob(context.Background(), 2)
		return err == nil && sj.State == store.JobCancelled && sj.Reported
	})
	if resumes.Load() || lost.Load() {
		t.Fatalf("a job an admin cancelled says resumes %v, lost %v", resumes.Load(), lost.Load())
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

// A job parked when the Bridge stopped keeps its park's code through the
// restart: the server hears a park whose code changes as a new park (a bell
// event), so no report while it stays parked drops progress.park, even the
// resumed handler's first steps that name none; Unpark drops the kept one.
func TestParkCodeSurvivesARestart(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 7, Kind: "k.v", Attempts: 1})
	park := map[string]any{"code": "agent_rate_limited"}
	first := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.Progress(Progress{Step: "waiting", Message: "Needs attention", Extra: map[string]any{"park": park, "source": "audio"}})
		rep.Park("agent_rate_limited", "rate limited until 3pm")
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), first)
	runFor(t, r, func() bool {
		sj, err := db.GetServerJob(context.Background(), 7)
		return countState(srv, 7, link.StateNeedsAttention) >= 1 && err == nil && strings.Contains(string(sj.Progress), "agent_rate_limited")
	})

	srv.mu.Lock()
	before := len(srv.updates[7])
	srv.mu.Unlock()
	second := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.Progress(Progress{Step: "starting", Message: "Starting"})
		waitFor(t, func() bool {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			for _, u := range srv.updates[7][before:] {
				if strings.Contains(string(u.Progress), "Starting") {
					return true
				}
			}
			return false
		})
		rep.Unpark()
		return nil, nil
	}}
	r2 := New(db, func() Server { return srv }, nil, testOpts(), second)
	runFor(t, r2, func() bool { s, _ := srv.finalState(7); return s == link.StateDone })
	srv.mu.Lock()
	defer srv.mu.Unlock()
	sawStarting, sawRunning := false, false
	for _, u := range srv.updates[7][before:] {
		if u.State == nil {
			continue
		}
		var p map[string]any
		_ = json.Unmarshal(u.Progress, &p)
		_, hasPark := p["park"]
		switch *u.State {
		case link.StateNeedsAttention:
			if !hasPark {
				t.Fatalf("a parked report after the restart dropped its park: %s", u.Progress)
			}
			if p["message"] == "Starting" {
				sawStarting = true
			}
		case link.StateRunning:
			sawRunning = true
			if hasPark {
				t.Fatalf("a running report kept the park: %s", u.Progress)
			}
		}
	}
	if !sawStarting || !sawRunning {
		t.Fatalf("starting %v running %v: %+v", sawStarting, sawRunning, srv.updates[7][before:])
	}
}

// A claim records the server's id; a start drops another server's open jobs
// (lost, nothing reported) instead of resuming or reporting them.
func TestServerIdentity(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	ctx := context.Background()
	srv.add(&link.Job{ID: 4, Kind: "k.v", Attempts: 1})
	srv.id = "srv-a"
	opts := testOpts()
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
			running := func(n int) func() bool { return func() bool { return countState(srv, 8, link.StateRunning) == n } }
			// A new step is sent at once: waited for, as a send under load can
			// take longer than the polls below.
			rep.Progress(same())
			if err := awaitCond(running(2), "the start and the first progress"); err != nil {
				return nil, err
			}
			// The same progress again: every 1 ms tick could send it, none does.
			for range 30 {
				rep.Progress(same())
				time.Sleep(2 * time.Millisecond)
			}
			if n := countState(srv, 8, link.StateRunning); n != 2 {
				return nil, fmt.Errorf("running reports = %d after the same progress again, want 2 (the start and the first progress)", n)
			}
			// Changed Extra only: a report again.
			changed := same()
			changed.Extra["eta_seconds"] = 60
			rep.Progress(changed)
			if err := awaitCond(running(3), "the changed progress"); err != nil {
				return nil, err
			}
			return nil, nil
		}})
	wantDone(t, r, srv, 8)
	// Exactly three: a repeat sent late (after the loop's check) would make a
	// fourth, which "reached three" above can't see.
	if n := countState(srv, 8, link.StateRunning); n != 3 {
		t.Fatalf("running reports = %d, want 3 (the start, the first progress, the changed one)", n)
	}
}

// abandoner is a handler that hears of the jobs the runner ends without it.
type abandoner struct {
	handler
	mu   sync.Mutex
	got  []int64
	lost []bool // Lost(ctx) at each Abandon
	ran  atomic.Bool
}

func (a *abandoner) Run(ctx context.Context, j Job, rep *Reporter) (any, error) {
	a.ran.Store(true)
	return a.handler.Run(ctx, j, rep)
}

func (a *abandoner) Abandon(ctx context.Context, j Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, j.ID)
	a.lost = append(a.lost, Lost(ctx))
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
		lost     bool // Abandon hears a lost claim (the server may hand the job out again)
	}{
		"cancelled while down": {serverID: "srv", setup: func(srv *fakeServer) { srv.cancel[9] = true }, abandon: true},
		"lost while down":      {serverID: "srv", setup: func(srv *fakeServer) { srv.notHeld[9] = true }, abandon: true, lost: true},
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
			srv.id = tc.serverID
			r := New(db, func() Server { return srv }, nil, testOpts(), h)
			runFor(t, r, func() bool {
				sj, err := db.GetServerJob(ctx, 9)
				return err == nil && sj.State != store.JobRunning
			})
			if got := h.abandoned(); tc.abandon != (len(got) == 1 && got[0] == 9) || (tc.abandon && h.ran.Load()) {
				t.Fatalf("abandoned %v (ran %v), want abandoned %v", got, h.ran.Load(), tc.abandon)
			}
			h.mu.Lock()
			lost := slices.Clone(h.lost)
			h.mu.Unlock()
			if tc.abandon && !slices.Equal(lost, []bool{tc.lost}) {
				t.Fatalf("Lost at Abandon = %v, want %v", lost, tc.lost)
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
	if err := db.RecordClaim(ctx, store.ServerJob{ServerID: testServer, ID: 11, Kind: "k.v", Attempt: 1}); err != nil {
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

// ShareOff (server#218): a job whose claim said share_off has it from the
// start; one told by an answer to a report has it from then on, and keeps it
// when a later answer says none (a share dropped is not taken up again). A
// job no answer told never has it.
func TestReporterShareOff(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1, ShareOff: true})
	srv.add(&link.Job{ID: 2, Kind: "k.v", Attempts: 1})
	srv.add(&link.Job{ID: 3, Kind: "k.v", Attempts: 1})
	got := map[int64][]bool{}
	var mu sync.Mutex
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		seen := []bool{j.shareOff, rep.ShareOff()}
		switch j.ID {
		case 2:
			srv.setShareOff(map[int64]bool{2: true})
			waitFor(t, rep.ShareOff)
			srv.setShareOff(nil)
			n := srv.triesOf(2)
			waitFor(t, func() bool { return srv.triesOf(2) > n+1 })
			seen = append(seen, rep.ShareOff())
		case 3:
			n := srv.triesOf(3)
			waitFor(t, func() bool { return srv.triesOf(3) > n+1 })
			seen = append(seen, rep.ShareOff())
		}
		mu.Lock()
		got[j.ID] = seen
		mu.Unlock()
		return map[string]int{"ok": 1}, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool {
		var n int
		for id := int64(1); id <= 3; id++ {
			if s, _ := srv.finalState(id); s == link.StateDone {
				n++
			}
		}
		return n == 3
	})
	mu.Lock()
	defer mu.Unlock()
	if want := []bool{true, true}; !slices.Equal(got[1], want) {
		t.Errorf("claimed with share_off: %v, want %v", got[1], want)
	}
	if want := []bool{false, false, true}; !slices.Equal(got[2], want) {
		t.Errorf("told by an answer: %v, want %v", got[2], want)
	}
	if want := []bool{false, false, false}; !slices.Equal(got[3], want) {
		t.Errorf("never told: %v, want %v", got[3], want)
	}
	if LocalReporter().ShareOff() {
		t.Fatal("a local reporter has share_off")
	}
}

// OnShareOff (server#218) is called the moment the job hears share_off: at
// once for a job whose claim said it, at the answer for one told by an answer
// (not at the handler's next look), and never for a job no answer told.
func TestReporterOnShareOff(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1, ShareOff: true})
	srv.add(&link.Job{ID: 2, Kind: "k.v", Attempts: 1})
	srv.add(&link.Job{ID: 3, Kind: "k.v", Attempts: 1})
	var calls [4]atomic.Int32
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.OnShareOff(func() { calls[j.ID].Add(1) })
		switch j.ID {
		case 1:
			if calls[1].Load() != 1 {
				t.Errorf("claimed with share_off: %d calls at once, want 1", calls[1].Load())
			}
		case 2:
			srv.setShareOff(map[int64]bool{2: true})
			waitFor(t, func() bool { return calls[2].Load() > 0 })
		case 3:
			n := srv.triesOf(3)
			waitFor(t, func() bool { return srv.triesOf(3) > n+1 })
		}
		rep.OnShareOff(nil)
		return nil, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool {
		var n int
		for id := int64(1); id <= 3; id++ {
			if s, _ := srv.finalState(id); s == link.StateDone {
				n++
			}
		}
		return n == 3
	})
	if calls[2].Load() == 0 || calls[3].Load() != 0 {
		t.Fatalf("told by an answer: %d calls; never told: %d", calls[2].Load(), calls[3].Load())
	}
}

// A job's share_off is kept in the store, from its claim or an answer since
// (server#218), so a job resumed after a restart has it before any answer: one
// whose server is unreachable at its first report still keeps its results
// local. Denied: a job no answer told resumes without it.
func TestShareOffSurvivesARestart(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1, ShareOff: true})
	srv.add(&link.Job{ID: 2, Kind: "k.v", Attempts: 1})
	srv.add(&link.Job{ID: 3, Kind: "k.v", Attempts: 1})
	var running atomic.Int32
	first := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		if j.ID == 2 {
			srv.setShareOff(map[int64]bool{2: true})
			waitFor(t, func() bool {
				sj, err := db.GetServerJob(context.Background(), 2)
				return err == nil && sj.ShareOff
			})
		}
		running.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), first)
	runFor(t, r, func() bool { return running.Load() == 3 })

	srv.mu.Lock()
	srv.failing, srv.shareOff = true, nil
	srv.mu.Unlock()
	var mu sync.Mutex
	got := map[int64][2]bool{}
	second := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		mu.Lock()
		got[j.ID] = [2]bool{j.Resumed, rep.ShareOff()}
		mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r2 := New(db, func() Server { return srv }, nil, testOpts(), second)
	runFor(t, r2, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 3
	})
	mu.Lock()
	defer mu.Unlock()
	want := map[int64][2]bool{1: {true, true}, 2: {true, true}, 3: {true, false}}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("job %d resumed: (resumed, share_off) = %v, want %v", id, got[id], w)
		}
	}
}

// shareOffFailStore fails SetServerJobShareOff while fail is set.
type shareOffFailStore struct {
	*store.DB
	fail  atomic.Bool
	tries atomic.Int32
}

func (s *shareOffFailStore) SetServerJobShareOff(ctx context.Context, id int64, attempt int) error {
	s.tries.Add(1)
	if s.fail.Load() {
		return errors.New("disk I/O error")
	}
	return s.DB.SetServerJobShareOff(ctx, id, attempt)
}

// A share_off the run heard but couldn't store survives a refused token: the
// job resumeStranded resumes has it before any answer (the server's answers
// no longer carry it), and the write is tried again until the store has it.
func TestShareOffHeardSurvivesARefusedToken(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	st := &shareOffFailStore{DB: db}
	st.fail.Store(true)
	srv.add(&link.Job{ID: 5, Kind: "k.v", Attempts: 1})
	var paired atomic.Bool
	paired.Store(true)
	var runs atomic.Int32
	var resumedShareOff atomic.Bool
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		if runs.Add(1) == 1 {
			srv.setShareOff(map[int64]bool{5: true})
			waitFor(t, func() bool { return rep.ShareOff() && st.tries.Load() > 0 })
			srv.mu.Lock()
			srv.unauthed = true
			srv.mu.Unlock()
			<-ctx.Done() // stopped by the refusal
			return nil, ctx.Err()
		}
		resumedShareOff.Store(rep.ShareOff())
		st.fail.Store(false)
		waitFor(t, func() bool {
			sj, err := db.GetServerJob(context.Background(), 5)
			return err == nil && sj.ShareOff
		})
		return nil, nil
	}}
	r := New(st, func() Server {
		if paired.Load() {
			return srv
		}
		return nil
	}, func(Server) { paired.Store(false) }, testOpts(), h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return !paired.Load() })
	// Stranded, it answers the contributing stage what it heard, with no
	// request (the link refused).
	waitFor(t, func() bool { off, err := r.ShareOff(context.Background(), 5, false); return off && err == nil })
	srv.mu.Lock()
	srv.unauthed, srv.shareOff = false, nil
	srv.mu.Unlock()
	paired.Store(true)
	r.Wake()
	waitFor(t, func() bool { s, _ := srv.finalState(5); return s == link.StateDone })
	if runs.Load() != 2 || !resumedShareOff.Load() {
		t.Fatalf("runs %d, resumed with share_off %v: want 2 runs, the second with it", runs.Load(), resumedShareOff.Load())
	}
}

// A share_off whose write failed is written again at every report, answered
// or not: a Bridge stopped while the server is out of reach must not lose it
// (the next start resumes the job from the store alone).
func TestShareOffWriteTriedWhileTheServerIsOut(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	st := &shareOffFailStore{DB: db}
	st.fail.Store(true)
	srv.add(&link.Job{ID: 8, Kind: "k.v", Attempts: 1})
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		srv.setShareOff(map[int64]bool{8: true})
		waitFor(t, func() bool { return rep.ShareOff() && st.tries.Load() > 0 })
		srv.setFailing(true)
		st.fail.Store(false)
		waitFor(t, func() bool {
			sj, err := db.GetServerJob(context.Background(), 8)
			return err == nil && sj.ShareOff
		})
		srv.setFailing(false)
		return nil, nil
	}}
	r := New(st, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool { s, _ := srv.finalState(8); return s == link.StateDone })
}

// Runner.ShareOff (the contributing stage's ask, server#218) reaches a job
// waiting for its lane slot through its Reporter, so a resumed job that never
// gets a slot doesn't keep the book parked. Without asking it answers only
// what is known here, and an error with no request for a job it can't ask: one
// not running here, one whose reports fail, or with the Bridge unpaired.
func TestRunnerShareOff(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	ctx := context.Background()
	for _, id := range []int64{1, 2} {
		if err := db.RecordClaim(ctx, store.ServerJob{ServerID: testServer, ID: id, Kind: "k.v", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	var paired atomic.Bool
	paired.Store(true)
	started := make(chan int64, 2)
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		started <- j.ID
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	opts := testOpts()
	opts.Lanes = map[string]int{LaneLight: 1}
	r := New(db, func() Server {
		if paired.Load() {
			return srv
		}
		return nil
	}, nil, opts, h)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(rctx); close(done) }()
	defer func() { cancel(); <-done }()
	running := <-started // the job given the slot; the other waits for it
	waiting := 3 - running

	if _, err := r.ShareOff(ctx, 9, true); !errors.Is(err, ErrShareNotRunning) {
		t.Fatalf("a job not running here: %v, want ErrShareNotRunning", err)
	}
	waitFor(t, func() bool { _, err := r.ShareOff(ctx, waiting, false); return err == nil })
	if off, err := r.ShareOff(ctx, waiting, false); off || err != nil {
		t.Fatalf("the waiting job, not asked: %v %v, want false", off, err)
	}
	srv.setShareOff(map[int64]bool{waiting: true})
	if off, err := r.ShareOff(ctx, waiting, true); !off || err != nil {
		t.Fatalf("the waiting job, asked: %v %v, want true", off, err)
	}
	if sj, err := db.GetServerJob(ctx, waiting); err != nil || !sj.ShareOff {
		t.Fatalf("the answer's share_off isn't stored: %+v %v", sj, err)
	}
	select {
	case id := <-started:
		t.Fatalf("job %d ran without a slot", id)
	default:
	}

	srv.setFailing(true)
	waitFor(t, func() bool { _, err := r.ShareOff(ctx, running, false); return errors.Is(err, ErrShareNotReachable) })
	if _, err := r.ShareOff(ctx, running, true); err == nil {
		t.Fatal("asked with the server out of reach: no error")
	}
	srv.setFailing(false)

	paired.Store(false)
	for _, ask := range []bool{false, true} {
		if _, err := r.ShareOff(ctx, running, ask); !errors.Is(err, ErrShareOffLink) {
			t.Fatalf("unpaired, ask %v: %v, want ErrShareOffLink", ask, err)
		}
	}
	if off, err := r.ShareOff(ctx, waiting, true); !off || err != nil {
		t.Fatalf("unpaired, a job that heard share_off: %v %v, want true", off, err)
	}
}

// What a stranded attempt heard answers for the job only while no later claim
// of it runs here: that claim's share is its own (denied: the stranded
// attempt's share_off is not the new claim's), asked through its own run.
func TestRunnerShareOffStrandedOnlyWithoutALaterClaim(t *testing.T) {
	t.Parallel()
	srv := newFakeServer()
	r := New(openStore(t), func() Server { return srv }, nil, testOpts())
	r.mu.Lock()
	r.stranded = append(r.stranded, Job{ID: 4, Attempt: 1, shareOff: true})
	r.running[4] = &run{job: Job{serverID: testServer, ID: 4, Attempt: 2}} // launched, its Reporter not made yet
	r.mu.Unlock()
	if off, err := r.ShareOff(context.Background(), 4, false); off || !errors.Is(err, ErrShareNotRunning) {
		t.Fatalf("a later claim running: %v %v, want ErrShareNotRunning", off, err)
	}
	r.mu.Lock()
	delete(r.running, 4)
	r.mu.Unlock()
	if off, err := r.ShareOff(context.Background(), 4, false); !off || err != nil {
		t.Fatalf("stranded alone: %v %v, want true", off, err)
	}
}

// getFailStore fails GetServerJob while fail is set.
type getFailStore struct {
	*store.DB
	fail atomic.Bool
}

func (s *getFailStore) GetServerJob(ctx context.Context, id int64) (*store.ServerJob, error) {
	if s.fail.Load() {
		return nil, errors.New("database is locked")
	}
	return s.DB.GetServerJob(ctx, id)
}

// A stranded job whose row can't be read when the link works again stays
// stranded and is resumed at a later look: dropped (as it was), nothing
// resumed it before a restart, and Runner.ShareOff no longer knew it, so a
// share job's book waited share_unconfirmed until then. Denied: one whose row
// says it ended, or has none, is dropped for good.
func TestStrandedJobWaitsOutAnUnreadableRow(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	st := &getFailStore{DB: db}
	ctx := context.Background()
	for _, id := range []int64{4, 5} {
		if err := db.RecordClaim(ctx, store.ServerJob{ServerID: testServer, ID: id, Kind: "k.v", Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.FinishServerJob(ctx, 5, 1, store.JobLost, nil, "taken back"); err != nil {
		t.Fatal(err)
	}
	ran := make(chan int64, 3)
	h := handler{kind: "k.v", lane: LaneLight, fn: func(_ context.Context, j Job, _ *Reporter) (any, error) {
		ran <- j.ID
		return nil, nil
	}}
	r := New(st, func() Server { return srv }, nil, testOpts(), h)
	r.mu.Lock()
	for _, id := range []int64{4, 5, 6} { // 6 has no row
		r.stranded = append(r.stranded, Job{serverID: testServer, ID: id, Kind: "k.v", Attempt: 1})
	}
	r.mu.Unlock()
	strandedIDs := func() (ids []int64) {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, s := range r.stranded {
			ids = append(ids, s.ID)
		}
		return ids
	}
	var wg sync.WaitGroup
	st.fail.Store(true)
	r.resumeStranded(ctx, srv, srv.id, &wg)
	if got := strandedIDs(); !slices.Equal(got, []int64{4, 5, 6}) {
		t.Fatalf("stranded after a look that could read no row = %v, want all three kept", got)
	}
	// Still stranded, it answers the contributing stage as one (nothing heard:
	// it can't be asked), not as a job no run here knows.
	if _, err := r.ShareOff(ctx, 4, false); !errors.Is(err, ErrShareOffLink) {
		t.Fatalf("ShareOff of the job kept stranded = %v, want ErrShareOffLink", err)
	}
	st.fail.Store(false)
	r.resumeStranded(ctx, srv, srv.id, &wg)
	wg.Wait()
	if got := strandedIDs(); len(got) != 0 {
		t.Fatalf("stranded after a look that read the rows = %v, want none", got)
	}
	close(ran)
	var got []int64
	for id := range ran {
		got = append(got, id)
	}
	if !slices.Equal(got, []int64{4}) {
		t.Fatalf("resumed %v, want only job 4 (5 ended, 6 has no row)", got)
	}
	if s, _ := srv.finalState(4); s != link.StateDone {
		t.Fatalf("job 4 ended %q, want done", s)
	}
}

// An answer that carries share_off and a cancel runs the handler's OnShareOff
// before the cancel ends the job's context: its act (keeping the book local)
// writes the store with that context.
func TestOnShareOffRunsBeforeTheAnswersCancel(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 7, Kind: "k.v", Attempts: 1})
	var calls atomic.Int32
	var live atomic.Bool
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.OnShareOff(func() {
			if calls.Add(1) == 1 {
				live.Store(ctx.Err() == nil)
			}
		})
		srv.mu.Lock()
		srv.shareOff, srv.cancel[7] = map[int64]bool{7: true}, true
		srv.mu.Unlock()
		rep.Progress(Progress{Step: "next"}) // a new step is reported at once
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool { s, _ := srv.finalState(7); return s == link.StateCancelled })
	if calls.Load() == 0 || !live.Load() {
		t.Fatalf("OnShareOff calls %d, the first with the job's context live: %v", calls.Load(), live.Load())
	}
}

// The handler's OnShareOff and the store's write run outside the Reporter's
// sendMu, and a write that keeps failing warns once a job, then logs at debug.
func TestShareOffWriteOutsideSendLockWarnsOnce(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	st := &shareOffFailStore{DB: db}
	st.fail.Store(true)
	srv.add(&link.Job{ID: 6, Kind: "k.v", Attempts: 1})
	logs := &logCounter{n: map[string]int{}}
	opts := testOpts()
	opts.Log = slog.New(logs)
	var locked, calls atomic.Int32
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.OnShareOff(func() {
			calls.Add(1)
			if !rep.sendMu.TryLock() {
				locked.Add(1)
				return
			}
			rep.sendMu.Unlock()
		})
		srv.setShareOff(map[int64]bool{6: true})
		waitFor(t, func() bool { return st.tries.Load() >= 3 })
		rep.OnShareOff(nil)
		return nil, nil
	}}
	r := New(st, func() Server { return srv }, nil, opts, h)
	runFor(t, r, func() bool { s, _ := srv.finalState(6); return s == link.StateDone })
	if calls.Load() == 0 || locked.Load() != 0 {
		t.Fatalf("OnShareOff calls %d, %d of them under sendMu", calls.Load(), locked.Load())
	}
	const warn, again = "WARN bridge: recording a job's share_off failed", "DEBUG bridge: recording a job's share_off failed again"
	if logs.count(warn) != 1 || logs.count(again) < 2 {
		t.Fatalf("warnings %d (want 1), debug lines %d (want 2 or more)", logs.count(warn), logs.count(again))
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
			rep := &Reporter{r: &Runner{server: func() Server { return srv }, onUnauthorized: func(Server) { unpaired = true }}, srv: srv, job: Job{serverID: testServer, ID: 81, Attempt: 2}, cancel: cancel}
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

// A parked job's report with a null park names none, as the server reads it:
// the stored park is kept, and Unpark drops it again.
func TestParkedProgressCarriesThroughANullPark(t *testing.T) {
	t.Parallel()
	park := map[string]any{"code": "agent_rate_limited"}
	rep := &Reporter{parked: true, parkMsg: "rate limited", progress: Progress{Step: "waiting", Extra: map[string]any{"park": park}}}
	rep.Progress(Progress{Step: "starting", Extra: map[string]any{"park": nil}})
	if got := rep.Last().Extra["park"]; got == nil {
		t.Fatalf("a null park dropped the stored one: %+v", rep.Last())
	}
	rep.Unpark()
	if _, ok := rep.Last().Extra["park"]; ok {
		t.Fatalf("Unpark kept the carried park: %+v", rep.Last())
	}
}

// The default keepalive is 30 s: a running job with nothing new reports twice a
// minute, which still leaves four renewals inside the server's 2-minute lease
// (a failed send or two never loses the claim). Change-driven reports keep their
// 2 s gap.
func TestDefaultKeepaliveLeavesLeaseMargin(t *testing.T) {
	t.Parallel()
	r := New(openStore(t), func() Server { return nil }, nil, Options{Log: quietLog()})
	if DefaultKeepalive != 30*time.Second || r.opts.Keepalive != DefaultKeepalive {
		t.Fatalf("Keepalive = %v, want 30s", r.opts.Keepalive)
	}
	if r.opts.ProgressEvery != 2*time.Second {
		t.Fatalf("ProgressEvery = %v, want 2s (change-driven reports unchanged)", r.opts.ProgressEvery)
	}
	const serverLease = 2 * time.Minute // audiosilo-server bridge.Lease
	if n := serverLease / DefaultKeepalive; n < 4 {
		t.Fatalf("only %d keepalives fit in the %v lease, want at least 4", n, serverLease)
	}
	if r.opts.ReportRetry != DefaultReportRetry {
		t.Fatalf("ReportRetry = %v, want DefaultReportRetry", r.opts.ReportRetry)
	}
	if n := serverLease / DefaultReportRetry; n < 10 {
		t.Fatalf("only %d retries fit in the %v lease, want at least 10", n, serverLease)
	}
}

// A report that fails is tried again after ReportRetry, not Keepalive, until one
// gets through; then the job is back on the Keepalive.
func TestFailedReportRetriesSoonerUntilOneSucceeds(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 10, Kind: "k.retry", Attempts: 1})
	opts := testOpts()
	opts.Keepalive = time.Second
	opts.ReportRetry = 50 * time.Millisecond
	tries := func() int {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.tries[10]
	}
	setFailing := func(v bool) {
		srv.mu.Lock()
		srv.failing = v
		srv.mu.Unlock()
	}
	await := func(cond func() bool, what string) error {
		for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for %s", what)
			}
		}
		return nil
	}
	r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.retry", lane: LaneLight,
		fn: func(_ context.Context, _ Job, _ *Reporter) (any, error) {
			setFailing(true)
			// The first keepalive (a second after the start's report) fails...
			first := tries()
			if err := await(func() bool { return tries() > first }, "the first keepalive"); err != nil {
				return nil, err
			}
			// ...and is tried again within ReportRetry, not a second later.
			failed := tries()
			time.Sleep(400 * time.Millisecond)
			if got := tries() - failed; got < 2 {
				return nil, fmt.Errorf("%d tries in 400 ms after a failed report, want a retry every 50 ms", got)
			}
			// Once one gets through, the job is back on the one-second keepalive.
			running := countState(srv, 10, link.StateRunning)
			setFailing(false)
			if err := await(func() bool { return countState(srv, 10, link.StateRunning) > running }, "a report through"); err != nil {
				return nil, err
			}
			// (300 ms, well short of the second, so a slow wake on a loaded
			// machine still lands before the next keepalive.)
			through := tries()
			time.Sleep(300 * time.Millisecond)
			if got := tries() - through; got != 0 {
				return nil, fmt.Errorf("%d reports in 300 ms after one got through, want none before the keepalive", got)
			}
			return nil, nil
		}})
	wantDone(t, r, srv, 10)
}

// Only a report that may get through if sent again soon takes the ReportRetry:
// a 4xx refusal (bar 408 and 429) waits the Keepalive like any report.
func TestUnreachable(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err  error
		want bool
	}{
		{errors.New("connection refused"), true},
		{context.DeadlineExceeded, true},
		{&link.APIError{Status: http.StatusInternalServerError}, true},
		{&link.APIError{Status: http.StatusBadGateway}, true},
		{&link.APIError{Status: http.StatusTooManyRequests}, true},
		{&link.APIError{Status: http.StatusRequestTimeout}, true},
		{&link.APIError{Status: http.StatusBadRequest}, false},
		{&link.APIError{Status: http.StatusNotFound}, false},
		{&link.APIError{Status: http.StatusConflict, Code: "kind_unavailable"}, false},
		{fmt.Errorf("wrapped: %w", &link.APIError{Status: http.StatusBadRequest}), false},
	} {
		if got := unreachable(c.err); got != c.want {
			t.Errorf("unreachable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// A running job with no new progress reports once per Keepalive, not once per
// ProgressEvery tick.
func TestIdleJobReportsOncePerKeepalive(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 9, Kind: "k.idle", Attempts: 1})
	opts := testOpts()
	opts.Keepalive = 150 * time.Millisecond
	r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.idle", lane: LaneLight,
		fn: func(_ context.Context, _ Job, _ *Reporter) (any, error) {
			start := countState(srv, 9, link.StateRunning)
			time.Sleep(500 * time.Millisecond)
			// ~3 keepalives in 500 ms at 150 ms; a report per 1 ms tick would be hundreds.
			if got := countState(srv, 9, link.StateRunning) - start; got < 1 || got > 4 {
				return nil, fmt.Errorf("keepalive reports in 500 ms = %d, want 1 to 4 at a 150 ms keepalive", got)
			}
			return nil, nil
		}})
	wantDone(t, r, srv, 9)
}

// wantDone runs r until job id ends and fails the test unless it ended done.
func wantDone(t *testing.T, r *Runner, srv *fakeServer, id int64) {
	t.Helper()
	runFor(t, r, func() bool { s, _ := srv.finalState(id); return s != "" })
	if s, u := srv.finalState(id); s != link.StateDone {
		msg := ""
		if u.Error != nil {
			msg = *u.Error
		}
		t.Fatalf("final = %q: %s", s, msg)
	}
}

// releaseFirst is an abandoner that records, at each Abandon, the state the
// store holds for the job: a job another waits on (generate's awaitOpenJobEnd
// reads the row's running state) must still read running while its work is
// released, never lost before (bridge#37).
type releaseFirst struct {
	handler
	db *store.DB
	mu sync.Mutex
	at []string
}

func (a *releaseFirst) Abandon(ctx context.Context, j Job) {
	sj, err := a.db.GetServerJob(context.WithoutCancel(ctx), j.ID)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.at = append(a.at, "error: "+err.Error())
		return
	}
	a.at = append(a.at, sj.State)
}

func (a *releaseFirst) states() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.at)
}

// A start that drops another server's running job releases it before it
// records it lost.
func TestDroppedJobIsReleasedBeforeItIsLost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, srv := openStore(t), newFakeServer()
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 9, Kind: "k.v", Attempt: 1, ServerID: "srv-old"}); err != nil {
		t.Fatal(err)
	}
	h := &releaseFirst{db: db, handler: handler{kind: "k.v", lane: LaneLight,
		fn: func(context.Context, Job, *Reporter) (any, error) { return nil, nil }}}
	srv.id = "srv-new"
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	runFor(t, r, func() bool {
		sj, err := db.GetServerJob(ctx, 9)
		return err == nil && sj.State == store.JobLost
	})
	if got := h.states(); !slices.Equal(got, []string{store.JobRunning}) {
		t.Fatalf("job state at Abandon = %v, want [running] (released before it is recorded lost)", got)
	}
}

// A job a refused token stopped, found on another server's link once the
// Bridge is paired again, is released before it is recorded lost.
func TestStrandedJobOfAnotherServerIsReleasedBeforeItIsLost(t *testing.T) {
	t.Parallel()
	db, a, b := openStore(t), newFakeServer(), newFakeServer()
	a.id, b.id = "a", "b"
	a.add(&link.Job{ID: 6, Kind: "k.v", Attempts: 1})
	var mu sync.Mutex
	current := Server(a)
	h := &releaseFirst{db: db, handler: handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, _ Job, _ *Reporter) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
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
	if got := h.states(); !slices.Equal(got, []string{store.JobRunning}) {
		t.Fatalf("job state at Abandon = %v, want [running] (released before it is recorded lost)", got)
	}
}

// switchingServer is a link that has the Bridge paired with another server
// as it answers the next report once armed.
type switchingServer struct {
	*fakeServer
	armed    atomic.Bool
	onUpdate func()
}

func (s *switchingServer) UpdateJob(ctx context.Context, id int64, u link.Update) (*link.Job, error) {
	if s.armed.CompareAndSwap(true, false) {
		s.onUpdate()
	}
	return s.fakeServer.UpdateJob(ctx, id, u)
}

// A handler's ask before it files (Reporter.AskShareOff) is answered while the
// Bridge's current link reaches the job's server, a new pairing with it
// included (allowed: the ask goes over the new link, and the run is stopped to
// run again there, errRelinked); unpaired it confirms nothing (denied); and
// paired with another server, before the ask or while its answer was on its
// way, the job is dropped: stopped, recorded lost and reported to no other
// server (denied). Runner.ShareOff asks the same way.
func TestAskShareOffOnlyOnTheJobsServer(t *testing.T) {
	t.Parallel()
	db, other := openStore(t), newFakeServer()
	srv := newFakeServer()
	again := &switchingServer{fakeServer: newFakeServer()}
	srv.id, again.id, other.id = "a", "a", "b"
	var cur atomic.Pointer[Server]
	setCur := func(s Server) { cur.Store(&s) }
	setCur(srv)
	again.onUpdate = func() { setCur(other) }
	reps := make(chan *Reporter, 2)
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, _ Job, rep *Reporter) (any, error) {
		reps <- rep
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	opts := testOpts()
	opts.Keepalive = time.Hour // only the asks report
	r := New(db, func() Server {
		if s := cur.Load(); s != nil {
			return *s
		}
		return nil
	}, nil, opts, h)
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1, State: "claimed"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	rep := <-reps

	if off, err := rep.AskShareOff(ctx); off || err != nil {
		t.Fatalf("on the job's link: %v %v, want false, nil", off, err)
	}
	cur.Store(nil) // unpaired
	if _, err := rep.AskShareOff(ctx); !errors.Is(err, ErrShareOffLink) {
		t.Fatalf("unpaired: %v, want ErrShareOffLink", err)
	}
	if _, err := r.ShareOff(ctx, 1, false); !errors.Is(err, ErrShareOffLink) {
		t.Fatalf("Runner.ShareOff unpaired: %v, want ErrShareOffLink", err)
	}
	// Paired again with the same server: a new link, which the ask uses.
	setCur(again)
	if off, err := rep.AskShareOff(ctx); off || err != nil {
		t.Fatalf("on a new link to the job's server: %v %v, want false, nil", off, err)
	}
	if again.triesOf(1) == 0 {
		t.Fatal("the ask didn't go over the new link to the job's server")
	}
	if !errors.Is(context.Cause(rep.ctx), errRelinked) {
		t.Fatalf("the run on the old link ended %v, want stopped to run again on the new one", context.Cause(rep.ctx))
	}
	rep = <-reps // resumed under its claim on the new link
	again.armed.Store(true)
	if _, err := r.ShareOff(ctx, 1, true); err == nil {
		t.Fatal("paired with another server while the answer came: nil, want an error")
	}
	if !errors.Is(context.Cause(rep.ctx), errOtherServer) {
		t.Fatalf("the job's end = %v, want dropped for another server", context.Cause(rep.ctx))
	}
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(ctx, 1)
		return err == nil && sj.State == store.JobLost
	})
	if n := other.triesOf(1); n != 0 {
		t.Fatalf("another server got %d of the job's reports", n)
	}
}

// The share job stuck on the old link (BR-LINK-RULE): a job parked on a
// pairing that has since moved to another server is dropped by its next ask,
// once and for good, rather than parking again at every recheck on the old
// link; and a job waiting with no link at all keeps saying it can't confirm.
func TestAskShareOffDropsAJobOfAnotherServer(t *testing.T) {
	t.Parallel()
	db, srv, other := openStore(t), newFakeServer(), newFakeServer()
	srv.id, other.id = "a", "b"
	var cur atomic.Pointer[fakeServer]
	cur.Store(srv)
	reps := make(chan *Reporter, 1)
	h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, _ Job, rep *Reporter) (any, error) {
		rep.Park("share_unconfirmed", "waiting")
		reps <- rep
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	opts := testOpts()
	opts.Keepalive = time.Hour
	r := New(db, func() Server {
		if s := cur.Load(); s != nil {
			return s
		}
		return nil
	}, nil, opts, h)
	srv.add(&link.Job{ID: 2, Kind: "k.v", Attempts: 1, State: "claimed"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	rep := <-reps
	cur.Store(nil) // unpaired: nothing confirms the share, nothing is dropped
	for range 2 {
		if _, err := rep.AskShareOff(ctx); !errors.Is(err, ErrShareOffLink) {
			t.Fatalf("unpaired: %v, want ErrShareOffLink", err)
		}
	}
	cur.Store(other) // paired with another server (no wake: the ask finds it)
	if _, err := rep.AskShareOff(ctx); !errors.Is(err, ErrStopped) {
		t.Fatalf("paired with another server: %v, want ErrStopped", err)
	}
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(ctx, 2)
		return err == nil && sj.State == store.JobLost && sj.ServerID == "a"
	})
	if n := other.triesOf(2); n != 0 {
		t.Fatalf("the new server heard of the old server's job %d times", n)
	}
}

// A running job's Bridge paired with another server is dropped as soon as the
// runner wakes (dropOtherServers), not left reporting on the old link; one
// paired again with its own server at a new address is stopped and run again
// under its claim on the new link (errRelinked: its handler may hold the old,
// revoked link), and ends there (allowed and denied re-pairs).
func TestRepairWithARunningJob(t *testing.T) {
	for name, nextID := range map[string]string{"same server": "a", "another server": "b"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, srv, next := openStore(t), newFakeServer(), newFakeServer()
			srv.id, next.id = "a", nextID
			var cur atomic.Pointer[fakeServer]
			cur.Store(srv)
			running, finish := make(chan struct{}), make(chan struct{})
			var runs atomic.Int32
			h := handler{kind: "k.v", lane: LaneLight, fn: func(ctx context.Context, j Job, _ *Reporter) (any, error) {
				if runs.Add(1) == 1 {
					close(running)
				} else if !j.Resumed {
					t.Error("the run again on the new link isn't its claim resumed")
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-finish:
					return nil, nil
				}
			}}
			opts := testOpts()
			opts.Keepalive = 20 * time.Millisecond
			r := New(db, func() Server { return cur.Load() }, nil, opts, h)
			srv.add(&link.Job{ID: 3, Kind: "k.v", Attempts: 1, State: "claimed"})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			<-running
			cur.Store(next)
			r.Wake()
			if nextID == "b" {
				waitFor(t, func() bool {
					sj, err := db.GetServerJob(ctx, 3)
					return err == nil && sj.State == store.JobLost
				})
				if n := next.triesOf(3); n != 0 {
					t.Fatalf("another server got %d of the job's reports", n)
				}
				return
			}
			waitFor(t, func() bool { return runs.Load() == 2 })
			close(finish)
			waitFor(t, func() bool { s, _ := next.finalState(3); return s == link.StateDone })
			if s, _ := srv.finalState(3); s == link.StateDone {
				t.Fatal("the end went to the old link")
			}
		})
	}
}

// A job of the server the Bridge no longer works for is recorded lost on its
// own row only: once the new server's claim of the same id and attempt holds
// the row (it claims as the old server's runs end), the drop leaves it alone
// (denied), and a row still the old server's is recorded lost (allowed).
func TestDroppedJobLeavesTheNewServersRow(t *testing.T) {
	t.Parallel()
	db, ctx := openStore(t), context.Background()
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 5, Kind: "k.v", Attempt: 1, ServerID: "b"}); err != nil {
		t.Fatal(err)
	}
	r := New(db, func() Server { return nil }, nil, testOpts())
	r.recordLost(ctx, Job{ID: 5, Attempt: 1, serverID: "a"}, nil, errOtherServer)
	if sj, err := db.GetServerJob(ctx, 5); err != nil || sj.State != store.JobRunning || sj.ServerID != "b" {
		t.Fatalf("the new server's row after the old one's drop: %+v %v, want it running", sj, err)
	}
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 6, Kind: "k.v", Attempt: 1, ServerID: "a"}); err != nil {
		t.Fatal(err)
	}
	r.recordLost(ctx, Job{ID: 6, Attempt: 1, serverID: "a"}, nil, errOtherServer)
	if sj, err := db.GetServerJob(ctx, 6); err != nil || sj.State != store.JobLost {
		t.Fatalf("the old server's own row: %+v %v, want lost", sj, err)
	}
}

// A row from before the Bridge recorded its server (or a server that named
// none) is no known server's job: a start drops it, recorded lost, never run
// and reporting nothing on the current link (denied), rather than taking it as
// that server's (BR-LINK-RULE: unknown is never the same). The server's own
// lease lapses it, and it is handed out again.
func TestRecoveredUnknownServerJobIsDropped(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	if err := db.RecordClaim(context.Background(), store.ServerJob{ID: 3, Kind: "k.v", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	var ran atomic.Bool
	h := handler{kind: "k.v", lane: LaneLight, fn: func(context.Context, Job, *Reporter) (any, error) {
		ran.Store(true)
		return nil, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool {
		sj, err := db.GetServerJob(ctx, 3)
		return err == nil && sj.State == store.JobLost
	})
	if ran.Load() {
		t.Fatal("a job of no known server ran")
	}
	if n := srv.triesOf(3); n != 0 {
		t.Fatalf("the current server got %d of an unknown server's job's reports", n)
	}
}

// The one rule's unknown: an id that isn't known is never the same server, on
// either side, and a job no runner claimed keys by the pairing.
func TestUnknownServerIsNeverTheSame(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{{"a", "a", true}, {"a", "b", false}, {"", "a", false}, {"a", "", false}, {"", "", false}} {
		if got := sameServer(c.a, c.b); got != c.same {
			t.Errorf("sameServer(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
	if got := (Job{serverID: "a"}).KeyServer("b"); got != "a" {
		t.Errorf("a claimed job's key server = %q, want its own", got)
	}
	if got := (Job{}).KeyServer("b"); got != "b" {
		t.Errorf("an unclaimed job's key server = %q, want the pairing's", got)
	}
}

// Link hands a job's handler the link only while it reaches the job's server:
// another server's link, or one that names no server, is no link at all.
func TestLinkOnlyReachesTheJobsServer(t *testing.T) {
	a, b, unknown := newFakeServer(), newFakeServer(), newFakeServer()
	a.id, b.id, unknown.id = "a", "b", ""
	job := Job{serverID: "a"}
	if Link[Server](job, a) == nil {
		t.Fatal("the job's own server's link was withheld")
	}
	if Link[Server](job, b) != nil || Link[Server](job, unknown) != nil || Link[Server](job, nil) != nil {
		t.Fatal("another (or an unknown) server's link was handed to the job")
	}
	if Link[Server](Job{}, b) == nil {
		t.Fatal("a job no runner claimed lost its link")
	}
	// A job claimed under a pairing: its own pairing's link to its server
	// carries it (allowed); a new pairing's with the same server (an unpair,
	// then a pairing again) carries nothing of it (denied).
	own, again := newFakeServer(), newFakeServer()
	own.id, own.pairing, again.id, again.pairing = "a", "p1", "a", "p2"
	paired := Job{serverID: "a", pairing: "p1"}
	if Link[Server](paired, own) == nil {
		t.Fatal("the job's own pairing's link was withheld")
	}
	if Link[Server](paired, again) != nil {
		t.Fatal("a new pairing's link was handed to an earlier pairing's job")
	}
}
