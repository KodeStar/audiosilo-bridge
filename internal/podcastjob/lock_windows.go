package podcastjob

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

func tryLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}
func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}

// releaseLock lets go, then removes the episode's .fetch.lock. Windows can't
// delete a file another job has open, so the removal fails (and is ignored)
// whenever anyone could still lock it.
func releaseLock(root *os.Root, p string, f *os.File) {
	unlockFile(f)
	_ = f.Close()
	_ = root.Remove(p)
}

func probeInput(_ *exec.Cmd, f *os.File) string { return f.Name() }
