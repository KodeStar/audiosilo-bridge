package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

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
// goes on reporting (the lease holds, a cancel still reaches it); Unpark sends
// running with the error cleared.
func TestParkAndUnpark(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 1, Kind: "k.v", Attempts: 1})
	h := handler{kind: "k.v", lane: LanePipeline, fn: func(ctx context.Context, j Job, rep *Reporter) (any, error) {
		rep.Progress(Progress{Step: "fact_pass", Extra: map[string]any{"park": map[string]string{"code": "agent_unavailable"}}})
		rep.Park("agent_unavailable", "the agent container isn't answering")
		waitFor(t, func() bool { return countState(srv, 1, link.StateNeedsAttention) >= 2 })
		if parked, msg := rep.Parked(); !parked || msg != "the agent container isn't answering" {
			t.Errorf("Parked() = %v %q", parked, msg)
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
		parked, msg := rep.Parked()
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
