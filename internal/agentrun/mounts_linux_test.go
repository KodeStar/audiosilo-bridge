//go:build linux

package agentrun

import "testing"

// The real mountinfo is read: every process has its root mounted.
func TestReadMounts(t *testing.T) {
	t.Parallel()
	ms, err := readMounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.point == "/" && m.fstype != "" {
			return
		}
	}
	t.Fatalf("no root mount in %+v", ms)
}
