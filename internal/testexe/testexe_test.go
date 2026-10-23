package testexe

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// Many writers and runners at once never see "text file busy".
func TestWriteFileThenRunUnderParallelForks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := filepath.Join(dir, "tool"+string(rune('a'+i)))
			for range 5 {
				if err := WriteFile(p, []byte("#!/bin/sh\nexit 0\n")); err != nil {
					t.Error(err)
					return
				}
				if err := exec.Command(p).Run(); err != nil {
					t.Error(err)
					return
				}
				_ = os.Remove(p)
			}
		}()
	}
	wg.Wait()
}
