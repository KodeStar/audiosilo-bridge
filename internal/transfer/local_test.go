package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func source(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.m4b")
	writeFile(t, p, content)
	return p
}

// noTemps fails when a staged copy is left anywhere under root.
func noTemps(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), tempPrefix) {
			t.Errorf("staged copy left behind: %s", p)
		}
		return nil
	})
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPlaceInstallsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src := source(t, "hello-audio")
	var reported int64
	req := PlaceRequest{Root: root, RelPath: "Will Wight/Cradle/C02 - Soulsmith/Soulsmith.m4b", SrcPath: src,
		Progress: func(c, _ int64) { reported = c }}
	res, err := NewLocal().Place(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyPresent || res.Size != 11 || res.RelPath != "Will Wight/Cradle/C02 - Soulsmith/Soulsmith.m4b" {
		t.Fatalf("first place = %+v", res)
	}
	dst := filepath.Join(root, "Will Wight", "Cradle", "C02 - Soulsmith", "Soulsmith.m4b")
	if read(t, dst) != "hello-audio" {
		t.Fatal("content differs")
	}
	if reported != 11 {
		t.Fatalf("progress reported %d", reported)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(dst); st.Mode().Perm()&0o044 != 0o044 {
			t.Fatalf("installed mode %v: the server (another user) must read it", st.Mode())
		}
	}
	res2, err := NewLocal().Place(ctx, req)
	if err != nil || !res2.AlreadyPresent {
		t.Fatalf("re-place of the same bytes = %+v, %v; want AlreadyPresent", res2, err)
	}
	noTemps(t, root)
}

func TestPlaceRefusesADifferentFile(t *testing.T) {
	for name, existing := range map[string]string{"other size": "old-bigger-content", "same size": "HELLO-AUDIO"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dst := filepath.Join(root, "B", "f.m4b")
			writeFile(t, dst, existing)
			_, err := NewLocal().Place(context.Background(), PlaceRequest{Root: root, RelPath: "B/f.m4b", SrcPath: source(t, "hello-audio")})
			if !errors.Is(err, ErrExists) {
				t.Fatalf("err = %v, want ErrExists", err)
			}
			if read(t, dst) != existing {
				t.Fatal("the existing file was changed")
			}
			noTemps(t, root)
		})
	}
}

func TestPlaceRefusesAFolderOrLinkAtTheDestination(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "B", "f.m4b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal().Place(context.Background(), PlaceRequest{Root: root, RelPath: "B/f.m4b", SrcPath: source(t, "x")}); !errors.Is(err, ErrExists) {
		t.Fatalf("folder at destination: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(t.TempDir(), "elsewhere.m4b")
	writeFile(t, target, "x")
	if err := os.Symlink(target, filepath.Join(root, "B", "g.m4b")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal().Place(context.Background(), PlaceRequest{Root: root, RelPath: "B/g.m4b", SrcPath: source(t, "x")}); !errors.Is(err, ErrExists) {
		t.Fatalf("symlink at destination (even to the same bytes): %v", err)
	}
}

// SafeJoin's rules hold for every destination: a path that leaves the library
// is refused outright (never clamped), and nothing is written.
func TestPlaceSafeJoinRefusals(t *testing.T) {
	outside := t.TempDir()
	root := filepath.Join(t.TempDir(), "lib")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	src := source(t, "payload")
	for _, rel := range []string{"../escape.m4b", "../../etc/passwd", "a/../../b.m4b", "/abs/path.m4b", "", ".", "a/..", "a\x00b.m4b", "Author/.hidden.m4b",
		".audible/Author/x.m4b", "Author/.hidden/x.m4b"} {
		_, err := NewLocal().Place(context.Background(), PlaceRequest{Root: root, RelPath: rel, SrcPath: src})
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Place(%q) = %v, want ErrInvalidPath", rel, err)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("refused places wrote %v", entries)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// A symlink inside the library pointing out of it is not a way out either.
	if err := os.Symlink(outside, filepath.Join(root, "Out")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocal().Place(context.Background(), PlaceRequest{Root: root, RelPath: "Out/Book/x.m4b", SrcPath: src}); err == nil {
		t.Fatal("a symlinked folder led outside the library")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside the library: %v", entries)
	}
}

func TestPlaceAllowedPaths(t *testing.T) {
	root := t.TempDir()
	rels := []string{"Author/Series/Book 1/file.m4b", "loose.m4b", "./Author/./B/x.m4b"}
	if runtime.GOOS != "windows" {
		rels = append(rels, `AC\DC/Live/x.m4b`) // a backslash is a name character here
	}
	for _, rel := range rels {
		res, err := NewLocal().Place(context.Background(), PlaceRequest{Root: root, RelPath: rel, SrcPath: source(t, rel)})
		if err != nil {
			t.Fatalf("Place(%q): %v", rel, err)
		}
		if read(t, filepath.Join(root, filepath.FromSlash(res.RelPath))) != rel {
			t.Fatalf("Place(%q) wrote elsewhere: %+v", rel, res)
		}
	}
}

func TestPlaceDiskFull(t *testing.T) {
	root := t.TempDir()
	l := NewLocal()
	var asked string
	l.free = func(dir string) (int64, error) { asked = dir; return 1000, nil }
	_, err := l.Place(context.Background(), PlaceRequest{Root: root, RelPath: "New Author/Book/x.m4b", SrcPath: source(t, strings.Repeat("a", 950))})
	if !errors.Is(err, ErrDiskFull) {
		t.Fatalf("err = %v, want ErrDiskFull (950 x 1.1 > 1000)", err)
	}
	if asked != root {
		t.Fatalf("free space read at %q, want the nearest existing folder %q", asked, root)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("a refused place left %v", entries)
	}
	if _, err := l.Place(context.Background(), PlaceRequest{Root: root, RelPath: "New Author/Book/x.m4b", SrcPath: source(t, strings.Repeat("a", 900))}); err != nil {
		t.Fatalf("900 x 1.1 fits in 1000: %v", err)
	}
}

func TestNoSpaceDuringTheCopyIsDiskFull(t *testing.T) {
	err := noSpace(&os.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC})
	if runtime.GOOS != "windows" && !errors.Is(err, ErrDiskFull) {
		t.Fatalf("ENOSPC = %v, want ErrDiskFull", err)
	}
	if errors.Is(noSpace(os.ErrPermission), ErrDiskFull) {
		t.Fatal("a permission error is not disk full")
	}
}

// The no-replace race: a file appears at the destination after the checks and
// before the install. It is never overwritten, whether the filesystem has hard
// links or only the no-replace rename.
func TestPlaceNoReplaceRace(t *testing.T) {
	unsupported := func(_ *os.Root, oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: fsutil.UnsupportedLinkErrors[0]}
	}
	for name, link := range map[string]fsutil.LinkFunc{"hard link": nil, "rename fallback": unsupported} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dst := filepath.Join(root, "A", "b.m4b")
			l := NewLocal()
			l.link = link
			l.beforeInstall = func() { writeFile(t, dst, "an administrator's own file") }
			_, err := l.Place(context.Background(), PlaceRequest{Root: root, RelPath: "A/b.m4b", SrcPath: source(t, "ours")})
			if !errors.Is(err, ErrExists) {
				t.Fatalf("err = %v, want ErrExists", err)
			}
			if read(t, dst) != "an administrator's own file" {
				t.Fatal("the racing file was overwritten")
			}
			noTemps(t, root)

			// The same bytes arriving meanwhile (another run of this job) are fine.
			root2 := t.TempDir()
			dst2 := filepath.Join(root2, "A", "b.m4b")
			l.beforeInstall = func() { writeFile(t, dst2, "ours") }
			res, err := l.Place(context.Background(), PlaceRequest{Root: root2, RelPath: "A/b.m4b", SrcPath: source(t, "ours")})
			if err != nil || !res.AlreadyPresent {
				t.Fatalf("same bytes raced in: %+v, %v", res, err)
			}
			noTemps(t, root2)
		})
	}
}

func TestPlaceCancelLeavesNothing(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewLocal().Place(ctx, PlaceRequest{Root: root, RelPath: "d2/b.m4b", SrcPath: source(t, strings.Repeat("z", 4<<20))}); err == nil {
		t.Fatal("expected the cancelled place to fail")
	}
	if _, err := os.Stat(filepath.Join(root, "d2", "b.m4b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cancelled place must not leave a destination file")
	}
	noTemps(t, root)
}

// A missing root (an unmounted share), or a file where the root should be,
// fails before anything is written.
func TestPlaceMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := NewLocal().Place(context.Background(), PlaceRequest{Root: missing, RelPath: "a/b.m4b", SrcPath: source(t, "x")}); err == nil {
		t.Fatal("an unmounted library must not get a stub tree")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the missing root was created: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	writeFile(t, file, "not a folder")
	if _, err := NewLocal().Place(context.Background(), PlaceRequest{Root: file, RelPath: "a/b.m4b", SrcPath: source(t, "x")}); err == nil {
		t.Fatal("a file as the library root was accepted")
	}
}

// A filesystem whose free space can't be read (a share that doesn't answer
// statfs) is no refusal: the copy goes ahead.
func TestPlaceUnreadableFreeSpace(t *testing.T) {
	root := t.TempDir()
	l := NewLocal()
	l.free = func(string) (int64, error) { return 0, syscall.ENOSYS }
	res, err := l.Place(context.Background(), PlaceRequest{Root: root, RelPath: "A/B/x.m4b", SrcPath: source(t, "payload")})
	if err != nil || read(t, filepath.Join(root, "A", "B", "x.m4b")) != "payload" {
		t.Fatalf("res %+v, err %v", res, err)
	}
}

// OnDisk spells what exists of a path as the library lists it: on a case- or
// normalization-insensitive filesystem a folder found under another spelling
// takes the listed one; what doesn't exist keeps the path's.
func TestOnDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mara quill", "Orchard Cycle"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	_, statErr := os.Stat(filepath.Join(dir, "MARA QUILL"))
	insensitive := statErr == nil
	for rel, want := range map[string]string{
		"Mara Quill/Orchard Cycle/OC02/x.m4b": "mara quill/Orchard Cycle/OC02/x.m4b",
		"mara quill/orchard cycle/x.m4b":      "mara quill/Orchard Cycle/x.m4b",
		"New Author/Book/x.m4b":               "New Author/Book/x.m4b",
		"x.m4b":                               "x.m4b",
	} {
		if !insensitive {
			want = rel // a case-sensitive disk: another spelling is another folder
		}
		got, err := OnDisk(RootFS(root), rel)
		if err != nil || got != want {
			t.Errorf("OnDisk(%q) = %q, %v; want %q (case-insensitive disk: %v)", rel, got, err, want, insensitive)
		}
	}
	// A file where a folder should be is an error, not a spelling.
	writeFile(t, filepath.Join(dir, "Loose"), "x")
	if _, err := OnDisk(RootFS(root), "Loose/Book/x.m4b"); err == nil {
		t.Error("a file where a folder goes was accepted")
	}
}

func TestPlaceLinksFinishedSourceAndConfirmsBeforePublication(t *testing.T) {
	for _, fast := range []bool{true, false} {
		for _, allowed := range []bool{true, false} {
			root := t.TempDir()
			src := source(t, "complete audio")
			checks := 0
			refused := errors.New("lease not held")
			req := PlaceRequest{Root: root, RelPath: "Author/book.m4b", SrcPath: src, LinkSource: fast,
				BeforeInstall: func(context.Context) error {
					checks++
					if _, err := os.Lstat(filepath.Join(root, "Author/book.m4b")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("publication preceded lease confirmation")
					}
					if !allowed {
						return refused
					}
					return nil
				}}
			_, err := NewLocal().Place(context.Background(), req)
			dst, statErr := os.Stat(filepath.Join(root, "Author/book.m4b"))
			if checks != 1 || !allowed && (!errors.Is(err, refused) || !errors.Is(statErr, os.ErrNotExist)) {
				t.Fatalf("allowed=%t checks=%d result=%v stat=%v", allowed, checks, err, statErr)
			}
			if allowed {
				original, _ := os.Stat(src)
				if err != nil || statErr != nil || (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && os.SameFile(original, dst) != fast {
					t.Fatalf("fast=%t result=%v stat=%v", fast, err, statErr)
				}
			}
			noTemps(t, root)
		}
	}
}

func TestSweepStagedOnlyRemovesOldPlaceFiles(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	const name = ".audiosilo-import-0123456789abcdef.part"
	now := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	cutoff := now.Add(-7 * 24 * time.Hour)
	for _, dir := range []string{"Old", "Young", ".private"} {
		p := filepath.Join(root, dir, name)
		writeFile(t, p, "audio")
		at := cutoff
		if dir == "Young" {
			at = cutoff.Add(time.Second)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	lookalike := filepath.Join(root, ".audiosilo-import-not-ours.part")
	writeFile(t, lookalike, "mine")
	if err := os.Chtimes(lookalike, cutoff, cutoff); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(outside, name)
	writeFile(t, external, "outside")
	if err := os.Chtimes(external, cutoff, cutoff); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(root, "alias")); err != nil {
			t.Fatal(err)
		}
	}
	if err := SweepStaged(context.Background(), root, cutoff, func() bool { return false }); !errors.Is(err, context.Canceled) {
		t.Fatal("sweep did not stop on remap", err)
	}
	if _, err := os.Stat(filepath.Join(root, "Old", name)); err != nil {
		t.Fatal("sweep removed a file after a remap", err)
	}
	if err := SweepStaged(context.Background(), root, cutoff, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Old", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old copy survived", err)
	}
	for _, p := range []string{filepath.Join(root, "Young", name), filepath.Join(root, ".private", name), lookalike, external} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal("unrelated file removed", p, err)
		}
	}
}

func TestPlaceCopiesFinishedSourceAcrossDevices(t *testing.T) {
	root := t.TempDir()
	for _, base := range []string{".", "/dev/shm"} {
		dir, err := os.MkdirTemp(base, ".transfer-test-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		src := filepath.Join(dir, "source.m4b")
		writeFile(t, src, "audio")
		probe := filepath.Join(root, "probe")
		err = os.Link(src, probe)
		if !errors.Is(err, syscall.EXDEV) {
			_ = os.Remove(probe)
			continue
		}
		res, err := NewLocal().Place(context.Background(), PlaceRequest{Root: root, RelPath: "book.m4b", SrcPath: src, LinkSource: true})
		if err != nil || res.Size != 5 || read(t, filepath.Join(root, "book.m4b")) != "audio" || read(t, src) != "audio" {
			t.Fatal("cross-device install", res, err)
		}
		noTemps(t, root)
		return
	}
	t.Skip("no second writable filesystem")
}
