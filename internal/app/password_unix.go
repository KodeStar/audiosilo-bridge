//go:build unix

package app

import (
	"os"
	"syscall"
)

// checkOwner applies ownerRefusal to dir's owner.
func checkOwner(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return ownerRefusal(os.Geteuid(), int(st.Uid), int(st.Gid), dir)
}
