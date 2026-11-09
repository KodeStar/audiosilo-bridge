package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The push rows: a sync lists titles (an unlisted title loses its waiting
// place), a want waits on a listed row only, a push records its place, and
// the cached content reference and the place heard stay through a listing.
func TestAudiblePushRows(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	a := AudibleAccount{ID: "abcd1234", Label: "Chris", Marketplace: "us", ServerID: "srv-1", UserID: 3, LibraryID: 1, Policy: "manual"}
	if err := db.SignInAudibleAccount(ctx, a, at); err != nil {
		t.Fatal(err)
	}
	if err := db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{
		{ASIN: "B000000001", LibraryID: 1, Path: "A/One", RuntimeS: 3600},
		{ASIN: "B000000002", LibraryID: 1, Path: "A/Two", RuntimeS: 7200},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAudiblePush(ctx, a.ID, "B000000009"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown asin = %v", err)
	}
	if err := db.WantAudiblePush(ctx, a.ID, "B000000001", AudibleRow{Ms: 60_000, Seen: at}); err != nil {
		t.Fatal(err)
	}
	if err := db.WantAudiblePush(ctx, a.ID, "B000000002", AudibleRow{Ms: 90_000, Seen: at}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAudibleACR(ctx, a.ID, "B000000001", "CR!ACR1"); err != nil {
		t.Fatal(err)
	}
	if err := db.AudibleHeard(ctx, AudiblePush{Account: a.ID, ASIN: "B000000001", RowMs: 60_000, RowSeen: Timestamp(at)}, 30_000, false, at); err != nil {
		t.Fatal(err)
	}
	p, err := db.GetAudiblePush(ctx, a.ID, "B000000001")
	if err != nil || !p.Listed || p.WantMs == nil || *p.WantMs != 60_000 || p.RowMs != 60_000 || p.RowSeen != Timestamp(at) || p.LastMs != -1 ||
		p.ACR != "CR!ACR1" || p.HeardMs != 30_000 || p.RuntimeS != 3600 || p.Path != "A/One" {
		t.Fatalf("push = %+v, %v", p, err)
	}
	if w, err := db.AudibleWaiting(ctx, a.ID); err != nil || len(w) != 2 {
		t.Fatalf("waiting = %+v, %v", w, err)
	}
	// Dropped: the waiting place goes without a write.
	if err := db.DropAudiblePush(ctx, a.ID, "B000000002"); err != nil {
		t.Fatal(err)
	}
	if w, _ := db.AudibleWaiting(ctx, a.ID); len(w) != 1 || w[0].ASIN != "B000000001" {
		t.Fatalf("waiting after a drop = %+v", w)
	}
	if two, _ := db.GetAudiblePush(ctx, a.ID, "B000000002"); two.WantMs != nil || two.LastMs != -1 {
		t.Fatalf("a drop wrote a push: %+v", two)
	}
	_ = db.WantAudiblePush(ctx, a.ID, "B000000002", AudibleRow{Ms: 90_000, Seen: at})
	if err := db.AudiblePushed(ctx, a.ID, "B000000001", 60_000, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if p, _ = db.GetAudiblePush(ctx, a.ID, "B000000001"); p.WantMs != nil || p.LastMs != 60_000 || p.At != Timestamp(at.Add(time.Minute)) ||
		p.HeardMs != -1 {
		t.Fatalf("after the push = %+v (the place heard before it is no longer Audible's)", p)
	}

	// The next sync lists only the first (the second was marked Don't sync):
	// the second is unlisted and its waiting place forgotten; a want on it is
	// refused from now on.
	if err := db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000001", LibraryID: 1, Path: "A/One", RuntimeS: 3600}}); err != nil {
		t.Fatal(err)
	}
	two, _ := db.GetAudiblePush(ctx, a.ID, "B000000002")
	if two.Listed || two.WantMs != nil {
		t.Fatalf("an unlisted title kept its place: %+v", two)
	}
	if err := db.WantAudiblePush(ctx, a.ID, "B000000002", AudibleRow{Ms: 1, Seen: at}); err != nil {
		t.Fatal(err)
	}
	if two, _ = db.GetAudiblePush(ctx, a.ID, "B000000002"); two.WantMs != nil {
		t.Fatalf("an unlisted title took a want: %+v", two)
	}
	if p, _ = db.GetAudiblePush(ctx, a.ID, "B000000001"); p.ACR != "CR!ACR1" || p.LastMs != 60_000 {
		t.Fatalf("a listing forgot the push state: %+v", p)
	}
	// A title whose book moved forgets its waiting place.
	_ = db.WantAudiblePush(ctx, a.ID, "B000000001", AudibleRow{Ms: 5, Seen: at})
	_ = db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000001", LibraryID: 1, Path: "A/One (moved)", RuntimeS: 3600}})
	if p, _ = db.GetAudiblePush(ctx, a.ID, "B000000001"); p.WantMs != nil || p.Path != "A/One (moved)" {
		t.Fatalf("a moved book kept its waiting place: %+v", p)
	}
	// Seen: the waiting place goes, the row stays.
	_ = db.WantAudiblePush(ctx, a.ID, "B000000001", AudibleRow{Ms: 5, Seen: at})
	if err := db.SeenAudibleRow(ctx, a.ID, "B000000001", AudibleRow{Ms: 7, Seen: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if p, _ = db.GetAudiblePush(ctx, a.ID, "B000000001"); p.WantMs != nil || p.RowMs != 7 ||
		p.RowSeen != Timestamp(at.Add(time.Hour)) {
		t.Fatalf("seen = %+v", p)
	}
	list, err := db.AudiblePushes(ctx, a.ID)
	if err != nil || len(list) != 2 || list[0].ASIN != "B000000001" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// Removing the account removes its rows.
	if err := db.DeleteAudibleAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.AudiblePushes(ctx, a.ID); len(list) != 0 {
		t.Fatalf("rows outlived the account: %+v", list)
	}
}

// The two directions' markers: a place heard from Audible ends the place last
// written (unless the push wrote again since the job started), a place the
// server applied drops the place waiting from an older server row (unless the
// push read a newer row since), and a place pending on a title not listed yet
// is kept by the listing that lists it for its book, else forgotten.
func TestAudiblePushMarkers(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	a := AudibleAccount{ID: "abcd1234", Label: "Chris", Marketplace: "us", ServerID: "srv-1", UserID: 3, LibraryID: 1, Policy: "manual"}
	if err := db.SignInAudibleAccount(ctx, a, at); err != nil {
		t.Fatal(err)
	}
	one := AudibleSyncTitle{ASIN: "B000000001", LibraryID: 1, Path: "A/One", RuntimeS: 3600}
	if err := db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{one}); err != nil {
		t.Fatal(err)
	}
	get := func() AudiblePush {
		t.Helper()
		p, err := db.GetAudiblePush(ctx, a.ID, "B000000001")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The push wrote 60 s an hour ago; a job starting now hears 90 s: the
	// write is no longer Audible's place. Not applied: the waiting place stays.
	_ = db.AudiblePushed(ctx, a.ID, "B000000001", 60_000, at.Add(-time.Hour))
	_ = db.WantAudiblePush(ctx, a.ID, "B000000001", AudibleRow{Ms: 70_000, Seen: at})
	snap := get()
	if err := db.AudibleHeard(ctx, snap, 90_000, false, at); err != nil {
		t.Fatal(err)
	}
	if p := get(); p.HeardMs != 90_000 || p.LastMs != -1 || p.WantMs == nil {
		t.Fatalf("heard = %+v", p)
	}
	// Applied: the waiting place goes.
	if err := db.AudibleHeard(ctx, snap, 95_000, true, at); err != nil {
		t.Fatal(err)
	}
	if p := get(); p.WantMs != nil || p.HeardMs != 95_000 {
		t.Fatalf("applied = %+v", p)
	}
	// The push wrote again and read a newer row while the job ran: both stay.
	snap = get()
	_ = db.AudiblePushed(ctx, a.ID, "B000000001", 100_000, at.Add(time.Minute))
	_ = db.WantAudiblePush(ctx, a.ID, "B000000001", AudibleRow{Ms: 110_000, Seen: at.Add(time.Minute)})
	if err := db.AudibleHeard(ctx, snap, 120_000, true, at); err != nil {
		t.Fatal(err)
	}
	if p := get(); p.LastMs != 100_000 || p.WantMs == nil || *p.WantMs != 110_000 || p.HeardMs != 120_000 {
		t.Fatalf("a push during the job was undone: %+v", p)
	}
	// Pending: a place on a title the feed syncs but the last job didn't list.
	row := AudibleRow{Ms: 5_000, Seen: at}
	if err := db.PendAudiblePush(ctx, a.ID, "B000000002", 1, "A/Two", true, row); err != nil {
		t.Fatal(err)
	}
	if w, _ := db.AudibleWaiting(ctx, a.ID); len(w) != 1 || w[0].ASIN != "B000000001" {
		t.Fatalf("a pending place is waiting to be pushed: %+v", w)
	}
	// A re-match: the listed title's book changes on the server first.
	if err := db.PendAudiblePush(ctx, a.ID, "B000000001", 1, "A/One (right)", true,
		AudibleRow{Ms: 5_000, Seen: at.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if p := get(); p.Listed || p.Path != "A/One (right)" || p.WantMs == nil || p.RowSeen != Timestamp(at.Add(2*time.Minute)) ||
		p.LastMs != 100_000 {
		t.Fatalf("re-matched = %+v", p)
	}
	// The next job lists both for those books: kept, and now waiting.
	if err := db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000001", LibraryID: 1, Path: "A/One (right)", RuntimeS: 3600},
		{ASIN: "B000000002", LibraryID: 1, Path: "A/Two", RuntimeS: 7200}}); err != nil {
		t.Fatal(err)
	}
	if w, _ := db.AudibleWaiting(ctx, a.ID); len(w) != 2 || w[1].RuntimeS != 7200 {
		t.Fatalf("listed pending places = %+v", w)
	}
	// A pending place the next job doesn't list for its book goes.
	_ = db.PendAudiblePush(ctx, a.ID, "B000000003", 1, "A/Three", true, row)
	// History pending: the row only, no waiting place.
	if err := db.PendAudiblePush(ctx, a.ID, "B000000004", 1, "A/Four", false, row); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.GetAudiblePush(ctx, a.ID, "B000000004"); p.WantMs != nil || p.RowMs != 5_000 || p.Listed {
		t.Fatalf("history pending = %+v", p)
	}
	_ = db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000003", LibraryID: 1, Path: "A/Other", RuntimeS: 60}})
	if p, _ := db.GetAudiblePush(ctx, a.ID, "B000000003"); p.WantMs != nil || !p.Listed {
		t.Fatalf("a pending place on another book was kept: %+v", p)
	}
	// A newer row read while the job ran, with no push since it started, and
	// behind the place the server took: it goes (Audible is still there).
	later := at.Add(2 * time.Hour)
	snap, _ = db.GetAudiblePush(ctx, a.ID, "B000000003")
	_ = db.WantAudiblePush(ctx, a.ID, "B000000003", AudibleRow{Ms: 130_000, Seen: later.Add(time.Minute)})
	if err := db.AudibleHeard(ctx, snap, 140_000, true, later); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.GetAudiblePush(ctx, a.ID, "B000000003"); p.WantMs != nil {
		t.Fatalf("a place behind the one the server took stayed: %+v", p)
	}
	// One further on than the place heard stays.
	snap, _ = db.GetAudiblePush(ctx, a.ID, "B000000003")
	_ = db.WantAudiblePush(ctx, a.ID, "B000000003", AudibleRow{Ms: 150_000, Seen: later.Add(2 * time.Minute)})
	if err := db.AudibleHeard(ctx, snap, 145_000, true, later); err != nil {
		t.Fatal(err)
	}
	if p, _ := db.GetAudiblePush(ctx, a.ID, "B000000003"); p.WantMs == nil || *p.WantMs != 150_000 {
		t.Fatalf("a place further on went: %+v", p)
	}
	// A job listing the title for another book forgets the row last read.
	_ = db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000003", LibraryID: 1, Path: "A/Elsewhere", RuntimeS: 60}})
	if p, _ := db.GetAudiblePush(ctx, a.ID, "B000000003"); p.WantMs != nil || p.RowMs != -1 || p.RowSeen != "" {
		t.Fatalf("re-listed for another book = %+v", p)
	}
}

// The feed's cursor and its start: kept through a mapping that changes only
// the label or policy, started again (with the waiting places and listing
// forgotten) when the account moves to another server, person or library.
func TestAudiblePushCursorRemap(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	a := AudibleAccount{ID: "abcd1234", Label: "Chris", Marketplace: "us", ServerID: "srv-1", UserID: 3, LibraryID: 1, Policy: "manual"}
	if err := db.SignInAudibleAccount(ctx, a, at); err != nil {
		t.Fatal(err)
	}
	_ = db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000001", LibraryID: 1, Path: "A/One"}})
	set := func() {
		t.Helper()
		if err := db.SetAudiblePushCursor(ctx, a.ID, "c42", at); err != nil {
			t.Fatal(err)
		}
		_ = db.WantAudiblePush(ctx, a.ID, "B000000001", AudibleRow{Ms: 5, Seen: at})
	}
	// A baseline still under way (no from) leaves the start unset.
	if err := db.SetAudiblePushCursor(ctx, a.ID, "c41", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetAudibleAccount(ctx, a.ID); got.PushCursor != "c41" || got.PushFrom != "" {
		t.Fatalf("mid-baseline = %q from %q", got.PushCursor, got.PushFrom)
	}
	set()
	// The start is kept once set.
	if err := db.SetAudiblePushCursor(ctx, a.ID, "c43", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetAudibleAccount(ctx, a.ID)
	if got.PushCursor != "c43" || got.PushFrom != Timestamp(at) {
		t.Fatalf("cursor = %q from %q", got.PushCursor, got.PushFrom)
	}
	a.Label, a.Policy = "Chris again", "all"
	if err := db.MapAudibleAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := db.SignInAudibleAccount(ctx, a, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetAudibleAccount(ctx, a.ID)
	p, _ := db.GetAudiblePush(ctx, a.ID, "B000000001")
	if got.PushCursor != "c43" || p.WantMs == nil || !p.Listed {
		t.Fatalf("a mapping in place started the feed again: %+v, %+v", got, p)
	}
	for name, move := range map[string]func(*AudibleAccount){
		"server":  func(a *AudibleAccount) { a.ServerID = "srv-2" },
		"user":    func(a *AudibleAccount) { a.UserID = 4 },
		"library": func(a *AudibleAccount) { a.LibraryID = 2 },
	} {
		_ = db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000001", LibraryID: a.LibraryID, Path: "A/One"}})
		set()
		move(&a)
		if err := db.MapAudibleAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
		got, _ = db.GetAudibleAccount(ctx, a.ID)
		p, _ = db.GetAudiblePush(ctx, a.ID, "B000000001")
		if got.PushCursor != "" || got.PushFrom != "" || p.WantMs != nil || p.Listed || p.RowMs != -1 || p.RowSeen != "" {
			t.Fatalf("%s: a move kept the feed: %+v, %+v", name, got, p)
		}
	}
	// A sign-in again that maps it elsewhere starts it again too.
	_ = db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000001", LibraryID: a.LibraryID, Path: "A/One"}})
	set()
	a.UserID = 9
	if err := db.SignInAudibleAccount(ctx, a, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.GetAudibleAccount(ctx, a.ID); got.PushCursor != "" {
		t.Fatalf("a sign-in mapping it elsewhere kept the feed: %+v", got)
	}
}

// The sync's health: a failure records why and the wait, a success clears both;
// neither touches a refusal.
func TestAudibleSyncHealth(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	a := AudibleAccount{ID: "abcd1234", Label: "Chris", Marketplace: "us", LibraryID: 1, Policy: "manual"}
	if err := db.SignInAudibleAccount(ctx, a, at); err != nil {
		t.Fatal(err)
	}
	if err := db.AudibleSyncFailed(ctx, a.ID, "Audible asked to wait", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetAudibleAccount(ctx, a.ID)
	if got.SyncError != "Audible asked to wait" || got.SyncRetryAt != Timestamp(at.Add(time.Hour)) || got.RefusedAt != "" {
		t.Fatalf("failed = %+v", got)
	}
	// The job and the push share the wait: a failure with no wait of its
	// own, a shorter one or a success before it has passed never ends it.
	if err := db.AudibleSyncFailed(ctx, a.ID, "no wait", time.Time{}); err != nil {
		t.Fatal(err)
	}
	_ = db.AudibleSyncFailed(ctx, a.ID, "a shorter wait", at.Add(time.Minute))
	if got, _ = db.GetAudibleAccount(ctx, a.ID); got.SyncRetryAt != Timestamp(at.Add(time.Hour)) || got.SyncError != "a shorter wait" {
		t.Fatalf("a later failure shortened the wait: %+v", got)
	}
	if err := db.AudibleSyncOK(ctx, a.ID, at.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.GetAudibleAccount(ctx, a.ID); got.SyncError != "" || got.SyncRetryAt != Timestamp(at.Add(time.Hour)) {
		t.Fatalf("a success ended a wait still ahead: %+v", got)
	}
	_ = db.AudibleSyncFailed(ctx, a.ID, "a longer wait", at.Add(90*time.Minute))
	if got, _ = db.GetAudibleAccount(ctx, a.ID); got.SyncRetryAt != Timestamp(at.Add(90*time.Minute)) {
		t.Fatalf("a longer wait was not kept: %+v", got)
	}
	if err := db.AudibleSyncOK(ctx, a.ID, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.GetAudibleAccount(ctx, a.ID); got.SyncError != "" || got.SyncRetryAt != "" || got.SyncAt != Timestamp(at.Add(2*time.Hour)) {
		t.Fatalf("ok = %+v", got)
	}
}

// A new device discards old sync errors and waits. A new mapping or sync-off
// clears the error without recording a success, but keeps a wait Audible gave
// (the device is the same); an ordinary mapping save keeps both.
func TestAudibleSyncHealthReset(t *testing.T) {
	for _, change := range []string{"sign in", "remap", "same mapping", "sync off"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			db := open(t)
			at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
			a := AudibleAccount{ID: "abcd1234", Marketplace: "us", ServerID: "srv-1", UserID: 3, LibraryID: 1}
			if err := db.SignInAudibleAccount(ctx, a, at); err != nil {
				t.Fatal(err)
			}
			if err := db.AudibleSyncOK(ctx, a.ID, at); err != nil {
				t.Fatal(err)
			}
			if err := db.AudibleSyncFailed(ctx, a.ID, "old sync error", at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			var err error
			switch change {
			case "sign in":
				err = db.SignInAudibleAccount(ctx, a, at.Add(time.Minute))
			case "remap":
				a.UserID++
				err = db.MapAudibleAccount(ctx, a)
			case "same mapping":
				err = db.MapAudibleAccount(ctx, a)
			case "sync off":
				var cleared bool
				if cleared, err = db.ClearAudibleSyncError(ctx, a.ID); err == nil && !cleared {
					t.Error("clearing stale sync health reported nothing cleared")
				}
				if again, aerr := db.ClearAudibleSyncError(ctx, a.ID); aerr != nil || again {
					t.Errorf("clearing clear sync health = %v, %v; want false, nil", again, aerr)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := db.GetAudibleAccount(ctx, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.SyncAt != Timestamp(at) {
				t.Errorf("reset changed the last successful sync: %+v", got)
			}
			wait := Timestamp(at.Add(time.Hour))
			switch change {
			case "same mapping":
				if got.SyncError != "old sync error" || got.SyncRetryAt != wait {
					t.Errorf("ordinary save lost sync health: %+v", got)
				}
			case "sign in":
				if got.SyncError != "" || got.SyncRetryAt != "" {
					t.Errorf("a new device kept stale sync health: %+v", got)
				}
			default:
				if got.SyncError != "" || got.SyncRetryAt != wait {
					t.Errorf("%s: want the error cleared and Audible's wait kept: %+v", change, got)
				}
			}
		})
	}
}

// RearmAudiblePush (bridge#51) makes the server row last read wait again, only
// while nothing changed since the sync read the row: denied once the push
// wrote again, a new server row was recorded, a place already waits, or the
// title is no longer listed.
func TestRearmAudiblePush(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	a := AudibleAccount{ID: "abcd1234", Label: "Chris", Marketplace: "us", ServerID: "srv-1", UserID: 3, LibraryID: 1, Policy: "manual"}
	if err := db.SignInAudibleAccount(ctx, a, at); err != nil {
		t.Fatal(err)
	}
	if err := db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000001", LibraryID: 1, Path: "A/One", RuntimeS: 36000}}); err != nil {
		t.Fatal(err)
	}
	// The push wrote the server's place.
	if err := db.WantAudiblePush(ctx, a.ID, "B000000001", AudibleRow{Ms: 5_000_000, Seen: at}); err != nil {
		t.Fatal(err)
	}
	if err := db.AudiblePushed(ctx, a.ID, "B000000001", 5_000_000, at.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	read := func() AudiblePush {
		x, err := db.GetAudiblePush(ctx, a.ID, "B000000001")
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	rearm := func(x AudiblePush, now time.Time) bool {
		ok, err := db.RearmAudiblePush(ctx, x, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	now := at.Add(10 * time.Minute)

	// Denied: the push wrote again since the sync read the row.
	x := read()
	if err := db.WantAudiblePush(ctx, a.ID, "B000000001", AudibleRow{Ms: 5_000_000, Seen: at}); err != nil {
		t.Fatal(err)
	}
	if err := db.AudiblePushed(ctx, a.ID, "B000000001", 5_000_000, at.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if rearm(x, now) || read().WantMs != nil {
		t.Fatalf("re-armed after a push since: %+v", read())
	}
	// Denied: a new server row was recorded since.
	x = read()
	if err := db.WantAudiblePush(ctx, a.ID, "B000000001", AudibleRow{Ms: 6_000_000, Seen: now}); err != nil {
		t.Fatal(err)
	}
	if rearm(x, now) || *read().WantMs != 6_000_000 {
		t.Fatalf("re-armed over a new row: %+v", read())
	}
	// Denied: a place already waits (the same row read again).
	x = read()
	if rearm(x, now.Add(time.Minute)) || read().RowSeen != Timestamp(now) {
		t.Fatalf("re-armed over a waiting place: %+v", read())
	}
	if err := db.AudiblePushed(ctx, a.ID, "B000000001", 6_000_000, now); err != nil {
		t.Fatal(err)
	}
	// Denied: no longer listed.
	x = read()
	if err := db.ListAudibleSync(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if rearm(x, now) {
		t.Fatalf("re-armed an unlisted title: %+v", read())
	}
	// Allowed: listed again, nothing changed since the read.
	if err := db.ListAudibleSync(ctx, a.ID, []AudibleSyncTitle{{ASIN: "B000000001", LibraryID: 1, Path: "A/One", RuntimeS: 36000}}); err != nil {
		t.Fatal(err)
	}
	x = read()
	later := now.Add(20 * time.Minute)
	if !rearm(x, later) {
		t.Fatalf("not re-armed: %+v", read())
	}
	if y := read(); y.WantMs == nil || *y.WantMs != 6_000_000 || y.RowSeen != Timestamp(later) || y.LastMs != 6_000_000 {
		t.Fatalf("re-armed row = %+v", y)
	}
}
