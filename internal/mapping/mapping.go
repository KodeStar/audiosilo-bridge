// Package mapping is where the Bridge sees each server library, checked rather
// than assumed (BRIDGE-PLAN.md decision 13): a known indexed book's files are
// stat-ed through the mount and their sizes compared with what the server sees,
// then a probe file is written and removed to learn whether the Bridge can
// write there. Only the size comparison catches a stale copy of a library
// (spike 3), so a library with no indexed book is never reported mapped. A
// podcast library has no books: a marker the server reads back proves it
// instead (CheckPodcast). A check the server doesn't answer (a 429, a 5xx, a
// timeout) says nothing about the server's side, so the Registry keeps the
// library's last known state while the folder still shows what that state's
// check found there (proveKept), for at most KeptChecks check intervals, and
// asks again later, pacing its checks. Where it has no state that stands (one
// kept past that, or a failure the server didn't answer) Status says the
// library isn't checked, so a write job waits rather than failing. Every look
// at a library's folder is bounded by LookTimeout: a hung mount fails that
// library's check rather than holding the run, and since a look that times out
// says nothing definite either (an array disk spinning up answers in about 8 s),
// it is unknown as an unanswered check is: the state stands if there was one,
// and the library is looked at again after LookAgain.
package mapping

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
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
// problem describes the first that didn't. Each stat is bounded by LookTimeout,
// and the first a hung mount holds past it ends the comparison: timeout is that
// stat's fsutil.ErrStatTimeout when it is the first file that didn't match
// (nil when every stat answered, or an earlier file's answer already said the
// copy differs), which says nothing definite about the copy, so a caller looks
// again later rather than taking the mismatch as one.
func CompareFiles(mount string, files []link.BookFile) (checks []FileCheck, ok bool, problem string, timeout error) {
	return compareFiles(mount, files, bounded.stat)
}

// compareFiles is CompareFiles with its stat.
func compareFiles(mount string, files []link.BookFile, stat func(string) (fs.FileInfo, error)) (checks []FileCheck, ok bool, problem string, timeout error) {
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
			info, err = stat(full)
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
		first := !c.OK && ok
		if first {
			ok, problem = false, f.Path+": "+c.Error
		}
		checks = append(checks, c)
		if errors.Is(err, fsutil.ErrStatTimeout) {
			if first {
				// Only a silence before any definite mismatch is unknown: a file
				// the folder answered with another size already settled it.
				timeout = err
			}
			break // the mount hangs: every file after would cost LookTimeout too
		}
	}
	return checks, ok, problem, timeout
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
	case errors.Is(err, fsutil.ErrStatTimeout):
		return "not answering (no answer within " + LookTimeout.String() + ")"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// lostSight says why a stat of what (something an earlier check found in the
// folder) failed: a mount that didn't answer within LookTimeout says nothing
// about what is there, so it isn't reported as gone.
func lostSight(what string, err error) string {
	if errors.Is(err, fsutil.ErrStatTimeout) {
		return "the folder didn't answer within " + LookTimeout.String() + " to show " + what
	}
	return "the folder no longer shows " + what + " (" + describeStatError(err) + ")"
}

// ProbePrefix starts a probe file's name. The leading dot keeps a probe a
// crash leaves behind out of the server's scan (it never walks hidden names).
const ProbePrefix = ".audiosilo-bridge-probe-"

// ProbeWritable reports whether the Bridge can create, write and remove a file
// in dir (nil when it can). The probe is created exclusively, so it never
// replaces anything. It is bounded by LookTimeout: a folder a hung mount holds
// past it is fsutil.ErrStatTimeout (the probe goes on in the background, and a
// later probe of dir waits on it rather than starting another).
func ProbeWritable(dir string) error {
	_, err := fsutil.LookWithin("probe", dir, LookTimeout, func(d string) (struct{}, error) { return struct{}{}, probeWritable(d) })
	return err
}

// probeWritable is ProbeWritable without its bound.
func probeWritable(dir string) error { return probeWritableLaps(dir, func() {}) }

// probeWritableLaps is probeWritable calling lap after the probe's creation
// and after its write (a podcast look times its steps: lookPace).
func probeWritableLaps(dir string, lap func()) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	name := filepath.Join(dir, ProbePrefix+hex.EncodeToString(b[:]))
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // a fixed name in the checked folder
	lap()
	if err != nil {
		return err
	}
	_, werr := f.WriteString("audiosilo-bridge write check\n")
	cerr := f.Close()
	lap()
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

// BookReadSettled reports whether a failed server read (a book, or a podcast
// library's marker) is a definite answer (no such book or marker, a 4xx other
// than a refused token, a 408 or a 429) that asking again won't change. No
// answer at all, a refused token the job runner is dealing with, a 408, a 429
// (the server's request rate) and a 5xx may change, so a job's read asks again
// and a mapping check keeps the library's last state.
func BookReadSettled(err error) bool {
	if errors.Is(err, link.ErrNoBook) || errors.Is(err, link.ErrNoMarker) {
		return true
	}
	var apiErr *link.APIError
	return errors.As(err, &apiErr) && !apiErr.Transient()
}

// unsettled is err when it is a server failure that may change if asked again,
// else nil: a check's second result.
func unsettled(err error) error {
	if err == nil || BookReadSettled(err) {
		return nil
	}
	return err
}

// LookTimeout bounds each look a check makes at a library's folder (a stat, the
// write probe, its wait for a podcast check's whole look before the server's
// read) and the one that lets an unanswered check keep a state (proveKept): a
// hung mount ends the library's check, unknown (folderSlow), rather than
// holding the run and every check after it.
const LookTimeout = 5 * time.Second

// LookAgain is how soon a library whose folder didn't answer a check's look
// within LookTimeout is due again: the run goes on without it, and the next one
// looks again (a disk spinning up answers by then). It neither holds the other
// libraries' checks nor doubles: the folder's slowness says nothing about the
// server.
const LookAgain = time.Minute

// looks are the looks at a library's folder a check makes, each bounded by
// LookTimeout (bounded), unless a test hangs one. podcast is a podcast check's
// whole look before the server's read (lookPodcast), waited for as one change
// (podcastWithin: one that answers after every caller gave up is kept for the
// next check, or what it wrote undone).
type looks struct {
	stat    func(path string) (fs.FileInfo, error)
	probe   func(dir string) error
	podcast func(op, mount string, look func(string) (podcastFound, error)) (podcastFound, error)
}

var bounded = looks{
	stat:    func(p string) (fs.FileInfo, error) { return fsutil.StatWithin(p, LookTimeout) },
	probe:   ProbeWritable,
	podcast: podcastWithin(LookTimeout, LookKeptFor),
}

// folderSlow is a check whose look at the library's folder got no answer within
// LookTimeout: like a check the server didn't answer it says nothing definite,
// so the Registry keeps the library's state (or records it unknown), but the
// folder was slow, not the server, so it neither ends the run nor holds the
// next one, and the library is due again after LookAgain.
//
// wrote is set when the look that timed out may still write to the folder (a
// podcast check's, whose marker is what proveKept looks for): it reports
// whether a marker written there since isn't settled, so the marker the folder
// shows may be the look's own, which proves nothing (markerWrites).
type folderSlow struct {
	error
	wrote func() bool
}

func (e folderSlow) Unwrap() error { return e.error }

// slowLook is err as a check's second result: folderSlow when the look timed
// out, else nil (the folder's answer is definite).
func slowLook(err error) error {
	if errors.Is(err, fsutil.ErrStatTimeout) {
		return folderSlow{error: err}
	}
	return nil
}

// noAnswer says who didn't answer a check that left no definite result.
func noAnswer(err error) string {
	var slow folderSlow
	if errors.As(err, &slow) {
		return "the folder didn't answer this one within " + LookTimeout.String()
	}
	return "the server didn't answer this one"
}

// Check checks one library's mapping: mount must hold the server's sample book
// with the server's sizes, and only then is the folder probed for writing.
// unanswered is the server's failure when it gave no definite answer (a 429, a
// 5xx, a timeout, no answer at all): the status says so too, and the Registry
// may keep the library's last known state instead of it (proveKept). A look at
// the folder that a hung mount holds past LookTimeout is no definite answer
// either: unanswered is then folderSlow.
func Check(ctx context.Context, src BookSource, lib link.Library, mount string) (st link.LibraryStatus, unanswered error) {
	var empty func() (link.LibraryStatus, error)
	if marker, ok := src.(MarkerSource); ok {
		// Without the registry we cannot tell if a different marker's
		// library was removed. Keep it for its owner.
		empty = func() (link.LibraryStatus, error) {
			return bounded.checkPodcast(ctx, marker, lib, mount, func(int64) bool { return true })
		}
	}
	return bounded.check(ctx, src, lib, mount, empty)
}

// check is Check with these looks.
func (l looks) check(ctx context.Context, src BookSource, lib link.Library, mount string, empty func() (link.LibraryStatus, error)) (st link.LibraryStatus, unanswered error) {
	st = link.LibraryStatus{LibraryID: lib.ID, MountPath: mount}
	info, err := l.stat(mount)
	switch {
	case err != nil:
		st.Error = "the folder " + mount + " is " + describeStatError(err)
		return st, slowLook(err)
	case !info.IsDir():
		st.Error = mount + " is not a folder"
		return st, nil
	}
	book, err := src.Book(ctx, lib.ID, "")
	switch {
	case errors.Is(err, link.ErrNoBook):
		if empty != nil {
			return empty()
		}
		st.Error = "the library has no indexed book to compare against yet"
		return st, nil
	case err != nil:
		st.Error = "could not ask the server for a book: " + err.Error()
		return st, unsettled(err)
	}
	st.CheckedPath = book.Path
	_, ok, problem, timeout := compareFiles(mount, book.Files, l.stat)
	switch {
	case timeout != nil:
		// Not a mismatch: the folder went silent before any file differed.
		st.Error = "the folder didn't answer while the book's files were compared with the server's (" + problem + ")"
		return st, slowLook(timeout)
	case !ok:
		st.Error = "this folder doesn't hold the server's copy of the library (" + problem + ")"
		return st, nil
	}
	st.Mapped = true
	if err := l.probe(mount); err != nil {
		st.Error = "mapped, but not writable here: " + describeStatError(err)
		return st, slowLook(err)
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
	// UnansweredAt is when a check since CheckedAt last got no definite answer
	// from the server (nil when none did): Status is the result before it.
	UnansweredAt *time.Time `json:"unanswered_at"`
	// SlowAt is when a check since CheckedAt last found the folder not
	// answering within LookTimeout (nil when none did; never with UnansweredAt,
	// the later of the two stands): Status is the result before it.
	SlowAt *time.Time `json:"slow_at"`
	// Unknown: no state stands (Registry.Status says unchecked), Status says why.
	Unknown bool `json:"unknown"`
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

// KeptChecks is how many check intervals a library's state stands without a
// check the server answers (checks unanswered, or none made: a heartbeat that
// fails from the start of an outage): counted from the check that set it,
// after that the state is unknown, so writes wait for a check the server
// answers.
const KeptChecks = 3

// defaultInterval is config's default mapping.check_minutes, the interval
// until the first Due gives the configured one.
const defaultInterval = 10 * time.Minute

// Registry holds the server's libraries (from the last heartbeat) and each
// one's last check. It is safe for concurrent use.
type Registry struct {
	// look is bounded (a test hangs a look); clock is when Status and the rest
	// read a state, which ages it (view).
	look    looks
	clock   func() time.Time
	mu      sync.Mutex
	libs    []link.Library
	results map[int64]result
	// spacing is CheckSpacing (shorter in tests); interval is the configured
	// check interval (Due's last), which a kept state's age is capped in. holdUntil is when
	// Due may next start a run after one the server didn't answer, holdWhy
	// that run's failure; held counts such runs in a row.
	spacing   time.Duration
	interval  time.Duration
	holdUntil time.Time
	holdWhy   string
	held      int
	// gen counts Forget calls (a pair, an unpair, a refused token): a run's
	// reads from before one were the old link's, so they are dropped.
	gen int
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
// library had none at its mount). unanswered is when the last such check left
// an earlier state in place (zero when status is the last check's own).
// unknown: status is such a check's own failure, which says nothing definite
// (no state at the mount to keep, or one the folder no longer proved).
// expired: the state was found older than KeptChecks intervals (Due, record),
// which view also works out as it reads it. slow is unanswered's counterpart for
// a check whose look at the folder timed out (folderSlow; one of the two is
// set, the later): that library is due again LookAgain after it.
type result struct {
	status     link.LibraryStatus
	at         time.Time
	retry      bool
	unanswered time.Time
	slow       time.Time
	unknown    bool
	expired    bool
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{look: bounded, clock: time.Now, results: map[int64]result{}, pend: map[int64]bool{},
		spacing: CheckSpacing, interval: defaultInterval}
}

// keptTooLong reports whether a state set by a check at is past what it may
// stand for at now without a check the server answers: KeptChecks check
// intervals (r.mu held).
func (r *Registry) keptTooLong(at, now time.Time) bool {
	return now.Sub(at) >= KeptChecks*r.interval
}

// view is a library's state as a write job may take it at now (r.mu held), and
// whether it stands (known). A failure the server didn't answer doesn't, nor
// does a state past KeptChecks intervals: a mapped one reads as failed, saying
// since when, and an unmapped one keeps its own reason (it let nothing write).
// The state keeps the time of the check that set it.
func (r *Registry) view(res result, now time.Time) (st link.LibraryStatus, known bool) {
	switch {
	case res.unknown:
		return res.status, false
	case !res.expired && !r.keptTooLong(res.at, now):
		return res.status, true
	case !res.status.Mapped:
		return res.status, false
	}
	if !res.slow.IsZero() {
		return link.LibraryStatus{LibraryID: res.status.LibraryID, MountPath: res.status.MountPath,
			Error: "the folder hasn't answered a check of this mapping within " + LookTimeout.String() + " since " +
				res.at.Format(time.RFC3339) + ", so writes wait for one it answers"}, false
	}
	return link.LibraryStatus{LibraryID: res.status.LibraryID, MountPath: res.status.MountPath,
		Error: "the server hasn't answered a check of this mapping since " + res.at.Format(time.RFC3339) +
			", so writes wait for one it answers"}, false
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

// Forget drops the libraries, their checks and the hold when the link changes
// (a pair, an unpair, a refused token): the old link's answers say nothing
// about the new one. A run still going records nothing more of the old link's
// and goes on only to the checks queued after this (with the new link).
func (r *Registry) Forget() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.libs = nil
	clear(r.results)
	r.held, r.holdUntil, r.holdWhy = 0, time.Time{}, ""
	r.pendAll, r.pendSrc = false, nil
	clear(r.pend)
	r.gen++
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

// Status returns a library's last check, and whether it stands: false when it
// has none yet, or none the server's answer still backs (view: a failure the
// server didn't answer, a state past KeptChecks intervals). A write job needs
// it standing, mapped AND writable, waits while it doesn't stand, and takes
// only a standing "not mapped" as one.
func (r *Registry) Status(id int64) (link.LibraryStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.results[id]
	if !ok {
		return link.LibraryStatus{}, false
	}
	return r.view(res, r.clock())
}

// Statuses returns the last check of every library that has one, for the
// heartbeat: the state as recorded, not aged (view). A state too old to stand
// lets nothing write here (Status), but the server takes an unmapped one as
// definite: it refuses new write jobs and holds a podcast show's polls for 30
// minutes, which the next check the server answers would undo. So it keeps
// seeing the last state, and the jobs it queues wait here for that check.
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

// Unchecked reports whether a library listed has no check yet, or none that
// stands (view: unknown reads as unchecked, as Status says), so the setup
// checklist says the libraries are being checked rather than not writable.
func (r *Registry) Unchecked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	for _, l := range r.libs {
		res, ok := r.results[l.ID]
		if !ok {
			return true
		}
		if _, known := r.view(res, now); !known {
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
	now := r.clock()
	for _, l := range r.libs {
		e := Entry{Library: l}
		e.MountPath, e.Configured = mount(l.ID, l.Root)
		if res, ok := r.results[l.ID]; ok {
			st, known := r.view(res, now)
			at := res.at
			e.Status, e.CheckedAt, e.Unknown = &st, &at, !known
			if unanswered := res.unanswered; !unanswered.IsZero() {
				e.UnansweredAt = &unanswered
			}
			if slow := res.slow; !slow.IsZero() {
				e.SlowAt = &slow
			}
		}
		out = append(out, e)
	}
	return out
}

// Due reports whether a check is due: a library with none, one older than
// interval, one the server last left unanswered or one whose folder didn't
// answer in time LookAgain ago, with no check running (it
// refreshes them, so asking again would only queue a second pass) and no hold
// after an unanswered run (asking again at once would meet the same 429). Only
// the heartbeat's periodic run waits for Due: an admin's mapping edit, Check
// now and a changed library list call Check at once, hold or not. interval is
// the configured one (mapping.check_minutes), which also caps a state: Due
// marks the states no answered check has set for KeptChecks of it expired,
// hold or not (the heartbeat calls it on every beat, one that failed too), so
// writes wait for a check the server answers. view ages them as it reads them
// too, for a Registry no beat reaches.
func (r *Registry) Due(now time.Time, interval time.Duration) bool {
	due, _ := r.DueIDs(now, interval)
	return due
}

// DueIDs is Due with the libraries the run is due for: nil for every library
// (one has no check, a retry or a check older than interval), else only the
// ones whose folder was slow LookAgain ago, so a folder that keeps timing out
// never runs every other library's check (a server read each) every
// LookAgain. A slow library is due by LookAgain alone: the state a slow look
// kept is as old as the check that set it, often past interval (that is why it
// was checked), and its age would make it due again at once, the run's beat
// checking it in a tight loop.
func (r *Registry) DueIDs(now time.Time, interval time.Duration) (due bool, ids []int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interval = interval
	for id, res := range r.results {
		if !res.expired && r.keptTooLong(res.at, now) {
			res.expired = true
			r.results[id] = res
		}
	}
	if r.checking || now.Before(r.holdUntil) {
		return false, nil
	}
	for _, l := range r.libs {
		res, ok := r.results[l.ID]
		switch {
		case !ok || res.retry:
			return true, nil
		case !res.slow.IsZero():
			if !now.Before(res.slow.Add(LookAgain)) {
				ids = append(ids, l.ID)
			}
		case now.Sub(res.at) >= interval:
			return true, nil
		}
	}
	return len(ids) > 0, ids
}

// Check checks the libraries ids (every library with none) now, one at a time
// and at least spacing apart, and records the results. Checks never overlap:
// one asked for while another runs is done right after it (by that call, with
// this call's src), so a mapping changed mid-check is never missed. A check the
// server didn't answer keeps the library's last known state at the same mount
// (a 429 or a 503 says nothing about the folder), as does one whose look at the
// folder timed out (folderSlow: that library is due again after LookAgain, and
// the run goes on, unheld), and a 429, a refused token or a timeout
// ends the pass: the libraries it hadn't reached yet are due again after the
// hold, keeping their state only as such a check would (keepUnreached). The
// checks queued meanwhile are still made (an admin's mapping edit,
// Check now, a changed library list: they skip the hold, as they do when asked
// for after the run), ending at their first 429 too. It returns once there is
// nothing left to check, or ctx ends, and reports whether it checked (false:
// queued behind the running check).
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
	// pace spaces the run's server reads; unanswered is the failure the hold
	// is set by: of those the server didn't answer, the one asking for the
	// longest wait (the latest when none asked).
	pace := &pacer{spacing: r.spacing}
	var unanswered error
	for {
		gen := r.gen
		var libs []link.Library
		listed := map[int64]bool{}
		// prev: the state each library's check may keep, as the pass starts
		// (record reads the state again: Due may have ended it meanwhile).
		prev := map[int64]link.LibraryStatus{}
		for _, l := range r.libs {
			listed[l.ID] = true
			if len(ids) == 0 || slices.Contains(ids, l.ID) {
				libs = append(libs, l)
				if res, ok := r.results[l.ID]; ok {
					prev[l.ID] = res.status
				}
			}
		}
		r.mu.Unlock()
		// ended: a 429, a refused token or a request that timed out, which
		// the rest of the pass would meet too (a hung server would cost each
		// library the whole request timeout).
		ended := false
		for i, l := range libs {
			if ctx.Err() != nil {
				break
			}
			path, _ := mount(l.ID, l.Root)
			st, err := checkOne(ctx, r.look, src, pace, l, path, listed)
			if ctx.Err() != nil {
				break // cut short: it says nothing about the library or the server
			}
			if last, ok := prev[l.ID]; ok && err != nil {
				st, err = proveKept(st, err, last, r.look.stat) // a look at the folder: not under r.mu
			}
			r.mu.Lock()
			if r.gen != gen {
				r.mu.Unlock()
				break // the link changed: the rest of this pass is the old link's
			}
			at := now()
			r.record(st, err, at)
			var slow folderSlow
			if err != nil && !errors.As(err, &slow) {
				if unanswered == nil || retryAfter(err) >= retryAfter(unanswered) {
					unanswered = err
				}
				_, limited := link.RateLimited(err)
				// ctx is live here, so a deadline is the request's own timeout.
				ended = limited || errors.Is(err, link.ErrUnauthorized) || errors.Is(err, context.DeadlineExceeded)
			}
			r.mu.Unlock()
			if ended {
				r.keepUnreached(libs[i+1:], prev, mount, err, at, gen)
				break
			}
		}
		r.mu.Lock()
		if r.gen != gen {
			// The old link's failures say nothing about the new one: its queued
			// checks run now, and only their answers set the hold.
			unanswered, pace.asked = nil, false
		}
		if ctx.Err() != nil || (!r.pendAll && len(r.pend) == 0) {
			// A run that asked the server nothing (every folder missing) says
			// nothing about it either: the hold stands.
			if ctx.Err() == nil && r.gen == gen && (unanswered != nil || pace.asked) {
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

// checkOne checks one library by its kind (Check, or CheckPodcast) with look,
// its server read paced by pace.
func checkOne(ctx context.Context, look looks, src BookSource, pace *pacer, l link.Library, path string, listed map[int64]bool) (link.LibraryStatus, error) {
	marker, ok := src.(MarkerSource)
	checkMarker := func() (link.LibraryStatus, error) {
		return look.checkPodcast(ctx, pacedMarkers{marker, pace}, l, path, func(id int64) bool { return listed[id] })
	}
	if l.LibraryKind() == "podcasts" {
		if !ok {
			return link.LibraryStatus{LibraryID: l.ID, MountPath: path, Error: "the server link cannot check a library marker"}, nil
		}
		return checkMarker()
	}
	if !ok {
		checkMarker = nil
	}
	return look.check(ctx, pacedBooks{src, pace}, l, path, checkMarker)
}

// pacer spaces a run's server reads at least spacing apart. Only a read waits:
// a library whose folder is missing never asks, so it costs the run nothing.
// asked: the run has asked the server since the link last changed.
type pacer struct {
	spacing time.Duration
	last    time.Time
	asked   bool
}

// wait waits out the spacing since the run's last read, or ctx's error.
func (p *pacer) wait(ctx context.Context) error {
	if !p.last.IsZero() {
		if err := netguard.Wait(ctx, time.Until(p.last.Add(p.spacing))); err != nil {
			return err
		}
	}
	p.last, p.asked = time.Now(), true
	return nil
}

// pacedBooks and pacedMarkers are a source whose reads a pacer spaces.
type pacedBooks struct {
	src  BookSource
	pace *pacer
}

func (b pacedBooks) Book(ctx context.Context, libraryID int64, path string) (*link.Book, error) {
	if err := b.pace.wait(ctx); err != nil {
		return nil, err
	}
	return b.src.Book(ctx, libraryID, path)
}

type pacedMarkers struct {
	src  MarkerSource
	pace *pacer
}

func (m pacedMarkers) Marker(ctx context.Context, libraryID int64) (string, error) {
	if err := m.pace.wait(ctx); err != nil {
		return "", err
	}
	return m.src.Marker(ctx, libraryID)
}

// proveKept lets a check the server didn't answer (or whose look at the folder
// timed out, folderSlow) keep last, the library's
// state at the same mount, only while the folder still shows what last's check
// found there (its book's folder or file, or the podcast marker, through stat):
// a share that isn't mounted leaves an empty folder at the mount, where a kept
// "mapped, writable" would let an import create folders on the bare mount
// point. A folder that doesn't show it (gone, or a mount that doesn't answer
// within LookTimeout) makes the failure unproven, so the Registry records it
// and writes wait for a check the server answers, as does a book the last
// check found at the mount itself (nothing there tells it from an empty mount
// point), and a podcast marker that a look still out may have written itself
// (folderSlow.wrote: a look that timed out goes on, and writes a marker where
// it finds none). A kept state that wasn't mapped lets nothing write, so it
// needs no proof.
func proveKept(st link.LibraryStatus, unanswered error, last link.LibraryStatus, stat func(string) (fs.FileInfo, error)) (link.LibraryStatus, error) {
	var lost unproven
	if !last.Mapped || last.MountPath != st.MountPath || errors.As(unanswered, &lost) {
		return st, unanswered
	}
	st.Mapped, st.Writable = false, false
	if last.CheckedPath == "" {
		// A book at the library's own folder: an unmounted share's empty
		// mount point shows that folder too, so nothing here proves it.
		st.Error = "the last check found the book at the library folder itself, which proves nothing without the server, and " + noAnswer(unanswered)
		return st, unproven{unanswered}
	}
	full, err := SafeJoin(st.MountPath, last.CheckedPath)
	if err == nil {
		_, err = stat(full)
	}
	var slow folderSlow
	switch {
	case err != nil:
		st.Error = lostSight(last.CheckedPath+", which the last check found", err) + ", and " + noAnswer(unanswered)
	case errors.As(unanswered, &slow) && slow.wrote != nil && slow.wrote():
		// Asked only once the stat answered: a marker it saw was begun before.
		st.Error = "the folder shows " + last.CheckedPath + ", but a look of this check may have written it, which proves nothing, and " + noAnswer(unanswered)
	default:
		return st, unanswered
	}
	return st, unproven{unanswered}
}

// keepUnreached records the libraries a pass ended before (a 429, a refused
// token, a timeout: ended) as an unanswered check of each, so record keeps a
// library's last state, due again after the hold, only as it would that
// check's: at the same mount, and while the folder still shows what that
// state's check found (proveKept, outside r.mu), unknown past KeptChecks
// intervals. Otherwise the failure is recorded, unknown, so writes wait for a
// check the server answers. One with no check yet has nothing to keep and is
// due anyway.
func (r *Registry) keepUnreached(rest []link.Library, prev map[int64]link.LibraryStatus, mount MountFunc, ended error, at time.Time, gen int) {
	for _, l := range rest {
		last, ok := prev[l.ID]
		if !ok {
			continue
		}
		path, _ := mount(l.ID, l.Root)
		st := link.LibraryStatus{LibraryID: l.ID, MountPath: path,
			Error: "not checked: the server stopped the pass at another library (" + ended.Error() + ")"}
		st, err := proveKept(st, ended, last, r.look.stat)
		r.mu.Lock()
		if r.gen != gen {
			r.mu.Unlock()
			return // the link changed: the old link's failure says nothing now
		}
		r.record(st, err, at)
		r.mu.Unlock()
	}
}

// record keeps a library's check (r.mu held). One the server didn't answer
// (unanswered) leaves the last known state at the same mount in place, due
// again after the hold, marked expired once KeptChecks intervals after the
// check that set it have passed (view reads it as unknown, with its own time).
// With no state there, or one whose folder no longer showed what proved it
// (unproven), the failure is recorded, unknown, so the page says why and
// writes wait for a check the server answers. One whose look at the folder
// timed out (folderSlow) is the same, due again after LookAgain rather than
// after the hold, and recorded afresh over an unknown failure (its reason is
// the newer).
func (r *Registry) record(st link.LibraryStatus, unanswered error, at time.Time) {
	prev, ok := r.results[st.LibraryID]
	var lost unproven
	var slow folderSlow
	isSlow := errors.As(unanswered, &slow)
	if unanswered != nil && ok && prev.status.MountPath == st.MountPath && !errors.As(unanswered, &lost) &&
		(!isSlow || !prev.unknown) {
		r.keep(st.LibraryID, at, isSlow)
		return
	}
	res := result{status: st, at: at, retry: unanswered != nil && !isSlow, unknown: unanswered != nil}
	if isSlow {
		res.slow = at
	}
	r.results[st.LibraryID] = res
}

// keep keeps a checked library's state from before the check at that got no
// definite answer (r.mu held), expired once it is past KeptChecks intervals;
// one with no check yet is due anyway. The library is due again after the
// hold (retry), or, when the check's look at the folder timed out (slow),
// LookAgain after at: a retry would make it due at once when no hold stands,
// and the run's beat would check again straight away, a tight loop on a hung
// mount.
func (r *Registry) keep(id int64, at time.Time, slow bool) {
	if res, ok := r.results[id]; ok {
		res.retry, res.unanswered, res.slow = true, at, time.Time{}
		if slow {
			res.retry, res.unanswered, res.slow = false, time.Time{}, at
		}
		res.expired = res.expired || r.keptTooLong(res.at, at)
		r.results[id] = res
	}
}

// holdAfter sets when Due may next start a run (r.mu held): nothing to wait
// for after a run the server answered throughout; after one it didn't
// (unanswered), the answer's Retry-After, else RetryFirst doubling with each
// such run in a row, at most RetryMax. A failure without a Retry-After (a
// manual check's, run through a hold) never shortens a longer hold in place:
// only the server's own answer moves that, and a run that doesn't move the
// hold doesn't count toward the doubling either.
func (r *Registry) holdAfter(unanswered error, now time.Time) {
	if unanswered == nil {
		r.held, r.holdUntil, r.holdWhy = 0, time.Time{}, ""
		return
	}
	held := r.held + 1
	retry := link.APIError{} // no answer: no Retry-After
	if wait := retryAfter(unanswered); wait > 0 {
		// A zero Retry-After (or a date already past, a skewed clock) would
		// end the hold at once: the run's Kick would beat, find the libraries
		// due and ask again, a tight loop. It counts as none.
		retry.RetryAfter, retry.RetryAfterSet = wait, true
	}
	wait, ok := checkHold.Wait(&retry, held)
	if !ok {
		wait = RetryMax // Backoff gives up past Max; a hold stops there
	}
	if until := now.Add(wait); retry.RetryAfterSet || until.After(r.holdUntil) {
		r.held, r.holdUntil, r.holdWhy = held, until, unanswered.Error()
	}
}

// retryAfter is the wait a failure's answer asked for (its Retry-After), 0
// when it sent none or there was no answer.
func retryAfter(err error) time.Duration {
	var apiErr *link.APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return 0
}

// Held reports the hold a run the server didn't answer left: when Due may next
// start a run, and that run's failure ("" when there is no hold).
func (r *Registry) Held() (until time.Time, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.holdUntil, r.holdWhy
}

// checkHold is the hold's policy as a link.Backoff that never runs out of
// tries.
var checkHold = link.Backoff{Tries: math.MaxInt, First: RetryFirst, Max: RetryMax}
