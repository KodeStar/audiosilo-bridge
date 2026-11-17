package agentbox

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// posixACLs are the xattrs a POSIX ACL lives in: a file's access ACL and a
// dir's default one.
var posixACLs = []string{"system.posix_acl_access", "system.posix_acl_default"}

// dropACL removes the POSIX ACLs f inherited from a parent dir that carries a
// default ACL, leaving f's mode alone to say who may read it. None to remove
// (ENODATA) and a filesystem without ACLs (ENOTSUP, EOPNOTSUPP) are no error.
func dropACL(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var rerr error
	if err := conn.Control(func(fd uintptr) {
		for _, name := range posixACLs {
			err := unix.Fremovexattr(int(fd), name)
			if err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EOPNOTSUPP) {
				rerr = &os.PathError{Op: "removexattr", Path: f.Name(), Err: err}
				return
			}
		}
	}); err != nil {
		return err
	}
	return rerr
}
