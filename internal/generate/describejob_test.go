package generate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// testDescription is a composed description as the pipeline keeps it.
const testDescription = `{"license":"CC-BY-SA-4.0","sources":[{"type":"community"}],"text":"A paragraph.","work":"the-book"}`

// fakeDescriber stands in for the pipeline's description step: it writes the
// composed file into the engine book's work dir, or returns fail's error for a
// call (1-based), having spent 0.01 unless freeFail. With fromWork the file names
// the work characters.json names now, as the real step's re-compose does.
type fakeDescriber struct {
	db       *store.DB
	mu       sync.Mutex
	runs     []bool // each call's noWait
	fail     func(call int) error
	freeFail bool
	fromWork bool
}

func (f *fakeDescriber) Describe(ctx context.Context, bookID int64, noWait bool) (pipeline.DescribeResult, error) {
	f.mu.Lock()
	f.runs = append(f.runs, noWait)
	call := len(f.runs)
	fail, freeFail, fromWork := f.fail, f.freeFail, f.fromWork
	f.mu.Unlock()
	if fail != nil {
		if err := fail(call); err != nil {
			if freeFail {
				return pipeline.DescribeResult{}, err
			}
			return pipeline.DescribeResult{CostUSD: 0.01}, err
		}
	}
	b, err := f.db.GetBook(ctx, bookID)
	if err != nil {
		return pipeline.DescribeResult{}, err
	}
	path := pipeline.FinalDescriptionPath(b.WorkDir)
	desc := testDescription
	if fromWork {
		chars, _ := pipeline.FinalSidecarsPaths(b.WorkDir)
		var c struct {
			Work string `json:"work"`
		}
		raw, _ := os.ReadFile(chars) //nolint:gosec // the test's work dir
		_ = json.Unmarshal(raw, &c)
		desc = strings.Replace(testDescription, `"work":"the-book"`, `"work":"`+c.Work+`"`, 1)
	}
	if err := os.WriteFile(path, []byte(desc), 0o600); err != nil {
		return pipeline.DescribeResult{}, err
	}
	return pipeline.DescribeResult{Path: path, Chapters: 2, CheckedAgainst: pipeline.CheckedTranscripts,
		CostUSD: 0.25, GeneratedAt: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}, nil
}

// calls is each call's noWait.
func (f *fakeDescriber) calls() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.runs)
}

// describeHandler is a handler whose engine writes descriptions with d.
func (hr *harness) describeHandler(d Describer) *Handler {
	return New(Options{DB: hr.db, Engine: func() Engine { return hr.sched }, Describer: func() Describer { return d },
		Libraries: hr.libs, Mount: func(int64, string) (string, bool) { return hr.libs.root, false },
		Server: func() Server { return hr.cl }, WorkRoot: hr.work, Version: "0.2.0", RetentionDays: 30,
		Poll: 10 * time.Millisecond, IdlePoll: 10 * time.Millisecond, Recheck: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// runAll runs the generate and describe jobs of h until stopped.
func (hr *harness) runAll(h *Handler) (stop func()) {
	r := jobs.New(hr.db, func() jobs.Server { return hr.cl }, nil, jobs.Options{Keepalive: 30 * time.Millisecond,
		ProgressEvery: 5 * time.Millisecond, Wait: 10 * time.Millisecond, RetryEvery: 20 * time.Millisecond,
		ServerID: func() string { return "srv" }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, h, h.DescribeJob())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	return func() { cancel(); <-done }
}

// addDescribe queues a sidecars.describe job.
func (f *fakeServer) addDescribe(id int64, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lib := int64(1)
	f.queue = append(f.queue, link.Job{ID: id, Kind: DescribeKind, LibraryID: &lib, Path: &path, Params: json.RawMessage(`{}`), Attempts: 1, State: "claimed"})
}

// result decodes a done job's result.
func (hr *harness) result(id int64, v any) {
	hr.t.Helper()
	_, final := hr.srv.final(id)
	if err := json.Unmarshal(final.Result, v); err != nil {
		hr.t.Fatalf("result of %d = %s: %v", id, final.Result, err)
	}
}

// A generate job writes the description beside the characters and recaps in the
// same finish, asking for it once, without waiting out a rate limit.
func TestGenerateWritesTheDescription(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	d := &fakeDescriber{db: hr.db}
	h := hr.describeHandler(d)
	hr.exec.parks["contributing"] = 1 // a contributing park writes first, then done
	hr.srv.add(31, "Author/Book", `{}`)
	stop := hr.runAll(h)
	defer stop()
	hr.waitFor("the contributing park", func() bool {
		return hr.srv.saw(31, func(u link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return park["code"] == "core_needed"
		})
	})
	if err := h.Retry(context.Background(), 31); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(31); return s == link.StateDone })
	var res Result
	hr.result(31, &res)
	if !res.Description || res.DescriptionError != "" ||
		strings.Join(res.Files, ",") != "characters.json,recaps.json,description.json,bridge.json" {
		t.Fatalf("result = %+v", res)
	}
	// Asked at the park, and again at done (to re-compose it with the work the
	// contributing stage resolved), never waiting out a rate limit.
	if calls := d.calls(); len(calls) != 2 || !calls[0] || !calls[1] {
		t.Fatalf("describe calls = %+v, want two with noWait", calls)
	}
	adir := filepath.Join(hr.root, "Author", "Book", AttachDir)
	if raw, err := os.ReadFile(filepath.Join(adir, "description.json")); err != nil || string(raw) != testDescription {
		t.Fatalf("description.json = %s, %v", raw, err)
	}
	if info, _ := os.Stat(filepath.Join(adir, "description.json")); info.Mode().Perm() != 0o644 {
		t.Fatalf("description.json is %v", info.Mode().Perm())
	}
	var prov Provenance
	raw, _ := os.ReadFile(filepath.Join(adir, "bridge.json"))
	if err := json.Unmarshal(raw, &prov); err != nil || prov.Description == nil || prov.Description.JobID != 31 ||
		prov.Description.From != "opening_notes" || prov.Description.Chapters != 2 ||
		prov.Description.CheckedAgainst != "transcripts" || prov.Description.GeneratedAt != "2026-10-16T12:00:00Z" {
		t.Fatalf("bridge.json = %s, %v", raw, err)
	}
}

// A description that can't be written never fails, parks or holds the job: it
// finishes with the characters and recaps and says why.
func TestGenerateSurvivesADescriptionFailure(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	d := &fakeDescriber{db: hr.db, fail: func(int) error {
		return scheduler.ParkWithCodeAfter(state.ParkAgentRateLimited, "rate limited", time.Now().Add(time.Hour))
	}}
	hr.srv.add(32, "Author/Book", `{}`)
	stop := hr.runAll(hr.describeHandler(d))
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(32); return s == link.StateDone })
	var res Result
	hr.result(32, &res)
	if res.Description || res.DescriptionError != "the agent account hit its usage limit" || res.Characters != 2 ||
		strings.Join(res.Files, ",") != "characters.json,recaps.json,bridge.json" {
		t.Fatalf("result = %+v", res)
	}
	if hr.srv.saw(32, func(u link.Update, _ map[string]any) bool {
		return u.State != nil && *u.State == link.StateNeedsAttention
	}) {
		t.Fatal("the description parked the generate job")
	}
	if _, err := os.Stat(filepath.Join(hr.root, "Author", "Book", AttachDir, "description.json")); !os.IsNotExist(err) {
		t.Fatal("a description was written")
	}
	var prov map[string]any
	raw, _ := os.ReadFile(filepath.Join(hr.root, "Author", "Book", AttachDir, "bridge.json"))
	if err := json.Unmarshal(raw, &prov); err != nil || prov["description"] != nil {
		t.Fatalf("bridge.json = %s", raw)
	}
}

// generated runs a generate job for Author/Book to done with no description.
func (hr *harness) generated(id int64) {
	hr.t.Helper()
	hr.srv.add(id, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("the generate job", func() bool { s, _ := hr.srv.final(id); return s == link.StateDone })
}

// The description-only job: the notes of the book's earlier run, the file and
// bridge.json's description object saved with the book (the run's own bridge.json
// fields kept), the server asked to re-read them.
func TestDescribeJob(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(41)
	describing := func(_ link.Update, p map[string]any) bool {
		return p["stage"] == "describing" && p["step"] == "describing"
	}
	// The runner sends only a job's latest progress, so the call holds until the
	// server has the step reported before it (an instant call would replace it
	// first). poll, not waitFor: this runs on the job's goroutine; the test's own
	// wait for the step, which starts first, fails if it never comes.
	d := &fakeDescriber{db: hr.db, fail: func(int) error {
		poll(func() bool { return hr.srv.saw(42, describing) })
		return nil
	}}
	hr.srv.addDescribe(42, "Author/Book")
	stop := hr.runAll(hr.describeHandler(d))
	defer stop()
	hr.waitFor("the describing stage", func() bool { return hr.srv.saw(42, describing) })
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(42); return s == link.StateDone })
	var res DescribeJobResult
	hr.result(42, &res)
	if res.LibraryID != 1 || res.Path != "Author/Book" || !res.Description || res.CostUSD != 0.25 || res.CheckedAgainst != "transcripts" ||
		strings.Join(res.Files, ",") != "description.json,bridge.json" {
		t.Fatalf("result = %+v", res)
	}
	if calls := d.calls(); len(calls) != 1 || calls[0] {
		t.Fatalf("describe calls = %+v", calls)
	}
	adir := filepath.Join(hr.root, "Author", "Book", AttachDir)
	if raw, err := os.ReadFile(filepath.Join(adir, "description.json")); err != nil || string(raw) != testDescription {
		t.Fatalf("description.json = %s, %v", raw, err)
	}
	var prov Provenance
	raw, _ := os.ReadFile(filepath.Join(adir, "bridge.json"))
	if err := json.Unmarshal(raw, &prov); err != nil || prov.Kind != Kind || prov.JobID != 41 || len(prov.Inputs) != 2 ||
		prov.Description == nil || prov.Description.JobID != 42 || prov.Description.Chapters != 2 {
		t.Fatalf("bridge.json = %s, %v", raw, err)
	}
	hr.srv.mu.Lock()
	defer hr.srv.mu.Unlock()
	if n := len(hr.srv.rescans); n != 2 || hr.srv.rescans[1]["mode"] != "attachments" {
		t.Fatalf("rescans = %+v", hr.srv.rescans)
	}
}

// A description-only job for an ebook-sourced book that moved since its run: the
// .epub went with it, and a fresh bridge.json names it where it is now (before,
// the description's save didn't follow it and named the old, missing path).
func TestDescribeJobFollowsAMovedEpub(t *testing.T) {
	old := readEpub
	readEpub = func(string) error { return nil }
	t.Cleanup(func() { readEpub = old })
	hr := newHarness(t)
	mustWrite(t, filepath.Join(hr.root, "Author", "Book", "The Book.epub"), "epub!")
	hr.srv.add(91, "Author/Book", `{"source":"auto"}`)
	stop := hr.runner(hr.h)
	hr.waitFor("generated", func() bool { s, _ := hr.srv.final(91); return s == link.StateDone })
	stop()
	if err := os.Rename(filepath.Join(hr.root, "Author", "Book"), filepath.Join(hr.root, "Author", "Book (2026)")); err != nil {
		t.Fatal(err)
	}
	// No bridge.json to keep: the description writes a fresh one.
	if err := os.Remove(filepath.Join(hr.root, "Author", "Book (2026)", AttachDir, "bridge.json")); err != nil {
		t.Fatal(err)
	}
	hr.srv.set(func() {
		hr.srv.books["Author/Book (2026)"] = link.Book{LibraryID: 1, Path: "Author/Book (2026)", Files: []link.BookFile{
			{Path: "Author/Book (2026)/01.mp3", Size: 9}, {Path: "Author/Book (2026)/02.mp3", Size: 10}}}
		delete(hr.srv.books, "Author/Book")
	})
	// The server followed the book; so did the generate job's mapping.
	if err := hr.db.SetGenerateBookPath(context.Background(), 91, "Author/Book (2026)"); err != nil {
		t.Fatal(err)
	}
	hr.srv.addDescribe(92, "Author/Book (2026)")
	stopAll := hr.runAll(hr.describeHandler(&fakeDescriber{db: hr.db}))
	defer stopAll()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(92); return s == link.StateDone })
	var prov Provenance
	raw, _ := os.ReadFile(filepath.Join(hr.root, "Author", "Book (2026)", AttachDir, "bridge.json"))
	if err := json.Unmarshal(raw, &prov); err != nil || prov.Source != SourceEbook || len(prov.Inputs) != 1 ||
		prov.Inputs[0].Path != "Author/Book (2026)/The Book.epub" || prov.Inputs[0].Size != 5 {
		t.Fatalf("bridge.json = %s, %v", raw, err)
	}
}

// No run to read (never generated here), and a run still going: each fails
// with the console's sentence, before any spend.
func TestDescribeJobRefusals(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	d := &fakeDescriber{db: hr.db}
	h := hr.describeHandler(d)
	gate := hr.exec.gate("asr")
	hr.srv.add(51, "Author/Book", `{}`)
	stop := hr.runAll(h)
	defer stop()
	hr.waitFor("asr", func() bool {
		return hr.srv.saw(51, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	hr.srv.addDescribe(52, "Author/Book")
	hr.srv.addDescribe(53, "Loose.m4b")
	for id, want := range map[int64]describeFailure{52: errDescribeGenerating, 53: errDescribeNoNotes} {
		hr.waitFor("the refusal", func() bool { s, _ := hr.srv.final(id); return s == link.StateFailed })
		if _, u := hr.srv.final(id); u.Error == nil || *u.Error != string(want) {
			t.Fatalf("job %d failed with %v, want %q", id, u.Error, want)
		}
	}
	close(gate)
	if len(d.calls()) != 0 {
		t.Fatal("a refused job described")
	}
}

// An agent park parks the job with its code and hint and tries again on Retry (no
// retry_at) or at its retry_at; validation exhausted fails it.
func TestDescribeJobParks(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(61)
	var mu sync.Mutex
	mode := "budget"
	rateLimited := func(u link.Update, p map[string]any) bool {
		park, _ := p["park"].(map[string]any)
		return u.State != nil && *u.State == link.StateNeedsAttention &&
			park["code"] == "agent_rate_limited" && park["retry_at"] != nil
	}
	d := &fakeDescriber{db: hr.db, fail: func(call int) error {
		mu.Lock()
		m := mode
		mu.Unlock()
		switch {
		case m == "budget" && call == 1:
			return scheduler.ParkWithCode(state.ParkBudgetExceeded, "the budget is spent")
		case m == "rate" && call == 3:
			return scheduler.ParkWithCodeAfter(state.ParkAgentRateLimited, "rate limited", time.Now().Add(50*time.Millisecond))
		case m == "rate": // the try at retry_at holds until the server has the park, as in TestDescribeJob
			poll(func() bool { return hr.srv.saw(63, rateLimited) })
		case m == "checks":
			return scheduler.ParkWithCode(state.ParkAgentValidationExhausted, "kept failing")
		}
		return nil
	}}
	h := hr.describeHandler(d)
	stop := hr.runAll(h)
	defer stop()

	hr.srv.addDescribe(62, "Author/Book")
	hr.waitFor("the budget park", func() bool {
		return hr.srv.saw(62, func(u link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return u.State != nil && *u.State == link.StateNeedsAttention && u.Error != nil && *u.Error == "the budget is spent" &&
				park["code"] == "budget_exceeded" && park["hint"] == ParkHint("budget_exceeded") && park["retry_at"] == nil
		})
	})
	if err := h.Retry(context.Background(), 62); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done after the retry", func() bool { s, _ := hr.srv.final(62); return s == link.StateDone })

	mu.Lock()
	mode = "rate"
	mu.Unlock()
	if err := os.Remove(filepath.Join(hr.root, "Author", "Book", AttachDir, "description.json")); err != nil {
		t.Fatal(err)
	}
	hr.srv.addDescribe(63, "Author/Book")
	hr.waitFor("the rate limit's park", func() bool { return hr.srv.saw(63, rateLimited) })
	hr.waitFor("done at retry_at", func() bool { s, _ := hr.srv.final(63); return s == link.StateDone })

	mu.Lock()
	mode = "checks"
	mu.Unlock()
	hr.srv.addDescribe(64, "Author/Book")
	hr.waitFor("failed", func() bool { s, _ := hr.srv.final(64); return s == link.StateFailed })
	if _, u := hr.srv.final(64); u.Error == nil || *u.Error != string(errDescribeChecks) {
		t.Fatalf("failed with %v", u.Error)
	}
}

// A describe job parked on the monthly budget (retry_at the 1st) tries again
// when the budget is raised on the page (WakeParked), not before; a wake for
// another park leaves it.
func TestDescribeJobWakesWhenTheBudgetIsRaised(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(65)
	d := &fakeDescriber{db: hr.db, fail: func(call int) error {
		if call == 1 {
			return scheduler.ParkWithCodeAfter(state.ParkBudgetExceeded, "the monthly budget is spent",
				time.Now().Add(30*24*time.Hour))
		}
		return nil
	}}
	h := hr.describeHandler(d)
	stop := hr.runAll(h)
	defer stop()

	hr.srv.addDescribe(66, "Author/Book")
	hr.waitFor("the budget park", func() bool {
		return hr.srv.saw(66, func(u link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return u.State != nil && *u.State == link.StateNeedsAttention && park["code"] == "budget_exceeded"
		})
	})
	if n := h.WakeParked(state.ParkAgentRateLimited); n != 0 {
		t.Fatalf("a rate-limit wake woke %d", n)
	}
	hr.waitFor("woken", func() bool { return h.WakeParked(state.ParkBudgetExceeded) == 1 })
	hr.waitFor("done after the wake", func() bool { s, _ := hr.srv.final(66); return s == link.StateDone })
	if n := h.WakeParked(state.ParkBudgetExceeded); n != 0 {
		t.Fatalf("a finished job is still parked: woke %d", n)
	}
}

// A wake for the park a try ends in, made while that try ran, isn't lost: on the
// first try (no park registered yet) or a later one (its wake-up drained as a
// Retry the try answered), the job tries again at once rather than at retry_at.
func TestDescribeJobWakesDuringATry(t *testing.T) {
	t.Parallel()
	for name, wakeOn := range map[string]int{"the first try": 1, "a later try": 2} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hr := newHarness(t)
			hr.generated(67)
			trying, release := make(chan struct{}), make(chan struct{})
			d := &fakeDescriber{db: hr.db, fail: func(call int) error {
				if call == wakeOn {
					close(trying)
					<-release
				}
				if call <= wakeOn {
					return scheduler.ParkWithCodeAfter(state.ParkBudgetExceeded, "the monthly budget is spent",
						time.Now().Add(30*24*time.Hour))
				}
				return nil
			}}
			h := hr.describeHandler(d)
			stop := hr.runAll(h)
			defer stop()
			hr.srv.addDescribe(68, "Author/Book")
			if wakeOn > 1 {
				hr.waitFor("the first park", func() bool { return h.WakeParked(state.ParkBudgetExceeded) == 1 })
			}
			<-trying
			h.WakeParked(state.ParkBudgetExceeded)
			close(release)
			hr.waitFor("done after the wake", func() bool { s, _ := hr.srv.final(68); return s == link.StateDone })
		})
	}
}

// A new generate job doesn't delete the earlier run a description holds: the
// engine's Delete says busy, and the job asks to try again.
func TestGenerateWaitsForADescription(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(71)
	b := hr.engineBook(71)
	release, ok := hr.sched.Hold(b.ID)
	if !ok {
		t.Fatal("the finished book couldn't be held")
	}
	defer release()
	hr.srv.add(72, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("failed", func() bool { s, _ := hr.srv.final(72); return s == link.StateFailed })
	if _, u := hr.srv.final(72); u.Error == nil || !strings.Contains(*u.Error, "try again in a minute") {
		t.Fatalf("failed with %v", u.Error)
	}
	if _, err := hr.db.GetBook(context.Background(), b.ID); err != nil {
		t.Fatalf("the earlier run was deleted: %v", err)
	}
}

// A description that can't get the engine's hold on the book: the description-only
// job fails asking to try again, and a generate job finishes without one, saying
// why. Neither describes.
func TestDescriptionNeedsTheHold(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(81)
	b := hr.engineBook(81)
	release, ok := hr.sched.Hold(b.ID)
	if !ok {
		t.Fatal("the finished book couldn't be held")
	}
	d := &fakeDescriber{db: hr.db}
	stop := hr.runAll(hr.describeHandler(d))
	hr.srv.addDescribe(82, "Author/Book")
	hr.waitFor("failed", func() bool { s, _ := hr.srv.final(82); return s == link.StateFailed })
	if _, u := hr.srv.final(82); u.Error == nil || *u.Error != string(errBookBusy) {
		t.Fatalf("failed with %v", u.Error)
	}
	stop()
	release()

	// The generate finish, its engine refusing every hold.
	h := hr.describeHandler(d)
	h.o.Engine = func() Engine { return refusingEngine{hr.sched} }
	stopGen := hr.runAll(h)
	defer stopGen()
	hr.srv.add(83, "Loose.m4b", `{}`)
	hr.waitFor("the job done", func() bool { s, _ := hr.srv.final(83); return s == link.StateDone })
	var res Result
	hr.result(83, &res)
	if res.Description || res.DescriptionError != "the engine was busy with this book" {
		t.Fatalf("result = %+v", res)
	}
	if len(d.calls()) != 0 {
		t.Fatalf("described %d times without the hold", len(d.calls()))
	}
}

// refusingEngine is the scheduler with every hold refused (a stage runs on the book).
type refusingEngine struct{ Engine }

func (refusingEngine) Hold(int64) (func(), bool) { return func() {}, false }

// bridge.json after a description-only job keeps every field it had (unknown
// ones included) and only adds or replaces description; with none fit to keep
// it is written fresh.
func TestDescribedProvenance(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	adir := filepath.Join(dir, AttachDir)
	fresh := provenance("0.3.0", DescribeKind, 9, time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), SourceAudio, "w", nil)
	fresh.Description = &DescriptionProvenance{JobID: 9, From: descriptionFrom, Chapters: 3, CheckedAgainst: "none"}

	got := func() map[string]any {
		t.Helper()
		raw, err := describedProvenance(dir, "", fresh)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := got(); m["kind"] != DescribeKind || m["job_id"] != float64(9) {
		t.Fatalf("no bridge.json: %v", m)
	}
	mustWrite(t, filepath.Join(adir, "bridge.json"), `{"kind":"sidecars.generate","job_id":3,"generated_at":"2026-10-01T00:00:00Z",`+
		`"later_field":true,"description":{"job_id":1}}`)
	m := got()
	desc, _ := m["description"].(map[string]any)
	if m["kind"] != Kind || m["job_id"] != float64(3) || m["generated_at"] != "2026-10-01T00:00:00Z" || m["later_field"] != true ||
		desc["job_id"] != float64(9) || desc["chapters"] != float64(3) {
		t.Fatalf("kept = %v", m)
	}
	for _, bad := range []string{`[1,2]`, `not json`, `null`} {
		mustWrite(t, filepath.Join(adir, "bridge.json"), bad)
		if m := got(); m["kind"] != DescribeKind {
			t.Fatalf("%s: %v", bad, m)
		}
	}
	// A link in its place is not read through.
	_ = os.Remove(filepath.Join(adir, "bridge.json"))
	other := filepath.Join(dir, "elsewhere.json")
	mustWrite(t, other, `{"kind":"leaked"}`)
	if err := os.Symlink(other, filepath.Join(adir, "bridge.json")); err != nil {
		t.Fatal(err)
	}
	if m := got(); m["kind"] != DescribeKind {
		t.Fatalf("read through a link: %v", m)
	}
}

func TestParseDescribeParams(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{``, `null`, `{}`, `{"later":1}`} {
		if err := ParseDescribeParams(json.RawMessage(ok)); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{`[]`, `"x"`, `{`} {
		if err := ParseDescribeParams(json.RawMessage(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDescriptionErrorText(t *testing.T) {
	t.Parallel()
	cases := map[error]string{
		pipeline.ErrNoNotes:    "the Bridge has no notes for this book",
		pipeline.ErrOneChapter: "the book has a single chapter, so no description leaves out its ending",
		scheduler.ParkWithCode(state.ParkBudgetExceeded, "x"):           "the agent budget is spent",
		scheduler.ParkWithCode(state.ParkAgentCredentialRefused, "x"):   "the agent's credential was refused",
		scheduler.ParkWithCode(state.ParkAgentUnavailable, "x"):         "the agent isn't available",
		scheduler.ParkWithCode(state.ParkAgentValidationExhausted, "x"): "the description kept failing its checks",
		scheduler.ParkWithCode(state.ParkSupervisorEscalated, "x"):      "the agent stopped (supervisor_escalated)",
		context.Canceled:           "the job stopped while it was being written",
		errors.New("disk on fire"): "the description step failed (see the Bridge's log)",
	}
	for err, want := range cases {
		if got := descriptionErrorText(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}

// An admin cancels a parked generate job and asks for a description at once,
// before the job's next keepalive would tell the Bridge (BRIDGE-01): the
// description waits for the job's end and runs on the run's notes. Not
// cancelled, it is refused at once.
func TestDescribeRightAfterACancelRuns(t *testing.T) {
	t.Parallel()
	for name, cancel := range map[string]bool{"cancelled": true, "open": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hr := newHarness(t)
			d := &fakeDescriber{db: hr.db}
			h := hr.describeHandler(d)
			hr.exec.parks["contributing"] = 1
			hr.srv.add(301, "Author/Book", `{}`)
			defer hr.runnerKeepalive(time.Hour, h, h.DescribeJob())()
			// Parked and quiet: the park's description written and its last step
			// reported, so (at a one-hour keepalive) the job reports nothing more on
			// its own.
			hr.waitFor("the contributing park, settled", func() bool {
				hr.srv.mu.Lock()
				defer hr.srv.mu.Unlock()
				ups := hr.srv.updates[301]
				if len(ups) == 0 || len(d.calls()) == 0 {
					return false
				}
				var p map[string]any
				last := ups[len(ups)-1]
				_ = json.Unmarshal(last.Progress, &p)
				return *last.State == link.StateNeedsAttention && p["step"] == "contributing"
			})
			if cancel {
				hr.srv.set(func() { hr.srv.cancel[301] = true })
			}
			start := time.Now()
			hr.srv.addDescribe(302, "Author/Book")
			hr.waitFor("the description ended", func() bool { s, _ := hr.srv.final(302); return s != "" })
			s, u := hr.srv.final(302)
			switch {
			case cancel && s != link.StateDone:
				t.Fatalf("after a cancel the description ended %s (%q), want done", s, errOf(u))
			case !cancel && (s != link.StateFailed || errOf(u) != string(errDescribeGenerating)):
				t.Fatalf("with the job open the description ended %s (%q), want refused", s, errOf(u))
			case !cancel && time.Since(start) > 5*time.Second:
				t.Fatalf("the refusal took %v", time.Since(start))
			}
			if st, _ := hr.srv.final(301); cancel != (st == link.StateCancelled) {
				t.Fatalf("cancel %v: the generate job ended %q", cancel, st)
			}
		})
	}
}

// slowCancel is the engine with a Cancel that waits for gate to close (it says
// when the first one started on started).
type slowCancel struct {
	Engine
	once    *sync.Once
	started chan struct{}
	gate    chan struct{}
}

func (e slowCancel) Cancel(ctx context.Context, id int64) error {
	e.once.Do(func() { close(e.started) })
	<-e.gate
	return e.Engine.Cancel(ctx, id)
}

// A job's end is visible only once the engine's cancel of its book has returned:
// a description waiting on a just-cancelled job doesn't start while the engine is
// still cancelling the book.
func TestDescribeWaitsForTheEnginesCancel(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	d := &fakeDescriber{db: hr.db}
	eng := slowCancel{Engine: hr.sched, once: &sync.Once{}, started: make(chan struct{}), gate: make(chan struct{})}
	var opened sync.Once
	openGate := func() { opened.Do(func() { close(eng.gate) }) }
	h := New(Options{DB: hr.db, Engine: func() Engine { return eng }, Describer: func() Describer { return d },
		Libraries: hr.libs, Mount: func(int64, string) (string, bool) { return hr.libs.root, false },
		Server: func() Server { return hr.cl }, WorkRoot: hr.work, Version: "0.2.0", RetentionDays: 30,
		Poll: 10 * time.Millisecond, IdlePoll: 10 * time.Millisecond, Recheck: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	hr.exec.parks["contributing"] = 1
	hr.srv.add(311, "Author/Book", `{}`)
	defer hr.runnerKeepalive(time.Hour, h, h.DescribeJob())()
	defer openGate() // before the runner stops: a failure must not leave the cancel waiting
	// Parked and quiet (as in TestDescribeRightAfterACancelRuns): the cancel is
	// heard only through the description's Refresh.
	hr.waitFor("the contributing park, settled", func() bool {
		hr.srv.mu.Lock()
		defer hr.srv.mu.Unlock()
		ups := hr.srv.updates[311]
		if len(ups) == 0 || len(d.calls()) == 0 {
			return false
		}
		var p map[string]any
		last := ups[len(ups)-1]
		_ = json.Unmarshal(last.Progress, &p)
		return *last.State == link.StateNeedsAttention && p["step"] == "contributing"
	})
	calls := len(d.calls())
	hr.srv.set(func() { hr.srv.cancel[311] = true })
	hr.srv.addDescribe(312, "Author/Book")
	select {
	case <-eng.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the engine was never asked to cancel the book")
	}
	time.Sleep(300 * time.Millisecond)
	if s, u := hr.srv.final(312); s != "" || len(d.calls()) != calls {
		t.Fatalf("while the engine cancels the book: the description ended %q (%q), describe calls %d (was %d)",
			s, errOf(u), len(d.calls()), calls)
	}
	openGate()
	hr.waitFor("the description ended", func() bool { s, _ := hr.srv.final(312); return s != "" })
	if s, u := hr.srv.final(312); s != link.StateDone {
		t.Fatalf("the description ended %s (%q), want done", s, errOf(u))
	}
}
