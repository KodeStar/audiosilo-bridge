package execfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const script = "#!/bin/sh\nexit 0\n"

// runUnderForks has several goroutines each write a tool with write and run it
// at once, several times over, so their forks overlap the others' writes (the
// shape that failed with "text file busy" on Linux before the fork lock).
func runUnderForks(t *testing.T, write func(p string) (string, error)) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			p := filepath.Join(dir, "tool"+string(rune('a'+i)))
			for range 5 {
				got, err := write(p)
				if err != nil {
					t.Error(err)
					return
				}
				if err := exec.Command(got).Run(); err != nil { //nolint:gosec // the test's own tool
					t.Error(err)
					return
				}
				_ = os.Remove(got)
			}
		})
	}
	wg.Wait()
}

func TestWriteFileThenRunUnderParallelForks(t *testing.T) {
	runUnderForks(t, func(p string) (string, error) {
		return p, WriteFile(p, []byte(script), 0o755)
	})
}

func TestCreateTempThenRunUnderParallelForks(t *testing.T) {
	runUnderForks(t, func(p string) (string, error) {
		name, n, err := CreateTemp(filepath.Dir(p), filepath.Base(p)+"-", strings.NewReader(script))
		if err != nil {
			return "", err
		}
		if n != int64(len(script)) {
			t.Errorf("CreateTemp copied %d bytes, want %d", n, len(script))
		}
		return name, os.Chmod(name, 0o755) //nolint:gosec // the test's own tool
	})
}

// A failed copy still hands back the made file's name so the caller can remove it.
func TestCreateTempReturnsTheNameOnACopyError(t *testing.T) {
	dir := t.TempDir()
	name, _, err := CreateTemp(dir, "x-", errReader{})
	if err == nil {
		t.Fatal("CreateTemp succeeded with a failing reader")
	}
	if name == "" || filepath.Dir(name) != dir {
		t.Fatalf("name = %q, want a file in %s", name, dir)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, os.ErrClosed }
