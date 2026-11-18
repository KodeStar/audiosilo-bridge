package agentrun

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
