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
//     which the agent container's runner writes into its home), the
//     Bridge's own vault (its secret.key, a sealed secret) or a container
//     runtime's socket (runtimeSockets: every container's files) anywhere
//     within maxMountDepth folders (or links) below its point: that finds the
//     agent volume under any name or path, a bind mount of a CLI's login or of
//     the Bridge's data, and a parent folder holding either however far up
//     (Unraid's /mnt/user mounted whole holds the vault at
//     appdata/audiosilo-bridge/secret.key). Where the service's user can't
//     enter a directory, neither can a run's commands (the same uid, no
//     capability), so a closed one counts as holding nothing.
//
// Two kinds of mount are looked at shallower, at their point and one folder
// (or link) below: a tmpfs (pseudoFS says why) and the container's own root,
// as before the deep walk: it holds the image's files and its writable layer
// (deeper than maxMountDepth in places), and the host's files reach the
// container only through a mount, each walked on its own.
//
// Two things a run makes itself are not a host's credential:
//
//   - the per-run folders the service makes in its TmpDir (run-<id>, where
//     codex keeps codex/config.toml, and version-*), passed over by their
//     exact paths while the service holds them (own), never by a name a
//     host's tmpfs could hold too; and
//   - a file of a credential's name that a run planted (prompt injection
//     from a book's text could ask for one) to lock the service out, in its
//     TmpDir alone and only while that is a tmpfs of the service's own
//     (runAreaMount: the mount holding TmpDir, verified from mountinfo, is a
//     whole tmpfs, alone at its point, hidden by no later mount): one owned
//     by the service's own uid, reached from TmpDir through folders alone (no
//     link, no other mount's point). It is not counted, and sweepRunFiles
//     removes it when a run ends and when the service starts. Ownership alone
//     is no proof: a host folder or a share mounted at TmpDir or the staging
//     dir shows its files as the service's uid where the share squashes
//     owners (CIFS uid=, NFS all_squash), and sweeping one would delete a
//     host's file (the Bridge's secret.key). So a TmpDir that is not such a
//     tmpfs passes nothing, and the staging dir (any filesystem) passes
//     nothing either: a run's file there refuses the runs, names its path
//     for the admin to remove, and is never deleted. A file of any other
//     uid, or anywhere else, still refuses the runs.
//
// The check fails closed: a folder it can enter but not list, a mount deeper
// than maxMountDepth, more than maxWalkEntries entries over the whole walk (a
// library mounted by mistake), a look that doesn't answer within lookTimeout
// (a hung NFS or SMB mount) or the walk past walkBudget each refuse the runs
// as a credential found would, saying to mount narrower folders, rather than
// pass what it couldn't look at.
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
// views. A tmpfs isn't one of them (a host's /run, /run/user/<uid> or /dev/shm
// bound in, a swarm or Kubernetes secret), so it is looked at its point and
// one folder below, the container's own /tmp included, where the service's
// own per-run folders are passed over by their exact paths (own).
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true, "devpts": true, "mqueue": true,
	"devtmpfs": true, "securityfs": true, "debugfs": true, "tracefs": true, "selinuxfs": true,
	"efivarfs": true, "pstore": true, "bpf": true, "hugetlbfs": true, "fusectl": true, "configfs": true,
	"binfmt_misc": true, "nsfs": true, "autofs": true,
}

// maxMountChildren bounds the entries of the container's root or a tmpfs the
// check looks through (its shallow walk lists the point alone): more refuses
// the runs.
const maxMountChildren = 1024

// maxMountDepth is how many folders below a mount's point the check lists,
// so it sees the files one further down: Unraid's /mnt/user mounted whole
// holds the vault three down (appdata/audiosilo-bridge/secret.key) and a
// host's /var the agent volume's secrets seven down
// (lib/docker/volumes/<name>/_data/secrets/<file>). A folder deeper than that
// refuses the runs.
const maxMountDepth = 8

// maxWalkEntries bounds the entries the deep walk lists over all the mounts
// at one check (each status tick and each endpoint run's start). The compose
// example's agent-endpoint mounts its staging volume alone, a few run folders;
// a mount with more (a library of a few thousand books, a host's whole share)
// refuses the runs rather than be walked every tick or passed half looked at.
const maxWalkEntries = 100_000

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

// runtimeSockets are a container runtime's API sockets: one mounted puts
// every container's files (the agent volume, the Bridge's data) within reach
// of whoever can open it, so it counts as a credential's file, by name.
var runtimeSockets = []string{"docker.sock", "podman.sock", "containerd.sock"}

// runtimeSocketPaths are where a host's run folder keeps those sockets a
// folder or two down: /run is a tmpfs, looked at shallow (pseudoFS), so
// mounted whole it shows them only by these paths.
var runtimeSocketPaths = []string{"podman/podman.sock", "containerd/containerd.sock",
	"docker/containerd/containerd.sock"}

// credentialNames are the files that hold a subscription's credential or the
// agent home's marker, by the name a mount carries itself, at its point or
// from its source (a compose secret, a single file mounted).
var credentialNames = func() map[string]bool {
	out := map[string]bool{agentHomeMarker: true, "auth.json": true, ".credentials.json": true, ".claude.json": true,
		vaultKeyFile: true}
	for _, n := range runtimeSockets {
		out[n] = true
	}
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
// own dirs, the Bridge's data folder (secret.key, secrets/<name>.enc) or its
// vault's folder mounted alone, and a host's run folder with a container
// runtime's socket (at its top, or where podman, containerd and docker's own
// containerd keep theirs: runtimeSocketPaths).
var credentialPaths = func() []string {
	out := []string{agentHomeMarker, "auth.json", ".credentials.json", ".claude.json", ".claude/.credentials.json",
		".codex/auth.json", vaultKeyFile}
	out = append(out, runtimeSockets...)
	out = append(out, runtimeSocketPaths...)
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

// credentialRel is credentialPaths as a set: the deep walk matches a folder's
// entries by their name, or their folder's name and theirs (codex/config.toml).
var credentialRel = func() map[string]bool {
	out := map[string]bool{}
	for _, rel := range credentialPaths {
		out[rel] = true
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
	stat    func(string) (fs.FileInfo, error)
	readDir func(string) ([]fs.DirEntry, error)
}

// boundedLooks are the real calls, each bounded by lookTimeout
// (fsutil.LstatWithin, StatWithin, ReadDirNWithin: one look in flight per
// path, so a check every tick over a hung mount never piles up blocked ones).
// A folder is read up to one entry past maxWalkEntries, never whole, and every
// read asks that same n (ReadDirNWithin answers a read in flight whatever its
// n).
var boundedLooks = looks{
	lstat: func(p string) (fs.FileInfo, error) { return fsutil.LstatWithin(p, lookTimeout) },
	stat:  func(p string) (fs.FileInfo, error) { return fsutil.StatWithin(p, lookTimeout) },
	readDir: func(p string) ([]fs.DirEntry, error) {
		return fsutil.ReadDirNWithin(p, maxWalkEntries+1, lookTimeout)
	},
}

// mountWalk is one run of the check over the mounts: its looks, the deadline
// the whole walk must end by, and the entries the deep walk may list.
type mountWalk struct {
	looks
	deadline time.Time
	// listed and maxEntries are the entries the deep walk has listed so far
	// and may list in all.
	listed, maxEntries int
	// rootPoint is the container's own root's mount point, walked shallow ("/";
	// a test sets its own).
	rootPoint string
	// points are the mounts' points: the deep walk of one mount leaves another
	// mounted in it to that one's own walk.
	points map[string]bool
	// own reports whether a folder is one of the service's per-run folders in
	// its TmpDir, by its exact path (nil: none).
	own func(string) bool
	// selfUID is the service's own uid (-1: none), runArea its TmpDir (""
	// none) and areaHere whether the mount being looked at is runArea's
	// verified tmpfs (runAreaMount): a file of a credential's name of
	// selfUID's there is a run's own (runMade), which runFiles collects.
	selfUID  int
	runArea  string
	areaHere bool
	runFiles []runFile
}

// runFile is a file of a credential's name a run planted: its area (the
// walk's runArea) and its path below it.
type runFile struct{ area, rel string }

// newMountWalk is a check over the mounts starting now, with the walk's
// bounds (walkBudget, maxWalkEntries), passing nothing over as a run's own.
func newMountWalk(l looks) *mountWalk {
	return &mountWalk{looks: l, deadline: time.Now().Add(walkBudget), maxEntries: maxWalkEntries, rootPoint: "/",
		selfUID: -1}
}

// narrower ends the error of a bound the walk ran into: what the admin can do.
const narrower = "; mount narrower folders into this service"

// credentialMount names the first of mounts that is the agent volume or holds
// a credential's file (the rules above), "" when none does. A mount it can't
// look through is named too, with why (the check fails closed).
func (w *mountWalk) credentialMount(mounts []mount) string {
	w.points = make(map[string]bool, len(mounts))
	for _, m := range mounts {
		w.points[m.point] = true
	}
	areaMount := w.runAreaMount(mounts)
	for i, m := range mounts {
		switch {
		case isAgentVolume(m.root):
			return m.point + " is the agent volume (" + agentVolume + ")"
		case credentialNames[filepath.Base(m.point)] || credentialNames[path.Base(m.root)]:
			return m.point + " is mounted by a credential's name"
		case pseudoFS[m.fstype]:
			continue
		}
		w.areaHere = i == areaMount
		p, err := w.credentialIn(m)
		if err != nil {
			return m.point + " can't be checked: " + err.Error()
		}
		if p != "" {
			return m.point + " holds " + p
		}
	}
	return ""
}

// runAreaMount is the index in mounts of the mount holding runArea when it is
// a tmpfs of the service's own, -1 when it isn't (no file is then a run's
// own): the mount with the longest point at or above runArea is a tmpfs
// mounted whole (root "/", never a folder of a host's tmpfs bound in), the
// only mount at its point (nothing stacked on or under it) and no mount listed
// after it is at or above its point (one over a parent hides it). A host
// folder or a share mounted at TmpDir (a bind, CIFS, NFS) never passes, nor
// does a TmpDir whose path mountinfo doesn't show.
func (w *mountWalk) runAreaMount(mounts []mount) int {
	if w.runArea == "" || w.selfUID < 0 {
		return -1
	}
	held, point := -1, ""
	for i, m := range mounts {
		if fsutil.Within(m.point, w.runArea) && (held < 0 || len(m.point) > len(point)) {
			held, point = i, m.point
		}
	}
	if held < 0 || mounts[held].fstype != "tmpfs" || mounts[held].root != "/" {
		return -1
	}
	for i, m := range mounts {
		if i != held && (m.point == point || (i > held && fsutil.Within(m.point, point))) {
			return -1
		}
	}
	return held
}

// below is p's path relative to dir when p is dir or inside it (fsutil.Within's
// rule).
func below(dir, p string) (string, bool) {
	if !fsutil.Within(dir, p) {
		return "", false
	}
	rel, err := filepath.Rel(dir, p)
	return rel, err == nil
}

// runMade reports whether p, a file of a credential's name the walk found, is
// a run's own (the rules above): the mount being looked at is runArea's
// verified tmpfs (areaHere), runArea and every folder from it down to p a
// folder of that mount (never a link: one could lead to a host's file whose
// owner is the service's uid; nor another mount's point, p included: a host
// folder or file mounted inside TmpDir, which a look by path such as
// credentialUnder's reaches into), and p owned by the service's uid. It is
// added to runFiles once. Any look that fails is a no.
func (w *mountWalk) runMade(p string) bool {
	if w.selfUID < 0 || !w.areaHere {
		return false
	}
	rel, ok := below(w.runArea, p)
	if !ok || rel == "." || !w.foldersDown(w.runArea, rel) {
		return false
	}
	info, err := w.lstat(p)
	if err != nil {
		return false
	}
	if uid, ok := ownerUID(info); !ok || uid != w.selfUID {
		return false
	}
	if f := (runFile{area: w.runArea, rel: rel}); !slices.Contains(w.runFiles, f) {
		w.runFiles = append(w.runFiles, f)
	}
	return true
}

// foldersDown reports whether area and each folder between it and rel's last
// element is a folder, not a link, and neither those folders below area nor
// rel's last element is another mount's point.
func (w *mountWalk) foldersDown(area, rel string) bool {
	parts := strings.Split(rel, string(filepath.Separator))
	cur := area
	for i := 0; ; i++ {
		info, err := w.lstat(cur)
		if err != nil || !info.IsDir() {
			return false
		}
		cur = filepath.Join(cur, parts[i])
		if w.points[cur] {
			return false
		}
		if i == len(parts)-1 {
			return true
		}
	}
}

// enters reports whether the walk looks into p, a folder or a link it found:
// not when another mount is mounted there (left to that one's look) nor when
// it is one of the service's own per-run folders (own).
func (w *mountWalk) enters(p string) bool {
	return !w.points[p] && (w.own == nil || !w.own(p))
}

// credentialIn is the first credential's file in m, "" when none is: the deep
// walk's, or for a tmpfs and the container's root (at its point and one
// directory below) the shallow look's.
func (w *mountWalk) credentialIn(m mount) (string, error) {
	if m.fstype != "tmpfs" && m.point != w.rootPoint {
		return w.deepCredentialIn(m.point)
	}
	dirs, err := w.dirsOf(m.point)
	if err != nil {
		return "", err
	}
	for _, dir := range dirs {
		if p, err := w.credentialUnder(dir); err != nil || p != "" {
			return p, err
		}
	}
	return "", nil
}

// walkDir is a folder the deep walk is to list: its path as walked (through
// links), how many folders below the mount's point it is, and whether its own
// entry is a link (the only way to a folder already walked).
type walkDir struct {
	path  string
	depth int
	link  bool
}

// deepCredentialIn walks the mount at point breadth first, its folders and the
// links in them to maxMountDepth (a run follows a link as readily as it enters
// a folder; one to a file, nowhere or itself holds nothing, and one to a folder
// the walk has been in, by device and inode, is not walked again: no loop),
// and returns the first entry that is a credential's file (credentialRel),
// "" when none is. Only a link is held against the folders walked: a folder's
// own entry is always walked, as two folders can share a device and inode
// where a file system merges others (overlayfs over two file systems, Unraid's
// user shares, mergerfs), and passing the second over would pass over what it
// holds. A folder it doesn't enter (enters) and a run's planted files
// (runMade) are passed over. Breadth first, a credential near the top is found
// before a large folder further down spends the budget.
func (w *mountWalk) deepCredentialIn(point string) (string, error) {
	queue := []walkDir{{path: point}}
	seen := map[fileKey]bool{}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		entries, err := w.listDir(d, seen)
		if err != nil {
			return "", err
		}
		parent := filepath.Base(d.path)
		for _, e := range entries {
			if credentialRel[e.Name()] || credentialRel[parent+"/"+e.Name()] {
				if p := filepath.Join(d.path, e.Name()); !w.runMade(p) {
					return p, nil
				}
			}
		}
		for _, e := range entries {
			p := filepath.Join(d.path, e.Name())
			link := e.Type()&fs.ModeSymlink != 0
			if (e.IsDir() || link) && w.enters(p) {
				queue = append(queue, walkDir{path: p, depth: d.depth + 1, link: link})
			}
		}
	}
	return "", nil
}

// listDir is the entries of d for the deep walk, none when it holds nothing a
// run could reach: missing (at either look: the staging volume's job folders
// come and go under the walk), not a folder (a file, a link to one or to
// nowhere), a link loop, closed to the runner, or a link to a folder already
// walked. A folder deeper than maxMountDepth, entries past the walk's budget,
// one the runner can enter but not list, a look that didn't answer and the
// walk past its deadline are errors (fail closed).
func (w *mountWalk) listDir(d walkDir, seen map[fileKey]bool) ([]fs.DirEntry, error) {
	if err := w.inTime(); err != nil {
		return nil, err
	}
	info, err := w.stat(d.path)
	switch {
	case errors.Is(err, fsutil.ErrStatTimeout):
		return nil, timedOut(d.path)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ENOTDIR),
		errors.Is(err, syscall.ELOOP):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%s can't be looked at (%w)", d.path, err)
	case !info.IsDir():
		return nil, nil
	}
	if k, ok := fileKeyOf(info); ok {
		if d.link && seen[k] {
			return nil, nil
		}
		seen[k] = true
	}
	if d.depth > maxMountDepth {
		return nil, fmt.Errorf("%s is more than %d folders down, deeper than the check looks%s", d.path, maxMountDepth,
			narrower)
	}
	entries, err := w.readDir(d.path)
	switch {
	case err == nil:
	case errors.Is(err, fsutil.ErrStatTimeout):
		return nil, timedOut(d.path)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		// Gone, or a file by now, since the look above: a job's folder the
		// Bridge removed, a run's own temp folder. Not there for a run either.
		return nil, nil
	case errors.Is(err, fs.ErrPermission) && !enterable(d.path):
		return nil, nil
	default:
		return nil, fmt.Errorf("%s can't be listed (%w)", d.path, err)
	}
	w.listed += len(entries)
	if w.listed > w.maxEntries {
		return nil, fmt.Errorf("by %s the mounts hold more than %d entries, more than the check looks through%s",
			d.path, w.maxEntries, narrower)
	}
	return entries, nil
}

// credentialUnder is the first of credentialPaths present under dir and not
// a run's own (runMade), "" when none is.
func (w *mountWalk) credentialUnder(dir string) (string, error) {
	for _, rel := range credentialPaths {
		p := filepath.Join(dir, rel)
		ok, err := w.present(p)
		if err != nil {
			return "", err
		}
		if ok && !w.runMade(p) {
			return p, nil
		}
	}
	return "", nil
}

// present reports whether p is there. Missing, behind a directory the runner
// can't enter, or behind a link that never resolves (a loop) is absent (a run,
// the same uid, can't reach it either); a look that didn't answer in time, or
// the walk past its budget, is an error; any other answer counts as present:
// never a wrong no.
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
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.ENOTDIR),
		errors.Is(err, syscall.ELOOP):
		return false, nil
	default:
		return true, nil
	}
}

// dirsOf are the directories to look in for a mount at dir: dir itself and
// the directories directly in it and the links there (a run follows a link as
// readily as it enters a folder: the looks under one go through it, and one to
// a file, nowhere or itself holds nothing), but for another mount's point
// (left to its own look) and the service's own per-run folders (own). A mount
// that is a file, or that the runner can't enter, has none (nor can a run look
// in); one it can enter but not list, or with more than maxMountChildren
// entries, is an error: a run could still reach a folder in it by name.
func (w *mountWalk) dirsOf(dir string) ([]string, error) {
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
		return nil, fmt.Errorf("it holds more than %d entries, more than the check looks through%s", maxMountChildren,
			narrower)
	}
	out := []string{dir}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if (e.IsDir() || e.Type()&fs.ModeSymlink != 0) && w.enters(p) {
			out = append(out, p)
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
