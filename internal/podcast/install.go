package podcast

import (
	"errors"
	"os"
)

// linkFile is root.Link; tests replace it to stand in for a filesystem without
// hard links.
var linkFile = func(root *os.Root, oldname, newname string) error { return root.Link(oldname, newname) }

// installAudio puts the staged temp at name and never replaces a file there. An
// exclusive hard link is the first choice everywhere. A filesystem without hard
// links (SMB/CIFS, some NAS or FUSE shares, bind mounts of those) falls back to a
// rename that refuses an existing target. It reports whether temp still exists
// (it was linked), which the caller then removes.
func installAudio(root *os.Root, temp, name string) (linked bool, err error) {
	err = linkFile(root, temp, name)
	if err == nil {
		return true, nil
	}
	if !linkUnsupported(err) {
		return false, err
	}
	return false, renameNoReplace(root, temp, name)
}

// linkUnsupported reports whether a failed link means the filesystem has no hard
// links (as opposed to a target that exists, or a real permission problem the
// rename would meet too).
func linkUnsupported(err error) bool {
	for _, unsupported := range unsupportedLinkErrors {
		if errors.Is(err, unsupported) {
			return true
		}
	}
	return false
}

// renameIfAbsent is the portable no-replace rename. The caller holds the show's
// mutation lock and the episode's file lock, so only a writer outside the Bridge
// can race the absence check, which runs immediately before the rename.
func renameIfAbsent(root *os.Root, oldname, newname string) error {
	if _, err := root.Lstat(newname); err == nil {
		return ErrInvalidFile
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return root.Rename(oldname, newname)
}
