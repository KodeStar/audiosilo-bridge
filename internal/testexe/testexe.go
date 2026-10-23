// Package testexe writes the fake executables tests run (a fake claude, ffmpeg,
// whisper-cli). Imported by tests only.
//
// Writing an executable while parallel tests fork is racy: a child forked
// between the write's open and close holds the write descriptor until it execs,
// and exec of a file open for writing fails with ETXTBSY ("text file busy").
// WriteFile holds syscall.ForkLock for the write, the standard Go fix: no fork
// can happen while the descriptor is open.
package testexe

import (
	"os"
	"syscall"
)

// WriteFile writes data to path as an executable (0755) with no fork in between.
func WriteFile(path string, data []byte) error {
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	return os.WriteFile(path, data, 0o755) //nolint:gosec // a test's own executable
}
