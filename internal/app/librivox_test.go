package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/librivoxjob"
)

// The Bridge keeps librivox.fetch's handler for its sweep, over
// <data>/librivox: what an abandoned fetch left there goes once old.
func TestSweepLibrivox(t *testing.T) {
	b := newTestBridge(t)
	if b.librivox == nil {
		t.Fatal("no librivox handler")
	}
	dir := filepath.Join(b.dataDir, "librivox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, ".3-192.json.tmp-1")
	if err := os.WriteFile(old, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-librivoxjob.StagingMaxAge - time.Hour)
	_ = os.Chtimes(old, at, at)
	b.sweepLibrivox()
	if _, err := os.Stat(old); err == nil {
		t.Fatal("the sweep left an old file in <data>/librivox")
	}
}
