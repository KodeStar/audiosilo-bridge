// Package execfile writes files this process is about to exec (a downloaded
// ffmpeg or whisper-cli, a test's fake tool) so a concurrent fork can't make
// them busy.
//
// Writing an executable while another goroutine forks is racy: a child forked
// between the write's open and its close inherits the write descriptor and
// holds it until it execs, and exec of a file open for writing fails with
// ETXTBSY ("text file busy"). os/exec forks holding syscall.ForkLock for
// writing, so holding it for reading from the open to the close means no fork
// can happen while the descriptor exists (bridge#12). Every fork in the process
// waits while it is held, so the writes here should be quick local copies, never
// a download.
package execfile

import (
	"io"
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

// CreateTemp copies r into a new file made by os.CreateTemp(dir, pattern) with
// no fork while the file is open, and returns its name and the bytes copied.
// The file is closed on return; on an error after it was made, the name is
// still returned so the caller can remove it. r should be local (a staged file),
// since forks wait for the copy.
func CreateTemp(dir, pattern string, r io.Reader) (string, int64, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return f.Name(), n, err
}
