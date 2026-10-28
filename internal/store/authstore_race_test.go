package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// twoProcesses opens the same database file twice, as the running Bridge and a
// reset-password run each do, with "old" stored as the password.
func twoProcesses(t *testing.T) (bridge, reset *DB) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), FileName)
	var err error
	if bridge, err = Open(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	if reset, err = Open(context.Background(), dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reset.Close() })
	return bridge, reset
}

// resetHeld begins the reset's transaction and stores "reset" in it, holding
// SQLite's write lock until commit is called.
func resetHeld(t *testing.T, reset *DB) (commit func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := reset.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := setSetting(ctx, tx, authPasswordKey, "reset"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	return func() {
		if err := tx.Commit(); err != nil {
			t.Error(err)
		}
	}
}

// raceWithReset runs write on the Bridge's side while the reset holds the write
// lock, then commits the reset: write waits for the lock and runs after it.
func raceWithReset(t *testing.T, reset *DB, write func() (bool, error)) (bool, error) {
	t.Helper()
	commit := resetHeld(t, reset)
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := write()
		done <- result{ok, err}
	}()
	time.Sleep(100 * time.Millisecond) // write is waiting for the lock (busy_timeout)
	commit()
	r := <-done
	return r.ok, r.err
}

// bridge#16: a password change verified against "old" while a reset-password
// run in another process stored "reset" never overwrites the reset: the check
// and the write are one statement, so it sees the committed reset. Split into a
// read and a write, the read would see "old" (the reset not committed yet) and
// the write would land after the reset, or fail with a stale snapshot.
func TestAuthStoreReplacePasswordLosesToAResetInAnotherProcess(t *testing.T) {
	bridge, reset := twoProcesses(t)
	as := bridge.AuthStore()
	setPassword(t, as, "old")
	ok, err := raceWithReset(t, reset, func() (bool, error) { return as.ReplacePassword("old", "changed", "") })
	if err != nil || ok {
		t.Fatalf("a change racing the reset = %v, %v; want false, nil", ok, err)
	}
	if h, _ := as.LoadAuth(); h != "reset" {
		t.Fatalf("password = %q, want the reset's", h)
	}
}

// bridge#16: a first start's EnsureAdmin that found no password while a
// reset-password run stored one never overwrites it (create-if-absent in one
// statement). Allowed: with none stored it creates one.
func TestAuthStoreCreatePasswordLosesToAResetInAnotherProcess(t *testing.T) {
	bridge, reset := twoProcesses(t)
	as := bridge.AuthStore()
	ok, err := raceWithReset(t, reset, func() (bool, error) { return as.CreatePassword("first") })
	if err != nil || ok {
		t.Fatalf("a first password racing the reset = %v, %v; want false, nil", ok, err)
	}
	if h, _ := as.LoadAuth(); h != "reset" {
		t.Fatalf("password = %q, want the reset's", h)
	}

	fresh := open(t).AuthStore()
	if ok, err := fresh.CreatePassword("first"); err != nil || !ok {
		t.Fatalf("CreatePassword with none stored = %v, %v", ok, err)
	}
	if h, _ := fresh.LoadAuth(); h != "first" {
		t.Fatalf("password = %q, want first", h)
	}
}

// Security: CreatePassword signs every session out with the first password,
// and with one stored changes nothing (denied: no overwrite, no sign-out).
func TestAuthStoreCreatePasswordOnlyWhenNoneIsSet(t *testing.T) {
	as := open(t).AuthStore()
	if ok, err := as.CreatePassword("first"); err != nil || !ok {
		t.Fatalf("CreatePassword = %v, %v", ok, err)
	}
	if ok, err := as.AddSession("s1", time.Now(), "first"); err != nil || !ok {
		t.Fatalf("AddSession = %v, %v", ok, err)
	}
	if ok, err := as.CreatePassword("second"); err != nil || ok {
		t.Fatalf("CreatePassword with one stored = %v, %v; want false", ok, err)
	}
	if h, _ := as.LoadAuth(); h != "first" {
		t.Fatalf("password = %q, want first", h)
	}
	if ok, _ := as.HasSession("s1"); !ok {
		t.Fatal("a refused create signed a session out")
	}
}
