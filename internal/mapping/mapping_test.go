package mapping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// newRegistry is a Registry whose reads never age a state (its clock is the zero
// time): the tests check at fixed times, and those of the ageing set clock or
// call Due.
func newRegistry() *Registry {
	r := NewRegistry()
	r.clock = func() time.Time { return time.Time{} }
	return r
}

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
	checks, ok, problem, timeout := CompareFiles(root, files)
	if !ok || problem != "" || len(checks) != 2 || !checks[0].OK || timeout != nil {
		t.Fatalf("matching = %+v %v %q", checks, ok, problem)
	}
	if _, ok, problem, _ := CompareFiles(root, nil); ok || problem == "" {
		t.Fatal("no files compared as a match")
	}
	many := make([]link.BookFile, MaxFilesChecked+5)
	for i := range many {
		many[i] = files[0]
	}
	if checks, _, _, _ := CompareFiles(root, many); len(checks) != MaxFilesChecked {
		t.Fatalf("checked %d files, want %d", len(checks), MaxFilesChecked)
	}
	if err := os.Mkdir(filepath.Join(root, "Author", "Dir.mp3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, problem, timeout := CompareFiles(root, []link.BookFile{{Path: "Author/Dir.mp3", Size: 0}}); ok || !strings.Contains(problem, "not a regular file") || timeout != nil {
		t.Fatalf("a folder named like a file = %v %q", ok, problem)
	}

	// A stat that times out is unknown only when it is the first file that
	// didn't match: one the folder answered with another size before it is a
	// definite mismatch (a stale copy), never a slow folder whose last state a
	// check keeps.
	one := link.BookFile{Path: "Author/Book/01.mp3", Size: 3}
	two := link.BookFile{Path: "Author/Book/02.mp3", Size: 6}
	stat := func(p string) (fs.FileInfo, error) {
		if p == filepath.Join(root, "Author", "Book", "02.mp3") {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: fsutil.ErrStatTimeout}
		}
		return os.Stat(p)
	}
	if _, ok, _, timeout := compareFiles(root, []link.BookFile{one, two}, stat); ok || timeout == nil {
		t.Fatalf("a hung stat after a match = %v, timeout %v; want unknown", ok, timeout)
	}
	stale := []link.BookFile{{Path: one.Path, Size: one.Size + 1}, two}
	if _, ok, problem, timeout := compareFiles(root, stale, stat); ok || timeout != nil || !strings.Contains(problem, "size differs") {
		t.Fatalf("a hung stat after a size mismatch = %v %q, timeout %v; want the mismatch, definite", ok, problem, timeout)
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()
	r := newRegistry()
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
	r := newRegistry()
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
	r := newRegistry()
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
			r := newRegistry()
			r.SetLibraries([]link.Library{{ID: 1, Root: root}})
			src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
			r.Check(context.Background(), src, mount, func() time.Time { return at })
			src.answer(1, tc.err)
			later := at.Add(10 * time.Minute)
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
			r := newRegistry()
			r.SetLibraries([]link.Library{{ID: 1, Root: root}})
			src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
			r.Check(context.Background(), src, mount, func() time.Time { return at })
			src.answer(1, err)
			later := at.Add(10 * time.Minute)
			r.Check(context.Background(), src, mount, func() time.Time { return later })
			if st, ok := r.Status(1); !ok || st.Mapped || st.Error == "" {
				t.Fatalf("a definite answer kept the old state, or reads as unknown: %+v, %v", st, ok)
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
	r := newRegistry()
	r.SetLibraries([]link.Library{{ID: 1, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{1: &link.APIError{Status: http.StatusBadGateway}}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	if st, ok := r.Status(1); ok || st.Mapped || !strings.Contains(st.Error, "could not ask the server") {
		t.Fatalf("a first unanswered check = %+v, %v, want an unknown state saying why", st, ok)
	}
	if !r.Unchecked() {
		t.Fatal("an unknown state counts as checked for the setup checklist")
	}
	if !r.Due(at.Add(RetryFirst), 24*time.Hour) {
		t.Fatal("a first unanswered check is not due again after the hold")
	}

	src.answer(1, nil)
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	if r.Unchecked() {
		t.Fatal("an answered check still counts as unchecked")
	}
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
	r := newRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: root}, {ID: 3, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	src.mu.Lock()
	src.calls = nil
	src.mu.Unlock()
	src.answer(2, &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 2 * time.Minute, RetryAfterSet: true})
	later := at.Add(10 * time.Minute)
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
	r := newRegistry()
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
	later := at.Add(10 * time.Minute)
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

// A refused token ends the pass as a 429 does: every other read would be
// refused too.
func TestRefusedTokenEndsThePass(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: root}, {ID: 3, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	src.mu.Lock()
	src.calls = nil
	src.mu.Unlock()
	src.answer(1, link.ErrUnauthorized)
	later := at.Add(10 * time.Minute)
	r.Check(context.Background(), src, mount, func() time.Time { return later })
	if !slices.Equal(src.calls, []int64{1}) {
		t.Fatalf("asked for %v with a refused token, want 1 only", src.calls)
	}
	for _, st := range r.Statuses() {
		if !st.Mapped {
			t.Fatalf("library %d lost its state: %+v", st.LibraryID, st)
		}
	}
	if r.Due(later.Add(RetryFirst-time.Second), 24*time.Hour) || !r.Due(later.Add(RetryFirst), 24*time.Hour) {
		t.Fatal("the libraries aren't due again after the hold")
	}
}

// The hold is the longest Retry-After of a run's failures: a later failure
// without one (a timeout) doesn't shorten what the server asked for.
func TestHoldKeepsTheLongestRetryAfter(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: root}, {ID: 3, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{
		1: &link.APIError{Status: http.StatusServiceUnavailable, RetryAfter: 10 * time.Minute, RetryAfterSet: true},
		2: &url.Error{Op: "Get", URL: "https://server/api", Err: context.DeadlineExceeded},
		3: &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Minute, RetryAfterSet: true},
	}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	if r.Due(at.Add(10*time.Minute-time.Second), 24*time.Hour) || !r.Due(at.Add(10*time.Minute), 24*time.Hour) {
		t.Fatalf("held until %v, want the 503's ten minutes", r.holdUntil.Sub(at))
	}
}

// The page's entries say when a check left an earlier state in place: for the
// library the server didn't answer and for those the 429 kept the pass from,
// not for one whose check is its own result (answered, or a first failure),
// and no longer once a check is answered.
func TestEntriesSayWhenACheckWentUnanswered(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: root}, {ID: 3, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{
		3: &link.APIError{Status: http.StatusBadGateway},
	}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	for _, e := range r.Entries(mount) {
		if e.UnansweredAt != nil {
			t.Fatalf("library %d: a check that is its own result is marked unanswered at %v", e.Library.ID, e.UnansweredAt)
		}
	}
	if e := r.Entries(mount)[2]; e.Status == nil || e.Status.Mapped || e.Status.Error == "" {
		t.Fatalf("a first unanswered check = %+v", e.Status)
	}

	src.answer(2, &link.APIError{Status: http.StatusTooManyRequests})
	later := at.Add(10 * time.Minute)
	r.Check(context.Background(), src, mount, func() time.Time { return later })
	entries := r.Entries(mount)
	if entries[0].UnansweredAt != nil || *entries[0].CheckedAt != later {
		t.Fatalf("the answered library: checked %v, unanswered %v", entries[0].CheckedAt, entries[0].UnansweredAt)
	}
	for _, e := range entries[1:] {
		if e.UnansweredAt == nil || *e.UnansweredAt != later || *e.CheckedAt != at {
			t.Fatalf("library %d: checked %v, unanswered %v, want the earlier result and the 429's time", e.Library.ID, e.CheckedAt, e.UnansweredAt)
		}
	}
	if !entries[1].Status.Mapped || entries[2].Status.Mapped {
		t.Fatalf("the earlier results were replaced: %+v, %+v", entries[1].Status, entries[2].Status)
	}
	if until, why := r.Held(); !strings.Contains(why, "429") || !until.Equal(later.Add(RetryFirst*2)) {
		t.Fatalf("held until %v for %q, want the second unanswered run's two minutes and the 429", until, why)
	}

	src.answer(2, nil)
	src.answer(3, nil)
	r.Check(context.Background(), src, mount, func() time.Time { return later.Add(time.Minute) })
	for _, e := range r.Entries(mount) {
		if e.UnansweredAt != nil || !e.Status.Mapped {
			t.Fatalf("library %d after an answered check: %+v, unanswered %v", e.Library.ID, e.Status, e.UnansweredAt)
		}
	}
	if until, why := r.Held(); why != "" || !until.IsZero() {
		t.Fatalf("still held until %v for %q after an answered run", until, why)
	}
}

// The hold without a Retry-After doubles with each unanswered run in a row, at
// most RetryMax, and a run the server answers ends it.
func TestHoldBacksOff(t *testing.T) {
	t.Parallel()
	r := newRegistry()
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
	r := newRegistry()
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
	r := newRegistry()
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
	r := newRegistry()
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
	r := newRegistry()
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
	r := newRegistry()
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
	r := newRegistry()
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

// An unanswered check keeps a book library's state only while the folder still
// shows the book the last check found (allowed): an unmounted share leaves an
// empty folder at the mount, and a kept "mapped, writable" there would let an
// import create folders on the bare mount point (denied: the failure is
// recorded and writes wait).
func TestUnansweredCheckNeedsTheBook(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.SetLibraries([]link.Library{{ID: 1, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	src.answer(1, &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Minute, RetryAfterSet: true})
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) }, 1)
	if st, _ := r.Status(1); !st.Mapped || !st.Writable {
		t.Fatalf("allowed: a 429 with the book in place replaced the state: %+v", st)
	}

	if err := os.RemoveAll(filepath.Join(root, "Author")); err != nil {
		t.Fatal(err)
	}
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(2 * time.Minute) }, 1)
	st, ok := r.Status(1)
	if ok || st.Mapped || st.Writable || !strings.Contains(st.Error, "no longer shows Author/Book") {
		t.Fatalf("denied: a 429 at an emptied folder kept the old state, or reads as known: %+v, %v", st, ok)
	}
	if e := r.Entries(mount)[0]; e.UnansweredAt != nil || *e.CheckedAt != at.Add(2*time.Minute) {
		t.Fatalf("the failure isn't the check's own result: checked %v, unanswered %v", e.CheckedAt, e.UnansweredAt)
	}
	if !r.Due(at.Add(4*time.Minute), 24*time.Hour) {
		t.Fatal("the failure isn't due again after the hold")
	}
}

// A state unanswered checks keep stands for KeptChecks check intervals from the
// check that set it, then reads as unknown (Status false, so a write job waits
// rather than failing), keeping that check's time: at the next unanswered
// check, or at Due (the heartbeat) while the hold keeps checks from running.
func TestKeptStateExpires(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	limited := &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Hour, RetryAfterSet: true}
	setup := func() (*Registry, *scripted) {
		r := newRegistry()
		r.Due(at, time.Minute) // takes the check interval
		r.SetLibraries([]link.Library{{ID: 1, Root: root}})
		src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
		r.Check(context.Background(), src, mount, func() time.Time { return at })
		src.answer(1, limited)
		r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) }, 1)
		if st, _ := r.Status(1); !st.Mapped || !st.Writable {
			t.Fatalf("a 429 within the cap replaced the state: %+v", st)
		}
		return r, src
	}

	expired := func(r *Registry, unansweredAt time.Time) {
		t.Helper()
		st, ok := r.Status(1)
		if ok || st.Mapped || st.Writable || !strings.Contains(st.Error, "hasn't answered") {
			t.Fatalf("the kept state outlived the cap: %+v, %v", st, ok)
		}
		// The server takes "not mapped" as definite (it refuses new write
		// jobs, holds podcast polls): it keeps the last state, and its jobs
		// wait here.
		if sts := r.Statuses(); len(sts) != 1 || !sts[0].Mapped || !sts[0].Writable {
			t.Fatalf("the heartbeat reports an expired state as unmapped: %+v", sts)
		}
		e := r.Entries(mount)[0]
		if !e.Unknown || *e.CheckedAt != at || e.UnansweredAt == nil || *e.UnansweredAt != unansweredAt {
			t.Fatalf("an expired state: unknown %v, checked %v, unanswered %v; want unknown, the setting check's time and the last 429's",
				e.Unknown, e.CheckedAt, e.UnansweredAt)
		}
	}

	r, src := setup()
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(KeptChecks * time.Minute) }, 1)
	expired(r, at.Add(KeptChecks*time.Minute))

	r, _ = setup()
	if r.Due(at.Add(KeptChecks*time.Minute-time.Second), time.Minute) {
		t.Fatal("due inside the hold")
	}
	if st, ok := r.Status(1); !ok || !st.Mapped {
		t.Fatalf("the state ended before the cap: %+v, %v", st, ok)
	}
	r.Due(at.Add(KeptChecks*time.Minute), time.Minute)
	expired(r, at.Add(time.Minute))

	// A check the server answers ends it.
	src.answer(1, nil)
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(KeptChecks*time.Minute + time.Second) }, 1)
	if st, ok := r.Status(1); !ok || !st.Mapped || !st.Writable {
		t.Fatalf("an answered check after the cap = %+v, %v", st, ok)
	}
}

// A state no check challenges ages too: read past KeptChecks intervals from
// the check that set it (heartbeats failing from the start of an outage, so no
// check runs and Due may not either), it is unknown, mapped or not, so writes
// wait and a LibriVox fetch's "mapping gone" never starts on it.
func TestStateAgesWhenRead(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	other := t.TempDir() // holds no book: a definite "not mapped"
	mount := func(id int64, _ string) (string, bool) {
		if id == 2 {
			return other, true
		}
		return root, true
	}
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	read := at
	r := NewRegistry()
	r.clock = func() time.Time { return read }
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: other}})
	r.Check(context.Background(), books{book: &link.Book{Path: "Author/Book", Files: files}}, mount, func() time.Time { return at })

	read = at.Add(KeptChecks*defaultInterval - time.Second)
	if st, ok := r.Status(1); !ok || !st.Mapped || !st.Writable {
		t.Fatalf("allowed: a mapped state within the cap = %+v, %v", st, ok)
	}
	if st, ok := r.Status(2); !ok || st.Mapped {
		t.Fatalf("a definite not mapped within the cap = %+v, %v", st, ok)
	}

	read = at.Add(KeptChecks * defaultInterval)
	if st, ok := r.Status(1); ok || st.Mapped || st.Writable || !strings.Contains(st.Error, "hasn't answered") {
		t.Fatalf("denied: a mapped state read past the cap = %+v, %v", st, ok)
	}
	if st, ok := r.Status(2); ok || st.Mapped || !strings.Contains(st.Error, "doesn't hold the server's copy") {
		t.Fatalf("a not mapped state read past the cap = %+v, %v, want unknown with its own reason", st, ok)
	}
	// The heartbeat reports the states as checked, not aged: the server takes
	// "not mapped" as definite, while its jobs wait here.
	if sts := r.Statuses(); len(sts) != 2 || !sts[0].Mapped || !sts[0].Writable || sts[1].Mapped {
		t.Fatalf("the heartbeat reports aged states other than as checked: %+v", sts)
	}
	for _, e := range r.Entries(mount) {
		if !e.Unknown || *e.CheckedAt != at {
			t.Fatalf("library %d's entry: unknown %v, checked %v", e.Library.ID, e.Unknown, e.CheckedAt)
		}
	}
}

// A check made through the hold (Check now, a mapping edit) that fails without
// a Retry-After never shortens the hold the server asked for, nor grows the
// backoff; one with a Retry-After is the server's newer word and sets it.
func TestManualCheckKeepsALongerHold(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.SetLibraries([]link.Library{{ID: 1, Root: root}})
	r.holdAfter(&link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 20 * time.Minute, RetryAfterSet: true}, at)
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{
		1: &url.Error{Op: "Get", URL: "https://server/api", Err: context.DeadlineExceeded},
	}}
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) }, 1)
	if until, why := r.Held(); !until.Equal(at.Add(20*time.Minute)) || !strings.Contains(why, "429") {
		t.Fatalf("held until %v for %q, want the 429's twenty minutes", until.Sub(at), why)
	}
	if r.Due(at.Add(19*time.Minute), 24*time.Hour) {
		t.Fatal("a manual check's timeout shortened the hold")
	}
	if r.held != 1 {
		t.Fatalf("a manual check that left the hold counted toward the backoff: %d runs in a row, want 1", r.held)
	}

	src.answer(1, &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 2 * time.Minute, RetryAfterSet: true})
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) }, 1)
	if until, _ := r.Held(); !until.Equal(at.Add(3 * time.Minute)) {
		t.Fatalf("held until %v, want the newer Retry-After's", until.Sub(at))
	}
}

// A 429 that ends the pass leaves the libraries it never reached their state
// only while their folders still show it, as it does the library it reached:
// one whose share isn't mounted any more (an emptied folder) records the
// failure (denied), one whose book is still there keeps its state (allowed).
func TestRateLimitedPassProvesTheUnreached(t *testing.T) {
	t.Parallel()
	roots := map[int64]string{}
	var files []link.BookFile
	for id := int64(1); id <= 3; id++ {
		roots[id], files = library(t)
	}
	mount := func(id int64, _ string) (string, bool) { return roots[id], true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: "/a"}, {ID: 2, Root: "/b"}, {ID: 3, Root: "/c"}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	if err := os.RemoveAll(filepath.Join(roots[3], "Author")); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	src.calls = nil
	src.mu.Unlock()
	src.answer(1, &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Minute, RetryAfterSet: true})
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) })
	if !slices.Equal(src.calls, []int64{1}) {
		t.Fatalf("asked for %v after the 429, want 1 only", src.calls)
	}
	if st, _ := r.Status(2); !st.Mapped || !st.Writable {
		t.Fatalf("allowed: an unreached library with its book in place lost its state: %+v", st)
	}
	st, _ := r.Status(3)
	if st.Mapped || st.Writable || !strings.Contains(st.Error, "no longer shows Author/Book") {
		t.Fatalf("denied: an unreached library at an emptied folder kept its state: %+v", st)
	}
	if !r.Due(at.Add(2*time.Minute), 24*time.Hour) {
		t.Fatal("the unreached libraries aren't due again after the hold")
	}
}

// A kept state that wasn't mapped is unknown past the cap too, but keeps its
// own reason, which says more than the server's silence does.
func TestDueKeepsAnUnmappedState(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.Due(at, time.Minute)
	r.SetLibraries([]link.Library{{ID: 1, Root: root}})
	wrong := slices.Clone(files)
	wrong[0].Size++
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: wrong}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	src.answer(1, &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Hour, RetryAfterSet: true})
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) }, 1)
	r.Due(at.Add(KeptChecks*time.Minute), time.Minute)
	if st, ok := r.Status(1); ok || st.Mapped || !strings.Contains(st.Error, "doesn't hold the server's copy") {
		t.Fatalf("Due replaced an unmapped state's reason, or left it known: %+v, %v", st, ok)
	}
}

// bookAndMarkers answers book reads as scripted does and marker reads from the
// podcast libraries' folders, or with markerErr.
type bookAndMarkers struct {
	*scripted
	roots     map[int64]string
	markerErr error
}

func (s *bookAndMarkers) Marker(_ context.Context, id int64) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, id)
	err := s.markerErr
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(s.roots[id], MarkerPath))
	if err != nil {
		return "", link.ErrNoMarker
	}
	var marker libraryMarker
	if err := json.Unmarshal(b, &marker); err != nil {
		return "", err
	}
	return marker.Nonce, nil
}

// A pass a 429 ends at a book library never reaches the podcast libraries
// after it: one whose marker is still there keeps its state (allowed), one
// whose marker is gone records the failure (denied), so podcast writes wait.
func TestRateLimitedPassProvesTheUnreachedPodcasts(t *testing.T) {
	t.Parallel()
	bookRoot, files := library(t)
	roots := map[int64]string{1: bookRoot, 4: t.TempDir(), 5: t.TempDir()}
	mount := func(id int64, _ string) (string, bool) { return roots[id], true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: "/a"}, {ID: 4, Root: "/p", Kind: "podcasts"}, {ID: 5, Root: "/q", Kind: "podcasts"}})
	src := &bookAndMarkers{scripted: &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}, roots: roots}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	for _, id := range []int64{1, 4, 5} {
		if st, _ := r.Status(id); !st.Mapped || !st.Writable {
			t.Fatalf("library %d's first check = %+v", id, st)
		}
	}
	if err := os.RemoveAll(filepath.Join(roots[4], ".audiosilo")); err != nil {
		t.Fatal(err)
	}
	limited := &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Minute, RetryAfterSet: true}
	src.answer(1, limited)
	src.mu.Lock()
	src.calls, src.markerErr = nil, limited
	src.mu.Unlock()
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) })
	if !slices.Equal(src.calls, []int64{1}) {
		t.Fatalf("asked for %v after the 429, want 1 only", src.calls)
	}
	if st, _ := r.Status(5); !st.Mapped || !st.Writable {
		t.Fatalf("allowed: an unreached podcast library with its marker lost its state: %+v", st)
	}
	st, _ := r.Status(4)
	if st.Mapped || st.Writable || !strings.Contains(st.Error, "no longer shows "+MarkerPath) {
		t.Fatalf("denied: an unreached podcast library without its marker kept its state: %+v", st)
	}
}

// An unreached library whose mapping moved since its last check keeps nothing:
// the old state was the old folder's, so the failure is recorded.
func TestRateLimitedPassDropsAMovedUnreachedState(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	moved, _ := library(t)
	var mu sync.Mutex
	mounts := map[int64]string{1: root, 2: root}
	mount := func(id int64, _ string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return mounts[id], true
	}
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: "/a"}, {ID: 2, Root: "/b"}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	mu.Lock()
	mounts[2] = moved
	mu.Unlock()
	src.answer(1, &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Minute, RetryAfterSet: true})
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) })
	st, _ := r.Status(2)
	if st.Mapped || st.Writable || st.MountPath != moved || !strings.Contains(st.Error, "not checked") {
		t.Fatalf("an unreached library kept its state at the old mount: %+v", st)
	}
}

// A server read that times out ends the pass as a 429 does: a hung server
// would otherwise cost every library the whole request timeout. The rest keep
// their state, proved by their folders, and are due again after the hold.
func TestTimeoutEndsThePass(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: root}, {ID: 2, Root: root}, {ID: 3, Root: root}})
	src := &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	src.mu.Lock()
	src.calls = nil
	src.mu.Unlock()
	src.answer(1, &url.Error{Op: "Get", URL: "https://server/api", Err: context.DeadlineExceeded})
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) })
	if !slices.Equal(src.calls, []int64{1}) {
		t.Fatalf("asked for %v after a timeout, want 1 only", src.calls)
	}
	for _, st := range r.Statuses() {
		if !st.Mapped || !st.Writable {
			t.Fatalf("library %d lost its state: %+v", st.LibraryID, st)
		}
	}
	if !r.Due(at.Add(time.Minute+RetryFirst), 24*time.Hour) {
		t.Fatal("the libraries aren't due again after the hold")
	}
}

// A book the last check found at the library folder itself proves nothing on
// its own (an unmounted share's empty mount point is that folder too), so an
// unanswered check there records the failure.
func TestUnansweredCheckOfARootBookFailsClosed(t *testing.T) {
	t.Parallel()
	root, files := library(t)
	mount := func(int64, string) (string, bool) { return root, true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	r := newRegistry()
	r.SetLibraries([]link.Library{{ID: 1, Root: root}})
	src := &scripted{book: &link.Book{Path: "", Files: files}, err: map[int64]error{}}
	r.Check(context.Background(), src, mount, func() time.Time { return at })
	if st, _ := r.Status(1); !st.Mapped || st.CheckedPath != "" {
		t.Fatalf("the first check = %+v", st)
	}
	src.answer(1, &link.APIError{Status: http.StatusServiceUnavailable})
	r.Check(context.Background(), src, mount, func() time.Time { return at.Add(time.Minute) })
	if st, ok := r.Status(1); ok || st.Mapped || st.Writable || !strings.Contains(st.Error, "library folder itself") {
		t.Fatalf("an unanswered check kept a root book's state, or reads as known: %+v, %v", st, ok)
	}
}

// hangLooks makes r's looks at the paths hung marks wait for release, bounded
// by bound (each phase its own ops, so a look still out from an earlier phase
// is never joined); the others answer, bounded at LookTimeout, so a slow
// moment on a loaded runner never turns a healthy look into a hung one. A hung
// podcast look runs the real look (it writes the marker) and only then hangs.
func hangLooks(r *Registry, phase string, hung map[string]bool, bound time.Duration, release chan struct{}, fileStats *atomic.Int32, files string) {
	within := func(p string) time.Duration {
		if hung[p] {
			return bound
		}
		return LookTimeout
	}
	r.look = looks{
		stat: func(p string) (fs.FileInfo, error) {
			if files != "" && strings.HasPrefix(p, files) {
				fileStats.Add(1)
			}
			return fsutil.LookWithin("test-stat-"+phase, p, within(p), func(p string) (fs.FileInfo, error) {
				if hung[p] {
					<-release
					return nil, errors.New("released") // touches nothing: the folder may be gone
				}
				return os.Stat(p)
			})
		},
		probe: func(d string) error {
			_, err := fsutil.LookWithin("test-probe-"+phase, d, within(d), func(d string) (struct{}, error) {
				if hung[d] {
					<-release
					return struct{}{}, errors.New("released")
				}
				return struct{}{}, probeWritable(d)
			})
			return err
		},
		podcast: func(op, mount string, look func(string) (podcastFound, error)) (podcastFound, error) {
			return podcastWithin(within(mount))(op+"-"+phase, mount, func(m string) (podcastFound, error) {
				f, err := look(m)
				if hung[m] {
					<-release
				}
				return f, err
			})
		},
	}
}

// A look at a library's folder that a hung mount holds (the folder's stat, a
// book file's stat, the write probe, a podcast check's look) ends that
// library's check within its bound and the run goes on to the next library:
// one hung mount never holds every check after it. A hung file stat ends the
// comparison (each would cost the bound). A look that timed out says nothing
// definite (an array disk spinning up answers in about 8 s), so with no state
// before it the library is unknown (Status false, writes wait) with the reason,
// never a definite "not mapped"; the run sets no hold, and the library is due
// again after LookAgain, not at once (the run's beat would check again in a
// tight loop).
func TestHungMountFailsOnlyItsLibrary(t *testing.T) {
	t.Parallel()
	roots := map[int64]string{}
	var files []link.BookFile
	for id := int64(1); id <= 5; id++ {
		roots[id], files = library(t)
	}
	roots[4] = t.TempDir() // a podcast library
	hung := map[string]bool{
		roots[1]: true, // the folder itself
		filepath.Join(roots[2], "Author", "Book", "01.mp3"): true,
		filepath.Join(roots[2], "Author", "Book", "02.mp3"): true,
		roots[3]: true, // its write probe (its stat is the folder's too: only the probe hangs, below)
		roots[4]: true,
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var fileStats atomic.Int32
	r := newRegistry()
	r.spacing = time.Millisecond
	hangLooks(r, "1", hung, 20*time.Millisecond, release, &fileStats, filepath.Join(roots[2], "Author"))
	stat := r.look.stat
	r.look.stat = func(p string) (fs.FileInfo, error) {
		if p == roots[3] {
			return os.Stat(p)
		}
		return stat(p)
	}
	r.SetLibraries([]link.Library{{ID: 1, Root: "/a"}, {ID: 2, Root: "/b"}, {ID: 3, Root: "/c"},
		{ID: 4, Root: "/d", Kind: "podcasts"}, {ID: 5, Root: "/e"}})
	src := &bookAndMarkers{scripted: &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}, roots: roots}
	mount := func(id int64, _ string) (string, bool) { return roots[id], true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	done := make(chan struct{})
	go func() {
		r.Check(context.Background(), src, mount, func() time.Time { return at })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a hung mount held the run")
	}
	for id, why := range map[int64]string{
		1: "the folder " + roots[1] + " is not answering",
		2: ".mp3: not answering",
		3: "not writable here: not answering",
		4: "the library folder is not answering",
	} {
		st, ok := r.Status(id)
		if ok || st.Writable || !strings.Contains(st.Error, why) {
			t.Errorf("library %d = %+v, %v; want unknown, saying %q", id, st, ok, why)
		}
	}
	if st, ok := r.Status(5); !ok || !st.Mapped || !st.Writable {
		t.Errorf("the answering library = %+v, %v", st, ok)
	}
	if n := fileStats.Load(); n != 1 {
		t.Errorf("%d file stats at the hung mount, want the comparison to end at the first", n)
	}
	for _, e := range r.Entries(mount) {
		slow := e.Library.ID != 5
		if e.Unknown != slow || (e.SlowAt != nil) != slow || e.UnansweredAt != nil {
			t.Errorf("library %d's entry: unknown %v, slow_at %v, unanswered_at %v; want slow %v", e.Library.ID, e.Unknown, e.SlowAt, e.UnansweredAt, slow)
		}
	}
	if until, why := r.Held(); why != "" {
		t.Fatalf("a slow folder held the checks until %v: %s", until, why)
	}
	if r.Due(at.Add(time.Second), time.Hour) {
		t.Fatal("a slow folder made the libraries due at once: the run's beat would check again in a tight loop")
	}
	if !r.Due(at.Add(LookAgain), time.Hour) {
		t.Fatal("a slow folder isn't looked at again after LookAgain")
	}
}

// A look that times out keeps the library's last state while it stands (within
// KeptChecks intervals, its folder still showing the book), as a check the
// server didn't answer does: a write job runs on through a disk spinning up
// rather than failing on one slow look. Past the cap it is unknown, saying the
// folder hasn't answered, and an answered look ends it.
func TestSlowLookKeepsTheState(t *testing.T) {
	t.Parallel()
	books, files := library(t)
	podcasts := t.TempDir()
	roots := map[int64]string{1: books, 2: podcasts}
	r := newRegistry()
	r.spacing = time.Millisecond
	r.SetLibraries([]link.Library{{ID: 1, Root: "/a"}, {ID: 2, Root: "/b", Kind: "podcasts"}})
	src := &bookAndMarkers{scripted: &scripted{book: &link.Book{Path: "Author/Book", Files: files}, err: map[int64]error{}}, roots: roots}
	mount := func(id int64, _ string) (string, bool) { return roots[id], true }
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return at }
	r.Check(context.Background(), src, mount, clock)
	for id := int64(1); id <= 2; id++ {
		if st, ok := r.Status(id); !ok || !st.Mapped || !st.Writable {
			t.Fatalf("library %d's first check = %+v, %v", id, st, ok)
		}
	}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hangLooks(r, "slow", map[string]bool{books: true, podcasts: true}, 20*time.Millisecond, release, nil, "")
	stat := r.look.stat
	r.look.stat = func(p string) (fs.FileInfo, error) {
		if p != books {
			return os.Stat(p) // the proof: the book's folder answers
		}
		return stat(p)
	}
	at = at.Add(time.Minute)
	r.Check(context.Background(), src, mount, clock)
	r.clock = clock
	for _, e := range r.Entries(mount) {
		st, ok := r.Status(e.Library.ID)
		if !ok || !st.Mapped || !st.Writable || e.Unknown || e.SlowAt == nil || !e.SlowAt.Equal(at) {
			t.Errorf("library %d after a slow look = %+v, %v (entry unknown %v, slow_at %v); want the last state kept", e.Library.ID, st, ok, e.Unknown, e.SlowAt)
		}
	}
	at = at.Add(KeptChecks * defaultInterval)
	st, ok := r.Status(1)
	if ok || st.Mapped || !strings.Contains(st.Error, "the folder hasn't answered a check of this mapping within 5s") {
		t.Fatalf("a state kept past the cap by slow looks = %+v, %v", st, ok)
	}
	if e := r.Entries(mount)[0]; !e.Unknown {
		t.Fatal("the page doesn't badge a state too old to stand as unknown")
	}
}

// A podcast check's whole look before the server's read is bounded as one: a
// mount that hangs after the marker's open, read or write (here, once the look
// wrote it) ends the check unknown within the bound, and the marker the look
// wrote is removed once the mount answers, since nobody else will.
func TestHungPodcastLookIsBoundedAndUndone(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	r := newRegistry()
	r.SetLibraries([]link.Library{{ID: 2, Root: "/p", Kind: "podcasts"}})
	release := make(chan struct{})
	hangLooks(r, "1", map[string]bool{mount: true}, 20*time.Millisecond, release, nil, "")
	src := &bookAndMarkers{scripted: &scripted{err: map[int64]error{}}, roots: map[int64]string{2: mount}}
	mountFn := func(int64, string) (string, bool) { return mount, true }
	done := make(chan struct{})
	go func() {
		r.Check(context.Background(), src, mountFn, time.Now)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("a podcast folder that hangs after its marker's write held the run")
	}
	if st, ok := r.Status(2); ok || st.Mapped || !strings.Contains(st.Error, "not answering") {
		t.Fatalf("a hung podcast look = %+v, %v; want unknown, not answering", st, ok)
	}
	if len(src.calls) != 0 {
		t.Fatal("the server was asked to read a marker the look never handed over")
	}
	// The look goes on past the bound: it writes the marker, then hangs.
	waitUntil(t, "the hung look's marker", func() bool {
		_, err := os.Stat(filepath.Join(mount, MarkerPath))
		return err == nil
	})
	close(release)
	waitUntil(t, "the hung look's marker removed once the mount answered", func() bool {
		_, err := os.Lstat(filepath.Join(mount, ".audiosilo"))
		return errors.Is(err, os.ErrNotExist)
	})
}

// waitUntil waits (at most 5 s) for ok, failing the test as what otherwise.
func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
