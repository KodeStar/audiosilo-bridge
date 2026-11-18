//go:build linux

package agentrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A command an endpoint's run leaves behind (forked into the background, in a
// session of its own, so neither its group's kill nor init's reaping ends it)
// is ended by the endpoint's service once the run ends (allowed: the real
// sweep, the runner its subreaper as Main makes it); without the sweep it
// outlives the run (denied: the agent container's runner, which this test
// would fail to tell apart if the sweep were gone).
//
// Not parallel: the sweep kills every stray of this test process, so no other
// test's CLI may be running meanwhile (parallel tests wait until the serial
// ones are done).
func TestEndpointSweepEndsStrays(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid here")
	}
	if err := endpointHardening(); err != nil {
		t.Fatal(err)
	}
	// Process-wide: undone for the tests after this one (a subreaper keeps the
	// zombies of their killed helpers, which kill(pid, 0) still finds).
	t.Cleanup(func() {
		_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0)
		_ = unix.Prctl(unix.PR_SET_DUMPABLE, 1, 0, 0, 0)
		killStrays(func(int) bool { return false })
	})
	// What the run leaves: a subshell that exits at once, so its child is
	// orphaned, in a session of its own; it writes its pid and sleeps.
	behavior := `(CAP="$CAP" setsid sh -c 'echo $$ > "$CAP/stray.tmp"; mv "$CAP/stray.tmp" "$CAP/stray"; exec sleep 300' ` +
		`</dev/null >/dev/null 2>&1 &)` + "\n" +
		`while [ ! -f "$CAP/stray" ]; do sleep 0.05; done` + "\n"

	run := func(t *testing.T, sweep bool) int {
		t.Helper()
		te, _ := newEndpointEnv(t, endpointParentEnv())
		te.r.sweep = nil
		if sweep {
			te.r.sweep = killStrays
		}
		_ = os.WriteFile(filepath.Join(te.codexCap, "behavior"), []byte(behavior), 0o644)
		te.start(t)
		if ex := waitDone(t, te.submit(t, endpointReq), 30*time.Second); ex.Error != "" || ex.ExitCode != 0 {
			t.Fatalf("exit = %+v", ex)
		}
		te.stop()
		b, err := os.ReadFile(filepath.Join(te.codexCap, "stray"))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		return pid
	}

	t.Run("swept", func(t *testing.T) {
		pid := run(t, true)
		if alive(pid) {
			_ = unix.Kill(pid, unix.SIGKILL)
			t.Fatalf("the stray %d outlived its run", pid)
		}
	})
	t.Run("not swept", func(t *testing.T) {
		pid := run(t, false)
		if !alive(pid) {
			t.Fatalf("the stray %d ended without a sweep: this test can't tell the sweep works", pid)
		}
		killStrays(func(int) bool { return false })
		if alive(pid) {
			t.Fatalf("a sweep left the stray %d", pid)
		}
	})
}

// alive reports whether pid is a process that hasn't exited (a zombie counts
// as ended).
func alive(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	p, ok := parseStat(pid, string(b))
	return ok && !p.zombie
}
