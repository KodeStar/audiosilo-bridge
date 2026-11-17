//go:build unix

package agentbox

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// otherUID is a uid that isn't this process's: the runner's when the test's
// own files stand for another uid's (a forger's), as a test can't write a
// file under another uid without root.
func otherUID() int64 { return int64(os.Geteuid()) + 2 }

// fromRunner pins the runner's uid at the first live runner.json and believes
// only that uid's single-link files after (allowed: the runner's own file;
// denied: before the pin, the Bridge's own uid, another uid, a second link).
func TestFromRunner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "runner.json")
	mustWrite(t, file, "{}")
	info := func() fs.FileInfo {
		t.Helper()
		i, err := os.Lstat(file)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	me := os.Geteuid()

	r, _ := newTestRunner(t, agent.IDClaude)
	if _, own := r.fromRunner(info(), false); own {
		t.Fatal("a file read before any live runner.json counts as the runner's")
	}
	if uid, own := r.fromRunner(info(), true); !own || uid != me || r.runnerUID.Load() != int64(me) {
		t.Fatalf("the first live runner.json: uid %d, own %v, pinned %d (want %d, true, %d)", uid, own, r.runnerUID.Load(), me, me)
	}
	if _, own := r.fromRunner(info(), false); !own {
		t.Fatal("the pinned runner's file doesn't count as the runner's")
	}
	if err := os.Link(file, filepath.Join(dir, "exit.json")); err != nil {
		t.Fatal(err)
	}
	if _, own := r.fromRunner(info(), false); own {
		t.Fatal("a file with a second link counts as the runner's")
	}
	// A live runner.json with a second link can't pin, and says so once.
	linked, _ := newTestRunner(t, agent.IDClaude)
	var said strings.Builder
	linked.log = slog.New(slog.NewTextHandler(&said, nil))
	for range 2 {
		if _, own := linked.fromRunner(info(), true); own || linked.runnerUID.Load() != noUID {
			t.Fatalf("a live runner.json with a second link: own %v, pinned %d", own, linked.runnerUID.Load())
		}
	}
	if n := strings.Count(said.String(), "more than one link"); n != 1 {
		t.Fatalf("the second link is logged %d times, want 1:\n%s", n, said.String())
	}

	self, _ := newTestRunner(t, agent.IDClaude)
	self.uid = me
	single := filepath.Join(t.TempDir(), "runner.json")
	mustWrite(t, single, "{}")
	si, err := os.Lstat(single)
	if err != nil {
		t.Fatal(err)
	}
	if _, own := self.fromRunner(si, true); own || self.runnerUID.Load() != noUID {
		t.Fatalf("a runner.json of the Bridge's own uid: own %v, pinned %d", own, self.runnerUID.Load())
	}

	// Nothing to warn of before a pin, nor for the Bridge's own uid.
	var logged strings.Builder
	self.log = slog.New(slog.NewTextHandler(&logged, nil))
	self.notFromRunner("runner.json", me+1)
	self.runnerUID.Store(otherUID())
	self.notFromRunner("runner.json", me)
	if logged.Len() != 0 {
		t.Fatalf("warned: %s", logged.String())
	}

	// A live runner.json from a second uid ends the pin: neither is the
	// runner's after, said once; a file of either uid then counts as no one's.
	other, _ := newTestRunner(t, agent.IDClaude)
	var contested strings.Builder
	other.log = slog.New(slog.NewTextHandler(&contested, nil))
	other.runnerUID.Store(otherUID())
	for range 2 {
		if _, own := other.fromRunner(si, true); own || other.runnerUID.Load() != contestedUID {
			t.Fatalf("a second uid's live runner.json: own %v, pinned %d", own, other.runnerUID.Load())
		}
	}
	if _, own := other.fromRunner(si, false); own {
		t.Fatal("a file counts as the runner's once the pin is contested")
	}
	if n := strings.Count(contested.String(), "a second uid wrote a live runner.json"); n != 1 {
		t.Fatalf("the contest is logged %d times, want 1:\n%s", n, contested.String())
	}
	other.notFromRunner("runner.json", me)
	if strings.Contains(contested.String(), "isn't the runner's") {
		t.Fatalf("warned of a foreign file after the contest: %s", contested.String())
	}
}

// A runner.json naming no Bridge group moves the credential to every uid only
// when the runner's uid wrote it (allowed: 0777 and 0444, as for a runner
// without group_add); written by any other uid it is a forgery, and the
// credential stays the group's (denied: 0770 and 0440), logged once (as a
// second uid ending the pin: the test's runner.json is live).
func TestForgedRunnerJSONKeepsTheGroup(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pin       int64
		dir, file fs.FileMode
		logged    int
	}{
		"the runner's": {pin: noUID, dir: fs.ModeDir | 0o777, file: 0o444},
		"another uid's": {pin: otherUID(), dir: fs.ModeDir | credentialGroupDirMode, file: credentialGroupFileMode,
			logged: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, run := handoffStager(t, nil)
			var logged strings.Builder
			r.log = slog.New(slog.NewTextHandler(&logged, nil))
			r.runnerUID.Store(tc.pin)
			for range 2 {
				if m := run(); m.dir != tc.dir || m.file != tc.file {
					t.Fatalf("modes = dir %v, file %v (want %v, %v)", m.dir, m.file, tc.dir, tc.file)
				}
			}
			if n := strings.Count(logged.String(), "stays the Bridge's group's alone"); n != tc.logged {
				t.Fatalf("the forged runner.json is logged %d times, want %d:\n%s", n, tc.logged, logged.String())
			}
		})
	}
}

// A credential_denied moves the rest of the process off the group only from
// the runner's uid (allowed: restaged in the every-uid modes, the group off);
// an exit.json another uid wrote is the job refused, never restaged, and the
// group stays on (denied).
func TestForgedCredentialDeniedKeepsTheGroup(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, pin int64) (*Runner, *fakeRunner, error) {
		t.Helper()
		r, dir := vaultHandoff(t, []int{os.Getegid()})
		r.runnerUID.Store(pin)
		fake := &fakeRunner{fn: (&deniedGroupRunner{deny: true}).fn(t)}
		stop := fake.start(t, dir)
		_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
		stop()
		assertNoJobDirs(t, dir)
		return r, fake, err
	}
	t.Run("the runner's", func(t *testing.T) {
		t.Parallel()
		r, fake, err := run(t, noUID)
		if err != nil || fake.claimed.Load() != 2 || !r.groupDenied.Load() || r.Status().ShareGroup {
			t.Fatalf("err = %v, %d jobs, group denied %v", err, fake.claimed.Load(), r.groupDenied.Load())
		}
	})
	t.Run("another uid's", func(t *testing.T) {
		t.Parallel()
		r, fake, err := run(t, otherUID())
		if err == nil || !strings.Contains(err.Error(), "refused the job") || !strings.Contains(err.Error(), "restart the Bridge") ||
			fake.claimed.Load() != 1 ||
			r.groupDenied.Load() || !r.Status().ShareGroup {
			t.Fatalf("err = %v, %d jobs, group denied %v", err, fake.claimed.Load(), r.groupDenied.Load())
		}
	})
}

// plantDenial plays a uid that moves an exit.json of the runner's into the
// next job in dir without the runner running it: any uid can have the runner
// write one saying it was denied a credential (a job dir of its own making,
// whose credential the runner may not delete). before sets the job dir up
// first, after runs once the job is marked done. The test's files stand for
// the runner's uid, so the exit.json's owner is the runner's. The returned
// stop joins it.
func plantDenial(t *testing.T, dir string, before, after func(job string)) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	stop = func() { cancel(); <-stopped }
	t.Cleanup(stop)
	go func() {
		defer close(stopped)
		for ctx.Err() == nil {
			time.Sleep(2 * time.Millisecond)
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				job := filepath.Join(dir, e.Name())
				if !agentproto.ValidJobID(e.Name()) || !present(job, agentproto.RequestFile) {
					continue
				}
				if before != nil {
					before(job)
				}
				_ = os.WriteFile(filepath.Join(job, agentproto.ExitFile),
					[]byte(`{"exit_code":-1,"error":"couldn't delete the Bridge's credential.json before the run: permission denied","credential_denied":true}`), 0o644)
				_ = os.WriteFile(filepath.Join(job, agentproto.DoneFile), nil, 0o644)
				if after != nil {
					after(job)
				}
				return
			}
		}
	}()
	return stop
}

// An exit.json of the runner's own moves the credential off the group only
// for a job the runner itself claimed and whose credential it left in place
// (allowed: TestForgedCredentialDeniedKeepsTheGroup's runner). One moved into
// a job the runner never claimed, one whose claim any uid may write (a dir
// moved in) and one whose credential the runner takes a moment later (it had
// only just claimed the job) are each the job refused, never restaged, and
// the group stays on (denied).
func TestMovedCredentialDeniedKeepsTheGroup(t *testing.T) {
	t.Parallel()
	claim := func(mode fs.FileMode) func(job string) {
		return func(job string) {
			path := filepath.Join(job, agentproto.ClaimDir)
			if err := os.Mkdir(path, 0o700); err == nil {
				_ = os.Chmod(path, mode) //nolint:gosec // 0777 is the case under test: a dir any uid may move
			}
		}
	}
	for name, tc := range map[string]struct {
		before, after func(job string)
		takeFor       time.Duration
	}{
		"never claimed":             {},
		"a claim any uid may write": {before: claim(0o777)},
		// The Bridge finds the credential in place at its first look, and gone
		// by its second, takeFor later.
		"the credential taken a moment later": {before: claim(0o700), takeFor: time.Second, after: func(job string) {
			time.Sleep(50 * time.Millisecond)
			_ = os.Remove(filepath.Join(job, agentproto.CredentialDir, agentproto.CredentialFile))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, dir := vaultHandoff(t, []int{os.Getegid()})
			if tc.takeFor > 0 {
				r.takeFor = tc.takeFor
			}
			stop := plantDenial(t, dir, tc.before, tc.after)
			_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
			stop()
			assertNoJobDirs(t, dir)
			if err == nil || !strings.Contains(err.Error(), "refused the job") || r.groupDenied.Load() || !r.Status().ShareGroup {
				t.Fatalf("err = %v, group denied %v", err, r.groupDenied.Load())
			}
			if r.runnerUID.Load() != int64(os.Geteuid()) {
				t.Fatalf("the runner's uid isn't pinned (%d): the exit.json's owner was never the question", r.runnerUID.Load())
			}
		})
	}
}

// agent.require_group never moves the credential to every uid: a runner
// outside the group, a volume that refuses the chown and a runner denied the
// group's file each make the agent unavailable, saying so in its status (the
// page, the heartbeat, the checklist) so no later job is staged, and the job
// (when one was staged) is never opened to every uid. A runner in the group
// runs as before (allowed).
func TestRequireGroup(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, r *Runner, dir string, d *deniedGroupRunner) (*fakeRunner, error) {
		t.Helper()
		r.requireGroup = true
		fake := &fakeRunner{fn: d.fn(t)}
		stop := fake.start(t, dir)
		_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
		stop()
		assertNoJobDirs(t, dir)
		return fake, err
	}
	unavailable := func(t *testing.T, r *Runner, err error) {
		t.Helper()
		var na *agent.NotAvailableError
		if !errors.As(err, &na) || !strings.Contains(na.Detail, "agent.require_group") {
			t.Fatalf("err = %v, want the agent unavailable naming agent.require_group", err)
		}
		if st := r.Status(); st.Available || st.Refused || st.Problem != ProblemGroupRequired || st.Detail != na.Detail {
			t.Fatalf("status = %+v, want unavailable with %q", st, na.Detail)
		}
		if av := r.Detect(context.Background()); av.Available || av.Detail != na.Detail {
			t.Fatalf("detect = %+v", av)
		}
	}
	t.Run("in the group", func(t *testing.T) {
		t.Parallel()
		r, dir := vaultHandoff(t, []int{os.Getegid()})
		d := &deniedGroupRunner{}
		if _, err := run(t, r, dir, d); err != nil || d.ran != 1 || d.modes[0] != credentialGroupDirMode {
			t.Fatalf("err = %v, %d runs, modes %v", err, d.ran, d.modes)
		}
	})
	t.Run("outside the group", func(t *testing.T) {
		t.Parallel()
		r, dir := vaultHandoff(t, nil)
		fake, err := run(t, r, dir, &deniedGroupRunner{})
		unavailable(t, r, err)
		if fake.claimed.Load() != 0 {
			t.Fatalf("%d jobs staged", fake.claimed.Load())
		}
	})
	t.Run("the volume refuses the group", func(t *testing.T) {
		t.Parallel()
		gid := foreignGID(t)
		r, dir := vaultHandoff(t, []int{gid})
		r.gid = gid
		fake, err := run(t, r, dir, &deniedGroupRunner{})
		unavailable(t, r, err)
		if fake.claimed.Load() != 0 {
			t.Fatalf("%d jobs staged", fake.claimed.Load())
		}
	})
	t.Run("denied the group's file", func(t *testing.T) {
		t.Parallel()
		r, dir := vaultHandoff(t, []int{os.Getegid()})
		d := &deniedGroupRunner{deny: true}
		fake, err := run(t, r, dir, d)
		unavailable(t, r, err)
		if fake.claimed.Load() != 1 || d.ran != 0 || r.groupDenied.Load() || !r.Status().ShareGroup {
			t.Fatalf("%d jobs, %d runs, group denied %v", fake.claimed.Load(), d.ran, r.groupDenied.Load())
		}
		// The next run is refused before anything is staged.
		if fake, err = run(t, r, dir, d); fake.claimed.Load() != 0 {
			t.Fatalf("a later run staged %d jobs (%v)", fake.claimed.Load(), err)
		}
		unavailable(t, r, err)
	})
}
