package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"
)

// ErrStatTimeout is StatWithin's error for a file system that didn't answer in
// time (a hung network mount).
var ErrStatTimeout = errors.New("the file system did not answer in time")

// statsInFlight are the stats StatWithin has started and that haven't answered
// yet, by path: each one's channel is closed with its answer.
var (
	statsMu       sync.Mutex
	statsInFlight = map[string]*statCall{}
)

// statCall is one stat out: done is closed once info and err hold its answer.
type statCall struct {
	done    chan struct{}
	started time.Time
	info    fs.FileInfo
	err     error
}

// StatWithin is os.Stat bounded by d: a stat a hung mount holds past d returns
// ErrStatTimeout (the stat goes on in the background and ends whenever the
// kernel answers). While one stat of a path is still out, a later StatWithin of
// the same path waits on that one rather than starting another, and only for
// what is left of d since it started, so a loop that polls a hung path never
// piles up blocked stats nor waits d again at every look (bridge#24).
func StatWithin(path string, d time.Duration) (fs.FileInfo, error) {
	statsMu.Lock()
	c, ok := statsInFlight[path]
	if !ok {
		c = &statCall{done: make(chan struct{}), started: time.Now()}
		statsInFlight[path] = c
		go func() {
			info, err := os.Stat(path)
			statsMu.Lock()
			c.info, c.err = info, err
			delete(statsInFlight, path)
			statsMu.Unlock()
			close(c.done)
		}()
	}
	statsMu.Unlock()
	// An answer already in hand wins over a bound already spent (a select with
	// both ready picks at random).
	select {
	case <-c.done:
		return c.info, c.err
	default:
	}
	t := time.NewTimer(d - time.Since(c.started))
	defer t.Stop()
	select {
	case <-c.done:
		return c.info, c.err
	case <-t.C:
		return nil, &fs.PathError{Op: "stat", Path: path, Err: ErrStatTimeout}
	}
}
