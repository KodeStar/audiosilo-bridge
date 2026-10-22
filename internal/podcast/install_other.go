//go:build !linux

package podcast

import "os"

// renameNoReplace has no portable atomic form here: the absence check under the
// locks guards the rename.
func renameNoReplace(root *os.Root, oldname, newname string) error {
	return renameIfAbsent(root, oldname, newname)
}
