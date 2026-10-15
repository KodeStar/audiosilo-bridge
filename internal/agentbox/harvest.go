package agentbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// The harvest's limits (contract section 7). The pipeline's own agent.Harvest
// checks each declared output again, with its own caps, when it moves it on.
const (
	maxHarvestDepth   = 8
	maxHarvestFile    = 8 << 20
	maxHarvestTotal   = 64 << 20
	maxHarvestEntries = 2000
)

// harvestLimitError is an output that broke one of the harvest's limits: what the
// agent wrote, not the Bridge's trouble reading it (every other harvest error).
type harvestLimitError struct{ msg string }

func (e *harvestLimitError) Error() string { return e.msg }

func limitErr(format string, args ...any) error {
	return &harvestLimitError{msg: fmt.Sprintf(format, args...)}
}

// harvest copies the job's work/out/ back into the staged dir's out/, replacing
// it, so the pipeline finds the outputs where a local run leaves them. Regular
// files only (a link or a device is skipped), every read inside the job dir's
// os.Root, under depth, count and size limits; any limit broken fails the whole
// harvest (a *harvestLimitError) and leaves the staged out/ as it was.
func harvest(job *os.Root, reqDir string) error {
	src, err := job.OpenRoot(path.Join(agentproto.WorkDir, agentproto.OutDir))
	if err != nil {
		return fmt.Errorf("agent container: open work/out: %w", err)
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.MkdirTemp(reqDir, ".out-harvest-*")
	if err != nil {
		return fmt.Errorf("agent container: harvest: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	var entries int
	var total int64
	err = fs.WalkDir(src.FS(), ".", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == "." {
			return nil
		}
		if entries++; entries > maxHarvestEntries {
			return limitErr("more than %d entries", maxHarvestEntries)
		}
		if strings.Count(p, "/")+1 > maxHarvestDepth {
			return limitErr("%s is more than %d levels deep", p, maxHarvestDepth)
		}
		dst := filepath.Join(tmp, filepath.FromSlash(p))
		switch {
		case d.IsDir():
			return os.Mkdir(dst, 0o750)
		case d.Type().IsRegular():
			n, err := copyOut(src, p, dst)
			if err != nil {
				return err
			}
			if total += n; total > maxHarvestTotal {
				return limitErr("the outputs are over %d bytes in all", maxHarvestTotal)
			}
			return nil
		default:
			return nil // a link, a FIFO or a device is never harvested
		}
	})
	if err != nil {
		return fmt.Errorf("agent container: harvest out/: %w", err)
	}
	out := agent.OutPath(reqDir)
	if err := os.RemoveAll(out); err != nil {
		return fmt.Errorf("agent container: harvest: clear out/: %w", err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return fmt.Errorf("agent container: harvest: %w", err)
	}
	return nil
}

// copyOut copies one output file, re-checking it once open: the walk saw a
// regular file, but the container could have swapped it since.
func copyOut(src *os.Root, name, dst string) (int64, error) {
	f, err := agentproto.OpenRegular(src, name)
	if err != nil {
		if errors.Is(err, agentproto.ErrNotRegular) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil {
		return 0, err
	} else if info.Size() > maxHarvestFile {
		return 0, limitErr("%s is %d bytes, over the %d cap", name, info.Size(), maxHarvestFile)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // dst is inside the Bridge's own temp dir
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(f, maxHarvestFile+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if n > maxHarvestFile {
		return n, limitErr("%s grew over the %d cap", name, maxHarvestFile)
	}
	return n, os.Chmod(dst, 0o644) //nolint:gosec // a normal artifact, as agent.Harvest writes them
}
