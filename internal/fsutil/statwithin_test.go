package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
