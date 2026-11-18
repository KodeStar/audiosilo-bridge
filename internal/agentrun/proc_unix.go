//go:build unix

package agentrun

import (
	"io/fs"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// supported reports whether the runner can run here (it needs process groups).
const supported = true

// setProcGroup puts the CLI in a session of its own (and so a process group of
// its own, the CLI's pid), so a cancel signals every helper it started, and
// nothing it starts can join another run's group (a group is joined only
// within its session): the endpoint service's sweep tells a run's processes by
// their session (sweep.go).
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// signalGroup signals the CLI's whole process group (gone already is fine).
func signalGroup(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}

// exitCode is the CLI's exit code; a signal is 128+signal, as a shell reports it.
func exitCode(ps *os.ProcessState, werr error) int {
	if ps == nil {
		if werr != nil {
			return -1
		}
		return 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// setUmask sets the process umask (inherited by every CLI the runner starts).
func setUmask(mask int) { syscall.Umask(mask) }

// enterable reports whether this process may write or search dir (by its
// effective ids, as a later open would be judged).
func enterable(dir string) bool {
	return unix.Faccessat(unix.AT_FDCWD, dir, unix.W_OK, unix.AT_EACCESS) == nil ||
		unix.Faccessat(unix.AT_FDCWD, dir, unix.X_OK, unix.AT_EACCESS) == nil
}

// ownerUID is info's owner, as this process's user namespace shows it; ok
// false where the OS reports none.
func ownerUID(info fs.FileInfo) (uid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
