package agentproto

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Reading a file the other side wrote. Both sides share one copy of these, so
// the Bridge reading the runner's outputs and the runner reading the Bridge's
// request get the same guarantees: every read goes through an os.Root (no path,
// and no link in the middle of one, reaches outside the dir), a link in the
// last place is refused rather than followed, a FIFO or a device never blocks
// the open, and a file over its size limit is refused.

// ErrNotRegular marks a name that isn't a regular file (a link, a dir, a FIFO,
// a device), or that was swapped for something else while it was opened.
var ErrNotRegular = errors.New("not a regular file")

// ErrTooLarge marks a file over the size limit its reader allows.
var ErrTooLarge = errors.New("over the size limit")

// OpenRegular opens name read-only inside root and checks it is a regular file.
// The Lstat refuses a link before anything is opened; O_NONBLOCK keeps a FIFO
// swapped in after the Lstat from blocking the open; and the open file must be
// the very file the Lstat saw, so a swap in between is refused too.
func OpenRegular(root *os.Root, name string) (*os.File, error) {
	f, _, err := openRegular(root, name)
	return f, err
}

// openRegular is OpenRegular, with the open file's FileInfo.
func openRegular(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s: %w", name, ErrNotRegular)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s: %w", name, ErrNotRegular)
	}
	return f, info, nil
}

// ReadRegular reads a regular file inside root whole, refusing one over limit
// bytes (checked on the open file and again while reading, since the writer
// may still be appending).
func ReadRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	f, info, err := openRegular(root, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	tooLarge := fmt.Errorf("%s is over %d bytes: %w", name, limit, ErrTooLarge)
	if info.Size() > limit {
		return nil, tooLarge
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, tooLarge
	}
	return b, nil
}

// Exists reports whether name exists inside root. A link counts and is never
// followed: a job dir's markers (claim, done, cancel) are checked this way.
func Exists(root *os.Root, name string) bool {
	_, err := root.Lstat(name)
	return err == nil
}
