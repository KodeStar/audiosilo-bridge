//go:build !linux

package agentbox

import "os"

// dropACL does nothing off Linux: the agent container's staging volume is a
// Linux one, where acl_linux.go removes the POSIX ACLs a file inherited.
func dropACL(*os.File) error { return nil }
