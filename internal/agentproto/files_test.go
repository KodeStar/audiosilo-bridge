package agentproto

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// openTestRoot is an os.Root over a fresh dir holding ok.txt ("hello").
func openTestRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

func TestReadRegularAllows(t *testing.T) {
	t.Parallel()
	root, _ := openTestRoot(t)
	b, err := ReadRegular(root, "ok.txt", 5)
	if err != nil || string(b) != "hello" {
		t.Fatalf("ReadRegular = %q, %v", b, err)
	}
	if !Exists(root, "ok.txt") || Exists(root, "missing") {
		t.Fatal("Exists")
	}
}

func TestReadRegularRefusesOversize(t *testing.T) {
	t.Parallel()
	root, _ := openTestRoot(t)
	if _, err := ReadRegular(root, "ok.txt", 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// A link is refused, never followed: one pointing outside the root, and one
// pointing at a regular file inside it too. A dir isn't a regular file, and a
// path can't climb out of the root.
func TestOpenRegularRefusesLinksAndDirs(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out-link")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if err := os.Symlink("ok.txt", filepath.Join(dir, "in-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"out-link", "in-link", "sub"} {
		if b, err := ReadRegular(root, name, 1<<20); !errors.Is(err, ErrNotRegular) {
			t.Errorf("%s: read %q, err = %v, want ErrNotRegular", name, b, err)
		}
	}
	if b, err := ReadRegular(root, "../"+filepath.Base(dir)+"/ok.txt", 1<<20); err == nil {
		t.Errorf("a path out of the root was read: %q", b)
	}
	if !Exists(root, "out-link") {
		t.Error("Exists must see a link (without following it)")
	}
}
