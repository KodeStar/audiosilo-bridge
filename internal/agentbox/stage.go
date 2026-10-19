package agentbox

import (
	"bytes"
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
	// credentialDirMode lets the runner (another uid) delete the handed
	// credential.json; the agent could anyway put a file there, which the
	// runner reads strictly and only ever as its own run's credential.
	credentialDirMode = 0o777
)

// stage creates the job dir and writes it: work/ (a copy of the staged dir),
// prompt.txt, then request.json renamed into place, the runner's ready signal.
// The agent uid can write the staging dir too (runner.json), so it can remove or
// rename the Bridge's entries there and put a link in their place, and it can
// write work/out/. So every write goes through an os.Root opened on the dir just
// made, never through its path, and the dir stays private (0700) until it is
// whole: nothing the agent does meanwhile can lead a write, or a chmod, out of
// it. The caller closes the returned Root.
//
// handed, when set, is the vault's credential for the run: credential.json in
// its own dir, written before the request so the runner finds it at the claim.
func stage(job string, req agent.Request, cli string, timeout int, handed *agentproto.Credential) (*os.Root, error) {
	if err := os.Mkdir(job, 0o700); err != nil {
		return nil, err
	}
	root, err := openMadeDir(job)
	if err != nil {
		return nil, err
	}
	if err := fill(root, req, cli, timeout, handed); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// openMadeDir opens the dir just made at path as an os.Root, refusing it when
// the entry at path is no longer that dir: a link put in its place would take
// every write wherever it points.
func openMadeDir(path string) (*os.Root, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err == nil {
		var entry os.FileInfo
		if entry, err = os.Lstat(path); err == nil && (!entry.IsDir() || !os.SameFile(opened, entry)) {
			err = fmt.Errorf("%s was replaced while it was staged", filepath.Base(path))
		}
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// fill writes the job into its (still private) dir, opens the dir to the agent
// uid, and renames request.json into place.
func fill(root *os.Root, req agent.Request, cli string, timeout int, handed *agentproto.Credential) error {
	if err := copyTree(req.Dir, root, agentproto.WorkDir); err != nil {
		return err
	}
	if handed != nil {
		if err := writeCredential(root, *handed); err != nil {
			return err
		}
	}
	if err := createFile(root, agentproto.PromptFile, strings.NewReader(req.Prompt), inputFile, true); err != nil {
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
	// request.json.tmp is the contract's name for the request in progress: it is
	// written whole, then renamed to request.json, the ready signal, once the dir
	// is open to the agent uid.
	if err := createFile(root, agentproto.RequestTmpFile, bytes.NewReader(body), requestMode, true); err != nil {
		return err
	}
	if err := root.Chmod(".", jobDirMode); err != nil { //nolint:gosec // shared with the agent uid by design (decision 15)
		return err
	}
	return root.Rename(agentproto.RequestTmpFile, agentproto.RequestFile)
}

// writeCredential writes credential.json into its own dir: the dir open to the
// agent uid (not sticky, so the runner can delete the file before the CLI starts),
// the file read-only and synced.
func writeCredential(root *os.Root, c agentproto.Credential) error {
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := mkdir(root, agentproto.CredentialDir, credentialDirMode); err != nil {
		return err
	}
	return createFile(root, filepath.Join(agentproto.CredentialDir, agentproto.CredentialFile), bytes.NewReader(body), inputFile, true)
}

// copyTree copies the staged dir src into dst under root: regular files only
// (the staged dir holds copies, never links), inputs read-only, out/ writable by
// the agent. dst's out/ always exists, so the agent has somewhere to write on a
// first attempt.
func copyTree(src string, root *os.Root, dst string) error {
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
			return mkdir(root, target, mode)
		case d.Type().IsRegular():
			mode := fs.FileMode(inputFile)
			if inOut {
				mode = outFileMode
			}
			return copyFile(path, root, target, mode)
		default:
			return nil // a link or a device has no place in a staged dir
		}
	})
	if err != nil {
		return err
	}
	out := filepath.Join(dst, agentproto.OutDir)
	if _, err := root.Lstat(out); err == nil {
		return nil
	}
	return mkdir(root, out, outDirMode)
}

// mkdir creates dir under root with exactly mode (the Bridge's umask is 022).
func mkdir(root *os.Root, dir string, mode fs.FileMode) error {
	if err := root.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return root.Chmod(dir, mode)
}

// copyFile streams src into a new file dst under root with exactly mode.
func copyFile(src string, root *os.Root, dst string, mode fs.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // src is a file of the pipeline's own staged dir
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := createFile(root, dst, in, mode, false); err != nil {
		return fmt.Errorf("copy %s: %w", filepath.Base(src), err)
	}
	return nil
}

// createFile writes r into a new file name under root with exactly mode (set on
// the open file, never through a path), synced when sync is set (the prompt and
// the request; a copied input needs no more than the copy).
func createFile(root *os.Root, name string, r io.Reader, mode fs.FileMode, sync bool) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err == nil && sync {
		err = f.Sync()
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
