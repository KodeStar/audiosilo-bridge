// Package execfile writes files this process is about to exec at once (a test's
// fake tool) so a concurrent fork can't make them busy. The tools toolfetch
// downloads are written plainly instead: their first run is its self-check,
// which retries a run refused as busy (bridge#16).
//
// Writing an executable while another goroutine forks is racy: a child forked
// between the write's open and its close inherits the write descriptor and
// holds it until it execs, and exec of a file open for writing fails with
// ETXTBSY ("text file busy"). os/exec forks holding syscall.ForkLock for
// writing, so holding it for reading from the open to the close means no fork
// can happen while the descriptor exists (bridge#12). Every fork in the process
// waits while it is held, so the writes here should be small, never a download
// or a copy of a large binary.
//
// fsutil.WriteFileAtomic with an executable mode is NOT fork-safe in this way:
// a file it writes that is run at once belongs here.
package execfile

import (
	"os"
	"syscall"
)

// WriteFile writes data to path with perm (os.WriteFile's rules, so the umask
// applies to a new file) with no fork while the file is open.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, data, perm)
}
