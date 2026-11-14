package librivoxjob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
)

// stageAndStop leaves a fetch's staging folder and state: archive.org cuts
// section 3 every time (an outage keeps both).
func (x *harness) stageAndStop(t *testing.T) {
	t.Helper()
	x.fetch.files[sectionURL(3)].cut = []int{0, 0, 0}
	if _, err := x.run(context.Background(), false); err == nil {
		t.Fatal("the outage didn't fail the run")
	}
	x.fetch.files[sectionURL(3)].cut = nil
	if !x.exists(stagingDir) {
		t.Fatal("no staging folder")
	}
}

// age sets the fetch's state as last used d ago.
func (x *harness) age(t *testing.T, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d)
	if err := os.Chtimes(x.h.statePath(3, testID), at, at); err != nil {
		t.Fatal(err)
	}
}

func (x *harness) hasState() bool {
	_, err := os.Stat(x.h.statePath(3, testID))
	return err == nil
}

// The sweep removes an abandoned fetch's staging folder, the author folder it
// made and its state once unused for StagingMaxAge; a younger one stays.
func TestSweepRemovesAbandonedStaging(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t)
	x.age(t, StagingMaxAge-time.Hour)
	x.h.Sweep(time.Now())
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the sweep took a young fetch's staging")
	}
	x.age(t, StagingMaxAge+time.Hour)
	// A temp file a crash left, as old, goes too; a young one stays.
	old, young := filepath.Join(x.stateDir, ".3-192.json.tmp-1"), filepath.Join(x.stateDir, ".3-192.json.tmp-2")
	for _, p := range []string{old, young} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-StagingMaxAge - time.Hour)
	_ = os.Chtimes(old, at, at)
	x.h.Sweep(time.Now())
	if x.exists(stagingDir) || x.exists("L. Leslie Brooke") || x.hasState() {
		t.Fatal("the sweep left an abandoned fetch")
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("an old temp file stayed")
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatal("a young temp file went")
	}
}

// A run that resumes marks its state used, so a fetch retried after days
// (meta down again) isn't swept as abandoned.
func TestRunMarksStateUsed(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	x.meta.bookErr = metaops.ErrUnavailable
	if _, err := x.run(context.Background(), false); err == nil {
		t.Fatal("meta down didn't fail the run")
	}
	x.h.Sweep(time.Now())
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the sweep took a fetch a run just used")
	}
}

// The sweep leaves a fetch running here, a placed book (only its state goes),
// and one whose library isn't a writable mapping now (kept for later).
func TestSweepLeavesWhatItMustNot(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	k := key{lib: 3, id: testID}
	x.h.begin(k)
	x.h.Sweep(time.Now())
	x.h.end(k)
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the sweep took a running fetch's staging")
	}
	x.libs.writable = false
	x.h.Sweep(time.Now())
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the sweep dropped a fetch it can't reach")
	}
	x.libs.writable = true

	// Placed, the rescan failed, never retried: the book stays.
	x = newHarness(t)
	boom := errors.New("server busy")
	x.srv.rescanErr = []error{boom, boom, boom}
	if _, err := x.run(context.Background(), false); err == nil {
		t.Fatal("the rescan didn't fail")
	}
	x.age(t, StagingMaxAge+time.Hour)
	x.h.Sweep(time.Now())
	if !x.exists(bookDir+"/03 - Johnny Crow's New Garden.mp3") || x.hasState() {
		t.Fatal("the sweep took a placed book, or kept its state")
	}
}

// A state of another server is never acted on: Abandon and the sweep drop it
// without touching this server's same-numbered library, and a run plans
// afresh. A state from before the server id was kept counts as this server's.
func TestOtherServersStateNeverActedOn(t *testing.T) {
	x := newHarness(t)
	x.serverID = "server-a"
	x.stageAndStop(t)
	x.serverID = "server-b"
	x.h.Abandon(jobs.AbandonContext(context.Background(), false), x.job(true))
	if !x.exists(stagingDir) {
		t.Fatal("Abandon acted on another server's state")
	}
	if x.hasState() {
		t.Fatal("another server's state stayed")
	}

	x = newHarness(t)
	x.serverID = "server-a"
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	x.serverID = "server-b"
	x.h.Sweep(time.Now())
	if !x.exists(stagingDir) || x.hasState() {
		t.Fatal("the sweep acted on another server's state, or kept it")
	}

	// Allowed: this server's state, and one with no server id.
	for _, recorded := range []string{"server-b", ""} {
		x = newHarness(t)
		x.serverID = recorded
		x.stageAndStop(t)
		x.serverID = "server-b"
		x.h.Abandon(jobs.AbandonContext(context.Background(), false), x.job(true))
		if x.exists(stagingDir) || x.hasState() {
			t.Fatalf("recorded %q: Abandon left this server's staging", recorded)
		}
	}
}

// A second run of a fetch already running here (the same library and
// project: a re-add claimed while a cancelled parked run is still ending)
// waits for it to end; it fails when it doesn't within the bound, leaving the
// first's staging alone, as Abandon does.
func TestRunAlreadyRunning(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t)
	k := key{lib: 3, id: testID}
	x.h.begin(k)
	polls := 0
	x.h.o.Wait = func(ctx context.Context, _ time.Duration) error {
		polls++
		return ctx.Err()
	}
	if _, err := x.run(context.Background(), false); err == nil {
		t.Fatal("a second run of the same fetch ran")
	}
	if polls != awaitPolls {
		t.Fatalf("%d polls", polls)
	}
	x.h.Abandon(jobs.AbandonContext(context.Background(), false), x.job(true))
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the second run or Abandon touched the first's staging")
	}
	// The first run ends during the wait: the second goes on.
	polls = 0
	x.h.o.Wait = func(ctx context.Context, _ time.Duration) error {
		if polls++; polls == 3 {
			x.h.end(k)
		}
		return ctx.Err()
	}
	if _, err := x.run(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

// meta refusing for good (a 400, 403, 410) ends the fetch at once: read once,
// never retried, and the staging folder goes (nothing to resume).
func TestFetchMetaRefusedFailsFast(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t)
	reads := x.meta.reads
	x.meta.bookErr = metaops.ErrRefused
	out, err := x.run(context.Background(), false)
	if err == nil || !errors.Is(err, metaops.ErrRefused) || failureCode(t, out, err) != "" {
		t.Fatalf("%#v %v", out, err)
	}
	if x.meta.reads != reads+1 {
		t.Fatalf("%d reads: a refusal was retried", x.meta.reads-reads)
	}
	if x.exists(stagingDir) || x.hasState() {
		t.Fatal("a refusal kept the staging folder")
	}
}

func TestParseStateName(t *testing.T) {
	for name, want := range map[string]key{"3-192.json": {3, 192}, "12-7.json": {12, 7}} {
		if got, ok := parseStateName(name); !ok || got != want {
			t.Errorf("%s: %v %v", name, got, ok)
		}
	}
	for _, name := range []string{".3-192.json.tmp-1", "3-192", "x-1.json", "0-1.json", "3-0.json", "3.json", "-3-1.json"} {
		if _, ok := parseStateName(name); ok {
			t.Errorf("%s parsed", name)
		}
	}
}
