package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestAudibleAccounts(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	if _, err := db.GetAudibleAccount(ctx, "abcd1234"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := db.AudibleAccountByCustomer(ctx, "", "us"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty hash matches nothing: %v", err)
	}
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	a := AudibleAccount{ID: "abcd1234", Label: "Chris (US)", Marketplace: "us", CustomerHash: "h1", ServerID: "srv-1",
		UserID: 3, UserName: "Chris", LibraryID: 1, Policy: "manual", Sync: true}
	if err := db.SignInAudibleAccount(ctx, a, at); err != nil {
		t.Fatal(err)
	}
	if err := db.AudiblePolled(ctx, "abcd1234", 30, 12, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.RefuseAudibleAccount(ctx, "abcd1234", RefusalSignIn, "Audible refused", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAudibleAccount(ctx, "abcd1234")
	if err != nil || got.RefusedAt != Timestamp(at.Add(2*time.Hour)) || got.LastError != "Audible refused" ||
		got.LastTitles != 30 || got.LastMatched != 12 || got.LastPollAt != Timestamp(at.Add(time.Hour)) || !got.Sync ||
		got.DeviceProfile != "" || got.RefusalKind != RefusalSignIn {
		t.Fatalf("account = %+v, %v", got, err)
	}
	if by, err := db.AudibleAccountByCustomer(ctx, "h1", "us"); err != nil || by.ID != "abcd1234" {
		t.Fatalf("by customer = %+v, %v", by, err)
	}
	if _, err := db.AudibleAccountByCustomer(ctx, "h1", "uk"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another marketplace = %v", err)
	}

	// A sign-in again forgets the refusal, keeps created_at and the counts,
	// and records its device's profile.
	a.Label, a.DeviceProfile = "Chris", "4.60/26.2/46000856"
	if err := db.SignInAudibleAccount(ctx, a, at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetAudibleAccount(ctx, "abcd1234")
	if got.RefusedAt != "" || got.LastError != "" || got.RefusalKind != "" || got.CreatedAt != Timestamp(at) || got.LastTitles != 30 ||
		got.Label != "Chris" || got.LastOKAt != Timestamp(at.Add(3*time.Hour)) || got.DeviceProfile != "4.60/26.2/46000856" {
		t.Fatalf("after a sign-in again = %+v", got)
	}

	a.LibraryID, a.Policy, a.Sync, a.ServerID = 2, "all", false, "srv-2"
	if err := db.MapAudibleAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := db.MapAudibleAccount(ctx, AudibleAccount{ID: "nope0000"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mapping an unknown account = %v", err)
	}
	if err := db.AudibleAccountError(ctx, "abcd1234", "the server could not be reached"); err != nil {
		t.Fatal(err)
	}
	list, err := db.AudibleAccounts(ctx)
	if err != nil || len(list) != 1 || list[0].LibraryID != 2 || list[0].Policy != "all" || list[0].Sync ||
		list[0].ServerID != "srv-2" || list[0].LastError != "the server could not be reached" ||
		list[0].DeviceProfile != "4.60/26.2/46000856" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := db.DeleteAudibleAccount(ctx, "abcd1234", ""); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.AudibleAccounts(ctx); len(list) != 0 {
		t.Fatalf("after delete = %+v", list)
	}
}

// One account per Amazon account and marketplace: a second row for the same
// pair is refused, another marketplace or an empty customer hash is not.
func TestAudibleAccountsUniquePerCustomer(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	a := AudibleAccount{ID: "abcd1234", Label: "Chris", Marketplace: "us", CustomerHash: "h1", ServerID: "srv-1",
		UserID: 3, UserName: "Chris", LibraryID: 1, Policy: "manual"}
	if err := db.SignInAudibleAccount(ctx, a, at); err != nil {
		t.Fatal(err)
	}
	b := a
	b.ID = "efgh5678"
	if err := db.SignInAudibleAccount(ctx, b, at); err == nil {
		t.Fatal("a second account for one Amazon account and marketplace was stored")
	}
	b.Marketplace = "uk"
	if err := db.SignInAudibleAccount(ctx, b, at); err != nil {
		t.Fatalf("another marketplace: %v", err)
	}
	for _, id := range []string{"none0001", "none0002"} {
		c := a
		c.ID, c.CustomerHash = id, ""
		if err := db.SignInAudibleAccount(ctx, c, at); err != nil {
			t.Fatalf("an unknown customer: %v", err)
		}
	}
}

// A poll ends a sign-in refusal but never a device one (a device Audible
// denies licenses still lists its library), and a poll's error never replaces
// a refusal's reason; a sign-in ends either. The kind is a column (bridge#46),
// never read from last_error's text.
func TestAudiblePolledKeepsADeviceRefusal(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	a := AudibleAccount{ID: "abcd1234", Label: "Chris", Marketplace: "us", CustomerHash: "h1", LibraryID: 1, Policy: "manual"}
	if err := db.SignInAudibleAccount(ctx, a, at); err != nil {
		t.Fatal(err)
	}
	get := func() AudibleAccount {
		t.Helper()
		got, err := db.GetAudibleAccount(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// A sign-in refusal ends with a poll.
	if err := db.RefuseAudibleAccount(ctx, a.ID, RefusalSignIn, "sign-in refused", at); err != nil {
		t.Fatal(err)
	}
	if err := db.AudiblePolled(ctx, a.ID, 3, 1, at); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.RefusedAt != "" || got.LastError != "" || got.RefusalKind != "" {
		t.Fatalf("a sign-in refusal outlived a poll: %+v", got)
	}
	// An error with no refusal is recorded.
	if err := db.AudibleAccountError(ctx, a.ID, "server down"); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.LastError != "server down" {
		t.Fatalf("error = %+v", got)
	}

	// A device refusal stays through an error and a poll, whatever its text.
	if err := db.RefuseAudibleAccount(ctx, a.ID, RefusalDevice, "any words", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.AudibleAccountError(ctx, a.ID, "server down"); err != nil {
		t.Fatal(err)
	}
	if err := db.AudiblePolled(ctx, a.ID, 4, 2, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.RefusedAt != Timestamp(at.Add(time.Hour)) || got.LastError != "any words" ||
		got.RefusalKind != RefusalDevice || got.LastTitles != 4 {
		t.Fatalf("the device refusal didn't stay: %+v", got)
	}
	// Another kind's acceptance leaves it; its own ends it.
	if ended, err := db.AudibleAccepted(ctx, a.ID, RefusalSignIn); err != nil || ended {
		t.Fatalf("accepted another kind = %v, %v", ended, err)
	}
	if got := get(); got.RefusalKind != RefusalDevice {
		t.Fatalf("the device refusal ended: %+v", got)
	}
	// A sign-in refusal (a listing's 401) never overwrites a standing device
	// one: its guidance, sign in again, is what the account needs (bridge#64).
	if err := db.RefuseAudibleAccount(ctx, a.ID, RefusalSignIn, "sign-in refused", at.Add(90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.RefusalKind != RefusalDevice || got.LastError != "any words" || got.RefusedAt != Timestamp(at.Add(time.Hour)) {
		t.Fatalf("a sign-in refusal replaced the device's: %+v", got)
	}
	// A sign-in again ends it; a sign-in refusal then is recorded as one.
	if err := db.SignInAudibleAccount(ctx, a, at.Add(100*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.RefuseAudibleAccount(ctx, a.ID, RefusalSignIn, "sign-in refused", at); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.RefusalKind != RefusalSignIn || got.LastError != "sign-in refused" {
		t.Fatalf("a sign-in refusal = %+v", got)
	}
	// A device refusal replaces a sign-in one.
	if err := db.RefuseAudibleAccount(ctx, a.ID, RefusalDevice, "throttled", at); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.RefusalKind != RefusalDevice || got.LastError != "throttled" {
		t.Fatalf("a device refusal over a sign-in one = %+v", got)
	}
	if err := db.SignInAudibleAccount(ctx, a, at.Add(110*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := db.RefuseAudibleAccount(ctx, a.ID, RefusalSignIn, "sign-in refused", at); err != nil {
		t.Fatal(err)
	}
	if ended, err := db.AudibleAccepted(ctx, a.ID, RefusalSignIn); err != nil || !ended {
		t.Fatalf("accepted = %v, %v", ended, err)
	}
	if got := get(); got.RefusedAt != "" || got.LastError != "" || got.RefusalKind != "" {
		t.Fatalf("accepted kept the refusal: %+v", got)
	}
	if err := db.RefuseAudibleAccount(ctx, a.ID, RefusalDevice, "throttled", at); err != nil {
		t.Fatal(err)
	}
	if err := db.SignInAudibleAccount(ctx, a, at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.RefusedAt != "" || got.LastError != "" || got.RefusalKind != "" {
		t.Fatalf("a sign-in kept the refusal: %+v", got)
	}
}

// Migration 0011 gives the refusals 4b stored their kind: a refused row whose
// last_error is the throttled text is a device refusal, any other refused row
// a sign-in one, and a row not refused none.
func TestMigrationRefusalKind(t *testing.T) {
	ctx := context.Background()
	sqldb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	sqldb.SetMaxOpenConns(1)
	db := &DB{sql: sqldb}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.migrateTo(ctx, "0010_audible_profile.sql"); err != nil {
		t.Fatal(err)
	}
	const throttled = "Audible refused this device its downloads (customer throttled): sign in again"
	for _, r := range []struct{ id, refused, why string }{
		{"throttled1", "2026-11-03T10:00:00.000000000Z", throttled},
		{"signedin01", "2026-11-03T10:00:00.000000000Z", "Audible refused the sign-in: sign in again"},
		{"working001", "", "the server could not be reached"},
		{"oldtext001", "", throttled}, // the text alone, never refused: no kind
	} {
		if _, err := sqldb.ExecContext(ctx, `INSERT INTO audible_accounts (id, label, marketplace, created_at, refused_at, last_error)
			VALUES (?, 'x', 'us', '2026-11-01T00:00:00.000000000Z', ?, ?)`, r.id, r.refused, r.why); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"throttled1": RefusalDevice, "signedin01": RefusalSignIn, "working001": "", "oldtext001": ""} {
		got, err := db.GetAudibleAccount(ctx, id)
		if err != nil || got.RefusalKind != want {
			t.Errorf("%s: kind %q, want %q (%v)", id, got.RefusalKind, want, err)
		}
	}
	// The device refusal now outlives a poll by its kind.
	if err := db.AudiblePolled(ctx, "throttled1", 1, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetAudibleAccount(ctx, "throttled1"); got.RefusedAt == "" || got.LastError != throttled {
		t.Fatalf("a migrated device refusal ended with a poll: %+v", got)
	}
}

func TestMigrationAudibleRemovals(t *testing.T) {
	ctx := t.Context()
	sqldb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	sqldb.SetMaxOpenConns(1)
	db := &DB{sql: sqldb}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.migrateTo(ctx, "0021_server_jobs_pairing.sql"); err != nil {
		t.Fatal(err)
	}
	a := AudibleAccount{ID: "abcd1234", Label: "Chris", Marketplace: "us", CustomerHash: "customer", ServerID: "server"}
	if _, err := sqldb.ExecContext(ctx, `INSERT INTO audible_accounts (id, label, marketplace, customer_hash, server_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, a.ID, a.Label, a.Marketplace, a.CustomerHash, a.ServerID, timestamp(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetAudibleAccount(ctx, a.ID); err != nil || got.Label != a.Label || got.CustomerHash != a.CustomerHash {
		t.Fatalf("upgraded account = %+v, %v", got, err)
	}
	if _, err := db.NextAudibleRemoval(ctx, "pair", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("upgrade queued a removal: %v", err)
	}
	if _, err := sqldb.ExecContext(ctx, `CREATE TRIGGER fail_removal BEFORE DELETE ON audible_accounts
		BEGIN SELECT RAISE(ABORT, 'local delete failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAudibleAccount(ctx, a.ID, "pair"); err == nil {
		t.Fatal("local deletion succeeded despite the trigger")
	}
	if _, err := db.NextAudibleRemoval(ctx, "pair", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled back local removal queued a DELETE: %v", err)
	}
	if _, err := sqldb.ExecContext(ctx, `DROP TRIGGER fail_removal`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAudibleAccount(ctx, a.ID, "pair"); err != nil {
		t.Fatal(err)
	}
	if r, err := db.NextAudibleRemoval(ctx, "pair", time.Now()); err != nil || r.Account != a.ID || r.Pairing != "pair" || r.Attempts != 0 {
		t.Fatalf("removal after upgrade = %+v, %v", r, err)
	}
}
