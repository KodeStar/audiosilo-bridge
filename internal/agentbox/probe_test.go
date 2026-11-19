package agentbox

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
	"github.com/kodestar/audiosilo-bridge/internal/testwait"
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
		if err := takeCredential(job); err != nil {
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

// takeCredential takes job's credential as agentrun's runner does: read and
// deleted, then decoded (a probe's is refused, naming its kind).
func takeCredential(job string) error {
	name := filepath.Join(job, agentproto.CredentialDir, agentproto.CredentialFile)
	raw, err := os.ReadFile(name)
	_ = os.Remove(name)
	if err == nil {
		_, err = agentproto.DecodeCredential(raw, agent.IDClaude)
	}
	return err
}

// takeProbe is a runner's take of a probe: its credential taken and refused.
func takeProbe(job string) agentproto.Exit {
	return agentproto.Exit{ExitCode: -1, Error: takeCredential(job).Error()}
}

// The runner's refusal of a probe names the probe's kind, for either CLI:
// probeJob's take hangs on it (allowed: the probe kind refused by name;
// denied: a real credential kind is taken, never refused).
func TestProbeRefusalNamesTheKind(t *testing.T) {
	t.Parallel()
	for _, cli := range []string{agentproto.CLIClaude, agentproto.CLICodex} {
		raw, err := json.Marshal(agentproto.Credential{CLI: cli, Kind: probeKind, Value: probeKind})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := agentproto.DecodeCredential(raw, cli); err == nil || !strings.Contains(err.Error(), strconv.Quote(probeKind)) {
			t.Errorf("%s: the probe's refusal = %v, want it naming %q", cli, err, probeKind)
		}
	}
	raw, _ := json.Marshal(agentproto.Credential{CLI: agentproto.CLIClaude, Kind: agentproto.CredentialAPIKey, Value: "sk-ant-x"})
	if _, err := agentproto.DecodeCredential(raw, agentproto.CLIClaude); err != nil {
		t.Fatalf("a real credential: %v", err)
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
	fake := &fakeRunner{fn: takeProbe}
	stop := fake.start(t, dir)
	waitProbe := func() {
		t.Helper()
		for deadline := time.Now().Add(testwait.Scaled(5 * time.Second)); r.probing.Load(); {
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

// A probe ends the fallback whoever the volume shows the runner as: the id
// mapping's fix gives the runner another uid there, which contests the pin
// (trust.go), and both containers as one user never pin one; runner.json's
// owner stands for the runner then (probeRunner), for its denial too, which
// ends a spoiled probe's warning (allowed: the take ends the fallback with the
// pin contested or never set; denied: a runner still denied the group's file
// keeps it either way).
func TestProbeEndsTheFallbackWithoutAPin(t *testing.T) {
	t.Parallel()
	for name, pinned := range map[string]int64{"contested": contestedUID, "unpinned": noUID} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, dir := vaultHandoff(t, []int{os.Getegid()})
			m := &mappedRunner{}
			m.deny.Store(true)
			stop := (&fakeRunner{fn: m.fn(t)}).start(t, dir)
			r.groupDenied.Store(true)
			r.runnerUID.Store(pinned)
			r.spoiled.Store(&probeSpoil{uid: 4242})
			if got := r.probeGroup(context.Background()); got != probeDenied || !r.groupDenied.Load() || r.spoiled.Load() != nil {
				t.Fatalf("probe while denied = %v, group denied %v, spoiled %+v", got, r.groupDenied.Load(), r.spoiled.Load())
			}
			m.deny.Store(false)
			if got := r.probeGroup(context.Background()); got != probeTaken || r.groupDenied.Load() {
				t.Fatalf("probe once mapped = %v, group denied %v", got, r.groupDenied.Load())
			}
			stop()
			if m.ran != 0 {
				t.Fatalf("a probe reached the CLI %d times", m.ran)
			}
			assertNoJobDirs(t, dir)
		})
	}
}

// A probe claimed but never finished (its heartbeat kept fresh) ends by
// itself after the claim timeout and staleFor, with no ctx deadline of its
// caller's, leaving the fallback and no job dir (allowed: the probe returns;
// denied: it never ends the fallback).
func TestProbeStuckClaimEnds(t *testing.T) {
	t.Parallel()
	r, dir := vaultHandoff(t, []int{os.Getegid()})
	r.groupDenied.Store(true)
	r.claimTimeout = 50 * time.Millisecond
	r.staleFor = 200 * time.Millisecond
	r.cancelFor = 50 * time.Millisecond
	ctx := context.Background()
	stopTouch := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		// A claim that stays fresh and never finishes: the claim and the
		// heartbeat are made once, then only touched (never made again while
		// the probe removes the dir).
		defer wg.Done()
		claimed := map[string]bool{}
		for {
			select {
			case <-stopTouch:
				return
			case <-time.After(5 * time.Millisecond):
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if !e.IsDir() || !agentproto.ValidJobID(e.Name()) {
					continue
				}
				job := filepath.Join(dir, e.Name())
				if !claimed[job] {
					claimed[job] = true
					_ = os.Mkdir(filepath.Join(job, agentproto.ClaimDir), 0o700)
					_ = os.WriteFile(filepath.Join(job, agentproto.HeartbeatFile), nil, 0o644)
				}
				now := time.Now()
				_ = os.Chtimes(filepath.Join(job, agentproto.HeartbeatFile), now, now)
			}
		}
	}()
	done := make(chan probeOutcome, 1)
	go func() { done <- r.probeGroup(ctx) }()
	select {
	case got := <-done:
		if got != probeUnknown || !r.groupDenied.Load() {
			t.Errorf("stuck probe = %v, group denied %v", got, r.groupDenied.Load())
		}
	case <-time.After(testwait.Scaled(10 * time.Second)):
		t.Error("a stuck probe never ended")
	}
	close(stopTouch)
	wg.Wait()
	assertNoJobDirs(t, dir)
}

// A probe cut short by its ctx (the Bridge stopping) is waited for, so its job
// dir is gone by the time Wait returns (allowed: Wait returns once the probe
// took its job back; denied: no job dir outlives it).
func TestProbeWaitedForAtAStop(t *testing.T) {
	t.Parallel()
	r, dir := vaultHandoff(t, []int{os.Getegid()})
	r.groupDenied.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	r.Probe(ctx) // no runner claims it: it waits for the claim
	staged := func() bool {
		entries, _ := os.ReadDir(dir)
		return slices.ContainsFunc(entries, func(e os.DirEntry) bool { return agentproto.ValidJobID(e.Name()) })
	}
	for deadline := time.Now().Add(testwait.Scaled(5 * time.Second)); !staged(); {
		if time.Now().After(deadline) {
			t.Fatal("the probe never staged its job")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	r.Wait()
	if r.probing.Load() || !r.groupDenied.Load() {
		t.Fatalf("after the wait: probing %v, group denied %v", r.probing.Load(), r.groupDenied.Load())
	}
	assertNoJobDirs(t, dir)
}

// A run that learns agent.require_group can't be met just as a probe ends an
// earlier block still answers with its own reason (allowed: the kept reason
// while one is kept; denied: no read of a block a probe took away).
func TestBlockGroupAfterAProbeEndedTheBlock(t *testing.T) {
	t.Parallel()
	r, _ := vaultHandoff(t, []int{os.Getegid()})
	r.requireGroup = true
	first := r.blockGroup("the first reason", "cause")
	var na *agent.NotAvailableError
	if !strings.Contains(first.Error(), "the first reason") {
		t.Fatalf("the first block = %v", first)
	}
	if !errors.As(r.blockGroup("a later reason", "cause"), &na) || !strings.Contains(na.Detail, "the first reason") {
		t.Fatalf("a second block = %v, want the kept reason", na)
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			r.groupBlocked.Store(nil) // as probeGroup's Swap(nil)
		}
	})
	for range 2000 {
		if !errors.As(r.blockGroup("a reason", "cause"), &na) || !strings.Contains(na.Detail, "reason") {
			t.Fatalf("blockGroup = %v", na)
		}
	}
	stop.Store(true)
	wg.Wait()
}
