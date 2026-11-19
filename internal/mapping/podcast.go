package mapping

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
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
	found, err := l.podcast(op, mount, func(m string) (podcastFound, error) { return lookPodcast(m, lib.ID, current), nil })
	if err != nil {
		st.Error = "the library folder is " + describeStatError(err)
		return st, slowLook(err)
	}
	if found.problem != "" {
		st.Error = found.problem
		return st, nil
	}
	defer func() {
		if !st.Mapped && (found.createdMarker || found.createdDir) {
			// Bounded too, its answer unneeded: one a hung mount holds goes on
			// in the background (a later one waits on it).
			_, _ = fsutil.LookWithin(op+" undo", mount, LookTimeout, func(m string) (struct{}, error) {
				found.undo(m)
				return struct{}{}, nil
			})
		}
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
}

// podcastWithin bounds a podcast check's look (lookPodcast) by d as one change
// (fsutil.ChangeWithin, one out per library and mount): a look that answers
// after every caller gave up is undone, so a marker it wrote is not left for
// nobody to clean up.
func podcastWithin(d time.Duration) func(op, mount string, look func(string) (podcastFound, error)) (podcastFound, error) {
	return func(op, mount string, look func(string) (podcastFound, error)) (podcastFound, error) {
		return fsutil.ChangeWithin(op, mount, d, look, func(f podcastFound, _ error) { f.undo(mount) })
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
// the folder for writing and writes one for library id.
func lookPodcast(mount string, id int64, current func(int64) bool) (f podcastFound) {
	root, err := os.OpenRoot(mount)
	if err != nil {
		f.problem = "the library folder is " + describeStatError(err)
		return f
	}
	defer func() { _ = root.Close() }()
	fail := func(problem string) podcastFound {
		f.undo(mount)
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
		if err := writeMarker(root, marker); err != nil {
			return fail("could not write the library marker")
		}
		f.marker, f.createdMarker = marker, true
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
