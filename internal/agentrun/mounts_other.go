//go:build !linux

package agentrun

// readMounts: no mountinfo here, and the endpoint-only runner (the one that
// reads it) refuses to start off Linux (sweep_other.go).
func readMounts() ([]mount, error) { return nil, nil }
