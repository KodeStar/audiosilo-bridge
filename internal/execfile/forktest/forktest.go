// Package forktest is the fork-stress test shared by execfile and toolfetch:
// several goroutines each write a tool and run it at once, several times over,
// so their forks overlap the others' writes (the shape that failed with "text
// file busy" on Linux, bridge#12). For tests only; it imports nothing of the
// Bridge's, so any package's tests can use it.
package forktest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// Script is a tool that runs and exits 0.
const Script = "#!/bin/sh\nexit 0\n"

// Exec runs the tool at p, the way a first run without retries does.
func Exec(p string) error {
	return exec.Command(p).Run() //nolint:gosec // the test's own tool
}

// Run has 4 goroutines each write a tool with write (given the path to use,
// returning the path it wrote) and run it with run, 5 times over, removing it
// after each run. It returns the directory the tools went in, empty unless a
// write left something behind. Windows skips: the tools are shell scripts.
func Run(t *testing.T, write func(p string) (string, error), run func(p string) error) string {
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
				if err := run(got); err != nil {
					t.Error(err)
					return
				}
				_ = os.Remove(got)
			}
		})
	}
	wg.Wait()
	return dir
}
