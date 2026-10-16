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
// call (1-based).
type fakeDescriber struct {
	db      *store.DB
	mu      sync.Mutex
	reports []pipeline.DescribeReport
	fail    func(call int) error
}

func (f *fakeDescriber) Describe(ctx context.Context, bookID int64, rep pipeline.DescribeReport) (pipeline.DescribeResult, error) {
	f.mu.Lock()
	f.reports = append(f.reports, rep)
	call := len(f.reports)
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		if err := fail(call); err != nil {
			return pipeline.DescribeResult{CostUSD: 0.01}, err
		}
	}
	b, err := f.db.GetBook(ctx, bookID)
	if err != nil {
		return pipeline.DescribeResult{}, err
	}
	path := pipeline.FinalDescriptionPath(b.WorkDir)
	if err := os.WriteFile(path, []byte(testDescription), 0o600); err != nil {
		return pipeline.DescribeResult{}, err
	}
	return pipeline.DescribeResult{Path: path, Chapters: 2, LogicalChapters: 6, CheckedAgainst: pipeline.CheckedTranscripts,
		CostUSD: 0.25, GeneratedAt: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeDescriber) calls() []pipeline.DescribeReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pipeline.DescribeReport(nil), f.reports...)
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
	if calls := d.calls(); len(calls) != 1 || !calls[0].NoWait {
		t.Fatalf("describe calls = %+v, want one with NoWait", calls)
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
	d := &fakeDescriber{db: hr.db}
	hr.srv.addDescribe(42, "Author/Book")
	stop := hr.runAll(hr.describeHandler(d))
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(42); return s == link.StateDone })
	var res DescribeJobResult
	hr.result(42, &res)
	if res.LibraryID != 1 || res.Path != "Author/Book" || !res.Description || res.CostUSD != 0.25 || res.CheckedAgainst != "transcripts" ||
		strings.Join(res.Files, ",") != "description.json,bridge.json" {
		t.Fatalf("result = %+v", res)
	}
	if calls := d.calls(); len(calls) != 1 || calls[0].NoWait {
		t.Fatalf("describe calls = %+v", calls)
	}
	if !hr.srv.saw(42, func(_ link.Update, p map[string]any) bool {
		return p["stage"] == "describing" && p["step"] == "describing"
	}) {
		t.Fatal("the job never reported the describing stage")
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
	d := &fakeDescriber{db: hr.db, fail: func(call int) error {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case mode == "budget" && call == 1:
			return scheduler.ParkWithCode(state.ParkBudgetExceeded, "the budget is spent")
		case mode == "rate" && call == 3:
			return scheduler.ParkWithCodeAfter(state.ParkAgentRateLimited, "rate limited", time.Now().Add(50*time.Millisecond))
		case mode == "checks":
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
				park["code"] == "budget_exceeded" && park["hint"] != "" && park["retry_at"] == nil
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
	hr.waitFor("done at retry_at", func() bool { s, _ := hr.srv.final(63); return s == link.StateDone })
	if !hr.srv.saw(63, func(_ link.Update, p map[string]any) bool {
		park, _ := p["park"].(map[string]any)
		return park["code"] == "agent_rate_limited" && park["retry_at"] != nil
	}) {
		t.Fatal("the rate limit was never reported")
	}

	mu.Lock()
	mode = "checks"
	mu.Unlock()
	hr.srv.addDescribe(64, "Author/Book")
	hr.waitFor("failed", func() bool { s, _ := hr.srv.final(64); return s == link.StateFailed })
	if _, u := hr.srv.final(64); u.Error == nil || *u.Error != string(errDescribeChecks) {
		t.Fatalf("failed with %v", u.Error)
	}
}

// A new generate job doesn't delete the earlier run a description is reading.
func TestGenerateWaitsForADescription(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(71)
	b := hr.engineBook(71)
	hr.h.markDescribing(b.ID, 1)
	hr.srv.add(72, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("failed", func() bool { s, _ := hr.srv.final(72); return s == link.StateFailed })
	if _, u := hr.srv.final(72); u.Error == nil || !strings.Contains(*u.Error, "still writing a description") {
		t.Fatalf("failed with %v", u.Error)
	}
	if _, err := hr.db.GetBook(context.Background(), b.ID); err != nil {
		t.Fatalf("the earlier run was deleted: %v", err)
	}
	hr.h.markDescribing(b.ID, -1)
	if hr.h.isDescribing(b.ID) {
		t.Fatal("the count didn't drop")
	}
}

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
		pipeline.ErrNoNotes: "the Bridge has no notes for this book",
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
