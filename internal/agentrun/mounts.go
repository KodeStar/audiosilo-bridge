package agentrun

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
)

// The endpoint's service (--endpoint-only) must have nothing of the
// subscriptions in reach, wherever it is mounted: --home missing or closed
// (homeReachable) isn't enough when the agent volume, or a CLI's login, is
// mounted at another path. So it reads its mounts (/proc/self/mountinfo,
// mounts_linux.go) and refuses its runs while one of them
//
//   - is the agent volume by compose's name (agent-config, or
//     <project>_agent-config): a local volume shows in mountinfo as
//     .../volumes/<name>/_data, so this holds even where the service's user
//     can't open it; or
//   - holds a credential's file or the agent home's marker
//     (agentHomeMarker, which the agent container's runner writes into its
//     home) at the mount point or one directory below it: that finds the agent
//     volume under any name or path, a bind mount of a CLI's login, and a
//     parent folder holding either. Where the service's user can't enter a
//     directory, neither can a run's commands (the same uid, no capability),
//     so a closed one counts as holding nothing.
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
// views and a tmpfs (made empty for this container, so only its own processes
// write there: a run's per-run files are on one).
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true, "devpts": true, "mqueue": true,
	"devtmpfs": true, "tmpfs": true, "securityfs": true, "debugfs": true, "tracefs": true,
	"pstore": true, "bpf": true, "hugetlbfs": true, "fusectl": true, "configfs": true,
	"binfmt_misc": true, "nsfs": true, "autofs": true,
}

// maxMountChildren bounds the directories looked into below one mount point
// (a large library folder mounted by mistake isn't walked whole).
const maxMountChildren = 1024

// credentialNames are the files that hold a subscription's credential or the
// agent home's marker, by the name a mount point may carry itself (a compose
// secret, a single file mounted).
var credentialNames = func() map[string]bool {
	out := map[string]bool{agentHomeMarker: true, "auth.json": true, ".credentials.json": true, ".claude.json": true}
	for _, k := range credentialVars {
		out[strings.ToLower(k)] = true
	}
	return out
}()

// credentialPaths are where, under a directory, such a file sits: the agent
// home's layout (the marker, secrets/<lowercase variable>, codex/'s login and
// its config.toml, which a later subscription run loads), a secrets dir or a
// CODEX_HOME or CLAUDE_CONFIG_DIR mounted alone, and a user's home with the
// CLIs' own dirs.
var credentialPaths = func() []string {
	out := []string{agentHomeMarker, "auth.json", ".credentials.json", ".claude.json", ".claude/.credentials.json",
		".codex/auth.json"}
	for _, f := range slices.Sorted(maps.Keys(codexKeep)) {
		out = append(out, path.Join(codexDir, f))
	}
	for _, k := range credentialVars {
		out = append(out, path.Join(secretsDir, strings.ToLower(k)), strings.ToLower(k))
	}
	return out
}()

// parseMountinfo reads /proc/self/mountinfo's lines: "id parent major:minor
// root point options [optional...] - fstype source superoptions", the paths
// with space, tab, newline and backslash escaped as octal (\040). A line it
// can't read is left out.
func parseMountinfo(b []byte) []mount {
	var out []mount
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
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
// is a local volume compose named agent-config: .../volumes/<name>/_data.
func isAgentVolume(root string) bool {
	dir, last := path.Split(path.Clean(root))
	if last != "_data" {
		return false
	}
	dir = path.Clean(dir)
	name := path.Base(dir)
	return path.Base(path.Dir(dir)) == "volumes" && (name == agentVolume || strings.HasSuffix(name, "_"+agentVolume))
}

// credentialMount names the first of mounts that is the agent volume or holds
// a credential's file (the rules above), "" when none does.
func credentialMount(mounts []mount) string {
	for _, m := range mounts {
		if pseudoFS[m.fstype] || fsutil.Within("/proc", m.point) || fsutil.Within("/sys", m.point) || fsutil.Within("/dev", m.point) {
			continue
		}
		if isAgentVolume(m.root) {
			return m.point + " is the agent volume (" + agentVolume + ")"
		}
		if credentialNames[filepath.Base(m.point)] {
			return m.point + " is mounted by a credential's name"
		}
		for _, dir := range append([]string{m.point}, subdirs(m.point)...) {
			if p := credentialUnder(dir); p != "" {
				return m.point + " holds " + p
			}
		}
	}
	return ""
}

// credentialUnder is the first of credentialPaths present under dir, "" when
// none is.
func credentialUnder(dir string) string {
	for _, rel := range credentialPaths {
		p := filepath.Join(dir, rel)
		if present(p) {
			return p
		}
	}
	return ""
}

// present reports whether p is there. Missing, or behind a directory the
// runner can't enter, is absent (a run, the same uid, can't reach it either);
// any other answer counts as present: never a wrong no.
func present(p string) bool {
	_, err := os.Lstat(p)
	switch {
	case err == nil:
		return true
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ENOTDIR):
		return false
	default:
		return true
	}
}

// subdirs are the directories directly in dir (not links), at most
// maxMountChildren; none when dir can't be listed.
func subdirs(dir string) []string {
	f, err := os.Open(dir) //nolint:gosec // a mount point from mountinfo, only listed
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	entries, _ := f.ReadDir(maxMountChildren)
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}
