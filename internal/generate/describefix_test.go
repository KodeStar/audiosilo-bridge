package generate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// newParks counts a job's moves into needs_attention with park code: each one the
// server announces as a new park.
func (f *fakeServer) newParks(id int64, code string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, prev := 0, ""
	for _, u := range f.updates[id] {
		if u.State == nil || *u.State == prev {
			continue
		}
		var p struct {
			Park *struct {
				Code string `json:"code"`
			} `json:"park"`
		}
		_ = json.Unmarshal(u.Progress, &p)
		if *u.State == link.StateNeedsAttention && p.Park != nil && p.Park.Code == code {
			n++
		}
		prev = *u.State
	}
	return n
}

// A new generate job finds the earlier run held by a description (the server asked
// it to give way, which it does at its next report): it waits for the engine to
// let go, then generates, instead of failing.
func TestGenerateWaitsForADescriptionToLetGo(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(73)
	b := hr.engineBook(73)
	release, ok := hr.sched.Hold(b.ID)
	if !ok {
		t.Fatal("the finished book couldn't be held")
	}
	h := hr.handler()
	h.o.Poll = 50 * time.Millisecond // giveWayTries polls: two seconds to let go in
	hr.srv.add(74, "Author/Book", `{}`)
	stop := hr.runner(h)
	defer stop()
	hr.waitFor("the wait for the earlier run", func() bool {
		return hr.srv.saw(74, func(_ link.Update, p map[string]any) bool {
			return p["message"] == "Waiting for the engine to let go of the book's earlier run"
		})
	})
	release()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(74); return s == link.StateDone })
	if _, err := hr.db.GetBook(context.Background(), b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the earlier run wasn't cleared: %v", err)
	}
}

// A description written at a contributing park is composed again at done (the
// step's free re-compose): the contributing stage resolved the work since, and the
// library's description names the work its characters do.
func TestTheParkDescriptionFollowsTheResolvedWork(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	d := &fakeDescriber{db: hr.db, fromWork: true}
	h := hr.describeHandler(d)
	hr.exec.parks["contributing"] = 1
	hr.srv.add(33, "Author/Book", `{}`)
	stop := hr.runAll(h)
	defer stop()
	adir := filepath.Join(hr.root, "Author", "Book", AttachDir)
	hr.waitFor("the park's description", func() bool {
		raw, err := os.ReadFile(filepath.Join(adir, "description.json"))
		return err == nil && strings.Contains(string(raw), `"work":"the-book"`)
	})
	// The contributing stage resolves the work (its reconcile rewrites
	// characters.json; the description is left as it was).
	chars, _ := pipeline.FinalSidecarsPaths(hr.engineBook(33).WorkDir)
	raw, err := os.ReadFile(chars)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chars, []byte(strings.Replace(string(raw), `"work":"the-book"`, `"work":"the-real-book"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Retry(context.Background(), 33); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(33); return s == link.StateDone })
	for _, name := range []string{"characters.json", "description.json"} {
		if raw, err := os.ReadFile(filepath.Join(adir, name)); err != nil || !strings.Contains(string(raw), `"work":"the-real-book"`) {
			t.Fatalf("%s = %s, %v", name, raw, err)
		}
	}
	if calls := d.calls(); len(calls) != 2 {
		t.Fatalf("describe calls = %v, want the park's and done's", calls)
	}
}

// At a contributing park the job reports the park before it writes, and every
// report while parked carries it (the page shows the park, never a bare error),
// the write's and the description's steps included.
func TestAContributingParkIsReportedThroughTheWrite(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	gate := make(chan struct{})
	d := &fakeDescriber{db: hr.db, fail: func(int) error { <-gate; return nil }}
	h := hr.describeHandler(d)
	hr.exec.parks["contributing"] = 1
	hr.srv.add(38, "Author/Book", `{}`)
	stop := hr.runAll(h)
	defer stop()
	// The description step is held: the park is the job's meanwhile.
	hr.waitFor("the park while describing", func() bool {
		return hr.srv.saw(38, func(u link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return u.State != nil && *u.State == link.StateNeedsAttention && p["step"] == pipeline.DescribeStage &&
				park["code"] == string(state.ParkCoreNeeded)
		})
	})
	close(gate)
	if hr.srv.saw(38, func(u link.Update, p map[string]any) bool {
		return u.State != nil && *u.State == link.StateNeedsAttention && p["park"] == nil
	}) {
		t.Fatal("a parked report without its park")
	}
	if err := h.Retry(context.Background(), 38); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(38); return s == link.StateDone })
}

// A try at the park that failed without spending (a rate limit) gets a second go
// at done; one that spent (its checks exhausted) doesn't.
func TestTheParkDescriptionFailureAtDone(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		park  error
		free  bool
		calls int
		desc  bool
	}{
		"free":  {scheduler.ParkWithCodeAfter(state.ParkAgentRateLimited, "rate limited", time.Now().Add(time.Hour)), true, 2, true},
		"spent": {scheduler.ParkWithCode(state.ParkAgentValidationExhausted, "kept failing"), false, 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hr := newHarness(t)
			d := &fakeDescriber{db: hr.db, freeFail: tc.free, fail: func(call int) error {
				if call == 1 {
					return tc.park
				}
				return nil
			}}
			h := hr.describeHandler(d)
			hr.exec.parks["contributing"] = 1
			hr.srv.add(34, "Author/Book", `{}`)
			stop := hr.runAll(h)
			defer stop()
			hr.waitFor("the park's try", func() bool {
				return len(d.calls()) == 1 && hr.srv.saw(34, func(u link.Update, p map[string]any) bool {
					park, _ := p["park"].(map[string]any)
					return park["code"] == "core_needed"
				})
			})
			if err := h.Retry(context.Background(), 34); err != nil {
				t.Fatal(err)
			}
			hr.waitFor("done", func() bool { s, _ := hr.srv.final(34); return s == link.StateDone })
			var res Result
			hr.result(34, &res)
			if calls := d.calls(); len(calls) != tc.calls || res.Description != tc.desc {
				t.Fatalf("calls %v, result %+v", calls, res)
			}
		})
	}
}

// When the re-compose at done fails, the description the park wrote stands.
func TestTheParkDescriptionStandsWhenDoneCantRecompose(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	d := &fakeDescriber{db: hr.db, fail: func(call int) error {
		if call == 2 {
			return errors.New("the disk is full")
		}
		return nil
	}}
	h := hr.describeHandler(d)
	hr.exec.parks["contributing"] = 1
	hr.srv.add(37, "Author/Book", `{}`)
	stop := hr.runAll(h)
	defer stop()
	hr.waitFor("the park's description", func() bool { return len(d.calls()) == 1 })
	hr.waitFor("the park", func() bool { return hr.srv.newParks(37, string(state.ParkCoreNeeded)) > 0 })
	if err := h.Retry(context.Background(), 37); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(37); return s == link.StateDone })
	var res Result
	hr.result(37, &res)
	raw, err := os.ReadFile(filepath.Join(hr.root, "Author", "Book", AttachDir, "bridge.json"))
	if len(d.calls()) != 2 || !res.Description || err != nil || !strings.Contains(string(raw), `"description"`) {
		t.Fatalf("calls %v, result %+v, bridge.json %s", d.calls(), res, raw)
	}
}

// A description that spent and then failed its checks still counts in the
// generate job's cost, as the budgets count it.
func TestGenerateCostCountsAFailedDescription(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	d := &spendingDescriber{db: hr.db}
	hr.srv.add(35, "Author/Book", `{}`)
	stop := hr.runAll(hr.describeHandler(d))
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(35); return s == link.StateDone })
	var res Result
	hr.result(35, &res)
	sum, _, err := hr.db.SumStageRunBudgetCost(context.Background(), hr.engineBook(35).ID)
	if err != nil || res.Description || res.CostUSD != Cents(sum) || res.CostUSD != 3.65 {
		t.Fatalf("result %+v, the book's spend %v, %v", res, sum, err)
	}
}

// spendingDescriber spends on a describing stage run as the real step does (three
// tries at 0.05), then fails its checks.
type spendingDescriber struct{ db *store.DB }

func (s *spendingDescriber) Describe(ctx context.Context, bookID int64, _ bool) (pipeline.DescribeResult, error) {
	attempt, err := s.db.NextStageAttempt(ctx, bookID, pipeline.DescribeStage)
	if err != nil {
		return pipeline.DescribeResult{}, err
	}
	runID, err := s.db.StartStageRun(ctx, bookID, pipeline.DescribeStage, attempt)
	if err != nil {
		return pipeline.DescribeResult{}, err
	}
	for range 3 {
		id, err := s.db.StartAgentInvocation(ctx, bookID, pipeline.DescribeStage, "describing-a01", "claude", "sonnet")
		if err != nil {
			return pipeline.DescribeResult{}, err
		}
		if err := s.db.FinishAgentInvocation(ctx, id, "validation_failed", "sonnet", 900, 120, 0, 0.05, true, 0, false, "too short"); err != nil {
			return pipeline.DescribeResult{}, err
		}
	}
	_ = s.db.FinishStageRun(ctx, runID, false, nil)
	cost, _ := s.db.StageRunBudgetCost(ctx, runID)
	return pipeline.DescribeResult{CostUSD: cost}, scheduler.ParkWithCode(state.ParkAgentValidationExhausted, "kept failing")
}

// The characters and recaps reach the library before the description step runs:
// the step may wait a long time for an agent slot, and never holds them back.
func TestGenerateSavesBeforeTheDescription(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	var savedFirst atomic.Bool
	chars := filepath.Join(hr.root, "Author", "Book", AttachDir, "characters.json")
	d := &fakeDescriber{db: hr.db, fail: func(int) error {
		_, err := os.Stat(chars)
		savedFirst.Store(err == nil)
		return nil
	}}
	hr.srv.add(36, "Author/Book", `{}`)
	stop := hr.runAll(hr.describeHandler(d))
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(36); return s == link.StateDone })
	var res Result
	hr.result(36, &res)
	if !savedFirst.Load() || !res.Description ||
		strings.Join(res.Files, ",") != "characters.json,recaps.json,description.json,bridge.json" {
		t.Fatalf("saved before describing: %v, result %+v", savedFirst.Load(), res)
	}
}

// A description-only job whose book's .audiosilo/ can't be written parks
// book_not_writable once and stays parked while it tries again (never running in
// between, which the server would hear as a new park each time).
func TestDescribeJobParkDoesNotFlap(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(41)
	adir := filepath.Join(hr.root, "Author", "Book", AttachDir)
	if err := os.RemoveAll(adir); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, adir, "a file in the folder's place")
	h := hr.describeHandler(&fakeDescriber{db: hr.db})
	h.o.Recheck = 20 * time.Millisecond
	hr.srv.addDescribe(42, "Author/Book")
	stop := hr.runAll(h)
	defer stop()
	hr.waitFor("the park", func() bool { return hr.srv.newParks(42, parkBookNotWritable) > 0 })
	time.Sleep(400 * time.Millisecond) // many rechecks
	if n := hr.srv.newParks(42, parkBookNotWritable); n != 1 {
		t.Fatalf("parked anew %d times", n)
	}
	if err := os.Remove(adir); err != nil {
		t.Fatal(err)
	}
	if err := h.Retry(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(42); return s == link.StateDone })
}

// heldRescan holds the attachments rescan until gate closes.
type heldRescan struct {
	Server
	gate chan struct{}
}

func (g heldRescan) RescanAttachments(ctx context.Context, libraryID int64, path string) (*link.Attachments, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Server.RescanAttachments(ctx, libraryID, path)
}

// A description-only job parked when the Bridge stopped resumes with its park
// restored; the try that succeeds lifts it, so the run's later reports aren't
// the old park.
func TestDescribeJobLiftsARestoredPark(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(41)
	d := &fakeDescriber{db: hr.db, fail: func(call int) error {
		if call == 1 {
			return scheduler.ParkWithCodeAfter(state.ParkAgentRateLimited, "rate limited", time.Now().Add(time.Hour))
		}
		return nil
	}}
	stop := hr.runAll(hr.describeHandler(d))
	hr.srv.addDescribe(42, "Author/Book")
	hr.waitFor("the park", func() bool { return hr.srv.newParks(42, string(state.ParkAgentRateLimited)) > 0 })
	stop() // the Bridge stops with the job parked
	hr.srv.set(func() { hr.srv.updates[42] = nil })

	gate := make(chan struct{})
	h := hr.describeHandler(d)
	h.o.Server = func() Server { return heldRescan{Server: hr.cl, gate: gate} }
	stop2 := hr.runAll(h)
	defer stop2()
	hr.waitFor("the rescan", func() bool {
		return hr.srv.saw(42, func(_ link.Update, p map[string]any) bool { return p["step"] == stepRescanning })
	})
	time.Sleep(100 * time.Millisecond) // a keepalive or two
	if hr.srv.saw(42, func(u link.Update, p map[string]any) bool {
		return (p["step"] == stepWriting || p["step"] == stepRescanning) && u.State != nil && *u.State == link.StateNeedsAttention
	}) {
		t.Fatal("the restored park went on after the description succeeded")
	}
	close(gate)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(42); return s == link.StateDone })
}

// A Retry pressed while a parked description-only job tries again at its
// retry_at is answered by that try: when it parks again, the job waits for its new
// retry_at rather than trying once more at once.
func TestDescribeJobRetryDuringATry(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(41)
	trying := make(chan struct{})
	gate := make(chan struct{})
	d := &fakeDescriber{db: hr.db, freeFail: true}
	d.fail = func(call int) error {
		switch call {
		case 1:
			return scheduler.ParkWithCodeAfter(state.ParkAgentRateLimited, "rate limited", time.Now().Add(20*time.Millisecond))
		case 2:
			close(trying)
			<-gate
			return scheduler.ParkWithCodeAfter(state.ParkAgentRateLimited, "rate limited", time.Now().Add(time.Hour))
		}
		return nil
	}
	h := hr.describeHandler(d)
	stop := hr.runAll(h)
	defer stop()
	hr.srv.addDescribe(42, "Author/Book")
	<-trying
	if err := h.Retry(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	close(gate)
	time.Sleep(200 * time.Millisecond)
	if n := len(d.calls()); n != 2 {
		t.Fatalf("describe calls = %d after the press during a try", n)
	}
	if err := h.Retry(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(42); return s == link.StateDone })
}

// unmappedLibs is library 1, checked mapped and writable or not, as told.
type unmappedLibs struct {
	root   string
	mapped atomic.Bool
}

func (l *unmappedLibs) Library(id int64) (link.Library, bool) {
	return link.Library{ID: 1, Name: "Books", Root: l.root}, id == 1
}

func (l *unmappedLibs) Status(id int64) (link.LibraryStatus, bool) {
	m := l.mapped.Load()
	return link.LibraryStatus{LibraryID: 1, MountPath: l.root, Mapped: m, Writable: m}, id == 1
}

// A description-only job resumed after a restart onto a library that isn't mapped
// (yet) parks library_not_writable and goes on once it is (denied: a new one is
// refused at once).
func TestDescribeJobResumedOntoAnUnmappedLibrary(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(71)
	libs := &unmappedLibs{root: hr.root}
	libs.mapped.Store(true)
	handler := func(d Describer) *Handler {
		return New(Options{DB: hr.db, Engine: func() Engine { return hr.sched }, Describer: func() Describer { return d },
			Libraries: libs, Mount: func(int64, string) (string, bool) { return hr.root, false },
			Server: func() Server { return hr.cl }, WorkRoot: hr.work, Version: "0.2.0", RetentionDays: 30,
			Poll: 10 * time.Millisecond, IdlePoll: 10 * time.Millisecond, Recheck: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}
	d := &fakeDescriber{db: hr.db, fail: func(call int) error {
		if call == 1 {
			return scheduler.ParkWithCode(state.ParkBudgetExceeded, "the budget is spent")
		}
		return nil
	}}
	stop := hr.runAll(handler(d))
	hr.srv.addDescribe(72, "Author/Book")
	hr.waitFor("the budget park", func() bool { return hr.srv.newParks(72, string(state.ParkBudgetExceeded)) > 0 })
	stop()
	libs.mapped.Store(false)
	stop2 := hr.runAll(handler(d))
	defer stop2()
	hr.waitFor("library_not_writable", func() bool {
		return hr.srv.saw(72, func(_ link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return park["code"] == parkLibraryNotWritable
		})
	})
	if s, _ := hr.srv.final(72); s != "" {
		t.Fatalf("the resumed job ended: %s", s)
	}
	libs.mapped.Store(true)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(72); return s == link.StateDone })

	// A new one on the unmapped library is refused at once.
	libs.mapped.Store(false)
	hr.srv.addDescribe(73, "Author/Book")
	hr.waitFor("the refusal", func() bool { s, _ := hr.srv.final(73); return s == link.StateFailed })
}

// A book that moved after its generate job ended: the Bridge's records stayed at
// the old place, but the book's bridge.json moved with it and names the run, whose
// notes the description-only job reads; the job's mapping follows the book.
func TestDescribeJobFindsARunThatMovedAfterItsJob(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(93)
	if err := os.Rename(filepath.Join(hr.root, "Author", "Book"), filepath.Join(hr.root, "Author", "Book (2026)")); err != nil {
		t.Fatal(err)
	}
	hr.srv.set(func() {
		hr.srv.books["Author/Book (2026)"] = link.Book{LibraryID: 1, Path: "Author/Book (2026)", Files: []link.BookFile{
			{Path: "Author/Book (2026)/01.mp3", Size: 9}, {Path: "Author/Book (2026)/02.mp3", Size: 10}}}
		delete(hr.srv.books, "Author/Book")
	})
	d := &fakeDescriber{db: hr.db}
	stop := hr.runAll(hr.describeHandler(d))
	defer stop()
	hr.srv.addDescribe(94, "Author/Book (2026)")
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(94); return s == link.StateDone })
	if len(d.calls()) != 1 {
		t.Fatalf("describe calls = %v", d.calls())
	}
	if m, err := hr.db.GetGenerateBook(context.Background(), 93); err != nil || m.RelPath != "Author/Book (2026)" {
		t.Fatalf("the run's mapping = %+v, %v", m, err)
	}
}

// A description-only job doesn't take another server's run of the same library id
// and path for its notes: that run's book lies outside this library's folder.
func TestDescribeJobIgnoresAnotherServersRun(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	ctx := context.Background()
	other := filepath.Join(t.TempDir(), "elsewhere", "Author", "Book")
	b, err := hr.db.CreateBook(ctx, store.NewBook{SourcePath: other, WorkDir: filepath.Join(hr.work, "other"), Title: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	_ = hr.db.SetBookState(ctx, b.ID, "done", "", "", "")
	if err := hr.db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 9, BookID: &b.ID, LibraryID: 1, RelPath: "Author/Book"}); err != nil {
		t.Fatal(err)
	}
	_ = hr.db.EndGenerateBook(ctx, 9, time.Now())
	d := &fakeDescriber{db: hr.db}
	stop := hr.runAll(hr.describeHandler(d))
	defer stop()
	hr.srv.addDescribe(42, "Author/Book")
	hr.waitFor("the refusal", func() bool { s, _ := hr.srv.final(42); return s == link.StateFailed })
	if _, u := hr.srv.final(42); u.Error == nil || *u.Error != string(errDescribeNoNotes) || len(d.calls()) != 0 {
		t.Fatalf("failed with %v, calls %v", u.Error, d.calls())
	}
}

// A regenerate whose own description fails leaves the earlier description.json in
// place (nothing deletes into a library), and its bridge.json carries over the
// earlier one's description object, so the file is still vouched for. With no
// description.json in place there is nothing to vouch for (see
// TestGenerateSurvivesADescriptionFailure).
func TestARegenerateKeepsTheKeptDescriptionVouchedFor(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	adir := filepath.Join(hr.root, "Author", "Book", AttachDir)
	hr.srv.add(45, "Author/Book", `{}`)
	stop := hr.runAll(hr.describeHandler(&fakeDescriber{db: hr.db}))
	hr.waitFor("the first run", func() bool { s, _ := hr.srv.final(45); return s == link.StateDone })
	stop()

	failing := &fakeDescriber{db: hr.db, fail: func(int) error {
		return scheduler.ParkWithCode(state.ParkAgentValidationExhausted, "kept failing")
	}}
	hr.srv.add(46, "Author/Book", `{}`)
	stop = hr.runAll(hr.describeHandler(failing))
	defer stop()
	hr.waitFor("the regenerate", func() bool { s, _ := hr.srv.final(46); return s == link.StateDone })
	var res Result
	hr.result(46, &res)
	if res.Description || len(failing.calls()) != 1 {
		t.Fatalf("result %+v, calls %v", res, failing.calls())
	}
	if raw, err := os.ReadFile(filepath.Join(adir, "description.json")); err != nil || string(raw) != testDescription {
		t.Fatalf("description.json = %s, %v", raw, err)
	}
	var prov Provenance
	raw, _ := os.ReadFile(filepath.Join(adir, "bridge.json"))
	if err := json.Unmarshal(raw, &prov); err != nil || prov.JobID != 46 || prov.Description == nil ||
		prov.Description.JobID != 45 || prov.Description.CheckedAgainst != pipeline.CheckedTranscripts ||
		prov.Description.GeneratedAt != "2026-10-16T12:00:00Z" {
		t.Fatalf("bridge.json = %s, %v", raw, err)
	}
}

// keptObject reads the description object only beside a description.json
// that is a regular file, and only from a bridge.json that holds one.
func TestKeptDescription(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adir := filepath.Join(dir, AttachDir)
	mustWrite(t, filepath.Join(adir, "bridge.json"),
		`{"job_id":9,"description":{"job_id":8,"generated_at":"2026-10-16T12:00:00Z","from":"opening_notes","chapters":2,"checked_against":"book text"}}`)
	if d := keptObject[DescriptionProvenance](dir, "", descriptionName, "description"); d != nil {
		t.Fatalf("no description.json, kept %+v", d)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.json"), filepath.Join(adir, "description.json")); err != nil {
		t.Fatal(err)
	}
	if d := keptObject[DescriptionProvenance](dir, "", descriptionName, "description"); d != nil {
		t.Fatalf("a linked description.json, kept %+v", d)
	}
	_ = os.Remove(filepath.Join(adir, "description.json"))
	mustWrite(t, filepath.Join(adir, "description.json"), testDescription)
	if d := keptObject[DescriptionProvenance](dir, "", descriptionName, "description"); d == nil || d.JobID != 8 || d.CheckedAgainst != "book text" {
		t.Fatalf("kept %+v", d)
	}
	for _, prov := range []string{`{"job_id":9}`, `{"job_id":9,"description":null}`, `{"description":"x"}`, `not json`} {
		mustWrite(t, filepath.Join(adir, "bridge.json"), prov)
		if d := keptObject[DescriptionProvenance](dir, "", descriptionName, "description"); d != nil {
			t.Fatalf("bridge.json %s, kept %+v", prov, d)
		}
	}
}

// The engine's try at the start of contributing spent and failed: the job's pass
// at done doesn't pay for a second (for an ebook it would be checked against
// nothing), and says why there is none.
func TestGenerateDoesNotPayTwiceForADescription(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	d := &fakeDescriber{db: hr.db}
	gate := hr.exec.gate("contributing")
	hr.srv.add(47, "Author/Book", `{}`)
	stop := hr.runAll(hr.describeHandler(d))
	defer stop()
	hr.waitFor("contributing", func() bool {
		return hr.srv.saw(47, func(_ link.Update, p map[string]any) bool { return p["stage"] == "contributing" })
	})
	// The engine's own try, as the real step records one that failed its checks.
	var park *scheduler.ParkError
	if _, err := (&spendingDescriber{db: hr.db}).Describe(context.Background(), hr.engineBook(47).ID, true); !errors.As(err, &park) {
		t.Fatalf("the engine's try = %v", err)
	}
	close(gate)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(47); return s == link.StateDone })
	var res Result
	hr.result(47, &res)
	if len(d.calls()) != 0 || res.Description || !strings.Contains(res.DescriptionError, "an earlier try") || res.CostUSD != 3.65 {
		t.Fatalf("calls %v, result %+v", d.calls(), res)
	}
}
