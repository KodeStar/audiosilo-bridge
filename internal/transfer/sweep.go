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

// SweepStaged removes old Place temporaries from a checked library. WalkDir
// does not follow symlinks. Place never writes into hidden directories.
func SweepStaged(ctx context.Context, mount string, cutoff time.Time, stillMapped func() bool) error {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var failures []error
	err = fs.WalkDir(root.FS(), ".", func(rel string, e fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			failures = append(failures, walkErr)
			return nil
		}
		if e.IsDir() && rel != "." && strings.HasPrefix(e.Name(), ".") {
			return fs.SkipDir
		}
		if !e.Type().IsRegular() || !stagedName.MatchString(e.Name()) {
			return nil
		}
		// Pin the parent for the final check and unlink.
		parent, err := root.OpenRoot(path.Dir(rel))
		if err == nil {
			var info fs.FileInfo
			info, err = parent.Lstat(e.Name())
			if err == nil && info.Mode().IsRegular() && !info.ModTime().After(cutoff) {
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
