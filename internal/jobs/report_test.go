package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// logCounter counts log records by message.
type logCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *logCounter) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCounter) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[r.Level.String()+" "+r.Message]++
	return nil
}
func (c *logCounter) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCounter) WithGroup(string) slog.Handler      { return c }

func (c *logCounter) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[key]
}

func awaitCond(cond func() bool, what string) error {
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", what)
		}
	}
	return nil
}

func (f *fakeServer) setFailing(v bool) {
	f.mu.Lock()
	f.failing = v
	f.mu.Unlock()
}

func (f *fakeServer) triesOf(id int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tries[id]
}

func (p *Reporter) failingNow() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failing
}

// While the server is unreachable a job warns once, not at every retry, and
// says once that its reports get through again (bridge#31).
func TestFailedReportsWarnOnceAndRecoverOnce(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 11, Kind: "k.outage", Attempts: 1})
	logs := &logCounter{n: map[string]int{}}
	opts := testOpts()
	opts.Keepalive, opts.ReportRetry, opts.Log = time.Second, 10*time.Millisecond, slog.New(logs)
	const warn, again, back = "WARN bridge: reporting progress failed", "DEBUG bridge: reporting progress failed again",
		"INFO bridge: reporting progress again"
	r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.outage", lane: LaneLight,
		fn: func(_ context.Context, _ Job, rep *Reporter) (any, error) {
			srv.setFailing(true)
			rep.Progress(Progress{Step: "one"}) // a new step reports at once, and fails
			if err := awaitCond(func() bool { return logs.count(again) >= 3 }, "three more failed reports"); err != nil {
				return nil, err
			}
			if n := logs.count(warn); n != 1 {
				return nil, fmt.Errorf("%d warnings over %d failed reports, want 1", n, srv.triesOf(11))
			}
			srv.setFailing(false)
			if err := awaitCond(func() bool { return logs.count(back) == 1 }, "the recovery line"); err != nil {
				return nil, err
			}
			// A second outage warns again, once.
			srv.setFailing(true)
			rep.Progress(Progress{Step: "two"})
			if err := awaitCond(func() bool { return logs.count(warn) == 2 }, "the second outage's warning"); err != nil {
				return nil, err
			}
			srv.setFailing(false)
			return nil, awaitCond(func() bool { return logs.count(back) == 2 }, "the second recovery line")
		}})
	wantDone(t, r, srv, 11)
}

// A ConfirmLease that gets through ends the outage as a report would: the job
// is back on the Keepalive and the recovery is logged.
func TestConfirmLeaseClearsFailing(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 12, Kind: "k.confirm", Attempts: 1})
	logs := &logCounter{n: map[string]int{}}
	opts := testOpts()
	opts.Keepalive, opts.ReportRetry, opts.Log = time.Hour, time.Hour, slog.New(logs)
	r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.confirm", lane: LaneLight,
		fn: func(ctx context.Context, _ Job, rep *Reporter) (any, error) {
			srv.setFailing(true)
			rep.Progress(Progress{Step: "one"})
			if err := awaitCond(rep.failingNow, "a failed report"); err != nil {
				return nil, err
			}
			if err := rep.ConfirmLease(ctx); err == nil {
				return nil, errors.New("ConfirmLease got through an unreachable server")
			}
			if !rep.failingNow() {
				return nil, errors.New("a failed ConfirmLease cleared failing")
			}
			srv.setFailing(false)
			if err := rep.ConfirmLease(ctx); err != nil {
				return nil, err
			}
			if rep.failingNow() || logs.count("INFO bridge: reporting progress again") != 1 {
				return nil, fmt.Errorf("after a ConfirmLease got through: failing %v, recovery lines %d",
					rep.failingNow(), logs.count("INFO bridge: reporting progress again"))
			}
			return nil, nil
		}})
	wantDone(t, r, srv, 12)
}

// Refresh reports the job at once, parked as it is: nil while the server holds
// it, ErrStopped once the server's answer carries a cancel, which ends the job's
// context.
func TestRefreshHearsACancelAtOnce(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 13, Kind: "k.refresh", Attempts: 1})
	opts := testOpts()
	opts.Keepalive, opts.ReportRetry = time.Hour, time.Hour
	r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.refresh", lane: LaneLight,
		fn: func(ctx context.Context, _ Job, rep *Reporter) (any, error) {
			rep.Park("some_park", "Waiting on something")
			if err := awaitCond(func() bool { return countState(srv, 13, link.StateNeedsAttention) == 1 }, "the park"); err != nil {
				return nil, err
			}
			if err := rep.Refresh(context.Background()); err != nil {
				return nil, fmt.Errorf("Refresh of a held job = %v", err)
			}
			if n := countState(srv, 13, link.StateNeedsAttention); n != 2 {
				return nil, fmt.Errorf("%d needs_attention reports after a Refresh of a parked job, want 2", n)
			}
			srv.mu.Lock()
			srv.cancel[13] = true
			srv.mu.Unlock()
			if err := rep.Refresh(context.Background()); !errors.Is(err, ErrStopped) {
				return nil, fmt.Errorf("Refresh of a cancelled job = %v, want ErrStopped", err)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}})
	runFor(t, r, func() bool { s, _ := srv.finalState(13); return s == link.StateCancelled })
}

// A LocalReporter has no server to ask.
func TestLocalReporterRefresh(t *testing.T) {
	if err := LocalReporter().Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}
