package audibleacct

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
		e.m.RetryRemoval(ctx, func() Server { return e.srv })
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
	if _, err := e.db.NextAudibleRemoval(ctx, e.srv.PairingID(), now.AddDate(1, 0, 0)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("acknowledged removal still queued: %v", err)
	}
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
			e.m.RetryRemoval(ctx, func() Server { return e.srv })
			e.m.RetryRemoval(ctx, func() Server { return e.srv })
			if len(e.srv.deleted) != 2 || strings.Contains(e.logs.String(), "server refused account removal") != tc.refused {
				t.Fatalf("DELETE calls: %v, logs: %s", e.srv.deleted, e.logs)
			}
			if _, err := e.db.NextAudibleRemoval(ctx, e.srv.PairingID(), e.m.o.Now().AddDate(1, 0, 0)); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("pending after terminal response: %v", err)
			}
		})
	}
}

func TestReaddAfterLostAnswerSettlesOnlyItsOwnRemoval(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	a := e.signIn(t)
	e.srv.delErr = errors.New("lost response")
	if _, err := e.m.Remove(ctx, e.srv, a.ID); err != nil {
		t.Fatal(err)
	}

	// Neither another Amazon customer nor another store claims this removal.
	e.svc.customer = "other"
	other := e.signIn(t)
	e.svc.customer = stubCust
	st, err := e.m.Start("uk")
	if err != nil {
		t.Fatal(err)
	}
	uk, err := e.m.Finish(ctx, e.srv, Finish{Login: st.Login,
		RedirectURL: "https://www.amazon.co.uk/ap/maplanding?openid.oa2.authorization_code=" + stubCode, Mapping: mapping()})
	if err != nil || len(e.srv.deleted) != 1 {
		t.Fatalf("other marketplace: %v, DELETEs %v", err, e.srv.deleted)
	}

	// A later 429 cannot establish what happened to the first DELETE.
	e.srv.delErr = &link.APIError{Status: http.StatusTooManyRequests}
	if _, err := e.trySignIn(); err == nil {
		t.Fatal("reused an id that a delayed DELETE can still reach")
	}
	r, err := e.db.RemovedAudibleCustomer(ctx, e.srv.PairingID(), a.CustomerHash, a.Marketplace)
	if err != nil || !r.Uncertain {
		t.Fatalf("lost the earlier unanswered removal: %+v, %v", r, err)
	}

	e.srv.delErr = nil
	again := e.signIn(t)
	if again.ID == a.ID || len(e.srv.deleted) != 3 || e.srv.deleted[2] != a.ID {
		t.Fatalf("re-add %s, previous %s, DELETEs %v", again.ID, a.ID, e.srv.deleted)
	}
	e.m.o.Now = func() time.Time { return time.Now().AddDate(1, 0, 0) }
	if _, err := e.db.NextAudibleRemoval(ctx, e.srv.PairingID(), e.m.o.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("re-add left its removal queued: %v", err)
	}
	e.m.RetryRemoval(ctx, func() Server { return e.srv })
	if len(e.srv.deleted) != 3 || len(e.srv.put) != 3 {
		t.Fatalf("after retry: DELETEs %v, server accounts %v", e.srv.deleted, e.srv.put)
	}
	for _, id := range []string{again.ID, other.ID, uk.ID} {
		if _, ok := e.srv.put[id]; !ok {
			t.Errorf("server lost %s", id)
		}
	}
}

// A connection refusal never delivered the DELETE. Re-adding the account
// cancels the pending request with the local commit, even across a restart.
func TestReaddCancelsUndeliveredRemoval(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	a := e.signIn(t)
	ts := httptest.NewServer(http.NotFoundHandler())
	ts.Close()
	cl, err := link.New(ts.URL, "token", "test")
	if err != nil {
		t.Fatal(err)
	}
	cl = cl.WithPairing(e.srv.PairingID()).WithServerID(e.srv.ServerID())
	if warning, err := e.m.Remove(ctx, cl, a.ID); err != nil || !strings.Contains(warning, "try again") {
		t.Fatalf("unreachable server: %q, %v", warning, err)
	}
	r, err := e.db.RemovedAudibleCustomer(ctx, cl.PairingID(), a.CustomerHash, a.Marketplace)
	if err != nil || r.Uncertain || r.Attempts != 1 {
		t.Fatalf("removal after a dial failure: %+v, %v", r, err)
	}
	e.reopen(t)
	e.m.o.Now = func() time.Time { return time.Now().AddDate(1, 0, 0) }
	reached, release := e.holdPut(t)
	result := make(chan error, 1)
	var again store.AudibleAccount
	go func() {
		var err error
		again, err = e.trySignIn()
		result <- err
	}()
	reached("re-add")
	// This attempt is due while the server PUT is in flight.
	retried := make(chan struct{})
	go func() {
		e.m.RetryRemoval(ctx, func() Server { return e.srv })
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
	if again.ID != a.ID {
		t.Fatalf("re-add created a second server row: %s and %s", a.ID, again.ID)
	}
	e.m.RetryRemoval(ctx, func() Server { return e.srv })
	if _, err := e.db.NextAudibleRemoval(ctx, cl.PairingID(), e.m.o.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("re-add left a queued removal: %v", err)
	}
	if len(e.srv.deleted) != 0 || len(e.srv.put) != 1 {
		t.Fatalf("after re-add: DELETEs %v, server accounts %v", e.srv.deleted, e.srv.put)
	}
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
	if _, err := e.db.NextAudibleRemoval(ctx, e.srv.PairingID(), time.Now().AddDate(1, 0, 0)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("queued a DELETE on another server: %v", err)
	}
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
