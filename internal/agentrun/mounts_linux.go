//go:build linux

package agentrun

import (
	"errors"
	"os"
)

// readMounts lists this process's mounts (mounts.go). None at all is an error:
// a process always has its root, so mountinfo wasn't read as it should be.
func readMounts() ([]mount, error) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	ms := parseMountinfo(b)
	if len(ms) == 0 {
		return nil, errors.New("/proc/self/mountinfo lists no mount")
	}
	return ms, nil
}
