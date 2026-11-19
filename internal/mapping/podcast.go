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
func CheckPodcast(ctx context.Context, src MarkerSource, lib link.Library, mount string, current func(int64) bool) (st link.LibraryStatus, unanswered error) {
	st = link.LibraryStatus{LibraryID: lib.ID, MountPath: mount, CheckedPath: MarkerPath}
	root, err := os.OpenRoot(mount)
	if err != nil {
		st.Error = "the library folder is " + describeStatError(err)
		return st, nil
	}
	defer func() { _ = root.Close() }()
	createdDir, createdMarker := false, false
	var marker libraryMarker
	defer func() {
		if !st.Mapped && createdMarker {
			current, e := readMarker(root)
			if e == nil && current.Nonce == marker.Nonce {
				_ = root.Remove(MarkerPath)
			}
		}
		if !st.Mapped && createdDir {
			_ = root.Remove(".audiosilo")
		}
	}()
	info, err := root.Lstat(".audiosilo")
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		st.Error = "the marker folder must not be a symlink"
		return st, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		st.Error = "could not read the marker folder"
		return st, nil
	}
	marker, err = readMarker(root)
	if err == nil && marker.LibraryID != lib.ID && !current(marker.LibraryID) {
		if err := root.Remove(MarkerPath); err != nil {
			st.Error = "could not replace the marker of a removed library"
			return st, nil
		}
		err = os.ErrNotExist
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := ProbeWritable(mount); err != nil {
			st.Error = "the library is not writable here"
			return st, nil
		}
		if info == nil {
			if err := root.Mkdir(".audiosilo", 0o755); err != nil {
				st.Error = "could not create the marker folder"
				return st, nil
			}
			createdDir = true
			if err := root.Chmod(".audiosilo", 0o755); err != nil {
				st.Error = "could not set marker folder permissions"
				return st, nil
			}
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			st.Error = "could not create a marker nonce"
			return st, nil
		}
		marker = libraryMarker{Version: 1, LibraryID: lib.ID, Nonce: hex.EncodeToString(nonce[:])}
		if err := writeMarker(root, marker); err != nil {
			st.Error = "could not write the library marker"
			return st, nil
		}
		createdMarker = true
	} else if err != nil {
		st.Error = "the library marker is invalid"
		return st, nil
	}
	if marker.LibraryID != lib.ID {
		st.Error = "the local marker names another library"
		return st, nil
	}
	nonce, err := src.Marker(ctx, lib.ID)
	if err != nil || nonce != marker.Nonce {
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
		if unanswered != nil && createdMarker {
			unanswered = unproven{unanswered}
		}
		return st, unanswered
	}
	st.Mapped = true
	if err := ProbeWritable(mount); err != nil {
		st.Error = "mapped, but not writable here: " + describeStatError(err)
		return st, nil
	}
	st.Writable = true
	return st, nil
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
