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
// nonce back from disk. A marker created for a failed check is removed.
func CheckPodcast(ctx context.Context, src MarkerSource, lib link.Library, mount string) link.LibraryStatus {
	st := link.LibraryStatus{LibraryID: lib.ID, MountPath: mount, CheckedPath: MarkerPath}
	root, err := os.OpenRoot(mount)
	if err != nil {
		st.Error = "the library folder is " + describeStatError(err)
		return st
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
		return st
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		st.Error = "could not read the marker folder"
		return st
	}
	marker, err = readMarker(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := ProbeWritable(mount); err != nil {
			st.Error = "the library is not writable here"
			return st
		}
		if info == nil {
			if err := root.Mkdir(".audiosilo", 0o755); err != nil {
				st.Error = "could not create the marker folder"
				return st
			}
			createdDir = true
			if err := root.Chmod(".audiosilo", 0o755); err != nil {
				st.Error = "could not set marker folder permissions"
				return st
			}
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			st.Error = "could not create a marker nonce"
			return st
		}
		marker = libraryMarker{Version: 1, LibraryID: lib.ID, Nonce: hex.EncodeToString(nonce[:])}
		if err := writeMarker(root, marker); err != nil {
			st.Error = "could not write the library marker"
			return st
		}
		createdMarker = true
	} else if err != nil {
		st.Error = "the library marker is invalid"
		return st
	}
	if marker.LibraryID != lib.ID {
		st.Error = "the local marker names another library"
		return st
	}
	nonce, err := src.Marker(ctx, lib.ID)
	if err != nil || nonce != marker.Nonce {
		st.Error = "this folder isn't the library the server sees"
		return st
	}
	st.Mapped = true
	if err := ProbeWritable(mount); err != nil {
		st.Error = "mapped, but not writable here: " + describeStatError(err)
		return st
	}
	st.Writable = true
	return st
}

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
	tmp := ".audiosilo/.audiosilo-tmp-" + m.Nonce
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = root.Remove(tmp) }()
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := root.Lstat(MarkerPath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("marker appeared during the check")
	}
	return root.Rename(tmp, MarkerPath)
}
