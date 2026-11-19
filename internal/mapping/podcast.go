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
// read, a stale marker's removal, the write probe and the marker's write) is
// bounded by LookTimeout as one (lookPodcast), as is the write probe after it:
// a hung mount ends the check, unanswered folderSlow, and what the look wrote
// is undone once it answers.
func CheckPodcast(ctx context.Context, src MarkerSource, lib link.Library, mount string, current func(int64) bool) (st link.LibraryStatus, unanswered error) {
	return bounded.checkPodcast(ctx, src, lib, mount, current)
}

// checkPodcast is CheckPodcast with these looks.
func (l looks) checkPodcast(ctx context.Context, src MarkerSource, lib link.Library, mount string, current func(int64) bool) (st link.LibraryStatus, unanswered error) {
	st = link.LibraryStatus{LibraryID: lib.ID, MountPath: mount, CheckedPath: MarkerPath}
	op := "podcast marker " + strconv.FormatInt(lib.ID, 10)
	writes := markerWritesAt(op, mount)
	found, err := l.podcast(op, mount, func(m string) (podcastFound, error) { return lookPodcast(m, lib.ID, current, writes), nil })
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
	nonce, err := src.Marker(ctx, lib.ID)
	if err != nil || nonce != found.marker.Nonce {
		var api *link.APIError
		switch {
		case errors.Is(err, link.ErrNoMarker), err == nil:
			st.Error = "this folder isn't the library the server sees"
		case errors.As(err, &api) && api.Code == "library_not_found":
			st.Error = "the library was removed from the server"
		default:
			st.Error = "could not verify this folder with the server"
		}
		unanswered = unsettled(err)
		if unanswered != nil && found.createdMarker {
			unanswered = unproven{unanswered}
		}
		return st, unanswered
	}
	st.Mapped = true
	if err := l.probe(mount); err != nil {
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

// podcastWithin bounds a podcast check's look (lookPodcast) by d as one change
// (fsutil.ChangeWithin, one out per library and mount): a look that answers
// after every caller gave up is undone, so a marker it wrote is not left for
// nobody to clean up.
func podcastWithin(d time.Duration) func(op, mount string, look func(string) (podcastFound, error)) (podcastFound, error) {
	return func(op, mount string, look func(string) (podcastFound, error)) (podcastFound, error) {
		return fsutil.ChangeWithin(op, mount, d, look, func(f podcastFound) {
			f.undo(mount)
			f.write.settle()
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
		if current, err := readMarker(root); err == nil && current.Nonce == f.marker.Nonce {
			_ = root.Remove(MarkerPath)
		}
	}
	if f.createdDir {
		_ = root.Remove(".audiosilo")
	}
}

// lookPodcast is a podcast check's look at mount before the server's read,
// unbounded (checkPodcast bounds it as one): it opens the folder, reads the
// marker, replaces one of a removed library, and when there is none probes
// the folder for writing and writes one for library id, its write counted in
// writes.
func lookPodcast(mount string, id int64, current func(int64) bool, writes *markerWrites) (f podcastFound) {
	root, err := os.OpenRoot(mount)
	if err != nil {
		f.problem = "the library folder is " + describeStatError(err)
		return f
	}
	defer func() { _ = root.Close() }()
	fail := func(problem string) podcastFound {
		f.undoIn(root)
		return podcastFound{problem: problem}
	}
	info, err := root.Lstat(".audiosilo")
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fail("the marker folder must not be a symlink")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail("could not read the marker folder")
	}
	marker, err := readMarker(root)
	if err == nil && marker.LibraryID != id && !current(marker.LibraryID) {
		if err := root.Remove(MarkerPath); err != nil {
			return fail("could not replace the marker of a removed library")
		}
		err = os.ErrNotExist
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := probeWritable(mount); err != nil {
			return fail("the library is not writable here: " + describeStatError(err))
		}
		if info == nil {
			if err := root.Mkdir(".audiosilo", 0o755); err != nil {
				return fail("could not create the marker folder")
			}
			f.createdDir = true
			if err := root.Chmod(".audiosilo", 0o755); err != nil {
				return fail("could not set marker folder permissions")
			}
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return fail("could not create a marker nonce")
		}
		marker = libraryMarker{Version: 1, LibraryID: id, Nonce: hex.EncodeToString(nonce[:])}
		write := writes.begin()
		if err := writeMarker(root, marker); err != nil {
			defer write.settle() // once fail has undone what the look made
			return fail("could not write the library marker")
		}
		f.marker, f.createdMarker, f.write = marker, true, write
		return f
	} else if err != nil {
		return fail("the library marker is invalid")
	}
	if marker.LibraryID != id {
		return fail("the local marker names another library")
	}
	f.marker = marker
	return f
}

// unproven is an unanswered check of a folder that no longer shows what proved
// the library's last state: a podcast folder that held no marker of the library
// (CheckPodcast wrote one for the read, and removed it again), or one without
// the book or marker the last check found (proveKept). The Registry records the
// failure instead of keeping that state, so writes wait for a check the server
// answers.
type unproven struct{ error }

func (e unproven) Unwrap() error { return e.error }

func readMarker(root *os.Root) (libraryMarker, error) {
	var m libraryMarker
	info, err := root.Lstat(MarkerPath)
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return m, errors.New("invalid marker")
	}
	f, err := root.Open(MarkerPath)
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
func writeMarker(root *os.Root, m libraryMarker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return fsutil.WriteRootAtomic(root, ".audiosilo", "library.json", b, 0o644, func() error {
		if _, err := root.Lstat(MarkerPath); !errors.Is(err, os.ErrNotExist) {
			return errors.New("marker appeared during the check")
		}
		return nil
	})
}
