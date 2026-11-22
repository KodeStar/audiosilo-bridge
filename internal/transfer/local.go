// Package transfer installs one audiobook file into a library the Bridge has a
// checked, writable mapping of (Local: this host or a mounted share). It never
// replaces a file: the copy is staged in a temp file beside the destination and
// installed with fsutil.InstallNoReplace (an exclusive hard link, else a rename
// that refuses an existing target), the podcast episode install's primitive.
// Every destination goes through mapping.SafeJoin against the mapping's root and
// every filesystem call through an os.Root there, so neither a "..", an
// absolute path nor a symlink in the library reaches outside it.
package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/progress"
)

// Errors Place answers (wrapped; test with errors.Is).
var (
	// ErrExists: a different file (or a folder, or a link) is already at the
	// destination. Nothing was written there.
	ErrExists = errors.New("a different file is already at the destination")
	// ErrDiskFull: the destination's filesystem has too little free space (or
	// ran out during the copy). Nothing was left behind.
	ErrDiskFull = errors.New("not enough free space in the library")
	// ErrInvalidPath: the destination isn't a usable library-relative file path
	// (outside the library, empty, a folder, a hidden name).
	ErrInvalidPath = errors.New("the destination isn't a valid path in the library")
)

// DefaultHeadroom is the free space Place wants at the destination, as a factor
// of the file's size: the staged copy plus a margin for the filesystem.
const DefaultHeadroom = 1.1

// tempPrefix names Place's staged copies (a dotfile the server's scan skips).
const tempPrefix = ".audiosilo-import-"

// PlaceRequest is one file to install.
type PlaceRequest struct {
	// Root is the library's mount on the Bridge (a checked, writable mapping).
	Root string
	// RelPath is the destination file, library-relative and "/"-separated.
	RelPath string
	// SrcPath is the file to copy (on this machine).
	SrcPath string
	// LinkSource allows a hard link to the caller's finished, private staging file.
	// It already has library permissions and the caller only unlinks it after Place.
	LinkSource bool
	// BeforeInstall confirms the lease immediately before publishing the staged file.
	BeforeInstall func(context.Context) error
	// Progress (may be nil) reports bytes copied, throttled.
	Progress func(copied, total int64)
}

// PlaceResult is the outcome of a Place.
type PlaceResult struct {
	// RelPath is the destination as installed (clean, "/"-separated).
	RelPath string
	// Size is the file's size.
	Size int64
	// AlreadyPresent: the destination already held these exact bytes (an earlier
	// run installed them), so nothing was copied.
	AlreadyPresent bool
}

// Local installs into a directory on this machine (a local path or a mounted
// SMB/NFS share).
type Local struct {
	// Seams for tests: the free-space reader and the hard link. A racing
	// writer is PlaceRequest.BeforeInstall.
	free func(dir string) (int64, error)
	link fsutil.LinkFunc
}

// NewLocal returns a Local transfer.
func NewLocal() *Local { return &Local{} }

// CleanRel checks a library-relative destination file path and returns it
// clean: through mapping.SafeJoin's rules (no absolute path, no "..", no NUL),
// not the library root itself, and no hidden (dot) segment, folder or file:
// the server's scan never indexes a path with one.
func CleanRel(rel string) (string, error) {
	if _, err := mapping.SafeJoin("/", rel); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	clean := strings.TrimPrefix(path.Clean("/"+rel), "/")
	if clean == "" {
		return "", ErrInvalidPath
	}
	for _, seg := range strings.Split(clean, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", fmt.Errorf("%w: %q is a hidden name", ErrInvalidPath, seg)
		}
	}
	return clean, nil
}

// OnDisk is rel (clean) spelled as the library spells what exists of it: a
// folder (or the file) already there under another spelling, which a case- or
// normalization-insensitive filesystem (APFS, SMB, NTFS) finds under rel's,
// takes the name its folder lists, since the server indexes a path only as it
// is spelled on disk. What doesn't exist yet keeps rel's spelling.
func OnDisk(fsys LibraryFS, rel string) (string, error) {
	segs := strings.Split(rel, "/")
	dir := "."
	for i, seg := range segs {
		st, err := fsys.Lstat(path.Join(dir, seg))
		if errors.Is(err, fs.ErrNotExist) {
			return path.Join(append([]string{dir}, segs[i:]...)...), nil
		}
		if err != nil {
			return "", err
		}
		name, err := listedName(fsys, dir, seg, st)
		if err != nil {
			return "", err
		}
		dir = path.Join(dir, name)
	}
	return dir, nil
}

// listedName is the name dir lists for the entry found as seg (st): seg when
// it is listed as is, else the listed entry that is the same file.
func listedName(fsys LibraryFS, dir, seg string, st fs.FileInfo) (string, error) {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() == seg {
			return seg, nil
		}
	}
	// The same name folded first: each entry looked at is a stat, and a
	// library's root may list thousands.
	for _, folded := range []bool{true, false} {
		for _, e := range entries {
			if strings.EqualFold(e.Name(), seg) != folded {
				continue
			}
			if info, err := e.Info(); err == nil && os.SameFile(info, st) {
				return e.Name(), nil
			}
		}
	}
	return seg, nil
}

// LibraryFS is a library's mount as OnDisk reads it: RootFS, or a view over it
// (one that lists each folder once, say).
type LibraryFS interface {
	fs.ReadLinkFS
	fs.ReadDirFS
}

// RootFS is root's files as a LibraryFS.
func RootFS(root *os.Root) LibraryFS { return root.FS().(LibraryFS) }

// Place copies SrcPath to RelPath under Root and never replaces anything:
//   - the destination holding the same bytes already is AlreadyPresent (an
//     earlier run installed it), anything else there is ErrExists;
//   - less free space than DefaultHeadroom x the size at the destination is
//     ErrDiskFull, as is running out during the copy;
//   - the copy is staged beside the destination, size-checked and installed
//     with fsutil.InstallNoReplace; a file that appears meanwhile wins and is
//     ErrExists (or AlreadyPresent when it is these bytes). A failure or a
//     cancel (ctx) leaves no staged copy behind.
//
// Missing folders are created (0755 under the umask). The caller checks the
// mapping is still writable before calling.
func (l *Local) Place(ctx context.Context, req PlaceRequest) (PlaceResult, error) {
	if err := ctx.Err(); err != nil {
		return PlaceResult{}, err
	}
	rel, err := CleanRel(req.RelPath)
	if err != nil {
		return PlaceResult{}, err
	}
	// A root that is missing or not a folder (an unmounted share) fails here,
	// before any copy, rather than getting a stub tree.
	root, err := os.OpenRoot(req.Root)
	if err != nil {
		return PlaceResult{}, fmt.Errorf("library root unavailable: %w", err)
	}
	defer func() { _ = root.Close() }()
	src, err := os.Open(req.SrcPath)
	if err != nil {
		return PlaceResult{}, err
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return PlaceResult{}, err
	}
	if !info.Mode().IsRegular() {
		return PlaceResult{}, fmt.Errorf("%s is not a regular file", req.SrcPath)
	}
	size := info.Size()
	res := PlaceResult{RelPath: rel, Size: size}

	if same, err := sameAt(ctx, root, rel, src, info); err != nil {
		return PlaceResult{}, err
	} else if same {
		res.AlreadyPresent = true
		return res, nil
	}
	// A copy checks the room before creating folders; a link needs none
	// unless it falls back to a copy (stage checks then).
	if !req.LinkSource {
		if err := l.checkFree(req.Root, rel, size); err != nil {
			return PlaceResult{}, err
		}
	}
	dir := path.Dir(rel)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: library folders the server (another user) must read
			return PlaceResult{}, noSpace(fmt.Errorf("creating the book's folder: %w", err))
		}
	}
	req.RelPath = rel
	temp, err := l.stage(ctx, root, src, size, req)
	if err != nil {
		return PlaceResult{}, err
	}
	defer func() { _ = root.Remove(temp) }() // gone already when the install renamed it
	if req.BeforeInstall != nil {
		if err := req.BeforeInstall(ctx); err != nil {
			return PlaceResult{}, fmt.Errorf("confirming the lease before installing the book: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return PlaceResult{}, err
	}
	if _, err := fsutil.InstallNoReplace(root, temp, rel, l.link); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return PlaceResult{}, err
		}
		// Something arrived at the destination meanwhile: these bytes are fine,
		// anything else stays as it is.
		same, serr := sameAt(ctx, root, rel, src, info)
		if serr != nil {
			return PlaceResult{}, serr
		}
		if !same {
			return PlaceResult{}, fmt.Errorf("%w: %s", ErrExists, rel)
		}
		res.AlreadyPresent = true
	}
	return res, nil
}

// Matches checks the bytes at a previous placement destination during crash
// recovery. A different file, link, folder or missing file never matches.
func Matches(ctx context.Context, mount, rel, staged string) (bool, error) {
	rel, err := CleanRel(rel)
	if err != nil {
		return false, err
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	src, err := os.Open(staged) //nolint:gosec // G304: private staging path from the validated account ID and ASIN
	if err != nil {
		return false, err
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return false, err
	}
	same, err := sameAt(ctx, root, rel, src, info)
	if errors.Is(err, ErrExists) {
		return false, nil
	}
	return same, err
}

// sameAt reports whether rel holds exactly src's bytes. Nothing there is false;
// a folder, a link, a special file or a different file is ErrExists.
func sameAt(ctx context.Context, root *os.Root, rel string, src *os.File, info fs.FileInfo) (bool, error) {
	st, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() || st.Size() != info.Size() {
		return false, fmt.Errorf("%w: %s", ErrExists, rel)
	}
	if os.SameFile(st, info) {
		return true, ctx.Err()
	}
	dst, err := root.Open(rel)
	if err != nil {
		return false, err
	}
	defer func() { _ = dst.Close() }()
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	equal, err := sameBytes(ctx, src, dst)
	if err != nil {
		return false, err
	}
	if !equal {
		return false, fmt.Errorf("%w: %s", ErrExists, rel)
	}
	return true, nil
}

// sameBytes compares two readers to their ends.
func sameBytes(ctx context.Context, a, b io.Reader) (bool, error) {
	const chunk = 1 << 20
	ba, bb := make([]byte, chunk), make([]byte, chunk)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		na, ea := io.ReadFull(a, ba)
		nb, eb := io.ReadFull(b, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		endA := errors.Is(ea, io.EOF) || errors.Is(ea, io.ErrUnexpectedEOF)
		endB := errors.Is(eb, io.EOF) || errors.Is(eb, io.ErrUnexpectedEOF)
		switch {
		case ea != nil && !endA:
			return false, ea
		case eb != nil && !endB:
			return false, eb
		case endA || endB:
			return endA && endB, nil
		}
	}
}

// checkFree wants DefaultHeadroom x size free on the filesystem that will
// hold rel (its nearest existing folder).
func (l *Local) checkFree(rootDir, rel string, size int64) error {
	free := l.free
	if free == nil {
		free = fsutil.FreeBytes
	}
	n, err := free(NearestDir(rootDir, rel))
	if err != nil {
		// The free space can't be read there (a share that doesn't answer
		// statfs): the copy goes ahead, and running out during it is still
		// ErrDiskFull.
		return nil
	}
	if need := int64(float64(size) * DefaultHeadroom); n < need {
		return fmt.Errorf("%w: %d bytes free, %d needed", ErrDiskFull, n, need)
	}
	return nil
}

// NearestDir is the nearest existing folder of rel's (a clean library-relative
// file path) under rootDir, rootDir itself at the latest: where the free space
// for a file at rel is read.
func NearestDir(rootDir, rel string) string {
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		full := filepath.Join(rootDir, filepath.FromSlash(dir))
		if st, err := os.Stat(full); err == nil && st.IsDir() {
			return full
		}
	}
	return rootDir
}

// stage links (req.LinkSource) or copies src into a new temp file beside the
// cleaned req.RelPath, a copy synced and size-checked, and returns its
// root-relative name. A failure removes it.
func (l *Local) stage(ctx context.Context, root *os.Root, src *os.File, size int64, req PlaceRequest) (string, error) {
	dir := path.Dir(req.RelPath)
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	temp := path.Join(dir, tempPrefix+hex.EncodeToString(b)+".part")
	if req.LinkSource {
		linked, err := linkSource(root, dir, src.Name(), path.Base(temp))
		if err != nil {
			return "", noSpace(err)
		}
		if linked {
			if req.Progress != nil {
				req.Progress(size, size)
			}
			return temp, nil
		}
		if err := l.checkFree(req.Root, req.RelPath, size); err != nil {
			return "", err
		}
	}
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", noSpace(err)
	}
	fail := func(err error) (string, error) {
		_ = f.Close()
		_ = root.Remove(temp)
		return "", noSpace(err)
	}
	// Keep both files visible to io.CopyN so Go can use the kernel's copy
	// operation. Bound each call to keep cancellation and progress responsive.
	for copied := int64(0); copied < size; {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		n, err := io.CopyN(f, src, min(progress.EmitEvery, size-copied))
		if err != nil {
			return fail(err)
		}
		copied += n
		if req.Progress != nil {
			req.Progress(copied, size)
		}
	}
	if st, err := src.Stat(); err != nil {
		return fail(err)
	} else if st.Size() != size {
		return fail(fmt.Errorf("source size changed during copy: expected %d, got %d", size, st.Size()))
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(temp)
		return "", noSpace(err)
	}
	return temp, nil
}

// noSpace marks a filesystem-full error as ErrDiskFull (keeping the cause).
func noSpace(err error) error {
	if fsutil.IsNoSpace(err) {
		return fmt.Errorf("%w: %v", ErrDiskFull, err)
	}
	return err
}
