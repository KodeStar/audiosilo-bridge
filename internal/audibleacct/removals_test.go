package audibleacct

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

func (e *env) reopen(t *testing.T) {
	t.Helper()
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(e.data, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	opts := e.m.o
	opts.DB = db
	e.db, e.m = db, New(opts)
}

// retry is the heartbeat's turn at the pending removals.
func (e *env) retry() { e.m.RetryRemoval(context.Background(), e.srv) }

// noPendingRemoval fails t when any removal is still owed on e's pairing.
func (e *env) noPendingRemoval(t *testing.T) {
	t.Helper()
	if r, err := e.db.NextAudibleRemoval(context.Background(), e.srv.PairingID(), time.Now().AddDate(10, 0, 0)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removal still pending: %+v, %v", r, err)
	}
}

// A temporary failure removes the account and the device once. Both the
// retry time and the retry count survive a restart, including Retry-After.
func TestRemovalBackoffAcrossRestart(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	now := time.Now()
	e.m.o.Now = func() time.Time { return now }
	a := e.signIn(t)
	e.srv.delErr = errors.New("connection refused")
	warning, err := e.m.Remove(ctx, e.srv, a.ID)
	if err != nil || !strings.Contains(warning, "try again") {
		t.Fatalf("remove = %q, %v", warning, err)
	}
	if _, err := e.m.Account(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("local account after removal: %v", err)
	}
	e.reopen(t)
	retry := func(want int) {
		t.Helper()
		e.retry()
		if len(e.srv.deleted) != want {
			t.Fatalf("at %s: DELETE calls = %v, want %d", now, e.srv.deleted, want)
		}
	}
	retry(1)
	now = now.Add(time.Minute)
	e.srv.delErr = &link.APIError{Status: http.StatusServiceUnavailable}
	retry(2)
	now = now.Add(time.Minute)
	retry(2)
	now = now.Add(time.Minute)
	e.srv.delErr = &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: 3 * time.Hour, RetryAfterSet: true}
	retry(3)
	now = now.Add(3*time.Hour - time.Second)
	retry(3)
	now = now.Add(time.Second)
	e.srv.delErr = nil
	retry(4)
	e.noPendingRemoval(t)
	if len(e.svc.deregistered) != 1 {
		t.Fatalf("Amazon deregistrations = %v", e.svc.deregistered)
	}
}

func TestRemovalTerminalAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		refused bool
	}{
		{"missing", &link.APIError{Status: http.StatusNotFound}, false},
		{"unauthorized", link.ErrUnauthorized, true},
		{"forbidden", &link.APIError{Status: http.StatusForbidden}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := t.Context()
			a := e.signIn(t)
			e.srv.delErr = &link.APIError{Status: http.StatusBadGateway}
			if _, err := e.m.Remove(ctx, e.srv, a.ID); err != nil {
				t.Fatal(err)
			}
			e.srv.delErr = tc.err
			e.m.o.Now = func() time.Time { return time.Now().Add(24 * time.Hour) }
			e.retry()
			e.retry()
			if len(e.srv.deleted) != 2 || strings.Contains(e.logs.String(), "server refused account removal") != tc.refused {
				t.Fatalf("DELETE calls: %v, logs: %s", e.srv.deleted, e.logs)
			}
			e.noPendingRemoval(t)
		})
	}
}

// A removal the server never confirmed never holds up a sign-in again of the
// same Amazon account, and its late DELETE can't reach it: the re-add gets a
// fresh id, and the old one's DELETE goes on until the server answers it.
func TestReaddWhileRemovalPendingGetsAFreshID(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	now := time.Now()
	e.m.o.Now = func() time.Time { return now }
	a := e.signIn(t)
	e.srv.delErr = &link.APIError{Status: http.StatusBadGateway}
	if _, err := e.m.Remove(ctx, e.srv, a.ID); err != nil {
		t.Fatal(err)
	}
	// The server takes the mapping but still fails the DELETE.
	again := e.signIn(t)
	if again.ID == a.ID || again.CustomerHash != a.CustomerHash {
		t.Fatalf("re-add = %+v, removed %s", again, a.ID)
	}
	now = now.Add(time.Hour)
	e.retry()
	e.srv.delErr = nil
	now = now.Add(time.Hour)
	e.retry()
	if got := e.srv.deleted; len(got) != 3 || got[0] != a.ID || got[1] != a.ID || got[2] != a.ID {
		t.Fatalf("DELETEs %v, want three of %s", got, a.ID)
	}
	if _, ok := e.srv.put[again.ID]; !ok || len(e.srv.put) != 1 {
		t.Fatalf("server accounts %v, want only %s", e.srv.put, again.ID)
	}
	e.noPendingRemoval(t)
}

// The heartbeat's retry takes no lock: a sign-in waiting on a slow server
// never holds it up.
func TestRetryRemovalDoesNotWaitForASignIn(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	a := e.signIn(t)
	e.srv.delErr = errors.New("lost response")
	if _, err := e.m.Remove(ctx, e.srv, a.ID); err != nil {
		t.Fatal(err)
	}
	e.srv.delErr = nil
	e.m.o.Now = func() time.Time { return time.Now().Add(time.Hour) }
	reached, release := e.holdPut(t)
	result := make(chan error, 1)
	go func() {
		_, err := e.trySignIn()
		result <- err
	}()
	reached("re-add")
	retried := make(chan struct{})
	go func() {
		e.retry()
		close(retried)
	}()
	select {
	case <-retried:
	case <-time.After(10 * time.Second):
		release()
		t.Fatal("the heartbeat waited for the in-flight sign-in")
	}
	release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	e.noPendingRemoval(t)
}

// Only the pairing that recorded a removal sends it: a heartbeat on another
// pairing (an unpair and a pairing since) drops it unsent.
func TestRetryRemovalDropsAnEndedPairingsRemovals(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	a := e.signIn(t)
	e.srv.delErr = errors.New("unreachable")
	if _, err := e.m.Remove(ctx, e.srv, a.ID); err != nil {
		t.Fatal(err)
	}
	e.srv.delErr = nil
	e.srv.pairing = "pairing-2"
	e.m.o.Now = func() time.Time { return time.Now().Add(time.Hour) }
	e.retry()
	if len(e.srv.deleted) != 1 || !strings.Contains(e.logs.String(), "ended pairing") {
		t.Fatalf("DELETEs %v, logs %s", e.srv.deleted, e.logs)
	}
	e.srv.pairing = ""
	e.noPendingRemoval(t)
}

func TestRemovalOnAnotherServerIsLocalOnly(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	a := e.signIn(t)
	e.srv.serverID = "another-server"
	warning, err := e.m.Remove(ctx, e.srv, a.ID)
	if err != nil || !strings.Contains(warning, "isn't paired") || len(e.srv.deleted) != 0 {
		t.Fatalf("remove on other server = %q, %v, DELETEs %v", warning, err, e.srv.deleted)
	}
	if _, err := e.m.Account(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("local account: %v", err)
	}
	e.noPendingRemoval(t)
}

type failingLocalRemoval struct{ Store }

func (failingLocalRemoval) DeleteAudibleAccount(context.Context, string, string) error {
	return errors.New("could not write local removal")
}

func TestLocalRemovalFailureKeepsSignIn(t *testing.T) {
	e := newEnv(t)
	a := e.signIn(t)
	name := secrets.AudibleName(a.ID)
	raw, err := e.vault.Get(name)
	if err != nil || raw == "" {
		t.Fatal("missing fixture sign-in")
	}
	e.m.o.DB = failingLocalRemoval{e.db}
	if _, err := e.m.Remove(t.Context(), e.srv, a.ID); err == nil {
		t.Fatal("local removal unexpectedly succeeded")
	}
	if kept, err := e.vault.Get(name); err != nil || kept != raw {
		t.Fatal("did not restore the sign-in after a local write failure")
	}
	if len(e.srv.deleted) != 0 || len(e.svc.deregistered) != 0 {
		t.Fatal("removed on the server or Amazon after a local failure")
	}
}
