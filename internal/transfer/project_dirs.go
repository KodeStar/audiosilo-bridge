package transfer

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
)

// LibrivoxStagePrefix identifies resumable LibriVox project directories.
const LibrivoxStagePrefix = ".audiosilo-librivox-"

// LibrivoxStageName names a project's staging directory beside its destination.
func LibrivoxStageName(id int) string { return LibrivoxStagePrefix + strconv.Itoa(id) }

// ErrStagingDir means the staging path could not be confirmed as a directory.
var ErrStagingDir = errors.New("cannot use the staging directory")

// PrepareProjectDir creates the project's parent and staging directory under
// the open library root. It returns the affected relative path on failure.
func PrepareProjectDir(root *os.Root, stage string) (string, error) {
	parent := path.Dir(stage)
	if err := makeParent(root, parent); err != nil {
		return parent, err
	}
	if err := root.Mkdir(stage, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return stage, err
	}
	if info, err := root.Lstat(stage); err != nil || !info.IsDir() {
		return stage, ErrStagingDir
	}
	return stage, nil
}

// strayLookWait bounds one directory read on a library mount.
const strayLookWait = 10 * time.Second

// StagedProject is a LibriVox staging folder found in a library.
type StagedProject struct {
	Path string
	ID   int
}

// StagedProjects lists matching directories in the mount and directly below
// its visible author directories. It skips symlinks. A per-read timeout bounds
// a stalled mount. The total budget and shuffled author order let successive
// walks cover different parts of a slow library.
func StagedProjects(mount string, budget time.Duration) []StagedProject {
	until := time.Now().Add(budget)
	top, err := fsutil.ReadDirWithin(mount, strayLookWait)
	if err != nil {
		return nil
	}
	var out []StagedProject
	var authors []string
	for _, e := range top {
		if !e.IsDir() {
			continue
		}
		if id, ok := stagingID(e.Name()); ok {
			out = append(out, StagedProject{Path: e.Name(), ID: id})
		} else if !strings.HasPrefix(e.Name(), ".") {
			authors = append(authors, e.Name())
		}
	}
	rand.Shuffle(len(authors), func(i, j int) { authors[i], authors[j] = authors[j], authors[i] }) //nolint:gosec // G404: the walk's order, no secret
	for _, name := range authors {
		left := time.Until(until)
		if left <= 0 {
			return out
		}
		inner, err := fsutil.ReadDirWithin(filepath.Join(mount, name), min(strayLookWait, left))
		if errors.Is(err, fsutil.ErrStatTimeout) {
			return out
		}
		for _, f := range inner {
			if id, ok := stagingID(f.Name()); ok && f.IsDir() {
				out = append(out, StagedProject{Path: path.Join(name, f.Name()), ID: id})
			}
		}
	}
	return out
}

// stagingID is the LibriVox project id a staging folder's name carries.
func stagingID(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, LibrivoxStagePrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(rest)
	return id, err == nil && id > 0 && LibrivoxStageName(id) == name
}
