package fsutil

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

// ErrStatTimeout is StatWithin's and ReadDirWithin's error for a file system
// that didn't answer in time (a hung network mount).
var ErrStatTimeout = errors.New("the file system did not answer in time")

// lookCall is one look at a path out (a stat, a folder read): done is closed
// once val and err hold its answer.
type lookCall[T any] struct {
	done    chan struct{}
	started time.Time
	val     T
	err     error
}

// looksOut are the looks of one kind that have started and haven't answered
// yet, by path.
type looksOut[T any] struct {
	mu    sync.Mutex
	calls map[string]*lookCall[T]
}

var (
	stats    = &looksOut[fs.FileInfo]{calls: map[string]*lookCall[fs.FileInfo]{}}
	lstats   = &looksOut[fs.FileInfo]{calls: map[string]*lookCall[fs.FileInfo]{}}
	readDirs = &looksOut[[]fs.DirEntry]{calls: map[string]*lookCall[[]fs.DirEntry]{}}
	// readDirNs are ReadDirNWithin's, apart from ReadDirWithin's: a read in
	// flight answers only a later read of its own kind.
	readDirNs = &looksOut[[]fs.DirEntry]{calls: map[string]*lookCall[[]fs.DirEntry]{}}
)

// within is look(path) bounded by d: a look a hung mount holds past d returns
// ErrStatTimeout as op's PathError (the look goes on in the background and ends
// whenever the kernel answers). While one look at a path is still out, a later
// one at the same path waits on that one rather than starting another, and only
// for what is left of d since it started, so a loop that polls a hung path
// never piles up blocked looks nor waits d again at every turn (bridge#24).
func (o *looksOut[T]) within(op, path string, d time.Duration, look func(string) (T, error)) (T, error) {
	o.mu.Lock()
	c, ok := o.calls[path]
	if !ok {
		c = &lookCall[T]{done: make(chan struct{}), started: time.Now()}
		o.calls[path] = c
		go func() {
			val, err := look(path)
			o.mu.Lock()
			c.val, c.err = val, err
			delete(o.calls, path)
			o.mu.Unlock()
			close(c.done)
		}()
	}
	o.mu.Unlock()
	// An answer already in hand wins over a bound already spent (a select with
	// both ready picks at random).
	select {
	case <-c.done:
		return c.val, c.err
	default:
	}
	t := time.NewTimer(d - time.Since(c.started))
	defer t.Stop()
	select {
	case <-c.done:
		return c.val, c.err
	case <-t.C:
		var zero T
		return zero, &fs.PathError{Op: op, Path: path, Err: ErrStatTimeout}
	}
}

// StatWithin is os.Stat bounded by d (looksOut.within): a stat a hung mount
// holds past d is ErrStatTimeout.
func StatWithin(path string, d time.Duration) (fs.FileInfo, error) {
	return stats.within("stat", path, d, os.Stat)
}

// LstatWithin is os.Lstat bounded by d, as StatWithin is os.Stat: a look a
// hung mount holds past d is ErrStatTimeout (the endpoint's agent service's
// mount check, agentrun).
func LstatWithin(path string, d time.Duration) (fs.FileInfo, error) {
	return lstats.within("lstat", path, d, os.Lstat)
}

// ReadDirWithin is os.ReadDir bounded by d, as StatWithin is os.Stat: a folder
// read a hung mount holds past d is ErrStatTimeout (bridge#65).
func ReadDirWithin(path string, d time.Duration) ([]fs.DirEntry, error) {
	return readDirs.within("readdir", path, d, os.ReadDir)
}

// ReadDirNWithin is ReadDirWithin reading at most n entries (n > 0), in the
// folder's own order, so a large folder is never read whole: a caller that
// looks through no more than n-1 learns there are more from n coming back.
// A read in flight answers a later one of the same path whatever its n, so
// each path is read with one n (the endpoint's agent service's mount check,
// agentrun).
func ReadDirNWithin(path string, n int, d time.Duration) ([]fs.DirEntry, error) {
	return readDirNs.within("readdir", path, d, func(p string) ([]fs.DirEntry, error) {
		f, err := os.Open(p) //nolint:gosec // the caller's folder, only listed
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		entries, err := f.ReadDir(n)
		if errors.Is(err, io.EOF) {
			err = nil // an empty folder
		}
		return entries, err
	})
}
