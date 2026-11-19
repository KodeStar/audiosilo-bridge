//go:build !unix

package agentrun

import (
	"io/fs"
	"os"
	"os/exec"
	"syscall"
)

// The agent runner runs in a Linux container; elsewhere it builds but refuses
// to start (see supported).

const supported = false

func setProcGroup(*exec.Cmd) {}

func signalGroup(pid int, _ syscall.Signal) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func exitCode(ps *os.ProcessState, werr error) int {
	if ps == nil {
		if werr != nil {
			return -1
		}
		return 0
	}
	return ps.ExitCode()
}

func setUmask(int) {}

// enterable can't be told here: count it as reachable.
func enterable(string) bool { return true }

// closeDir makes the folder dir 0700 unless it is that already (a link or a
// file is left as it is: the runner refuses to start here anyway).
func closeDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() == 0o700 {
		return err
	}
	return os.Chmod(dir, 0o700) //nolint:gosec // a folder: 0700 is its owner's alone
}

// ownerUID: a file has no owner here.
func ownerUID(fs.FileInfo) (int, bool) { return 0, false }

// removeRunFile removes nothing here: with no owner, no file is a run's.
func removeRunFile(string, string, int) bool { return false }

// fileKey is a file's device and inode where the system gives them; here it
// gives none, so the deep mount walk stops a link loop by its depth alone.
type fileKey struct{}

// fileKeyOf is false: no device and inode here.
func fileKeyOf(fs.FileInfo) (fileKey, bool) { return fileKey{}, false }
