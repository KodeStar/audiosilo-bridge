//go:build !windows

package podcastjob

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
func unlockFile(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func probeInput(cmd *exec.Cmd, f *os.File) string {
	cmd.ExtraFiles = []*os.File{f}
	return "file:/dev/fd/3"
}
