package podcast

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256 reports whether s is a SHA-256 as a Fetch records it: 64
// lowercase hex digits.
func ValidSHA256(s string) bool { return shaPattern.MatchString(s) }

func ValidID(s string) bool { return showIDPattern.MatchString(s) }
func ValidEpisodePath(s string) bool {
	p := strings.Split(s, "/")
	return len(p) == 2 && ValidShow(p[0]) && ValidShow(p[1]) && IsAudioExtension(strings.TrimPrefix(strings.ToLower(path.Ext(s)), "."))
}
func (f *Fetch) Valid() bool {
	if f == nil || f.Generation < 1 || !shaPattern.MatchString(f.SHA256) || f.Bytes <= 0 || f.DurationMS <= 0 {
		return false
	}
	if _, err := time.Parse(time.RFC3339, f.FetchedAt); err != nil {
		return false
	}
	if f.DeletedAt != "" {
		if _, err := time.Parse(time.RFC3339, f.DeletedAt); err != nil {
			return false
		}
	}
	return true
}

// NextFetch pins a new hash while retaining the historical timeline warning.
func NextFetch(previous *Fetch, received Fetch) (Fetch, error) {
	received.Generation = 1
	received.DeletedAt = ""
	if previous != nil {
		if !previous.Valid() {
			return Fetch{}, ErrInvalidFile
		}
		received.Generation = previous.Generation
		received.TimelineChanged = previous.TimelineChanged
		if previous.SHA256 != received.SHA256 {
			if previous.Generation == math.MaxInt {
				return Fetch{}, ErrInvalidFile
			}
			received.Generation++
			received.TimelineChanged = true
		}
	}
	if !received.Valid() {
		return Fetch{}, ErrInvalidFile
	}
	return received, nil
}

// LstatRegular is the regular file at name, by OpenRegular's rule (no link in
// any component), without opening it: what a removal needs, which a file it
// cannot read must not stop.
func LstatRegular(root *os.Root, name string) (os.FileInfo, error) {
	parts := strings.Split(name, "/")
	var named os.FileInfo
	for i, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "\\\x00") {
			return nil, ErrInvalidFile
		}
		st, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, err
		}
		if i < len(parts)-1 {
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return nil, ErrInvalidFile
			}
		} else if !st.Mode().IsRegular() {
			return nil, ErrInvalidFile
		}
		named = st
	}
	return named, nil
}

// OpenRegular rejects links in every component, including links within the root.
// As agentproto.OpenRegular does, O_NONBLOCK keeps a FIFO swapped in after the
// Lstat from blocking the open, and the open file must be the one the Lstat saw.
func OpenRegular(root *os.Root, name string) (*os.File, error) {
	return OpenRegularFile(root, name, os.O_RDONLY)
}

// OpenRegularFile is OpenRegular with the open's access flag (os.O_RDWR for an
// append); any other flag is dropped, so it never creates, truncates or
// appends before the file is proven the one the Lstat saw.
func OpenRegularFile(root *os.Root, name string, flag int) (*os.File, error) {
	named, err := LstatRegular(root, name)
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, flag&(os.O_WRONLY|os.O_RDWR)|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || !os.SameFile(named, st) {
		_ = f.Close()
		return nil, ErrInvalidFile
	}
	return f, nil
}

func Matches(root *os.Root, name string, fetch *Fetch) bool {
	return fetch.Valid() && Holds(root, name, fetch.Bytes, fetch.SHA256)
}

// Holds reports whether name is a regular file of size bytes whose hex
// sha256 is sum. A file that could not be read to tell does not hold them
// (Verify tells the two apart).
func Holds(root *os.Root, name string, size int64, sum string) bool {
	ok, err := Verify(root, name, size, sum)
	return ok && err == nil
}

// Verify is Holds for a caller that must tell a file proven to hold other
// content (false, no error: another size, or read in full to another hash)
// from one that could not be read to tell (the error: not there, not a
// regular file, or a read that failed, as a network share's does for a
// moment). Only the first says the content changed.
func Verify(root *os.Root, name string, size int64, sum string) (bool, error) {
	f, err := OpenRegular(root, name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if st.Size() != size {
		return false, nil
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, size+1))
	if err != nil {
		return false, err
	}
	return n == size && hex.EncodeToString(h.Sum(nil)) == sum, nil
}

func AttachmentPath(episode, suffix string) string {
	return path.Dir(episode) + "/.audiosilo/" + strings.TrimSuffix(path.Base(episode), path.Ext(episode)) + suffix
}
