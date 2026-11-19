//go:build linux

package agentrun

import (
	"os"
	"path/filepath"
	"testing"
)

// The real mountinfo is read: every process has its root mounted. A tmpfs's
// folder shows, to a stat, the device mountinfo gives it (runMade and
// removeRunFile hold a run's files to the one TmpDir's tmpfs shows).
func TestReadMounts(t *testing.T) {
	t.Parallel()
	ms, err := readMounts()
	if err != nil {
		t.Fatal(err)
	}
	top := map[string]mount{} // the last mount at each point, the one a path reaches
	for _, m := range ms {
		top[m.point] = m
	}
	if m, ok := top["/"]; !ok || m.fstype == "" {
		t.Fatalf("no root mount in %+v", ms)
	}
	for point, m := range top {
		if m.fstype != "tmpfs" {
			continue
		}
		if info, err := os.Lstat(point); err == nil && info.IsDir() {
			if dev, _ := deviceOf(info); dev != m.dev {
				t.Errorf("the tmpfs at %s: a stat shows device %s, mountinfo %s", point, dev, m.dev)
			}
		}
	}
}

// removeRunFile never takes a file through a mount on its way: /dev/shm is a
// tmpfs of its own inside /dev, so a file of the service's uid there, reached
// from /dev, is on another device than the area's (denied: kept).
func TestRemoveRunFileStopsAtAMount(t *testing.T) {
	t.Parallel()
	if info, err := os.Lstat("/dev/shm"); err != nil || !info.IsDir() {
		t.Skip("no /dev/shm folder here")
	}
	dev := devOf(t, "/dev")
	if devOf(t, "/dev/shm") == dev {
		t.Skip("/dev/shm is no mount of its own here")
	}
	f, err := os.CreateTemp("/dev/shm", "auth.json-")
	if err != nil {
		t.Skip("/dev/shm can't be written here: ", err)
	}
	_ = f.Close()
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	if removeRunFile("/dev", dev, filepath.Join("shm", filepath.Base(f.Name())), os.Geteuid()) {
		t.Fatal("removeRunFile removed a file through a mount on its way")
	}
	if _, err := os.Lstat(f.Name()); err != nil {
		t.Fatalf("the file past the mount: %v, want it kept", err)
	}
}
