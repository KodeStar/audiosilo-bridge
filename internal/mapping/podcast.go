package mapping

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/link"
)

const MarkerPath = ".audiosilo/library.json"

var noncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type MarkerSource interface {
	Marker(context.Context, int64) (string, error)
}
type libraryMarker struct {
	Version   int    `json:"version"`
	LibraryID int64  `json:"library_id"`
	Nonce     string `json:"nonce"`
}

// CheckPodcast proves an empty library mapping by having the server read a
// nonce back from disk. A marker created for a failed check is removed. A
// marker naming another library is refused while the server still has that
// library (current; the mount shows its folder) and replaced once it doesn't
// (the library was deleted, or this folder added again under a new id).
// unanswered is as Check's: the server gave no definite answer to the marker
// read (a 429, a 5xx, a refused token, no answer at all), so the Registry keeps
// the library's last known state. Not when the folder held no marker of the
// library and one was written for the read (unproven): the folder may be
// another one now (a share that isn't mounted), so the failure is recorded.
// The whole look at the folder before the server's read (its open, the marker
// read, a stale marker's removal, the write probe and the marker's write,
// lookPodcast) is one unit the check waits LookTimeout for, as it does for the
// write probe after it: past that the check ends, unanswered folderSlow, and
// the look goes on. A mount that is slow but working (each step within
// LookTimeout, the look within LookCeiling) still finishes it: that answer,
// with the marker it wrote, is kept for the library's next check (LookKeptFor),
// which takes it instead of looking again, so such a mount is mapped one check
// later rather than never. What any other late look wrote is undone once it
// answers.
func CheckPodcast(ctx context.Context, src MarkerSource, lib link.Library, mount string, current func(int64) bool) (st link.LibraryStatus, unanswered error) {
	return bounded.checkPodcast(ctx, src, lib, mount, current)
}

// checkPodcast is CheckPodcast with these looks.
func (l looks) checkPodcast(ctx context.Context, src MarkerSource, lib link.Library, mount string, current func(int64) bool) (st link.LibraryStatus, unanswered error) {
	st = link.LibraryStatus{LibraryID: lib.ID, MountPath: mount, CheckedPath: MarkerPath}
	op := "podcast marker " + strconv.FormatInt(lib.ID, 10)
	writes := markerWritesAt(op, mount)
	found, err := l.podcast(op, mount, func(m string) (podcastFound, error) {
		return lookPodcast(m, lib.ID, current, writes, time.Now)
	})
	if err != nil {
		st.Error = "the library folder is " + describeStatError(err)
		if !errors.Is(err, fsutil.ErrStatTimeout) {
			return st, nil
		}
		// The look goes on, and writes a marker where it finds none: one the
		// folder shows from here on may be its own (proveKept asks wrote).
		return st, folderSlow{error: err, wrote: writes.unsettled()}
	}
	if found.problem != "" {
		st.Error = found.problem
		return st, nil
	}
	defer func() {
		if st.Mapped || (!found.createdMarker && !found.createdDir) {
			found.write.settle() // the server read it back: the library's marker now
			return
		}
		// Bounded as the look is, and as the look's own change (one out per
		// library and mount): one a hung mount holds goes on in the
		// background, and a later look meanwhile waits on it and is slow,
		// rather than read the marker it is about to remove (and have the
		// server's read of it fail, a definite "not mapped").
		_, _ = l.podcast(op, mount, func(m string) (podcastFound, error) {
			found.undo(m)
			found.write.settle()
			return podcastFound{}, &fs.PathError{Op: op, Path: m, Err: fsutil.ErrStatTimeout}
		})
	}()
	if found.kept {
		// An earlier check's look, which answered after that check had moved
		// on: the folder may be another one since (a share unmounted in
		// between leaves a bare mount point, which the server's read of its
		// own copy would not notice), so it must still show the marker.
		full, err := SafeJoin(mount, MarkerPath)
		if err == nil {
			_, err = l.stat(full)
		}
		if err != nil {
			st.Error = lostSight("the marker an earlier look found", err)
			return st, folderSlow{error: err, wrote: writes.unsettled()}
		}
	}
	st, unanswered = l.verifyMarker(ctx, src, st, found.marker)
	if unanswered != nil && !st.Mapped && found.createdMarker {
		unanswered = unproven{unanswered}
	}
	return st, unanswered
}

// verifyMarker maps st once the server reads back m's nonce, and probes the
// folder for writing.
func (l looks) verifyMarker(ctx context.Context, src MarkerSource, st link.LibraryStatus, m libraryMarker) (link.LibraryStatus, error) {
	nonce, err := src.Marker(ctx, st.LibraryID)
	if err != nil || nonce != m.Nonce {
		var api *link.APIError
		switch {
		case errors.Is(err, link.ErrNoMarker), err == nil:
			st.Error = "this folder isn't the library the server sees"
		case errors.As(err, &api) && api.Code == "library_not_found":
			st.Error = "the library was removed from the server"
		default:
			st.Error = "could not verify this folder with the server"
		}
		return st, unsettled(err)
	}
	st.Mapped = true
	if err := l.probe(st.MountPath); err != nil {
		st.Error = "mapped, but not writable here: " + describeStatError(err)
		return st, slowLook(err)
	}
	st.Writable = true
	return st, nil
}

// podcastFound is what a podcast check's look at its folder found before the
// server's read (lookPodcast): the marker the server is to read back and what
// the look created for it, or why the check fails there (problem; nothing it
// created is left then).
type podcastFound struct {
	marker        libraryMarker
	createdDir    bool
	createdMarker bool
	problem       string
	// write is the marker's write (createdMarker), to settle once the marker
	// is removed again or the server read it back.
	write *markerWrite
	// kept: the look answered after its check had moved on, and was kept for
	// this one (podcastWithin), which looks at the marker again first.
	kept bool
}

// markerWrites counts the markers the looks at one library's folder began to
// write (begun, before the write) and those settled since: removed again, or
// read back by the server (the library's marker then). A look that timed out
// goes on, so until every write is settled a marker the folder shows may be a
// look's own, there only until its undo, and proves nothing of the folder
// (proveKept, through folderSlow.wrote).
type markerWrites struct{ begun, settled atomic.Int64 }

// podcastWrites are the markerWrites by library and mount (a look's op and
// path): one per mapping ever checked.
var podcastWrites sync.Map

func markerWritesAt(op, mount string) *markerWrites {
	w, _ := podcastWrites.LoadOrStore(op+"\x00"+mount, &markerWrites{})
	return w.(*markerWrites)
}

// unsettled returns a func reporting whether a marker's write was unsettled
// now, or has begun since. Called once a stat saw a marker, it tells one a
// look wrote: that write began before the stat's answer, and a write settled
// before now left no marker of its own for the stat to see.
func (w *markerWrites) unsettled() func() bool {
	settled := w.settled.Load()
	return func() bool { return w.begun.Load() != settled }
}

// begin counts a marker's write begun, before it is made.
func (w *markerWrites) begin() *markerWrite {
	w.begun.Add(1)
	return &markerWrite{writes: w}
}

// markerWrite is one marker's write, settled once (nil: no marker written).
type markerWrite struct {
	writes *markerWrites
	once   sync.Once
}

func (m *markerWrite) settle() {
	if m != nil {
		m.once.Do(func() { m.writes.settled.Add(1) })
	}
}

// LookCeiling bounds a podcast check's whole look at its folder (lookPodcast),
// each step of which may take LookTimeout: a mount slower than either is not
// answering, and what the look wrote there is undone.
const LookCeiling = 30 * time.Second

// LookKeptFor is how long a podcast look that answered after its check had
// moved on is kept for the library's next check (due LookAgain after that one,
// at the heartbeat's next beat), before it is undone instead.
const LookKeptFor = 3 * LookAgain

// podcastWithin bounds a check's wait for a podcast look (lookPodcast) by d, as
// one change (fsutil.ChangeWithin, one out per library and mount). A look that
// answers after every caller gave up is kept for the next check, for keptFor,
// when it is a whole one (no problem, and within lookPodcast's own bounds: the
// marker it wrote stands until then), and otherwise undone, as is a kept one
// no check came for, so a marker is never left for nobody to clean up.
func podcastWithin(d, keptFor time.Duration) func(op, mount string, look func(string) (podcastFound, error)) (podcastFound, error) {
	return func(op, mount string, look func(string) (podcastFound, error)) (podcastFound, error) {
		return fsutil.ChangeWithin(op, mount, d, look, fsutil.Late[podcastFound]{
			Undo: func(f podcastFound) {
				f.undo(mount)
				f.write.settle()
			},
			Keep: func(f podcastFound, err error) (podcastFound, bool) {
				f.kept = true
				return f, err == nil && f.problem == ""
			},
			For: keptFor,
		})
	}
}

// undo removes what the look created at mount: its marker (only while it is
// still the one it wrote) and its marker folder (only once empty).
func (f podcastFound) undo(mount string) {
	if !f.createdMarker && !f.createdDir {
		return
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	f.undoIn(root)
}

// undoIn is undo in the mount's root, already open.
func (f podcastFound) undoIn(root *os.Root) {
	if f.createdMarker {
		if current, err := readMarker(root, nil); err == nil && current.Nonce == f.marker.Nonce {
			_ = root.Remove(MarkerPath)
		}
	}
	if f.createdDir {
		_ = root.Remove(".audiosilo")
	}
}

// lookPace times a podcast look's steps: late once one took more than
// LookTimeout, or the look more than LookCeiling (a hung mount, a disk
// spinning up), which says nothing definite about the folder.
type lookPace struct {
	now         func() time.Time
	start, last time.Time
	late        bool
}

func newLookPace(now func() time.Time) *lookPace {
	t := now()
	return &lookPace{now: now, start: t, last: t}
}

// lap ends a step, and reports whether the look is late (nil: never).
func (p *lookPace) lap() bool {
	if p == nil {
		return false
	}
	t := p.now()
	if t.Sub(p.last) > LookTimeout || t.Sub(p.start) > LookCeiling {
		p.late = true
	}
	p.last = t
	return p.late
}

// lookPodcast is a podcast check's look at mount before the server's read: it
// opens the folder, reads the marker, replaces one of a removed library, and
// when there is none probes the folder for writing and writes one for library
// id, its write counted in writes. The check bounds its own wait for the look
// (checkPodcast), not the look, which bounds itself by now's clock: a step
// that took more than LookTimeout, or a look past LookCeiling, ends it as
// fsutil.ErrStatTimeout whatever the step answered, what it made undone, so a
// folder that slow is never a definite failure nor a marker left behind.
func lookPodcast(mount string, id int64, current func(int64) bool, writes *markerWrites, now func() time.Time) (f podcastFound, _ error) {
	pace := newLookPace(now)
	slow := &fs.PathError{Op: "podcast look", Path: mount, Err: fsutil.ErrStatTimeout}
	root, err := os.OpenRoot(mount)
	if pace.lap() {
		if err == nil {
			_ = root.Close()
		}
		return podcastFound{}, slow
	}
	if err != nil {
		f.problem = "the library folder is " + describeStatError(err)
		return f, nil
	}
	defer func() { _ = root.Close() }()
	fail := func(problem string) (podcastFound, error) {
		f.undoIn(root)
		f.write.settle()
		if pace.lap() || problem == "" {
			return podcastFound{}, slow
		}
		return podcastFound{problem: problem}, nil
	}
	// late ends a step: the look is too slow to say anything (fail("")).
	late := pace.lap
	info, err := root.Lstat(".audiosilo")
	if late() {
		return fail("")
	}
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fail("the marker folder must not be a symlink")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("could not read the marker folder")
	}
	marker, err := readMarker(root, pace)
	if late() {
		return fail("")
	}
	if err == nil && marker.LibraryID != id && !current(marker.LibraryID) {
		err = root.Remove(MarkerPath)
		if late() {
			return fail("")
		}
		if err != nil {
			return fail("could not replace the marker of a removed library")
		}
		err = os.ErrNotExist
	}
	if errors.Is(err, os.ErrNotExist) {
		err := probeWritableLaps(mount, func() { pace.lap() })
		if late() {
			return fail("")
		}
		if err != nil {
			return fail("the library is not writable here: " + describeStatError(err))
		}
		if info == nil {
			err := root.Mkdir(".audiosilo", 0o755)
			if err == nil {
				f.createdDir = true
			}
			if late() {
				return fail("")
			}
			if err != nil {
				return fail("could not create the marker folder")
			}
			err = root.Chmod(".audiosilo", 0o755)
			if late() {
				return fail("")
			}
			if err != nil {
				return fail("could not set marker folder permissions")
			}
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return fail("could not create a marker nonce")
		}
		marker = libraryMarker{Version: 1, LibraryID: id, Nonce: hex.EncodeToString(nonce[:])}
		f.write = writes.begin()
		err = writeMarker(root, marker, pace)
		if err == nil {
			f.marker, f.createdMarker = marker, true
		}
		if late() {
			return fail("")
		}
		if err != nil {
			return fail("could not write the library marker")
		}
		return f, nil
	} else if err != nil {
		return fail("the library marker is invalid")
	}
	if marker.LibraryID != id {
		return fail("the local marker names another library")
	}
	f.marker = marker
	return f, nil
}

// unproven is an unanswered check of a folder that no longer shows what proved
// the library's last state: a podcast folder that held no marker of the library
// (CheckPodcast wrote one for the read, and removed it again), or one without
// the book or marker the last check found (proveKept). The Registry records the
// failure instead of keeping that state, so writes wait for a check the server
// answers.
type unproven struct{ error }

func (e unproven) Unwrap() error { return e.error }

// MarkerNames reports whether root holds a valid library marker naming
// libraryID: a missing, unreadable or invalid marker, or one of another
// library, does not. The read is untimed.
func MarkerNames(root *os.Root, libraryID int64) bool {
	m, err := readMarker(root, nil)
	return err == nil && m.LibraryID == libraryID
}

// readMarker reads the folder's marker, each of its steps timed by pace (nil:
// untimed).
func readMarker(root *os.Root, pace *lookPace) (libraryMarker, error) {
	var m libraryMarker
	info, err := root.Lstat(MarkerPath)
	pace.lap()
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return m, errors.New("invalid marker")
	}
	f, err := root.Open(MarkerPath)
	pace.lap()
	if err != nil {
		return m, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return m, err
	}
	if len(b) > 4096 || json.Unmarshal(b, &m) != nil || m.Version != 1 || !noncePattern.MatchString(m.Nonce) {
		return m, errors.New("invalid marker")
	}
	return m, nil
}

// writeMarker writes the folder's marker, its temp file's write and the look
// before the rename timed by pace as a step each (the rename is the caller's).
func writeMarker(root *os.Root, m libraryMarker, pace *lookPace) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return fsutil.WriteRootAtomic(root, ".audiosilo", "library.json", b, 0o644, func() error {
		pace.lap()
		_, err := root.Lstat(MarkerPath)
		pace.lap()
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("marker appeared during the check")
		}
		return nil
	})
}
