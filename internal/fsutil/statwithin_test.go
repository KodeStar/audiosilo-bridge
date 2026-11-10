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
	c := &statCall{done: make(chan struct{}), started: time.Now()}
	statsMu.Lock()
	statsInFlight[path] = c // a stat stuck on a hung mount
	statsMu.Unlock()
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
	statsMu.Lock()
	if statsInFlight[path] != c {
		statsMu.Unlock()
		t.Fatal("a second stat of the hung path was started")
	}
	// The mount answers.
	c.err = fs.ErrNotExist
	delete(statsInFlight, path)
	statsMu.Unlock()
	close(c.done)
	if _, err := StatWithin(path, 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after the mount answered = %v, want a fresh stat's ErrNotExist", err)
	}
}
