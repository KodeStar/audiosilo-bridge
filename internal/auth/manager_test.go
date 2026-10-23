package auth

import (
	"errors"
	"testing"
	"time"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	enc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword("correct horse battery staple", enc)
	if err != nil || !ok {
		t.Fatalf("verify correct: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong", enc)
	if err != nil {
		t.Fatalf("verify wrong err: %v", err)
	}
	if ok {
		t.Fatal("verify wrong: ok=true, want false")
	}
}

func TestEnsureAdminGeneratesOnceOnly(t *testing.T) {
	m := New(NewMemStore())
	pw, err := m.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	if pw == "" {
		t.Fatal("expected a one-time password on first run")
	}
	// Second call must NOT regenerate or reprint.
	pw2, err := m.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin 2: %v", err)
	}
	if pw2 != "" {
		t.Errorf("second EnsureAdmin returned a password %q, want empty", pw2)
	}
}

func TestLoginAllowedAndDenied(t *testing.T) {
	m := New(NewMemStore())
	pw, err := m.EnsureAdmin()
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}

	// Denied: wrong password.
	if _, err := m.Login("nope"); !errors.Is(err, ErrInvalidCreds) {
		t.Errorf("wrong password err = %v, want ErrInvalidCreds", err)
	}

	// Allowed: correct password mints a token that resolves.
	tok, err := m.Login(pw)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	ok, err := m.Resolve(tok)
	if err != nil || !ok {
		t.Fatalf("Resolve minted token: ok=%v err=%v", ok, err)
	}

	// Denied: a bogus token does not resolve.
	if ok, _ := m.Resolve("garbage"); ok {
		t.Error("bogus token resolved")
	}
	if ok, _ := m.Resolve(""); ok {
		t.Error("empty token resolved")
	}
}

func TestLogoutRevokes(t *testing.T) {
	m := New(NewMemStore())
	pw, _ := m.EnsureAdmin()
	tok, err := m.Login(pw)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := m.Logout(tok); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if ok, _ := m.Resolve(tok); ok {
		t.Error("token still resolves after logout")
	}
}

func TestChangePassword(t *testing.T) {
	m := New(NewMemStore())
	pw, _ := m.EnsureAdmin()
	mine, _ := m.Login(pw)
	other, _ := m.Login(pw)

	// Denied: wrong current password.
	if err := m.ChangePassword("wrong", "a-new-strong-pass", mine); !errors.Is(err, ErrInvalidCreds) {
		t.Errorf("wrong current err = %v, want ErrInvalidCreds", err)
	}
	// Denied: new password too short.
	if err := m.ChangePassword(pw, "short", mine); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("short new err = %v, want ErrPasswordTooShort", err)
	}
	// Allowed.
	if ok, _ := m.Resolve(other); !ok {
		t.Fatal("a refused change signed a session out")
	}
	if err := m.ChangePassword(pw, "a-new-strong-pass", mine); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	// The caller stays signed in; every other session is signed out.
	if ok, _ := m.Resolve(mine); !ok {
		t.Error("the caller's session was signed out")
	}
	if ok, _ := m.Resolve(other); ok {
		t.Error("another session survived a password change")
	}
	// Old password no longer works; new one does.
	if _, err := m.Login(pw); !errors.Is(err, ErrInvalidCreds) {
		t.Errorf("old password still works: %v", err)
	}
	if _, err := m.Login("a-new-strong-pass"); err != nil {
		t.Errorf("new password login: %v", err)
	}
}

// Reopen-persistence across a real Close/Open cycle of the file-backed store is
// covered by store.TestAuthStoreReopenPersists (internal/store); the auth package
// itself is storage-agnostic, so its tests use the in-memory MemStore.

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestRateLimiterAllowsBurstThenDenies(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	rl := NewRateLimiter(3, 1) // burst 3, 1 token/sec
	rl.now = c.now

	for i := 0; i < 3; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("attempt %d denied within burst", i)
		}
	}
	// Burst exhausted.
	if rl.Allow("1.2.3.4") {
		t.Fatal("4th attempt allowed, want denied")
	}
	// A different key has its own bucket.
	if !rl.Allow("5.6.7.8") {
		t.Fatal("different key denied")
	}
	// After 1 second, one token refills.
	c.t = c.t.Add(time.Second)
	if !rl.Allow("1.2.3.4") {
		t.Fatal("attempt after refill denied")
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("second attempt after single refill allowed")
	}
}

func TestResetPasswordReplacesItAndSignsEveryoneOut(t *testing.T) {
	m := New(NewMemStore())
	old, err := m.EnsureAdmin()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := m.Login(old)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := m.ResetPassword()
	if err != nil || fresh == "" || fresh == old {
		t.Fatalf("ResetPassword = %q, %v", fresh, err)
	}
	// Denied: the old password and the old session no longer work.
	if _, err := m.Login(old); !errors.Is(err, ErrInvalidCreds) {
		t.Errorf("old password after reset: %v, want ErrInvalidCreds", err)
	}
	if ok, _ := m.Resolve(tok); ok {
		t.Error("a session from before the reset still resolves")
	}
	// Allowed: the new one signs in, and the first-run path doesn't print another.
	if _, err := m.Login(fresh); err != nil {
		t.Errorf("new password: %v", err)
	}
	if pw, _ := m.EnsureAdmin(); pw != "" {
		t.Errorf("EnsureAdmin after a reset printed %q", pw)
	}
}

func TestResetPasswordProvisionsWhenNoneWasSet(t *testing.T) {
	m := New(NewMemStore())
	pw, err := m.ResetPassword()
	if err != nil || pw == "" {
		t.Fatalf("ResetPassword on an empty store = %q, %v", pw, err)
	}
	if _, err := m.Login(pw); err != nil {
		t.Errorf("login: %v", err)
	}
}
