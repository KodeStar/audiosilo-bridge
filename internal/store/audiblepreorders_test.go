package store

import (
	"context"
	"testing"
	"time"
)

// The remembered preorders: a set replaces the account's rows (another
// account's stay), times round-trip (zero as unknown), and removing the
// account forgets its rows (and a late set for it fails).
func TestAudiblePreorders(t *testing.T) {
	ctx := context.Background()
	db := open(t)
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{"abcd1234", "efgh5678"} {
		if err := db.SignInAudibleAccount(ctx, AudibleAccount{ID: id, Label: id, Marketplace: "uk", LibraryID: 1, Policy: "manual"}, at); err != nil {
			t.Fatal(err)
		}
	}
	ordered := time.Date(2026, 10, 19, 19, 49, 51, 0, time.UTC)
	if err := db.SetAudiblePreorders(ctx, "abcd1234", []AudiblePreorder{
		{ASIN: "B0HN716NQ7", OrderedAt: ordered, LastSeenAt: at, Record: `{"title":"Beware of Chicken 6"}`, FilledAt: at},
		{ASIN: "B0GV5G2ZKY", OrderedAt: ordered, LastSeenAt: at},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAudiblePreorders(ctx, "efgh5678", []AudiblePreorder{{ASIN: "B0OTHER001", OrderedAt: ordered}}); err != nil {
		t.Fatal(err)
	}
	got, err := db.AudiblePreorders(ctx, "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ASIN != "B0GV5G2ZKY" || got[1].ASIN != "B0HN716NQ7" {
		t.Fatalf("rows = %+v", got)
	}
	if b := got[1]; !b.OrderedAt.Equal(ordered) || !b.LastSeenAt.Equal(at) ||
		!b.FilledAt.Equal(at) || b.Record != `{"title":"Beware of Chicken 6"}` {
		t.Fatalf("row = %+v", b)
	}
	if a := got[0]; a.Record != "" || !a.FilledAt.IsZero() {
		t.Fatalf("an unfilled row = %+v", a)
	}

	// A set replaces: the one left out goes.
	if err := db.SetAudiblePreorders(ctx, "abcd1234", []AudiblePreorder{got[1]}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.AudiblePreorders(ctx, "abcd1234"); len(got) != 1 || got[0].ASIN != "B0HN716NQ7" {
		t.Fatalf("after a replace = %+v", got)
	}
	if other, _ := db.AudiblePreorders(ctx, "efgh5678"); len(other) != 1 {
		t.Fatalf("another account's rows = %+v", other)
	}

	if err := db.DeleteAudibleAccount(ctx, "abcd1234"); err != nil {
		t.Fatal(err)
	}
	if got, err := db.AudiblePreorders(ctx, "abcd1234"); err != nil || len(got) != 0 {
		t.Fatalf("a removed account's rows = %+v, %v", got, err)
	}
	if other, _ := db.AudiblePreorders(ctx, "efgh5678"); len(other) != 1 {
		t.Fatalf("another account's rows after a removal = %+v", other)
	}

	// Denied: a poll that finishes after the removal can't write them back.
	if err := db.SetAudiblePreorders(ctx, "abcd1234", []AudiblePreorder{got[1]}); err == nil {
		t.Fatal("a removed account's preorders were written back")
	}
	if got, _ := db.AudiblePreorders(ctx, "abcd1234"); len(got) != 0 {
		t.Fatalf("a removed account's rows after a late write = %+v", got)
	}
}
