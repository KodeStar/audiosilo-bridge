package transfer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

var stagedName = regexp.MustCompile("^" + regexp.QuoteMeta(tempPrefix) + "[0-9a-f]{16}\\.part$")

// SweepStaged removes old Place temporaries from a checked library, never one
// a Place here is still using (staging). WalkDir does not follow symlinks.
// Known placement folders restrict the walk to their entries and the ancestors
// leading to them. With no records, an account's library may have older temps anywhere.
func SweepStaged(ctx context.Context, mount string, cutoff time.Time, stillMapped func() bool, folders ...string) error {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	// A true entry names a placement folder. A false entry is only on the
	// way to one. WalkDir skips every other directory before reading it.
	dirs := map[string]bool{}
	for _, dir := range folders {
		dirs[dir] = true
		for dir != "." {
			dir = path.Dir(dir)
			if _, seen := dirs[dir]; seen {
				break
			}
			dirs[dir] = false
		}
	}
	var failures []error
	err = fs.WalkDir(root.FS(), ".", func(rel string, e fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			failures = append(failures, walkErr)
			return nil
		}
		if e.IsDir() {
			_, visit := dirs[rel]
			if (rel != "." && strings.HasPrefix(e.Name(), ".")) || (len(folders) > 0 && !visit) {
				return fs.SkipDir
			}
			return nil
		}
		if (len(folders) > 0 && !dirs[path.Dir(rel)]) || !e.Type().IsRegular() || !stagedName.MatchString(e.Name()) {
			return nil
		}
		// Pin the parent for the final check and unlink.
		parent, err := root.OpenRoot(path.Dir(rel))
		if err == nil {
			var info fs.FileInfo
			info, err = parent.Lstat(e.Name())
			if err == nil && info.Mode().IsRegular() && !info.ModTime().After(cutoff) && !inUse(e.Name()) {
				if !stillMapped() {
					_ = parent.Close()
					return context.Canceled
				}
				err = parent.Remove(e.Name())
			}
			_ = parent.Close()
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			failures = append(failures, err)
		}
		return nil
	})
	return errors.Join(append(failures, err)...)
}
