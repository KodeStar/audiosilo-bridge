package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileAtomicCreatesParentAndWrites(t *testing.T) {
	dir := t.TempDir()
	// A path two levels deep to prove the parent tree is created.
	path := filepath.Join(dir, "sub", "nested", "file.json")
	data := []byte("hello\n")
	if err := WriteFileAtomic(path, data, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content = %q, want %q", got, data)
	}
	// No stray temp file left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1 (no temp leftover)", len(entries))
	}
}

func TestWriteFileAtomicAppliesPerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestWriteFileAtomicOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := WriteFileAtomic(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out", "nested", "dst.txt")
	if err := CopyFile(src, dst, 0o444); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "payload\n" {
		t.Errorf("content = %q, want %q", got, "payload\n")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(dst)
		if perm := info.Mode().Perm(); perm != 0o444 {
			t.Errorf("perm = %o, want 444", perm)
		}
	}
	if err := CopyFile(filepath.Join(dir, "missing"), dst, 0o644); err == nil {
		t.Error("CopyFile(missing src) = nil, want error")
	}
}

func TestWithin(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "a", "b")
	cases := []struct {
		root, path string
		want       bool
	}{
		{root, root, true},                                                 // root itself is contained
		{root, filepath.Join(root, "c"), true},                             // direct child
		{root, filepath.Join(root, "c", "d"), true},                        // nested descendant
		{root, filepath.Join(string(filepath.Separator), "a"), false},      // parent escapes
		{root, filepath.Join(string(filepath.Separator), "a", "x"), false}, // sibling escapes
		{root, filepath.Join(root, "..", "x"), false},                      // traversal escapes
	}
	for _, tc := range cases {
		if got := Within(tc.root, tc.path); got != tc.want {
			t.Errorf("Within(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}

func TestIsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsFile(file) {
		t.Error("IsFile(regular file) = false, want true")
	}
	if IsFile(dir) {
		t.Error("IsFile(dir) = true, want false")
	}
	if IsFile(filepath.Join(dir, "missing")) {
		t.Error("IsFile(missing) = true, want false")
	}
}

func TestFileSHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, sum, err := FileSHA256(p)
	if err != nil || n != 3 || sum != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("FileSHA256 = %d %s %v", n, sum, err)
	}
	if _, _, err := FileSHA256(p + "x"); err == nil {
		t.Fatal("a missing file hashed")
	}
}

func TestWriteRootAtomicWritesAndHonoursBeforeRename(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "show"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := WriteRootAtomic(root, "show", "a.json", []byte("one"), 0o644, nil); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused")
	if err := WriteRootAtomic(root, "show", "a.json", []byte("two"), 0o644, func() error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the beforeRename error", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "show", "a.json"))
	if err != nil || string(got) != "one" {
		t.Fatalf("destination = %q, %v; want the first write kept", got, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "show"))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(filepath.Join(dir, "show", "a.json")); info.Mode().Perm() != 0o644 {
			t.Fatalf("mode = %v", info.Mode().Perm())
		}
	}
}
