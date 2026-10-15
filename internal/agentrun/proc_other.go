//go:build !unix

package agentrun

import (
	"os"
	"os/exec"
	"syscall"
)

// The agent runner runs in a Linux container; elsewhere it builds but refuses
// to start (see supported).

const noFollow = 0

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
