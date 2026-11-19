package podcast

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
)

var ErrInvalidFile = errors.New("invalid feed.json")

// maxReadBytes is the reader's cap (the server reads up to the same 32 MiB);
// the Bridge writes at most MaxFileBytes.
const maxReadBytes = 32 << 20

var showIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// readHook, when a test sets it, is called once per feed.json parsed.
var readHook func(show string)

// Read parses show's feed.json, whose episodes must all live in show.
func Read(root *os.Root, show string) (*Feed, error) {
	f, err := readFile(root, show)
	if err != nil {
		return nil, err
	}
	if !episodesIn(f, show) {
		return nil, ErrInvalidFile
	}
	return f, nil
}

// readFile parses show's feed.json (a regular file in a real folder) without
// checking which folder its episode paths name.
func readFile(root *os.Root, show string) (*Feed, error) {
	if !ValidShow(show) {
		return nil, ErrInvalidFile
	}
	info, err := root.Lstat(show)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrInvalidFile
	}
	name := show + "/feed.json"
	info, err = root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrInvalidFile
	}
	if info.Size() > maxReadBytes {
		return nil, ErrTooLarge
	}
	rd, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	b, err := io.ReadAll(io.LimitReader(rd, maxReadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxReadBytes {
		return nil, ErrTooLarge
	}
	if readHook != nil {
		readHook(show)
	}
	var f Feed
	if json.Unmarshal(b, &f) != nil || f.Version != Version || !showIDPattern.MatchString(f.ShowID) || len(f.Episodes) > MaxEpisodes {
		return nil, ErrInvalidFile
	}
	return &f, nil
}

// episodesIn reports whether every episode path of f is a distinct audio file
// directly in show.
func episodesIn(f *Feed, show string) bool {
	used := map[string]bool{}
	for _, e := range f.Episodes {
		parts := strings.Split(e.Path, "/")
		if len(parts) != 2 || parts[0] != show || !ValidShow(parts[1]) || path.Clean(e.Path) != e.Path || !IsAudioExtension(strings.TrimPrefix(strings.ToLower(path.Ext(e.Path)), ".")) || used[e.Path] {
			return false
		}
		used[e.Path] = true
	}
	return true
}

// ReadMoved is Read for a show folder that may have been renamed by hand: its
// feed.json still names every episode under the folder's old name. It answers
// that old name, or "" when the episodes live in show (as Read). A feed whose
// old-name folder still holds the same show is a copy, not a rename:
// ErrInvalidFile, as is a feed whose episodes name no single folder. An old
// name that now holds another show (two folders' names swapped by hand, or
// another show moved there) or no show at all is a rename. An old-name folder
// whose own feed.json cannot be read refuses too: it may be the show's.
func ReadMoved(root *os.Root, show string) (*Feed, string, error) {
	f, err := readFile(root, show)
	if err != nil {
		return nil, "", err
	}
	if episodesIn(f, show) {
		return f, "", nil
	}
	from := path.Dir(f.Episodes[0].Path)
	if from == show || !ValidShow(from) || !episodesIn(f, from) {
		return nil, "", ErrInvalidFile
	}
	if held, err := readFile(root, from); err == nil && held.ShowID == f.ShowID || err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", ErrInvalidFile
	}
	return f, from, nil
}

// Write changes updated_at only when another field changed. Removed episode
// descriptions are expendable when the serialized file reaches its cap.
func Write(root *os.Root, show string, f *Feed, now time.Time) (bool, error) {
	if err := prepare(show, f); err != nil {
		return false, err
	}
	old, err := Read(root, show)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if old != nil {
		copyOld, copyNew := *old, *f
		copyOld.UpdatedAt, copyNew.UpdatedAt = "", ""
		a, _ := json.Marshal(copyOld)
		b, _ := json.Marshal(copyNew)
		if bytes.Equal(a, b) {
			f.UpdatedAt = old.UpdatedAt
			return false, nil
		}
	}
	if err := save(root, show, f, now); err != nil {
		return false, err
	}
	return true, nil
}

// writeChanged is Write for a caller that knows f differs from the show's
// feed.json on disk (it changed a feed it read under the same show lock), so
// the file is not read again to compare.
func writeChanged(root *os.Root, show string, f *Feed, now time.Time) error {
	if err := prepare(show, f); err != nil {
		return err
	}
	return save(root, show, f, now)
}

// prepare checks f can be written as show's feed.json and scrubs it.
func prepare(show string, f *Feed) error {
	if !ValidShow(show) {
		return ErrInvalidFile
	}
	if len(f.Episodes) > MaxEpisodes {
		return ErrTooLarge
	}
	f.ScrubPrivate()
	return nil
}

// save writes f, already scrubbed, as show's feed.json updated at now.
func save(root *os.Root, show string, f *Feed, now time.Time) error {
	f.UpdatedAt = now.UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if len(b)+1 > MaxFileBytes {
		for i := range f.Episodes {
			if f.Episodes[i].RemovedAt != "" {
				f.Episodes[i].Description = ""
			}
		}
		b, err = json.MarshalIndent(f, "", "  ")
		if err != nil {
			return err
		}
	}
	if len(b)+1 > MaxFileBytes {
		return ErrTooLarge
	}
	return WriteAtomic(root, show, "feed.json", append(b, '\n'))
}

// WriteAtomic is rooted at the checked mount, with exclusive hidden temporary
// files on the destination filesystem. Symlinked show folders are refused.
func WriteAtomic(root *os.Root, show, name string, data []byte) error {
	if !ValidShow(show) || !ValidShow(name) {
		return ErrInvalidFile
	}
	info, err := root.Lstat(show)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalidFile
	}
	if info, err := root.Lstat(show + "/" + name); err == nil && !info.Mode().IsRegular() {
		return ErrInvalidFile
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsutil.WriteRootAtomic(root, show, name, data, 0o644, nil)
}
