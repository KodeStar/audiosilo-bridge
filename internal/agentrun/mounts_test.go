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
		"a mount with more entries than the check looks through": {
			mounts: func(t *testing.T, dir string) ([]mount, error) {
				for i := range maxMountChildren + 1 {
					if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return []mount{at(dir)}, nil
			},
			why: "more than 1024 entries",
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
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.dir != nil {
				tc.dir(t, dir)
			}
			w := &mountWalk{looks: boundedLooks, deadline: time.Now().Add(time.Minute)}
			if tc.budgetSpent {
				w.deadline = time.Now().Add(-time.Second)
			}
			if tc.lstat != nil {
				w.lstat = func(p string) (fs.FileInfo, error) { return nil, tc.lstat(p) }
			}
			if tc.readDir != nil {
				w.readDir = func(p string) ([]fs.DirEntry, error) { return nil, tc.readDir(p) }
			}
			why := w.credentialMount([]mount{at(dir)})
			if !strings.Contains(why, tc.why) || !strings.Contains(why, "can't be checked") {
				t.Fatalf("credentialMount = %q, want %q", why, tc.why)
			}
		})
	}
	// The same mount, answering, passes (allowed).
	w := &mountWalk{looks: boundedLooks, deadline: time.Now().Add(time.Minute)}
	if why := w.credentialMount([]mount{at(t.TempDir())}); why != "" {
		t.Fatalf("an empty mount = %q, want none", why)
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

// The mounts are walked once per status interval, not per call: the calls in
// between take the last walk's answer, a status tick walks again whatever its
// age, and so does the next call once the last is an interval old.
func TestMountWalkCachedPerTick(t *testing.T) {
	t.Parallel()
	te, _ := newEndpointEnv(t, endpointParentEnv())
	var walks atomic.Int32
	te.r.mounts = func() ([]mount, error) { walks.Add(1); return nil, nil }
	now := time.Unix(1_800_000_000, 0)
	te.r.now = func() time.Time { return now }
	for range 3 {
		_ = te.r.credentialProblem()
		_ = te.r.status()
	}
	if n := walks.Load(); n != 1 {
		t.Fatalf("%d walks in one interval, want 1", n)
	}
	te.r.refreshMounts()
	if n := walks.Load(); n != 2 {
		t.Fatalf("%d walks after a tick, want 2", n)
	}
	now = now.Add(te.r.statusEvery)
	_ = te.r.credentialProblem()
	if n := walks.Load(); n != 3 {
		t.Fatalf("%d walks an interval later, want 3", n)
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
