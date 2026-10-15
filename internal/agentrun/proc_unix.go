//go:build unix

package agentrun

import (
	"os"
	"os/exec"
	"syscall"
)

// noFollow makes an open refuse a symlink in the last component.
const noFollow = syscall.O_NOFOLLOW

// supported reports whether the runner can run here (it needs process groups).
const supported = true

// setProcGroup puts the CLI in its own process group, so a cancel signals every
// helper it started.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
