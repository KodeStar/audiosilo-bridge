package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// StatWithin answers as os.Stat does for a file system that answers.
func TestStatWithinAnswers(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "a.m4b")
	if err := os.WriteFile(p, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := StatWithin(p, 5*time.Second)
	if err != nil || info.Size() != 5 {
		t.Fatalf("StatWithin = %v, %v", info, err)
	}
	if _, err := StatWithin(filepath.Join(t.TempDir(), "gone"), 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file = %v, want ErrNotExist", err)
	}
}

// A stat that doesn't answer in time returns ErrStatTimeout, and a second ask
// of the same path waits on the stat already out instead of starting another;
// once it answers, the next ask starts afresh.
func TestStatWithinTimesOutAndSharesTheStatOut(t *testing.T) {
	t.Parallel()
	const path = "/hung/mount/book.m4b"
	c := &lookCall[fs.FileInfo]{done: make(chan struct{}), started: time.Now()}
	stats.mu.Lock()
	stats.calls[path] = c // a stat stuck on a hung mount
	stats.mu.Unlock()
	for range 3 {
		if _, err := StatWithin(path, 10*time.Millisecond); !errors.Is(err, ErrStatTimeout) {
			t.Fatalf("a hung stat = %v, want ErrStatTimeout", err)
		}
	}
	// A stat out longer than d already: a later ask doesn't wait d again.
	c.started = time.Now().Add(-time.Minute)
	began := time.Now()
	if _, err := StatWithin(path, 5*time.Second); !errors.Is(err, ErrStatTimeout) || time.Since(began) > time.Second {
		t.Fatalf("joining a stat out past d = %v after %v, want ErrStatTimeout at once", err, time.Since(began))
	}
	stats.mu.Lock()
	if stats.calls[path] != c {
		stats.mu.Unlock()
		t.Fatal("a second stat of the hung path was started")
	}
	// The mount answers.
	c.err = fs.ErrNotExist
	delete(stats.calls, path)
	stats.mu.Unlock()
	close(c.done)
	if _, err := StatWithin(path, 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after the mount answered = %v, want a fresh stat's ErrNotExist", err)
	}
}

// ReadDirWithin answers as os.ReadDir does for a file system that answers, and
// a folder read a hung mount holds is ErrStatTimeout, a second ask waiting on
// the read already out (bridge#65).
func TestReadDirWithin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.mp3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadDirWithin(dir, 5*time.Second)
	if err != nil || len(entries) != 1 || entries[0].Name() != "a.mp3" {
		t.Fatalf("ReadDirWithin = %v, %v", entries, err)
	}
	if _, err := ReadDirWithin(filepath.Join(dir, "gone"), 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing folder = %v, want ErrNotExist", err)
	}

	const hung = "/hung/mount/Author/Book"
	c := &lookCall[[]fs.DirEntry]{done: make(chan struct{}), started: time.Now()}
	readDirs.mu.Lock()
	readDirs.calls[hung] = c // a folder read stuck on a hung mount
	readDirs.mu.Unlock()
	for range 2 {
		var pe *fs.PathError
		if _, err := ReadDirWithin(hung, 10*time.Millisecond); !errors.Is(err, ErrStatTimeout) || !errors.As(err, &pe) || pe.Op != "readdir" {
			t.Fatalf("a hung folder read = %v, want a readdir ErrStatTimeout", err)
		}
	}
	readDirs.mu.Lock()
	same := readDirs.calls[hung] == c
	delete(readDirs.calls, hung)
	readDirs.mu.Unlock()
	close(c.done)
	if !same {
		t.Fatal("a second read of the hung folder was started")
	}
}

// LstatWithin answers as os.Lstat does (a link is the link, not its target),
// and a look a hung mount holds is ErrStatTimeout.
func TestLstatWithin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Fatal(err)
	}
	info, err := LstatWithin(link, 5*time.Second)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("LstatWithin = %v, %v, want the link itself", info, err)
	}
	if _, err := LstatWithin(filepath.Join(dir, "gone"), 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file = %v, want ErrNotExist", err)
	}
	const path = "/hung/mount/auth.json"
	c := &lookCall[fs.FileInfo]{done: make(chan struct{}), started: time.Now()}
	lstats.mu.Lock()
	lstats.calls[path] = c
	lstats.mu.Unlock()
	if _, err := LstatWithin(path, 10*time.Millisecond); !errors.Is(err, ErrStatTimeout) {
		t.Fatalf("a hung lstat = %v, want ErrStatTimeout", err)
	}
	lstats.mu.Lock()
	delete(lstats.calls, path)
	lstats.mu.Unlock()
	close(c.done)
}

// ReadDirNWithin reads at most n entries (a large folder isn't read whole),
// answers an empty folder with none, and a missing one as os.Open does.
func TestReadDirNWithin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if entries, err := ReadDirNWithin(dir, 3, 5*time.Second); err != nil || len(entries) != 0 {
		t.Fatalf("an empty folder = %v, %v", entries, err)
	}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if entries, err := ReadDirNWithin(dir, 3, 5*time.Second); err != nil || len(entries) != 3 {
		t.Fatalf("five entries read three at most = %d, %v", len(entries), err)
	}
	if entries, err := ReadDirNWithin(dir, 10, 5*time.Second); err != nil || len(entries) != 5 {
		t.Fatalf("five entries read ten at most = %d, %v", len(entries), err)
	}
	if _, err := ReadDirNWithin(filepath.Join(dir, "gone"), 3, 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing folder = %v, want ErrNotExist", err)
	}
}

// LookWithin answers as its look does, and a look a hung mount holds is
// ErrStatTimeout naming the path: a second ask of the same op and path waits on
// the look already out, while another op at that path starts its own.
func TestLookWithin(t *testing.T) {
	t.Parallel()
	const path = "/hung/mount/library"
	if n, err := LookWithin("count", path, 5*time.Second, func(string) (int, error) { return 7, nil }); n != 7 || err != nil {
		t.Fatalf("an answered look = %v, %v", n, err)
	}
	release := make(chan struct{})
	defer close(release)
	var started atomic.Int32
	hung := func(string) (struct{}, error) {
		started.Add(1)
		<-release
		return struct{}{}, nil
	}
	for range 3 {
		_, err := LookWithin("probe", path, 10*time.Millisecond, hung)
		var pe *fs.PathError
		if !errors.Is(err, ErrStatTimeout) || !errors.As(err, &pe) || pe.Path != path || pe.Op != "probe" {
			t.Fatalf("a hung look = %v, want probe's ErrStatTimeout at %s", err, path)
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d looks of the hung path started, want 1", n)
	}
	if _, err := LookWithin("other", path, 5*time.Second, func(string) (struct{}, error) { return struct{}{}, fs.ErrPermission }); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("another op at the hung path = %v, want its own answer", err)
	}
}

// ChangeWithin answers as its change does; one a hung mount holds past the
// bound is ErrStatTimeout, a second ask waiting on the change already out, and
// a change that finishes once every caller gave up is undone (nobody else
// will), while one a caller took is not.
func TestChangeWithin(t *testing.T) {
	t.Parallel()
	const path = "/hung/mount/podcasts"
	undone := make(chan int, 2)
	undo := func(n int) { undone <- n }
	if n, err := ChangeWithin("answers", path, 5*time.Second, func(string) (int, error) { return 7, nil }, undo); n != 7 || err != nil {
		t.Fatalf("an answered change = %v, %v", n, err)
	}
	release := make(chan struct{})
	var started atomic.Int32
	hung := func(string) (int, error) {
		started.Add(1)
		<-release
		return 9, nil
	}
	for range 3 {
		_, err := ChangeWithin("marker", path, 10*time.Millisecond, hung, undo)
		var pe *fs.PathError
		if !errors.Is(err, ErrStatTimeout) || !errors.As(err, &pe) || pe.Path != path || pe.Op != "marker" {
			t.Fatalf("a hung change = %v, want marker's ErrStatTimeout at %s", err, path)
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d changes of the hung path started, want 1", n)
	}
	select {
	case n := <-undone:
		t.Fatalf("undid %d before the change finished", n)
	default:
	}
	close(release)
	select {
	case n := <-undone:
		if n != 9 {
			t.Fatalf("undid %d, want the hung change's own answer", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a change nobody waited for any more was not undone")
	}
	// The answered change (taken by its caller) was never undone.
	select {
	case n := <-undone:
		t.Fatalf("undid a change a caller took (%d)", n)
	case <-time.After(50 * time.Millisecond):
	}
}
