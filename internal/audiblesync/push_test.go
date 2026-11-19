package audiblesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// list records the titles a sync listed (as a job would).
func (e *env) list(t *testing.T, titles ...store.AudibleSyncTitle) {
	t.Helper()
	if err := e.db.ListAudibleSync(context.Background(), acctID, titles); err != nil {
		t.Fatal(err)
	}
}

func st(asin, path string, runtimeS int64) store.AudibleSyncTitle {
	return store.AudibleSyncTitle{ASIN: asin, LibraryID: 1, Path: path, RuntimeS: runtimeS}
}

// started runs the first pass (the baseline: the feed's start), so later rows
// are new.
func (e *env) started(t *testing.T, p *Pusher) {
	t.Helper()
	p.Tick(context.Background())
	if a := e.account(t); a.PushFrom == "" {
		t.Fatalf("the first pass set no start: %+v", a)
	}
}

// read runs a pass now: the rows added since are first read at this time,
// the moment their stillness counts from.
func (e *env) read(p *Pusher) { p.Tick(context.Background()) }

func (e *env) sets() []string {
	e.accts.mu.Lock()
	defer e.accts.mu.Unlock()
	return append([]string(nil), e.accts.sets...)
}

// A place is written to Audible once its row has been still for 3 minutes:
// not while the listener is still moving it, once after, and not again while
// it stays within 5 seconds of the place written (the debounce); a later move
// past 5 seconds is written again. The content reference is read once.
func TestPushDebounce(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	p := NewPusher(e.o)
	e.started(t, p)

	e.srv.add("A/One", "B000000001", 600, e.now, "web-1")
	e.read(p)
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if len(e.sets()) != 0 {
		t.Fatalf("a place a minute old was written: %v", e.sets())
	}
	// Still moving: the newest row restarts the wait, from when the Bridge
	// reads it.
	e.srv.add("A/One", "B000000001", 660, e.now, "web-1")
	e.now = e.now.Add(2 * time.Minute)
	p.Tick(context.Background())
	e.now = e.now.Add(2 * time.Minute)
	p.Tick(context.Background())
	if len(e.sets()) != 0 {
		t.Fatalf("a place still for 2 minutes was written: %v", e.sets())
	}
	e.now = e.now.Add(time.Minute) // 3 minutes still
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B000000001@660000" {
		t.Fatalf("sets = %v", got)
	}
	// Nothing new: nothing more.
	e.now = e.now.Add(5 * time.Minute)
	p.Tick(context.Background())
	// A row within 5 seconds of the place written isn't written again.
	e.srv.add("A/One", "B000000001", 664, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 {
		t.Fatalf("a place within 5 s of the last one was written again: %v", got)
	}
	// One further on is.
	e.srv.add("A/One", "B000000001", 900, e.now, "web-1")
	e.read(p)
	e.now = e.now.Add(3 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 2 || got[1] != "B000000001@900000" || e.accts.acrs != 1 {
		t.Fatalf("sets = %v (acr reads %d)", got, e.accts.acrs)
	}
	if a := e.account(t); a.PushAt == "" || a.PushError != "" || a.PushCursor == "" {
		t.Fatalf("account = %+v", a)
	}
}

// Echo suppression: a row Audible's own sync wrote (device audible:<account>;
// the server leaves them out, the Bridge guards too) and a row at the place
// the last sync heard from Audible (a player that moved to Audible's place)
// are never written back.
func TestPushEchoSuppression(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000), st("B000000002", "A/Two", 36000))
	if err := e.db.AudibleHeard(context.Background(), store.AudiblePush{Account: acctID, ASIN: "B000000002"}, 1_200_000, false, e.now); err != nil {
		t.Fatal(err)
	}
	p := NewPusher(e.o)
	e.started(t, p)
	e.srv.add("A/One", "B000000001", 700, e.now, DeviceID(acctID))
	e.srv.add("A/Two", "B000000002", 1202, e.now, "web-1") // within 5 s of the place heard
	e.read(p)                                              // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 0 {
		t.Fatalf("an echo was written back: %v", got)
	}
	// A real move on the second title is written.
	e.srv.add("A/Two", "B000000002", 1500, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B000000002@1500000" {
		t.Fatalf("sets = %v", got)
	}
}

// Denied: a place past Audible's runtime (the match looks wrong) is never
// written to the real account; a title the last sync didn't list (Don't sync
// this book, Not this book), or a row on another book than the title's match,
// neither.
func TestPushGuards(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 3600), st("B000000002", "A/Two", 36000))
	p := NewPusher(e.o)
	e.started(t, p)
	e.srv.add("A/One", "B000000001", 3600*1.05+121, e.now, "web-1") // past an hour's runtime
	e.srv.add("A/Other", "B000000002", 900, e.now, "web-1")         // not the title's book
	e.srv.add("A/Three", "B000000003", 900, e.now, "web-1")         // never listed
	e.read(p)                                                       // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 0 {
		t.Fatalf("a guarded place was written: %v", got)
	}
	// Allowed: within the runtime's slack it is written.
	e.srv.add("A/One", "B000000001", 3600*1.05+60, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 {
		t.Fatalf("sets = %v", got)
	}
	// Don't sync this book: the next sync lists it no longer; a place waiting
	// is dropped, and a new one isn't taken.
	e.srv.add("A/Two", "B000000002", 900, e.now, "web-1")
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	e.list(t, st("B000000001", "A/One", 3600))
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 {
		t.Fatalf("an unlisted title was written: %v", got)
	}
}

// The first pass reads the whole feed but writes no history: only places
// changed in the last baselineWindow wait to be written. The cursor is kept,
// so the next pass reads from it.
func TestPushBaselineSkipsHistory(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000), st("B000000002", "A/Two", 36000))
	e.srv.pageSize = 1 // a page a row: the pass follows more
	e.srv.add("A/One", "B000000001", 600, e.now.Add(-24*time.Hour), "web-1")
	e.srv.add("A/Two", "B000000002", 900, e.now.Add(-2*time.Minute), "web-1")
	p := NewPusher(e.o)
	p.Tick(context.Background())
	e.now = e.now.Add(3 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B000000002@900000" {
		t.Fatalf("sets = %v", got)
	}
	a := e.account(t)
	if a.PushCursor != "c2" {
		t.Fatalf("cursor = %q", a.PushCursor)
	}
	if last := e.srv.feedAsks[len(e.srv.feedAsks)-1]; last != "c2" {
		t.Fatalf("the second pass asked from %q", last)
	}
}

// The baseline reads a row's updated_at against the server's clock (the feed's
// server_time, bridge#50): with the Bridge's clock an hour ahead, a place the
// server changed 2 min ago still waits to be written and one from yesterday
// stays history; with the Bridge's clock alone (a server before 2.7.0 sends
// none) the recent one would read as history.
func TestPushBaselineReadsTheServersClock(t *testing.T) {
	for name, c := range map[string]struct {
		serverTime bool
		want       []string
	}{
		"server_time":                      {true, []string{"B000000002@900000"}},
		"no server_time (an older server)": {false, nil},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.list(t, st("B000000001", "A/One", 36000), st("B000000002", "A/Two", 36000))
			server := e.now.Add(-time.Hour) // the Bridge's clock is an hour ahead
			if c.serverTime {
				e.srv.clock = server
			}
			e.srv.add("A/One", "B000000001", 600, server.Add(-24*time.Hour), "web-1")
			e.srv.add("A/Two", "B000000002", 900, server.Add(-2*time.Minute), "web-1")
			p := NewPusher(e.o)
			p.Tick(context.Background())
			e.now = e.now.Add(3 * time.Minute)
			p.Tick(context.Background())
			if got := e.sets(); !slices.Equal(got, c.want) {
				t.Fatalf("sets = %v, want %v", got, c.want)
			}
		})
	}
}

// Denied: a refused account (the "Sign in again" state) neither reads the feed
// nor writes; an account the server answers sync_off for is left for a while.
// A failure writing to Audible that asks to wait keeps the place and records
// the wait, never a refusal.
func TestPushRefusedOffAndBackoff(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	p := NewPusher(e.o)
	if err := e.db.RefuseAudibleAccount(context.Background(), acctID, store.RefusalDevice, "refused", e.now); err != nil {
		t.Fatal(err)
	}
	p.Tick(context.Background())
	if len(e.srv.feedAsks) != 0 {
		t.Fatalf("a refused account read the feed: %v", e.srv.feedAsks)
	}
	if err := e.db.SignInAudibleAccount(context.Background(), e.account(t), e.now); err != nil {
		t.Fatal(err)
	}

	e.srv.feedErr = link.ErrSyncOff
	p.Tick(context.Background())
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if len(e.srv.feedAsks) != 1 {
		t.Fatalf("a sync-off account was asked again at once: %v", e.srv.feedAsks)
	}
	if a := e.account(t); a.PushError != "" {
		t.Fatalf("sync off is no error: %+v", a)
	}
	e.srv.feedErr = nil
	e.now = e.now.Add(offWait)
	e.started(t, p)

	e.srv.add("A/One", "B000000001", 600, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	e.accts.setErr = audibleAnswer(t, http.StatusServiceUnavailable, "900")
	p.Tick(context.Background())
	a := e.account(t)
	if a.RefusedAt != "" || !strings.Contains(a.PushError, "wait") || a.SyncRetryAt != store.Timestamp(e.now.Add(15*time.Minute)) {
		t.Fatalf("after a 503: %+v", a)
	}
	if x, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000001"); x.WantMs == nil {
		t.Fatal("the place was dropped on a wait")
	}
	// While it waits nothing is asked; after, the place is written.
	e.accts.setErr = nil
	asks := len(e.srv.feedAsks)
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if len(e.srv.feedAsks) != asks {
		t.Fatal("a waiting account read the feed")
	}
	e.now = e.now.Add(15 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 {
		t.Fatalf("sets = %v", got)
	}
	if a := e.account(t); a.PushError != "" || a.SyncRetryAt != "" {
		t.Fatalf("after the write: %+v", a)
	}

	// Another HTTP refusal drops the place (not tried every minute) and
	// records why; it never refuses the account.
	e.srv.add("A/One", "B000000001", 2000, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	e.accts.setErr = audibleAnswer(t, http.StatusForbidden, "")
	p.Tick(context.Background())
	x, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000001")
	if a := e.account(t); a.RefusedAt != "" || a.PushError == "" || x.WantMs != nil {
		t.Fatalf("after a 403: %+v, %+v", a, x)
	}
}

// A person the server can't sync for pauses the account and records why.
func TestPushUserUnavailable(t *testing.T) {
	e := newEnv(t)
	e.srv.feedErr = link.ErrUserUnavailable
	p := NewPusher(e.o)
	p.Tick(context.Background())
	if a := e.account(t); !errors.Is(e.srv.feedErr, link.ErrUserUnavailable) || a.PushError == "" || e.changed != 1 {
		t.Fatalf("account = %+v (changed %d)", a, e.changed)
	}
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if len(e.srv.feedAsks) != 1 {
		t.Fatalf("asked again at once: %v", e.srv.feedAsks)
	}
}

// A server with no progress feed (older than 2.6.0: link's ErrNoAudibleSync)
// pauses the account for an hour and says to update, rather than failing
// every minute, once it answers so twice in a row: the first 404 is asked
// again after errorWait with nothing recorded (a proxy's bare 404 while the
// server restarts reads the same).
func TestPushOldServer(t *testing.T) {
	e := newEnv(t)
	e.srv.feedErr = link.ErrNoAudibleSync
	p := NewPusher(e.o)
	p.Tick(context.Background())
	if a := e.account(t); a.PushError != "" || e.changed != 0 {
		t.Fatalf("after one 404: %+v (changed %d)", a, e.changed)
	}
	e.now = e.now.Add(errorWait)
	p.Tick(context.Background())
	if a := e.account(t); !strings.Contains(a.PushError, "update it to 2.6.0") || a.SyncRetryAt != "" || len(e.srv.feedAsks) != 2 {
		t.Fatalf("after two 404s: %+v, asks %v", a, e.srv.feedAsks)
	}
	e.now = e.now.Add(refusedWait + time.Minute)
	p.Tick(context.Background())
	if len(e.srv.feedAsks) != 2 {
		t.Fatalf("asked again within the hour: %v", e.srv.feedAsks)
	}
	e.now = e.now.Add(oldServerWait)
	p.Tick(context.Background())
	if len(e.srv.feedAsks) != 3 {
		t.Fatalf("not asked again after the hour: %v", e.srv.feedAsks)
	}
}

// A 404 between two answers that aren't (a server restarting behind a proxy:
// a 404, a 502, a 404) never pauses the push for the hour nor says to update
// it (allowed: the feed is read again within errorWait each time; denied: no
// old-server pause on 404s that aren't in a row).
func TestPushRestartingServerIsNotOld(t *testing.T) {
	e := newEnv(t)
	p := NewPusher(e.o)
	for i, err := range []error{link.ErrNoAudibleSync, &link.APIError{Status: http.StatusBadGateway},
		link.ErrNoAudibleSync, nil} {
		e.srv.feedErr = err
		p.Tick(context.Background())
		if len(e.srv.feedAsks) != i+1 {
			t.Fatalf("answer %d: asks %v", i, e.srv.feedAsks)
		}
		if a := e.account(t); strings.Contains(a.PushError, "update it to 2.6.0") {
			t.Fatalf("answer %d: %+v", i, a)
		}
		e.now = e.now.Add(errorWait)
	}
}

// Nothing runs while unpaired.
func TestPushUnpaired(t *testing.T) {
	e := newEnv(t)
	e.o.Server = func() Server { return nil }
	NewPusher(e.o).Tick(context.Background())
	if len(e.srv.feedAsks) != 0 {
		t.Fatal("an unpaired Bridge read the feed")
	}
}

// Furthest wins both ways: a place Audible moved further on since the Bridge
// last wrote or heard it (listening in the Audible app no sync has brought in
// yet, here during a sync off) isn't overwritten by an older AudioSilo place:
// the waiting place goes and the next sync sends Audible's. A move back made
// on AudioSilo while Audible stayed where the Bridge left it is written.
func TestPushLeavesAudibleFurtherOn(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	p := NewPusher(e.o)
	e.started(t, p)
	e.srv.add("A/One", "B000000001", 3000, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B000000001@3000000" {
		t.Fatalf("sets = %v", got)
	}

	// The person listens on in the Audible app to 5000 s, then plays a little
	// on AudioSilo from the stale 3000 s.
	e.accts.mu.Lock()
	e.accts.places["B000000001"] = audible.Position{PositionMs: 5_000_000, LastUpdated: e.now}
	e.accts.mu.Unlock()
	e.now = e.now.Add(time.Minute)
	e.srv.add("A/One", "B000000001", 3010, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 {
		t.Fatalf("Audible's further place was overwritten: %v", got)
	}
	if x, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000001"); x.WantMs != nil {
		t.Fatalf("the older place still waits: %+v", x)
	}
	res, err := e.runSync(t, params(t, title("B000000001", "A/One", 36000)))
	if err != nil || res.Applied != 1 || e.srv.server["A/One"] != 5000 {
		t.Fatalf("the sync = %+v, %v (server %v)", res, err, e.srv.server["A/One"])
	}

	// Audible is where the sync heard it; a move back on AudioSilo is written.
	e.now = e.now.Add(time.Minute)
	e.srv.add("A/One", "B000000001", 1200, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 2 || got[1] != "B000000001@1200000" {
		t.Fatalf("a move back wasn't written: %v", got)
	}
}

// A read Audible refuses partway (a 4xx after some batches) still keeps the
// places it gave before the refusal (bridge#50): one Audible moved further on
// isn't overwritten, and the titles it didn't answer are written as when
// Audible has no place.
func TestPushKeepsPlacesReadBeforeARefusal(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000), st("B000000002", "A/Two", 36000))
	p := NewPusher(e.o)
	e.started(t, p)
	e.srv.add("A/One", "B000000001", 3000, e.now, "web-1")
	e.srv.add("A/Two", "B000000002", 3000, e.now, "web-1")
	e.read(p)
	e.accts.mu.Lock()
	e.accts.places["B000000001"] = audible.Position{PositionMs: 5_000_000, LastUpdated: e.now}
	e.accts.places["B000000002"] = audible.Position{PositionMs: 5_000_000, LastUpdated: e.now}
	e.accts.readFirst = []string{"B000000001"}
	e.accts.placesErr = audibleAnswer(t, http.StatusBadRequest, "")
	e.accts.mu.Unlock()
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B000000002@3000000" {
		t.Fatalf("sets = %v, want only the title Audible didn't answer", got)
	}
}

// A place the sync applies (Audible was further on) ends the place waiting
// from the older server row, so the push never moves Audible back to it.
func TestPushAfterAppliedListen(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	p := NewPusher(e.o)
	e.started(t, p)
	e.srv.server["A/One"] = 3000
	e.srv.add("A/One", "B000000001", 3100, e.now, "web-1") // stopped behind Audible's place
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if x, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000001"); x.WantMs == nil {
		t.Fatal("no place waiting")
	}
	e.accts.mu.Lock()
	e.accts.places["B000000001"] = audible.Position{PositionMs: 4_000_000, LastUpdated: e.now.Add(-5 * time.Minute)}
	e.accts.mu.Unlock()
	res, err := e.runSync(t, params(t, title("B000000001", "A/One", 36000)))
	if err != nil || res.Applied != 1 {
		t.Fatalf("the sync = %+v, %v", res, err)
	}
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 0 {
		t.Fatalf("the place the sync replaced was written over Audible's: %v", got)
	}
}

// The two directions' markers end each other: once the sync hears Audible
// elsewhere, a move back to the place last written is no debounce; once the
// push writes, a move back to the place last heard is no echo. Both are
// written.
func TestPushMarkersSupersede(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	p := NewPusher(e.o)
	e.started(t, p)
	e.srv.add("A/One", "B000000001", 3000, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background()) // writes 3000 s
	// Audible plays on to 5000 s, and the sync brings it in.
	e.now = e.now.Add(time.Minute)
	e.accts.mu.Lock()
	e.accts.places["B000000001"] = audible.Position{PositionMs: 5_000_000, LastUpdated: e.now}
	e.accts.mu.Unlock()
	if res, err := e.runSync(t, params(t, title("B000000001", "A/One", 36000))); err != nil || res.Applied != 1 {
		t.Fatalf("the sync = %+v, %v", res, err)
	}
	// Back on AudioSilo to within 5 s of the place written before.
	e.now = e.now.Add(time.Minute)
	e.srv.add("A/One", "B000000001", 3002, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 2 || got[1] != "B000000001@3002000" {
		t.Fatalf("a move back to the old write was taken for the debounce: %v", got)
	}
	// Now the place heard (5000 s) is no longer Audible's: moving to it on
	// AudioSilo is written too.
	e.now = e.now.Add(time.Minute)
	e.srv.add("A/One", "B000000001", 5001, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 3 || got[2] != "B000000001@5001000" {
		t.Fatalf("a move to the old place heard was taken for Audible's own: %v", got)
	}
}

// Audible's answers: a license it denies (no content reference) drops the
// place and isn't asked again every minute (a device denied every license
// leaves the account for a while); a 5xx keeps the place for a later pass (no
// wait for the sync); a cached content reference Audible no longer takes is
// read again and the place written in the same pass; a wait is counted from
// when the 429 came back, after the client's own waits.
func TestPushAudibleAnswers(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	p := NewPusher(e.o)
	e.started(t, p)

	e.accts.acrErr = fmt.Errorf("%w: no content reference (acr) returned for B000000001", audible.ErrNoRights)
	e.srv.add("A/One", "B000000001", 600, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	for range 3 {
		p.Tick(ctx)
		e.now = e.now.Add(time.Minute)
	}
	if x, _ := e.db.GetAudiblePush(ctx, acctID, "B000000001"); x.WantMs != nil || e.accts.acrs != 1 {
		t.Fatalf("a denied license: %+v (%d license requests)", x, e.accts.acrs)
	}
	e.accts.acrErr = fmt.Errorf("%w: no content reference (acr) returned for B000000001", audible.ErrThrottled)
	e.srv.add("A/One", "B000000001", 700, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(ctx)
	asks := len(e.srv.feedAsks)
	e.now = e.now.Add(time.Minute)
	p.Tick(ctx)
	if a := e.account(t); len(e.srv.feedAsks) != asks || a.RefusedAt != "" || e.accts.acrs != 2 {
		t.Fatalf("a throttled device: asked %d -> %d, %+v (%d license requests)", asks, len(e.srv.feedAsks), a, e.accts.acrs)
	}
	e.accts.acrErr = nil
	e.now = e.now.Add(refusedWait)

	// A 502: kept, left for errorWait (the sync isn't held), then written.
	e.accts.setErr = audibleAnswer(t, http.StatusBadGateway, "")
	e.srv.add("A/One", "B000000001", 800, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(ctx)
	if x, _ := e.db.GetAudiblePush(ctx, acctID, "B000000001"); x.WantMs == nil {
		t.Fatal("a 502 dropped the place")
	}
	if a := e.account(t); a.PushError == "" || a.SyncRetryAt != "" {
		t.Fatalf("after a 502: %+v", a)
	}
	e.accts.setErr = nil
	e.now = e.now.Add(errorWait)
	p.Tick(ctx)
	if got := e.sets(); len(got) != 1 || got[0] != "B000000001@800000" {
		t.Fatalf("sets = %v", got)
	}

	// A stale cached content reference: read again, written in this pass.
	if err := e.db.SetAudibleACR(ctx, acctID, "B000000001", "CR!OLD"); err != nil {
		t.Fatal(err)
	}
	e.accts.wrongACR = audibleAnswer(t, http.StatusNotFound, "")
	acrs := e.accts.acrs
	e.srv.add("A/One", "B000000001", 900, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(ctx)
	if got := e.sets(); len(got) != 2 || got[1] != "B000000001@900000" || e.accts.acrs != acrs+1 {
		t.Fatalf("a stale content reference: sets %v (%d license requests)", got, e.accts.acrs-acrs)
	}

	// The 429 comes back 3 minutes into the call (the client's own waits):
	// the wait runs from then.
	e.accts.setErr = audibleAnswer(t, http.StatusTooManyRequests, "600")
	e.accts.onSet = func() { e.now = e.now.Add(3 * time.Minute) }
	e.srv.add("A/One", "B000000001", 1000, e.now, "web-1")
	e.read(p) // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(ctx)
	if a := e.account(t); a.SyncRetryAt != store.Timestamp(e.now.Add(10*time.Minute)) {
		t.Fatalf("wait until %q, want %q", a.SyncRetryAt, store.Timestamp(e.now.Add(10*time.Minute)))
	}
}

// A feed row on a title the last sync didn't list for that book (a new match,
// a re-match) is kept pending, never skipped past: the next sync lists it and
// the place is written.
func TestPushPendingTitle(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	p := NewPusher(e.o)
	e.started(t, p)
	e.srv.add("A/Two", "B000000002", 900, e.now, "web-1")          // a title matched since the last sync
	e.srv.add("A/One (right)", "B000000001", 1500, e.now, "web-1") // a title matched again
	e.read(p)                                                      // the Bridge reads the rows: they are still from now
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 0 {
		t.Fatalf("a title not listed yet was written: %v", got)
	}
	e.list(t, st("B000000001", "A/One (right)", 36000), st("B000000002", "A/Two", 36000))
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 2 || got[0] != "B000000001@1500000" || got[1] != "B000000002@900000" {
		t.Fatalf("sets = %v", got)
	}
}

// The server asking the push to wait (its own 429/503) pauses the push only:
// it is no wait for Audible, so the sync isn't held.
func TestPushServerWait(t *testing.T) {
	e := newEnv(t)
	p := NewPusher(e.o)
	e.srv.feedErr = &link.APIError{Status: http.StatusServiceUnavailable, Code: "unavailable", RetryAfter: 10 * time.Minute}
	p.Tick(context.Background())
	if a := e.account(t); a.SyncRetryAt != "" || a.PushError == "" {
		t.Fatalf("after the server's 503: %+v", a)
	}
	e.now = e.now.Add(5 * time.Minute)
	p.Tick(context.Background())
	if len(e.srv.feedAsks) != 1 {
		t.Fatalf("the server's wait wasn't kept: %v", e.srv.feedAsks)
	}
	// The feed back with nothing to write: the push's error ends (no push
	// would clear it, nor the job's success).
	e.srv.feedErr = nil
	e.now = e.now.Add(10 * time.Minute)
	changed := e.changed
	p.Tick(context.Background())
	if a := e.account(t); len(e.srv.feedAsks) != 2 || a.PushError != "" || e.changed != changed+1 {
		t.Fatalf("after the feed came back: %+v, asks %v (changed %d)", a, e.srv.feedAsks, e.changed-changed)
	}
}

// The feed pages in the order the server wrote rows, so a row comes again
// with its updated_at unchanged (an edit, a diverged place applied): a new
// place is new by its place alone, and its stillness counts from when
// the Bridge first read it (its own clock), never the row's time. The same
// row read again, or read again at its place with another version (its finish
// or length changed), neither restarts the wait nor writes again, before the
// push or after it.
func TestPushRowSentAgain(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	p := NewPusher(e.o)
	e.started(t, p)
	old := e.now.Add(-time.Hour) // a time far behind the feed's start
	e.srv.add("A/One", "B000000001", 600, old, "web-1")
	e.read(p)
	e.now = e.now.Add(2 * time.Minute)
	// The same row and the same place with a new version: the wait runs on.
	e.srv.resend(0, func(*link.AudibleProgressRow) {})
	e.srv.resend(0, func(r *link.AudibleProgressRow) { r.Version, r.Duration = 50, 36000 })
	p.Tick(context.Background())
	if len(e.sets()) != 0 {
		t.Fatalf("written before 3 minutes still: %v", e.sets())
	}
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B000000001@600000" {
		t.Fatalf("sets = %v (a row an hour old by its time, new to the feed)", got)
	}
	// Written again with its updated_at unchanged and a new place: new, and
	// still from the Bridge's read of it, not from the hour-old time.
	e.srv.resend(0, func(r *link.AudibleProgressRow) { r.Version, r.Position = 51, 900 })
	e.read(p)
	e.now = e.now.Add(2 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 {
		t.Fatalf("a place read 2 minutes ago was written: %v", got)
	}
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 2 || got[1] != "B000000001@900000" {
		t.Fatalf("a place written again with its old time wasn't pushed: %v", got)
	}
	// Read again unchanged, then at the same place with a new version (a
	// finish, a length): no new place, nothing more.
	e.srv.resend(0, func(r *link.AudibleProgressRow) { r.Version, r.Position = 51, 900 })
	e.srv.resend(0, func(r *link.AudibleProgressRow) { r.Version, r.Position, r.Finished = 52, 900, true })
	for range 2 {
		e.now = e.now.Add(4 * time.Minute)
		p.Tick(context.Background())
	}
	if got := e.sets(); len(got) != 2 {
		t.Fatalf("a row at the place written was written again: %v", got)
	}
}

// The baseline is the first read of the feed TO ITS END: one that stops at
// maxPages carries on at the next pass with history still history, and only
// then the feed's start is set. After it, a row is new whatever its
// updated_at (an offline backlog uploaded late is pushed).
func TestPushBaselineAcrossPasses(t *testing.T) {
	e := newEnv(t)
	e.srv.pageSize = 1
	var titles []store.AudibleSyncTitle
	for i := range maxPages + 5 {
		asin := fmt.Sprintf("B%09d", i+1)
		titles = append(titles, st(asin, "A/"+asin, 36000))
		e.srv.add("A/"+asin, asin, 600, e.now.Add(-24*time.Hour), "web-1")
	}
	e.list(t, titles...)
	p := NewPusher(e.o)
	p.Tick(context.Background())
	if a := e.account(t); a.PushFrom != "" || a.PushCursor != fmt.Sprintf("c%d", maxPages) {
		t.Fatalf("a baseline cut short = cursor %q from %q", a.PushCursor, a.PushFrom)
	}
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if a := e.account(t); a.PushFrom == "" || a.PushCursor != fmt.Sprintf("c%d", maxPages+5) {
		t.Fatalf("the baseline's end = cursor %q from %q", a.PushCursor, a.PushFrom)
	}
	if w, _ := e.db.AudibleWaiting(context.Background(), acctID); len(w) != 0 {
		t.Fatalf("history waits to be pushed: %+v", w)
	}
	// A place listened to offline yesterday, uploaded now: pushed.
	e.srv.add("A/B000000001", "B000000001", 1200, e.now.Add(-20*time.Hour), "phone-1")
	e.read(p)
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B000000001@1200000" {
		t.Fatalf("a late row = %v", got)
	}
}

// The push takes the server's feed as the judge of a book's length: the feed
// leaves out a book whose length disagrees with the title's runtime
// (duration_mismatch, by the indexed book's length), so a row's own duration
// (a book rescanned to the right length, a row written before it) never stops
// a push. Only the place against Audible's runtime does (TestPushGuards).
func TestPushIgnoresRowDuration(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000), st("B000000002", "A/Two", 36000))
	e.srv.duration = map[string]float64{"A/One": 7200, "A/Two": 0}
	p := NewPusher(e.o)
	e.started(t, p)
	e.srv.add("A/One", "B000000001", 600, e.now, "web-1")
	e.srv.add("A/Two", "B000000002", 600, e.now, "web-1")
	e.read(p)
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 2 || got[0] != "B000000001@600000" || got[1] != "B000000002@600000" {
		t.Fatalf("sets = %v", got)
	}
}

// failWant is the store with WantAudiblePush failing while fail is set.
type failWant struct {
	*store.DB
	fail bool
}

func (f *failWant) WantAudiblePush(ctx context.Context, account, asin string, row store.AudibleRow) error {
	if f.fail {
		return errors.New("database is locked")
	}
	return f.DB.WantAudiblePush(ctx, account, asin, row)
}

// A row the push failed to record keeps the feed's cursor where it was: the
// next pass reads it again and the place is written, never skipped past.
func TestPushRowNotRecordedIsReadAgain(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 3600))
	db := &failWant{DB: e.db}
	e.o.DB = db
	p := NewPusher(e.o)
	e.started(t, p)
	cursor := e.account(t).PushCursor

	db.fail = true
	e.srv.add("A/One", "B000000001", 600, e.now, "web-1")
	e.read(p)
	if a := e.account(t); a.PushCursor != cursor {
		t.Fatalf("the cursor moved past a row not recorded: %q -> %q", cursor, a.PushCursor)
	}
	db.fail = false
	e.read(p)
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B000000001@600000" {
		t.Fatalf("sets = %v", got)
	}
}

// A cursor the server refuses (400) is not sent again every pass, stopping the
// push for good: the push reads the feed again from its start, and a place
// changed meanwhile is still written. A 400 with no cursor is a failure like
// any other.
func TestPushRejectedCursorReadsFromTheStart(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 3600))
	p := NewPusher(e.o)
	e.started(t, p)
	if err := e.db.SetAudiblePushCursor(context.Background(), acctID, "c99", time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.srv.badSince = "c99"
	e.srv.add("A/One", "B000000001", 600, e.now, "web-1")

	e.read(p)
	if a := e.account(t); a.PushCursor != "" || a.PushFrom == "" || !a.PushReread {
		t.Fatalf("after the refused cursor: cursor %q from %q reread %v", a.PushCursor, a.PushFrom, a.PushReread)
	}
	e.read(p)
	if a := e.account(t); a.PushCursor != "c1" || a.PushReread {
		t.Fatalf("the read from the start left cursor %q reread %v", a.PushCursor, a.PushReread)
	}
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 || got[0] != "B000000001@600000" {
		t.Fatalf("sets = %v", got)
	}
	if asks := e.srv.feedAsks; len(asks) < 3 || asks[1] != "c99" || asks[2] != "" {
		t.Fatalf("feed asks = %q, want the refused cursor then the start", asks)
	}

	// No cursor to blame: the 400 is recorded as the push's failure.
	e2 := newEnv(t)
	e2.srv.feedErr = &link.APIError{Status: http.StatusBadRequest, Code: "invalid_request"}
	NewPusher(e2.o).Tick(context.Background())
	if a := e2.account(t); a.PushError == "" || a.PushCursor != "" {
		t.Fatalf("a 400 on the feed's start: %+v", a)
	}
}

// The read from the start after a refused cursor keeps the account's baseline:
// a row changed before it is history again whatever place it has now, so a
// server restored from a backup (its cursor refused, its rows at older places
// than the ones the Bridge read and wrote to Audible) has none of them written
// over Audible's. That holds over the passes the read takes; a place changed
// since the baseline is still written, and once the read reached the feed's end
// a row is new by its place alone again.
func TestPushRejectedCursorKeepsTheBaseline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000), st("B000000002", "A/Two", 36000), st("B000000003", "A/Three", 36000))
	p := NewPusher(e.o)
	e.started(t, p)
	baseline := e.now
	// The Bridge reads 5 h on One and writes it to Audible.
	e.srv.add("A/One", "B000000001", 18000, e.now, "web-1")
	e.read(p)
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(ctx)
	if got := e.sets(); len(got) != 1 || got[0] != "B000000001@18000000" {
		t.Fatalf("sets = %v", got)
	}
	e.accts.mu.Lock()
	e.accts.places["B000000001"] = audible.Position{PositionMs: 18_000_000}
	e.accts.mu.Unlock()

	// A day on the server answers from a backup: the cursor is refused, and
	// from the start the feed has One at 3 h and Three at 1 h, both from
	// before the baseline, then Two moved a minute ago.
	e.now = e.now.Add(24 * time.Hour)
	e.srv.mu.Lock()
	e.srv.badSince = e.account(t).PushCursor
	e.srv.rows = nil
	e.srv.pageSize = 1
	e.srv.mu.Unlock()
	e.srv.add("A/One", "B000000001", 10800, baseline.Add(-48*time.Hour), "web-1")
	e.srv.add("A/Three", "B000000003", 3600, baseline.Add(-time.Hour), "web-1")
	e.srv.add("A/Two", "B000000002", 900, e.now.Add(-time.Minute), "web-1")
	e.read(p) // refused: back to the start
	e.srv.mu.Lock()
	e.srv.badSince = "" // the stub's cursors are the same ones again
	e.srv.mu.Unlock()

	// A row not recorded keeps the read at the start for another pass, still
	// against the baseline.
	db := &failWant{DB: e.db, fail: true}
	p.o.DB = db
	e.read(p)
	if a := e.account(t); a.PushCursor != "" || !a.PushReread {
		t.Fatalf("a read from the start cut short: cursor %q reread %v", a.PushCursor, a.PushReread)
	}
	db.fail = false
	e.read(p)
	if a := e.account(t); a.PushCursor != "c3" || a.PushReread {
		t.Fatalf("the read from the start's end: cursor %q reread %v", a.PushCursor, a.PushReread)
	}
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(ctx)
	if got := e.sets(); len(got) != 2 || got[1] != "B000000002@900000" {
		t.Fatalf("sets = %v, want only the place changed since the baseline", got)
	}
	for asin, ms := range map[string]int64{"B000000001": 10_800_000, "B000000003": 3_600_000} {
		if x, err := e.db.GetAudiblePush(ctx, acctID, asin); err != nil || x.WantMs != nil || x.RowMs != ms {
			t.Fatalf("%s after the read = %+v (%v), want its row recorded with no place waiting", asin, x, err)
		}
	}

	// The read is over: a row the server writes again is new by its place,
	// whatever its updated_at.
	e.srv.resend(1, func(r *link.AudibleProgressRow) { r.Position = 7200 })
	e.read(p)
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(ctx)
	if got := e.sets(); len(got) != 3 || got[2] != "B000000003@7200000" {
		t.Fatalf("sets = %v, want the place written after the read", got)
	}
}

// A read from the start after a refused cursor never writes back to Audible a
// place the server holds from before the one this Bridge last read or wrote
// for that title (bridge#50): a server restored from a backup taken after the
// baseline answers a row changed since the baseline, but older than the place
// handled since. A title's place written on the server after the one handled
// is still new, the two clocks compared through the page's server_time (here
// the server's is two hours behind the Bridge's).
func TestPushRereadSkipsPlacesOlderThanKnown(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000), st("B000000002", "A/Two", 36000))
	const skew = -2 * time.Hour
	clock := func(d time.Duration) {
		e.now = e.now.Add(d)
		e.srv.mu.Lock()
		e.srv.clock = e.now.Add(skew)
		e.srv.mu.Unlock()
	}
	clock(0)
	p := NewPusher(e.o)
	e.started(t, p)

	// An hour on, One at 5 h and Two at 1 h are read and written.
	clock(time.Hour)
	e.srv.add("A/One", "B000000001", 18000, e.now.Add(skew), "web-1")
	e.srv.add("A/Two", "B000000002", 3600, e.now.Add(skew), "web-1")
	backup := append([]link.AudibleProgressRow(nil), e.srv.rows...) // the backup is taken now
	e.read(p)
	clock(4 * time.Minute)
	p.Tick(ctx)
	// An hour later One moves on to 6 h, read and written too.
	clock(time.Hour)
	e.srv.add("A/One", "B000000001", 21600, e.now.Add(skew), "web-1")
	e.read(p)
	clock(4 * time.Minute)
	p.Tick(ctx)
	if got := e.sets(); strings.Join(got, " ") != "B000000001@18000000 B000000002@3600000 B000000001@21600000" {
		t.Fatalf("sets before the restore = %v", got)
	}

	// The server is restored from the backup and refuses the cursor; Two is
	// listened to on it since, to 2 h.
	clock(30 * time.Minute)
	e.srv.mu.Lock()
	e.srv.badSince = e.account(t).PushCursor
	e.srv.rows = backup
	e.srv.mu.Unlock()
	e.srv.add("A/Two", "B000000002", 7200, e.now.Add(skew), "web-1")
	e.read(p) // refused: back to the start
	if a := e.account(t); a.PushCursor != "" || !a.PushReread {
		t.Fatalf("after the refusal: cursor %q reread %v", a.PushCursor, a.PushReread)
	}
	e.srv.mu.Lock()
	e.srv.badSince = ""
	e.srv.mu.Unlock()
	e.read(p)
	clock(4 * time.Minute)
	p.Tick(ctx)
	if got := e.sets(); len(got) != 4 || got[3] != "B000000002@7200000" {
		t.Fatalf("sets after the restore = %v, want Two's new place and not One's older one", got)
	}
	if x, err := e.db.GetAudiblePush(ctx, acctID, "B000000001"); err != nil || x.WantMs != nil || x.RowMs != 18_000_000 {
		t.Fatalf("One after the read = %+v (%v), want the server's row recorded with no place waiting", x, err)
	}
}

// A server that refuses even the cursors it gave is not read from the start
// every pass: after resetStrikes refusals in a row the account waits
// resetWait, logged once and recorded as the push's failure, until a pass
// reads its pages with none (bridge#50).
func TestPushRefusedCursorBacksOff(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	var logs strings.Builder
	e.o.Log = slog.New(slog.NewTextHandler(&logs, nil))
	e.list(t, st("B000000001", "A/One", 36000))
	e.srv.pageSize = 1
	e.srv.refuseAll = true
	e.srv.add("A/One", "B000000001", 600, e.now, "web-1")
	e.srv.add("A/One", "B000000001", 900, e.now, "web-1")
	p := NewPusher(e.o)
	asks := func() int {
		e.srv.mu.Lock()
		defer e.srv.mu.Unlock()
		return len(e.srv.feedAsks)
	}
	for range resetStrikes {
		p.Tick(ctx) // the start, then the next page's cursor refused
	}
	if n := asks(); n != 2*resetStrikes {
		t.Fatalf("feed asks = %d, want %d", n, 2*resetStrikes)
	}
	if a := e.account(t); a.PushError == "" || a.PushCursor != "" {
		t.Fatalf("after %d refusals: %+v", resetStrikes, a)
	}
	p.Tick(ctx)
	if n := asks(); n != 2*resetStrikes {
		t.Fatalf("a pass while waiting asked the feed (%d asks)", n)
	}
	e.now = e.now.Add(resetWait)
	p.Tick(ctx) // refused again: waits again, not logged again
	p.Tick(ctx)
	if n := asks(); n != 2*resetStrikes+2 {
		t.Fatalf("feed asks after the wait = %d", n)
	}
	if n := strings.Count(logs.String(), "refused the push's cursor"); n != 1 {
		t.Fatalf("the first refusal logged %d times:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "keeps refusing"); n != 1 {
		t.Fatalf("the waits logged %d times:\n%s", n, logs.String())
	}

	// The server takes its cursors again: the read reaches the end and the
	// run is over, so a later refusal is the first again.
	e.srv.mu.Lock()
	e.srv.refuseAll = false
	e.srv.mu.Unlock()
	e.now = e.now.Add(resetWait)
	p.Tick(ctx)
	if a := e.account(t); a.PushCursor != "c2" || a.PushReread {
		t.Fatalf("the read after the waits: cursor %q reread %v", a.PushCursor, a.PushReread)
	}
	p.mu.Lock()
	run := p.resets[acctID]
	p.mu.Unlock()
	if run != 0 {
		t.Fatalf("refusals in a row after a clean read = %d", run)
	}
}

// The line a read from the start after a refused cursor draws is the
// baseline's end (push_from, the Bridge's clock) moved onto the server's clock
// by the page's server_time, less the window; with no server_time it stays on
// the Bridge's. The baseline's own read counts back from server_time.
func TestHistoryCutoffClocks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	from := now.Add(-24 * time.Hour)
	behind := now.Add(-2 * time.Hour).Format(time.RFC3339)
	reread := store.AudibleAccount{PushFrom: store.Timestamp(from), PushReread: true}
	for name, c := range map[string]struct {
		a          store.AudibleAccount
		serverTime string
		want       time.Time
	}{
		"reread, server behind":    {reread, behind, from.Add(-2*time.Hour - baselineWindow)},
		"reread, no server_time":   {reread, "", from.Add(-baselineWindow)},
		"baseline":                 {store.AudibleAccount{}, behind, now.Add(-2*time.Hour - baselineWindow)},
		"after the baseline's end": {store.AudibleAccount{PushFrom: store.Timestamp(from)}, behind, time.Time{}},
	} {
		if got := historyCutoff(c.a, serverSkew(c.serverTime, now), now); !got.Equal(c.want) {
			t.Errorf("%s: cutoff %v, want %v", name, got, c.want)
		}
	}
}

// A server refusing even the cursors it gave, with a feed that fits a page:
// each read from the start sends no cursor, so it doesn't end the run of
// refusals, and the account still waits resetWait after resetStrikes of them
// (bridge#50).
func TestPushRefusedCursorBacksOffOnAPage(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	e.srv.refuseAll = true
	e.srv.add("A/One", "B000000001", 600, e.now, "web-1")
	p := NewPusher(e.o)
	for range 2 * resetStrikes {
		p.Tick(ctx) // the start (its cursor kept), then that cursor refused
	}
	e.srv.mu.Lock()
	asks := len(e.srv.feedAsks)
	e.srv.mu.Unlock()
	if asks != 2*resetStrikes {
		t.Fatalf("feed asks = %d, want %d", asks, 2*resetStrikes)
	}
	if !p.isPaused(acctID, e.now) {
		t.Fatalf("not waiting after %d refusals with reads from the start between", resetStrikes)
	}
	if a := e.account(t); a.PushError == "" {
		t.Fatalf("no push error after %d refusals: %+v", resetStrikes, a)
	}

	// After the wait the read from the start ends the push's error, and the
	// next refusal records it again with another wait: the console never shows
	// a healthy push while it waits.
	e.now = e.now.Add(resetWait)
	p.Tick(ctx)
	if a := e.account(t); a.PushError != "" {
		t.Fatalf("the clean read after the wait kept the error: %+v", a)
	}
	p.Tick(ctx)
	if a := e.account(t); a.PushError == "" || !p.isPaused(acctID, e.now) {
		t.Fatalf("a refusal after the wait: paused %v, %+v", p.isPaused(acctID, e.now), a)
	}
}

// olderThanKnown compares a row's updated_at (the server's clock) with when
// the Bridge last read the title's place (row_seen, its own clock) moved by
// the skew either way, and never with when it wrote the place to Audible
// (at): a place another device wrote between the read and the write is new.
func TestOlderThanKnown(t *testing.T) {
	t.Parallel()
	seen := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	x := store.AudiblePush{RowSeen: store.Timestamp(seen), At: store.Timestamp(seen.Add(3 * time.Minute))}
	for name, c := range map[string]struct {
		updated time.Time
		skew    time.Duration
		want    bool
	}{
		"before the read":                {seen.Add(-time.Minute), 0, true},
		"between the read and the write": {seen.Add(time.Minute), 0, false},
		"server ahead, before":           {seen.Add(time.Hour - time.Minute), time.Hour, true},
		"server ahead, after":            {seen.Add(time.Hour + time.Minute), time.Hour, false},
		"server behind, before":          {seen.Add(-time.Hour - time.Minute), -time.Hour, true},
		"server behind, after":           {seen.Add(-time.Hour + time.Minute), -time.Hour, false},
	} {
		if got := olderThanKnown(c.updated, x, c.skew); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if olderThanKnown(seen.Add(-time.Hour), store.AudiblePush{At: x.At}, 0) {
		t.Error("a title never read counts as handled")
	}
}

// The skew is taken when the page comes back, not at the pass's start: time
// spent before the read (other accounts, a slow server) is no lead of the
// server's clock, so a read from the start after a refused cursor still writes
// a place changed after the one the Bridge last read (bridge#50).
func TestPushRereadSkewAtTheRead(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	sync := func() {
		e.srv.mu.Lock()
		e.srv.clock = e.now
		e.srv.mu.Unlock()
	}
	sync()
	p := NewPusher(e.o)
	e.started(t, p)

	e.now = e.now.Add(2 * time.Hour)
	sync()
	read := e.now
	e.srv.add("A/One", "B000000001", 18000, read, "web-1")
	e.read(p)
	e.now = e.now.Add(4 * time.Minute)
	sync()
	p.Tick(ctx)
	if got := e.sets(); len(got) != 1 || got[0] != "B000000001@18000000" {
		t.Fatalf("sets = %v", got)
	}

	// One moves on after the read; the cursor is then refused, so the place
	// is first read from the start, on a pass whose page comes back an hour
	// after it began.
	e.srv.add("A/One", "B000000001", 21600, read.Add(5*time.Minute), "web-1")
	e.srv.mu.Lock()
	e.srv.badSince = e.account(t).PushCursor
	e.srv.mu.Unlock()
	e.now = e.now.Add(time.Minute)
	sync()
	e.read(p) // refused: back to the start
	e.srv.mu.Lock()
	e.srv.badSince = ""
	e.srv.onFeed = func() {
		e.now = e.now.Add(time.Hour)
		e.srv.clock = e.now
		e.srv.onFeed = nil
	}
	e.srv.mu.Unlock()
	e.read(p)
	e.now = e.now.Add(4 * time.Minute)
	sync()
	p.Tick(ctx)
	if got := e.sets(); len(got) != 2 || got[1] != "B000000001@21600000" {
		t.Fatalf("sets = %v, want the place changed after the read written", got)
	}
}
