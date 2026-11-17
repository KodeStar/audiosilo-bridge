package librivoxjob

import (
	"context"
	"encoding/json"
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
	x.h.Sweep(time.Now(), nil)
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
	x.h.Sweep(time.Now(), nil)
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
	x.h.Sweep(time.Now(), nil)
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the sweep took a fetch a run just used")
	}
}

// A run that ends long after its state was last saved (parked for days, then
// meta down) leaves it marked used: the sweep goes by when the run ended.
func TestRunEndMarksStateUsed(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t)
	x.meta.bookErr = metaops.ErrUnavailable
	x.h.o.Wait = func(ctx context.Context, _ time.Duration) error {
		x.age(t, StagingMaxAge+time.Hour) // the run goes on, its state untouched for days
		return ctx.Err()
	}
	if _, err := x.run(context.Background(), false); err == nil {
		t.Fatal("meta down didn't fail the run")
	}
	x.h.Sweep(time.Now(), nil)
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the sweep took a fetch whose run just ended")
	}
}

// A fetch whose job the runner still holds (a recovered job waiting for its
// lane, one a refused token stopped) has no run here yet: the sweep leaves
// it, however old its state.
func TestSweepLeavesOpenJobs(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	other := json.RawMessage(`{"librivox_id":7,"work_id":"w","recording_id":"r","library_id":3}`)
	x.h.Sweep(time.Now(), []json.RawMessage{json.RawMessage(`{`), other, x.job(true).Params})
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the sweep took the fetch of an open job")
	}
	x.h.Sweep(time.Now(), []json.RawMessage{other})
	if x.exists(stagingDir) || x.hasState() {
		t.Fatal("the sweep left an abandoned fetch")
	}
}

// A staging folder that can't be removed now keeps its state, which is all
// that names it: the sweep and Abandon leave both for a later sweep.
func TestStateStaysWithStagingThatCantBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a read-only folder")
	}
	x := newHarness(t)
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	author := x.path("L. Leslie Brooke")
	if err := os.Chmod(author, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(author, 0o755) })
	x.h.Sweep(time.Now(), nil)
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the sweep dropped the state of a staging folder it couldn't remove")
	}
	x.h.Abandon(jobs.AbandonContext(context.Background(), false), x.job(true))
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("Abandon dropped the state of a staging folder it couldn't remove")
	}
	if err := os.Chmod(author, 0o755); err != nil {
		t.Fatal(err)
	}
	x.h.Sweep(time.Now(), nil)
	if x.exists(stagingDir) || x.exists("L. Leslie Brooke") || x.hasState() {
		t.Fatal("a later sweep left the fetch")
	}
}

// In the state folder the sweep removes old regular files only (never a
// folder), and a file whose name isn't a state's own never stands for one.
func TestSweepStateFolderStrays(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t) // a young fetch of library 3
	at := time.Now().Add(-StagingMaxAge - time.Hour)
	dir, alias := filepath.Join(x.stateDir, "kept"), filepath.Join(x.stateDir, "03-192.json")
	if err := os.MkdirAll(filepath.Join(dir, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alias, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, alias} {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	x.h.Sweep(time.Now(), nil)
	if _, err := os.Stat(filepath.Join(dir, "inner")); err != nil {
		t.Fatal("the sweep removed a folder")
	}
	if _, err := os.Stat(alias); err == nil {
		t.Fatal("an old stray file stayed")
	}
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("a stray name swept a young fetch")
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
	x.h.Sweep(time.Now(), nil)
	x.h.end(k)
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("the sweep took a running fetch's staging")
	}
	x.libs.writable = false
	x.h.Sweep(time.Now(), nil)
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
	x.h.Sweep(time.Now(), nil)
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
	x.h.Sweep(time.Now(), nil)
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

// A state naming a server while the paired server's id isn't known (a start
// before the first heartbeat) is never acted on by Abandon or the sweep: it
// can't be told to be this server's (bridge#71). Once the id is known, it is.
func TestUnknownServerNeverActedOn(t *testing.T) {
	x := newHarness(t)
	x.serverID = "server-a"
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	x.serverID = ""
	x.h.Abandon(jobs.AbandonContext(context.Background(), false), x.job(true))
	x.h.Sweep(time.Now(), nil)
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("Abandon or the sweep acted on a state with the server unknown")
	}
	x.serverID = "server-a"
	x.h.Sweep(time.Now(), nil)
	if x.exists(stagingDir) || x.hasState() {
		t.Fatal("the sweep left this server's abandoned fetch")
	}
}

// loadRaw reads the fetch's state as it is on disk.
func (x *harness) loadRaw(t *testing.T) state {
	t.Helper()
	b, err := os.ReadFile(x.h.statePath(3, testID))
	if err != nil {
		t.Fatal(err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// An unplaced fetch whose library's mapping is gone keeps its state
// StagingMaxAge after the sweep first finds it gone, then the state ends, its
// staging folder left in the library (bridge#71). A library that comes back
// in time is swept as before; one only unwritable now keeps its state.
func TestSweepEndsAStateWhoseLibraryIsGone(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	x.libs.mount = "" // the library deleted on the server
	now := time.Now()
	x.h.Sweep(now, nil)
	if !x.hasState() || x.loadRaw(t).Unmapped.IsZero() {
		t.Fatal("the first sweep without the library didn't keep the state with the time it went")
	}
	x.h.Sweep(now.Add(StagingMaxAge-time.Hour), nil)
	if !x.hasState() {
		t.Fatal("the state ended before StagingMaxAge without its library")
	}
	x.h.Sweep(now.Add(StagingMaxAge+time.Hour), nil)
	if x.hasState() || !x.exists(stagingDir) {
		t.Fatal("the state of a library gone for StagingMaxAge stayed, or its staging folder was touched")
	}

	// Back in time: swept as an abandoned fetch.
	x = newHarness(t)
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	mount := x.libs.mount
	x.libs.mount = ""
	x.h.Sweep(now, nil)
	x.libs.mount = mount
	// A run on the mapped library that saves nothing (meta down) still forgets
	// the time it went, so a later unmapping counts from its own start.
	x.meta.bookErr = metaops.ErrUnavailable
	if _, err := x.run(context.Background(), true); err == nil {
		t.Fatal("meta down didn't fail the run")
	}
	if !x.loadRaw(t).Unmapped.IsZero() {
		t.Fatal("a run on the mapped library left the time it went in the state")
	}
	x.h.Sweep(now.Add(StagingMaxAge+time.Hour), nil)
	if x.hasState() || x.exists(stagingDir) {
		t.Fatal("a library back in time didn't have its abandoned fetch swept")
	}

	// Mapped but unwritable: not gone, kept however long.
	x = newHarness(t)
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	x.libs.writable = false
	x.h.Sweep(now, nil)
	x.age(t, 3*StagingMaxAge)
	x.h.Sweep(now.Add(2*StagingMaxAge), nil)
	if !x.hasState() || !x.loadRaw(t).Unmapped.IsZero() {
		t.Fatal("an unwritable library's fetch was treated as unmapped")
	}
}

// mkStray makes a staging-named folder rel in the library holding what a fetch
// writes there (a section, a partial, the cover, the marker and atomic writes'
// temps) and the extra files given, every one of them last changed at.
func (x *harness) mkStray(t *testing.T, rel string, at time.Time, extra ...string) {
	t.Helper()
	files := append([]string{"01 - Chapter One.mp3", "02 - Chapter Two.mp3.part", coverName, ".audiosilo-tmp-0a1b2c",
		markerRel, ".audiosilo/.audiosilo-tmp-3d4e5f"}, extra...)
	for _, f := range files {
		p := filepath.Join(x.path(rel), filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { //nolint:gosec // test
			t.Fatal(err)
		}
	}
	err := filepath.WalkDir(x.path(rel), func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, at, at)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// touchStray sets the folder rel in the library as last changed at, after an
// entry was added to it.
func (x *harness) touchStray(t *testing.T, rel string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(x.path(rel), at, at); err != nil {
		t.Fatal(err)
	}
}

// A staging-named folder the sweep can't tell is only a fetch's leftovers
// stays, however old: one holding a file a fetch never names, a folder or a
// symlink (in it or in its .audiosilo/), one of a project another library's
// state knows, and one whose newest file is young (bridge#71).
func TestSweepLeavesStagingItCantVouchFor(t *testing.T) {
	x := newHarness(t)
	now := time.Now()
	old := now.Add(-StagingMaxAge - time.Hour)
	foreign := map[string]string{
		stagingName(90): "notes.txt",
		stagingName(91): "01.mp3", // not the "NN - title" a fetch names a section
		stagingName(92): "Extras/01 - Bonus.mp3",
		stagingName(93): ".audiosilo/characters.json",
	}
	for rel, extra := range foreign {
		x.mkStray(t, rel, old, extra)
	}
	x.mkStray(t, stagingName(94), old)
	if err := os.Symlink(x.stateDir, filepath.Join(x.path(stagingName(94)), "03 - Linked.mp3")); err != nil {
		t.Fatal(err)
	}
	x.touchStray(t, stagingName(94), old)
	// Project 95's state is library 4's (this folder as another mount reaches it).
	x.mkStray(t, stagingName(95), old)
	if err := os.MkdirAll(x.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(x.stateDir, "4-95.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Project 96's partial still grows.
	x.mkStray(t, stagingName(96), old)
	if err := os.WriteFile(filepath.Join(x.path(stagingName(96)), "03 - Three.mp3.part"), []byte("x"), 0o644); err != nil { //nolint:gosec // test
		t.Fatal(err)
	}
	x.touchStray(t, stagingName(96), old)
	x.mkStray(t, stagingName(97), old) // only a fetch's leftovers: goes
	// A user's folder named like a staging folder but not exactly one.
	similar := []string{".audiosilo-librivox-097", ".audiosilo-librivox-97 (copy)", "audiosilo-librivox-97",
		".audiosilo-librivox-+97", ".Audiosilo-librivox-89", ".audiosilo-librivox-97.bak"}
	for _, rel := range similar {
		x.mkStray(t, rel, old)
	}
	// A staging-named symlink out of the library, and an author folder that
	// is a symlink out of it holding a staging folder: neither is followed.
	outside := t.TempDir()
	x.mkStray(t, "../"+filepath.Base(outside)+"/"+stagingName(98), old) // fetch-like leftovers outside
	if err := os.Symlink(filepath.Join(outside, stagingName(98)), x.path(stagingName(98))); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, x.path("Linked Author")); err != nil {
		t.Fatal(err)
	}

	x.h.Sweep(now, nil)
	for _, rel := range similar {
		if !x.exists(rel) {
			t.Errorf("the user's folder %s went", rel)
		}
	}
	if !x.exists(stagingName(98)) {
		t.Error("the staging-named symlink went")
	}
	if _, err := os.Stat(filepath.Join(outside, stagingName(98), coverName)); err != nil {
		t.Errorf("a staging folder outside the library, reached by a symlink, was emptied: %v", err)
	}
	for _, id := range []int{90, 91, 92, 93, 94, 95, 96} {
		if !x.exists(stagingName(id)) {
			t.Errorf("%s went", stagingName(id))
		}
	}
	if x.exists(stagingName(97)) {
		t.Error("a fetch's leftovers stayed")
	}
	if _, err := os.Stat(x.stateDir); err != nil {
		t.Fatalf("the symlink's target went: %v", err)
	}
}

// Staging folders no state names (in the library's root or an author folder)
// go once older than StagingMaxAge, at most once a day (bridge#71); a young
// one, a held fetch's, a staging folder its state names, and folders that only
// look like one stay.
func TestSweepRemovesStrayStaging(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t) // its state names stagingDir
	now := time.Now()
	old, young := now.Add(-StagingMaxAge-time.Hour), now.Add(-time.Hour)
	if err := os.Chtimes(x.path(stagingDir), old, old); err != nil {
		t.Fatal(err)
	}
	x.mkStray(t, stagingName(77), old)
	x.mkStray(t, "An Author/"+stagingName(78), old)
	x.mkStray(t, stagingName(79), young)
	x.mkStray(t, stagingName(80), old)
	x.mkStray(t, stagingPrefix+"x", old)
	x.mkStray(t, "An Author/Book/"+stagingName(81), old) // deeper than a staging folder goes
	open := []json.RawMessage{json.RawMessage(`{"librivox_id":80,"work_id":"w","recording_id":"r","library_id":3}`)}
	x.h.Sweep(now, open)
	for _, gone := range []string{stagingName(77), "An Author/" + stagingName(78)} {
		if x.exists(gone) {
			t.Fatalf("the stray %s stayed", gone)
		}
	}
	if !x.exists("An Author") {
		t.Fatal("the author folder of a stray went")
	}
	for _, kept := range []string{stagingDir, stagingName(79), stagingName(80), stagingPrefix + "x", "An Author/Book/" + stagingName(81)} {
		if !x.exists(kept) {
			t.Fatalf("%s went", kept)
		}
	}
	if !x.hasState() {
		t.Fatal("the young fetch's state went")
	}

	// At most once a day.
	x.mkStray(t, stagingName(82), old)
	x.h.Sweep(now.Add(time.Hour), nil)
	if !x.exists(stagingName(82)) {
		t.Fatal("the strays were looked for again within the day")
	}
	x.h.Sweep(now.Add(strayWalkEvery), nil)
	if x.exists(stagingName(82)) || x.exists(stagingName(80)) {
		t.Fatal("the next day's sweep left a stray, or a fetch no longer held")
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
