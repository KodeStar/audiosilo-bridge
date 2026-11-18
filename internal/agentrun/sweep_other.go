//go:build !linux

package agentrun

import "errors"

// The endpoint-only runner needs Linux (a child subreaper and /proc); the
// agent container is Linux, and elsewhere such a runner refuses to start.

const sweepSupported = false

func endpointHardening() error { return errors.New("the endpoint's agent service runs on Linux") }

func ownSession() int { return -1 }

func killStrays(func(int) bool) int { return 0 }
