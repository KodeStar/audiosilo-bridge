//go:build unix

package agentbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
)

// Only a volume that refuses the group (EPERM, ENOTSUP) gets the every-uid
// hand-off: any other failure to give the group (an I/O error) fails the
// stage as the agent being unavailable, the job never opened to the runner,
// in both modes, and Status says so (not ready, with the reason) rather than
// call ready an agent whose every job fails; no later job is staged until a
// group probe finds the group can be given again (allowed: a refusal still
// falls back, TestHandoffCredentialFallsBackWhenTheGroupIsRefused, and the
// agent is ready again once the chown works; denied: an EIO never falls back,
// never blocks agent.require_group's way, and never reads as ready). Not
// parallel: it replaces chownGroup.
func TestHandoffCredentialOtherChownErrorFails(t *testing.T) {
	saved := chownGroup
	t.Cleanup(func() { chownGroup = saved })
	for _, strict := range []bool{false, true} {
		var chowns atomic.Int32
		chownGroup = func(*os.Root, string, int) error {
			chowns.Add(1)
			return &fs.PathError{Op: "chownat", Path: "credential.json", Err: syscall.EIO}
		}
		r, dir := vaultHandoff(t, []int{os.Getegid()})
		r.requireGroup = strict
		m := &mappedRunner{}
		fake := &fakeRunner{fn: m.fn(t)}
		stop := fake.start(t, dir)
		run := func() error {
			_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
			return err
		}
		var na *agent.NotAvailableError
		if err := run(); !errors.As(err, &na) || !strings.Contains(na.Detail, "input/output error") || fake.claimed.Load() != 0 {
			t.Fatalf("strict %v: err = %v, %d jobs claimed", strict, err, fake.claimed.Load())
		}
		if strings.Contains(na.Detail, dir) || strings.Contains(na.Detail, "credential.json") {
			t.Fatalf("strict %v: the reason names a path: %q", strict, na.Detail)
		}
		st := r.Status()
		if r.groupBlocked.Load() != nil || r.groupDenied.Load() || st.Problem == ProblemGroupRequired {
			t.Fatalf("strict %v: an I/O error blocked or denied the group: %+v", strict, st)
		}
		if st.Available || st.Refused || st.Problem != ProblemHandoffFailed || st.Detail != na.Detail || st.HandoffMode != HandoffGroup {
			t.Fatalf("strict %v: after the failed hand-off the status = %+v, want not ready with the reason", strict, st)
		}
		if r.Detect(context.Background()).Available {
			t.Fatalf("strict %v: Detect calls the agent available", strict)
		}
		// The next job isn't staged at all.
		before := chowns.Load()
		if err := run(); !errors.As(err, &na) || na.Detail != st.Detail || chowns.Load() != before {
			t.Fatalf("strict %v: a second run = %v, %d chowns more", strict, err, chowns.Load()-before)
		}
		// A probe that fails the same way keeps it.
		if got := r.probeGroup(context.Background()); got != probeUnknown || r.Status().Available || fake.claimed.Load() != 0 {
			t.Fatalf("strict %v: a probe while the chown fails = %v, available %v", strict, got, r.Status().Available)
		}
		// One that can give the group ends it, and the next job runs.
		chownGroup = saved
		if got := r.probeGroup(context.Background()); got != probeTaken || r.groupFailed.Load() != nil {
			t.Fatalf("strict %v: a probe once the chown works = %v, failed %v", strict, got, r.groupFailed.Load())
		}
		if st := r.Status(); !st.Available || st.Problem != "" || st.HandoffMode != HandoffGroup {
			t.Fatalf("strict %v: after the probe the status = %+v", strict, st)
		}
		if err := run(); err != nil {
			t.Fatalf("strict %v: a run once the chown works: %v", strict, err)
		}
		stop()
		if m.ran != 1 || m.modes[0] != credentialGroupDirMode {
			t.Fatalf("strict %v: %d runs, credential dirs %v", strict, m.ran, m.modes)
		}
		assertNoJobDirs(t, dir)
	}
}

// A volume that turns from failing the chown to refusing it ends the failed
// hand-off too: the next run answers a refusal by itself (allowed: the
// every-uid fallback of a volume that refuses; denied: the stale failure
// keeping the agent unavailable for good). Not parallel: it replaces
// chownGroup.
func TestProbeEndsAFailedHandoffOnARefusingVolume(t *testing.T) {
	saved := chownGroup
	t.Cleanup(func() { chownGroup = saved })
	errno := syscall.EIO
	chownGroup = func(*os.Root, string, int) error {
		return &fs.PathError{Op: "chownat", Path: "credential.json", Err: errno}
	}
	r, dir := vaultHandoff(t, []int{os.Getegid()})
	m := &mappedRunner{}
	stop := (&fakeRunner{fn: m.fn(t)}).start(t, dir)
	run := func() error {
		_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
		return err
	}
	var na *agent.NotAvailableError
	if err := run(); !errors.As(err, &na) || r.Status().Available {
		t.Fatalf("err = %v, available %v", err, r.Status().Available)
	}
	errno = syscall.EPERM
	if got := r.probeGroup(context.Background()); got != probeRefused || !r.Status().Available {
		t.Fatalf("a probe on a refusing volume = %v, available %v", got, r.Status().Available)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	stop()
	if st := r.Status(); m.ran != 1 || m.modes[0] != credentialDirMode || st.HandoffMode != HandoffWorld {
		t.Fatalf("%d runs, credential dirs %v, mode %q (want the every-uid hand-off)", m.ran, m.modes, st.HandoffMode)
	}
	assertNoJobDirs(t, dir)
}

// groupRefused is a refusal (EPERM, ENOTSUP, EOPNOTSUPP, ENOSYS, wrapped or
// not), never another error.
func TestGroupRefused(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]bool{
		syscall.EPERM: true,
		&fs.PathError{Op: "chownat", Path: "x", Err: syscall.ENOTSUP}:    true,
		&fs.PathError{Op: "chownat", Path: "x", Err: syscall.EOPNOTSUPP}: true,
		&fs.PathError{Op: "chownat", Path: "x", Err: syscall.ENOSYS}:     true,
		&fs.PathError{Op: "chownat", Path: "x", Err: syscall.EIO}:        false,
		syscall.ENOSPC:          false,
		syscall.EACCES:          false,
		errors.New("something"): false,
	} {
		if got := groupRefused(err); got != want {
			t.Errorf("groupRefused(%v) = %v, want %v", err, got, want)
		}
	}
}
