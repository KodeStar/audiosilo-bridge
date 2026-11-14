package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/librivoxjob"
	"github.com/kodestar/audiosilo-bridge/internal/store"
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
	// Two old states of placed books (only the state goes): the one whose job
	// the runner still holds (a running row) is named to the sweep and stays.
	held, gone := filepath.Join(dir, "3-192.json"), filepath.Join(dir, "3-7.json")
	for _, p := range []string{held, gone} {
		if err := os.WriteFile(p, []byte(`{"library_id":3,"parent":"","dest":"Book","placed":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, at, at)
	}
	ctx := context.Background()
	if err := b.db.RecordClaim(ctx, store.ServerJob{ID: 41, Kind: librivoxjob.Kind, Attempt: 1,
		Params: []byte(`{"librivox_id":192,"work_id":"w","recording_id":"r","library_id":3}`)}); err != nil {
		t.Fatal(err)
	}
	b.sweepLibrivox(ctx)
	if _, err := os.Stat(old); err == nil {
		t.Fatal("the sweep left an old file in <data>/librivox")
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatal("the sweep took the state of a fetch whose job is still open")
	}
	if _, err := os.Stat(gone); err == nil {
		t.Fatal("the sweep left an abandoned fetch's state")
	}
}
