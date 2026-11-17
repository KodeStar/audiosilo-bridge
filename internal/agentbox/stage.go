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

// Modes in a job dir. The Bridge and the agent run as different uids, and no
// mode here counts on a group they share (the one exception is the handed
// credential, below), so what the agent must write is world-writable and what
// the Bridge must keep is the Bridge's own: the job dir is sticky (the agent
// adds its files but can't delete or replace the Bridge's), inputs are
// read-only, and only work/out/ is the agent's to change (a validation retry
// patches the previous attempt's files there, so those are writable too).
const (
	jobDirMode  = 0o777 | os.ModeSticky
	inputDir    = 0o755
	inputFile   = 0o444
	outDirMode  = 0o777
	outFileMode = 0o666
	requestMode = 0o644
	// The handed credential.json and its dir. The runner (another uid) must
	// read the file and delete it. When it is in the Bridge's group
	// (runner.json's groups) both are that group's alone: the dir 0770 and
	// the file 0440, so no other uid can read it. The group needs read on the
	// dir as well as write and search: the runner takes the file through an
	// os.Root, which opens each dir on the way (an open a dir without read
	// refuses). Else (an older runner, or a container started without the
	// Bridge's group) the dir is 0777 and the file 0444, as before: readable
	// by every uid until the runner takes it. The agent can anyway put a file
	// there, which the runner reads strictly and only ever as its own run's
	// credential. A staging volume that refuses the chown or the ACL change
	// (NFS, CIFS) gets the 0777 and 0444 hand-off too, rather than no run, and
	// so does a runner its group's credential is denied to (Run).
	credentialDirMode       = 0o777
	credentialFileMode      = inputFile
	credentialGroupDirMode  = 0o770
	credentialGroupFileMode = 0o440
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
// its own dir, written before the request so the runner finds it at the claim;
// group, when not noGroup, is the Bridge's gid: the credential is handed to
// that group alone (the runner is in it). groupErr is why it went to every uid
// instead (the staging volume refused), nil when it didn't.
func stage(job string, req agent.Request, cli string, timeout int, handed *agentproto.Credential, group int) (root *os.Root, groupErr, err error) {
	if err := os.Mkdir(job, 0o700); err != nil {
		return nil, nil, err
	}
	root, err = openMadeDir(job)
	if err != nil {
		return nil, nil, err
	}
	if groupErr, err = fill(root, req, cli, timeout, handed, group); err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	return root, groupErr, nil
}

// noGroup is stage's group for a credential handed to every uid.
const noGroup = -1

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
// uid, and renames request.json into place. groupErr is writeCredential's.
func fill(root *os.Root, req agent.Request, cli string, timeout int, handed *agentproto.Credential, group int) (groupErr, err error) {
	if err := copyTree(req.Dir, root, agentproto.WorkDir); err != nil {
		return nil, err
	}
	if handed != nil {
		if groupErr, err = writeCredential(root, *handed, group); err != nil {
			return nil, err
		}
	}
	if err := createFile(root, agentproto.PromptFile, strings.NewReader(req.Prompt), inputFile, true); err != nil {
		return nil, err
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
		return nil, err
	}
	// request.json.tmp is the contract's name for the request in progress: it is
	// written whole, then renamed to request.json, the ready signal, once the dir
	// is open to the agent uid.
	if err := createFile(root, agentproto.RequestTmpFile, bytes.NewReader(body), requestMode, true); err != nil {
		return nil, err
	}
	if err := root.Chmod(".", jobDirMode); err != nil { //nolint:gosec // shared with the agent uid by design (decision 15)
		return nil, err
	}
	return groupErr, root.Rename(agentproto.RequestTmpFile, agentproto.RequestFile)
}

// writeCredential writes credential.json into its own dir: the dir open to the
// runner (not sticky, so it can delete the file before the CLI starts), the
// file read-only and synced. With a group (the Bridge's) both belong to it
// (set explicitly: a setgid staging dir would hand them its own), any access
// ACL they inherited from a staging dir with a default ACL is removed (its
// named users and groups would read the file under the mode's mask), and no
// other uid gets any access; else every uid may read the file. A staging
// volume that refuses the chown or the ACL change (NFS, CIFS) gets the file
// as every uid's too, with groupErr saying why: the hand-off as it was before
// there was a group, rather than a run that fails. The job dir is still
// private (0700) throughout, so no step leaves a window.
func writeCredential(root *os.Root, c agentproto.Credential, group int) (groupErr, err error) {
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	dirMode, fileMode := fs.FileMode(credentialDirMode), fs.FileMode(credentialFileMode)
	if group != noGroup {
		dirMode, fileMode = credentialGroupDirMode, credentialGroupFileMode
	}
	name := filepath.Join(agentproto.CredentialDir, agentproto.CredentialFile)
	if err := root.Mkdir(agentproto.CredentialDir, 0o700); err != nil {
		return nil, err
	}
	if err := createFile(root, name, bytes.NewReader(body), fileMode, true); err != nil {
		return nil, err
	}
	if group != noGroup {
		if groupErr = giveGroup(root, group, name, agentproto.CredentialDir); groupErr != nil {
			dirMode = credentialDirMode
			if err := root.Chmod(name, credentialFileMode); err != nil {
				return nil, err
			}
		}
	}
	return groupErr, root.Chmod(agentproto.CredentialDir, dirMode)
}

// giveGroup makes each of names (under root) group's, with no ACL beside its
// mode.
func giveGroup(root *os.Root, group int, names ...string) error {
	for _, p := range names {
		if err := root.Chown(p, -1, group); err != nil {
			return err
		}
		if err := dropACLAt(root, p); err != nil {
			return err
		}
	}
	return nil
}

// dropACLAt removes the POSIX ACLs name (under root) inherited (dropACL).
func dropACLAt(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	err = dropACL(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
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
