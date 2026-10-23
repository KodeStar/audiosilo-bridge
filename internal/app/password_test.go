package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/auth"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

func TestResetPasswordKeepsPairingAndSecrets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	am := auth.New(db.AuthStore())
	old, err := am.EnsureAdmin()
	if err != nil {
		t.Fatal(err)
	}
	session, err := am.Login(old)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{keyServerURL: "https://books.example.com", keyServerID: "srv-1", keyPairedAt: "2026-10-20T10:00:00Z"} {
		if err := db.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	vault, _, err := secrets.Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(secrets.ServerToken, "asb_secret"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	fresh, err := ResetPassword(ctx, dir)
	if err != nil || fresh == "" || fresh == old {
		t.Fatalf("ResetPassword = %q, %v", fresh, err)
	}

	db, err = store.Open(ctx, filepath.Join(dir, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	am = auth.New(db.AuthStore())
	if _, err := am.Login(old); !errors.Is(err, auth.ErrInvalidCreds) {
		t.Errorf("the old password after a reset: %v", err)
	}
	if ok, _ := am.Resolve(session); ok {
		t.Error("a session from before the reset still resolves")
	}
	if _, err := am.Login(fresh); err != nil {
		t.Errorf("the new password: %v", err)
	}
	if v, _, _ := db.GetSetting(ctx, keyServerURL); v != "https://books.example.com" {
		t.Errorf("pairing server URL = %q after a reset", v)
	}
	if v, _, _ := db.GetSetting(ctx, keyServerID); v != "srv-1" {
		t.Errorf("pairing server id = %q after a reset", v)
	}
	vault, _, err = secrets.Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := vault.Get(secrets.ServerToken); err != nil || tok != "asb_secret" {
		t.Errorf("server token after a reset = %q, %v", tok, err)
	}
}

func TestResetPasswordRefusesAFolderWithNoBridge(t *testing.T) {
	dir := t.TempDir()
	_, err := ResetPassword(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "no Bridge data") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, store.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused reset created a database")
	}
}

func TestOwnerRefusal(t *testing.T) {
	// Denied: root in another user's folder.
	if err := ownerRefusal(0, 1000, 1000, "/bridge-data"); err == nil || !strings.Contains(err.Error(), "docker exec -u 1000:1000") {
		t.Errorf("root in a 1000:1000 folder: %v", err)
	}
	// Allowed: the owner itself, any non-root caller (the open fails on its own
	// if it can't write), and root in root's folder.
	for _, c := range [][3]int{{1000, 1000, 1000}, {501, 1000, 1000}, {0, 0, 0}} {
		if err := ownerRefusal(c[0], c[1], c[2], "/d"); err != nil {
			t.Errorf("ownerRefusal%v = %v", c, err)
		}
	}
}
