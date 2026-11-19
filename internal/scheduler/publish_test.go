package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/events"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// frame is the fields of a book.state event.
type frame struct {
	State    string `json:"state"`
	Status   string `json:"status"`
	Error    string `json:"error"`
	ParkCode string `json:"park_code"`
	RetryAt  string `json:"retry_at"`
}

// lastFrame is the last book.state frame hub holds for book id (ok false when
// none).
func lastFrame(t *testing.T, hub *events.Hub, id int64) (f frame, ok bool) {
	t.Helper()
	replay, sub := hub.Subscribe(0)
	sub.Close()
	for _, ev := range replay {
		if ev.Type != "book.state" || ev.BookID != id {
			continue
		}
		f = frame{}
		if err := json.Unmarshal(ev.Data, &f); err != nil {
			t.Fatal(err)
		}
		ok = true
	}
	return f, ok
}

// storedFrame is book id as stored, in a frame's terms.
func storedFrame(t *testing.T, db *store.DB, id int64) frame {
	t.Helper()
	b, err := db.GetBook(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return frame{State: b.State, Status: b.Status, Error: b.Error, ParkCode: b.ParkCode, RetryAt: b.RetryAt}
}

// A Pause landing as a stage finishes (after its sentinel, before the advance)
// is the book's last frame: the advance publishes the book as stored, never the
// StatusNone it was dispatched under, so the last event never shows a paused
// book running.
func TestAdvanceNeverPublishesOverAPauseThatLandedFirst(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "pausewin", "", "")
	var sched *Scheduler
	var once sync.Once
	exec := &hookExecutor{after: func(bk store.Book, stage state.State) {
		if stage == state.Inspecting {
			once.Do(func() {
				if err := sched.Pause(context.Background(), bk.ID); err != nil {
					t.Errorf("pause: %v", err)
				}
			})
		}
	}}
	sched = New(db, h.hub, exec, 1, h.workRoot, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = sched.Start(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	sched.Notify()

	deadline := time.Now().Add(waitLimit)
	for {
		if f, ok := lastFrame(t, h.hub, b.ID); ok && f.State == string(state.Splitting) {
			if f.Status != string(state.StatusPaused) {
				t.Fatalf("the advance published %+v over the pause, want it paused", f)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no frame at splitting; last %+v", storedFrame(t, db, b.ID))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A dispatch pass that read a book at a waypoint before a Pause landed moves it
// on and publishes it paused, as stored.
func TestWaypointAdvanceNeverPublishesOverAPauseThatLandedFirst(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	read := h.addBook(t, db, "waypoint", "", "")
	if !state.IsWaypoint(state.State(read.State)) {
		t.Fatalf("a new book starts at %s, want a waypoint", read.State)
	}
	if err := s.Pause(ctx, read.ID); err != nil {
		t.Fatal(err)
	}
	next, ok := s.advanceWaypoint(ctx, read)
	if !ok {
		t.Fatal("the waypoint didn't advance")
	}
	f, _ := lastFrame(t, h.hub, read.ID)
	if f.State != next || f.Status != string(state.StatusPaused) {
		t.Fatalf("last frame %+v, want %s paused", f, next)
	}
}

// Controls racing on one book (Pause, Resume, Cancel, Retry, the pass's waypoint
// advance and a stage's park) leave the book's last frame equal to the book as
// stored: no change's frame lands after a later change's. The store serialises
// the writes, so the windows are narrow and the deterministic tests above pin
// the ordering; this one runs the publish locks under contention for -race.
func TestConcurrentChangesLeaveTheLastFrameAsStored(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	hub := events.NewHub(16)
	s := New(db, hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	for round := range 8 {
		b := h.addBook(t, db, "race"+string(rune('a'+round)), "", "")
		var wg sync.WaitGroup
		for w := range 4 {
			wg.Go(func() {
				for i := range 25 {
					switch (w + i) % 6 {
					case 0:
						_ = s.Pause(ctx, b.ID)
					case 1:
						_ = s.Resume(ctx, b.ID)
					case 2:
						s.setStatus(b.ID, state.StatusNeedsAttention, "parked", state.ParkAgentUnavailable, time.Now().Add(time.Hour))
					case 3:
						_ = s.Retry(ctx, b.ID)
					case 4:
						if cur, err := db.GetBook(ctx, b.ID); err == nil {
							s.advanceWaypoint(ctx, cur)
						}
					case 5:
						if i > 20 {
							_ = s.Cancel(ctx, b.ID)
						}
					}
				}
			})
		}
		wg.Wait()
		f, ok := lastFrame(t, hub, b.ID)
		if want := storedFrame(t, db, b.ID); !ok || f != want {
			t.Fatalf("round %d: last frame %+v, stored %+v", round, f, want)
		}
	}
}

// One book's publishes never wait on another's, and the lock map keeps no entry
// once a book's publishes are done.
func TestPublishLocksArePerBook(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	a := h.addBook(t, db, "a", "", "")
	b := h.addBook(t, db, "b", "", "")

	unlockA := s.lockPublish(a.ID)
	published := make(chan struct{})
	go func() { s.publishStored(ctx, b.ID); close(published) }()
	select {
	case <-published:
	case <-time.After(waitLimit):
		t.Fatal("book b's publish waited on book a's lock")
	}
	blocked := make(chan struct{})
	go func() { s.publishStored(ctx, a.ID); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("book a's publish didn't wait for its own lock")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	<-blocked
	s.pubMu.Lock()
	n := len(s.pubLocks)
	s.pubMu.Unlock()
	if n != 0 {
		t.Fatalf("%d publish locks left after the publishes", n)
	}
}

// A book deleted before its publish publishes nothing.
func TestPublishStoredSkipsADeletedBook(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	b := h.addBook(t, db, "gone", "", "")
	if err := db.DeleteBook(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	s.PublishStored(ctx, b.ID)
	if f, ok := lastFrame(t, h.hub, b.ID); ok {
		t.Fatalf("a deleted book published %+v", f)
	}
}

// A change written outside the scheduler (generate's continue clearing a run's
// cancel) is written and published as stored through ContinueStored: the book's
// last event is no longer the cancel.
func TestContinueStoredPublishesTheClearedCancel(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/src/chapters", WorkDir: h.workRoot + "/chapters", Title: "chapters", Goal: state.GoalChapters})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookPipelineState(ctx, b.ID, string(state.ChapterDetecting)); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if f, _ := lastFrame(t, h.hub, b.ID); f.Error != cancelledReason {
		t.Fatalf("the cancel published %+v", f)
	}
	if err := s.ContinueStored(ctx, b.ID, func() error {
		return db.ContinueChapters(ctx, b.ID, string(state.ChapterDetecting), string(state.ChapterDetecting),
			store.NewBook{Title: "chapters"}, store.GenerateBook{})
	}); err != nil {
		t.Fatal(err)
	}
	if f, _ := lastFrame(t, h.hub, b.ID); f.Status != "" || f.Error != "" || f.State != string(state.ChapterDetecting) {
		t.Fatalf("after the continue the last frame is %+v, want it running", f)
	}
}

// The supervisor's requeue of a parked book with no worker publishes the cleared
// status, which it used to write unpublished.
func TestSupervisorRequeuePublishesTheClearedStatus(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	b := h.addBook(t, db, "requeue", "", "")
	if err := db.SetBookState(ctx, b.ID, string(state.ASR), string(state.StatusNeedsAttention), "stuck", string(state.ParkAgentUnavailable)); err != nil {
		t.Fatal(err)
	}
	b, _ = db.GetBook(ctx, b.ID)
	if _, err := s.supervisorTerminateRequeue(ctx, b); err != nil {
		t.Fatal(err)
	}
	f, ok := lastFrame(t, h.hub, b.ID)
	if !ok || f.Status != "" || f.State != string(state.ASR) {
		t.Fatalf("last frame %+v (%v), want asr running", f, ok)
	}
}

// Two Pauses racing on one book: the one whose write loses (it read the book
// running, the other paused it first) is a no-op, as Pause promises for a book
// already paused, not store.ErrStatusChanged. A Cancel landing after the read
// instead is kept and reported.
func TestPauseLosingToAPauseIsANoOpAndToACancelAnError(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)

	b := h.addBook(t, db, "paused twice", "", "")
	read, err := db.GetBook(ctx, b.ID) // the losing Pause's read: running
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(ctx, b.ID); err != nil { // the winning Pause
		t.Fatal(err)
	}
	if err := s.pauseAsRead(ctx, read); err != nil {
		t.Fatalf("a Pause losing to a Pause = %v, want nil (the book is paused)", err)
	}
	if got := storedFrame(t, db, b.ID); got.Status != string(state.StatusPaused) {
		t.Fatalf("status %q, want paused", got.Status)
	}

	c := h.addBook(t, db, "cancelled", "", "")
	read, err = db.GetBook(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.pauseAsRead(ctx, read); !errors.Is(err, store.ErrStatusChanged) {
		t.Fatalf("a Pause losing to a Cancel = %v, want ErrStatusChanged", err)
	}
	if got, _ := db.GetBook(ctx, c.ID); !IsCancelled(got) {
		t.Fatalf("the Pause wrote over the cancel: %+v", got)
	}
}

// A Cancel landing between a Resume's read of a paused book and its write is
// kept and reported: the Resume clears only the pause it read, and the book's
// last frame stays the cancel.
func TestResumeLosingToACancelKeepsTheCancel(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	ctx := context.Background()
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)

	b := h.addBook(t, db, "resumed late", "", "")
	if err := s.Pause(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	read, err := db.GetBook(ctx, b.ID) // the Resume's read: paused
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.resumeAsRead(ctx, read); !errors.Is(err, store.ErrStatusChanged) {
		t.Fatalf("a Resume losing to a Cancel = %v, want ErrStatusChanged", err)
	}
	if got, _ := db.GetBook(ctx, b.ID); !IsCancelled(got) {
		t.Fatalf("the Resume cleared the cancel: %+v", got)
	}
	if f, ok := lastFrame(t, h.hub, b.ID); !ok || f.Error != cancelledReason {
		t.Fatalf("last frame %+v (%v), want the cancel", f, ok)
	}
}

// A readmit or supersede_rerun failing after its status clear puts the park back
// (restoreStatus) before a Retry, generate's continue (ContinueStored), a Resume
// or the supervisor's terminate_requeue of the book clears anything: the
// put-back can't tell that clear from its own, so it holds the book's publish
// lock from its clear (supersede_rerun after the worker it cancelled returned,
// never across that wait). A Cancel (or a Pause) and then the clear landing
// while it runs end with the book running, never parked again.
func TestFailedRestoreNeverLandsOverALaterClear(t *testing.T) {
	changes := []struct {
		name string
		run  func(s *Scheduler, ctx context.Context, b store.Book) error
	}{
		{"readmit", func(s *Scheduler, ctx context.Context, b store.Book) error { return s.readmit(ctx, b) }},
		{"supersede_rerun", func(s *Scheduler, ctx context.Context, b store.Book) error {
			_, err := s.supervisorSupersedeRerun(ctx, b, string(state.Fixing))
			return err
		}},
	}
	cancel := func(db *store.DB, id int64) error { return CancelStored(context.Background(), db, id) }
	pause := func(db *store.DB, id int64) error {
		return db.SetBookStatusIf(context.Background(), id, "", "", string(state.StatusPaused), "", "", "")
	}
	clears := []struct {
		name string
		stop func(db *store.DB, id int64) error // lands after the change's clear
		run  func(s *Scheduler, db *store.DB, read store.Book) error
	}{
		{"retry", cancel, func(s *Scheduler, db *store.DB, read store.Book) error {
			cancelled, err := db.GetBook(context.Background(), read.ID) // the Retry's read: cancelled
			if err != nil {
				return err
			}
			return s.RetryAsRead(context.Background(), cancelled)
		}},
		{"continue", cancel, func(s *Scheduler, db *store.DB, read store.Book) error {
			bg := context.Background()
			return s.ContinueStored(bg, read.ID, func() error {
				return db.ContinueReadalong(bg, read.ID, read.State, read.State, state.GoalReadalong, store.GenerateBook{})
			})
		}},
		{"resume", pause, func(s *Scheduler, _ *store.DB, read store.Book) error {
			return s.Resume(context.Background(), read.ID)
		}},
		{"terminate_requeue", pause, func(s *Scheduler, db *store.DB, read store.Book) error {
			paused, err := db.GetBook(context.Background(), read.ID) // the supervisor's read: paused
			if err != nil {
				return err
			}
			_, err = s.supervisorTerminateRequeue(context.Background(), paused)
			return err
		}},
	}
	for _, change := range changes {
		for _, clear := range clears {
			t.Run(change.name+"/"+clear.name, func(t *testing.T) {
				h := newHarness(t)
				db := h.openDB(t)
				bg := context.Background()
				s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
				read := parkedAudit(t, h, db, "cleared")

				ctx, fail := context.WithCancel(bg)
				defer fail()
				cleared := make(chan error, 1)
				var once sync.Once
				s.afterStatusClear = func() {
					once.Do(func() {
						if err := clear.stop(db, read.ID); err != nil {
							t.Error(err)
						}
						go func() { cleared <- clear.run(s, db, read) }()
						// The clear waits for this change; one that didn't would clear
						// the stop here, before the failure below puts the park back.
						waitQueuedOnPublish(t, s, read.ID, cleared)
						fail() // the change's store write fails
					})
				}
				if err := change.run(s, ctx, read); !errors.Is(err, context.Canceled) {
					t.Fatalf("the failed %s = %v, want context.Canceled", change.name, err)
				}
				select {
				case err := <-cleared:
					if err != nil {
						t.Fatalf("the %s after the cancel = %v", clear.name, err)
					}
				case <-time.After(waitLimit):
					t.Fatalf("the %s never ran", clear.name)
				}
				if got := storedFrame(t, db, read.ID); got.Status != "" || got.ParkCode != "" {
					t.Fatalf("the failed %s parked the book again over the %s: %+v", change.name, clear.name, got)
				}
				if f, ok := lastFrame(t, h.hub, read.ID); !ok || f != storedFrame(t, db, read.ID) {
					t.Fatalf("last frame %+v (%v), stored %+v", f, ok, storedFrame(t, db, read.ID))
				}
			})
		}
	}
}

// waitQueuedOnPublish waits until a second caller waits for book id's publish
// lock (the test holds it), or until done has its result: a caller that never
// waited for the lock. The result is put back for the test to read.
func waitQueuedOnPublish(t *testing.T, s *Scheduler, id int64, done chan error) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		s.pubMu.Lock()
		l := s.pubLocks[id]
		queued := l != nil && l.refs >= 2
		s.pubMu.Unlock()
		if queued {
			return
		}
		select {
		case err := <-done:
			done <- err
			return
		case <-time.After(time.Millisecond):
		}
	}
	t.Error("nothing waited for the publish lock")
}

// A publish's failed read of its book warns once, then logs at Debug; a book
// deleted since and a store closed at shutdown log nothing.
func TestPublishReadFailureWarnsOnceAndNeverAtShutdown(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var warned atomic.Bool
	broken := errors.New("disk I/O error")

	logPublishReadFailed(log, &warned, true, 1, broken)
	logPublishReadFailed(log, &warned, false, 1, store.ErrNotFound)
	if buf.Len() != 0 || warned.Load() {
		t.Fatalf("a closed store or a deleted book logged: %q", buf.String())
	}
	logPublishReadFailed(log, &warned, false, 1, broken)
	logPublishReadFailed(log, &warned, false, 2, broken)
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 || strings.Count(out, "level=DEBUG") != 1 {
		t.Fatalf("want one warning then one debug line, got %q", out)
	}
}

// publishStored after the store closed (a stage's last publish at shutdown)
// publishes nothing and returns.
func TestPublishStoredAfterCloseIsQuiet(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	b := h.addBook(t, db, "closed", "", "")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s.publishStored(context.Background(), b.ID)
	if s.publishReadWarned.Load() {
		t.Fatal("a publish after the store closed warned")
	}
	if _, ok := lastFrame(t, h.hub, b.ID); ok {
		t.Fatal("a publish after the store closed published a frame")
	}
}

// A publish that reads its book again after a warned failure clears the
// warn-once flag: the store's next failure is a new fault and warns, where a
// flag never cleared left every later one at Debug for the scheduler's lifetime.
func TestPublishReadSucceedingWarnsAgainNextTime(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	b := h.addBook(t, db, "recovered", "", "")
	s.publishReadWarned.Store(true) // an earlier read failed and warned
	s.publishStored(context.Background(), b.ID)
	if s.publishReadWarned.Load() {
		t.Fatal("a successful read left the warn-once flag set")
	}
	if _, ok := lastFrame(t, h.hub, b.ID); !ok {
		t.Fatal("the publish published no frame")
	}
}

// ContinueStored publishes nothing when its write fails: the book is as it was.
func TestContinueStoredPublishesOnlyAWrite(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	b := h.addBook(t, db, "refused", "", "")
	if err := s.ContinueStored(context.Background(), b.ID, func() error { return store.ErrNotContinuable }); !errors.Is(err, store.ErrNotContinuable) {
		t.Fatalf("ContinueStored = %v, want the write's error", err)
	}
	if f, ok := lastFrame(t, h.hub, b.ID); ok {
		t.Fatalf("a refused continue published %+v", f)
	}
}
