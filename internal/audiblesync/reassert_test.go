package audiblesync

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// pushed has the push write the server's place (seconds) for B000000001 on
// A/One, as a listener who stopped on AudioSilo would, and returns the pusher.
func (e *env) pushed(t *testing.T, placeS float64) *Pusher {
	t.Helper()
	e.list(t, st("B000000001", "A/One", 36000))
	p := NewPusher(e.o)
	e.started(t, p)
	e.srv.server["A/One"] = placeS
	e.srv.add("A/One", "B000000001", placeS, e.now, "web-1")
	e.read(p)
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 {
		t.Fatalf("the place wasn't written: %v", got)
	}
	return p
}

// audibleAt sets Audible's place, as the Audible app uploading it would.
func (e *env) audibleAt(ms int64, at time.Time) {
	e.accts.mu.Lock()
	e.accts.places["B000000001"] = audible.Position{PositionMs: ms, LastUpdated: at}
	e.accts.mu.Unlock()
}

// bridge#51: the Audible app uploads the older place it still held over the
// one the Bridge wrote. The next sync hears it, the server answers behind
// (furthest wins), and the server's place is written to Audible again once it
// has been still for StillFor; after that Audible's place is the echo.
func TestSyncReassertsAPlaceAudibleOverwrote(t *testing.T) {
	e := newEnv(t)
	p := e.pushed(t, 5393.514)
	raw := params(t, title("B000000001", "A/One", 36000))

	e.now = e.now.Add(time.Minute)
	e.audibleAt(4_000_000, e.now) // chapter 4, newer than the write
	res, err := e.runSync(t, raw)
	if err != nil || res.Behind != 1 || res.Reasserted != 1 || e.srv.server["A/One"] != 5393.514 {
		t.Fatalf("the sync = %+v, %v (server %v)", res, err, e.srv.server["A/One"])
	}
	x, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000001")
	if x.WantMs == nil || *x.WantMs != 5_393_514 || x.HeardMs != 4_000_000 {
		t.Fatalf("push row = %+v", x)
	}

	// The same Audible place again re-arms nothing more (already heard).
	if res, err := e.runSync(t, raw); err != nil || res.Reasserted != 0 {
		t.Fatalf("again = %+v, %v", res, err)
	}

	// Not before it has been still for StillFor, then written once.
	e.now = e.now.Add(time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 {
		t.Fatalf("written before StillFor: %v", got)
	}
	e.now = e.now.Add(3 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 2 || got[1] != "B000000001@5393514" {
		t.Fatalf("sets = %v", got)
	}
	e.now = e.now.Add(5 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 2 {
		t.Fatalf("written again: %v", got)
	}
	// Audible now answers the place written: its echo, nothing to send.
	if res, err := e.runSync(t, raw); err != nil || res.SkippedEcho != 1 || res.Reasserted != 0 {
		t.Fatalf("after = %+v, %v", res, err)
	}
}

// Denied: a re-armed place is never written over Audible once it moved further
// on (the listener went on in the Audible app before the push came round): the
// push's read before the write drops it, and the next sync brings Audible's in.
func TestReassertNeverOverwritesAudibleFurtherOn(t *testing.T) {
	e := newEnv(t)
	p := e.pushed(t, 5393.514)
	e.now = e.now.Add(time.Minute)
	e.audibleAt(4_000_000, e.now)
	if res, err := e.runSync(t, params(t, title("B000000001", "A/One", 36000))); err != nil || res.Reasserted != 1 {
		t.Fatalf("the sync = %+v, %v", res, err)
	}
	e.now = e.now.Add(time.Minute)
	e.audibleAt(6_000_000, e.now) // further on than the server's place
	e.now = e.now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if got := e.sets(); len(got) != 1 {
		t.Fatalf("Audible's further place was overwritten: %v", got)
	}
	if x, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000001"); x.WantMs != nil {
		t.Fatalf("the place still waits: %+v", x)
	}
	if res, err := e.runSync(t, params(t, title("B000000001", "A/One", 36000))); err != nil || res.Applied != 1 ||
		e.srv.server["A/One"] != 6000 {
		t.Fatalf("the sync = %+v, %v (server %v)", res, err, e.srv.server["A/One"])
	}
}

// Denied: Audible behind a place the Bridge never wrote (the server's is
// ahead from listening elsewhere, no push yet) is not re-armed: only a place
// the push wrote is re-asserted. Nor is one once the server's row moved on
// since the push (that new place waits on its own).
func TestReassertOnlyAPlaceTheBridgeWrote(t *testing.T) {
	e := newEnv(t)
	e.list(t, st("B000000001", "A/One", 36000))
	e.srv.server["A/One"] = 5000
	e.audibleAt(4_000_000, e.now.Add(-time.Minute))
	res, err := e.runSync(t, params(t, title("B000000001", "A/One", 36000)))
	if err != nil || res.Behind != 1 || res.Reasserted != 0 {
		t.Fatalf("the sync = %+v, %v", res, err)
	}
	if x, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000001"); x.WantMs != nil {
		t.Fatalf("a place the Bridge never wrote was armed: %+v", x)
	}

	e2 := newEnv(t)
	p := e2.pushed(t, 5393.514)
	e2.srv.server["A/One"] = 6000
	e2.srv.add("A/One", "B000000001", 6000, e2.now, "web-1") // the listener went on
	e2.read(p)
	e2.audibleAt(4_000_000, e2.now)
	if res, err := e2.runSync(t, params(t, title("B000000001", "A/One", 36000))); err != nil || res.Reasserted != 0 {
		t.Fatalf("the sync = %+v, %v", res, err)
	}
	if x, _ := e2.db.GetAudiblePush(context.Background(), acctID, "B000000001"); x.WantMs == nil || *x.WantMs != 6_000_000 {
		t.Fatalf("push row = %+v", x)
	}
}

func TestOverwritten(t *testing.T) {
	x := store.AudiblePush{LastMs: 5_000_000, RowMs: 5_000_000}
	cases := []struct {
		name  string
		x     store.AudiblePush
		heard int64
		want  bool
	}{
		{"older place over the write", x, 4_000_000, true},
		{"within the echo", x, 4_996_000, false},
		{"never written", store.AudiblePush{LastMs: -1, RowMs: 5_000_000}, 4_000_000, false},
		{"no server row", store.AudiblePush{LastMs: 5_000_000, RowMs: -1}, 4_000_000, false},
		{"server moved since", store.AudiblePush{LastMs: 5_000_000, RowMs: 6_000_000}, 4_000_000, false},
		{"row within the debounce", store.AudiblePush{LastMs: 5_000_000, RowMs: 5_003_000}, 4_000_000, true},
	}
	for _, c := range cases {
		if got := overwritten(c.x, c.heard); got != c.want {
			t.Errorf("%s: overwritten = %v", c.name, got)
		}
	}
}
