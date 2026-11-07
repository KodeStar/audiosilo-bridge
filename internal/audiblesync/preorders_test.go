package audiblesync

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// remember records asins as pending preorders the poll remembers.
func (e *env) remember(t *testing.T, asins ...string) {
	t.Helper()
	rows := make([]store.AudiblePreorder, len(asins))
	for i, a := range asins {
		rows[i] = store.AudiblePreorder{ASIN: a, OrderedAt: e.now.AddDate(0, 0, -10), LastSeenAt: e.now}
	}
	if err := e.db.SetAudiblePreorders(context.Background(), acctID, rows); err != nil {
		t.Fatal(err)
	}
}

// A title the server lists for sync that the Bridge remembers as a pending
// preorder (a preorder fuzzy-matched to another edition on disk) is left out:
// Audible is never asked for its place, it isn't listed, and the account's
// health stays clean. The other titles sync as ever.
func TestSyncSkipsRememberedPreorders(t *testing.T) {
	e := newEnv(t)
	e.remember(t, "B0PREORDR1")
	heard := e.now.Add(-time.Hour).Truncate(time.Millisecond)
	e.accts.places = map[string]audible.Position{
		"B000000001": {PositionMs: 1_234_500, LastUpdated: heard},
		"B0PREORDR1": {PositionMs: 60_000, LastUpdated: heard},
	}
	res, err := e.runSync(t, params(t, title("B000000001", "A/One", 36000), title("B0PREORDR1", "A/Six", 36000)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Titles != 1 || res.SkippedPreorder != 1 || res.Heard != 1 || res.Sent != 1 {
		t.Fatalf("result = %+v", res)
	}
	for _, ev := range e.srv.posted[0].Events {
		if ev.Path == "A/Six" {
			t.Fatalf("a preorder's place was sent: %+v", ev)
		}
	}
	if p, err := e.db.GetAudiblePush(context.Background(), acctID, "B0PREORDR1"); err == nil && p.Listed {
		t.Fatalf("the preorder is listed for the push: %+v", p)
	}
	if a := e.account(t); a.SyncError != "" {
		t.Fatalf("sync error = %q", a.SyncError)
	}

	// Released (the poll forgot it): the next sync takes it.
	e.remember(t)
	res, err = e.runSync(t, params(t, title("B000000001", "A/One", 36000), title("B0PREORDR1", "A/Six", 36000)))
	if err != nil || res.Titles != 2 || res.SkippedPreorder != 0 || res.Heard != 2 {
		t.Fatalf("after the release = %+v, %v", res, err)
	}
}

// The push never writes a remembered pending preorder's place (Audible denies
// its content reference: a false sync error every pass); the place waits and
// is written once the poll forgets the preorder.
func TestPushSkipsRememberedPreorders(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B0PREORDR1", "A/Six", 36000))
	e.remember(t, "B0PREORDR1")
	p := NewPusher(e.o)
	e.started(t, p)

	e.srv.add("A/Six", "B0PREORDR1", 600, e.now, "web-1")
	e.read(p)
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 0 || e.accts.acrs != 0 {
		t.Fatalf("a preorder's place was pushed: %v (acr reads %d)", got, e.accts.acrs)
	}
	if a := e.account(t); a.SyncError != "" {
		t.Fatalf("sync error = %q", a.SyncError)
	}

	e.remember(t) // released: the poll forgot it
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B0PREORDR1@600000" {
		t.Fatalf("after the release, sets = %v", got)
	}
}
