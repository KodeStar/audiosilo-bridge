//go:build linux || darwin

package transfer

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
)

// Link into the directory opened through the library root. Only the source
// path is absolute and it belongs to the caller's private staging folder.
func linkSource(root *os.Root, dir, src, name string) (bool, error) {
	parent, err := root.Open(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = parent.Close() }()
	err = unix.Linkat(unix.AT_FDCWD, src, int(parent.Fd()), name, 0)
	if errors.Is(err, unix.EXDEV) || fsutil.LinkUnsupported(err) || errors.Is(err, unix.EACCES) {
		return false, nil
	}
	return err == nil, err
}
