package agentrun

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

// mountinfo's lines are read with their optional fields and octal escapes,
// split on a space alone (a no-break space or a carriage return in a path is
// kept as it is); a line cut short is left out.
func TestParseMountinfo(t *testing.T) {
	t.Parallel()
	in := `22 1 0:21 / / rw,relatime - overlay overlay rw,lowerdir=/x
36 22 8:1 /var/lib/docker/volumes/audiosilo_agent-config/_data /mnt/a\040b rw,relatime shared:1 master:2 - ext4 /dev/sda1 rw
37 22 0:30 / /tmp rw,nosuid - tmpfs tmpfs rw
38 22 8:1 /cut short
` + "39 22 8:1 /srv/my share/agent /mnt/x y\rz rw - ext4 /dev/sda1 rw\n"
	got := parseMountinfo([]byte(in))
	want := []mount{
		{root: "/", point: "/", fstype: "overlay"},
		{root: "/var/lib/docker/volumes/audiosilo_agent-config/_data", point: "/mnt/a b", fstype: "ext4"},
		{root: "/", point: "/tmp", fstype: "tmpfs"},
		{root: "/srv/my share/agent", point: "/mnt/x y\rz", fstype: "ext4"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("mounts = %+v, want %+v", got, want)
	}
	if got := unescapeMount(`a\134b\011c\0`); got != "a\\b\tc\\0" {
		t.Fatalf("unescape = %q", got)
	}
}

// The agent volume is known by compose's name, prefixed by its project or
// not, under docker's or podman's volumes, whole or a subpath of it (compose's
// volume.subpath); an anonymous volume (the image's own /agent in the
// endpoint's service) and a look-alike are not it.
func TestIsAgentVolume(t *testing.T) {
	t.Parallel()
	for root, want := range map[string]bool{
		"/var/lib/docker/volumes/audiosilo_agent-config/_data":           true,
		"/volumes/agent-config/_data":                                    true,
		"/var/lib/containers/storage/volumes/stack_agent-config/_data/":  true,
		"/var/lib/docker/volumes/audiosilo_agent-config/_data/codex":     true,
		"/var/lib/docker/volumes/" + strings.Repeat("ab", 32) + "/_data": false,
		"/var/lib/docker/volumes/audiosilo_agent-configs/_data":          false,
		"/var/lib/docker/volumes/audiosilo_agent-config/data":            false,
		"/mnt/user/appdata/agent-config/_data":                           false,
		"/":                                                              false,
	} {
		if got := isAgentVolume(root); got != want {
			t.Errorf("isAgentVolume(%q) = %v, want %v", root, got, want)
		}
	}
}

// mkfile makes p (and its dirs) with some content.
func mkfile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// at is a mount of dir as a plain bind (ext4), its root dir itself.
func at(dir string) mount { return mount{root: dir, point: dir, fstype: "ext4"} }

// The endpoint's service refuses its runs while any of its mounts is the
// agent volume or holds a credential's file, wherever it is mounted (denied:
// each case runs nothing, the hand-off deleted, runner.json says so), and the
// log says which. --home being missing alone isn't enough.
func TestEndpointRunRefusedBesideCredentialMounts(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mounts func(t *testing.T, dir string) ([]mount, error)
		why    string
	}{
		"the agent volume by compose's name, closed": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				if err := os.Chmod(dir, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
				return []mount{{root: "/var/lib/docker/volumes/audiosilo_agent-config/_data", point: dir, fstype: "ext4"}}, nil
			},
			why: "is the agent volume",
		},
		"the agent home's marker at another path": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, agentHomeMarker))
				return []mount{at(dir)}, nil
			},
			why: agentHomeMarker,
		},
		"a subscription secret in a secrets dir": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, "secrets", "claude_code_oauth_token"))
				return []mount{at(dir)}, nil
			},
			why: "claude_code_oauth_token",
		},
		"codex's login one folder below a mount": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, "chris", ".codex", "auth.json"))
				return []mount{at(dir)}, nil
			},
			why: "auth.json",
		},
		"the agent home inside a mounted parent": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, "agent", "codex", "config.toml"))
				return []mount{at(dir)}, nil
			},
			why: "config.toml",
		},
		"a key mounted as a file of its name": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				p := filepath.Join(dir, "openai_api_key")
				mkfile(t, p)
				return []mount{at(p)}, nil
			},
			why: "by a credential's name",
		},
		"a login mounted under another name": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				return []mount{{root: "/home/chris/.codex/auth.json", point: filepath.Join(dir, "login"), fstype: "ext4"}}, nil
			},
			why: "by a credential's name",
		},
		"a subpath of the agent volume": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				return []mount{{root: "/var/lib/docker/volumes/audiosilo_agent-config/_data/codex", point: dir, fstype: "ext4"}}, nil
			},
			why: "is the agent volume",
		},
		"the agent volume under /dev": {
			mounts: func(*testing.T, string) ([]mount, error) {
				return []mount{{root: "/var/lib/docker/volumes/audiosilo_agent-config/_data", point: "/dev/agent", fstype: "ext4"}}, nil
			},
			why: "is the agent volume",
		},
		"a secret on a tmpfs (swarm, Kubernetes)": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, "openai_api_key"))
				return []mount{{root: "/", point: dir, fstype: "tmpfs"}}, nil
			},
			why: "openai_api_key",
		},
		"the mounts unreadable": {
			mounts: func(*testing.T, string) ([]mount, error) { return nil, errors.New("no /proc") },
			why:    "can't be read",
		},
		"the Bridge's data folder (its vault's key)": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, "bridge-data", "secret.key"))
				return []mount{at(dir)}, nil
			},
			why: "secret.key",
		},
		"the Bridge's sealed secrets, the key elsewhere": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, "secrets", "server_token.enc"))
				return []mount{at(dir)}, nil
			},
			why: "server_token.enc",
		},
		"the Bridge's vault folder mounted alone": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, "endpoint_api_key.enc"))
				return []mount{at(dir)}, nil
			},
			why: "endpoint_api_key.enc",
		},
		"the vault's key mounted as a file of its name": {
			mounts: func(*testing.T, string) ([]mount, error) {
				return []mount{{root: "/srv/bridge-data/secret.key", point: "/run/k", fstype: "ext4"}}, nil
			},
			why: "by a credential's name",
		},
		"Unraid's /mnt/user mounted whole, the vault three folders down": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, "media", "Audiobooks", "Author", "Book", "01.mp3"))
				mkfile(t, filepath.Join(dir, "appdata", "plex", "Preferences.xml"))
				mkfile(t, filepath.Join(dir, "appdata", "audiosilo-bridge", "secret.key"))
				return []mount{{root: "/mnt/user", point: dir, fstype: "shfs"}}, nil
			},
			why: filepath.Join("appdata", "audiosilo-bridge", "secret.key"),
		},
		"a parent of the agent volume, its home five folders down": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				mkfile(t, filepath.Join(dir, "lib", "docker", "volumes", "stack_agent", "_data", agentHomeMarker))
				return []mount{at(dir)}, nil
			},
			why: agentHomeMarker,
		},
		"a mount deeper than the check looks": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				deep := dir
				for i := range maxMountDepth + 1 {
					deep = filepath.Join(deep, "d"+strconv.Itoa(i))
				}
				if err := os.MkdirAll(deep, 0o700); err != nil {
					t.Fatal(err)
				}
				return []mount{at(dir)}, nil
			},
			why: "deeper than the check looks; mount narrower folders",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			te, _ := newEndpointEnv(t, endpointParentEnv())
			ms, err := tc.mounts(t, t.TempDir())
			te.r.mounts = func() ([]mount, error) { return ms, err }
			if why := te.r.credentialProblem(); !strings.Contains(why, tc.why) {
				t.Fatalf("credentialProblem = %q, want %q", why, tc.why)
			}
			if !te.r.status().OwnCredentials {
				t.Fatal("runner.json doesn't say the service holds a credential")
			}
			te.start(t)
			job, cred := te.submitWithCredential(t, endpointReq, `{"cli":"codex","kind":"endpoint_api_key","value":"sk-endpoint-SECRET"}`)
			ex := waitDone(t, job, 30*time.Second)
			if !ex.Unavailable || !strings.Contains(ex.Error, "mounted anywhere") || strings.Contains(ex.Error, "SECRET") {
				t.Fatalf("exit = %+v, want the run refused as unavailable", ex)
			}
			if _, err := os.Lstat(cred); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("the endpoint key's credential.json is deleted all the same")
			}
			if _, err := os.Stat(filepath.Join(te.codexCap, "argv")); err == nil {
				t.Fatal("the CLI ran beside a mounted credential")
			}
		})
	}
}

// What the endpoint's service has mounted in the compose example runs
// (allowed): the image's own /agent as an anonymous volume, closed to it; its
// staging volume with job dirs; a tmpfs (made empty for the container: a
// run's own files are there) and the kernel's views; a closed folder holding
// the agent home's marker, which a run can't open either.
func TestEndpointRunBesideHarmlessMounts(t *testing.T) {
	t.Parallel()
	te, _ := newEndpointEnv(t, endpointParentEnv())
	anon := t.TempDir()
	if err := os.Chmod(anon, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(anon, 0o700) })
	tmpfs := t.TempDir()
	mkfile(t, filepath.Join(tmpfs, "run-1", "auth.json"))
	closed := t.TempDir()
	mkfile(t, filepath.Join(closed, "agent", agentHomeMarker))
	if err := os.Chmod(filepath.Join(closed, "agent"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(closed, "agent"), 0o700) })
	ms := []mount{
		{root: "/var/lib/docker/volumes/" + strings.Repeat("0f", 32) + "/_data", point: anon, fstype: "ext4"},
		at(te.staging),
		{root: "/", point: tmpfs, fstype: "tmpfs"},
		{root: "/", point: "/proc", fstype: "proc"},
		{root: "/", point: "/dev", fstype: "tmpfs"},
		{root: "/null", point: "/proc/kcore", fstype: "tmpfs"},
		{root: "/var/lib/docker/containers/" + strings.Repeat("0f", 32) + "/hosts", point: "/etc/hosts", fstype: "ext4"},
	}
	if os.Geteuid() != 0 {
		ms = append(ms, at(closed))
	}
	te.r.mounts = func() ([]mount, error) { return ms, nil }
	if why := te.r.credentialProblem(); why != "" {
		t.Fatalf("credentialProblem = %q, want none", why)
	}
	if te.r.status().OwnCredentials {
		t.Fatal("runner.json says the service holds a credential")
	}
	te.start(t)
	if ex := waitDone(t, te.submit(t, endpointReq), 30*time.Second); ex.Error != "" || ex.ExitCode != 0 {
		t.Fatalf("exit = %+v", ex)
	}
}

// The agent container's runner marks its home when it starts (and leaves a
// marker that is there alone), by which the endpoint's service finds the
// volume under any path.
func TestAgentRunnerMarksItsHome(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	te.start(t)
	marker := filepath.Join(te.home, agentHomeMarker)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Lstat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent home has no marker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	te.stop()
	if err := os.WriteFile(marker, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := te.r.markHome(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(marker); string(b) != "kept" {
		t.Fatalf("the marker was rewritten: %q", b)
	}
}

// The check fails closed (denied): a mount point the runner can enter but not
// list (a run could still reach a folder in it by name), a listing that fails,
// a look a hung mount holds past lookTimeout, and a walk past its budget each
// refuse the runs, never pass what wasn't looked at.
func TestMountCheckFailsClosed(t *testing.T) {
	t.Parallel()
	hung := func(op string) func(string) error {
		return func(p string) error { return &fs.PathError{Op: op, Path: p, Err: fsutil.ErrStatTimeout} }
	}
	for name, tc := range map[string]struct {
		lstat, readDir func(string) error
		dir            func(t *testing.T, dir string)
		budgetSpent    bool
		maxEntries     int
		why            string
	}{
		"a listing that fails": {
			readDir: func(p string) error { return &fs.PathError{Op: "readdirent", Path: p, Err: syscall.EIO} },
			why:     "can't be listed",
		},
		"a mount point that can't be looked at": {
			lstat: func(p string) error { return &fs.PathError{Op: "lstat", Path: p, Err: syscall.EIO} },
			why:   "can't be looked at",
		},
		"a hung mount's folder read": {readDir: hung("readdir"), why: "didn't answer within"},
		"a hung mount's stat":        {lstat: hung("lstat"), why: "didn't answer within"},
		"the walk past its budget":   {budgetSpent: true, why: "didn't finish within"},
		"entered but not listed": {
			dir: func(t *testing.T, dir string) {
				if os.Geteuid() == 0 {
					t.Skip("root lists every dir")
				}
				if err := os.Chmod(dir, 0o100); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			},
			why: "can't be listed",
		},
		"a subfolder entered but not listed": {
			dir: func(t *testing.T, dir string) {
				if os.Geteuid() == 0 {
					t.Skip("root lists every dir")
				}
				sub := filepath.Join(dir, "a", "b")
				mkfile(t, filepath.Join(sub, "file"))
				if err := os.Chmod(sub, 0o100); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
			},
			why: "can't be listed",
		},
		"a hung subfolder": {
			dir: func(t *testing.T, dir string) { mkfile(t, filepath.Join(dir, "share", "nfs", "file")) },
			readDir: func(p string) error {
				if filepath.Base(p) == "nfs" {
					return &fs.PathError{Op: "readdir", Path: p, Err: fsutil.ErrStatTimeout}
				}
				return nil
			},
			why: "nfs didn't answer within",
		},
		"a hung subfolder's stat": {
			dir: func(t *testing.T, dir string) { mkfile(t, filepath.Join(dir, "share", "nfs", "file")) },
			lstat: func(p string) error {
				if filepath.Base(p) == "nfs" {
					return &fs.PathError{Op: "stat", Path: p, Err: fsutil.ErrStatTimeout}
				}
				return nil
			},
			why: "nfs didn't answer within",
		},
		"more entries than the walk's budget": {
			dir: func(t *testing.T, dir string) {
				for i := range 3 {
					for j := range 20 {
						mkfile(t, filepath.Join(dir, "book"+strconv.Itoa(i), strconv.Itoa(j)+".mp3"))
					}
				}
			},
			maxEntries: 50,
			why:        "more than 50 entries, more than the check looks through; mount narrower folders",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.dir != nil {
				tc.dir(t, dir)
			}
			w := newMountWalk(boundedLooks)
			if tc.budgetSpent {
				w.deadline = time.Now().Add(-time.Second)
			}
			if tc.maxEntries > 0 {
				w.maxEntries = tc.maxEntries
			}
			// An injected look's error stands for both stats (the deep walk
			// follows links, the shallow one doesn't); nil looks for real.
			if tc.lstat != nil {
				w.lstat = func(p string) (fs.FileInfo, error) {
					if err := tc.lstat(p); err != nil {
						return nil, err
					}
					return boundedLooks.lstat(p)
				}
				w.stat = func(p string) (fs.FileInfo, error) {
					if err := tc.lstat(p); err != nil {
						return nil, err
					}
					return boundedLooks.stat(p)
				}
			}
			if tc.readDir != nil {
				w.readDir = func(p string) ([]fs.DirEntry, error) {
					if err := tc.readDir(p); err != nil {
						return nil, err
					}
					return boundedLooks.readDir(p)
				}
			}
			why := w.credentialMount([]mount{at(dir)})
			if !strings.Contains(why, tc.why) || !strings.Contains(why, "can't be checked") {
				t.Fatalf("credentialMount = %q, want %q", why, tc.why)
			}
		})
	}
	// The same mount, answering, passes (allowed), and so does one within the
	// budget.
	if why := newMountWalk(boundedLooks).credentialMount([]mount{at(t.TempDir())}); why != "" {
		t.Fatalf("an empty mount = %q, want none", why)
	}
	within := t.TempDir()
	for j := range 20 {
		mkfile(t, filepath.Join(within, "book", strconv.Itoa(j)+".mp3"))
	}
	w := newMountWalk(boundedLooks)
	w.maxEntries = 50
	if why := w.credentialMount([]mount{at(within)}); why != "" {
		t.Fatalf("a mount within the budget = %q, want none", why)
	}
}

// A hung agent home counts as reachable (never a wrong no).
func TestHungHomeCountsAsReachable(t *testing.T) {
	t.Parallel()
	te, _ := newEndpointEnv(t, endpointParentEnv())
	te.r.looks.lstat = func(p string) (fs.FileInfo, error) {
		return nil, &fs.PathError{Op: "lstat", Path: p, Err: fsutil.ErrStatTimeout}
	}
	if why := te.r.credentialProblem(); !strings.Contains(why, "the agent home") {
		t.Fatalf("credentialProblem = %q, want the home reachable", why)
	}
}

// The mounts are walked once per status tick, not per call: the first call
// walks them, the calls after it take that answer, and a tick walks again.
func TestMountWalkCachedPerTick(t *testing.T) {
	t.Parallel()
	te, _ := newEndpointEnv(t, endpointParentEnv())
	var walks atomic.Int32
	te.r.mounts = func() ([]mount, error) { walks.Add(1); return nil, nil }
	for range 3 {
		_ = te.r.credentialProblem()
		_ = te.r.status()
	}
	if n := walks.Load(); n != 1 {
		t.Fatalf("%d walks before a tick, want 1", n)
	}
	te.r.tickMounts()
	_ = te.r.credentialProblem()
	if n := walks.Load(); n != 2 {
		t.Fatalf("%d walks after a tick, want 2", n)
	}
}

// A tick that finds a walk in flight (an endpoint run's start) returns at once
// and walks nothing: it never queues behind that walk to walk again, which
// over a slow mount would hold runner.json's heartbeat past the Bridge's
// freshness bound. A run's start still waits its turn and walks afresh.
func TestTickLeavesAWalkInFlight(t *testing.T) {
	t.Parallel()
	te, _ := newEndpointEnv(t, endpointParentEnv())
	var walks atomic.Int32
	te.r.mounts = func() ([]mount, error) { walks.Add(1); return nil, nil }
	te.r.mountMu.Lock()
	ticked := make(chan struct{})
	go func() { te.r.tickMounts(); close(ticked) }()
	select {
	case <-ticked:
	case <-time.After(10 * time.Second):
		te.r.mountMu.Unlock()
		t.Fatal("the tick waited behind a walk in flight")
	}
	started := make(chan struct{})
	go func() { te.r.refreshMounts(); close(started) }()
	select {
	case <-started:
		t.Fatal("a run's start walked beside a walk in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if n := walks.Load(); n != 0 {
		t.Fatalf("%d walks beside the one in flight, want none", n)
	}
	te.r.mountMu.Unlock()
	<-started
	if n := walks.Load(); n != 1 {
		t.Fatalf("%d walks once the one in flight ended, want the run's own", n)
	}
}

// A credential that came within reach after the last walk (the status tick's,
// up to an interval old) refuses the next endpoint run (denied): the run's
// start walks the mounts afresh rather than take the last answer.
func TestCredentialSinceTheLastWalkRefusesTheRun(t *testing.T) {
	t.Parallel()
	te, _ := newEndpointEnv(t, endpointParentEnv())
	planted := t.TempDir()
	mkfile(t, filepath.Join(planted, agentHomeMarker))
	var appeared atomic.Bool
	te.r.mounts = func() ([]mount, error) {
		if appeared.Load() {
			return []mount{at(planted)}, nil
		}
		return nil, nil
	}
	if why := te.r.credentialProblem(); why != "" { // the last walk: nothing
		t.Fatalf("credentialProblem = %q, want none yet", why)
	}
	appeared.Store(true)
	te.start(t)
	ex := waitDone(t, te.submit(t, endpointReq), 30*time.Second)
	if !ex.Unavailable || !strings.Contains(ex.Error, "credential of its own") {
		t.Fatalf("exit = %+v, want the run refused as unavailable", ex)
	}
	if _, err := os.Stat(filepath.Join(te.codexCap, "argv")); err == nil {
		t.Fatal("the CLI ran beside a credential the last walk hadn't seen")
	}
}

// A link directly in a mount is looked through as a folder is (denied: a run
// follows it to the vault), while a link to a file, to nowhere or to itself
// holds nothing (allowed). A container runtime's socket, mounted or in a
// mounted folder, counts as a credential's file (denied).
func TestMountCheckFollowsLinksAndFindsSockets(t *testing.T) {
	t.Parallel()
	walk := func(ms ...mount) string {
		return newMountWalk(boundedLooks).credentialMount(ms)
	}
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "appdata", "audiosilo-bridge", "secret.key"))
	if err := os.Symlink(filepath.Join("appdata", "audiosilo-bridge"), filepath.Join(dir, "bridge")); err != nil {
		t.Fatal(err)
	}
	if why := walk(at(dir)); !strings.Contains(why, filepath.Join(dir, "bridge", "secret.key")) {
		t.Fatalf("a link to the Bridge's data = %q, want it found", why)
	}
	harmless := t.TempDir()
	mkfile(t, filepath.Join(harmless, "file"))
	for name, target := range map[string]string{"to-a-file": "file", "to-nowhere": "gone", "loop": "loop"} {
		if err := os.Symlink(target, filepath.Join(harmless, name)); err != nil {
			t.Fatal(err)
		}
	}
	if why := walk(at(harmless)); why != "" {
		t.Fatalf("links to a file, to nowhere and to itself = %q, want none", why)
	}
	if why := walk(mount{root: "/run/docker.sock", point: "/var/run/docker.sock", fstype: "tmpfs"}); !strings.Contains(why, "by a credential's name") {
		t.Fatalf("docker's socket mounted = %q", why)
	}
	run := t.TempDir()
	mkfile(t, filepath.Join(run, "podman", "podman.sock"))
	if why := walk(at(run)); !strings.Contains(why, "podman.sock") {
		t.Fatalf("a host's run folder with podman's socket = %q", why)
	}
	// A host's /run is a tmpfs, looked at its point alone: the sockets a
	// folder or two down are still found, by their paths (denied), and a run
	// folder with none passes (allowed).
	for _, sock := range runtimeSocketPaths {
		tmpfs := t.TempDir()
		mkfile(t, filepath.Join(tmpfs, "user", "1000", "bus"))
		m := mount{root: "/", point: tmpfs, fstype: "tmpfs"}
		if why := walk(m); why != "" {
			t.Fatalf("a tmpfs run folder with no socket = %q, want none", why)
		}
		mkfile(t, filepath.Join(tmpfs, filepath.FromSlash(sock)))
		if why := walk(m); !strings.Contains(why, filepath.Join(tmpfs, filepath.FromSlash(sock))) {
			t.Fatalf("a tmpfs run folder with %s = %q, want it found", sock, why)
		}
	}
}

// A credential that comes within reach while an endpoint run is in progress
// (here a mount holding the agent home's marker) is found by the next status
// tick's walk, which ends the run as the agent unavailable (denied), and
// runner.json names what was found for the Bridge's page.
func TestCredentialMidRunEndsTheRun(t *testing.T) {
	t.Parallel()
	te, _ := newEndpointEnv(t, endpointParentEnv())
	te.r.statusEvery = 50 * time.Millisecond
	planted := t.TempDir()
	mkfile(t, filepath.Join(planted, agentHomeMarker))
	var appeared atomic.Bool
	te.r.mounts = func() ([]mount, error) {
		if appeared.Load() {
			return []mount{at(planted)}, nil
		}
		return nil, nil
	}
	if err := os.WriteFile(filepath.Join(te.codexCap, "behavior"), []byte("touch \"$CAP/started\"; sleep 30\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	te.start(t)
	job := te.submit(t, endpointReq)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(te.codexCap, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	appeared.Store(true)
	ex := waitDone(t, job, 20*time.Second)
	if !ex.Unavailable || !strings.Contains(ex.Error, "credential of its own") || ex.Cancelled {
		t.Fatalf("exit = %+v, want the run ended as unavailable", ex)
	}
	if ex.DurationMS > 15_000 {
		t.Fatalf("the run lasted %d ms, want it ended at the next tick", ex.DurationMS)
	}
	raw, err := os.ReadFile(filepath.Join(te.staging, agentproto.RunnerFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), agentHomeMarker) || !strings.Contains(string(raw), `"own_credentials":true`) {
		t.Fatalf("runner.json = %s, want the marker named", raw)
	}
}

// The agent container's runner closes its home, its secrets and codex's
// CODEX_HOME to other users at the start (a volume or a mounted folder keeps
// its own modes), never through a link.
func TestAgentRunnerClosesItsHome(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	elsewhere := t.TempDir()
	if err := os.Chmod(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(te.home, secretsDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(te.home, codexDir), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{te.home, filepath.Join(te.home, codexDir)} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	te.r.closeHome()
	for _, d := range []string{te.home, filepath.Join(te.home, codexDir)} {
		if info, err := os.Stat(d); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s = %v, %v, want 0700", d, info.Mode(), err)
		}
	}
	if info, _ := os.Stat(elsewhere); info.Mode().Perm() != 0o755 {
		t.Fatalf("a linked secrets dir's target = %v, want it untouched", info.Mode())
	}
	// A real secrets dir is closed too.
	te2 := newTestEnv(t, parentEnv())
	sec := filepath.Join(te2.home, secretsDir)
	if err := os.Mkdir(sec, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sec, 0o755); err != nil {
		t.Fatal(err)
	}
	te2.start(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if info, err := os.Stat(sec); err == nil && info.Mode().Perm() == 0o700 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the secrets dir was never closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The Bridge's vault names here are the vault's own: its key's variable and
// file, the GitHub token's variable, and every sealed secret it always uses.
func TestVaultNamesMatchTheVault(t *testing.T) {
	t.Parallel()
	if vaultKeyEnv != secrets.KeyEnv || vaultKeyFile != secrets.KeyFileName || githubTokenEnv != contrib.GitHubTokenEnv {
		t.Fatal("the vault's names moved: update mounts.go")
	}
	for _, n := range append(secrets.Names(), secrets.ServerToken) {
		if !slices.Contains(vaultSealed, n) {
			t.Errorf("vaultSealed lacks %s", n)
		}
	}
}

// A deep tree with nothing of the subscriptions or the vault in it passes
// (allowed): a library to maxMountDepth folders down, files at the bottom, and
// a tmpfs mounted inside it, left to its own look (its point alone), with a
// run's files one folder down.
func TestDeepCleanMountAllowed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	deep := dir
	for i := range maxMountDepth {
		deep = filepath.Join(deep, "d"+strconv.Itoa(i))
	}
	mkfile(t, filepath.Join(deep, "01.mp3"))
	mkfile(t, filepath.Join(dir, "Author", "Series", "Book", "cover.jpg"))
	tmp := filepath.Join(dir, "tmp")
	mkfile(t, filepath.Join(tmp, "run-1", "codex", "auth.json"))
	ms := []mount{at(dir), {root: "/", point: tmp, fstype: "tmpfs"}}
	if why := newMountWalk(boundedLooks).credentialMount(ms); why != "" {
		t.Fatalf("a deep clean tree = %q, want none", why)
	}
	// The same tree with the vault at its bottom is refused (denied).
	mkfile(t, filepath.Join(deep, vaultKeyFile))
	if why := newMountWalk(boundedLooks).credentialMount(ms); !strings.Contains(why, filepath.Join(deep, vaultKeyFile)) {
		t.Fatalf("the vault at the bottom = %q, want it found", why)
	}
}

// Links that lead back up the tree are walked once, never round and round
// (allowed: no loop is a credential, nor runs into the depth bound), while a
// credential reached only through a link deep down is still found (denied).
func TestDeepWalkLinkLoops(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "a", "b", "c", "file"))
	for link, target := range map[string]string{
		filepath.Join(dir, "a", "b", "c", "up"):   filepath.Join("..", ".."),
		filepath.Join(dir, "a", "b", "self"):      ".",
		filepath.Join(dir, "a", "top"):            dir,
		filepath.Join(dir, "a", "b", "c", "loop"): "loop",
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if why := newMountWalk(boundedLooks).credentialMount([]mount{at(dir)}); why != "" {
		t.Fatalf("links back up the tree = %q, want none", why)
	}
	elsewhere := t.TempDir()
	mkfile(t, filepath.Join(elsewhere, "bridge-data", "secrets", "server_token.enc"))
	if err := os.Symlink(elsewhere, filepath.Join(dir, "a", "b", "c", "out")); err != nil {
		t.Fatal(err)
	}
	if why := newMountWalk(boundedLooks).credentialMount([]mount{at(dir)}); !strings.Contains(why, "server_token.enc") {
		t.Fatalf("a link out to the vault = %q, want it found", why)
	}
}

// The container's own root is looked at shallow, as before the deep walk: its
// point and one folder below (allowed: the image's trees are deeper than
// maxMountDepth), still finding a login there (denied).
func TestRootMountShallow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	deep := root
	for i := range maxMountDepth + 2 {
		deep = filepath.Join(deep, "usr"+strconv.Itoa(i))
	}
	mkfile(t, filepath.Join(deep, "lib.so"))
	walk := func() string {
		w := newMountWalk(boundedLooks)
		w.rootPoint = root
		return w.credentialMount([]mount{{root: "/", point: root, fstype: "overlay"}})
	}
	if why := walk(); why != "" {
		t.Fatalf("the image's deep root = %q, want none", why)
	}
	mkfile(t, filepath.Join(root, "home", ".codex", "auth.json"))
	if why := walk(); !strings.Contains(why, "auth.json") {
		t.Fatalf("a login in the root = %q, want it found", why)
	}
}
