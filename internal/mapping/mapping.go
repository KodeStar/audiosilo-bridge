// Package mapping is where the Bridge sees each server library, checked rather
// than assumed (BRIDGE-PLAN.md decision 13): a known indexed book's files are
// stat-ed through the mount and their sizes compared with what the server sees,
// then a probe file is written and removed to learn whether the Bridge can
// write there. Only the size comparison catches a stale copy of a library
// (spike 3), so a library with no indexed book is never reported mapped. A
// podcast library has no books: a marker the server reads back proves it
// instead (CheckPodcast). A check the server doesn't answer (a 429, a 5xx, a
// timeout) says nothing about the folder, so the Registry keeps the library's
// last known state and asks again later, pacing its checks.
package mapping

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// MaxFilesChecked bounds how many of the sample book's files are stat-ed.
const MaxFilesChecked = 50

// ErrOutsideRoot is a library-relative path that would leave the library.
var ErrOutsideRoot = errors.New("the path leaves the library")

// SafeJoin joins a server library-relative path ("/"-separated) onto the
// Bridge's mount of that library, refusing an absolute path, a ".." segment, a
// NUL byte and anything else that would land outside root. A backslash is an
// ordinary name character on Linux and macOS ("AC\DC"), and refused only where
// it separates paths.
func SafeJoin(root, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) || strings.HasPrefix(rel, "/") ||
		(filepath.Separator == '\\' && strings.Contains(rel, `\`)) {
		return "", ErrOutsideRoot
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", ErrOutsideRoot
		}
	}
	full := filepath.Join(root, filepath.FromSlash(path.Clean("/"+rel)))
	if !fsutil.Within(root, full) {
		return "", ErrOutsideRoot
	}
	return full, nil
}

// FileCheck is one file of a book as the Bridge found it through its mount.
type FileCheck struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	ExpectedSize int64  `json:"expected_size"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
}

// CompareFiles stats each file (at most MaxFilesChecked) through mount and
// compares its size with the server's. ok is true when every file matched;
// problem describes the first that didn't.
func CompareFiles(mount string, files []link.BookFile) (checks []FileCheck, ok bool, problem string) {
	ok = len(files) > 0
	if !ok {
		problem = "the server listed no files for the book"
	}
	for i, f := range files {
		if i == MaxFilesChecked {
			break
		}
		c := FileCheck{Path: f.Path, ExpectedSize: f.Size}
		full, err := SafeJoin(mount, f.Path)
		if err == nil {
			var info fs.FileInfo
			info, err = os.Stat(full)
			if err == nil && !info.Mode().IsRegular() {
				err = errors.New("not a regular file")
			}
			if err == nil {
				c.Size = info.Size()
			}
		}
		switch {
		case err != nil:
			c.Error = describeStatError(err)
		case c.Size != c.ExpectedSize:
			c.Error = fmt.Sprintf("size differs: %d bytes here, %d on the server", c.Size, c.ExpectedSize)
		default:
			c.OK = true
		}
		if !c.OK && ok {
			ok, problem = false, f.Path+": "+c.Error
		}
		checks = append(checks, c)
	}
	return checks, ok, problem
}

// describeStatError words a stat failure without the absolute path.
func describeStatError(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found here"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, ErrOutsideRoot):
		return "the server's path leaves the library"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// ProbePrefix starts a probe file's name. The leading dot keeps a probe a
// crash leaves behind out of the server's scan (it never walks hidden names).
const ProbePrefix = ".audiosilo-bridge-probe-"

// ProbeWritable reports whether the Bridge can create, write and remove a file
// in dir (nil when it can). The probe is created exclusively, so it never
// replaces anything.
func ProbeWritable(dir string) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	name := filepath.Join(dir, ProbePrefix+hex.EncodeToString(b[:]))
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // a fixed name in the checked folder
	if err != nil {
		return err
	}
	_, werr := f.WriteString("audiosilo-bridge write check\n")
	cerr := f.Close()
	rerr := os.Remove(name)
	return errors.Join(werr, cerr, rerr)
}

// BookSource looks up an indexed book's files (the server link).
type BookSource interface {
	Book(ctx context.Context, libraryID int64, path string) (*link.Book, error)
}

// BookReadBudget is how long a job's book read is asked again where it gives
// up at all: a verify's every retry, and a generate's read of an answer that
// can't be used (link.ErrUnusableAnswer).
const BookReadBudget = 2 * time.Minute

// BookReadSettled reports whether a failed book read is a definite answer (no
// such book, a 4xx other than a refused token, a 408 or a 429) that asking
// again won't change. No answer at all, a refused token the job runner is
// dealing with, a 408, a 429 (the server's request rate) and a 5xx may change,
// so a job's read asks again.
func BookReadSettled(err error) bool { return settled(err) }

// settled reports whether a failed server read is a definite answer: no such
// book or marker, or an APIError that isn't Transient. Anything else (a 408, a
// 429, a 5xx, a refused token, a timeout, no answer at all) may change if
// asked again.
func settled(err error) bool {
	if errors.Is(err, link.ErrNoBook) || errors.Is(err, link.ErrNoMarker) {
		return true
	}
	var apiErr *link.APIError
	return errors.As(err, &apiErr) && !apiErr.Transient()
}

// unsettled is err when it is a server failure that may change if asked again,
// else nil: a check's second result.
func unsettled(err error) error {
	if err == nil || settled(err) {
		return nil
	}
	return err
}

// Check checks one library's mapping: mount must hold the server's sample book
// with the server's sizes, and only then is the folder probed for writing.
// unanswered is the server's failure when it gave no definite answer (a 429, a
// 5xx, a timeout, no answer at all): the status says so too, and the Registry
// keeps the library's last known state instead of it.
func Check(ctx context.Context, src BookSource, lib link.Library, mount string) (st link.LibraryStatus, unanswered error) {
	st = link.LibraryStatus{LibraryID: lib.ID, MountPath: mount}
	info, err := os.Stat(mount)
	switch {
	case err != nil:
		st.Error = "the folder " + mount + " is " + describeStatError(err)
		return st, nil
	case !info.IsDir():
		st.Error = mount + " is not a folder"
		return st, nil
	}
	book, err := src.Book(ctx, lib.ID, "")
	switch {
	case errors.Is(err, link.ErrNoBook):
		st.Error = "the library has no indexed book to compare against yet"
		return st, nil
	case err != nil:
		st.Error = "could not ask the server for a book: " + err.Error()
		return st, unsettled(err)
	}
	st.CheckedPath = book.Path
	_, ok, problem := CompareFiles(mount, book.Files)
	if !ok {
		st.Error = "this folder doesn't hold the server's copy of the library (" + problem + ")"
		return st, nil
	}
	st.Mapped = true
	if err := ProbeWritable(mount); err != nil {
		st.Error = "mapped, but not writable here: " + describeStatError(err)
		return st, nil
	}
	st.Writable = true
	return st, nil
}

// Entry is one server library with its mount and its last check.
type Entry struct {
	Library    link.Library        `json:"library"`
	MountPath  string              `json:"mount_path"`
	Configured bool                `json:"configured"`
	Status     *link.LibraryStatus `json:"status"`
	CheckedAt  *time.Time          `json:"checked_at"`
}

// MountFunc says where the Bridge sees a library (config.Config.MountFor).
type MountFunc func(libraryID int64, serverRoot string) (path string, configured bool)

// CheckSpacing is the least time between two libraries' checks in a run: each
// asks the server, and a run of many mappings asking at once could meet its
// rate limit.
const CheckSpacing = 250 * time.Millisecond

// After a run in which the server gave no definite answer for a library (a
// 429, a 5xx, a timeout, no answer), Due holds the next run for the answer's
// Retry-After, else RetryFirst doubling with each such run in a row; either at
// most RetryMax.
const (
	RetryFirst = time.Minute
	RetryMax   = 30 * time.Minute
)

// Registry holds the server's libraries (from the last heartbeat) and each
// one's last check. It is safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	libs    []link.Library
	results map[int64]result
	// spacing is CheckSpacing (shorter in tests). holdUntil is when Due may
	// next start a run after one the server didn't answer; held counts such
	// runs in a row.
	spacing   time.Duration
	holdUntil time.Time
	held      int
	// checking: a Check is running; pendAll/pend are what was asked for
	// meanwhile, which it checks right after, with pendSrc, the newest caller's
	// link (a check asked for after a re-pair must not use the old one).
	checking bool
	pendAll  bool
	pend     map[int64]bool
	pendSrc  BookSource
}

// result is a library's last known state. retry: a check since then got no
// definite answer from the server, so the library is due again once the hold
// ends (status and at are still the state before it, or that failure when the
// library had none at its mount).
type result struct {
	status link.LibraryStatus
	at     time.Time
	retry  bool
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{results: map[int64]result{}, pend: map[int64]bool{}, spacing: CheckSpacing}
}

// SetLibraries records the server's libraries and reports whether they differ
// from the last list (a library added, removed, renamed or moved). Checks of
// libraries that are gone are dropped.
func (r *Registry) SetLibraries(libs []link.Library) (changed bool) {
	sorted := slices.Clone(libs)
	for i := range sorted {
		sorted[i].Kind = sorted[i].LibraryKind()
	}
	slices.SortFunc(sorted, func(a, b link.Library) int { return cmp.Compare(a.ID, b.ID) })
	r.mu.Lock()
	defer r.mu.Unlock()
	changed = !slices.Equal(sorted, r.libs)
	r.libs = sorted
	for id := range r.results {
		if !slices.ContainsFunc(sorted, func(l link.Library) bool { return l.ID == id }) {
			delete(r.results, id)
		}
	}
	return changed
}

// Library returns a server library by id.
func (r *Registry) Library(id int64) (link.Library, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.libs {
		if l.ID == id {
			return l, true
		}
	}
	return link.Library{}, false
}

// Status returns a library's last check (false when it has none yet): a write
// job needs it mapped AND writable.
func (r *Registry) Status(id int64) (link.LibraryStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.results[id]
	return res.status, ok
}

// Statuses returns the last check of every library that has one, for the
// heartbeat.
func (r *Registry) Statuses() []link.LibraryStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []link.LibraryStatus{}
	for _, l := range r.libs {
		if res, ok := r.results[l.ID]; ok {
			out = append(out, res.status)
		}
	}
	return out
}

// Unchecked reports whether a library listed has no check yet.
func (r *Registry) Unchecked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.libs {
		if _, ok := r.results[l.ID]; !ok {
			return true
		}
	}
	return false
}

// Entries lists every library with its mount and last check, for the Bridge's
// page.
func (r *Registry) Entries(mount MountFunc) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.libs))
	for _, l := range r.libs {
		e := Entry{Library: l}
		e.MountPath, e.Configured = mount(l.ID, l.Root)
		if res, ok := r.results[l.ID]; ok {
			st, at := res.status, res.at
			e.Status, e.CheckedAt = &st, &at
		}
		out = append(out, e)
	}
	return out
}

// Due reports whether a check is due: a library with none, one older than
// interval or one the server last left unanswered, with no check running (it
// refreshes them, so asking again would only queue a second pass) and no hold
// after an unanswered run (asking again at once would meet the same 429).
func (r *Registry) Due(now time.Time, interval time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.checking || now.Before(r.holdUntil) {
		return false
	}
	for _, l := range r.libs {
		res, ok := r.results[l.ID]
		if !ok || res.retry || now.Sub(res.at) >= interval {
			return true
		}
	}
	return false
}

// Check checks the libraries ids (every library with none) now, one at a time
// and at least spacing apart, and records the results. Checks never overlap:
// one asked for while another runs is done right after it (by that call, with
// this call's src), so a mapping changed mid-check is never missed. A check the
// server didn't answer keeps the library's last known state at the same mount
// (a 429 or a 503 says nothing about the folder), and a 429 ends the run: the
// libraries it hadn't reached yet, and those queued meanwhile, are due again
// after the hold. It returns once there is nothing left to check, or ctx ends,
// and reports whether it checked (false: queued behind the running check).
func (r *Registry) Check(ctx context.Context, src BookSource, mount MountFunc, now func() time.Time, ids ...int64) bool {
	r.mu.Lock()
	if r.checking {
		if len(ids) == 0 {
			r.pendAll = true
		}
		for _, id := range ids {
			r.pend[id] = true
		}
		r.pendSrc = src
		r.mu.Unlock()
		return false
	}
	r.checking = true
	// last is when the run's previous library was checked (spacing);
	// unanswered is its last failure the server didn't answer (the hold).
	var last time.Time
	var unanswered error
	for {
		var libs []link.Library
		listed := map[int64]bool{}
		for _, l := range r.libs {
			listed[l.ID] = true
			if len(ids) == 0 || slices.Contains(ids, l.ID) {
				libs = append(libs, l)
			}
		}
		r.mu.Unlock()
		limited := false
		for i, l := range libs {
			if ctx.Err() != nil || (!last.IsZero() && !sleep(ctx, time.Until(last.Add(r.spacing)))) {
				break
			}
			last = time.Now()
			path, _ := mount(l.ID, l.Root)
			st, err := checkOne(ctx, src, l, path, listed)
			if ctx.Err() != nil {
				break // cut short: it says nothing about the library or the server
			}
			r.mu.Lock()
			r.record(st, err, now())
			if err != nil {
				unanswered = err
				if _, limited = link.RateLimited(err); limited {
					for _, rest := range libs[i+1:] {
						r.markRetry(rest.ID)
					}
				}
			}
			r.mu.Unlock()
			if limited {
				break
			}
		}
		r.mu.Lock()
		if limited {
			// Asking for the queued checks now would meet the same 429.
			for _, l := range r.libs {
				if r.pendAll || r.pend[l.ID] {
					r.markRetry(l.ID)
				}
			}
		}
		if limited || ctx.Err() != nil || (!r.pendAll && len(r.pend) == 0) {
			if ctx.Err() == nil {
				r.holdAfter(unanswered, now())
			}
			r.checking, r.pendAll, r.pendSrc = false, false, nil
			clear(r.pend)
			r.mu.Unlock()
			return true
		}
		ids = nil
		if !r.pendAll {
			for id := range r.pend {
				ids = append(ids, id)
			}
		}
		if r.pendSrc != nil {
			src = r.pendSrc
		}
		r.pendAll, r.pendSrc = false, nil
		clear(r.pend)
	}
}

// checkOne checks one library by its kind (Check, or CheckPodcast).
func checkOne(ctx context.Context, src BookSource, l link.Library, path string, listed map[int64]bool) (link.LibraryStatus, error) {
	if l.LibraryKind() != "podcasts" {
		return Check(ctx, src, l, path)
	}
	marker, ok := src.(MarkerSource)
	if !ok {
		return link.LibraryStatus{LibraryID: l.ID, MountPath: path, Error: "the server link cannot check a library marker"}, nil
	}
	return CheckPodcast(ctx, marker, l, path, func(id int64) bool { return listed[id] })
}

// record keeps a library's check (r.mu held). One the server didn't answer
// (unanswered) leaves the last known state at the same mount in place, due
// again after the hold; with none there it is recorded, so the page says why.
func (r *Registry) record(st link.LibraryStatus, unanswered error, at time.Time) {
	prev, ok := r.results[st.LibraryID]
	if unanswered != nil && ok && prev.status.MountPath == st.MountPath {
		prev.retry = true
		r.results[st.LibraryID] = prev
		return
	}
	r.results[st.LibraryID] = result{status: st, at: at, retry: unanswered != nil}
}

// markRetry makes a checked library due again after the hold (r.mu held); one
// with no check yet is due anyway.
func (r *Registry) markRetry(id int64) {
	if res, ok := r.results[id]; ok {
		res.retry = true
		r.results[id] = res
	}
}

// holdAfter sets when Due may next start a run (r.mu held): nothing to wait
// for after a run the server answered throughout; after one it didn't
// (unanswered), the answer's Retry-After, else RetryFirst doubling with each
// such run in a row, at most RetryMax.
func (r *Registry) holdAfter(unanswered error, now time.Time) {
	if unanswered == nil {
		r.held, r.holdUntil = 0, time.Time{}
		return
	}
	r.held++
	wait := RetryFirst
	for n := 1; n < r.held && wait < RetryMax; n++ {
		wait *= 2
	}
	var apiErr *link.APIError
	if errors.As(unanswered, &apiErr) && apiErr.RetryAfterSet {
		wait = apiErr.RetryAfter
	}
	r.holdUntil = now.Add(min(wait, RetryMax))
}

// sleep waits d, false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
