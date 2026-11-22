//go:build linux || darwin

package transfer

import (
	"errors"
	"os"
	"runtime"

	"golang.org/x/sys/unix"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
)

// Link into the directory opened through the library root. Only the source
// path is absolute and it belongs to the caller's private staging folder. A
// folder where a copy would come out another way (its group, a default ACL)
// gets the copy instead.
func linkSource(root *os.Root, dir, src, name string) (bool, error) {
	parent, err := root.Open(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = parent.Close() }()
	fd := int(parent.Fd())
	if !linkLikeCopy(fd, src) {
		return false, nil
	}
	err = unix.Linkat(unix.AT_FDCWD, src, fd, name, 0)
	if errors.Is(err, unix.EXDEV) || fsutil.LinkUnsupported(err) || errors.Is(err, unix.EACCES) {
		return false, nil
	}
	return err == nil, err
}

// linkLikeCopy reports whether src linked into the folder fd keeps what a copy
// made there gets: the folder's group on macOS (BSD rules) and in a
// set-group-ID folder (a shared library folder's usual setup), else this
// process's; and, on Linux, no default ACL, which would set a copy's
// permissions. Anything it can't read counts as no.
func linkLikeCopy(fd int, src string) bool {
	var folder, file unix.Stat_t
	if unix.Fstat(fd, &folder) != nil || unix.Lstat(src, &file) != nil {
		return false
	}
	gid := uint32(os.Getegid()) //nolint:gosec // G115: a gid fits in 32 bits
	if runtime.GOOS == "darwin" || uint32(folder.Mode)&unix.S_ISGID != 0 {
		gid = folder.Gid
	}
	if file.Gid != gid {
		return false
	}
	if runtime.GOOS == "linux" {
		_, err := unix.Fgetxattr(fd, "system.posix_acl_default", nil)
		return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
	}
	return true
}
