//go:build !unix

package agentbox

import "io/fs"

// fileOwner reports no owner off Unix: the agent container's staging volume
// is a Linux one, where owner_unix.go reads it.
func fileOwner(fs.FileInfo) (uid int, links uint64, ok bool) { return 0, 0, false }
