package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audibleacct"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/setup"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

type sweepBook struct{}

func (sweepBook) Book(_ context.Context, id int64, _ string) (*link.Book, error) {
	return &link.Book{LibraryID: id, Path: "Sample.m4b", Files: []link.BookFile{{Path: "Sample.m4b", Size: 1}}}, nil
}

func checkedBooksMount(t *testing.T, b *Bridge) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Sample.m4b"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.libs.SetLibraries([]link.Library{{ID: 1, Root: root}})
	b.libs.Check(t.Context(), sweepBook{}, b.mount, time.Now)
	if st, ok := b.libs.Status(1); !ok || !st.Writable {
		t.Fatalf("library check: %+v", st)
	}
	return root
}

func oldPlaceTemp(t *testing.T, root, folder string) string {
	t.Helper()
	dir := filepath.Join(root, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, ".audiosilo-import-0123456789abcdef.part")
	if err := os.WriteFile(file, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestAudibleSweepNeedsAccountWithoutPlacementRecords(t *testing.T) {
	b, ctx := pairedBridge(t, &setupServer{kind: "new"})
	root := checkedBooksMount(t, b)
	file := oldPlaceTemp(t, root, "Author/Book")
	b.sweepAudible(ctx, b.conn.Client())
	b.audibleSwept.Range(func(_, _ any) bool {
		t.Error("walked the library with no Audible account or record")
		return false
	})
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	a := store.AudibleAccount{ID: "acct0001", LibraryID: 1, ServerID: b.conn.Client().ServerID(), Marketplace: "us"}
	if err := b.db.SignInAudibleAccount(ctx, a, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.sweepAudible(ctx, b.conn.Client())
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old temp in the account's library: %v", err)
	}
}

func TestAudibleSweepUsesExpiredPlacementRecord(t *testing.T) {
	before, ctx := pairedBridge(t, &setupServer{kind: "new"})
	dir := filepath.Join(before.dataDir, "audible", "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	record := filepath.Join(dir, "acct0001-B0FAKE0001.placed.json")
	if err := os.WriteFile(record, []byte(`{"library_id":1,"path":"Author/Book/Book.m4b"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(record, old, old); err != nil {
		t.Fatal(err)
	}
	if err := before.db.Close(); err != nil {
		t.Fatal(err)
	}
	b, _, err := New(ctx, Options{DataDir: before.dataDir, Version: "test", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.db.Close()
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired private record: %v", err)
	}
	root := checkedBooksMount(t, b)
	recorded := oldPlaceTemp(t, root, "Author/Book")
	ancestor := oldPlaceTemp(t, root, "Author")
	descendant := oldPlaceTemp(t, root, "Author/Book/Extras")
	sibling := oldPlaceTemp(t, root, "Author/Other")
	b.sweepAudible(ctx, b.conn.Client())
	if _, err := os.Stat(recorded); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old temp in recorded folder: %v", err)
	}
	for _, keep := range []string{ancestor, descendant, sibling} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("outside the recorded folder: %v", err)
		}
	}
}

// registerErr is a real Register failure against an identity API answering
// status, wrapped as audibleacct.Finish wraps it.
func registerErr(t *testing.T, status int) error {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"response":{"error":{"code":"InvalidValue"}}}`))
	}))
	defer ts.Close()
	mp, _ := audible.MarketplaceFor("us")
	pkce, err := audible.NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	_, err = audible.Register(context.Background(), mp, "code", pkce, "SERIAL",
		audible.WithEndpoints(audible.Endpoints{Amazon: ts.URL, AmazonAPI: ts.URL, API: ts.URL}))
	if err == nil {
		t.Fatalf("a %d registered", status)
	}
	return fmt.Errorf("%w: %w", audibleacct.ErrRegister, err)
}

func TestAudibleSetupSyncHealth(t *testing.T) {
	b := newTestBridge(t)
	ctx := context.Background()
	if got := b.audibleSetup(ctx); got != (setup.Audible{}) {
		t.Fatalf("empty accounts: %+v", got)
	}
	at := time.Now()
	for _, id := range []string{"healthy001", "refused001", "syncfail01", "syncfail02"} {
		a := store.AudibleAccount{ID: id, Marketplace: "us", DeviceProfile: audible.ProfileVersion}
		if err := b.db.SignInAudibleAccount(ctx, a, at); err != nil {
			t.Fatal(err)
		}
		// syncfail02's push failed, not its job: it counts the same.
		side := store.SyncJob
		if id == "syncfail02" {
			side = store.SyncPush
		}
		if id != "healthy001" {
			if err := b.db.AudibleSyncFailed(ctx, id, side, "sync failed", at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := b.db.RefuseAudibleAccount(ctx, "refused001", store.RefusalDevice, "sign in again", at); err != nil {
		t.Fatal(err)
	}
	if got := b.audibleSetup(ctx); got.Accounts != 4 || got.Refused != 1 || got.OldProfile != 0 ||
		got.SyncFailing != 2 || got.SyncError != "sync failed" {
		t.Fatalf("setup with sync failures: %+v", got)
	}
	for _, id := range []string{"refused001", "syncfail01", "syncfail02"} {
		a, err := b.db.GetAudibleAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.db.SignInAudibleAccount(ctx, a, at.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if got := b.audibleSetup(ctx); got.Accounts != 4 || got.Refused != 0 || got.SyncFailing != 0 || got.SyncError != "" {
		t.Fatalf("setup after signing in again: %+v", got)
	}
	// Paired with srv-1: an account mapped on another server isn't counted,
	// nor its refusal or sync failure (bridge#40).
	b.conn.mu.Lock()
	b.conn.pairing.ServerID = "srv-1"
	b.conn.mu.Unlock()
	for _, id := range []string{"healthy001", "refused001", "syncfail01"} {
		a, err := b.db.GetAudibleAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		a.ServerID = "srv-1"
		if err := b.db.MapAudibleAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	other, err := b.db.GetAudibleAccount(ctx, "syncfail02")
	if err != nil {
		t.Fatal(err)
	}
	other.ServerID = "srv-2"
	if err := b.db.MapAudibleAccount(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := b.db.RefuseAudibleAccount(ctx, "syncfail02", store.RefusalDevice, "sign in again", at); err != nil {
		t.Fatal(err)
	}
	if got := b.audibleSetup(ctx); got.Accounts != 3 || got.Refused != 0 || got.OtherServer != 1 {
		t.Fatalf("setup with an account on another server: %+v", got)
	}
}

// A refusal (Amazon's or the server's 4xx) is a 422 with its answer; Amazon or
// the server failing or not answering is a 502 with a fixed sentence, never the
// transport's text.
func TestAudibleErrorKinds(t *testing.T) {
	t.Parallel()
	transport := &url.Error{Op: "Post", URL: "https://api.amazon.com/auth/register", Err: errors.New("dial tcp: connection refused")}
	cases := []struct {
		name string
		err  error
		kind error
		want string
	}{
		{"amazon refuses the code", registerErr(t, http.StatusBadRequest), api.ErrRefused, "wasn't accepted"},
		{"amazon fails", registerErr(t, http.StatusInternalServerError), api.ErrUnreachable, "Amazon couldn't take the sign-in"},
		{"amazon unreachable", fmt.Errorf("%w: %w", audibleacct.ErrRegister, transport), api.ErrUnreachable, "Amazon couldn't take the sign-in"},
		{"server refuses", &link.APIError{Status: 404, Code: "unknown_user", Message: "no such user"}, api.ErrRefused, "the server refused the account: no such user"},
		{"server fails", &link.APIError{Status: 503, Message: "Service Unavailable"}, api.ErrUnreachable, "HTTP 503"},
		{"server unreachable", &url.Error{Op: "Put", URL: "http://srv/api", Err: errors.New("EOF")}, api.ErrUnreachable, "couldn't be reached"},
		{"bridge token refused", link.ErrUnauthorized, api.ErrRefused, ""},
		{"expired", audibleacct.ErrLoginExpired, api.ErrNotFound, "expired"},
		{"not paired", audibleacct.ErrNotPaired, api.ErrConflict, "pair"},
	}
	for _, c := range cases {
		got := audibleError(c.err)
		if !errors.Is(got, c.kind) || !strings.Contains(got.Error(), c.want) {
			t.Errorf("%s: %v, want %v holding %q", c.name, got, c.kind, c.want)
		}
		if c.kind == api.ErrUnreachable && (strings.Contains(got.Error(), "dial tcp") || strings.Contains(got.Error(), "EOF")) {
			t.Errorf("%s: the transport's text reached the page: %v", c.name, got)
		}
	}
	if audibleError(nil) != nil {
		t.Fatal("nil is an error")
	}
	local := errors.New("disk full")
	if got := audibleError(local); got != local {
		t.Fatalf("a local failure = %v, want it as it is (a logged 500)", got)
	}
}

// A heartbeat the server answered sends a pending Audible removal, only on
// the pairing that recorded it (an unpair and a pairing again with the same
// server drop it); one the server failed sends nothing and keeps it.
func TestHeartbeatRetriesAudibleRemovalOnlyOnItsPairing(t *testing.T) {
	for _, tc := range []struct {
		name              string
		repair, beatFails bool
		deletes           int32
		pending           bool
	}{
		{name: "paired", deletes: 1},
		{name: "repaired", repair: true},
		{name: "heartbeat fails", beatFails: true, pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			status := &setupServer{kind: "old"}
			var deletes atomic.Int32
			var beatFails atomic.Bool
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if r.URL.Path != "/api/v1/bridge/audible/accounts/acct0001" {
						t.Errorf("DELETE %s", r.URL.Path)
					}
					deletes.Add(1)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if beatFails.Load() && r.URL.Path == "/api/v1/bridge/status" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				status.ServeHTTP(w, r)
			}))
			defer ts.Close()
			b := newTestBridge(t)
			if err := b.Pair(ctx, ts.URL, "CODE"); err != nil {
				t.Fatal(err)
			}
			cl := b.conn.Client()
			a := store.AudibleAccount{ID: "acct0001", Marketplace: "us", ServerID: cl.ServerID()}
			if err := b.db.SignInAudibleAccount(ctx, a, time.Now()); err != nil {
				t.Fatal(err)
			}
			// Simulate a stop after the local transaction but before the HTTP call.
			if err := b.db.DeleteAudibleAccount(ctx, a.ID, cl.PairingID()); err != nil {
				t.Fatal(err)
			}
			if tc.repair {
				if err := b.Unpair(ctx); err != nil {
					t.Fatal(err)
				}
				if err := b.Pair(ctx, ts.URL, "CODE"); err != nil {
					t.Fatal(err)
				}
			}
			beatFails.Store(tc.beatFails)
			b.beat(ctx)
			b.bg.wg.Wait() // the retry runs beside the beat
			if got := deletes.Load(); got != tc.deletes {
				t.Fatalf("sent %d DELETEs, want %d", got, tc.deletes)
			}
			_, err := b.db.NextAudibleRemoval(ctx, cl.PairingID(), time.Now().AddDate(1, 0, 0))
			if pending := err == nil; pending != tc.pending {
				t.Fatalf("removal pending = %t (%v), want %t", pending, err, tc.pending)
			}
		})
	}
}

// A DELETE the server is slow to answer never holds up the heartbeat: the
// retry runs beside it and clears the removal once the server answers.
func TestHeartbeatDoesNotWaitForAPendingRemoval(t *testing.T) {
	ctx := t.Context()
	status := &setupServer{kind: "old"}
	reached, release := make(chan struct{}, 1), make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			reached <- struct{}{}
			<-release
			w.WriteHeader(http.StatusNoContent)
			return
		}
		status.ServeHTTP(w, r)
	}))
	defer ts.Close()
	var released atomic.Bool
	free := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	defer free()
	b := newTestBridge(t)
	if err := b.Pair(ctx, ts.URL, "CODE"); err != nil {
		t.Fatal(err)
	}
	cl := b.conn.Client()
	a := store.AudibleAccount{ID: "acct0001", Marketplace: "us", ServerID: cl.ServerID()}
	if err := b.db.SignInAudibleAccount(ctx, a, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := b.db.DeleteAudibleAccount(ctx, a.ID, cl.PairingID()); err != nil {
		t.Fatal(err)
	}
	beaten := make(chan struct{})
	go func() {
		b.beat(ctx)
		close(beaten)
	}()
	<-reached
	select {
	case <-beaten:
	case <-time.After(10 * time.Second):
		t.Fatal("the heartbeat waited for the pending removal's DELETE")
	}
	free()
	b.bg.wg.Wait()
	if _, err := b.db.NextAudibleRemoval(ctx, cl.PairingID(), time.Now().AddDate(1, 0, 0)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removal still pending: %v", err)
	}
}

// A refused token doesn't end the pairing: an account removed meanwhile owes
// the server its DELETE on the pairing, sent once the token works again,
// rather than left to no one as while unpaired.
func TestAudibleRemovalWhileRefusedStaysOwed(t *testing.T) {
	ctx := t.Context()
	status := &setupServer{kind: "old"}
	var deletes atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		status.ServeHTTP(w, r)
	}))
	defer ts.Close()
	b := newTestBridge(t)
	if err := b.Pair(ctx, ts.URL, "CODE"); err != nil {
		t.Fatal(err)
	}
	cl := b.conn.Client()
	a := store.AudibleAccount{ID: "acct0001", Marketplace: "us", ServerID: cl.ServerID()}
	if err := b.db.SignInAudibleAccount(ctx, a, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.conn.mu.Lock()
	b.conn.refusedAt = time.Now()
	b.conn.mu.Unlock()
	warning, err := b.RemoveAudibleAccount(ctx, a.ID)
	if err != nil || !strings.Contains(warning, "try again") || deletes.Load() != 1 {
		t.Fatalf("remove while refused = %q, %v, %d DELETEs", warning, err, deletes.Load())
	}
	if r, err := b.db.NextAudibleRemoval(ctx, cl.PairingID(), time.Now().AddDate(1, 0, 0)); err != nil || r.Account != a.ID {
		t.Fatalf("owed removal = %+v, %v", r, err)
	}
}
