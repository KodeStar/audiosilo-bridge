package podcast

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

var ErrInvalidFile = errors.New("invalid feed.json")
var showIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

func Read(root *os.Root, show string) (*Feed, error) {
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
	if !info.Mode().IsRegular() || info.Size() > 32<<20 {
		return nil, ErrInvalidFile
	}
	rd, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	b, err := io.ReadAll(io.LimitReader(rd, (32<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 32<<20 {
		return nil, ErrTooLarge
	}
	var f Feed
	if json.Unmarshal(b, &f) != nil || f.Version != Version || !showIDPattern.MatchString(f.ShowID) || len(f.Episodes) > MaxEpisodes {
		return nil, ErrInvalidFile
	}
	used := map[string]bool{}
	for _, e := range f.Episodes {
		parts := strings.Split(e.Path, "/")
		if len(parts) != 2 || parts[0] != show || !ValidShow(parts[1]) || path.Clean(e.Path) != e.Path || !IsAudioExtension(strings.TrimPrefix(strings.ToLower(path.Ext(e.Path)), ".")) || used[e.Path] {
			return nil, ErrInvalidFile
		}
		used[e.Path] = true
	}
	return &f, nil
}

// Write changes updated_at only when another field changed. Removed episode
// descriptions are expendable when the serialized file reaches its cap.
func Write(root *os.Root, show string, f *Feed, now time.Time) (bool, error) {
	if !ValidShow(show) {
		return false, ErrInvalidFile
	}
	if len(f.Episodes) > MaxEpisodes {
		return false, ErrTooLarge
	}
	f.ScrubPrivate()
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
	f.UpdatedAt = now.UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return false, err
	}
	if len(b)+1 > MaxFileBytes {
		for i := range f.Episodes {
			if f.Episodes[i].RemovedAt != "" {
				f.Episodes[i].Description = ""
			}
		}
		b, err = json.MarshalIndent(f, "", "  ")
		if err != nil {
			return false, err
		}
	}
	if len(b)+1 > MaxFileBytes {
		return false, ErrTooLarge
	}
	if err := WriteAtomic(root, show, "feed.json", append(b, '\n')); err != nil {
		return false, err
	}
	return true, nil
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
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	tmp := show + "/.audiosilo-tmp-" + hex.EncodeToString(random[:])
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = root.Remove(tmp) }()
	if err := file.Chmod(0o644); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, show+"/"+name)
}
