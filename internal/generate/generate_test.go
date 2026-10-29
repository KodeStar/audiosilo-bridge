package generate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/events"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// fakeServer implements the server's Bridge routes the job calls: the claim,
// job updates (with a cancel or a 409 on demand), the book lookup with its book
// block, and the attachments rescan.
type fakeServer struct {
	mu      sync.Mutex
	queue   []link.Job
	updates map[int64][]link.Update
	cancel  map[int64]bool
	notHeld map[int64]bool
	books   map[string]link.Book
	rescans []map[string]any
	// paths is a job's path in the answers to its reports: the server moved the
	// job with its book.
	paths map[int64]string
}

func newFakeServer() *fakeServer {
	return &fakeServer{updates: map[int64][]link.Update{}, cancel: map[int64]bool{}, notHeld: map[int64]bool{}, books: map[string]link.Book{}}
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/v1/bridge/jobs/next":
		kinds := r.URL.Query()["kind"]
		for i, j := range f.queue {
			if slices.Contains(kinds, j.Kind) {
				f.queue = append(f.queue[:i], f.queue[i+1:]...)
				_ = json.NewEncoder(w).Encode(j)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/v1/bridge/jobs/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v1/bridge/jobs/"), 10, 64)
		var u link.Update
		_ = json.NewDecoder(r.Body).Decode(&u)
		f.updates[id] = append(f.updates[id], u)
		if f.notHeld[id] {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"not held","code":"not_held"}`)
			return
		}
		j := link.Job{ID: id, State: *u.State, CancelRequested: f.cancel[id]}
		if p, ok := f.paths[id]; ok {
			j.Path = &p
		}
		_ = json.NewEncoder(w).Encode(j)
	case r.URL.Path == "/api/v1/bridge/libraries/1/book":
		b, ok := f.books[r.URL.Query().Get("path")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no indexed book there","code":"no_book"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(b)
	case r.URL.Path == "/api/v1/bridge/rescan":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.rescans = append(f.rescans, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"path": body["path"], "attachments": map[string]bool{"characters": true, "recaps": true, "description": true, "chapters": true, "bridge": true}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeServer) add(id int64, path string, params string) { f.addKind(id, Kind, path, params) }

func (f *fakeServer) addKind(id int64, kind, path string, params string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lib := int64(1)
	f.queue = append(f.queue, link.Job{ID: id, Kind: kind, LibraryID: &lib, Path: &path, Params: json.RawMessage(params), Attempts: 1, State: "claimed"})
}

// final is the last non-running state reported for id ("" for none).
func (f *fakeServer) final(id int64) (string, link.Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ups := f.updates[id]
	for i := len(ups) - 1; i >= 0; i-- {
		if s := ups[i].State; s != nil && *s != link.StateRunning && *s != link.StateNeedsAttention {
			return *s, ups[i]
		}
	}
	return "", link.Update{}
}

// saw reports whether any update for id matches.
func (f *fakeServer) saw(id int64, match func(link.Update, map[string]any) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.updates[id] {
		var p map[string]any
		_ = json.Unmarshal(u.Progress, &p)
		if match(u, p) {
			return true
		}
	}
	return false
}

func (f *fakeServer) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// libraries is a fake registry: library 1 at root, checked mapped and writable.
type libraries struct {
	root     string
	writable atomic.Bool
}

func (l *libraries) Library(id int64) (link.Library, bool) {
	return link.Library{ID: 1, Name: "Books", Root: l.root}, id == 1
}

func (l *libraries) Status(id int64) (link.LibraryStatus, bool) {
	return link.LibraryStatus{LibraryID: 1, MountPath: l.root, Mapped: true, Writable: l.writable.Load()}, id == 1
}

// hooks is a test executor: the scheduler's stub with a hook per stage, and the
// validating stage leaving sidecars in the work dir as the real one does.
type hooks struct {
	t     *testing.T
	stub  *scheduler.StubExecutor
	db    *store.DB
	mu    sync.Mutex
	parks map[string]int // stage -> how many times to park there
	block map[string]chan struct{}
	// estimated, when set, is the fact pass's spend as a codex run records it: a
	// complete estimate from the pricing table, no reported cost.
	estimated float64
	// windowed makes asr report a windows transcription (Phase 1d), so
	// chapter_detecting runs and leaves a detected list (detectChapters);
	// inspectErr fails inspect with that sentence.
	windowed   bool
	inspectErr error
}

func (e *hooks) Execute(ctx context.Context, b store.Book, st state.State, r scheduler.StageReport) (scheduler.StageResult, error) {
	e.mu.Lock()
	gate := e.block[string(st)]
	park := e.parks[string(st)] > 0
	if park {
		e.parks[string(st)]--
	}
	e.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return scheduler.StageResult{}, ctx.Err()
		}
	}
	if park {
		if st == state.Contributing {
			return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkCoreNeeded, "the book needs a core record first")
		}
		if st == state.ChapterDetecting {
			return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkChaptersNotDetected, "couldn't find the chapters")
		}
		if st == state.ASR {
			return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkASRWorkerWaiting, "speech recognition waits for an ASR worker")
		}
		return scheduler.StageResult{}, scheduler.ParkWithCode(state.ParkAgentValidationExhausted, "the fact pass kept failing its checks")
	}
	e.mu.Lock()
	windowed, inspectErr := e.windowed, e.inspectErr
	e.mu.Unlock()
	switch {
	case st == state.Inspecting && inspectErr != nil:
		return scheduler.StageResult{}, inspectErr
	case st == state.Inspecting:
		// probe.json as inspect leaves it for a single file: its size, written
		// after the file was (the detected list's continue check reads both).
		if info, err := os.Stat(b.SourcePath); err == nil && info.Mode().IsRegular() && os.MkdirAll(b.WorkDir, 0o750) == nil {
			_ = os.WriteFile(filepath.Join(b.WorkDir, audio.ProbeName),
				[]byte(fmt.Sprintf(`{"format":{"duration":"30","size":"%d"}}`, info.Size())), 0o600)
		}
	case st == state.ASR && windowed:
		res := scheduler.StageResult{Windowed: true}
		return res, scheduler.WriteSentinel(b.WorkDir, string(st), res)
	case st == state.ChapterDetecting:
		detectChapters(e.t, b)
	}
	switch st {
	case state.FactPass:
		// Spend, as an agent stage records it on its open run.
		e.mu.Lock()
		estimated := e.estimated
		e.mu.Unlock()
		if estimated > 0 {
			_ = e.db.AddOpenStageRunUsageDetailed(ctx, b.ID, string(st), "gpt-5", 100, 100, 0, 0, false, estimated, true)
		} else {
			_ = e.db.AddOpenStageRunUsage(ctx, b.ID, string(st), "opus", 100, 100, 3.5)
		}
	case state.Validating:
		dir := filepath.Join(b.WorkDir, "sidecars")
		_ = os.MkdirAll(dir, 0o750)
		_ = os.WriteFile(filepath.Join(dir, "characters.json"),
			[]byte(`{"work":"the-book","characters":[{"id":"kaladin","name":"Kaladin"},{"id":"shallan","name":"Shallan"}],"license":"CC-BY-SA-4.0","sources":[]}`), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "recaps.json"),
			[]byte(`{"work":"the-book","recaps":[{"through":{"chapter":1},"text":"It begins."}],"license":"CC-BY-SA-4.0","sources":[]}`), 0o600)
	}
	return e.stub.Execute(ctx, b, st, r)
}

func (e *hooks) gate(stage string) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := make(chan struct{})
	e.block[stage] = c
	return c
}

// harness is a Bridge with the engine wired to a test executor and a library
// with one folder book and one single-file book.
type harness struct {
	t     *testing.T
	db    *store.DB
	srv   *fakeServer
	cl    *link.Client
	libs  *libraries
	exec  *hooks
	sched *scheduler.Scheduler
	root  string
	work  string
	h     *Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	mustWrite(t, filepath.Join(root, "Author", "Book", "01.mp3"), "audio-one")
	mustWrite(t, filepath.Join(root, "Author", "Book", "02.mp3"), "audio-two!")
	mustWrite(t, filepath.Join(root, "Loose.m4b"), "single")
	db, err := store.Open(context.Background(), filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := newFakeServer()
	srv.books["Author/Book"] = link.Book{LibraryID: 1, Path: "Author/Book", Files: []link.BookFile{
		{Path: "Author/Book/01.mp3", Size: 9, ModTime: 1700000000}, {Path: "Author/Book/02.mp3", Size: 10, ModTime: 1700000001}},
		Info: &link.BookInfo{Title: "The Book", Authors: []string{"An Author"}, Narrators: []string{"A Reader"}, Series: "The Saga",
			SeriesIndex: "2", ASIN: "B000", ISBN: "978", Duration: 7200, WorkID: "the-book"}}
	srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{{Path: "Loose.m4b", Size: 6}},
		Info: &link.BookInfo{Title: "Loose"}}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	cl, err := link.New(hs.URL, "tok", "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	libs := &libraries{root: root}
	libs.writable.Store(true)
	exec := &hooks{t: t, stub: scheduler.NewStubExecutor(time.Millisecond, 2*time.Millisecond), db: db, parks: map[string]int{}, block: map[string]chan struct{}{}}
	work := filepath.Join(dir, "work")
	sched := scheduler.New(db, events.NewHub(256), exec, 2, work, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = sched.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	hr := &harness{t: t, db: db, srv: srv, cl: cl, libs: libs, exec: exec, sched: sched, root: root, work: work}
	hr.h = hr.handler()
	return hr
}

func (hr *harness) handler() *Handler {
	return hr.handlerWith(func() Engine { return hr.sched })
}

// handlerWith is handler with the engine engine gives (nil while it starts).
func (hr *harness) handlerWith(engine func() Engine) *Handler {
	return New(Options{DB: hr.db, Engine: engine, Libraries: hr.libs,
		Mount:  func(int64, string) (string, bool) { return hr.libs.root, false },
		Server: func() Server { return hr.cl }, WorkRoot: hr.work, Version: "0.2.0", RetentionDays: 30,
		Poll: 10 * time.Millisecond, IdlePoll: 10 * time.Millisecond, Recheck: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// runner runs h's jobs (and more's) until stopped (the returned func), as the
// Bridge would.
func (hr *harness) runner(h *Handler, more ...jobs.Handler) (stop func()) {
	return hr.runnerOf(append([]jobs.Handler{h}, more...)...)
}

// runnerOf runs the handlers' jobs until stopped (the returned func): it claims
// only their kinds.
func (hr *harness) runnerOf(handlers ...jobs.Handler) (stop func()) {
	r := jobs.New(hr.db, func() jobs.Server { return hr.cl }, nil, jobs.Options{Keepalive: 30 * time.Millisecond,
		ProgressEvery: 5 * time.Millisecond, Wait: 10 * time.Millisecond, RetryEvery: 20 * time.Millisecond,
		ServerID: func() string { return "srv" }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, handlers...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	return func() { cancel(); <-done }
}

func (hr *harness) waitFor(what string, cond func() bool) {
	hr.t.Helper()
	if !poll(cond) {
		hr.t.Fatalf("timed out waiting for %s", what)
	}
}

// poll waits up to 10 s for cond and reports whether it held. It never fails the
// test, so it can run off the test's goroutine (in a fake a job calls).
func poll(cond func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// engineBook is the engine book job id maps to.
func (hr *harness) engineBook(id int64) store.Book {
	hr.t.Helper()
	m, err := hr.db.GetGenerateBook(context.Background(), id)
	if err != nil || m.BookID == nil {
		hr.t.Fatalf("job %d has no engine book: %+v, %v", id, m, err)
	}
	b, err := hr.db.GetBook(context.Background(), *m.BookID)
	if err != nil {
		hr.t.Fatal(err)
	}
	return b
}

// The whole loop for a folder book: the engine book gets the server's identity,
// progress reaches the server with the stage, ETA and cost, the attachments land
// in the book's .audiosilo/ and the server is asked to re-read them.
func TestGenerateFolderBook(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	asr := hr.exec.gate("asr")
	hr.srv.add(21, "Author/Book", `{"source":"auto","contribute":"local"}`)
	stop := hr.runner(hr.h)
	defer stop()

	hr.waitFor("the job to reach asr with an ETA", func() bool {
		return hr.srv.saw(21, func(u link.Update, p map[string]any) bool {
			_, eta := p["eta_seconds"]
			return p["stage"] == "asr" && p["step"] == "asr" && eta && p["source"] == "audio"
		})
	})
	b := hr.engineBook(21)
	want := filepath.Join(hr.root, "Author", "Book")
	if real, err := filepath.EvalSymlinks(want); err == nil {
		want = real
	}
	if b.SourcePath != want || b.Title != "The Book" || b.Authors[0] != "An Author" || b.Narrators[0] != "A Reader" ||
		b.Series != "The Saga" || b.SeriesPos != "2" || b.ASIN != "B000" || b.ISBN != "978" || b.WorkID != "the-book" ||
		b.Kind != "audio" || b.ContribMode != "local" || b.IdentitySources["title"] != "server" {
		t.Fatalf("engine book = %+v", b)
	}
	close(asr)

	hr.waitFor("done", func() bool { s, _ := hr.srv.final(21); return s == link.StateDone })
	if !hr.srv.saw(21, func(u link.Update, p map[string]any) bool { c, _ := p["cost_usd"].(float64); return c == 3.5 }) {
		t.Fatal("the spend never reached the server")
	}
	_, final := hr.srv.final(21)
	var res Result
	if err := json.Unmarshal(final.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.LibraryID != 1 || res.Path != "Author/Book" || res.Source != "audio" || res.Characters != 2 || res.Recaps != 1 ||
		res.CostUSD != 3.5 || res.WorkID != "the-book" || strings.Join(res.Files, ",") != "characters.json,recaps.json,bridge.json" ||
		res.Contributed != nil {
		t.Fatalf("result = %s", final.Result)
	}
	adir := filepath.Join(hr.root, "Author", "Book", AttachDir)
	chars, err := os.ReadFile(filepath.Join(adir, "characters.json"))
	if err != nil || !strings.Contains(string(chars), "Kaladin") {
		t.Fatalf("characters.json = %s, %v", chars, err)
	}
	var prov Provenance
	raw, _ := os.ReadFile(filepath.Join(adir, "bridge.json"))
	if err := json.Unmarshal(raw, &prov); err != nil || prov.JobID != 21 || prov.Source != "audio" || prov.WorkID != "the-book" ||
		len(prov.Inputs) != 2 || prov.Inputs[1].Path != "Author/Book/02.mp3" || prov.Inputs[1].Size != 10 {
		t.Fatalf("bridge.json = %s, %v", raw, err)
	}
	hr.srv.mu.Lock()
	defer hr.srv.mu.Unlock()
	if len(hr.srv.rescans) != 1 || hr.srv.rescans[0]["mode"] != "attachments" || hr.srv.rescans[0]["path"] != "Author/Book" {
		t.Fatalf("rescans = %+v", hr.srv.rescans)
	}
	if g, _ := hr.db.GetGenerateBook(context.Background(), 21); g.EndedAt == "" {
		t.Fatal("the job's end wasn't recorded")
	}
}

// A single-file book's attachments sit beside it under its stem.
func TestGenerateSingleFileBook(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.srv.add(22, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(22); return s == link.StateDone })
	for _, n := range []string{"Loose.characters.json", "Loose.recaps.json", "Loose.bridge.json"} {
		if _, err := os.Stat(filepath.Join(hr.root, AttachDir, n)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if b := hr.engineBook(22); !strings.HasSuffix(b.SourcePath, "Loose.m4b") || b.Title != "Loose" {
		t.Fatalf("engine book = %+v", b)
	}
}

// A park reports needs_attention with the park's code and hint, a Retry from
// the Bridge's page re-admits the book, and the job reports running again.
func TestGenerateParkAndRetry(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.parks["fact_pass"] = 1
	hr.srv.add(23, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("the park", func() bool {
		return hr.srv.saw(23, func(u link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return u.State != nil && *u.State == link.StateNeedsAttention && u.Error != nil &&
				*u.Error == "the fact pass kept failing its checks" && park["code"] == "agent_validation_exhausted" && park["hint"] != ""
		})
	})
	if sj, _ := hr.db.GetServerJob(context.Background(), 23); sj.State != store.JobRunning || sj.Error == "" {
		t.Fatalf("stored while parked = %+v", sj)
	}
	// The engine holds the book at the next stage until the server has heard the
	// job running again: the runner sends only a job's latest progress, so a run
	// straight to done could replace it.
	synth := hr.exec.gate("synthesizing")
	if err := hr.h.Retry(context.Background(), 23); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("running again after the retry", func() bool {
		hr.srv.mu.Lock()
		defer hr.srv.mu.Unlock()
		parked := false
		for _, u := range hr.srv.updates[23] {
			if u.State != nil && *u.State == link.StateNeedsAttention {
				parked = true
			}
			if parked && u.State != nil && *u.State == link.StateRunning && u.Error != nil && *u.Error == "" {
				return true
			}
		}
		return false
	})
	close(synth)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(23); return s == link.StateDone })
	if err := hr.h.Retry(context.Background(), 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a retry of an unknown job = %v", err)
	}
}

// An admin's cancel (the server answers cancel_requested) cancels the engine
// book; the job reports cancelled.
func TestGenerateCancel(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.gate("asr")
	hr.srv.add(24, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("asr", func() bool {
		return hr.srv.saw(24, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	hr.srv.set(func() { hr.srv.cancel[24] = true })
	hr.waitFor("cancelled", func() bool { s, _ := hr.srv.final(24); return s == link.StateCancelled })
	hr.waitFor("the engine book cancelled", func() bool {
		b := hr.engineBook(24)
		return scheduler.IsCancelled(b)
	})
}

// A job running when the Bridge stops leaves its engine book alone and, resumed
// at the next start, follows the same book to the end.
func TestGenerateResume(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	gate := hr.exec.gate("asr")
	hr.srv.add(25, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	hr.waitFor("asr", func() bool {
		return hr.srv.saw(25, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	before := hr.engineBook(25)
	stop()
	if b := hr.engineBook(25); b.Status != "" {
		t.Fatalf("the Bridge's stop touched the engine book: %+v", b)
	}
	if sj, _ := hr.db.GetServerJob(context.Background(), 25); sj.State != store.JobRunning {
		t.Fatalf("the stopped job = %+v", sj)
	}
	close(gate)
	stop2 := hr.runner(hr.handler())
	defer stop2()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(25); return s == link.StateDone })
	if after := hr.engineBook(25); after.ID != before.ID {
		t.Fatalf("the resumed job made a new engine book (%d, was %d)", after.ID, before.ID)
	}
	books, _ := hr.db.ListBooks(context.Background())
	if len(books) != 1 {
		t.Fatalf("engine books = %d, want 1", len(books))
	}
}

// A book folder the Bridge can't write parks book_not_writable before the
// engine gets the book; once it can, a Retry goes on.
func TestGenerateBookNotWritable(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes through 0555")
	}
	hr := newHarness(t)
	book := filepath.Join(hr.root, "Author", "Book")
	if err := os.Chmod(book, 0o555); err != nil { //nolint:gosec // the test's own folder
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(book, 0o755) }) //nolint:gosec // as above
	hr.srv.add(26, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("the park", func() bool {
		return hr.srv.saw(26, func(u link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return u.State != nil && *u.State == link.StateNeedsAttention && park["code"] == "book_not_writable"
		})
	})
	if _, err := hr.db.GetGenerateBook(context.Background(), 26); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the engine got a book whose folder can't be written")
	}
	if err := os.Chmod(book, 0o755); err != nil { //nolint:gosec // as above
		t.Fatal(err)
	}
	if err := hr.h.Retry(context.Background(), 26); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(26); return s == link.StateDone })
}

// A library that isn't mapped and writable fails a new job, saying so.
func TestGenerateLibraryNotWritable(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.libs.writable.Store(false)
	hr.srv.add(27, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("failed", func() bool { s, _ := hr.srv.final(27); return s == link.StateFailed })
	if _, final := hr.srv.final(27); final.Error == nil || !strings.Contains(*final.Error, "read-only") {
		t.Fatalf("error = %v", final.Error)
	}
}

// A regenerate of a finished book deletes its earlier engine book first; a job
// for a book another open job follows is refused.
func TestGenerateAgain(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.srv.add(28, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("the first run", func() bool { s, _ := hr.srv.final(28); return s == link.StateDone })
	first := hr.engineBook(28)

	gate := hr.exec.gate("asr")
	hr.srv.add(29, "Author/Book", `{"contribute":"issue"}`)
	hr.waitFor("the second run at asr", func() bool {
		return hr.srv.saw(29, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	second := hr.engineBook(29)
	if second.ID == first.ID || second.ContribMode != "issue" {
		t.Fatalf("second engine book = %+v (first %d)", second, first.ID)
	}
	if g, _ := hr.db.GetGenerateBook(context.Background(), 28); g.BookID != nil {
		t.Fatal("the first job still maps to a deleted book")
	}

	hr.srv.add(30, "Author/Book", `{}`)
	hr.waitFor("the third refused", func() bool { s, _ := hr.srv.final(30); return s == link.StateFailed })
	if _, final := hr.srv.final(30); final.Error == nil || !strings.Contains(*final.Error, "job 29 is already generating") {
		t.Fatalf("error = %v", final.Error)
	}
	close(gate)
	hr.waitFor("the second run", func() bool { s, _ := hr.srv.final(29); return s == link.StateDone })
}

// An ebook beside the audio is read instead (source auto), and bridge.json
// names it as the input.
func TestGenerateEbook(t *testing.T) {
	old := readEpub
	readEpub = func(string) error { return nil }
	t.Cleanup(func() { readEpub = old })
	hr := newHarness(t)
	mustWrite(t, filepath.Join(hr.root, "Author", "Book", "The Book.epub"), "epub!")
	hr.srv.add(31, "Author/Book", `{"source":"auto"}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(31); return s == link.StateDone })
	b := hr.engineBook(31)
	if b.Kind != "ebook" || !strings.HasSuffix(b.EbookPath, "The Book.epub") || strings.HasSuffix(b.SourcePath, ".epub") {
		t.Fatalf("engine book = %+v", b)
	}
	var prov Provenance
	raw, _ := os.ReadFile(filepath.Join(hr.root, "Author", "Book", AttachDir, "bridge.json"))
	if err := json.Unmarshal(raw, &prov); err != nil || prov.Source != "ebook" || len(prov.Inputs) != 1 ||
		prov.Inputs[0].Path != "Author/Book/The Book.epub" || prov.Inputs[0].Size != 5 {
		t.Fatalf("bridge.json = %s, %v", raw, err)
	}
}

// contribute=issue on a book the community database lacks parks core_needed at
// contributing; the local files are written (and re-read) all the same.
func TestGenerateCoreNeededStillWritesLocally(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.parks["contributing"] = 1
	hr.srv.add(32, "Author/Book", `{"contribute":"issue"}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("the core_needed park", func() bool {
		return hr.srv.saw(32, func(u link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return u.State != nil && *u.State == link.StateNeedsAttention && park["code"] == "core_needed"
		})
	})
	hr.waitFor("the local files", func() bool {
		_, err := os.Stat(filepath.Join(hr.root, "Author", "Book", AttachDir, "recaps.json"))
		hr.srv.mu.Lock()
		defer hr.srv.mu.Unlock()
		return err == nil && len(hr.srv.rescans) == 1
	})
	if s, _ := hr.srv.final(32); s != "" {
		t.Fatalf("a parked job finished: %s", s)
	}
	if b := hr.engineBook(32); b.ContribMode != "issue" {
		t.Fatalf("engine book mode = %q", b.ContribMode)
	}
}

// An earlier volume whose job ended (here an admin's cancel) no longer holds its
// series' lock: the next volume's job runs to the end, without that volume's
// carryover, instead of waiting at its first carryover stage for ever.
func TestGenerateSeriesGoesOnPastAnEndedVolume(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	mustWrite(t, filepath.Join(hr.root, "Author", "Book1", "01.mp3"), "vol-one")
	hr.srv.set(func() {
		hr.srv.books["Author/Book1"] = link.Book{LibraryID: 1, Path: "Author/Book1",
			Files: []link.BookFile{{Path: "Author/Book1/01.mp3", Size: 7, ModTime: 1700000000}},
			Info:  &link.BookInfo{Title: "Volume One", Series: "The Saga", SeriesIndex: "1"}}
	})
	asr := hr.exec.gate("asr")
	hr.srv.add(81, "Author/Book1", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("volume 1 at asr", func() bool {
		return hr.srv.saw(81, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	hr.srv.set(func() { hr.srv.cancel[81] = true })
	hr.waitFor("volume 1 cancelled", func() bool { s, _ := hr.srv.final(81); return s == link.StateCancelled })
	hr.waitFor("its engine book cancelled", func() bool { return scheduler.IsCancelled(hr.engineBook(81)) })
	close(asr)

	hr.srv.add(82, "Author/Book", `{}`) // The Saga #2
	hr.waitFor("volume 2 done", func() bool { s, _ := hr.srv.final(82); return s == link.StateDone })
}

// A .audiosilo/ the Bridge can't write at the end (another user's, while the
// book's folder itself is writable) parks the job book_not_writable instead of
// failing it after the whole run; once it can, a Retry writes the files.
func TestGenerateAttachDirNotWritableParks(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes through 0555")
	}
	hr := newHarness(t)
	adir := filepath.Join(hr.root, "Author", "Book", AttachDir)
	if err := os.Mkdir(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(adir, 0o555); err != nil { //nolint:gosec // the test's own folder
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(adir, 0o755) }) //nolint:gosec // as above
	hr.srv.add(91, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("the park", func() bool {
		return hr.srv.saw(91, func(u link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return u.State != nil && *u.State == link.StateNeedsAttention && park["code"] == "book_not_writable" &&
				u.Error != nil && strings.Contains(*u.Error, ".audiosilo folder can't be written")
		})
	})
	if s, _ := hr.srv.final(91); s != "" {
		t.Fatalf("the job ended %s instead of waiting", s)
	}
	if b := hr.engineBook(91); b.State != string(state.Done) {
		t.Fatalf("engine book = %s", b.State)
	}
	if err := os.Chmod(adir, 0o755); err != nil { //nolint:gosec // as above
		t.Fatal(err)
	}
	if err := hr.h.Retry(context.Background(), 91); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(91); return s == link.StateDone })
	if _, err := os.Stat(filepath.Join(adir, "characters.json")); err != nil {
		t.Fatal(err)
	}
	if _, u := hr.srv.final(91); u.Error == nil || *u.Error != "" {
		t.Fatalf("the done report kept the park's error: %v", u.Error)
	}
}

// A book the server moved mid-run (it carries the open job to the new path, which
// the answers to the job's reports say) gets its attachments where it is now, and
// the result and the rescan name the new path; before, the write failed at the end.
func TestGenerateFollowsAMovedBook(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	gate := hr.exec.gate("auditing")
	hr.srv.add(92, "Author/Book", `{}`)
	// The write may come before a report's answer has carried the new path: the
	// book isn't at the old one, so the job parks and looks again (every minute in
	// earnest).
	h := hr.handler()
	h.o.Recheck = 20 * time.Millisecond
	stop := hr.runner(h)
	defer stop()
	hr.waitFor("auditing", func() bool {
		return hr.srv.saw(92, func(_ link.Update, p map[string]any) bool { return p["stage"] == "auditing" })
	})
	if err := os.Rename(filepath.Join(hr.root, "Author", "Book"), filepath.Join(hr.root, "Author", "Book (2026)")); err != nil {
		t.Fatal(err)
	}
	hr.srv.set(func() {
		hr.srv.books["Author/Book (2026)"] = link.Book{LibraryID: 1, Path: "Author/Book (2026)", Files: []link.BookFile{
			{Path: "Author/Book (2026)/01.mp3", Size: 9}, {Path: "Author/Book (2026)/02.mp3", Size: 10}}}
		delete(hr.srv.books, "Author/Book")
		hr.srv.paths = map[int64]string{92: "Author/Book (2026)"}
	})
	close(gate)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(92); return s == link.StateDone })
	_, final := hr.srv.final(92)
	var res Result
	if err := json.Unmarshal(final.Result, &res); err != nil || res.Path != "Author/Book (2026)" {
		t.Fatalf("result = %s, %v", final.Result, err)
	}
	moved := filepath.Join(hr.root, "Author", "Book (2026)", AttachDir)
	var prov Provenance
	raw, err := os.ReadFile(filepath.Join(moved, "bridge.json"))
	if err != nil || json.Unmarshal(raw, &prov) != nil || prov.Inputs[0].Path != "Author/Book (2026)/01.mp3" {
		t.Fatalf("bridge.json at the new path = %s, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(moved, "characters.json")); err != nil {
		t.Fatal(err)
	}
	hr.srv.mu.Lock()
	rescans := slices.Clone(hr.srv.rescans)
	hr.srv.mu.Unlock()
	if len(rescans) != 1 || rescans[0]["path"] != "Author/Book (2026)" {
		t.Fatalf("rescans = %+v", rescans)
	}
	if g, _ := hr.db.GetGenerateBook(context.Background(), 92); g.RelPath != "Author/Book (2026)" {
		t.Fatalf("mapping = %+v, want the new path", g)
	}
}

// The job's spend is on the budgets' basis: a run that reports no cost but a
// complete estimate (codex priced from the table) counts its estimate, as the
// per-book and monthly caps do; before, the job reported $0.
func TestGenerateCostIsTheBudgetBasis(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.mu.Lock()
	hr.exec.estimated = 2.25
	hr.exec.mu.Unlock()
	hr.srv.add(93, "Author/Book", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(93); return s == link.StateDone })
	_, final := hr.srv.final(93)
	var res Result
	if err := json.Unmarshal(final.Result, &res); err != nil || res.CostUSD != 2.25 {
		t.Fatalf("result = %s, %v; want cost_usd 2.25", final.Result, err)
	}
}

// contribute=issue on a Bridge with no GitHub token fails the job at once, before
// the engine gets the book, instead of parking after the whole spend; with a
// token it runs.
func TestGenerateIssueNeedsAGitHubToken(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	var cred atomic.Pointer[error]
	setCred := func(err error) { cred.Store(&err) }
	setCred(contrib.ErrNoCredential)
	h := hr.handler()
	h.o.ContribCredential = func(context.Context) error { return *cred.Load() }
	hr.srv.add(94, "Author/Book", `{"contribute":"issue"}`)
	stop := hr.runner(h)
	defer stop()
	hr.waitFor("failed", func() bool { s, _ := hr.srv.final(94); return s == link.StateFailed })
	if _, final := hr.srv.final(94); final.Error == nil || *final.Error != errCantContribute.Error() {
		t.Fatalf("error = %v", final.Error)
	}
	if _, err := hr.db.GetGenerateBook(context.Background(), 94); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the engine got a book it couldn't contribute")
	}
	// Signed in, but GitHub couldn't be reached to refresh the sign-in: worded apart.
	setCred(contrib.ErrSignInUnrefreshed)
	hr.srv.add(96, "Author/Book", `{"contribute":"issue"}`)
	hr.waitFor("failed", func() bool { s, _ := hr.srv.final(96); return s == link.StateFailed })
	if _, final := hr.srv.final(96); final.Error == nil || *final.Error != errCantContributeUnrefreshed.Error() {
		t.Fatalf("error = %v", final.Error)
	}
	setCred(nil)
	hr.srv.add(95, "Loose.m4b", `{"contribute":"issue"}`)
	hr.waitFor("the second job runs", func() bool {
		_, err := hr.db.GetGenerateBook(context.Background(), 95)
		return err == nil
	})
}
