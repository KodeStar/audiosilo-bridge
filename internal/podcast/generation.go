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
	"time"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

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

// OpenRegular rejects links in every component, including links within the root.
func OpenRegular(root *os.Root, name string) (*os.File, error) {
	parts := strings.Split(name, "/")
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
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, ErrInvalidFile
	}
	return f, nil
}

func Matches(root *os.Root, name string, fetch *Fetch) bool {
	if !fetch.Valid() {
		return false
	}
	f, err := OpenRegular(root, name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() != fetch.Bytes {
		return false
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, fetch.Bytes+1))
	return err == nil && n == fetch.Bytes && hex.EncodeToString(h.Sum(nil)) == fetch.SHA256
}

func AttachmentPath(episode, suffix string) string {
	return path.Dir(episode) + "/.audiosilo/" + strings.TrimSuffix(path.Base(episode), path.Ext(episode)) + suffix
}
