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

	other, _ := newTestRunner(t, agent.IDClaude)
	other.runnerUID.Store(otherUID())
	if _, own := other.fromRunner(si, true); own || other.runnerUID.Load() != otherUID() {
		t.Fatalf("another uid's runner.json: own %v, pinned %d", own, other.runnerUID.Load())
	}
}

// A runner.json naming no Bridge group moves the credential to every uid only
// when the runner's uid wrote it (allowed: 0777 and 0444, as for a runner
// without group_add); written by any other uid it is a forgery, and the
// credential stays the group's (denied: 0770 and 0440), logged once.
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
			if n := strings.Count(logged.String(), "isn't the runner's"); n != tc.logged {
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
		if err == nil || !strings.Contains(err.Error(), "refused the job") || fake.claimed.Load() != 1 ||
			r.groupDenied.Load() || !r.Status().ShareGroup {
			t.Fatalf("err = %v, %d jobs, group denied %v", err, fake.claimed.Load(), r.groupDenied.Load())
		}
	})
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
