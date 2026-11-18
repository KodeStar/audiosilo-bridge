//go:build !linux

package agentrun

import "errors"

// The endpoint-only runner needs Linux (a child subreaper and /proc); the
// agent container is Linux, and elsewhere such a runner refuses to start.

func endpointHardening() error { return errors.New("it runs in the agent image (Linux)") }

func ownSession() int { return -1 }

func killStrays(func(int) bool) int { return 0 }
