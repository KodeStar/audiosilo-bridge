package podcast

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
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
// whose own feed.json cannot be read refuses too: it may be the show's. So
// does one still there without a feed.json that holds audio or a partial of
// the Bridge's (stillHolds, ErrHeld): that is the show's own folder that lost
// its feed.json, so a copy of it (a backup) is never taken in, which would
// leave the audio and the downloads behind. A new empty folder made at the
// old name after a rename by hand holds neither.
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
	if sameFolder(root, from, show) {
		// The old name spells this very folder otherwise: a rename that changed
		// only its case on a case-insensitive filesystem (APFS, SMB), or its
		// normal form on a normalising one. A rename, when show is the folder's
		// name on disk; the old spelling is never adopted back.
		if real, err := RealName(root, show); err != nil || real != show {
			return nil, "", ErrInvalidFile
		}
		return f, from, nil
	}
	held, err := readFile(root, from)
	switch {
	case err == nil:
		if held.ShowID == f.ShowID {
			return nil, "", ErrInvalidFile
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, "", ErrInvalidFile
	case stillHolds(root, from):
		return nil, from, ErrHeld
	}
	return f, from, nil
}

// sameFolder reports whether a and b, two spellings of a folder's name in
// root, name the same folder (a case-insensitive or normalising filesystem).
func sameFolder(root *os.Root, a, b string) bool {
	x, err := root.Lstat(a)
	if err != nil {
		return false
	}
	y, err := root.Lstat(b)
	return err == nil && os.SameFile(x, y)
}

// RealName is name (a show folder, or any path in root) as its parent folder
// spells its last component on disk. A case-insensitive filesystem (macOS
// APFS, SMB shares) opens "Show" for a folder renamed by hand to "show", so a
// job naming the old spelling would read, lock and write the folder under a
// name that is no longer its own. name itself when that is how it is spelt,
// or when no entry of its parent is the same file; the Lstat's error when
// name is not there.
func RealName(root *os.Root, name string) (string, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return name, err
	}
	dir, base := path.Split(name)
	parent := strings.TrimSuffix(dir, "/")
	if parent == "" {
		parent = "."
	}
	d, err := root.Open(parent)
	if err != nil {
		return name, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return name, err
	}
	if slices.Contains(names, base) {
		return name, nil
	}
	for _, n := range names {
		if other, err := root.Lstat(dir + n); err == nil && os.SameFile(st, other) {
			return dir + n, nil
		}
	}
	return name, nil
}

// ErrHeld is ReadMoved's refusal of a folder whose old name is still there
// without a feed.json, holding audio or an owned temp (stillHolds). It is an
// ErrInvalidFile. Nothing lifts it but those files (or the copy) going by
// hand, as no sweep reads a folder with no feed.json, so the job that meets
// it says so in the log: ReadMoved answers the old name with it, the one
// refusal that names a folder.
var ErrHeld = fmt.Errorf("%w: the folder its episodes name still holds audio or a partial", ErrInvalidFile)

// stillHolds reports whether from, the old name of a folder whose feed.json
// names its episodes there, is still there holding an audio file or an owned
// temp (a download's partial, an install's staged audio), or cannot be listed
// to tell (listFolder: a link or a file there refuses too). Any audio file
// counts, not only the episodes the feed.json read names: a backup made
// before an episode joined the feed does not name it, and a filesystem that
// normalises names spells an episode's otherwise. A folder that is gone holds
// nothing.
func stillHolds(root *os.Root, from string) bool {
	entries, err := listFolder(root, from)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if OwnedTemp(e.Name()) || IsAudioExtension(strings.TrimPrefix(strings.ToLower(path.Ext(e.Name())), ".")) {
			return true
		}
	}
	return false
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
