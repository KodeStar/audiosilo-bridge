//go:build unix

package agentbox

import (
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
