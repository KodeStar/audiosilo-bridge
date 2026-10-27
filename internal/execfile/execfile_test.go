package execfile

import (
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/execfile/forktest"
)

// A tool WriteFile wrote runs at once while other goroutines fork, never
// "text file busy" (bridge#12).
func TestWriteFileThenRunUnderParallelForks(t *testing.T) {
	forktest.Run(t, func(p string) (string, error) {
		return p, WriteFile(p, []byte(forktest.Script), 0o755)
	}, forktest.Exec)
}
