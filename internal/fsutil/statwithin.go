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

// statResult is one stat's answer.
type statResult struct {
	info fs.FileInfo
	err  error
}

// statsInFlight are the stats StatWithin has started and that haven't answered
// yet, by path: each one's channel is closed with its answer.
var (
	statsMu       sync.Mutex
	statsInFlight = map[string]*statCall{}
)

type statCall struct {
	done chan struct{}
	res  statResult
}

// StatWithin is os.Stat bounded by d: a stat a hung mount holds past d returns
// ErrStatTimeout (the stat goes on in the background and ends whenever the
// kernel answers). While one stat of a path is still out, a later StatWithin of
// the same path waits on that one rather than starting another, so a loop that
// polls a hung path never piles up blocked stats (bridge#24).
func StatWithin(path string, d time.Duration) (fs.FileInfo, error) {
	statsMu.Lock()
	c, ok := statsInFlight[path]
	if !ok {
		c = &statCall{done: make(chan struct{})}
		statsInFlight[path] = c
		go func() {
			info, err := os.Stat(path)
			statsMu.Lock()
			c.res = statResult{info: info, err: err}
			delete(statsInFlight, path)
			statsMu.Unlock()
			close(c.done)
		}()
	}
	statsMu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.done:
		return c.res.info, c.res.err
	case <-t.C:
		return nil, &fs.PathError{Op: "stat", Path: path, Err: ErrStatTimeout}
	}
}
