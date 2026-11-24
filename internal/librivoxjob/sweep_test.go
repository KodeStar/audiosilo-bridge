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
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
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

	// Allowed: this server's state. Denied: one with no server id, which
	// Abandon can't prove is this job's (the sweep's, once abandoned).
	for recorded, removed := range map[string]bool{"server-b": true, "": false} {
		x = newHarness(t)
		x.serverID = recorded
		x.stageAndStop(t)
		x.serverID = "server-b"
		x.h.Abandon(jobs.AbandonContext(context.Background(), false), x.job(true))
		if gone := !x.exists(stagingDir) && !x.hasState(); gone != removed {
			t.Fatalf("recorded %q: Abandon removed the staging %v, want %v", recorded, gone, removed)
		}
	}
}

// Abandon removes only the staging its own job recorded (allowed: its server
// and job id in the state, the libraries listed its server's, while the Bridge
// is unpaired too). Another job's fetch of the same project and library (a
// new claim since), a state from before the job id was kept, and a job of a
// server the Bridge was paired with before the last pairing are left (denied).
func TestAbandonRemovesOnlyItsOwnJobsStaging(t *testing.T) {
	ctx := jobs.AbandonContext(context.Background(), false)
	x := newHarness(t)
	x.stageAndStop(t)
	other := jobs.ClaimedFrom(x.job(true), testServer)
	other.ID = 42
	x.h.Abandon(ctx, other)
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("Abandon of another job removed this job's staging")
	}
	x.serverID = "server-b" // paired with server-b since, unpaired now: its libraries listed
	x.h.Abandon(ctx, jobs.ClaimedFrom(x.job(true), testServer))
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("Abandon followed its state into another server's library")
	}
	x.serverID = testServer
	x.srv = nil // unpaired: the libraries listed are still its server's
	x.h.Abandon(ctx, jobs.ClaimedFrom(x.job(true), testServer))
	if x.exists(stagingDir) || x.hasState() {
		t.Fatal("Abandon left its own job's staging")
	}

	x = newHarness(t)
	x.stageAndStop(t)
	st := x.h.loadState(3, testID, testServer)
	st.JobID = 0 // recorded before the job id was kept
	if err := x.h.saveState(testID, st); err != nil {
		t.Fatal(err)
	}
	x.h.Abandon(ctx, jobs.ClaimedFrom(x.job(true), testServer))
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("Abandon removed a staging its job can't be proven to own")
	}
	// A run of the job takes the state over, and its Abandon removes it.
	x.fetch.files[sectionURL(3)].cut = []int{0, 0, 0}
	if _, err := x.run(context.Background(), true); err == nil {
		t.Fatal("the outage didn't fail the run")
	}
	if st := x.h.loadState(3, testID, testServer); st == nil || st.JobID != 41 {
		t.Fatalf("the resumed run's state: %+v", st)
	}
	x.h.Abandon(ctx, jobs.ClaimedFrom(x.job(true), testServer))
	if x.exists(stagingDir) || x.hasState() {
		t.Fatal("Abandon left the staging its job took over")
	}
}

// The state under an abandoned job's id may be a later claim's of the same
// job by the time its work is released: the server handed the job out again,
// at a later attempt (under a pairing made since, too: the Bridge unpaired
// and paired again with the same server), and that claim's run took the
// state up while the earlier claim's release was still on its way. The job's
// id alone doesn't tell the two apart: the later claim's staging and state
// stay (denied), and its own Abandon removes them (allowed).
func TestAbandonLeavesTheStagingALaterClaimTookUp(t *testing.T) {
	ctx := jobs.AbandonContext(context.Background(), false)
	x := newHarness(t)
	x.stageAndStop(t)
	first := x.job(true)
	if st := x.h.loadState(3, testID, testServer); st == nil || st.JobID != first.ID || st.Attempt != first.Attempt {
		t.Fatalf("the first claim's state: %+v", st)
	}
	// The job handed out again: its run takes the state up, and stops.
	second := first
	second.Attempt = 2
	x.fetch.files[sectionURL(3)].cut = []int{0, 0, 0}
	if _, err := x.h.Run(context.Background(), second, jobs.LocalReporter()); err == nil {
		t.Fatal("the outage didn't fail the run")
	}
	x.fetch.files[sectionURL(3)].cut = nil
	if st := x.h.loadState(3, testID, testServer); st == nil || st.JobID != second.ID || st.Attempt != 2 {
		t.Fatalf("the later claim's state: %+v", st)
	}
	x.h.Abandon(ctx, first)
	if !x.exists(stagingDir) || !x.hasState() {
		t.Fatal("Abandon of the earlier claim removed the staging a later claim took up")
	}
	x.h.Abandon(ctx, second)
	if x.exists(stagingDir) || x.hasState() {
		t.Fatal("Abandon left its own claim's staging")
	}

	// The same attempt number under another pairing is another claim too (a
	// row of the old pairing dropped as the new one claims the job).
	for pairing, removed := range map[string]bool{"p2": false, "p1": true} {
		x = newHarness(t)
		x.stageAndStop(t)
		st := x.h.loadState(3, testID, testServer)
		st.Pairing = pairing // the claim that last took the state up
		if err := x.h.saveState(testID, st); err != nil {
			t.Fatal(err)
		}
		x.srv = nil // unpaired: the libraries listed are still its server's
		x.h.Abandon(ctx, jobs.ClaimedUnder(x.job(true), testServer, "p1"))
		if gone := !x.exists(stagingDir) && !x.hasState(); gone != removed {
			t.Fatalf("a state of pairing %q: Abandon of p1's claim removed the staging %v, want %v", pairing, gone, removed)
		}
	}
}

// A job of a server the Bridge is no longer paired with (dropped by the
// runner) follows its state into no library of the new pairing, whose
// libraries share ids with its own: Abandon removes nothing there (denied).
// Paired with its own server, its staging goes (allowed).
func TestAbandonOfADroppedJobLeavesTheNewPairingsLibrary(t *testing.T) {
	x := newHarness(t)
	x.serverID, x.srv.id = "server-a", "server-a"
	x.stageAndStop(t)
	job := jobs.ClaimedFrom(x.job(true), "server-a")
	x.serverID, x.srv.id = "server-b", "server-b"
	x.h.Abandon(jobs.AbandonContext(context.Background(), false), job)
	if !x.exists(stagingDir) {
		t.Fatal("Abandon of another server's job removed a folder in the new pairing's library")
	}
	if !x.hasState() {
		t.Fatal("Abandon of another server's job dropped its state")
	}
	x.serverID, x.srv.id = "server-a", "server-a"
	x.h.Abandon(jobs.AbandonContext(context.Background(), false), job)
	if x.exists(stagingDir) || x.hasState() {
		t.Fatal("Abandon left the staging of a job of the paired server")
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

// The sweep removes from a stray staging folder only what it saw there: a
// file put in after its look stays, and the folder with it; a folder renamed
// away after the look, another reached by a symlink now in its place, loses
// nothing; nor does one whose library stopped being writable during the look
// (bridge#71).
func TestSweepRemovesOnlyWhatItSawInAStray(t *testing.T) {
	now := time.Now()
	old := now.Add(-StagingMaxAge - time.Hour)

	x := newHarness(t)
	x.mkStray(t, stagingName(60), old)
	x.h.o.strayLooked = func(rel string) {
		for _, f := range []string{"notes.txt", ".audiosilo/characters.json"} {
			if err := os.WriteFile(filepath.Join(x.path(rel), filepath.FromSlash(f)), []byte("mine"), 0o644); err != nil { //nolint:gosec // test
				t.Error(err)
			}
		}
	}
	x.h.Sweep(now, nil)
	for _, kept := range []string{"notes.txt", ".audiosilo/characters.json"} {
		if !x.exists(stagingName(60) + "/" + kept) {
			t.Errorf("%s, put in the folder after the sweep looked at it, went", kept)
		}
	}
	if x.exists(stagingName(60) + "/" + coverName) {
		t.Error("the fetch's own files stayed")
	}

	x = newHarness(t)
	x.mkStray(t, stagingName(61), old)
	x.h.o.strayLooked = func(rel string) {
		if err := os.Rename(x.path(rel), x.path("Rescued")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("Rescued", x.path(rel)); err != nil {
			t.Error(err)
		}
	}
	x.h.Sweep(now, nil)
	for _, kept := range []string{"01 - Chapter One.mp3", coverName, markerRel} {
		if !x.exists("Rescued/" + kept) {
			t.Errorf("%s went from a folder renamed after the sweep looked at it", kept)
		}
	}
	if _, err := os.Lstat(x.path(stagingName(61))); err != nil {
		t.Errorf("the symlink put in the staging folder's place went: %v", err)
	}

	x = newHarness(t)
	x.mkStray(t, stagingName(62), old)
	x.h.o.strayLooked = func(string) { x.libs.writable = false }
	x.h.Sweep(now, nil)
	for _, kept := range []string{"01 - Chapter One.mp3", coverName, markerRel} {
		if !x.exists(stagingName(62) + "/" + kept) {
			t.Errorf("%s went from a library no longer writable once the sweep had looked", kept)
		}
	}
}

// A library that stops answering once a stray is found doesn't hold the
// sweep: it goes on after its wait, the library's other strays left for the
// next walk, and the removal is given up: the project is free again at once
// (a fetch of it runs rather than failing as already running), and nothing
// is removed once the mount answers (bridge#71).
func TestSweepDoesNotWaitOnAStrayItsMountHolds(t *testing.T) {
	x := newHarness(t)
	now := time.Now()
	old := now.Add(-StagingMaxAge - time.Hour)
	x.mkStray(t, stagingName(50), old)
	x.mkStray(t, stagingName(51), old)
	hung, answer, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
	x.h.o.strayWait = 20 * time.Millisecond
	x.h.o.strayLooked = func(string) {
		close(hung)
		<-answer
	}
	x.h.o.strayEnded = func(string) { close(ended) }
	x.h.Sweep(now, nil) // returns: the mount holds the removal
	<-hung
	k := key{lib: 3, id: 50}
	if !x.h.begin(k) {
		t.Fatal("the project stayed reserved after the sweep gave its removal up")
	}
	if !x.exists(stagingName(50)) || !x.exists(stagingName(51)) {
		t.Fatal("a stray went while the mount held, or the walk went on in a library not answering")
	}
	close(answer)
	<-ended
	x.h.end(k)
	for _, kept := range []string{stagingName(50), stagingName(50) + "/" + coverName, stagingName(50) + "/" + markerRel} {
		if !x.exists(kept) {
			t.Fatalf("%s went: the removal the sweep gave up went on once the mount answered", kept)
		}
	}
}

// While the sweep looks at a stray staging folder and removes it, no fetch of
// its project starts in any library (another library's mount may reach the
// folder), and the sweep leaves a stray whose project a fetch of another
// library holds (bridge#71).
func TestStraySweepHoldsTheProjectInEveryLibrary(t *testing.T) {
	x := newHarness(t)
	x.libs.second = t.TempDir()
	now := time.Now()
	old := now.Add(-StagingMaxAge - time.Hour)
	x.mkStray(t, stagingName(50), old)
	x.mkStray(t, stagingName(51), old)
	other := key{lib: 4, id: 51}
	if !x.h.begin(other) { // a fetch of project 51 into library 4
		t.Fatal("begin")
	}
	looked := 0
	x.h.o.strayLooked = func(rel string) {
		looked++
		for _, k := range []key{{lib: 3, id: 50}, {lib: 4, id: 50}} {
			if x.h.begin(k) {
				t.Errorf("denied: a fetch of project 50 into library %d started while the sweep removed its stray", k.lib)
				x.h.end(k)
			}
		}
	}
	x.h.Sweep(now, nil)
	if looked != 1 || x.exists(stagingName(50)) {
		t.Fatalf("allowed: the stray of a project no fetch knows stayed (%d looks)", looked)
	}
	if !x.exists(stagingName(51)) {
		t.Fatal("denied: the stray of a project another library's fetch holds went")
	}
	x.h.end(other)
	if !x.h.begin(key{lib: 4, id: 50}) {
		t.Fatal("the project stayed held after the sweep")
	}
}

// The walk for stray staging folders reads the author folders within its
// budget only: past it, the strays in the library's own folder still go and
// the author folders wait for the next walk (bridge#65).
func TestStrayWalkStopsAtItsBudget(t *testing.T) {
	x := newHarness(t)
	now := time.Now()
	old := now.Add(-StagingMaxAge - time.Hour)
	x.mkStray(t, stagingName(70), old)
	x.mkStray(t, "An Author/"+stagingName(71), old)
	x.h.o.strayWalkMax = time.Nanosecond
	x.h.Sweep(now, nil)
	if x.exists(stagingName(70)) {
		t.Fatal("the stray in the library's own folder stayed")
	}
	if !x.exists("An Author/" + stagingName(71)) {
		t.Fatal("an author folder was read past the walk's budget")
	}
	x.h.o.strayWalkMax = strayWalkTime
	x.h.Sweep(now.Add(strayWalkEvery), nil)
	if x.exists("An Author/" + stagingName(71)) {
		t.Fatal("the next walk within its budget left the author folder's stray")
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

// bookErr answers every book read with err.
type bookErr struct{ err error }

func (b bookErr) Book(context.Context, int64, string) (*link.Book, error) { return nil, b.err }

// The "mapping gone" timer starts only on a definite "not mapped": a check the
// server didn't answer leaves the library's state unknown (the registry's
// Status false), which isn't gone (denied), while the server's own "no indexed
// book" at a folder holding something is (allowed). An empty folder, which is
// what a share not mounted leaves at its mount point, isn't (denied).
func TestMappingGoneNeedsADefiniteAnswer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	reg := mapping.NewRegistry()
	reg.SetLibraries([]link.Library{{ID: 1, Root: root}})
	mount := func(int64, string) (string, bool) { return root, true }
	h := &Handler{o: Options{Libraries: reg, Mount: mount}}
	reg.Check(context.Background(), bookErr{&link.APIError{Status: 429}}, mount, time.Now)
	if h.mappingGone(1) {
		t.Fatal("denied: a check the server didn't answer counts as the mapping gone")
	}
	reg.Check(context.Background(), bookErr{link.ErrNoBook}, mount, time.Now)
	if st, checked := reg.Status(1); !checked || st.Mapped {
		t.Fatalf("the empty folder's check = %+v, %v; want a standing unmapped", st, checked)
	}
	if h.mappingGone(1) {
		t.Fatal("denied: an empty folder (an unmounted share's mount point) counts as the mapping gone")
	}
	// The same at the server's own path with no mapping configured (a compose
	// stack mounting the library at the same path in both containers).
	h.o.Mount = func(int64, string) (string, bool) { return root, false }
	if h.mappingGone(1) {
		t.Fatal("denied: an empty folder at the server's own path, no mapping configured, counts as the mapping gone")
	}
	h.o.Mount = mount
	if err := os.WriteFile(filepath.Join(root, "other.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reg.Check(context.Background(), bookErr{link.ErrNoBook}, mount, time.Now)
	if !h.mappingGone(1) {
		t.Fatal("allowed: the server's definite answer doesn't count as the mapping gone")
	}

	// A check that failed to look at the folder (a share not mounted now)
	// isn't gone (denied, bridge#65); with no mapping configured here any
	// more it is (allowed).
	missing := filepath.Join(root, "unmounted")
	reg.SetLibraries([]link.Library{{ID: 1, Root: missing}})
	configured := true
	mount = func(int64, string) (string, bool) { return missing, configured }
	h.o.Mount = mount
	reg.Check(context.Background(), bookErr{link.ErrNoBook}, mount, time.Now)
	if st, checked := reg.Status(1); !checked || st.Mapped {
		t.Fatalf("the missing folder's check = %+v, %v; want a standing unmapped", st, checked)
	}
	if h.mappingGone(1) {
		t.Fatal("denied: a check that couldn't look at the folder counts as the mapping gone")
	}
	configured = false
	if !h.mappingGone(1) {
		t.Fatal("allowed: a library no mapping is configured for, found unmapped, isn't gone")
	}
}

// A fetch whose library came back mapped but not writable forgets when its
// mapping went, keeping its age: a later unmapping counts from its own start
// (bridge#65).
func TestSweepForgetsUnmappedOnceMappedAgain(t *testing.T) {
	x := newHarness(t)
	x.stageAndStop(t)
	x.age(t, StagingMaxAge+time.Hour)
	mount := x.libs.mount
	x.libs.mount = ""
	now := time.Now()
	x.h.Sweep(now, nil)
	if x.loadRaw(t).Unmapped.IsZero() {
		t.Fatal("the sweep didn't record the time the mapping went")
	}
	x.libs.mount, x.libs.writable = mount, false
	x.age(t, StagingMaxAge+time.Hour)
	info, err := os.Stat(x.h.statePath(3, testID))
	if err != nil {
		t.Fatal(err)
	}
	x.h.Sweep(now.Add(StagingMaxAge+2*time.Hour), nil)
	if !x.loadRaw(t).Unmapped.IsZero() {
		t.Fatal("a library mapped again (unwritable) kept the time its mapping went")
	}
	if after, err := os.Stat(x.h.statePath(3, testID)); err != nil || !after.ModTime().Equal(info.ModTime()) {
		t.Fatalf("forgetting the time aged the state anew: %v", err)
	}
	// Unmapped again: a new time is recorded, the state not ended at once.
	x.libs.mount = ""
	x.h.Sweep(now.Add(StagingMaxAge+3*time.Hour), nil)
	if !x.hasState() || x.loadRaw(t).Unmapped.IsZero() {
		t.Fatal("a fetch unmapped again ended at once, or no new time was recorded")
	}
}
