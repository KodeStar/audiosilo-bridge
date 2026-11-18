package agentrun

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
)

// The endpoint's service (--endpoint-only) must have nothing of the
// subscriptions in reach, wherever it is mounted: --home missing or closed
// (homeReachable) isn't enough when the agent volume, or a CLI's login, is
// mounted at another path. So it reads its mounts (/proc/self/mountinfo,
// mounts_linux.go) and refuses its runs while one of them
//
//   - is the agent volume by compose's name (agent-config, or
//     <project>_agent-config), whole or a subpath of it: a local volume shows
//     in mountinfo as .../volumes/<name>/_data, so this holds even where the
//     service's user can't open it; or
//   - is mounted by a credential file's name, at its point or from its
//     source (a login bind-mounted under another name); or
//   - holds a credential's file, the agent home's marker (agentHomeMarker,
//     which the agent container's runner writes into its home) or the
//     Bridge's own vault (its secret.key, a sealed secret) at the mount point
//     or one directory below it (a tmpfs at its point only): that finds the
//     agent volume under any name or path, a bind mount of a CLI's login or
//     of the Bridge's data, and a parent folder holding either. Where the
//     service's user can't enter a directory, neither can a run's commands
//     (the same uid, no capability), so a closed one counts as holding
//     nothing.
//
// The check fails closed: a mount point it can enter but not list, one with
// more than maxMountChildren entries, or a look that doesn't answer within
// lookTimeout (a hung NFS or SMB mount) or past walkBudget refuses the runs as
// a credential found would, rather than pass what it couldn't look at.
//
// Every mount is looked at wherever it is, /dev included: only the kernel's
// own filesystems (pseudoFS) are passed over, by type, never by path.
//
// The marker is matched by presence rather than by device and inode: those
// would have to come from the agent container (through the Bridge, a field on
// every request), and the only volume they would catch that these rules don't
// is a renamed one the service can't open, which a run can't read either.

// agentHomeMarker is the file the agent container's runner keeps at the top of
// its home (/agent), naming the volume as the subscriptions' wherever it is
// mounted.
const agentHomeMarker = ".audiosilo-agent-home"

// agentVolume is compose's name for the agent volume (compose.example.yaml);
// compose prefixes its project's name: <project>_agent-config.
const agentVolume = "agent-config"

// mount is one line of /proc/self/mountinfo: the path shown inside its
// filesystem (root), where it is mounted (point) and the filesystem's type.
type mount struct{ root, point, fstype string }

// pseudoFS are filesystems that never hold a file of the host's: the kernel's
// views. A tmpfs isn't one of them (a host's /run or /dev/shm bound in, a
// swarm or Kubernetes secret), but the container's own (/tmp) holds a run's
// per-run files one folder down, so a tmpfs is looked at its point alone.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true, "devpts": true, "mqueue": true,
	"devtmpfs": true, "securityfs": true, "debugfs": true, "tracefs": true, "selinuxfs": true,
	"efivarfs": true, "pstore": true, "bpf": true, "hugetlbfs": true, "fusectl": true, "configfs": true,
	"binfmt_misc": true, "nsfs": true, "autofs": true,
}

// maxMountChildren bounds the entries of one mount point the check looks
// through: one with more (a large library folder mounted by mistake) refuses
// the runs rather than be walked whole or in part.
const maxMountChildren = 1024

// lookTimeout bounds each look of the check (a stat, a folder read), and
// walkBudget the whole walk: a hung network mount refuses the runs rather than
// hold the status tick (and runner.json's heartbeat) behind it.
const (
	lookTimeout = 5 * time.Second
	walkBudget  = 20 * time.Second
)

// The Bridge's own vault (internal/secrets), which the endpoint's service must
// never see either: its master key's variable and file, the variable holding a
// GitHub token (contrib.GitHubTokenEnv), and the sealed secrets' files
// (secrets/<name>.enc, by the names the Bridge always uses). Copied here, as
// agentrun imports neither package (a test pins them to theirs).
const (
	vaultKeyEnv    = "AUDIOSILO_BRIDGE_SECRET_KEY"
	vaultKeyFile   = "secret.key"
	githubTokenEnv = "AUDIOSILO_BRIDGE_GITHUB_TOKEN"
)

// vaultVars are the Bridge's variables an endpoint-only runner must not hold.
var vaultVars = []string{vaultKeyEnv, githubTokenEnv}

// vaultSealed are the sealed secrets' names (secrets.Names and the server
// token, which every paired Bridge has), each a <name>.enc file in the vault's
// folder.
var vaultSealed = []string{"server_token", "claude_oauth_token", "anthropic_api_key", "openai_api_key",
	"endpoint_api_key", "github_user_token"}

// credentialNames are the files that hold a subscription's credential or the
// agent home's marker, by the name a mount carries itself, at its point or
// from its source (a compose secret, a single file mounted).
var credentialNames = func() map[string]bool {
	out := map[string]bool{agentHomeMarker: true, "auth.json": true, ".credentials.json": true, ".claude.json": true,
		vaultKeyFile: true}
	for _, k := range credentialVars {
		out[strings.ToLower(k)] = true
	}
	for _, n := range vaultSealed {
		out[n+".enc"] = true
	}
	return out
}()

// credentialPaths are where, under a directory, such a file sits: the agent
// home's layout (the marker, secrets/<lowercase variable>, codex/'s login and
// its config.toml, which a later subscription run loads), a secrets dir or a
// CODEX_HOME or CLAUDE_CONFIG_DIR mounted alone, a user's home with the CLIs'
// own dirs, and the Bridge's data folder (secret.key, secrets/<name>.enc) or
// its vault's folder mounted alone.
var credentialPaths = func() []string {
	out := []string{agentHomeMarker, "auth.json", ".credentials.json", ".claude.json", ".claude/.credentials.json",
		".codex/auth.json", vaultKeyFile}
	for _, f := range slices.Sorted(maps.Keys(codexKeep)) {
		out = append(out, path.Join(codexDir, f))
	}
	for _, k := range credentialVars {
		out = append(out, path.Join(secretsDir, strings.ToLower(k)), strings.ToLower(k))
	}
	for _, n := range vaultSealed {
		out = append(out, path.Join(secretsDir, n+".enc"), n+".enc")
	}
	return out
}()

// parseMountinfo reads /proc/self/mountinfo's lines: "id parent major:minor
// root point options [optional...] - fstype source superoptions", the paths
// with space, tab, newline and backslash escaped as octal (\040). The fields
// are split on a space alone: a path may hold any other blank as it is (a
// no-break space, a carriage return), which strings.Fields would split on. A
// line it can't read is left out.
func parseMountinfo(b []byte) []mount {
	var out []mount
	for line := range strings.Lines(string(b)) {
		f := strings.Split(strings.TrimSuffix(line, "\n"), " ")
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(f) {
			continue
		}
		out = append(out, mount{root: unescapeMount(f[3]), point: unescapeMount(f[4]), fstype: f[sep+1]})
	}
	return out
}

// unescapeMount undoes mountinfo's octal escapes (\NNN).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isAgentVolume reports whether root (a mount's path inside its filesystem)
// is a local volume compose named agent-config, or a subpath of one:
// .../volumes/<name>/_data[/...].
func isAgentVolume(root string) bool {
	parts := strings.Split(path.Clean(root), "/")
	for i := 2; i < len(parts); i++ {
		name := parts[i-1]
		if parts[i] == "_data" && parts[i-2] == "volumes" && (name == agentVolume || strings.HasSuffix(name, "_"+agentVolume)) {
			return true
		}
	}
	return false
}

// looks are the file system calls the mount check makes, each bounded by
// lookTimeout (a test injects its own).
type looks struct {
	lstat   func(string) (fs.FileInfo, error)
	readDir func(string) ([]fs.DirEntry, error)
}

// boundedLooks are the real calls, each bounded by lookTimeout
// (fsutil.LstatWithin, fsutil.ReadDirWithin: one look in flight per path, so
// a check every tick over a hung mount never piles up blocked ones).
var boundedLooks = looks{
	lstat:   func(p string) (fs.FileInfo, error) { return fsutil.LstatWithin(p, lookTimeout) },
	readDir: func(p string) ([]fs.DirEntry, error) { return fsutil.ReadDirWithin(p, lookTimeout) },
}

// mountWalk is one run of the check over the mounts: its looks, and the
// deadline the whole walk must end by.
type mountWalk struct {
	looks
	deadline time.Time
}

// credentialMount names the first of mounts that is the agent volume or holds
// a credential's file (the rules above), "" when none does. A mount it can't
// look through is named too, with why (the check fails closed).
func (w *mountWalk) credentialMount(mounts []mount) string {
	for _, m := range mounts {
		switch {
		case isAgentVolume(m.root):
			return m.point + " is the agent volume (" + agentVolume + ")"
		case credentialNames[filepath.Base(m.point)] || credentialNames[path.Base(m.root)]:
			return m.point + " is mounted by a credential's name"
		case pseudoFS[m.fstype]:
			continue
		}
		dirs := []string{m.point}
		if m.fstype != "tmpfs" {
			sub, err := w.subdirs(m.point)
			if err != nil {
				return m.point + " can't be checked: " + err.Error()
			}
			dirs = append(dirs, sub...)
		}
		for _, dir := range dirs {
			p, err := w.credentialUnder(dir)
			if err != nil {
				return m.point + " can't be checked: " + err.Error()
			}
			if p != "" {
				return m.point + " holds " + p
			}
		}
	}
	return ""
}

// credentialUnder is the first of credentialPaths present under dir, "" when
// none is.
func (w *mountWalk) credentialUnder(dir string) (string, error) {
	for _, rel := range credentialPaths {
		p := filepath.Join(dir, rel)
		ok, err := w.present(p)
		if err != nil {
			return "", err
		}
		if ok {
			return p, nil
		}
	}
	return "", nil
}

// present reports whether p is there. Missing, or behind a directory the
// runner can't enter, is absent (a run, the same uid, can't reach it either);
// a look that didn't answer in time, or the walk past its budget, is an
// error; any other answer counts as present: never a wrong no.
func (w *mountWalk) present(p string) (bool, error) {
	if err := w.inTime(); err != nil {
		return false, err
	}
	_, err := w.lstat(p)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fsutil.ErrStatTimeout):
		return false, timedOut(p)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ENOTDIR):
		return false, nil
	default:
		return true, nil
	}
}

// subdirs are the directories directly in dir (not links). A mount that is a
// file, or that the runner can't enter, has none (nor can a run look in); one
// it can enter but not list, or with more than maxMountChildren entries, is
// an error: a run could still reach a folder in it by name.
func (w *mountWalk) subdirs(dir string) ([]string, error) {
	if err := w.inTime(); err != nil {
		return nil, err
	}
	info, err := w.lstat(dir)
	switch {
	case errors.Is(err, fsutil.ErrStatTimeout):
		return nil, timedOut(dir)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
		return nil, nil // under a later mount, or a closed parent: out of a run's reach too
	case err != nil:
		return nil, fmt.Errorf("it can't be looked at (%w)", err)
	case !info.IsDir():
		return nil, nil
	}
	entries, err := w.readDir(dir)
	switch {
	case err == nil:
	case errors.Is(err, fsutil.ErrStatTimeout):
		return nil, timedOut(dir)
	case errors.Is(err, fs.ErrPermission) && !enterable(dir):
		return nil, nil
	default:
		return nil, fmt.Errorf("its folders can't be listed (%w)", err)
	}
	if len(entries) > maxMountChildren {
		return nil, fmt.Errorf("it holds more than %d entries, more than the check looks through", maxMountChildren)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// inTime is an error once the walk is past its deadline.
func (w *mountWalk) inTime() error {
	if time.Now().After(w.deadline) {
		return fmt.Errorf("the check didn't finish within %s (a slow or hung mount?)", walkBudget)
	}
	return nil
}

// timedOut is the error for a look at p that didn't answer in time.
func timedOut(p string) error {
	return fmt.Errorf("a look at %s didn't answer within %s (a hung mount?)", p, lookTimeout)
}
