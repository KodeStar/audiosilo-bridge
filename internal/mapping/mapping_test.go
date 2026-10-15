package mapping

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	st := Check(context.Background(), src, lib, root)
	if !st.Mapped || !st.Writable || st.CheckedPath != "Author/Book" || st.Error != "" || st.MountPath != root {
		t.Fatalf("the right mapping = %+v", st)
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
		st := Check(context.Background(), src, lib, mount)
		if st.Mapped || st.Writable || st.Error == "" {
			t.Errorf("%s: %+v, want not mapped with a reason", name, st)
		}
		if left, _ := filepath.Glob(filepath.Join(mount, ProbePrefix+"*")); len(left) != 0 {
			t.Errorf("%s: probed a folder that isn't the library", name)
		}
	}
	if st := Check(context.Background(), src, lib, stale); !strings.Contains(st.Error, "size differs") {
		t.Errorf("a stale copy's reason = %q", st.Error)
	}

	// No book to compare: never mapped.
	if st := Check(context.Background(), books{err: link.ErrNoBook}, lib, root); st.Mapped || !strings.Contains(st.Error, "no indexed book") {
		t.Errorf("no book = %+v", st)
	}
	if st := Check(context.Background(), books{err: errors.New("boom")}, lib, root); st.Mapped || !strings.Contains(st.Error, "boom") {
		t.Errorf("server error = %+v", st)
	}
	// A server path that leaves the library is a mismatch, never a stat outside.
	evil := books{book: &link.Book{Path: "x", Files: []link.BookFile{{Path: "../../etc/passwd", Size: 1}}}}
	if st := Check(context.Background(), evil, lib, root); st.Mapped || !strings.Contains(st.Error, "leaves the library") {
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
	st := Check(context.Background(), books{book: &link.Book{Path: "Author/Book", Files: files}}, link.Library{ID: 1}, root)
	if !st.Mapped || st.Writable || !strings.Contains(st.Error, "not writable") {
		t.Fatalf("read-only = %+v", st)
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
	if !r.Due(now, time.Minute) {
		t.Fatal("unchecked libraries are due")
	}
	mount := func(id int64, root string) (string, bool) { return root, false }
	src := books{book: &link.Book{Path: "Author/Book", Files: files}}
	r.Check(context.Background(), src, mount, func() time.Time { return now })
	if r.Due(now.Add(30*time.Second), time.Minute) || !r.Due(now.Add(time.Minute), time.Minute) {
		t.Fatal("Due ignores the interval")
	}
	sts := r.Statuses()
	if len(sts) != 2 || sts[0].LibraryID != 1 || !sts[0].Mapped || sts[1].Mapped {
		t.Fatalf("statuses = %+v", sts)
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
	// Asked for while the first check runs: queued, and this call returns at once.
	r.Check(context.Background(), src, mount, func() time.Time { return now }, 2)
	close(release)
	<-done
	if maxInFlight != 1 || calls != 2 {
		t.Fatalf("checks in flight at once = %d, calls = %d; want 1 and 2", maxInFlight, calls)
	}
	if len(r.Statuses()) != 2 {
		t.Fatalf("statuses = %+v", r.Statuses())
	}
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
