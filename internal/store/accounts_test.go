package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAccounts(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if _, err := db.GetAccount(ctx, "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// No row: the updates record nothing and don't fail.
	if err := db.RefuseAccount(ctx, "github", "x", time.Now()); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if err := db.SignInAccount(ctx, "github", "octocat", at); err != nil {
		t.Fatal(err)
	}
	if err := db.RefuseAccount(ctx, "github", "GitHub refused the sign-in", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	a, err := db.GetAccount(ctx, "github")
	if err != nil {
		t.Fatal(err)
	}
	if a.Login != "octocat" || a.SignedInAt != Timestamp(at) || a.RefusedAt != Timestamp(at.Add(time.Hour)) ||
		a.LastError != "GitHub refused the sign-in" {
		t.Fatalf("account = %+v", a)
	}
	// A new sign-in forgets the refusal.
	if err := db.SignInAccount(ctx, "github", "hubot", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	a, _ = db.GetAccount(ctx, "github")
	if a.Login != "hubot" || a.RefusedAt != "" || a.LastError != "" || a.LastOKAt != Timestamp(at.Add(2*time.Hour)) {
		t.Fatalf("after sign-in: %+v", a)
	}
	if err := db.SetAccountError(ctx, "github", "couldn't reach GitHub"); err != nil {
		t.Fatal(err)
	}
	if err := db.AccountOK(ctx, "github", at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	a, _ = db.GetAccount(ctx, "github")
	if a.LastError != "" || a.LastOKAt != Timestamp(at.Add(3*time.Hour)) {
		t.Fatalf("after ok: %+v", a)
	}
	if err := db.DeleteAccount(ctx, "github"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAccount(ctx, "github"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
