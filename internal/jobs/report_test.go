package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/testwait"
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
	for deadline := time.Now().Add(testwait.Scaled(5 * time.Second)); !cond(); time.Sleep(5 * time.Millisecond) {
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
			if err := awaitCond(rep.unreached, "a failed report"); err != nil {
				return nil, err
			}
			if err := rep.ConfirmLease(ctx); err == nil {
				return nil, errors.New("ConfirmLease got through an unreachable server")
			}
			if !rep.unreached() {
				return nil, errors.New("a failed ConfirmLease cleared failing")
			}
			srv.setFailing(false)
			if err := rep.ConfirmLease(ctx); err != nil {
				return nil, err
			}
			if rep.unreached() || logs.count("INFO bridge: reporting progress again") != 1 {
				return nil, fmt.Errorf("after a ConfirmLease got through: failing %v, recovery lines %d",
					rep.unreached(), logs.count("INFO bridge: reporting progress again"))
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

// A Refresh that doesn't reach the server counts as a failed report: the job
// reads as unreachable until a report gets through, so a check that doesn't
// ask (Runner.ShareOff without ask, the contributing stage's before its
// lookups) says so at once rather than after one more round of lookups.
// Denied: a Refresh that failed because its caller's context ended says
// nothing about the server.
func TestFailedRefreshSetsFailing(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 14, Kind: "k.refresh", Attempts: 1})
	opts := testOpts()
	opts.Keepalive, opts.ReportRetry = time.Hour, time.Hour
	var r *Runner
	r = New(db, func() Server { return srv }, nil, opts, handler{kind: "k.refresh", lane: LaneLight,
		fn: func(ctx context.Context, _ Job, rep *Reporter) (any, error) {
			srv.setFailing(true)
			defer srv.setFailing(false) // a failure below is still reported
			ended, end := context.WithCancel(ctx)
			end()
			if err := rep.Refresh(ended); err == nil {
				return nil, errors.New("Refresh got through an unreachable server")
			}
			if rep.unreached() {
				return nil, errors.New("a Refresh whose caller's context ended set failing")
			}
			if off, err := r.ShareOff(ctx, 14, false); off || err != nil {
				return nil, fmt.Errorf("ShareOff, not asked, before a failed Refresh = %v %v, want false", off, err)
			}
			if err := rep.Refresh(ctx); err == nil {
				return nil, errors.New("Refresh got through an unreachable server")
			}
			if !rep.unreached() {
				return nil, errors.New("a failed Refresh didn't set failing")
			}
			if _, err := r.ShareOff(ctx, 14, false); !errors.Is(err, ErrShareNotReachable) {
				return nil, fmt.Errorf("ShareOff, not asked, after a failed Refresh = %v, want ErrShareNotReachable", err)
			}
			srv.setFailing(false)
			if err := rep.Refresh(ctx); err != nil {
				return nil, err
			}
			if rep.unreached() {
				return nil, errors.New("a Refresh that got through left failing set")
			}
			return nil, nil
		}})
	wantDone(t, r, srv, 14)
}

// A LocalReporter has no server to ask.
func TestLocalReporterRefresh(t *testing.T) {
	if err := LocalReporter().Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCancelContextReadsAsAnAdminsCancel(t *testing.T) {
	ctx, cancel := CancelContext(context.Background())
	if !Resumes(ctx) {
		t.Fatal("a running job's context resumes")
	}
	cancel()
	if ctx.Err() == nil || Resumes(ctx) {
		t.Fatal("an admin's cancel ends the job for good")
	}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if !Resumes(stopped) {
		t.Fatal("a stop resumes at the next start")
	}
}

// A cancel's cause says by whom when the server's answer does (bridge#80): a
// cancel_requested with the server's own reason as the job's error is the
// server's, one with none an admin's, and a 409 not_held "cancelled" (sent
// for both) says neither. Every one reads as a cancel: Resumes false.
func TestCancelCauseWordsTheServersReason(t *testing.T) {
	for _, c := range []struct {
		name string
		got  *link.Job
		err  error
		want string
	}{
		{"the server's", &link.Job{CancelRequested: true, Error: "the Audible account was removed"}, nil,
			"the job was cancelled by the server: the Audible account was removed"},
		{"an admin's", &link.Job{CancelRequested: true}, nil, "the job was cancelled by an admin"},
		{"either", nil, link.ErrCancelledHere, "the job was cancelled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			stop := (&Reporter{}).answer(c.got, c.err, false)
			if stop == nil || !errors.Is(stop, errCancelRequested) || stop.Error() != c.want {
				t.Fatalf("cause = %v, want %q", stop, c.want)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(stop)
			if Resumes(ctx) || Lost(ctx) {
				t.Fatal("a cancel resumes or reads as lost")
			}
		})
	}
	if stop := (&Reporter{}).answer(&link.Job{Error: "a park's message"}, nil, true); stop != nil {
		t.Fatalf("an error with no cancel asked = %v", stop)
	}
	// A parked report's own message as the job's error is no server reason:
	// the cancel says neither, whatever the server's cleaning made of the
	// message (a tab is a space there) and whether the job is still parked
	// here when the answer comes.
	for _, back := range []string{"a park's message", "a park's message: exit 1"} {
		parked := &Reporter{parked: true, parkMsg: "a park's message:\texit 1"}
		if stop := parked.answer(&link.Job{CancelRequested: true, Error: back}, nil, true); stop == nil ||
			stop.Error() != errCancelRequested.Error() {
			t.Fatalf("a cancel of a parked job (error %q) = %v", back, stop)
		}
	}
	if stop := (&Reporter{}).answer(&link.Job{CancelRequested: true, Error: "a park's message"}, nil, true); stop == nil ||
		stop.Error() != errCancelRequested.Error() {
		t.Fatalf("a cancel answering a parked report of a job unparked since = %v", stop)
	}
	if !errors.Is(errCancelledOnPage, errCancelRequested) {
		t.Fatal("a cancel on the Bridge's page isn't a cancel")
	}
}

// storedAs reports whether job id is recorded in state and reported.
func storedAs(db *store.DB, id int64, state string) bool {
	sj, err := db.GetServerJob(context.Background(), id)
	return err == nil && sj.State == state && sj.Reported
}

func TestJobRetryDeadlineFollowsLease(t *testing.T) {
	now := time.Now().UTC()
	claim := now.Add(time.Minute).Format(time.RFC3339Nano)
	job := jobOf(&link.Job{LeaseUntil: &claim})
	rep := &Reporter{leaseUntil: job.leaseUntil}
	if got := rep.retryDeadline(); !got.Equal(now.Add(time.Minute - leaseMargin)) {
		t.Fatalf("claim retry deadline = %v", got)
	}
	renewal := now.Add(2 * time.Minute).Format(time.RFC3339Nano)
	rep.answer(&link.Job{LeaseUntil: &renewal}, nil, false)
	rep.answer(nil, &link.APIError{Status: http.StatusTooManyRequests}, false)
	if got := rep.retryDeadline(); !got.Equal(now.Add(2*time.Minute - leaseMargin)) {
		t.Fatalf("renewed retry deadline = %v", got)
	}
	rep.answer(&link.Job{State: link.StateNeedsAttention}, nil, true)
	if got := rep.retryDeadline(); !got.IsZero() {
		t.Fatalf("parked retry deadline = %v", got)
	}
}

// The server schedules another run of its own jobs after a failure. Only an
// admin-started job can wait for someone to cancel and queue it again.
func TestRateLimitedJob(t *testing.T) {
	t.Parallel()
	const refused = "server answered 429: Too Many Requests"
	const attention = "the server is still rate limiting the Bridge (attempt 5): " + refused +
		". Cancel and queue the job again when the server is ready."
	for _, tc := range []struct {
		name           string
		queued         bool
		refusals       int32
		state, message string
	}{
		{"recovers", true, 4, store.JobDone, ""},
		{"server queued", true, 5, store.JobFailed, refused},
		{"admin", false, 5, store.JobNeedsAttention, attention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests, runs atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if n := requests.Add(1); n <= tc.refusals {
					after := "0"
					if n == 5 {
						after = "60" // past the remaining lease
					}
					w.Header().Set("Retry-After", after)
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				_, _ = w.Write([]byte(`{"path":"A/B"}`))
			}))
			defer api.Close()
			cl, _ := link.New(api.URL, "", "dev")
			db, srv := openStore(t), newFakeServer()
			until := time.Now().Add(leaseMargin + time.Second).Format(time.RFC3339Nano)
			srv.add(&link.Job{ID: 31, Kind: "k.scan", Attempts: 1, LeaseUntil: &until})
			r := New(db, func() Server { return srv }, nil, testOpts(), handler{kind: "k.scan", lane: LaneLight, queued: tc.queued,
				fn: func(ctx context.Context, _ Job, _ *Reporter) (any, error) {
					runs.Add(1)
					_, err := cl.Rescan(ctx, 1, "A/B")
					return nil, err
				}})
			runFor(t, r, func() bool { return storedAs(db, 31, tc.state) })
			state, final := srv.finalState(31)
			if state != tc.state || runs.Load() != 1 || requests.Load() != 5 {
				t.Fatalf("state %s, runs %d, requests %d", state, runs.Load(), requests.Load())
			}
			if final.Error == nil || *final.Error != tc.message {
				t.Fatalf("final report = %+v, want error %q", final, tc.message)
			}
		})
	}
}

func TestRateLimitedKeepaliveAllowsProgressAndFinish(t *testing.T) {
	t.Parallel()
	const kind = "bridge.verify"
	var claimed atomic.Bool
	var running atomic.Int32
	var sawProgress atomic.Bool
	refused := make(chan struct{})
	expires := make(chan time.Time, 1)
	finish := make(chan link.Update, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/v1/bridge/jobs/next":
			if claimed.Swap(true) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(link.Job{ID: 31, Kind: kind, Attempts: 1})
		case "/api/v1/bridge/jobs/31":
			var u link.Update
			if err := json.NewDecoder(req.Body).Decode(&u); err != nil || u.State == nil {
				t.Errorf("invalid update: %+v, %v", u, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if *u.State == link.StateRunning {
				switch running.Add(1) {
				case 1:
					end := time.Now().Add(time.Second)
					expires <- end
					until := end.UTC().Format(time.RFC3339Nano)
					_ = json.NewEncoder(w).Encode(link.Job{LeaseUntil: &until})
					return
				case 2:
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(http.StatusTooManyRequests)
					close(refused)
					return
				}
				var pr Progress
				if json.Unmarshal(u.Progress, &pr) == nil && pr.Step == "checked" {
					sawProgress.Store(true)
				}
			} else {
				finish <- u
			}
			_ = json.NewEncoder(w).Encode(link.Job{State: *u.State})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(api.Close)
	cl, err := link.New(api.URL, "", "dev")
	if err != nil {
		t.Fatal(err)
	}
	cl = cl.WithServerID(testServer)
	r := New(openStore(t), func() Server { return cl }, nil, testOpts(), handler{kind: kind, lane: LaneLight,
		fn: func(ctx context.Context, _ Job, rep *Reporter) (any, error) {
			select {
			case <-refused:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			rep.Progress(Progress{Step: "checked"})
			return nil, rep.Refresh(ctx)
		}})
	startControlled(t, r)
	end := await(t, expires)
	select {
	case u := <-finish:
		if *u.State != store.JobDone || !sawProgress.Load() {
			t.Fatalf("finish = %+v, server saw progress = %t", u, sawProgress.Load())
		}
	case <-time.After(time.Until(end)):
		t.Fatal("the keepalive's Retry-After held up progress or finish past the lease")
	}
}

func TestIdleReportDelayAfterRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := New(nil, nil, nil, testOpts())
		rep := &Reporter{r: r}
		for _, tc := range []struct{ after, want time.Duration }{
			{0, r.opts.ReportRetry},
			{15 * time.Second, 15 * time.Second},
			{2 * time.Minute, time.Minute},
		} {
			rep.failed(&link.APIError{Status: http.StatusTooManyRequests, RetryAfter: tc.after, RetryAfterSet: true})
			if delay := time.Until(rep.nextIdle); delay != tc.want {
				t.Fatalf("Retry-After %v: next idle report in %v, want %v", tc.after, delay, tc.want)
			}
		}
		rep.reached()
		if delay := time.Until(rep.nextIdle); delay != r.opts.Keepalive {
			t.Fatalf("after recovery: next idle report in %v, want %v", delay, r.opts.Keepalive)
		}
	})
}

// Changed progress waits out a 429's Retry-After as the keepalive does, so a
// busy job does not ask a rate-limiting server again every ProgressEvery.
func TestProgressWaitsOutRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := New(nil, nil, nil, testOpts())
		rep := &Reporter{r: r}
		rep.reached()
		rep.Progress(Progress{Step: "copying", Done: 1})
		time.Sleep(r.opts.ProgressEvery)
		if !rep.due(time.Now()) {
			t.Fatal("changed progress not sent after ProgressEvery")
		}
		rep.failed(&link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 30 * time.Second, RetryAfterSet: true})
		rep.Progress(Progress{Step: "copying", Done: 2})
		time.Sleep(time.Second)
		if rep.due(time.Now()) {
			t.Fatal("changed progress sent before the 429's Retry-After")
		}
		time.Sleep(29 * time.Second)
		if !rep.due(time.Now()) {
			t.Fatal("changed progress held past the 429's Retry-After")
		}
		rep.failed(&link.APIError{Status: http.StatusServiceUnavailable})
		time.Sleep(r.opts.ProgressEvery)
		if !rep.due(time.Now()) {
			t.Fatal("a 5xx held changed progress")
		}
	})
}

// A 429 never shortens the delay a report or claim would wait anyway.
func TestRetryAfterKeepsLongerDelay(t *testing.T) {
	limited := &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 10 * time.Second, RetryAfterSet: true}
	if got := retryAfter(90*time.Second, limited); got != 90*time.Second {
		t.Fatalf("retryAfter(90s, 429 10s) = %v", got)
	}
	if got := retryAfter(time.Second, limited); got != 10*time.Second {
		t.Fatalf("retryAfter(1s, 429 10s) = %v", got)
	}
}

// A Refresh waiting out a 429 when its job settles reports nothing more: a
// running report after the end could land after it.
func TestRefreshAfterSettleSendsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := &limitedRefreshServer{fakeServer: newFakeServer()}
		r := New(nil, func() Server { return srv }, nil, testOpts())
		rep := &Reporter{r: r, srv: srv, ctx: t.Context(), job: Job{serverID: testServer, ID: 31, Attempt: 1}}
		srv.refuseNext.Store(true)
		done := make(chan error, 1)
		go func() { done <- rep.Refresh(t.Context()) }()
		synctest.Wait()
		rep.sendMu.Lock()
		rep.settled = true
		rep.sendMu.Unlock()
		if err := <-done; !errors.Is(err, ErrStopped) {
			t.Fatalf("Refresh after settle = %v, want ErrStopped", err)
		}
		if n := len(srv.updates[31]); n != 0 {
			t.Fatalf("got %d updates after settle, want none", n)
		}
	})
}

type limitedRefreshServer struct {
	*fakeServer
	refuseNext atomic.Bool
}

func (s *limitedRefreshServer) UpdateJob(ctx context.Context, id int64, u link.Update) (*link.Job, error) {
	if s.refuseNext.Swap(false) {
		return nil, &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Minute, RetryAfterSet: true}
	}
	return s.fakeServer.UpdateJob(ctx, id, u)
}

func TestRateLimitedRefreshRetriesWithCurrentProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := &limitedRefreshServer{fakeServer: newFakeServer()}
		r := New(nil, func() Server { return srv }, nil, testOpts())
		rep := &Reporter{r: r, srv: srv, ctx: t.Context(), job: Job{serverID: testServer, ID: 31, Attempt: 1}}
		srv.refuseNext.Store(true)
		rep.Progress(Progress{Step: "reading"})
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- rep.Refresh(t.Context()) }()
		synctest.Wait()
		rep.Progress(Progress{Step: "saving"})
		if err := rep.Refresh(t.Context()); err != nil || time.Since(start) != 0 {
			t.Fatalf("new progress sent after %v: %v", time.Since(start), err)
		}
		if err := <-done; err != nil || time.Since(start) != time.Minute {
			t.Fatalf("refresh retried after %v: %v", time.Since(start), err)
		}
		updates := srv.updates[31]
		if len(updates) != 2 {
			t.Fatalf("got %d accepted updates, want 2", len(updates))
		}
		for _, u := range updates {
			var pr Progress
			if err := json.Unmarshal(u.Progress, &pr); err != nil || pr.Step != "saving" {
				t.Fatalf("stale refresh: %s, %v", u.Progress, err)
			}
		}
	})
}

// A cancel asked for after the job's last report, which a failure beat to the
// next keepalive (the stop itself may be what failed it), ends the job
// cancelled, on the server and on the Bridge's page; a cancel the server had
// already ended the job with (its 409 cancelled) too. A failure with no cancel
// held, and a job that got its work done, keep their end.
func TestCancelRacingTheJobsEnd(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		cancel, gone bool
		err          error
		want         string
	}{
		"failure racing a cancel":          {cancel: true, err: errors.New("the download was interrupted"), want: link.StateCancelled},
		"failure racing a server's cancel": {gone: true, err: errors.New("the download was interrupted"), want: link.StateCancelled},
		"failure before any cancel":        {err: errors.New("the download was interrupted"), want: link.StateFailed},
		"done before the cancel":           {cancel: true, want: link.StateDone},
		"done before a server's cancel":    {gone: true, want: link.StateDone},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, srv := openStore(t), newFakeServer()
			srv.add(&link.Job{ID: 21, Kind: "k.race", Attempts: 1})
			opts := testOpts()
			// No keepalive goes before the job ends: only the end hears the cancel.
			opts.Keepalive, opts.ReportRetry = time.Hour, time.Hour
			r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.race", lane: LaneLight,
				fn: func(context.Context, Job, *Reporter) (any, error) {
					srv.mu.Lock()
					srv.cancel[21], srv.gone[21] = c.cancel, c.gone
					srv.reason[21] = "the Audible account was removed"
					srv.mu.Unlock()
					return map[string]int{"files": 1}, c.err
				}})
			runFor(t, r, func() bool { return storedAs(db, 21, c.want) })
			state, final := srv.finalState(21)
			if state != c.want {
				t.Fatalf("reported %q, want %q", state, c.want)
			}
			wantErr := ""
			if c.want == link.StateFailed {
				wantErr = c.err.Error()
			}
			// A cancel sends no error of its own: the server keeps its reason.
			if final.Error == nil || *final.Error != wantErr {
				t.Fatalf("final report's error = %v, want %q", final.Error, wantErr)
			}
			if c.want == link.StateCancelled && string(final.Result) != `{"files":1}` {
				t.Fatalf("a cancelled job's result = %s", final.Result)
			}
		})
	}
}

// A job that parks after a cancel was asked for (a park never asks about one)
// is stored cancelled by the server, which says so in its answer (the job as it
// stored it) or its 409 cancelled (it had ended the job): the Bridge's page
// records it cancelled too, not parked waiting for someone. A park with no
// cancel stays parked, as does one the server moved on from for another
// reason (a plain 409 not_held).
func TestParkTheServerCancelled(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		cancel, gone, notHeld bool
		want                  string
	}{
		"park after a cancel":           {cancel: true, want: store.JobCancelled},
		"park after a server's cancel":  {gone: true, want: store.JobCancelled},
		"park before any cancel":        {want: store.JobNeedsAttention},
		"park the server moved on from": {notHeld: true, want: store.JobNeedsAttention},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, srv := openStore(t), newFakeServer()
			srv.add(&link.Job{ID: 25, Kind: "k.park", Attempts: 1})
			opts := testOpts()
			// No keepalive goes before the job ends: only the end hears the cancel.
			opts.Keepalive, opts.ReportRetry = time.Hour, time.Hour
			r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.park", lane: LaneLight,
				fn: func(context.Context, Job, *Reporter) (any, error) {
					srv.mu.Lock()
					srv.cancel[25], srv.gone[25], srv.notHeld[25] = c.cancel, c.gone, c.notHeld
					srv.mu.Unlock()
					return nil, &ParkError{Code: "budget_exceeded", Message: "over the budget"}
				}})
			runFor(t, r, func() bool { return storedAs(db, 25, c.want) })
			// The park went out as it was: the server's answer alone made it a cancel.
			if state, _ := srv.finalState(25); state != link.StateNeedsAttention {
				t.Fatalf("reported %q, want the park", state)
			}
			sj, err := db.GetServerJob(context.Background(), 25)
			wantErr := "budget_exceeded: over the budget"
			if c.want == store.JobCancelled {
				wantErr = ""
			}
			if err != nil || sj.Error != wantErr {
				t.Fatalf("stored = %+v, %v; want error %q", sj, err, wantErr)
			}
		})
	}
}

// heldCancel's cause says by whom as a keepalive's answer would; no cancel, a
// lost claim or no answer is none (the failure stands).
func TestHeldCancel(t *testing.T) {
	t.Parallel()
	srv := newFakeServer()
	rep := &Reporter{srv: srv, job: Job{ID: 22, Attempt: 1}}
	ctx := context.Background()
	if cause := rep.heldCancel(ctx); cause != nil {
		t.Fatalf("no cancel held = %v", cause)
	}
	srv.cancel[22], srv.reason[22] = true, "the Audible account was removed"
	if cause := rep.heldCancel(ctx); cause == nil ||
		cause.Error() != "the job was cancelled by the server: the Audible account was removed" {
		t.Fatalf("the server's cancel = %v", cause)
	}
	srv.reason[22] = ""
	if cause := rep.heldCancel(ctx); cause == nil || cause.Error() != "the job was cancelled by an admin" {
		t.Fatalf("an admin's cancel = %v", cause)
	}
	srv.gone[22] = true
	if cause := rep.heldCancel(ctx); !errors.Is(cause, errCancelRequested) {
		t.Fatalf("a job the server had cancelled = %v", cause)
	}
	srv.gone[22], srv.notHeld[22] = false, true
	if cause := rep.heldCancel(ctx); cause != nil {
		t.Fatalf("a lost claim = %v", cause)
	}
	srv.notHeld[22], srv.failing = false, true
	if cause := rep.heldCancel(ctx); cause != nil {
		t.Fatalf("no answer = %v", cause)
	}
}

// A server that doesn't answer holds a failed job's end for heldCancelWait at
// most: no answer is no cancel.
func TestHeldCancelWaitsBriefly(t *testing.T) {
	t.Parallel()
	srv := newFakeServer()
	srv.hang = make(chan struct{})
	defer close(srv.hang)
	srv.cancel[24] = true // what the answer would say, had it come
	rep := &Reporter{srv: srv, job: Job{ID: 24, Attempt: 1}}
	start := time.Now()
	if cause := rep.heldCancel(context.Background()); cause != nil {
		t.Fatalf("no answer = %v", cause)
	}
	if took := time.Since(start); took > heldCancelWait+2*time.Second {
		t.Fatalf("waited %v for an answer, want at most %v", took, heldCancelWait)
	}
}

// A failure whose report didn't get through, which the server cancelled
// meanwhile, is recorded cancelled: the Bridge's page reads what the server's
// does. The server says so in its answer to the retried report: the job as it
// stored it, cancelled (a cancel it held, which the cancel check got no answer
// about), or its 409 cancelled (it had ended the job).
func TestRetriedFailureTheServerCancelled(t *testing.T) {
	t.Parallel()
	for name, gone := range map[string]bool{"a cancel held": false, "ended cancelled": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db, srv := openStore(t), newFakeServer()
			srv.add(&link.Job{ID: 23, Kind: "k.retry", Attempts: 1})
			opts := testOpts()
			opts.Keepalive, opts.ReportRetry = time.Hour, time.Hour
			r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.retry", lane: LaneLight,
				fn: func(context.Context, Job, *Reporter) (any, error) {
					srv.setFailing(true)
					return nil, errors.New("the download was interrupted")
				}})
			var flipped bool
			runFor(t, r, func() bool {
				if !flipped {
					sj, err := db.GetServerJob(context.Background(), 23)
					if err != nil || sj.State != store.JobFailed || srv.triesOf(23) < 3 { // first report, probe, failed end
						return false
					}
					srv.mu.Lock()
					srv.cancel[23], srv.gone[23], srv.failing = !gone, gone, false
					srv.mu.Unlock()
					flipped = true
				}
				return storedAs(db, 23, store.JobCancelled)
			})
			// The retried report was the failure: the answer alone made it a cancel.
			if state, _ := srv.finalState(23); state != link.StateFailed {
				t.Fatalf("reported %q, want the failure sent again", state)
			}
			if sj, err := db.GetServerJob(context.Background(), 23); err != nil || sj.Error != "" {
				t.Fatalf("stored = %+v, %v; want no error on a cancelled job", sj, err)
			}
		})
	}
}

// cancelFailStore fails the first FinishServerJob that records a cancel.
type cancelFailStore struct {
	*store.DB
	failed atomic.Int32
}

func (s *cancelFailStore) FinishServerJob(ctx context.Context, id int64, attempt int, state string, result json.RawMessage, errText string) error {
	if state == store.JobCancelled && s.failed.CompareAndSwap(0, 1) {
		return errors.New("disk I/O error")
	}
	return s.DB.FinishServerJob(ctx, id, attempt, state, result, errText)
}

// A failure the server stored cancelled, which the Bridge's store then couldn't
// record as cancelled, stays an unreported failure: its report goes again until
// the Bridge's page reads cancelled too, never reported and left failed.
func TestCancelledFailureTheStoreMissed(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	st := &cancelFailStore{DB: db}
	srv.add(&link.Job{ID: 26, Kind: "k.store", Attempts: 1})
	opts := testOpts()
	opts.Keepalive, opts.ReportRetry = time.Hour, time.Hour
	r := New(st, func() Server { return srv }, nil, opts, handler{kind: "k.store", lane: LaneLight,
		fn: func(context.Context, Job, *Reporter) (any, error) {
			srv.setFailing(true)
			return nil, errors.New("the download was interrupted")
		}})
	var flipped bool
	runFor(t, r, func() bool {
		if !flipped {
			sj, err := db.GetServerJob(context.Background(), 26)
			if err != nil || sj.State != store.JobFailed || srv.triesOf(26) < 3 { // first report, probe, failed end
				return false
			}
			srv.mu.Lock()
			srv.cancel[26], srv.failing = true, false
			srv.mu.Unlock()
			flipped = true
		}
		return storedAs(db, 26, store.JobCancelled)
	})
	if st.failed.Load() != 1 {
		t.Fatal("the store never failed to record the cancel")
	}
	// The report whose answer the store missed, then the one that recorded it.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	var failures int
	for _, u := range srv.updates[26] {
		if u.State != nil && *u.State == link.StateFailed {
			failures++
		}
	}
	if failures < 2 {
		t.Fatalf("the failure reached the server %d times, want it sent again", failures)
	}
}

// A Bridge that stops while a failed job asks for a held cancel leaves the job
// running, to resume at the next start, as one stopped before its end does:
// no end is recorded or reported.
func TestStopDuringHeldCancel(t *testing.T) {
	t.Parallel()
	db, srv := openStore(t), newFakeServer()
	srv.add(&link.Job{ID: 25, Kind: "k.stop", Attempts: 1})
	logs := &logCounter{n: map[string]int{}}
	opts := testOpts()
	opts.Keepalive, opts.ReportRetry, opts.Log = time.Hour, time.Hour, slog.New(logs)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	r := New(db, func() Server { return srv }, nil, opts, handler{kind: "k.stop", lane: LaneLight,
		fn: func(context.Context, Job, *Reporter) (any, error) {
			srv.mu.Lock()
			srv.hang = hang
			srv.mu.Unlock()
			return nil, errors.New("the download was interrupted")
		}})
	runFor(t, r, func() bool {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.hung > 0 // the cancel check waits on the server
	})
	sj, err := db.GetServerJob(context.Background(), 25)
	if err != nil || sj.State != store.JobRunning {
		t.Fatalf("job = %+v, %v; want it left running", sj, err)
	}
	if n := logs.count("INFO bridge: a job was interrupted; it resumes at the next start"); n != 1 {
		t.Fatalf("interrupted logged %d times", n)
	}
	if n := logs.count("WARN bridge: recording a job failed"); n != 0 {
		t.Fatalf("an end was recorded %d times", n)
	}
	if state, _ := srv.finalState(25); state != "" {
		t.Fatalf("reported %q", state)
	}
}
