//go:build unix

package agentbox

import (
	"errors"
	"io/fs"
	"syscall"
)

// fileOwner is info's owner uid and link count; ok false where the OS reports
// neither.
func fileOwner(info fs.FileInfo) (uid int, links uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), uint64(st.Nlink), true //nolint:unconvert // Nlink is uint16 on darwin, uint64 on linux
}

// groupRefused reports whether err, giveGroup's, is the staging volume
// refusing the group: a chown or an ACL change it doesn't permit (EPERM: NFS
// with root squash, a CIFS mount's fixed owner) or doesn't support (ENOTSUP,
// EOPNOTSUPP). Only that falls back to the every-uid hand-off; any other
// error (an I/O error, a full volume) fails the stage instead.
func groupRefused(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
