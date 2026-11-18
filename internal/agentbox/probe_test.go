package agentbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// mappedRunner is a fake runner in the Bridge's group by its ids that, while
// deny is set, is denied a credential handed to that group alone (as under a
// user namespace that maps the volume's ids differently), and takes it (reads
// and deletes it, as agentrun does) once the mapping is fixed. A probe's
// credential is refused after the take, as agentrun's DecodeCredential does,
// so no CLI runs on it. modes are the credential dir's modes of the real jobs
// and ran how many got to the CLI (read them once the fake is stopped).
type mappedRunner struct {
	deny  atomic.Bool
	mu    sync.Mutex
	modes []fs.FileMode
	ran   int
}

func (m *mappedRunner) fn(t *testing.T) func(job string) agentproto.Exit {
	return func(job string) agentproto.Exit {
		dir := filepath.Join(job, agentproto.CredentialDir)
		info, err := os.Stat(dir)
		if err != nil {
			return agentproto.Exit{ExitCode: -1, Error: "no credential dir"}
		}
		if m.deny.Load() && info.Mode().Perm() == credentialGroupDirMode {
			return agentproto.Exit{ExitCode: -1, CredentialDenied: true,
				Error: "couldn't delete the Bridge's credential.json before the run: permission denied"}
		}
		name := filepath.Join(dir, agentproto.CredentialFile)
		raw, err := os.ReadFile(name)
		_ = os.Remove(name)
		if err == nil {
			_, err = agentproto.DecodeCredential(raw, "claude")
		}
		if err != nil {
			return agentproto.Exit{ExitCode: -1, Error: err.Error()}
		}
		m.mu.Lock()
		m.modes = append(m.modes, info.Mode().Perm())
		m.ran++
		m.mu.Unlock()
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile), claudeOK)
		return agentproto.Exit{}
	}
}

// A runner denied the group's credential makes later runs hand it to every
// uid; a probe while it is still denied changes nothing, and once its ids are
// mapped alike the next probe ends the fallback without a restart: the
// hand-off is the group's again, as Status and the next run show. No probe
// ever reaches the CLI (allowed: the fallback clears once the runner takes the
// group's file; denied: a denied probe keeps it, and a probe runs nothing).
func TestProbeEndsTheGroupFallback(t *testing.T) {
	t.Parallel()
	r, dir := vaultHandoff(t, []int{os.Getegid()})
	m := &mappedRunner{}
	m.deny.Store(true)
	stop := (&fakeRunner{fn: m.fn(t)}).start(t, dir)
	run := func() {
		t.Helper()
		if _, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if !r.groupDenied.Load() || r.Status().HandoffMode != HandoffWorld {
		t.Fatalf("after the denial: group denied %v, mode %q", r.groupDenied.Load(), r.Status().HandoffMode)
	}
	if got := r.probeGroup(context.Background()); got != probeDenied || !r.groupDenied.Load() {
		t.Fatalf("probe while denied = %v, group denied %v", got, r.groupDenied.Load())
	}
	m.deny.Store(false)
	if got := r.probeGroup(context.Background()); got != probeTaken || r.groupDenied.Load() {
		t.Fatalf("probe once mapped = %v, group denied %v", got, r.groupDenied.Load())
	}
	if st := r.Status(); !st.ShareGroup || st.HandoffMode != HandoffGroup {
		t.Fatalf("after the probe: share group %v, mode %q", st.ShareGroup, st.HandoffMode)
	}
	run()
	stop()
	want := []fs.FileMode{credentialDirMode, credentialGroupDirMode}
	if !slices.Equal(m.modes, want) || m.ran != 2 {
		t.Fatalf("credential dirs %v, %d runs (want %v, 2)", m.modes, m.ran, want)
	}
	assertNoJobDirs(t, dir)
}

// With agent.require_group a denied runner makes the agent unavailable; a
// probe once the ids are mapped alike makes it available again, the
// credential still the group's alone (allowed: available after the fix;
// denied: still unavailable while the runner is denied).
func TestProbeEndsTheRequiredGroupBlock(t *testing.T) {
	t.Parallel()
	r, dir := vaultHandoff(t, []int{os.Getegid()})
	r.requireGroup = true
	m := &mappedRunner{}
	m.deny.Store(true)
	stop := (&fakeRunner{fn: m.fn(t)}).start(t, dir)
	_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
	var na *agent.NotAvailableError
	if !errors.As(err, &na) {
		t.Fatalf("err = %v, want the agent unavailable", err)
	}
	if st := r.Status(); st.Available || st.Problem != ProblemGroupRequired || st.HandoffMode != HandoffGroupRequired {
		t.Fatalf("blocked: available %v, problem %q, mode %q", st.Available, st.Problem, st.HandoffMode)
	}
	if got := r.probeGroup(context.Background()); got != probeDenied || r.Status().Available {
		t.Fatalf("probe while denied = %v, available %v", got, r.Status().Available)
	}
	m.deny.Store(false)
	if got := r.probeGroup(context.Background()); got != probeTaken {
		t.Fatalf("probe once mapped = %v", got)
	}
	if st := r.Status(); !st.Available || st.HandoffMode != HandoffGroup {
		t.Fatalf("after the probe: available %v (%s), mode %q", st.Available, st.Detail, st.HandoffMode)
	}
	if _, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	stop()
	if !slices.Equal(m.modes, []fs.FileMode{credentialGroupDirMode}) || m.ran != 1 {
		t.Fatalf("credential dirs %v, %d runs", m.modes, m.ran)
	}
	assertNoJobDirs(t, dir)
}

// Probe runs only while a fallback holds, once probeEvery has passed since it
// began (or since the last probe), and only for a live runner that takes a
// hand-off and lists the group (allowed: a due probe stages one job; denied:
// none with no fallback, before it is due, or for a runner outside the group,
// which also puts the next probe probeEvery later).
func TestProbeOnlyWhenDue(t *testing.T) {
	t.Parallel()
	r, dir := vaultHandoff(t, []int{os.Getegid()})
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	r.now = func() time.Time { return time.Unix(0, clock.Load()) }
	// runner.json beats on the test's clock, so it stays live as it moves.
	beat := func(groups []int) {
		rj := handoffRunner(true)
		rj.Groups, rj.HeartbeatAt = groups, r.now()
		writeRunnerJSON(t, dir, rj)
	}
	fake := &fakeRunner{fn: func(job string) agentproto.Exit {
		_ = os.Remove(filepath.Join(job, agentproto.CredentialDir, agentproto.CredentialFile))
		return agentproto.Exit{ExitCode: -1, Error: "credential.json's kind isn't one claude takes"}
	}}
	stop := fake.start(t, dir)
	waitProbe := func() {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); r.probing.Load(); {
			if time.Now().After(deadline) {
				t.Fatal("the probe never finished")
			}
			time.Sleep(time.Millisecond)
		}
	}
	r.Probe(context.Background())
	waitProbe()
	if fake.claimed.Load() != 0 {
		t.Fatal("probed with no fallback held")
	}
	r.groupDenied.Store(true)
	r.deferProbe()
	r.Probe(context.Background())
	waitProbe()
	if fake.claimed.Load() != 0 {
		t.Fatal("probed before it was due")
	}
	clock.Add(int64(probeEvery))
	beat([]int{os.Getegid()})
	r.Probe(context.Background())
	waitProbe()
	if fake.claimed.Load() != 1 || r.groupDenied.Load() {
		t.Fatalf("a due probe: %d jobs, group denied %v", fake.claimed.Load(), r.groupDenied.Load())
	}

	// Outside the group: nothing for a probe to find, and runner.json is read
	// again only once the next probe is due.
	r.groupDenied.Store(true)
	clock.Add(int64(probeEvery))
	beat([]int{os.Getegid() + 1})
	r.Probe(context.Background())
	waitProbe()
	if fake.claimed.Load() != 1 {
		t.Fatalf("probed a runner outside the group: %d jobs", fake.claimed.Load())
	}
	beat([]int{os.Getegid()})
	r.Probe(context.Background())
	waitProbe()
	if fake.claimed.Load() != 1 {
		t.Fatalf("probed again before it was due after a gate that failed: %d jobs", fake.claimed.Load())
	}
	clock.Add(int64(probeEvery))
	beat([]int{os.Getegid()})
	r.Probe(context.Background())
	waitProbe()
	stop()
	if fake.claimed.Load() != 2 || r.groupDenied.Load() {
		t.Fatalf("the next due probe: %d jobs, group denied %v", fake.claimed.Load(), r.groupDenied.Load())
	}
	assertNoJobDirs(t, dir)
}
