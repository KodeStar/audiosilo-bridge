package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
)

// The jobs and attempts that come back: a server take-back and re-claim, a job
// id another server reuses, a job ended before its handler ran, a book whose
// transcripts went with an earlier end, and a park that survives a restart.

// runnerAs runs h's jobs as a Bridge paired with the server whose id is sid.
func (hr *harness) runnerAs(h *Handler, sid string) (stop func()) {
	r := jobs.New(hr.db, func() jobs.Server { return hr.cl }, nil, jobs.Options{Keepalive: 30 * time.Millisecond,
		ProgressEvery: 5 * time.Millisecond, Wait: 10 * time.Millisecond, RetryEvery: 20 * time.Millisecond,
		ServerID: func() string { return sid }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	return func() { cancel(); <-done }
}

// requeue hands job id (path) out again under attempt.
func (hr *harness) requeue(id int64, path string, attempt int) {
	hr.srv.set(func() {
		lib := int64(1)
		hr.srv.queue = append(hr.srv.queue, link.Job{ID: id, Kind: Kind, LibraryID: &lib, Path: &path,
			Params: json.RawMessage(`{}`), Attempts: attempt, State: "claimed"})
	})
}

// Denied: a job id another server reuses (both number their jobs from 1) is
// never taken for the earlier job's run, so one book's characters and recaps
// never land in another's folder.
func TestGenerateIgnoresAnotherJobsMapping(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.srv.add(21, "Author/Book", `{}`)
	stop := hr.runnerAs(hr.h, "server-a")
	hr.waitFor("server A's job done", func() bool { s, _ := hr.srv.final(21); return s == link.StateDone })
	stop()
	first := hr.engineBook(21)
	mustWrite(t, filepath.Join(first.WorkDir, "sidecars", "characters.json"),
		`{"work":"book-a","characters":[{"id":"only-in-book-a","name":"Only In Book A"}],"license":"CC-BY-SA-4.0","sources":[]}`)

	hr.srv.set(func() { hr.srv.updates[21] = nil })
	hr.srv.add(21, "Loose.m4b", `{}`)
	stop2 := hr.runnerAs(hr.handler(), "server-b")
	defer stop2()
	hr.waitFor("server B's job done", func() bool { s, _ := hr.srv.final(21); return s == link.StateDone })
	got, err := os.ReadFile(filepath.Join(hr.root, AttachDir, "Loose.characters.json"))
	if err != nil || strings.Contains(string(got), "only-in-book-a") {
		t.Fatalf("Loose.characters.json = %s (%v): another book's run was written", got, err)
	}
	if b := hr.engineBook(21); b.ID == first.ID || !strings.HasSuffix(b.SourcePath, "Loose.m4b") {
		t.Fatalf("the reused id followed the earlier book: %+v", b)
	}
}

// Allowed: the server takes a job back and hands the same job out again: the
// new attempt follows (re-admits) the same engine book.
func TestGenerateRequeueFollowsTheSameBook(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	gate := hr.exec.gate("asr")
	hr.srv.add(40, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("asr", func() bool {
		return hr.srv.saw(40, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	before := hr.engineBook(40)
	hr.srv.set(func() { hr.srv.notHeld[40] = true })
	hr.waitFor("the engine book stopped", func() bool { return scheduler.IsCancelled(hr.engineBook(40)) })
	close(gate)
	hr.srv.set(func() { hr.srv.notHeld[40], hr.srv.updates[40] = false, nil })
	hr.requeue(40, "Author/Book", 2)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(40); return s == link.StateDone })
	if after := hr.engineBook(40); after.ID != before.ID {
		t.Fatalf("the re-claimed job made a new engine book (%d, was %d)", after.ID, before.ID)
	}
	if g, _ := hr.db.GetGenerateBook(context.Background(), 40); g.EndedAt == "" {
		t.Fatal("the job's end wasn't recorded")
	}
}

// overlapServer holds job 40 under one attempt (another attempt's report is a
// 409) and can answer attempt 1's reports with a 503 (the server away).
type overlapServer struct {
	inner  *fakeServer
	mu     sync.Mutex
	held   int
	block1 bool
	got    []link.Update
}

func (v *overlapServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/bridge/jobs/40" {
		body, _ := io.ReadAll(r.Body)
		var u link.Update
		_ = json.Unmarshal(body, &u)
		v.mu.Lock()
		block, held := v.block1, v.held
		v.got = append(v.got, u)
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case u.Attempt == 1 && block:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"restarting"}`)
			return
		case held != 0 && u.Attempt != held:
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"not held","code":"not_held"}`)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	v.inner.ServeHTTP(w, r)
}

func (v *overlapServer) set(fn func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
	fn()
}

func (v *overlapServer) reports(attempt int, stage string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, u := range v.got {
		var p map[string]any
		_ = json.Unmarshal(u.Progress, &p)
		if u.Attempt == attempt && (stage == "" || p["stage"] == stage) {
			n++
		}
	}
	return n
}

// A later claim of a job can run here before the earlier attempt hears it was
// taken back (the server away longer than the lease, then the claim first): the
// earlier attempt's end leaves the book to the later one, which finishes it.
func TestGenerateLaterAttemptKeepsTheBook(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	v := &overlapServer{inner: hr.srv}
	hs := httptest.NewServer(v)
	t.Cleanup(hs.Close)
	cl, err := link.New(hs.URL, "tok", "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	hr.cl = cl
	gate := hr.exec.gate("asr")
	hr.srv.add(40, "Author/Book", `{}`)
	stop := hr.runner(hr.handler())
	defer stop()
	hr.waitFor("attempt 1 at asr", func() bool { return v.reports(1, "asr") > 0 })
	book := hr.engineBook(40)

	v.set(func() { v.block1 = true })
	n := v.reports(1, "")
	hr.waitFor("attempt 1's reports failing", func() bool { return v.reports(1, "") >= n+3 })
	v.set(func() { v.held = 2 })
	hr.requeue(40, "Author/Book", 2)
	hr.waitFor("attempt 2 at asr", func() bool { return v.reports(2, "asr") > 0 })
	n = v.reports(1, "")
	v.set(func() { v.block1 = false })
	hr.waitFor("attempt 1's 409", func() bool { return v.reports(1, "") > n })
	time.Sleep(100 * time.Millisecond)
	if b, _ := hr.db.GetBook(context.Background(), book.ID); scheduler.IsCancelled(b) {
		t.Fatalf("the earlier attempt stopped the book the later one follows: %+v", b)
	}
	close(gate)
	hr.waitFor("the job's end", func() bool { s, _ := hr.srv.final(40); return s != "" })
	if s, u := hr.srv.final(40); s != link.StateDone || u.Attempt != 2 {
		t.Fatalf("job 40 ended %s (attempt %d), want done (attempt 2)", s, u.Attempt)
	}
}

// A job the runner ends without running its handler (its first report after a
// restart answered with a cancel, or another server's job dropped at a start)
// stops the engine book an earlier run made and records the end.
func TestGenerateAbandonedJobStopsItsBook(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(hr *harness, id int64) (stop func()){
		"cancelled while the Bridge was down": func(hr *harness, id int64) func() {
			hr.srv.set(func() { hr.srv.cancel[id] = true })
			return hr.runner(hr.handler())
		},
		"lost while the Bridge was down": func(hr *harness, id int64) func() {
			hr.srv.set(func() { hr.srv.notHeld[id] = true })
			return hr.runner(hr.handler())
		},
		"another server's job": func(hr *harness, _ int64) func() {
			return hr.runnerAs(hr.handler(), "another-server")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hr := newHarness(t)
			hr.exec.gate("asr")
			hr.srv.add(42, "Author/Book", `{}`)
			stop := hr.runner(hr.h)
			hr.waitFor("asr", func() bool {
				return hr.srv.saw(42, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
			})
			stop()
			defer end(hr, 42)()
			// The end is recorded too (the retention sweep and the supervisor's
			// guard read it).
			hr.waitFor("the engine book stopped and the end recorded", func() bool {
				g, _ := hr.db.GetGenerateBook(context.Background(), 42)
				return scheduler.IsCancelled(hr.engineBook(42)) && g.EndedAt != ""
			})
		})
	}
}

// A resumed job cancelled while it waits for the engine to start (its first
// report went out, the next is answered with the cancel) never reaches its end:
// the engine book an earlier run made is still stopped and its end recorded once
// the engine is up, or a start would re-admit it with nothing to follow it.
func TestGenerateCancelledWhileWaitingForTheEngineStopsItsBook(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.gate("asr")
	hr.srv.add(43, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	hr.waitFor("asr", func() bool {
		return hr.srv.saw(43, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	stop()

	var up atomic.Bool // the engine is still starting
	defer hr.runner(hr.handlerWith(func() Engine {
		if up.Load() {
			return hr.sched
		}
		return nil
	}))()
	hr.waitFor("the resumed job waiting for the engine", func() bool {
		return hr.srv.saw(43, func(_ link.Update, p map[string]any) bool {
			return p["message"] == "Waiting for the engine to start"
		})
	})
	hr.srv.set(func() { hr.srv.cancel[43] = true })
	hr.waitFor("the job ended", func() bool { s, _ := hr.srv.final(43); return s != "" })
	up.Store(true)
	hr.waitFor("the engine book stopped and the end recorded", func() bool {
		g, _ := hr.db.GetGenerateBook(context.Background(), 43)
		return scheduler.IsCancelled(hr.engineBook(43)) && g.EndedAt != ""
	})
}

// With transcripts.retention_days 0 a job taken back deletes its book's
// transcripts as it ends; handed out again, the job starts the book over rather
// than re-admitting it onto inputs that are gone.
func TestGenerateSweptBookStartsOver(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	h := New(Options{DB: hr.db, Engine: func() Engine { return hr.sched }, Libraries: hr.libs,
		Mount:  func(int64, string) (string, bool) { return hr.libs.root, false },
		Server: func() Server { return hr.cl }, WorkRoot: hr.work, Version: "0.2.0", RetentionDays: 0,
		Poll: 10 * time.Millisecond, IdlePoll: 10 * time.Millisecond, Recheck: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	gate := hr.exec.gate("asr")
	hr.srv.add(43, "Author/Book", `{}`)
	stop := hr.runner(h)
	defer stop()
	hr.waitFor("asr", func() bool {
		return hr.srv.saw(43, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	before := hr.engineBook(43)
	hr.srv.set(func() { hr.srv.notHeld[43] = true })
	hr.waitFor("the transcripts swept", func() bool {
		g, _ := hr.db.GetGenerateBook(context.Background(), 43)
		return g.SweptAt != ""
	})
	close(gate)
	hr.srv.set(func() { hr.srv.notHeld[43], hr.srv.updates[43] = false, nil })
	hr.requeue(43, "Author/Book", 2)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(43); return s == link.StateDone })
	after := hr.engineBook(43)
	if after.ID == before.ID {
		t.Fatal("the swept book was re-admitted")
	}
	if g, _ := hr.db.GetGenerateBook(context.Background(), 43); g.EndedAt == "" || *g.BookID != after.ID {
		t.Fatalf("mapping = %+v", g)
	}
}

// A job parked when the Bridge stopped stays parked through the restart: no
// report says running before the engine book's park is reported again (the
// server would hear a new park and tell an admin again).
func TestGenerateResumedParkNeverReportsRunning(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.mu.Lock()
	hr.exec.parks["fact_pass"] = 100
	hr.exec.mu.Unlock()
	hr.srv.add(44, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	hr.waitFor("parked", func() bool {
		return hr.srv.saw(44, func(u link.Update, _ map[string]any) bool {
			return u.State != nil && *u.State == link.StateNeedsAttention
		})
	})
	stop()
	hr.srv.set(func() { hr.srv.updates[44] = nil })
	stop2 := hr.runner(hr.handler())
	defer stop2()
	hr.waitFor("a few reports after the restart", func() bool {
		hr.srv.mu.Lock()
		defer hr.srv.mu.Unlock()
		return len(hr.srv.updates[44]) >= 5
	})
	hr.srv.mu.Lock()
	defer hr.srv.mu.Unlock()
	for i, u := range hr.srv.updates[44] {
		if u.State == nil || *u.State != link.StateNeedsAttention {
			t.Fatalf("report %d after the restart = %+v, want needs_attention throughout", i, u)
		}
	}
}
