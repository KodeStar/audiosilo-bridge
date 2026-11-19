//go:build unix

package agentrun

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// supported reports whether the runner can run here (it needs process groups).
const supported = true

// setProcGroup puts the CLI in a session of its own (and so a process group of
// its own, the CLI's pid), so a cancel signals every helper it started, and
// nothing it starts can join another run's group (a group is joined only
// within its session): the endpoint service's sweep tells a run's processes by
// their session (sweep.go).
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// signalGroup signals the CLI's whole process group (gone already is fine).
func signalGroup(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}

// exitCode is the CLI's exit code; a signal is 128+signal, as a shell reports it.
func exitCode(ps *os.ProcessState, werr error) int {
	if ps == nil {
		if werr != nil {
			return -1
		}
		return 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// setUmask sets the process umask (inherited by every CLI the runner starts).
func setUmask(mask int) { syscall.Umask(mask) }

// enterable reports whether this process may write or search dir (by its
// effective ids, as a later open would be judged).
func enterable(dir string) bool {
	return unix.Faccessat(unix.AT_FDCWD, dir, unix.W_OK, unix.AT_EACCESS) == nil ||
		unix.Faccessat(unix.AT_FDCWD, dir, unix.X_OK, unix.AT_EACCESS) == nil
}

// closeDir makes the folder dir 0700 unless it is that already, through the
// folder itself opened without following a link: a link swapped in after any
// earlier look is refused (ELOOP), never followed. Neither a link nor a file
// is changed (ELOOP, ENOTDIR).
func closeDir(dir string) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: dir, Err: err}
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &fs.PathError{Op: "fstat", Path: dir, Err: err}
	}
	if st.Mode&0o777 == 0o700 {
		return nil
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return &fs.PathError{Op: "fchmod", Path: dir, Err: err}
	}
	return nil
}

// removeRunFile removes rel below area (a run's planted file, or an empty
// folder) when it is still uid's and on the device dev (major:minor, the one
// mountinfo showed for area's tmpfs): the area and each folder below it on
// rel's way are opened without following a link, so one swapped in since the
// walk stops it (ELOOP, ENOTDIR) rather than lead anywhere, each must be on
// dev (a mount on the way, or one over area since mountinfo was read, or a
// link above area that leads off it, is another file system: never a host's
// file), and rel's last element is looked at and removed in the folder so
// opened. A folder goes only when empty.
func removeRunFile(area, dev, rel string, uid int) bool {
	parts := strings.Split(rel, string(filepath.Separator))
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open(area, dirFlags, 0)
	if err != nil {
		return false
	}
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || devString(uint64(st.Dev)) != dev { // Dev is an int32 on darwin
		return false
	}
	areaDev := st.Dev
	for _, name := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, name, dirFlags, 0)
		_ = unix.Close(fd)
		fd = -1
		if err != nil {
			return false
		}
		fd = next
		if unix.Fstat(fd, &st) != nil || st.Dev != areaDev {
			return false
		}
	}
	last := parts[len(parts)-1]
	if err := unix.Fstatat(fd, last, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil || int(st.Uid) != uid || st.Dev != areaDev {
		return false
	}
	flags := 0
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		flags = unix.AT_REMOVEDIR
	}
	return unix.Unlinkat(fd, last, flags) == nil
}

// ownerUID is info's owner, as this process's user namespace shows it; ok
// false where the OS reports none.
func ownerUID(info fs.FileInfo) (uid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// deviceOf is info's device as mountinfo writes one (major:minor); ok false
// where the OS reports none.
func deviceOf(info fs.FileInfo) (dev string, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return devString(uint64(st.Dev)), true // Dev is an int32 on darwin
}

// devString is the device d as major:minor.
func devString(d uint64) string {
	return strconv.FormatUint(uint64(unix.Major(d)), 10) + ":" + strconv.FormatUint(uint64(unix.Minor(d)), 10)
}

// fileKey is a file's device and inode: the deep mount walk's mark of a
// folder it has been in (a link back to it is a loop).
type fileKey struct{ dev, ino uint64 }

// fileKeyOf is info's fileKey, false when the system gives none.
func fileKeyOf(info fs.FileInfo) (fileKey, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, false
	}
	return fileKey{dev: uint64(st.Dev), ino: st.Ino}, true // Dev is an int32 on darwin
}
