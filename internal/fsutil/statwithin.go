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
	// others are LookWithin's, by op and path.
	others = &looksOut[any]{calls: map[string]*lookCall[any]{}}
)

// within is look(path) bounded by d: a look a hung mount holds past d returns
// ErrStatTimeout as op's PathError (the look goes on in the background and ends
// whenever the kernel answers). While one look at a path is still out, a later
// one at the same path waits on that one rather than starting another, and only
// for what is left of d since it started, so a loop that polls a hung path
// never piles up blocked looks nor waits d again at every turn (bridge#24).
func (o *looksOut[T]) within(op, path string, d time.Duration, look func(string) (T, error)) (T, error) {
	return o.withinKey(path, op, path, d, look)
}

// withinKey is within with the looks out kept by key rather than by path.
func (o *looksOut[T]) withinKey(key, op, path string, d time.Duration, look func(string) (T, error)) (T, error) {
	o.mu.Lock()
	c, ok := o.calls[key]
	if !ok {
		c = &lookCall[T]{done: make(chan struct{}), started: time.Now()}
		o.calls[key] = c
		go func() {
			val, err := look(path)
			o.mu.Lock()
			c.val, c.err = val, err
			delete(o.calls, key)
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

// LookWithin is look(path) bounded by d, as StatWithin is os.Stat, for a look
// of a kind the others don't cover (op names it: a mapping check's write probe):
// a look a hung mount holds past d is ErrStatTimeout, and while one of op at
// path is out a later one waits on it. Every look of one op at one path must
// answer the same T.
func LookWithin[T any](op, path string, d time.Duration, look func(string) (T, error)) (T, error) {
	val, err := others.withinKey(op+"\x00"+path, op, path, d, func(p string) (any, error) { return look(p) })
	t, _ := val.(T)
	return t, err
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

// change is one ChangeWithin call out: done is closed (under changesMu) once
// val and err hold its answer. waiting counts the callers still waiting for it; one
// that gives up leaves the count, so a change that finishes with none left has
// nobody to hand its answer to. kept: it finished so, and its answer waits in
// val and err for the next call (Late.Keep).
type change struct {
	done    chan struct{}
	started time.Time
	waiting int
	kept    bool
	val     any
	err     error
}

var (
	changesMu sync.Mutex
	changes   = map[string]*change{}
)

// Late is what becomes of a change that finishes once every caller gave up
// (past ChangeWithin's bound), so nobody is left to take its answer.
type Late[T any] struct {
	// Undo undoes what the change left behind (nil: nothing to undo).
	Undo func(T)
	// Keep (nil: never) reports whether the answer is one the next call may
	// still take instead, and returns it as that call gets it (marked late,
	// say): it is then kept for that call, for at most For, and undone only
	// when none came. It is called with the changes locked, so it only reads
	// the answer.
	Keep func(T, error) (T, bool)
	For  time.Duration
}

// ChangeWithin is LookWithin for a look that changes what it looks at (a
// mapping check's podcast marker, written for the server to read back): bounded
// by d, ErrStatTimeout past it, and one out per op and path, a later call
// waiting on it for what is left of d (ErrStatTimeout at once when none is).
// Unlike a look, a change may leave something behind, so its answer is handed
// to every caller still waiting when it finishes. When none is (every caller
// gave up past d, so nobody will ever clean up after it), late says what
// becomes of it: an answer late.Keep takes is kept, and the next call of op at
// path within late.For gets it at once, rather than a change of its own (a
// mount too slow for d would otherwise never get one through); any other, and
// a kept one no call came for, goes to late.Undo in the background. The change
// stays out until its undo is done: a later call meanwhile gets ErrStatTimeout,
// never a change of its own racing the undo (it would see what the undo is
// about to remove) nor the undone answer.
func ChangeWithin[T any](op, path string, d time.Duration, do func(string) (T, error), late Late[T]) (T, error) {
	key := op + "\x00" + path
	timeout := func() (T, error) {
		var zero T
		return zero, &fs.PathError{Op: op, Path: path, Err: ErrStatTimeout}
	}
	// undone ends c, out and already unlocked, as undone: its answer is
	// ErrStatTimeout for whoever waited on it meanwhile.
	undone := func(c *change, val T) {
		if late.Undo != nil {
			late.Undo(val)
		}
		changesMu.Lock()
		c.val, c.err = timeout()
		delete(changes, key)
		close(c.done)
		changesMu.Unlock()
	}
	changesMu.Lock()
	c, ok := changes[key]
	if ok && c.kept {
		// A change that finished after its callers gave up: this call's.
		c.kept = false
		delete(changes, key)
		changesMu.Unlock()
		t, _ := c.val.(T)
		return t, c.err
	}
	if ok && time.Since(c.started) >= d {
		// Out for longer than this call may wait: its answer is late's when
		// it comes, never handed to a call that only just asked (it would
		// take a look that old for its own).
		changesMu.Unlock()
		return timeout()
	}
	if !ok {
		c = &change{done: make(chan struct{}), started: time.Now()}
		changes[key] = c
		go func() {
			val, err := do(path)
			changesMu.Lock()
			keep := false
			if c.waiting == 0 && late.Keep != nil && late.For > 0 {
				if v, ok := late.Keep(val, err); ok {
					val, keep = v, true
				}
			}
			switch {
			case c.waiting > 0:
				c.val, c.err = val, err
				delete(changes, key)
				close(c.done)
				changesMu.Unlock()
			case keep:
				c.val, c.err, c.kept = val, err, true
				changesMu.Unlock()
				time.AfterFunc(late.For, func() {
					changesMu.Lock()
					if !c.kept {
						changesMu.Unlock()
						return // a call took it
					}
					c.kept = false // out again, as its undo
					changesMu.Unlock()
					undone(c, val)
				})
			default:
				changesMu.Unlock()
				undone(c, val)
			}
		}()
	}
	c.waiting++
	changesMu.Unlock()
	answer := func() (T, error) {
		t, _ := c.val.(T)
		return t, c.err
	}
	t := time.NewTimer(d - time.Since(c.started))
	defer t.Stop()
	select {
	case <-c.done:
		return answer()
	case <-t.C:
	}
	changesMu.Lock()
	defer changesMu.Unlock()
	select {
	case <-c.done:
		// It finished as the bound ran out: this caller still counted, so the
		// change handed its answer here rather than to late.
		return answer()
	default:
	}
	c.waiting--
	return timeout()
}
