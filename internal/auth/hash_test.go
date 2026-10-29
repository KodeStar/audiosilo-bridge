package auth

import (
	"errors"
	"testing"
	"time"
)

// countingReleaser is a memoryReleaser whose release counts on a channel.
func countingReleaser(delay time.Duration) (*memoryReleaser, chan struct{}) {
	freed := make(chan struct{}, 16)
	return &memoryReleaser{delay: delay, free: func() { freed <- struct{}{} }}, freed
}

// A burst of hashes is released once, after it goes quiet; a later hash again.
func TestMemoryReleaserCoalescesBurst(t *testing.T) {
	r, freed := countingReleaser(50 * time.Millisecond)
	for range 5 {
		r.release()
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-freed:
	case <-time.After(5 * time.Second):
		t.Fatal("memory never released after the burst")
	}
	select {
	case <-freed:
		t.Fatal("burst released more than once")
	case <-time.After(150 * time.Millisecond):
	}
	r.release()
	select {
	case <-freed:
	case <-time.After(5 * time.Second):
		t.Fatal("a hash after the release was not released again")
	}
}

// Nothing is released while hashes keep coming inside the delay.
func TestMemoryReleaserWaitsForQuiet(t *testing.T) {
	r, freed := countingReleaser(200 * time.Millisecond)
	for range 6 {
		r.release()
		time.Sleep(50 * time.Millisecond)
		select {
		case <-freed:
			t.Fatal("released while hashes were still coming")
		default:
		}
	}
	select {
	case <-freed:
	case <-time.After(5 * time.Second):
		t.Fatal("memory never released")
	}
}

// swapReleaser points hashMemory at a counting releaser for the test. The test
// must not be parallel: hashMemory is package state.
func swapReleaser(t *testing.T) func(what string) {
	t.Helper()
	r, freed := countingReleaser(time.Millisecond)
	prev := hashMemory
	hashMemory = r
	t.Cleanup(func() { hashMemory = prev })
	return func(what string) {
		t.Helper()
		select {
		case <-freed:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not release its memory", what)
		}
	}
}

// Every hash and verify schedules the release: HashPassword, VerifyPassword with
// the right and the wrong password.
func TestHashAndVerifyReleaseMemory(t *testing.T) {
	wait := swapReleaser(t)
	h, err := HashPassword("pw")
	if err != nil {
		t.Fatal(err)
	}
	wait("HashPassword")
	if ok, _ := VerifyPassword("pw", h); !ok {
		t.Fatal("verify failed")
	}
	wait("VerifyPassword")
	if ok, _ := VerifyPassword("nope", h); ok {
		t.Fatal("wrong password verified")
	}
	wait("a failed VerifyPassword")
}

// The page's two hashing paths release too: the first start's generated password
// (EnsureAdmin) and a login, refused or allowed.
func TestFirstStartAndLoginReleaseMemory(t *testing.T) {
	wait := swapReleaser(t)
	m := New(NewMemStore())
	pw, err := m.EnsureAdmin()
	if err != nil || pw == "" {
		t.Fatalf("EnsureAdmin = %q, %v", pw, err)
	}
	wait("the first start")
	if _, err := m.Login("nope"); !errors.Is(err, ErrInvalidCreds) {
		t.Fatalf("wrong password err = %v, want ErrInvalidCreds", err)
	}
	wait("a refused login")
	if _, err := m.Login(pw); err != nil {
		t.Fatalf("Login: %v", err)
	}
	wait("a login")
}
