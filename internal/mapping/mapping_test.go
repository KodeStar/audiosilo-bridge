package mapping

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// Security: SafeJoin keeps every server path inside the mount (allowed: plain
// relative paths; denied: anything that could leave it).
func TestSafeJoin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for rel, want := range map[string]string{
		"Author/Book/1.mp3": filepath.Join(root, "Author", "Book", "1.mp3"),
		"Book.m4b":          filepath.Join(root, "Book.m4b"),
		"A/./B":             filepath.Join(root, "A", "B"),
		"":                  root,
		"..hidden/x":        filepath.Join(root, "..hidden", "x"),
		`AC\DC/Back`:        filepath.Join(root, `AC\DC`, "Back"),
	} {
		got, err := SafeJoin(root, rel)
		if err != nil || got != want {
			t.Errorf("allowed: SafeJoin(%q) = %q, %v; want %q", rel, got, err, want)
		}
	}
	for _, rel := range []string{"../x", "A/../../x", "/etc/passwd", "A/..", "..", "a\x00b"} {
		if got, err := SafeJoin(root, rel); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("denied: SafeJoin(%q) = %q, %v", rel, got, err)
		}
	}
}

// library writes a tiny library: Author/Book with two files.
func library(t *testing.T) (root string, files []link.BookFile) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "Author", "Book")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"01.mp3": "one", "02.mp3": "second"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, link.BookFile{Path: "Author/Book/" + name, Size: int64(len(body))})
	}
	return root, files
}

type books struct {
	book *link.Book
	err  error
}

func (b books) Book(context.Context, int64, string) (*link.Book, error) { return b.book, b.err }

func TestCheck(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	src := books{book: &link.Book{LibraryID: 1, Path: "Author/Book", Files: files}}
	lib := link.Library{ID: 1, Name: "Books", Root: "/books"}

	st, unanswered := Check(context.Background(), src, lib, root)
	if !st.Mapped || !st.Writable || st.CheckedPath != "Author/Book" || st.Error != "" || st.MountPath != root || unanswered != nil {
		t.Fatalf("the right mapping = %+v, %v", st, unanswered)
	}
	if left, _ := filepath.Glob(filepath.Join(root, ProbePrefix+"*")); len(left) != 0 {
		t.Fatalf("the probe was left behind: %v", left)
	}

	// One folder too high, a stale copy, a missing folder, a file.
	stale := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stale, "Author", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(stale, filepath.FromSlash(f.Path)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, mount := range map[string]string{
		"one folder too high": filepath.Dir(root),
		"a stale copy":        stale,
		"missing":             filepath.Join(root, "nope"),
		"a file":              filepath.Join(root, "Author", "Book", "01.mp3"),
	} {
		st, unanswered := Check(context.Background(), src, lib, mount)
		if st.Mapped || st.Writable || st.Error == "" || unanswered != nil {
			t.Errorf("%s: %+v, %v; want not mapped with a reason", name, st, unanswered)
		}
		if left, _ := filepath.Glob(filepath.Join(mount, ProbePrefix+"*")); len(left) != 0 {
			t.Errorf("%s: probed a folder that isn't the library", name)
		}
	}
	if st, _ := Check(context.Background(), src, lib, stale); !strings.Contains(st.Error, "size differs") {
		t.Errorf("a stale copy's reason = %q", st.Error)
	}

	// No book to compare: never mapped.
	if st, unanswered := Check(context.Background(), books{err: link.ErrNoBook}, lib, root); st.Mapped || !strings.Contains(st.Error, "no indexed book") || unanswered != nil {
		t.Errorf("no book = %+v, %v", st, unanswered)
	}
	// No answer: said in the status, and returned so the Registry keeps the
	// last known state.
	boom := errors.New("boom")
	if st, unanswered := Check(context.Background(), books{err: boom}, lib, root); st.Mapped || !strings.Contains(st.Error, "boom") || unanswered != boom {
		t.Errorf("server error = %+v, %v", st, unanswered)
	}
	// A server path that leaves the library is a mismatch, never a stat outside.
	evil := books{book: &link.Book{Path: "x", Files: []link.BookFile{{Path: "../../etc/passwd", Size: 1}}}}
	if st, _ := Check(context.Background(), evil, lib, root); st.Mapped || !strings.Contains(st.Error, "leaves the library") {
		t.Errorf("a path out of the library = %+v", st)
	}
}

// A read-only mount is mapped but not writable (spike 3's :ro case).
func TestCheckReadOnly(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes through a 0555 folder")
	}
	root, files := library(t)
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	st, _ := Check(context.Background(), books{book: &link.Book{Path: "Author/Book", Files: files}}, link.Library{ID: 1}, root)
	if !st.Mapped || st.Writable || !strings.Contains(st.Error, "not writable") {
		t.Fatalf("read-only = %+v", st)
	}
}

// BookReadSettled: a definite answer fails a job's book read at once; one that
// may change (no answer, a refused token, a 408, a 429, a 5xx) is asked again.
func TestBookReadSettled(t *testing.T) {
	t.Parallel()
	status := func(s int) error { return fmt.Errorf("wrapped: %w", &link.APIError{Status: s}) }
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"no book":   {fmt.Errorf("wrapped: %w", link.ErrNoBook), true},
		"400":       {status(http.StatusBadRequest), true},
		"403":       {status(http.StatusForbidden), true},
		"404":       {status(http.StatusNotFound), true},
		"refused":   {link.ErrUnauthorized, false},
		"408":       {status(http.StatusRequestTimeout), false},
		"429":       {status(http.StatusTooManyRequests), false},
		"500":       {status(http.StatusInternalServerError), false},
		"503":       {status(http.StatusServiceUnavailable), false},
		"no answer": {errors.New("dial tcp: connection refused"), false},
		"timed out": {context.DeadlineExceeded, false},
	} {
		if got := BookReadSettled(c.err); got != c.want {
			t.Errorf("%s: BookReadSettled = %v, want %v", name, got, c.want)
		}
	}
}

func TestCompareFilesCapsAndReports(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	checks, ok, problem := CompareFiles(root, files)
	if !ok || problem != "" || len(checks) != 2 || !checks[0].OK {
		t.Fatalf("matching = %+v %v %q", checks, ok, problem)
	}
	if _, ok, problem := CompareFiles(root, nil); ok || problem == "" {
		t.Fatal("no files compared as a match")
	}
	many := make([]link.BookFile, MaxFilesChecked+5)
	for i := range many {
		many[i] = files[0]
	}
	if checks, _, _ := CompareFiles(root, many); len(checks) != MaxFilesChecked {
		t.Fatalf("checked %d files, want %d", len(checks), MaxFilesChecked)
	}
	if err := os.Mkdir(filepath.Join(root, "Author", "Dir.mp3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, problem := CompareFiles(root, []link.BookFile{{Path: "Author/Dir.mp3", Size: 0}}); ok || !strings.Contains(problem, "not a regular file") {
		t.Fatalf("a folder named like a file = %v %q", ok, problem)
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	root, files := library(t)
	if !r.SetLibraries([]link.Library{{ID: 2, Name: "B", Root: "/b"}, {ID: 1, Name: "A", Root: root}}) {
		t.Fatal("the first list is a change")
	}
	if r.SetLibraries([]link.Library{{ID: 1, Name: "A", Root: root}, {ID: 2, Name: "B", Root: "/b"}}) {
		t.Fatal("the same list in another order is no change")
	}
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	if !r.Due(now, time.Minute) || !r.Unchecked() {
		t.Fatal("unchecked libraries are due")
	}
	mount := func(id int64, root string) (string, bool) { return root, false }
	src := books{book: &link.Book{Path: "Author/Book", Files: files}}
	r.Check(context.Background(), src, mount, func() time.Time { return now }, 1)
	if !r.Unchecked() {
		t.Fatal("library 2 has no check yet")
	}
	r.Check(context.Background(), src, mount, func() time.Time { return now })
	if r.Unchecked() {
		t.Fatal("every library is checked")
	}
	if r.Due(now.Add(30*time.Second), time.Minute) || !r.Due(now.Add(time.Minute), time.Minute) {
		t.Fatal("Due ignores the interval")
	}
	sts := r.Statuses()
	if len(sts) != 2 || sts[0].LibraryID != 1 || !sts[0].Mapped || sts[1].Mapped {
		t.Fatalf("statuses = %+v", sts)
	}
	if st, ok := r.Status(1); !ok || !st.Mapped {
		t.Fatalf("library 1's status = %+v, %v", st, ok)
	}
	if st, ok := r.Status(2); !ok || st.Mapped {
		t.Fatalf("library 2's status = %+v, %v", st, ok)
	}
	if _, ok := r.Status(9); ok {
		t.Fatal("an unknown library has a status")
	}
	entries := r.Entries(mount)
	if len(entries) != 2 || entries[0].Status == nil || entries[0].CheckedAt == nil || entries[1].MountPath != "/b" {
		t.Fatalf("entries = %+v", entries)
	}
	if !r.SetLibraries([]link.Library{{ID: 1, Name: "A renamed", Root: root}}) {
		t.Fatal("a rename and a removal are a change")
	}
	if sts := r.Statuses(); len(sts) != 1 || sts[0].LibraryID != 1 {
		t.Fatalf("a removed library's check stayed, or a kept one's went: %+v", sts)
	}
	if _, ok := r.Library(2); ok {
		t.Fatal("a removed library is still listed")
	}
}

// Check can check one library, and a check asked for while another runs is
// done right after it, never at the same time.
func TestCheckOneAndQueued(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	root, files := library(t)
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: root}})
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	var mu sync.Mutex
	inFlight, maxInFlight, calls := 0, 0, 0
	src := slowBooks{book: &link.Book{Path: "Author/Book", Files: files}, before: func() {
		mu.Lock()
		inFlight++
		calls++
		maxInFlight = max(maxInFlight, inFlight)
		first := calls == 1
		mu.Unlock()
		if first {
			<-release
		}
	}, after: func() { mu.Lock(); inFlight--; mu.Unlock() }}
	mount := func(id int64, root string) (string, bool) { return root, false }
	done := make(chan struct{})
	go func() { r.Check(context.Background(), src, mount, func() time.Time { return now }, 1); close(done) }()
	for {
		mu.Lock()
		c := calls
		mu.Unlock()
		if c == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// While a check runs nothing is due (it refreshes what is), so a heartbeat
	// doesn't ask for another one.
	if r.Due(now, time.Minute) {
		t.Fatal("due while a check runs")
	}
	// Asked for while the first check runs: queued, and this call returns at once
	// and says it didn't check (its caller doesn't beat for it).
	fresh := books{book: &link.Book{Path: "Author/Book", Files: files}}
	if r.Check(context.Background(), fresh, mount, func() time.Time { return now }, 2) {
		t.Fatal("a queued check reported it checked")
	}
	close(release)
	<-done
	if maxInFlight != 1 || calls != 1 {
		t.Fatalf("checks in flight at once = %d, calls on the first link = %d; want 1 and 1", maxInFlight, calls)
	}
	if len(r.Statuses()) != 2 {
		t.Fatalf("statuses = %+v", r.Statuses())
	}
}

// A check queued behind a running one is made with the link it was asked with,
// never the running one's: after a re-pair the old link is revoked, so a
// follow-up through it would record every library as refused (denied); through
// the new link it maps (allowed).
func TestQueuedCheckUsesItsOwnLink(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	root, files := library(t)
	r.SetLibraries([]link.Library{{ID: 1, Root: root}})
	now := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	book := &link.Book{Path: "Author/Book", Files: files}
	old := &revokedAfterFirst{book: book, release: make(chan struct{}), started: make(chan struct{})}
	mount := func(id int64, root string) (string, bool) { return root, false }
	done := make(chan struct{})
	go func() { r.Check(context.Background(), old, mount, func() time.Time { return now }); close(done) }()
	<-old.started
	r.Check(context.Background(), books{book: book}, mount, func() time.Time { return now })
	close(old.release)
	<-done
	if sts := r.Statuses(); len(sts) != 1 || !sts[0].Mapped || sts[0].Error != "" {
		t.Fatalf("the follow-up check = %+v, want mapped through the new link", sts)
	}
	if old.calls.Load() != 1 {
		t.Fatalf("the old link was asked %d times, want once", old.calls.Load())
	}
}

// revokedAfterFirst answers its first lookup (once released), then refuses.
type revokedAfterFirst struct {
	book             *link.Book
	release, started chan struct{}
	calls            atomic.Int32
}

func (b *revokedAfterFirst) Book(context.Context, int64, string) (*link.Book, error) {
	if b.calls.Add(1) > 1 {
		return nil, link.ErrUnauthorized
	}
	close(b.started)
	<-b.release
	return b.book, nil
}

type slowBooks struct {
	book          *link.Book
	before, after func()
}

func (b slowBooks) Book(context.Context, int64, string) (*link.Book, error) {
	b.before()
	defer b.after()
	return b.book, nil
}

// scripted answers each library's book lookup with err when set, else book,
// and counts the lookups (with when each was made).
type scripted struct {
	mu    sync.Mutex
	book  *link.Book
	err   map[int64]error
	calls []int64
	times []time.Time
}

func (s *scripted) Book(_ context.Context, id int64, _ string) (*link.Book, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, id)
	s.times = append(s.times, time.Now())
	if err := s.err[id]; err != nil {
		return nil, err
	}
	return s.book, nil
}

func (s *scripted) answer(id int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err[id] = err
}

// A check the server didn't answer (a 429, a timeout, a 503, a refused token)
// keeps the library's last known state and makes it due again once the hold
// ends; a definite answer (a 404, no indexed book) still changes it.
func TestUnansweredCheckKeepsLastState(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	timeout := &url.Error{Op: "Get", URL: "https://server/api", Err: context.DeadlineExceeded}
	for name, tc := range map[string]struct {
		err  error
		hold time.Duration
	}{
		"a 429 with Retry-After": {&link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 90 * time.Second, RetryAfterSet: true}, 90 * time.Second},
		"a 429 without":          {&link.APIError{Status: http.StatusTooManyRequests}, RetryFirst},
		"a timeout":              {timeout, RetryFirst},
		"a 503":                  {&link.APIError{Status: http.StatusServiceUnavailable}, RetryFirst},
		"a refused token":        {link.ErrUnauthorized, RetryFirst},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			r.SetLibraries([]link.Library{{ID: 1, Root: root}})
			src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
			r.Check(context.Background(), src, mount, func() time.Time { return at })
			src.answer(1, tc.err)
			later := at.Add(time.Hour)
			r.Check(context.Background(), src, mount, func() time.Time { return later })
			st, ok := r.Status(1)
			if !ok || !st.Mapped || !st.Writable || st.Error != "" {
				t.Fatalf("the last state was replaced: %+v", st)
			}
			if e := r.Entries(mount); *e[0].CheckedAt != at {
				t.Fatalf("checked at %v, want the kept state's %v", *e[0].CheckedAt, at)
			}
			if r.Due(later.Add(tc.hold-time.Second), 24*time.Hour) {
				t.Fatal("due again before the hold ends")
			}
			if !r.Due(later.Add(tc.hold), 24*time.Hour) {
				t.Fatal("not due again once the hold ends")
			}
		})
	}
	for name, err := range map[string]error{
		"a 404":            &link.APIError{Status: http.StatusNotFound, Code: "library_not_found"},
		"no indexed book":  link.ErrNoBook,
		"a 403 (definite)": &link.APIError{Status: http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			r.SetLibraries([]link.Library{{ID: 1, Root: root}})
			src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
			r.Check(context.Background(), src, mount, func() time.Time { return at })
			src.answer(1, err)
			later := at.Add(time.Hour)
			r.Check(context.Background(), src, mount, func() time.Time { return later })
			if st, _ := r.Status(1); st.Mapped || st.Error == "" {
				t.Fatalf("a definite answer kept the old state: %+v", st)
			}
			if r.Due(later, 24*time.Hour) {
				t.Fatal("a definite answer is due again at once")
			}
		})
	}
}

// With no state yet at the mount (a first check, or a mapping moved to another
// folder), an unanswered check is recorded, so the page says why, and is due
// again after the hold.
func TestUnansweredCheckWithoutAState(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	other, _ := library(t)
	cur := root
	mount := func(int64, string) (string, bool) { return cur, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := NewRegistry()
	r.SetLibraries([]link.Library{{ID: 1, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{1: &link.APIError{Status: http.StatusBadGateway}}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	if st, ok := r.Status(1); !ok || st.Mapped || !strings.Contains(st.Error, "could not ask the server") {
		t.Fatalf("a first unanswered check = %+v, %v", st, ok)
	}
	if !r.Due(at.Add(RetryFirst), 24*time.Hour) {
		t.Fatal("a first unanswered check is not due again after the hold")
	}

	src.answer(1, nil)
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	cur = other
	src.answer(1, &link.APIError{Status: http.StatusBadGateway})
	r.Check(context.Background(), src, mount, func() time.Time { return at }, 1)
	if st, _ := r.Status(1); st.Mapped || st.MountPath != other {
		t.Fatalf("the old mount's state stood for the new mount: %+v", st)
	}
}

// A 429 ends the pass: the libraries after it aren't asked, keep their state
// and are due again after the Retry-After.
func TestRateLimitEndsTheRun(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := NewRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: root}, {ID: 3, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	src.mu.Lock()
	src.calls = nil
	src.mu.Unlock()
	src.answer(2, &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 2 * time.Minute, RetryAfterSet: true})
	later := at.Add(time.Hour)
	r.Check(context.Background(), src, mount, func() time.Time { return later })
	if !slices.Equal(src.calls, []int64{1, 2}) {
		t.Fatalf("asked for %v after the 429, want 1 and 2 only", src.calls)
	}
	for _, st := range r.Statuses() {
		if !st.Mapped {
			t.Fatalf("library %d lost its state: %+v", st.LibraryID, st)
		}
	}
	if r.Due(later.Add(time.Minute), 24*time.Hour) || !r.Due(later.Add(2*time.Minute), 24*time.Hour) {
		t.Fatal("the hold isn't the Retry-After")
	}
	src.answer(2, nil)
	r.Check(context.Background(), src, mount, func() time.Time { return later.Add(2 * time.Minute) })
	if r.Due(later.Add(2*time.Minute), 24*time.Hour) {
		t.Fatal("still due after a run the server answered")
	}
}

// queueing asks for library 2's check (an admin's mapping edit) while library
// 1's read is out, then answers that read with err.
type queueing struct {
	*scripted
	r     *Registry
	mount MountFunc
	err   error
	once  sync.Once
}

func (s *queueing) Book(ctx context.Context, id int64, path string) (*link.Book, error) {
	book, err := s.scripted.Book(ctx, id, path)
	if id != 1 {
		return book, err
	}
	s.once.Do(func() {
		if s.r.Check(ctx, s, s.mount, time.Now, 2) {
			panic("the queued check ran beside the running one")
		}
	})
	return nil, s.err
}

// A mapping edit queued behind a pass a 429 ended is still checked right after
// it, as an edit made after the run is: left to the hold, the library would
// show its old folder's state at the new one for up to RetryMax.
func TestQueuedCheckFollowsARateLimitedPass(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	moved, _ := library(t)
	mounts := map[int64]string{1: root, 2: root, 3: root}
	var mu sync.Mutex
	mount := func(id int64, _ string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return mounts[id], true
	}
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := NewRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: root}, {ID: 3, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	src.mu.Lock()
	src.calls = nil
	src.mu.Unlock()
	mu.Lock()
	mounts[2] = moved
	mu.Unlock()
	limited := &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 2 * time.Minute, RetryAfterSet: true}
	later := at.Add(time.Hour)
	r.Check(context.Background(), &queueing{scripted: src, r: r, mount: mount, err: limited}, mount, func() time.Time { return later })
	if !slices.Equal(src.calls, []int64{1, 2}) {
		t.Fatalf("asked for %v, want 1 (the 429) then the queued 2, never 3", src.calls)
	}
	if st, _ := r.Status(2); !st.Mapped || st.MountPath != moved {
		t.Fatalf("the edited library kept its old folder's state: %+v", st)
	}
	if st, _ := r.Status(3); !st.Mapped {
		t.Fatalf("library 3 lost its state: %+v", st)
	}
	if r.Due(later.Add(time.Minute), 24*time.Hour) || !r.Due(later.Add(2*time.Minute), 24*time.Hour) {
		t.Fatal("the 429's libraries aren't held for its Retry-After")
	}
}

// The hold without a Retry-After doubles with each unanswered run in a row, at
// most RetryMax, and a run the server answers ends it.
func TestHoldBacksOff(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, RetryMax, RetryMax} {
		r.holdAfter(errors.New("no answer"), at)
		if got := r.holdUntil.Sub(at); got != want {
			t.Fatalf("hold %d = %v, want %v", r.held, got, want)
		}
	}
	r.holdAfter(&link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 24 * time.Hour, RetryAfterSet: true}, at)
	if got := r.holdUntil.Sub(at); got != RetryMax {
		t.Fatalf("a day's Retry-After held %v, want RetryMax", got)
	}
	r.holdAfter(nil, at)
	if r.held != 0 || !r.holdUntil.IsZero() {
		t.Fatal("an answered run left the hold")
	}
}

// A run asks for one library at a time, at least spacing apart (pacing a run
// of many mappings under the server's rate limit), and stops waiting at once
// when ctx ends.
func TestChecksArePaced(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	r := NewRegistry()
	r.spacing = 40 * time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: root}, {ID: 3, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, time.Now)
	if len(src.times) != 3 {
		t.Fatalf("asked %d times, want 3", len(src.times))
	}
	// The pacer times a read just before the source does, so a busy machine
	// can show two reads a little under spacing apart: half is the test's line.
	for i := 1; i < len(src.times); i++ {
		if gap := src.times[i].Sub(src.times[i-1]); gap < r.spacing/2 {
			t.Fatalf("checks %d and %d were %v apart, want about %v", i-1, i, gap, r.spacing)
		}
	}

	r.spacing = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	src.times = nil
	done := make(chan struct{})
	go func() { r.Check(ctx, src, mount, time.Now); close(done) }()
	for {
		src.mu.Lock()
		n := len(src.times)
		src.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run kept waiting after ctx ended")
	}
}

// Only a server read waits for the spacing: a run over libraries whose folders
// are missing asks nothing, so it doesn't wait at all.
func TestPacingSkipsUnaskedChecks(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	mount := func(int64, string) (string, bool) { return missing, true }
	r := NewRegistry()
	r.spacing = time.Hour
	r.SetLibraries([]link.Library{{ID: 1, Root: "/a"}, {ID: 2, Root: "/b"}, {ID: 3, Root: "/c"}})
	done := make(chan struct{})
	go func() { r.Check(context.Background(), &scripted{err: map[int64]error{}}, mount, time.Now); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a run that asked nothing waited for the spacing")
	}
	if len(r.Statuses()) != 3 {
		t.Fatalf("recorded %d checks, want 3", len(r.Statuses()))
	}
}

// ctxBooks answers once ctx ends, with its error (a check cut short by shutdown
// or a re-pair).
type ctxBooks struct{ started chan struct{} }

func (b ctxBooks) Book(ctx context.Context, _ int64, _ string) (*link.Book, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

// A check cut short by its context records nothing and sets no hold: it says
// nothing about the library or the server.
func TestCutShortCheckRecordsNothing(t *testing.T) {
	t.Parallel()
	root, _ := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := NewRegistry()
	r.SetLibraries([]link.Library{{ID: 1, Root: root}})
	ctx, cancel := context.WithCancel(context.Background())
	src := ctxBooks{started: make(chan struct{})}
	done := make(chan struct{})
	go func() { r.Check(ctx, src, mount, func() time.Time { return at }); close(done) }()
	<-src.started
	cancel()
	<-done
	if _, ok := r.Status(1); ok {
		t.Fatal("a cut-short check was recorded")
	}
	if !r.Due(at, time.Hour) {
		t.Fatal("a cut-short check held the next run")
	}
}

// A zero Retry-After (or a past date) counts as none: a hold of nothing would
// let the run's Kick find the libraries due and ask again at once.
func TestZeroRetryAfterStillHolds(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r.holdAfter(&link.APIError{Status: http.StatusTooManyRequests, RetryAfterSet: true}, at)
	if got := r.holdUntil.Sub(at); got != RetryFirst {
		t.Fatalf("a zero Retry-After held %v, want RetryFirst", got)
	}
}

// A run that asked the server nothing (every folder missing) leaves the hold:
// only an answer ends it.
func TestRunThatAskedNothingKeepsTheHold(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	mount := func(int64, string) (string, bool) { return missing, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := NewRegistry()
	r.SetLibraries([]link.Library{{ID: 1, Root: "/a"}})
	r.holdAfter(errors.New("no answer"), at)
	r.holdAfter(errors.New("no answer"), at)
	r.Check(context.Background(), &scripted{err: map[int64]error{}}, mount, func() time.Time { return at }, 1)
	if r.held != 2 || r.holdUntil.Sub(at) != 2*RetryFirst {
		t.Fatalf("a run that asked nothing changed the hold: %d, %v", r.held, r.holdUntil.Sub(at))
	}
}

// relinking answers the first read with a 429 after Forget (a re-pair) ran
// mid-read, and queues a check with the new link.
type relinking struct {
	r     *Registry
	next  BookSource
	mount MountFunc
	libs  []link.Library
}

func (s relinking) Book(context.Context, int64, string) (*link.Book, error) {
	s.r.Forget()
	s.r.SetLibraries(s.libs)
	if s.r.Check(context.Background(), s.next, s.mount, time.Now) {
		panic("the new link's check ran beside the old one")
	}
	return nil, &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Hour, RetryAfterSet: true}
}

// Forget (a pair, an unpair, a refused token) drops the hold, and an old
// link's run still going records nothing more and holds nothing: the new
// link's queued check runs at once.
func TestForgetEndsTheOldLinksRun(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	libs := []link.Library{{ID: 1, Root: root}}
	r := NewRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries(libs)
	r.holdAfter(errors.New("no answer"), time.Now())
	next := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), relinking{r: r, next: next, mount: mount, libs: libs}, mount, time.Now)
	if st, ok := r.Status(1); !ok || !st.Mapped {
		t.Fatalf("the new link's check = %+v, %v", st, ok)
	}
	if r.held != 0 || !r.holdUntil.IsZero() {
		t.Fatalf("the old link's 429 held the new link: %d, %v", r.held, r.holdUntil)
	}
}
