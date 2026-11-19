package generate

import (
	"cmp"
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

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// fakeSharer is the engine's share step.
type fakeSharer struct {
	mu    sync.Mutex
	err   error
	calls []string
	// filing, when set, runs as a share files, after its checks.
	filing func()
}

func (f *fakeSharer) ShareChecked(_ context.Context, bookID int64, workID string, dims []string,
	stopped func() (bool, error)) (pipeline.ShareResult, error) {
	if stopped != nil {
		// Asked before each dimension, as the engine's share asks.
		for range max(len(dims), 2) {
			if stop, err := stopped(); err != nil || stop {
				return pipeline.ShareResult{}, cmp.Or(err, pipeline.ErrShareStopped)
			}
		}
	}
	if f.filing != nil {
		f.filing()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, workID+"|"+strings.Join(dims, ","))
	if f.err != nil {
		return pipeline.ShareResult{}, f.err
	}
	_ = bookID
	return pipeline.ShareResult{WorkID: "the-book", Contributed: map[string]string{"characters": "https://github.com/x/y/issues/1"},
		Covered: []string{"recaps"}}, nil
}

// count is how many shares were filed.
func (f *fakeSharer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// shareRunner runs the generate and contribute jobs with s as the share step.
func (hr *harness) shareRunner(s Sharer, cred func(context.Context) error) (stop func()) {
	return hr.shareRunnerRecheck(s, cred, time.Hour)
}

// shareRunnerRecheck is shareRunner with a park's recheck every recheck.
func (hr *harness) shareRunnerRecheck(s Sharer, cred func(context.Context) error, recheck time.Duration) (stop func()) {
	return hr.shareRunnerOn(s, cred, recheck, func() jobs.Server { return hr.cl })
}

// shareRunnerOn is shareRunnerRecheck with the runner's link to the server
// from server (the Bridge paired anew when it changes).
func (hr *harness) shareRunnerOn(s Sharer, cred func(context.Context) error, recheck time.Duration,
	server func() jobs.Server) (stop func()) {
	return hr.shareRunnerWith(s, cred, recheck, server, hr.sched)
}

// shareRunnerWith is shareRunnerOn over eng rather than the harness's engine.
func (hr *harness) shareRunnerWith(s Sharer, cred func(context.Context) error, recheck time.Duration,
	server func() jobs.Server, eng Engine) (stop func()) {
	h := New(Options{DB: hr.db, Engine: func() Engine { return eng }, Sharer: func() Sharer { return s },
		Libraries: hr.libs, Mount: func(int64, string) (string, bool) { return hr.libs.root, false },
		Server: func() Server { return hr.cl }, WorkRoot: hr.work, Version: "0.2.0", RetentionDays: 30, ContribCredential: cred,
		Poll: 10 * time.Millisecond, IdlePoll: 10 * time.Millisecond, Recheck: recheck, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r := jobs.New(hr.db, server, nil, jobs.Options{Keepalive: 30 * time.Millisecond,
		ProgressEvery: 5 * time.Millisecond, Wait: 10 * time.Millisecond, RetryEvery: 20 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, h, h.ContributeJob())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	return func() { cancel(); <-done }
}

// addContribute queues a sidecars.contribute job.
func (f *fakeServer) addContribute(id int64, path, params string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lib := int64(1)
	f.queue = append(f.queue, link.Job{ID: id, Kind: ContributeKind, LibraryID: &lib, Path: &path, Params: json.RawMessage(params),
		Attempts: 1, State: "claimed"})
}

func TestContributeJob(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(71)
	s := &fakeSharer{}
	hr.srv.addContribute(72, "Author/Book", `{"work_id":"the-book","dimensions":["characters","recaps"],"later":1}`)
	stop := hr.shareRunner(s, func(context.Context) error { return nil })
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(72); return s == link.StateDone })
	var res ContributeResult
	hr.result(72, &res)
	if res.LibraryID != 1 || res.Path != "Author/Book" || res.WorkID != "the-book" ||
		res.Contributed["characters"] == "" || !slices.Equal(res.Covered, []string{"recaps"}) {
		t.Fatalf("result = %+v", res)
	}
	if s.calls[0] != "the-book|characters,recaps" {
		t.Fatalf("share calls = %v", s.calls)
	}
}

// A share asks the server just before it files: a stop the server holds for
// the job (its Contributions module turned off since the last report, heard
// only in an answer) files nothing, and the job ends cancelled (denied).
// TestContributeJob is the allowed half: a share still on files.
func TestContributeJobAsksBeforeFiling(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(91)
	s := &fakeSharer{}
	hr.srv.set(func() {
		hr.srv.atStep[92] = func(f *fakeServer, step string) {
			if step == stepSharing {
				f.cancel[92] = true
			}
		}
	})
	hr.srv.addContribute(92, "Author/Book", `{}`)
	stop := hr.shareRunner(s, func(context.Context) error { return nil })
	defer stop()
	hr.waitFor("cancelled", func() bool { st, _ := hr.srv.final(92); return st == link.StateCancelled })
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 0 {
		t.Fatalf("share calls = %v, want none after the server stopped the job", s.calls)
	}
}

// An answer just before filing that carries share_off without stopping the
// job (as a sidecars.generate run's answers do, shareOwed) files nothing
// either (denied).
func TestShareConfirmedFilesNothingOnShareOff(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(95)
	s := &fakeSharer{}
	hr.srv.set(func() {
		hr.srv.atStep[96] = func(f *fakeServer, step string) {
			if step == stepSharing {
				f.shareOff[96] = true
			}
		}
	})
	hr.srv.addContribute(96, "Author/Book", `{}`)
	stop := hr.shareRunner(s, func(context.Context) error { return nil })
	defer stop()
	hr.waitFor("failed", func() bool { st, _ := hr.srv.final(96); return st == link.StateFailed })
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 0 {
		t.Fatalf("share calls = %v, want none after a share_off answer", s.calls)
	}
}

// A share whose server can't be asked just before filing files nothing: the
// job parks share_unconfirmed and asks again, filing once the server answers
// (denied while it can't, allowed after).
func TestContributeJobWaitsWhenTheServerCantBeAsked(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(93)
	s := &fakeSharer{}
	hr.srv.set(func() {
		hr.srv.atStep[94] = func(f *fakeServer, step string) {
			if step == stepSharing && f.downHits[94] == 0 {
				f.down[94] = true
			}
		}
	})
	hr.srv.addContribute(94, "Author/Book", `{}`)
	stop := hr.shareRunnerRecheck(s, func(context.Context) error { return nil }, 20*time.Millisecond)
	defer stop()
	hr.waitFor("asks that fail", func() bool {
		hr.srv.mu.Lock()
		defer hr.srv.mu.Unlock()
		return hr.srv.downHits[94] >= 3
	})
	if calls := s.count(); calls != 0 {
		t.Fatalf("share calls = %d while the server can't be asked, want none", calls)
	}
	hr.srv.set(func() { hr.srv.down[94] = false })
	hr.waitFor("done", func() bool { st, _ := hr.srv.final(94); return st == link.StateDone })
	if !hr.srv.saw(94, func(u link.Update, _ map[string]any) bool {
		return u.State != nil && *u.State == link.StateNeedsAttention
	}) {
		t.Fatal("the job never reported its share_unconfirmed park")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 1 {
		t.Fatalf("share calls = %v, want one once the server answered", s.calls)
	}
}

// The server is asked whether the share still stands before the engine's hold
// on the book is taken: an answer slow to come keeps no other job that wants
// the book from it (allowed), and the share files once the answer comes.
func TestContributeJobAsksBeforeTheHold(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(101)
	bookID := hr.engineBook(101).ID
	s := &fakeSharer{}
	entered, answer := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hr.srv.set(func() {
		hr.srv.atStep[102] = func(_ *fakeServer, step string) {
			if step == stepSharing {
				// The server is slow to answer the job's reports from here.
				once.Do(func() { close(entered) })
				<-answer
			}
		}
	})
	hr.srv.addContribute(102, "Author/Book", `{}`)
	stop := hr.shareRunner(s, func(context.Context) error { return nil })
	defer stop()
	<-entered
	// The job's ask waits behind the slow report (or is it): the book is free.
	time.Sleep(50 * time.Millisecond)
	release, ok := hr.sched.Hold(bookID)
	if ok {
		release()
	}
	close(answer)
	if !ok {
		t.Fatal("the book was held while the job waited for the server's answer")
	}
	hr.waitFor("done", func() bool { st, _ := hr.srv.final(102); return st == link.StateDone })
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) != 1 {
		t.Fatalf("share calls = %v, want one", s.calls)
	}
}

// A share that had to wait for the book's hold asks the server again before it
// files, as its first answer is as old as the wait: a share_off set while it
// waited files nothing (denied). TestContributeJobAsksBeforeTheHold is the
// allowed half.
func TestContributeJobAsksAgainAfterWaitingForTheHold(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(131)
	bookID := hr.engineBook(131).ID
	release, ok := hr.sched.Hold(bookID)
	if !ok {
		t.Fatal("the book couldn't be held")
	}
	// The job asks first, then finds the book held: at that refusal, while
	// the job waits for the book, the share is turned off and the book let go.
	// Done in the refusal itself, not after a sleep, so the job's wait (a few
	// polls) can't run out before the book is free.
	eng := &refusedHold{Engine: hr.sched, onRefused: func() {
		hr.srv.set(func() { hr.srv.shareOff[132] = true })
		release()
	}}
	s := &fakeSharer{}
	hr.srv.addContribute(132, "Author/Book", `{}`)
	stop := hr.shareRunnerWith(s, func(context.Context) error { return nil }, time.Hour, func() jobs.Server { return hr.cl }, eng)
	defer stop()
	hr.waitFor("failed", func() bool { st, _ := hr.srv.final(132); return st == link.StateFailed })
	if calls := s.count(); calls != 0 {
		t.Fatalf("share calls = %d after a share_off set while the job waited for the book, want none", calls)
	}
	if _, u := hr.srv.final(132); u.Error == nil || !strings.Contains(*u.Error, errShareOffNow.Error()) {
		t.Fatalf("failed with %v, want the share_off heard by the second ask", u.Error)
	}
}

// refusedHold is an engine whose first refused Hold runs onRefused, on the
// job's goroutine, before the job hears the refusal.
type refusedHold struct {
	Engine
	once      sync.Once
	onRefused func()
}

func (e *refusedHold) Hold(id int64) (func(), bool) {
	release, ok := e.Engine.Hold(id)
	if !ok {
		e.once.Do(e.onRefused)
	}
	return release, ok
}

// A share parked share_unconfirmed gives its light-lane slot back: two such
// parks leave the lane (two slots) free for another light job (allowed), and
// neither files while it can't ask (denied).
func TestContributeParksGiveTheLightLaneBack(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(111)
	s := &fakeSharer{}
	hr.srv.set(func() {
		for _, id := range []int64{112, 113} {
			hr.srv.atStep[id] = func(f *fakeServer, step string) {
				if step == stepSharing {
					f.down[id] = true
				}
			}
		}
	})
	hr.srv.addContribute(112, "Author/Book", `{}`)
	hr.srv.addContribute(113, "Author/Book", `{}`)
	stop := hr.shareRunnerRecheck(s, func(context.Context) error { return nil }, 20*time.Millisecond)
	defer stop()
	hr.waitFor("both parked", func() bool {
		hr.srv.mu.Lock()
		defer hr.srv.mu.Unlock()
		return hr.srv.downHits[112] >= 2 && hr.srv.downHits[113] >= 2
	})
	hr.srv.addContribute(114, "Author/Book", `{}`)
	hr.waitFor("the third share done", func() bool { st, _ := hr.srv.final(114); return st == link.StateDone })
	if calls := s.count(); calls != 1 {
		t.Fatalf("share calls = %d, want only the third job's", calls)
	}
	// Back once the server answers: each takes a slot again and files.
	hr.srv.set(func() {
		for _, id := range []int64{112, 113} {
			delete(hr.srv.atStep, id)
			hr.srv.down[id] = false
		}
	})
	for _, id := range []int64{112, 113} {
		hr.waitFor("a parked share done", func() bool { st, _ := hr.srv.final(id); return st == link.StateDone })
	}
}

// A pairing that changes as the job reports its sharing step (before its ask,
// or while the answer is on its way, whichever report comes first), by the one
// rule, the server's id (BR-LINK-RULE): paired again with the job's own server
// at a new address, the job goes on over the new link and files once
// (allowed); paired with another server, the job is dropped (lost here,
// reported nowhere) and files nothing, rather than parking again at every
// recheck on the old link (denied). jobs' TestAskShareOffOnlyOnTheJobsServer
// pins the pairing that changes while the answer is on its way.
func TestContributeJobConfirmsOnlyOnTheJobsServer(t *testing.T) {
	t.Parallel()
	for name, sameServer := range map[string]bool{"same server, new address": true, "another server": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hr := newHarness(t)
			hr.generated(121)
			addr, id := "http://127.0.0.1:1", "srv-2"
			if sameServer {
				addr, id = strings.Replace(hr.cl.BaseURL(), "127.0.0.1", "localhost", 1), "srv"
			}
			next, err := link.New(addr, "tok", "0.2.0")
			if err != nil {
				t.Fatal(err)
			}
			next = next.WithServerID(id)
			var paired atomic.Bool
			hr.srv.set(func() {
				hr.srv.atStep[122] = func(_ *fakeServer, step string) {
					if step == stepSharing {
						paired.Store(true) // paired anew from this report on
					}
				}
			})
			s := &fakeSharer{}
			hr.srv.addContribute(122, "Author/Book", `{}`)
			stop := hr.shareRunnerOn(s, func(context.Context) error { return nil }, 20*time.Millisecond, func() jobs.Server {
				if paired.Load() {
					return next
				}
				return hr.cl
			})
			defer stop()
			if sameServer {
				hr.waitFor("done", func() bool { st, _ := hr.srv.final(122); return st == link.StateDone })
				if calls := s.count(); calls != 1 {
					t.Fatalf("share calls = %d, want one", calls)
				}
				return
			}
			hr.waitFor("dropped", func() bool {
				sj, err := hr.db.GetServerJob(context.Background(), 122)
				return err == nil && sj.State == store.JobLost
			})
			time.Sleep(100 * time.Millisecond) // a few rechecks' worth
			if calls := s.count(); calls != 0 {
				t.Fatalf("share calls = %d for another server's job, want none", calls)
			}
			if st, _ := hr.srv.final(122); st != "" {
				t.Fatalf("the dropped job reported its end (%s)", st)
			}
		})
	}
}

// A sidecars.contribute share whose work lookup the community database didn't
// answer (the share step's share_unconfirmed park, pipeline.ShareLookupMsg)
// parks and asks again (denied: before BR-LINK-RULE it failed the job), staying
// parked through each ask (denied: reported running for it, and parked again,
// once a recheck), and files once the database answers (allowed).
func TestContributeJobWaitsOnAnUnansweredLookup(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(141)
	s := &fakeSharer{err: pipeline.ShareUnconfirmedPark(pipeline.ShareLookupMsg, errors.New("work lookup failed")),
		filing: func() { time.Sleep(30 * time.Millisecond) }} // long enough for a report to go out
	hr.srv.addContribute(142, "Author/Book", `{}`)
	stop := hr.shareRunnerRecheck(s, func(context.Context) error { return nil }, 20*time.Millisecond)
	defer stop()
	hr.waitFor("the park", func() bool {
		return hr.srv.saw(142, func(u link.Update, _ map[string]any) bool {
			return u.State != nil && *u.State == link.StateNeedsAttention && u.Error != nil &&
				*u.Error == pipeline.ShareLookupMsg
		})
	})
	hr.waitFor("two rechecks", func() bool { return s.count() >= 3 })
	if st, _ := hr.srv.final(142); st != "" {
		t.Fatalf("the job ended %s while the lookup had no answer", st)
	}
	parkedYet, flapped := false, false
	hr.srv.saw(142, func(u link.Update, _ map[string]any) bool {
		if u.State != nil && *u.State == link.StateNeedsAttention {
			parkedYet = true
		} else if parkedYet && u.State != nil && *u.State == link.StateRunning {
			flapped = true
		}
		return false
	})
	if flapped {
		t.Fatal("the parked job reported running again while the lookup had no answer")
	}
	s.mu.Lock()
	s.err = nil // the database answers again
	s.mu.Unlock()
	hr.waitFor("done", func() bool { st, _ := hr.srv.final(142); return st == link.StateDone })
}

func TestContributeJobRefusals(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(81)
	cases := []struct {
		id     int64
		path   string
		params string
		err    error
		cred   error // what ContribCredential answers
		want   describeFailure
	}{
		{82, "Loose.m4b", `{}`, nil, nil, errShareNothing},
		{83, "Author/Book", `{}`, nil, contrib.ErrNoCredential, errShareCantContribute},
		{84, "Author/Book", `{}`, pipeline.ErrNotMatched, nil, errShareNotMatched},
		{85, "Author/Book", `{}`, scheduler.ParkWithCode(state.ParkContribUnavailable, pipeline.ContribUnavailableMsg), nil, errShareCantContribute},
		{86, "Author/Book", `{}`, pipeline.ErrNoSidecars, nil, errShareNothing},
		{87, "Author/Book", `{}`, fmt.Errorf("%w (the characters issue): x", pipeline.ErrFilingUnknown), nil, errShareFilingUnknown},
		{88, "Author/Book", `{}`, nil, contrib.ErrSignInUnrefreshed, errShareUnrefreshed},
		{89, "Author/Book", `{}`, &scheduler.ParkError{Code: state.ParkContribUnavailable, Reason: pipeline.ContribUnrefreshedMsg,
			Cause: contrib.ErrSignInUnrefreshed}, nil, errShareUnrefreshed},
	}
	for _, c := range cases {
		s := &fakeSharer{err: c.err}
		cred := c.cred
		hr.srv.addContribute(c.id, c.path, c.params)
		stop := hr.shareRunner(s, func(context.Context) error { return cred })
		hr.waitFor("the refusal", func() bool { st, _ := hr.srv.final(c.id); return st == link.StateFailed })
		stop()
		if _, u := hr.srv.final(c.id); u.Error == nil || *u.Error != string(c.want) {
			t.Fatalf("job %d failed with %v, want %q", c.id, u.Error, c.want)
		}
	}
}

func TestParseContributeParams(t *testing.T) {
	for raw, ok := range map[string]bool{
		``: true, `null`: true, `{}`: true, `{"dimensions":["recaps"]}`: true, `{"work_id":"w","new_field":1}`: true,
		`{"dimensions":["notes"]}`: false, `{"dimensions":["recaps","recaps"]}`: false, `[]`: false,
		`{"work_id":"` + strings.Repeat("w", 201) + `"}`: false,
	} {
		if _, err := ParseContributeParams(json.RawMessage(raw)); (err == nil) != ok {
			t.Errorf("%s: err = %v", raw, err)
		}
	}
}

// repHandler hands its run's Reporter out and holds the job until it ends.
type repHandler struct{ reps chan *jobs.Reporter }

func (repHandler) Kind() string { return "test.share" }
func (repHandler) Lane() string { return jobs.LaneLight }
func (h repHandler) Run(ctx context.Context, _ jobs.Job, rep *jobs.Reporter) (any, error) {
	h.reps <- rep
	<-ctx.Done()
	return nil, ctx.Err()
}

// An owed share's first check before it files asks the server too, as the
// share step looks the work up and resolves the GitHub sign-in between
// shareConfirmed's ask and its first filing: a share_off the server holds for
// the job, not yet heard, stops it (denied); a look at what was heard alone
// would go on.
func TestStoppedByAsksBeforeTheFirstDimension(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	reps := make(chan *jobs.Reporter, 1)
	// No report but the job's first and the asks.
	runner := jobs.New(hr.db, func() jobs.Server { return hr.cl }, nil, jobs.Options{Keepalive: time.Hour,
		ProgressEvery: time.Hour, Wait: 10 * time.Millisecond, RetryEvery: 20 * time.Millisecond,
		Log: slog.New(slog.DiscardHandler)}, repHandler{reps})
	hr.srv.set(func() {
		hr.srv.queue = append(hr.srv.queue, link.Job{ID: 141, Kind: "test.share", Attempts: 1, State: "claimed"})
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runner.Run(ctx) }()
	defer func() { cancel(); <-done }()
	rep := <-reps // its first report answered
	hr.srv.set(func() { hr.srv.shareOff[141] = true })
	stopped := stoppedBy(ctx, &run{rep: rep}, func(context.Context) (bool, error) { return true, nil })
	if stop, err := stopped(); !stop || err != nil {
		t.Fatalf("the first check: %v %v, want the share stopped by the server's share_off", stop, err)
	}
}
