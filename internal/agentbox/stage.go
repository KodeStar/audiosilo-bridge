package agentbox

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// Modes in a job dir. The Bridge and the agent run as different uids sharing no
// group, so what the agent must write is world-writable and what the Bridge must
// keep is the Bridge's own: the job dir is sticky (the agent adds its files but
// can't delete or replace the Bridge's), inputs are read-only, and only work/out/
// is the agent's to change (a validation retry patches the previous attempt's
// files there, so those are writable too).
const (
	jobDirMode  = 0o777 | os.ModeSticky
	inputDir    = 0o755
	inputFile   = 0o444
	outDirMode  = 0o777
	outFileMode = 0o666
	requestMode = 0o644
)

// stage writes the job dir: work/ (a copy of the staged dir), prompt.txt, then
// request.json renamed into place, the runner's ready signal.
func stage(job string, req agent.Request, cli string, timeout int) error {
	if err := os.Mkdir(job, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(job, jobDirMode); err != nil { //nolint:gosec // shared with the agent uid by design (decision 15)
		return err
	}
	work := filepath.Join(job, agentproto.WorkDir)
	if err := copyTree(req.Dir, work); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(job, agentproto.PromptFile), []byte(req.Prompt), inputFile); err != nil {
		return err
	}
	body, err := json.Marshal(agentproto.Request{
		Protocol:       agentproto.Protocol,
		CLI:            cli,
		Model:          req.Model,
		Effort:         req.Effort,
		Web:            req.Web,
		NoTools:        req.NoTools,
		MaxTurns:       req.MaxTurns,
		TimeoutSeconds: timeout,
	})
	if err != nil {
		return err
	}
	tmp := filepath.Join(job, agentproto.RequestTmpFile)
	if err := writeFile(tmp, body, requestMode); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(job, agentproto.RequestFile))
}

// copyTree copies the staged dir src into dst: regular files only (the staged dir
// holds copies, never links), inputs read-only, out/ writable by the agent. dst's
// out/ always exists, so the agent has somewhere to write on a first attempt.
func copyTree(src, dst string) error {
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		inOut := rel == agent.OutDirName || strings.HasPrefix(rel, agent.OutDirName+string(filepath.Separator))
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			mode := fs.FileMode(inputDir)
			if inOut {
				mode = outDirMode
			}
			return mkdir(target, mode)
		case d.Type().IsRegular():
			mode := fs.FileMode(inputFile)
			if inOut {
				mode = outFileMode
			}
			return copyFile(path, target, mode)
		default:
			return nil // a link or a device has no place in a staged dir
		}
	})
	if err != nil {
		return err
	}
	out := filepath.Join(dst, agentproto.OutDir)
	if _, err := os.Lstat(out); err == nil {
		return nil
	}
	return mkdir(out, outDirMode)
}

// mkdir creates dir with exactly mode (the Bridge's umask is 022).
func mkdir(dir string, mode fs.FileMode) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, mode)
}

// copyFile streams src into a new file dst with exactly mode.
func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // src is a file of the pipeline's own staged dir
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dst is inside the job dir the Bridge just created
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s: %w", filepath.Base(src), err)
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// writeFile writes a new file with exactly mode.
func writeFile(path string, data []byte, mode fs.FileMode) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
