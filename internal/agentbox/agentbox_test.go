package agentbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// newTestRunner is a Runner over a fresh staging dir with a fresh runner.json
// and the timings shortened.
func newTestRunner(t *testing.T, backend string) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	writeRunnerJSON(t, dir, agentproto.Runner{Protocol: 1, Version: "0.2.0", HeartbeatAt: time.Now(), Slots: 1,
		CLIs: map[string]agentproto.CLI{
			"claude": {Version: "2.1.300", Credential: "oauth_token"},
			"codex":  {Version: "0.162.1", Credential: "login"},
		}})
	r := New(Options{Dir: dir, Backend: backend, ClaimTimeout: 300 * time.Millisecond})
	r.poll = 5 * time.Millisecond
	r.staleFor = 300 * time.Millisecond
	r.beatCheck = 5 * time.Millisecond
	r.beatEvery = 10 * time.Millisecond
	r.cancelFor = 2 * time.Second
	return r, dir
}

func writeRunnerJSON(t *testing.T, dir string, rj agentproto.Runner) {
	t.Helper()
	b, err := json.Marshal(rj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, agentproto.RunnerFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stagedDir is a pipeline staged dir: an input and a previous attempt's output.
func stagedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "in.txt"), "input")
	mustWrite(t, filepath.Join(dir, "out", "prev.json"), "{}")
	return dir
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeRunner plays the agent container: it claims the first job that appears,
// runs fn on it (fn writes outputs and returns exit.json), then marks it done.
// skipDone leaves it claimed and unfinished (a dead runner).
type fakeRunner struct {
	fn       func(job string) agentproto.Exit
	skipDone bool
	claimed  atomic.Int32
}

func (f *fakeRunner) start(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	t.Cleanup(func() { cancel(); <-stopped })
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
				if err := os.Mkdir(filepath.Join(job, agentproto.ClaimDir), 0o700); err != nil {
					continue
				}
				f.claimed.Add(1)
				exit := f.fn(job)
				if f.skipDone {
					continue
				}
				b, _ := json.Marshal(exit)
				_ = os.WriteFile(filepath.Join(job, agentproto.ExitFile), b, 0o644)
				_ = os.WriteFile(filepath.Join(job, agentproto.DoneTmpFile), nil, 0o644)
				_ = os.Rename(filepath.Join(job, agentproto.DoneTmpFile), filepath.Join(job, agentproto.DoneFile))
			}
		}
	}()
}

func present(dir, name string) bool {
	_, err := os.Lstat(filepath.Join(dir, name))
	return err == nil
}

// beatFor touches the job's heartbeat for d, as a live runner does.
func beatFor(job string, d time.Duration) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		now := time.Now()
		hb := filepath.Join(job, agentproto.HeartbeatFile)
		if err := os.Chtimes(hb, now, now); err != nil {
			_ = os.WriteFile(hb, nil, 0o644)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const claudeOK = `{"type":"result","subtype":"success","is_error":false,"result":"done","num_turns":3,"total_cost_usd":0.25,"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10}}`

func TestRunClaudeSuccess(t *testing.T) {
	t.Parallel()
	r, dir := newTestRunner(t, agent.IDClaude)
	staged := stagedDir(t)
	var seen agentproto.Request
	var prompt, input string
	var inputMode, outMode, prevMode os.FileMode
	fake := &fakeRunner{fn: func(job string) agentproto.Exit {
		raw, _ := os.ReadFile(filepath.Join(job, agentproto.RequestFile))
		seen, _ = agentproto.DecodeRequest(raw)
		p, _ := os.ReadFile(filepath.Join(job, agentproto.PromptFile))
		prompt = string(p)
		in, _ := os.ReadFile(filepath.Join(job, "work", "in.txt"))
		input = string(in)
		if info, err := os.Stat(filepath.Join(job, "work", "in.txt")); err == nil {
			inputMode = info.Mode().Perm()
		}
		if info, err := os.Stat(filepath.Join(job, "work", "out")); err == nil {
			outMode = info.Mode().Perm()
		}
		if info, err := os.Stat(filepath.Join(job, "work", "out", "prev.json")); err == nil {
			prevMode = info.Mode().Perm()
		}
		beatFor(job, 60*time.Millisecond)
		_ = os.Remove(filepath.Join(job, "work", "out", "prev.json"))
		mustWrite(t, filepath.Join(job, "work", "out", "characters.json"), `{"a":1}`)
		mustWrite(t, filepath.Join(job, "work", "out", "sub", "b.json"), `{"b":2}`)
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile), claudeOK)
		return agentproto.Exit{ExitCode: 0, DurationMS: 60}
	}}
	fake.start(t, dir)

	var beats atomic.Int32
	res, err := r.Run(context.Background(), agent.Request{Dir: staged, Prompt: "do it", Model: "opus", Effort: "high",
		Web: true, MaxTurns: 40, Timeout: 90*time.Second + time.Millisecond, Heartbeat: func(time.Duration) { beats.Add(1) },
		Process: func(int, bool) { t.Error("Process called with a pid from another container") }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.Usage.Input != 100 || res.Usage.CostUSD != 0.25 || !res.Usage.CostReported || res.Usage.Model != "opus" {
		t.Fatalf("result = %+v", res)
	}
	want := agentproto.Request{Protocol: 1, CLI: "claude", Model: "opus", Effort: "high", Web: true, MaxTurns: 40, TimeoutSeconds: 91}
	if seen != want {
		t.Fatalf("request = %+v, want %+v", seen, want)
	}
	if prompt != "do it" || input != "input" {
		t.Fatalf("prompt %q input %q", prompt, input)
	}
	if inputMode != 0o444 || outMode != 0o777 || prevMode != 0o666 {
		t.Errorf("modes: input %v out %v prev %v", inputMode, outMode, prevMode)
	}
	if beats.Load() == 0 {
		t.Error("no heartbeat reached the request while the runner beat")
	}
	if b, _ := os.ReadFile(filepath.Join(staged, "out", "characters.json")); string(b) != `{"a":1}` {
		t.Errorf("characters.json = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(staged, "out", "sub", "b.json")); string(b) != `{"b":2}` {
		t.Errorf("sub/b.json = %q", b)
	}
	if present(filepath.Join(staged, "out"), "prev.json") {
		t.Error("a file the agent deleted came back")
	}
	assertNoJobDirs(t, dir)
}

func assertNoJobDirs(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if agentproto.ValidJobID(e.Name()) {
			t.Errorf("job dir %s left behind", e.Name())
		}
	}
}

func TestRunCodexUsesLastMessage(t *testing.T) {
	t.Parallel()
	r, dir := newTestRunner(t, agent.IDCodex)
	if r.EnforcesNoTools() || !r.SupportsWeb() || r.ID() != "codex" {
		t.Fatal("codex capabilities")
	}
	fake := &fakeRunner{fn: func(job string) agentproto.Exit {
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile),
			`{"type":"item.completed","item":{"type":"agent_message","text":"from stream"}}`+"\n"+
				`{"type":"turn.completed","usage":{"input_tokens":7,"cached_input_tokens":2,"output_tokens":3}}`+"\n")
		mustWrite(t, filepath.Join(job, agentproto.LastMessageFile), "from file\n")
		return agentproto.Exit{}
	}}
	fake.start(t, dir)
	res, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "from file" || res.Usage.Input != 7 || res.Usage.Turns != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunFailures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		exit   agentproto.Exit
		stdout string
		stderr string
		check  func(error) bool
	}{
		"exit code": {exit: agentproto.Exit{ExitCode: 2}, stderr: "boom",
			check: func(err error) bool { return strings.Contains(err.Error(), "claude exited: exit status 2: boom") }},
		"timed out": {exit: agentproto.Exit{ExitCode: 143, TimedOut: true},
			check: func(err error) bool { return strings.Contains(err.Error(), "claude timed out after 1m0s") }},
		"rate limit": {exit: agentproto.Exit{ExitCode: 1}, stdout: `{"type":"result","is_error":true,"result":"rate limit reached|9999999999"}`,
			check: func(err error) bool { var rl *agent.RateLimitError; return errors.As(err, &rl) }},
		"401": {exit: agentproto.Exit{ExitCode: 1}, stdout: `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Invalid API key · Fix external API key"}`,
			check: func(err error) bool {
				var na *agent.NotAvailableError
				return errors.As(err, &na) && strings.Contains(err.Error(), "Invalid API key")
			}},
		"unavailable": {exit: agentproto.Exit{ExitCode: -1, Error: "claude has no credential in the agent container", Unavailable: true},
			check: func(err error) bool { var na *agent.NotAvailableError; return errors.As(err, &na) }},
		"refused": {exit: agentproto.Exit{ExitCode: -1, Error: "request cli \"x\" must be claude or codex"},
			check: func(err error) bool { return strings.Contains(err.Error(), "refused the job") }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, dir := newTestRunner(t, agent.IDClaude)
			fake := &fakeRunner{fn: func(job string) agentproto.Exit {
				mustWrite(t, filepath.Join(job, agentproto.StdoutFile), tc.stdout)
				mustWrite(t, filepath.Join(job, agentproto.StderrFile), tc.stderr)
				return tc.exit
			}}
			fake.start(t, dir)
			_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
			if err == nil || !tc.check(err) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The container's outputs are untrusted: a link (to a file or a dir outside) is
// never followed or copied, while the regular files beside it are harvested.
func TestHarvestSkipsLinks(t *testing.T) {
	t.Parallel()
	r, dir := newTestRunner(t, agent.IDClaude)
	secret := filepath.Join(t.TempDir(), "secret.key")
	mustWrite(t, secret, "the Bridge's secret")
	staged := stagedDir(t)
	fake := &fakeRunner{fn: func(job string) agentproto.Exit {
		out := filepath.Join(job, "work", "out")
		mustWrite(t, filepath.Join(out, "ok.json"), "{}")
		_ = os.Symlink(secret, filepath.Join(out, "leak.json"))
		_ = os.Symlink(filepath.Dir(secret), filepath.Join(out, "escape"))
		_ = os.Symlink("../../"+agentproto.PromptFile, filepath.Join(out, "inside.txt"))
		if runtime.GOOS != "windows" {
			_ = syscall.Mkfifo(filepath.Join(out, "fifo"), 0o644)
		}
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile), claudeOK)
		return agentproto.Exit{}
	}}
	fake.start(t, dir)
	if _, err := r.Run(context.Background(), agent.Request{Dir: staged, Prompt: "p", Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(staged, "out")
	if !present(out, "ok.json") {
		t.Fatal("the regular file wasn't harvested")
	}
	for _, name := range []string{"leak.json", "escape", "inside.txt", "fifo"} {
		if present(out, name) {
			t.Errorf("%s was harvested", name)
		}
	}
}

// A harvest over a limit fails the run and leaves the staged out/ as it was.
func TestHarvestLimits(t *testing.T) {
	t.Parallel()
	for name, fill := range map[string]func(out string){
		"file over the cap": func(out string) {
			_ = os.WriteFile(filepath.Join(out, "big.json"), make([]byte, maxHarvestFile+1), 0o644)
		},
		"too many files": func(out string) {
			for i := range maxHarvestEntries + 1 {
				_ = os.WriteFile(filepath.Join(out, "f"+strconv.Itoa(i)), nil, 0o644)
			}
		},
		"too deep": func(out string) {
			deep := filepath.Join(out, "1", "2", "3", "4", "5", "6", "7", "8")
			_ = os.MkdirAll(deep, 0o755)
			_ = os.WriteFile(filepath.Join(deep, "f.json"), nil, 0o644)
		},
		"over the total": func(out string) {
			for i := range 9 {
				_ = os.WriteFile(filepath.Join(out, "part"+strconv.Itoa(i)), make([]byte, maxHarvestFile), 0o644)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, dir := newTestRunner(t, agent.IDClaude)
			r.staleFor = time.Minute // the fake writes thousands of files without a heartbeat
			staged := stagedDir(t)
			fake := &fakeRunner{fn: func(job string) agentproto.Exit {
				fill(filepath.Join(job, "work", "out"))
				mustWrite(t, filepath.Join(job, agentproto.StdoutFile), claudeOK)
				return agentproto.Exit{}
			}}
			fake.start(t, dir)
			_, err := r.Run(context.Background(), agent.Request{Dir: staged, Prompt: "p", Timeout: time.Minute})
			if err == nil || !strings.Contains(err.Error(), "harvest") {
				t.Fatalf("err = %v", err)
			}
			if !present(filepath.Join(staged, "out"), "prev.json") {
				t.Error("a failed harvest changed the staged out/")
			}
			assertNoJobDirs(t, dir)
		})
	}
}

// A job nobody claims is withdrawn (the Bridge takes the claim itself, so a late
// runner can't start it) and reported as the agent being unavailable.
func TestRunClaimTimeout(t *testing.T) {
	t.Parallel()
	r, dir := newTestRunner(t, agent.IDClaude)
	_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
	var na *agent.NotAvailableError
	if !errors.As(err, &na) || !strings.Contains(err.Error(), "didn't pick the job up") {
		t.Fatalf("err = %v", err)
	}
	assertNoJobDirs(t, dir)
}

// The withdrawal and a runner's claim race on one mkdir: exactly one wins.
func TestUnclaimableLosesToARunner(t *testing.T) {
	t.Parallel()
	r, _ := newTestRunner(t, agent.IDClaude)
	job := t.TempDir()
	root, err := os.OpenRoot(job)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Mkdir(filepath.Join(job, agentproto.ClaimDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if r.unclaimable(root) {
		t.Fatal("the Bridge withdrew a job the runner had claimed")
	}
	other := t.TempDir()
	root2, _ := os.OpenRoot(other)
	defer root2.Close()
	if !r.unclaimable(root2) || os.Mkdir(filepath.Join(other, agentproto.ClaimDir), 0o700) == nil {
		t.Fatal("a runner claimed a withdrawn job")
	}
}

func TestRunStaleHeartbeat(t *testing.T) {
	t.Parallel()
	r, dir := newTestRunner(t, agent.IDClaude)
	fake := &fakeRunner{skipDone: true, fn: func(string) agentproto.Exit { return agentproto.Exit{} }}
	fake.start(t, dir)
	_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
	var na *agent.NotAvailableError
	if err == nil || errors.As(err, &na) || !strings.Contains(err.Error(), "stopped answering") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCancel(t *testing.T) {
	t.Parallel()
	r, dir := newTestRunner(t, agent.IDClaude)
	ctx, cancel := context.WithCancel(context.Background())
	sawCancel := make(chan bool, 1)
	fake := &fakeRunner{fn: func(job string) agentproto.Exit {
		cancel()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) && !present(job, agentproto.CancelFile) {
			beatFor(job, 5*time.Millisecond)
		}
		sawCancel <- present(job, agentproto.CancelFile)
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile), claudeOK)
		return agentproto.Exit{ExitCode: 143, Cancelled: true}
	}}
	fake.start(t, dir)
	res, err := r.Run(ctx, agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if !<-sawCancel {
		t.Fatal("the runner never saw a cancel file")
	}
	if res.Usage.Input != 100 {
		t.Errorf("the usage of a cancelled run was lost: %+v", res.Usage)
	}
	assertNoJobDirs(t, dir)
}

// A cancel before any runner claimed the job withdraws it at once.
func TestRunCancelUnclaimed(t *testing.T) {
	t.Parallel()
	r, dir := newTestRunner(t, agent.IDClaude)
	r.claimTimeout = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
	assertNoJobDirs(t, dir)
}

// Run doesn't stage a job when the container is down: the pipeline parks at once.
func TestRunUnavailableStagesNothing(t *testing.T) {
	t.Parallel()
	r, dir := newTestRunner(t, agent.IDClaude)
	_ = os.Remove(filepath.Join(dir, agentproto.RunnerFile))
	_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p"})
	var na *agent.NotAvailableError
	if !errors.As(err, &na) {
		t.Fatalf("err = %v", err)
	}
	assertNoJobDirs(t, dir)
}

func TestStatus(t *testing.T) {
	t.Parallel()
	now := time.Now()
	full := map[string]agentproto.CLI{
		"claude": {Version: "2.1.300", Credential: "oauth_token"},
		"codex":  {Version: "0.162.1", Credential: "api_key"},
	}
	for name, tc := range map[string]struct {
		backend   string
		rj        *agentproto.Runner
		raw       string
		link      bool // runner.json is a link to a fresh status elsewhere
		available bool
		detail    string
	}{
		"fresh":          {backend: "claude", rj: &agentproto.Runner{Protocol: 1, HeartbeatAt: now, Slots: 1, CLIs: full}, available: true},
		"fresh codex":    {backend: "codex", rj: &agentproto.Runner{Protocol: 1, HeartbeatAt: now, CLIs: full}, available: true},
		"stale":          {backend: "claude", rj: &agentproto.Runner{Protocol: 1, HeartbeatAt: now.Add(-time.Minute), CLIs: full}, detail: "isn't running"},
		"wrong protocol": {backend: "claude", rj: &agentproto.Runner{Protocol: 2, HeartbeatAt: now, CLIs: full}, detail: "protocol 2"},
		"no credential": {backend: "claude", rj: &agentproto.Runner{Protocol: 1, HeartbeatAt: now,
			CLIs: map[string]agentproto.CLI{"claude": {Version: "2.1.300"}}}, detail: "claude has no credential"},
		"codex signed in instead": {backend: "claude", rj: &agentproto.Runner{Protocol: 1, HeartbeatAt: now,
			CLIs: map[string]agentproto.CLI{"claude": {Version: "2.1.300"}, "codex": {Version: "0.162.1", Credential: "login"}}},
			detail: "codex is signed in: set agent.backend: codex"},
		"no cli":  {backend: "codex", rj: &agentproto.Runner{Protocol: 1, HeartbeatAt: now, CLIs: map[string]agentproto.CLI{}}, detail: "no codex CLI"},
		"missing": {backend: "claude", detail: "isn't running"},
		"garbage": {backend: "claude", raw: "{not json", detail: "unreadable"},
		"a link":  {backend: "claude", link: true, detail: "can't be read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.rj != nil {
				writeRunnerJSON(t, dir, *tc.rj)
			}
			if tc.raw != "" {
				mustWrite(t, filepath.Join(dir, agentproto.RunnerFile), tc.raw)
			}
			if tc.link {
				elsewhere := t.TempDir()
				writeRunnerJSON(t, elsewhere, agentproto.Runner{Protocol: 1, HeartbeatAt: now, CLIs: full})
				if err := os.Symlink(filepath.Join(elsewhere, agentproto.RunnerFile), filepath.Join(dir, agentproto.RunnerFile)); err != nil {
					t.Skip("no symlinks here:", err)
				}
			}
			r := New(Options{Dir: dir, Backend: tc.backend})
			st := r.Status()
			if st.Available != tc.available || !strings.Contains(st.Detail, tc.detail) {
				t.Fatalf("status = %+v", st)
			}
			av := r.Detect(context.Background())
			// The container is expected back by itself, so a park on it retries.
			if av.Available != tc.available || av.Detail != st.Detail || av.RetryAfter != RetryAfter {
				t.Fatalf("detect = %+v", av)
			}
		})
	}
}

func TestStatusSubscription(t *testing.T) {
	t.Parallel()
	st := Status{CLIs: map[string]agentproto.CLI{"claude": {Credential: "oauth_token"}, "codex": {Credential: "api_key"}}}
	if !st.Subscription("claude") || st.Subscription("codex") || st.Subscription("other") {
		t.Fatal("subscription")
	}
	st.CLIs["codex"] = agentproto.CLI{Credential: "login"}
	if !st.Subscription("codex") {
		t.Fatal("a codex login is a subscription")
	}
}

// New deals with the job dirs an earlier process left: one older than a day,
// a finished one and an unclaimed one go; one the runner is running gets a
// cancel (its CLI must not run on for nothing) and stays until it is finished;
// anything that isn't a job dir is kept.
func TestNewSweepsOldJobDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := filepath.Join(dir, "00000000000000aa")
	unclaimed := filepath.Join(dir, "00000000000000bb")
	running := filepath.Join(dir, "00000000000000cc")
	finished := filepath.Join(dir, "00000000000000dd")
	other := filepath.Join(dir, "keep-me")
	for _, d := range []string{old, unclaimed, running, finished, other} {
		mustWrite(t, filepath.Join(d, "f"), "x")
	}
	for _, d := range []string{running, finished} {
		if err := os.Mkdir(filepath.Join(d, agentproto.ClaimDir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(finished, agentproto.DoneFile), "")
	long := time.Now().Add(-48 * time.Hour)
	for _, d := range []string{old, other} {
		if err := os.Chtimes(d, long, long); err != nil {
			t.Fatal(err)
		}
	}
	New(Options{Dir: dir, Backend: "claude"})
	for _, d := range []string{old, unclaimed, finished} {
		if present(dir, filepath.Base(d)) {
			t.Errorf("%s was kept", filepath.Base(d))
		}
	}
	if !present(dir, filepath.Base(other)) || !present(running, agentproto.CancelFile) {
		t.Fatal("the dir that isn't a job's went, or the running job got no cancel")
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o777 {
		t.Fatalf("staging dir mode = %v, %v", info.Mode(), err)
	}
}

func TestRequestTimeout(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]int{
		0:                                  agentproto.MaxTimeoutSeconds,
		time.Millisecond:                   1,
		time.Minute:                        60,
		time.Minute + time.Nanosecond:      61,
		48 * time.Hour:                     agentproto.MaxTimeoutSeconds,
		-time.Second:                       agentproto.MaxTimeoutSeconds,
		agentproto.MaxTimeoutSeconds * 1e9: agentproto.MaxTimeoutSeconds,
	} {
		if got := requestTimeout(d); got != want {
			t.Errorf("requestTimeout(%s) = %d, want %d", d, got, want)
		}
	}
}

// The agent uid can rename the Bridge's entries in the shared staging dir and
// put a link in their place: staging opens the dir it just made once, and
// refuses a link swapped in for it, never following it (denied); the dir itself
// opens (allowed), and is opened to the agent uid only once the job is whole.
func TestStageThroughTheJobDirsRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	job := filepath.Join(dir, "00000000000000aa")
	root, err := stage(job, agent.Request{Dir: stagedDir(t), Prompt: "p"}, agent.IDClaude, 60)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	info, err := os.Stat(job)
	if err != nil || info.Mode()&os.ModePerm != 0o777 || info.Mode()&os.ModeSticky == 0 || !present(job, agentproto.RequestFile) {
		t.Fatalf("job dir = %v, %v (request.json %v)", info.Mode(), err, present(job, agentproto.RequestFile))
	}

	swapped := filepath.Join(dir, "00000000000000bb")
	if err := os.Mkdir(swapped, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swapped, swapped+"-aside"); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, swapped); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if r, err := openMadeDir(swapped); err == nil {
		_ = r.Close()
		t.Fatal("a link in the job dir's place was opened")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("something was written through the link: %v", entries)
	}
}
