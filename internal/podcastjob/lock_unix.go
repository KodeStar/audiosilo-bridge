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

// releaseLock removes the episode's .fetch.lock while still holding it, then
// lets go: a job waiting on the removed file finds it gone (lockedAt) and
// opens the path again, so the files never pile up and no two jobs hold one.
func releaseLock(root *os.Root, p string, f *os.File) {
	_ = root.Remove(p)
	unlockFile(f)
	_ = f.Close()
}

func probeInput(cmd *exec.Cmd, f *os.File) string {
	cmd.ExtraFiles = []*os.File{f}
	return "file:/dev/fd/3"
}
