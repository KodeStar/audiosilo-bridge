package podcast

import (
	"errors"
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// renameNoReplace uses renameat2(RENAME_NOREPLACE), which the kernel makes
// atomic. A filesystem that lacks it (EINVAL, ENOSYS, EOPNOTSUPP) gets the
// absence check under the locks instead.
func renameNoReplace(root *os.Root, oldname, newname string) error {
	if dir := path.Dir(oldname); dir == path.Dir(newname) {
		if d, err := root.Open(dir); err == nil {
			fd := int(d.Fd())
			err = unix.Renameat2(fd, path.Base(oldname), fd, path.Base(newname), unix.RENAME_NOREPLACE)
			_ = d.Close()
			switch {
			case err == nil:
				return nil
			case errors.Is(err, unix.EEXIST):
				return ErrInvalidFile
			case !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EOPNOTSUPP):
				return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: err}
			}
		}
	}
	return renameIfAbsent(root, oldname, newname)
}
