package agentbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// endpointRunner is a Runner on the openai-compatible backend over an endpoint
// address the test sets (base), with a vault holding key ("" for none) for
// the first address (accounts.Agent hands a key only to the address it was
// entered for), and the endpoint's own service's runner.json in its staging
// dir: --endpoint-only, no credential of its own.
func endpointRunner(t *testing.T, key string) (*Runner, string, *atomic.Value) {
	t.Helper()
	r, dir := newTestRunner(t, agent.IDOpenAICompatible)
	const first = "http://192.168.1.20:11434/v1"
	base := &atomic.Value{}
	base.Store(first)
	r.endpoint = func() string { return base.Load().(string) }
	since := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	r.credential = func(backend, endpoint string) (string, string, time.Time, bool) {
		if backend != agent.IDOpenAICompatible || key == "" || endpoint != first {
			return "", "", time.Time{}, false
		}
		return agentproto.CredentialEndpointKey, key, since, true
	}
	writeRunnerJSON(t, dir, endpointService())
	return r, dir, base
}

// endpointService is the endpoint's own agent service's runner.json.
func endpointService() agentproto.Runner {
	rj := handoffRunner(true)
	rj.Endpoints, rj.EndpointOnly = true, true
	return rj
}

// The openai-compatible backend runs codex: request.json names codex and the
// endpoint, credential.json carries the endpoint's key for codex, and the run
// is reported as the endpoint's (ID, no web search, its own CLIs entry).
func TestEndpointRunIsStaged(t *testing.T) {
	t.Parallel()
	r, dir, _ := endpointRunner(t, "sk-endpoint-vault")
	if r.ID() != agent.IDOpenAICompatible || r.SupportsWeb() || r.EnforcesNoTools() {
		t.Fatalf("capabilities: id %q web %v notools %v", r.ID(), r.SupportsWeb(), r.EnforcesNoTools())
	}
	st := r.Status()
	if !st.Available || !st.Handoff || st.CLIs[agent.IDOpenAICompatible].Credential != agentproto.CredentialEndpointKey ||
		st.Subscription(agent.IDOpenAICompatible) {
		t.Fatalf("status = %+v", st)
	}
	if av := r.Detect(context.Background()); !av.Available || av.Version != "0.162.1" {
		t.Fatalf("detect = %+v", av)
	}
	var request, credential atomic.Value
	fake := &fakeRunner{fn: func(job string) agentproto.Exit {
		raw, _ := os.ReadFile(filepath.Join(job, agentproto.RequestFile))
		request.Store(string(raw))
		raw, _ = os.ReadFile(filepath.Join(job, agentproto.CredentialDir, agentproto.CredentialFile))
		credential.Store(string(raw))
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile),
			`{"type":"turn.completed","usage":{"input_tokens":240,"cached_input_tokens":40,"output_tokens":14}}`+"\n")
		mustWrite(t, filepath.Join(job, agentproto.LastMessageFile), "done")
		return agentproto.Exit{}
	}}
	stop := fake.start(t, dir)
	res, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "qwen3:32b", Web: true,
		Timeout: time.Minute})
	stop()
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.Usage.Input != 240 || res.Usage.Output != 14 || res.Usage.CacheRead != 40 || res.Usage.Model != "qwen3:32b" {
		t.Fatalf("result = %+v (usage recorded as for any codex run)", res)
	}
	req, err := agentproto.DecodeRequest([]byte(request.Load().(string)))
	if err != nil || req.CLI != agentproto.CLICodex || req.Endpoint == nil ||
		req.Endpoint.BaseURL != "http://192.168.1.20:11434/v1" || req.Model != "qwen3:32b" {
		t.Fatalf("request.json = %v, %+v", err, req)
	}
	if strings.Contains(request.Load().(string), "sk-endpoint") {
		t.Fatal("request.json carries the key")
	}
	c, err := agentproto.DecodeCredential([]byte(credential.Load().(string)), agentproto.CLICodex)
	if err != nil || c.Kind != agentproto.CredentialEndpointKey || c.Value != "sk-endpoint-vault" {
		t.Fatalf("credential.json = %v, %+v", err, c)
	}
}

// A local endpoint takes no key: the agent is ready without one and its runs
// carry no credential.json at all (allowed); a runner too old for endpoints is
// outdated and no address is no_endpoint, and nothing is staged then (denied).
func TestEndpointStatus(t *testing.T) {
	t.Parallel()
	r, dir, base := endpointRunner(t, "")
	st := r.Status()
	if !st.Available || st.Handoff || st.Problem != "" {
		t.Fatalf("keyless status = %+v", st)
	}
	var credential atomic.Bool
	fake := &fakeRunner{fn: func(job string) agentproto.Exit {
		if _, err := os.Lstat(filepath.Join(job, agentproto.CredentialDir)); err == nil {
			credential.Store(true)
		}
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile), `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`+"\n")
		return agentproto.Exit{}
	}}
	stop := fake.start(t, dir)
	if _, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "m", Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	stop()
	if credential.Load() {
		t.Fatal("a keyless run staged a credential (the container's own codex login must never go to an endpoint)")
	}

	base.Store("")
	st = r.Status()
	if st.Available || st.Problem != ProblemNoEndpoint || !strings.Contains(st.Detail, "Settings > Agent") {
		t.Fatalf("no address: %+v", st)
	}
	var na *agent.NotAvailableError
	if _, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "m", Timeout: time.Minute}); !errors.As(err, &na) {
		t.Fatalf("a run with no address = %v", err)
	}
	base.Store("http://192.168.1.20:11434/v1")

	old := handoffRunner(true) // Endpoints unset: an agent image from before bridge#84
	writeRunnerJSON(t, dir, old)
	st = r.Status()
	if st.Available || st.Problem != ProblemOutdated || !strings.Contains(st.Detail, "update the agent image") {
		t.Fatalf("an old runner: %+v", st)
	}
	old.CLIs["codex"] = agentproto.CLI{}
	old.Endpoints = true
	writeRunnerJSON(t, dir, old)
	if st = r.Status(); st.Problem != ProblemNoCLI || !strings.Contains(st.Detail, "no codex CLI") {
		t.Fatalf("no codex: %+v", st)
	}
	assertNoJobDirs(t, dir)
}

// A refused endpoint key is remembered as for any credential and says where
// to replace it (Settings > Agent, not the agent container).
func TestEndpointRefusalAdvice(t *testing.T) {
	t.Parallel()
	r, dir, base := endpointRunner(t, "sk-endpoint-vault")
	fake := &fakeRunner{fn: func(job string) agentproto.Exit {
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile),
			`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: invalid_api_key"}}`+"\n")
		return agentproto.Exit{ExitCode: 1}
	}}
	stop := fake.start(t, dir)
	_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "m", Timeout: time.Minute})
	stop()
	var na *agent.NotAvailableError
	if !errors.As(err, &na) || !na.CredentialRefused {
		t.Fatalf("run = %v", err)
	}
	if st := r.Status(); !st.Refused || !strings.Contains(st.Detail, "Settings > Agent") || strings.Contains(st.Detail, "sk-endpoint") {
		t.Fatalf("status = %+v", st)
	}
	// Another address is another provider: the refusal was the old one's.
	base.Store("https://openrouter.ai/api/v1")
	if st := r.Status(); st.Refused || !st.Available {
		t.Fatalf("after a new address: %+v", st)
	}
}

// A refusal is the address's the run went to: one that comes back after the
// address moved (a run in flight over a save) is no refusal of the new one.
func TestEndpointRefusalIsItsRunsAddress(t *testing.T) {
	t.Parallel()
	r, dir, base := endpointRunner(t, "")
	stop := (&fakeRunner{fn: func(job string) agentproto.Exit {
		base.Store("https://openrouter.ai/api/v1") // saved while the run is out
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile),
			`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: no key"}}`+"\n")
		return agentproto.Exit{ExitCode: 1}
	}}).start(t, dir)
	_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "m", Timeout: time.Minute})
	stop()
	var na *agent.NotAvailableError
	if !errors.As(err, &na) || !na.CredentialRefused {
		t.Fatalf("run = %v", err)
	}
	if st := r.Status(); st.Refused || !st.Available {
		t.Fatalf("the old address's refusal held against the new one: %+v", st)
	}
}

// The key handed to a run is the one entered for the very address the run
// goes to: when the address moves while the run starts (a save, then the new
// address's key), the run still goes to the address it read, with that
// address's own key or none, never the new address's (denied: the new key to
// the old address; allowed: the old key to the old address).
func TestEndpointKeyNeverFollowsAMovedAddress(t *testing.T) {
	t.Parallel()
	r, dir, base := endpointRunner(t, "")
	const first, moved = "http://192.168.1.20:11434/v1", "https://elsewhere.example/v1"
	keys := map[string]string{first: "sk-first-key", moved: "sk-moved-key"}
	r.credential = func(_, endpoint string) (string, string, time.Time, bool) {
		if k, ok := keys[endpoint]; ok {
			return agentproto.CredentialEndpointKey, k, time.Time{}, true
		}
		return "", "", time.Time{}, false
	}
	// The address moves right after Run's own read of it: count a Status's
	// reads (Run's starts with one), then move it on the read after those.
	orig := r.endpoint
	var calls atomic.Int32
	r.endpoint = func() string { calls.Add(1); return orig() }
	r.Status()
	perStatus := calls.Swap(0)
	r.endpoint = func() string {
		v := orig()
		if calls.Add(1) == perStatus+1 {
			base.Store(moved)
		}
		return v
	}
	var request, credential atomic.Value
	stop := (&fakeRunner{fn: func(job string) agentproto.Exit {
		raw, _ := os.ReadFile(filepath.Join(job, agentproto.RequestFile))
		request.Store(string(raw))
		raw, _ = os.ReadFile(filepath.Join(job, agentproto.CredentialDir, agentproto.CredentialFile))
		credential.Store(string(raw))
		mustWrite(t, filepath.Join(job, agentproto.StdoutFile), `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`+"\n")
		return agentproto.Exit{}
	}}).start(t, dir)
	_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "m", Timeout: time.Minute})
	stop()
	if err != nil {
		t.Fatal(err)
	}
	req, err := agentproto.DecodeRequest([]byte(request.Load().(string)))
	if err != nil || req.Endpoint == nil || req.Endpoint.BaseURL != first {
		t.Fatalf("request.json = %v, %+v", err, req)
	}
	c, err := agentproto.DecodeCredential([]byte(credential.Load().(string)), agentproto.CLICodex)
	if err != nil || c.Value != "sk-first-key" {
		t.Fatalf("credential.json = %v, %+v, want the first address's key", err, c)
	}
	if base.Load() != moved {
		t.Fatal("the address never moved under the run")
	}
}

// A short stand-in key (a local server that takes any) is not cut from the
// run's output: cutting "e" or "none" out of codex's JSON stream would lose
// the run's usage and garble its answer (allowed: the output read whole; a
// real key is cut, TestEndpointKeyCutFromOutput).
func TestEndpointShortKeyLeavesTheOutputWhole(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"e", "none", "ollama"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			r, dir, _ := endpointRunner(t, key)
			stop := (&fakeRunner{fn: func(job string) agentproto.Exit {
				mustWrite(t, filepath.Join(job, agentproto.StdoutFile),
					`{"type":"turn.completed","usage":{"input_tokens":240,"cached_input_tokens":40,"output_tokens":14}}`+"\n")
				mustWrite(t, filepath.Join(job, agentproto.LastMessageFile), "none of the llamas were done; ollama")
				return agentproto.Exit{}
			}}).start(t, dir)
			res, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "m", Timeout: time.Minute})
			stop()
			if err != nil || res.Usage.Input != 240 || res.Usage.Output != 14 || res.Text != "none of the llamas were done; ollama" {
				t.Fatalf("run = %+v, %v", res, err)
			}
		})
	}
}

// A group probe on the openai-compatible backend goes as an endpoint run: the
// runner reads an endpoint run's credential whatever the container has of its
// own, and a plain codex run's never while the container is signed in (the
// fake runner below is), so only so can the probe's take end a fallback
// (allowed: the endpoint probe taken; denied: a plain codex probe, which the
// signed-in runner never decodes, would leave the fallback for good).
func TestEndpointProbe(t *testing.T) {
	t.Parallel()
	r, dir, _ := endpointRunner(t, "sk-endpoint-vault")
	r.Status() // pins the runner's uid, as the heartbeat does
	r.groupDenied.Store(true)
	stop := (&fakeRunner{fn: func(job string) agentproto.Exit {
		raw, _ := os.ReadFile(filepath.Join(job, agentproto.RequestFile))
		req, err := agentproto.DecodeRequest(raw)
		if err != nil {
			return agentproto.Exit{ExitCode: -1, Error: err.Error()}
		}
		name := filepath.Join(job, agentproto.CredentialDir, agentproto.CredentialFile)
		cred, rerr := os.ReadFile(name)
		_ = os.Remove(name)
		if req.Endpoint == nil {
			// agentrun's takeCredential with a codex login of the container's own.
			return agentproto.Exit{ExitCode: -1, Error: "open prompt.txt: no such file or directory"}
		}
		if rerr != nil {
			return agentproto.Exit{ExitCode: -1, Error: rerr.Error()}
		}
		if _, err := agentproto.DecodeCredential(cred, req.CLI); err != nil {
			return agentproto.Exit{ExitCode: -1, Error: err.Error()}
		}
		return agentproto.Exit{}
	}}).start(t, dir)
	defer stop()
	if got := r.probeGroup(context.Background()); got != probeTaken || r.groupDenied.Load() {
		t.Fatalf("probe = %v, group denied %v", got, r.groupDenied.Load())
	}
}

// An endpoint that repeats the key it was sent in its error never gets it into
// the run's error or the agent's state, refused or not (allowed: the
// endpoint's words; denied: the key among them).
func TestEndpointKeyCutFromOutput(t *testing.T) {
	t.Parallel()
	const key = "sk-endpoint-vault"
	for name, c := range map[string]struct {
		stdout  string
		refused bool
	}{
		"refused": {`{"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized: bad key ` + key + `"}}`, true},
		"failed":  {`{"type":"turn.failed","error":{"message":"unexpected status 500: upstream got ` + key + `"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, dir, _ := endpointRunner(t, key)
			stop := (&fakeRunner{fn: func(job string) agentproto.Exit {
				mustWrite(t, filepath.Join(job, agentproto.StdoutFile), c.stdout+"\n")
				mustWrite(t, filepath.Join(job, agentproto.StderrFile), "Authorization: Bearer "+key+"\n")
				return agentproto.Exit{ExitCode: 1}
			}}).start(t, dir)
			_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "m", Timeout: time.Minute})
			stop()
			var na *agent.NotAvailableError
			if err == nil || errors.As(err, &na) != c.refused || strings.Contains(err.Error(), key) ||
				!strings.Contains(err.Error(), "[the key]") {
				t.Fatalf("run = %v", err)
			}
			if st := r.Status(); strings.Contains(st.Detail, key) {
				t.Fatalf("status = %+v", st)
			}
		})
	}
}

// A stage with no endpoint model (cleared on the page while another backend
// waits for the restart) is the agent unavailable, saying where to set one,
// and nothing is staged: the runner would refuse the request (denied).
func TestEndpointRunWithoutModel(t *testing.T) {
	t.Parallel()
	r, dir, _ := endpointRunner(t, "sk-endpoint-vault")
	var na *agent.NotAvailableError
	_, err := r.Run(context.Background(), agent.Request{Stage: "fact_pass", Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
	if !errors.As(err, &na) || !strings.Contains(na.Detail, "fact_pass") || !strings.Contains(na.Detail, "Settings > Agent") {
		t.Fatalf("a run with no model = %v", err)
	}
	assertNoJobDirs(t, dir)
}

// An endpoint's model is whoever answers at the address, and codex runs its
// commands as the runner's uid: while the endpoint's service holds a
// credential of its own (a CLI's, or whatever else the runner reports: the
// agent home within its reach) the agent isn't ready and nothing is staged
// (denied); without one it is (allowed, TestEndpointStatus).
func TestEndpointRefusedBesideOwnCredentials(t *testing.T) {
	t.Parallel()
	for name, hold := range map[string]func(rj *agentproto.Runner){
		"codex's login": func(rj *agentproto.Runner) {
			rj.CLIs["codex"] = agentproto.CLI{Version: "0.162.1", Credential: agentproto.CredentialLogin}
		},
		"claude's token": func(rj *agentproto.Runner) {
			rj.CLIs["claude"] = agentproto.CLI{Version: "2.1.0", Credential: agentproto.CredentialOAuthToken}
		},
		"the runner's word": func(rj *agentproto.Runner) { rj.OwnCredentials = true },
	} {
		r, dir, _ := endpointRunner(t, "sk-endpoint-vault")
		rj := endpointService()
		if st := r.Status(); !st.Available {
			t.Fatalf("%s: no credential of its own: %+v", name, st)
		}
		hold(&rj)
		writeRunnerJSON(t, dir, rj)
		st := r.Status()
		if st.Available || st.Refused || st.Problem != ProblemOwnCredentials || !strings.Contains(st.Detail, "credential of its own") {
			t.Fatalf("%s: status = %+v", name, st)
		}
		if st.Found != "" {
			t.Fatalf("%s: found %q, with none reported", name, st.Found)
		}
		if av := r.Detect(context.Background()); av.Available {
			t.Fatalf("%s: detect = %+v", name, av)
		}
		var na *agent.NotAvailableError
		_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "m", Timeout: time.Minute})
		if !errors.As(err, &na) || na.Detail != st.Detail {
			t.Fatalf("%s: a run = %v", name, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() {
				t.Fatalf("%s: a job was staged (%s)", name, e.Name())
			}
		}
	}
}

// What the endpoint's service found is Status.Found (trimmed: runner.json is
// the service's, which a run could rewrite), for Settings > Agent to name the
// file to remove; never in Detail, which a parked book's reason carries. It
// is reported only with the refusal (a found beside no refusal is dropped).
func TestEndpointStatusNamesWhatWasFound(t *testing.T) {
	t.Parallel()
	r, dir, base := endpointRunner(t, "sk-endpoint-vault")
	first := base.Load().(string)
	rj := endpointService()
	rj.OwnCredentials, rj.Found = true, "/mnt/x holds /mnt/x/agent/.audiosilo-agent-home\x1b[2J"
	writeRunnerJSON(t, dir, rj)
	st := r.Status()
	if st.Problem != ProblemOwnCredentials || st.Found != "/mnt/x holds /mnt/x/agent/.audiosilo-agent-home?[2J" {
		t.Fatalf("status = %+v", st)
	}
	if strings.Contains(st.Detail, "/mnt/x") {
		t.Fatalf("Detail names the path: %q", st.Detail)
	}
	// With no endpoint address either, the credential is still what is
	// reported (denied): no_endpoint reads as a running service on the page.
	base.Store("")
	if st := r.Status(); st.Problem != ProblemOwnCredentials || st.Found == "" {
		t.Fatalf("no address beside a credential: %+v", st)
	}
	base.Store(first)
	rj.OwnCredentials = false
	writeRunnerJSON(t, dir, rj)
	if st := r.Status(); !st.Available || st.Found != "" {
		t.Fatalf("no refusal: %+v", st)
	}
}

// While the endpoint's agent service isn't running, the agent's state names
// that service and how to start it (its compose profile), never the agent
// container; the claude and codex backends name the agent container.
func TestEndpointNotRunningNamesItsService(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		runnerJSON func(t *testing.T, dir string)
		want       string
	}{
		"no runner.json": {runnerJSON: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, agentproto.RunnerFile)); err != nil {
				t.Fatal(err)
			}
		}, want: "hasn't written its status"},
		"a stale heartbeat": {runnerJSON: func(t *testing.T, dir string) {
			rj := endpointService()
			rj.HeartbeatAt = time.Now().Add(-time.Hour)
			writeRunnerJSON(t, dir, rj)
		}, want: "last heartbeat was"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, _, _ := endpointRunner(t, "")
			r.dir = t.TempDir() // a first read
			writeRunnerJSON(t, r.dir, endpointService())
			tc.runnerJSON(t, r.dir)
			st := r.Status()
			if st.Problem != ProblemNotRunning || !strings.Contains(st.Detail, tc.want) ||
				!strings.Contains(st.Detail, "agent-endpoint") || !strings.Contains(st.Detail, "--profile endpoint") ||
				strings.Contains(st.Detail, "agent container") {
				t.Fatalf("status = %+v", st)
			}
		})
	}
	r, dir := newTestRunner(t, agent.IDClaude)
	if err := os.Remove(filepath.Join(dir, agentproto.RunnerFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if st := r.Status(); st.Problem != ProblemNotRunning || !strings.Contains(st.Detail, "the agent container isn't running") ||
		strings.Contains(st.Detail, "agent-endpoint") {
		t.Fatalf("claude: %+v", st)
	}
}

// The openai-compatible backend's runs go only to the endpoint's own agent
// service: with none set, a staging dir shared with the agent container's
// (one dir, or one inside the other), a runner that isn't --endpoint-only, or
// one running as the agent container's user, the agent isn't ready, says why,
// and nothing is staged (denied); a service of its own is ready (allowed, as
// is one beside an agent container that isn't running). And the claude and
// The endpoint backend runs one job at a time whatever runner.json says: a
// run can rewrite it, and a job staged beside a live run holds a key that run
// could read (denied: slots 4 read as 1; allowed: claude follows the file).
func TestEndpointSlotsCappedAtOne(t *testing.T) {
	t.Parallel()
	r, dir, _ := endpointRunner(t, "")
	rj := endpointService()
	rj.Slots = 4
	writeRunnerJSON(t, dir, rj)
	if st := r.Status(); st.Slots != 1 {
		t.Fatalf("endpoint slots = %d, want 1", st.Slots)
	}
	c, cdir := newTestRunner(t, agent.IDClaude)
	crj := handoffRunner(true)
	crj.Slots = 4
	writeRunnerJSON(t, cdir, crj)
	if st := c.Status(); st.Slots != 4 {
		t.Fatalf("claude slots = %d, want runner.json's 4", st.Slots)
	}
}

// codex backends never send their runs to the endpoint's service.
func TestEndpointServiceRequired(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, r *Runner, dir string, want Problem, says string) {
		t.Helper()
		st := r.Status()
		if st.Available || st.Refused || st.Problem != want || !strings.Contains(st.Detail, says) {
			t.Fatalf("status = %+v, want %s saying %q", st, want, says)
		}
		var na *agent.NotAvailableError
		_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Model: "m", Timeout: time.Minute})
		if !errors.As(err, &na) || na.Detail != st.Detail {
			t.Fatalf("a run = %v", err)
		}
		if dir != "" {
			assertNoJobDirs(t, dir)
		}
	}
	t.Run("none set", func(t *testing.T) {
		t.Parallel()
		r := New(Options{Backend: agent.IDOpenAICompatible, AgentDir: t.TempDir(), Endpoint: func() string { return "http://h/v1" }})
		refused(t, r, "", ProblemEndpointService, "agent.endpoint_staging_dir")
	})
	for name, layout := range map[string]func(t *testing.T, endpoint string) string{
		"the same dir":          func(_ *testing.T, endpoint string) string { return endpoint },
		"the agent's inside it": func(_ *testing.T, endpoint string) string { return filepath.Join(endpoint, "agent") },
		"inside the agent's":    func(_ *testing.T, endpoint string) string { return filepath.Dir(endpoint) },
		"the same dir by another name": func(t *testing.T, endpoint string) string {
			// A symlink: no path test sees the one dir under both names.
			link := filepath.Join(t.TempDir(), "agent")
			if err := os.Symlink(endpoint, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"a link to a dir inside it": func(t *testing.T, endpoint string) string {
			inside := filepath.Join(endpoint, "agent")
			if err := os.Mkdir(inside, 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(t.TempDir(), "agent")
			if err := os.Symlink(inside, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"inside the agent's, by a link": func(t *testing.T, endpoint string) string {
			link := filepath.Join(t.TempDir(), "agent")
			if err := os.Symlink(filepath.Dir(endpoint), link); err != nil {
				t.Fatal(err)
			}
			return link
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, dir, _ := endpointRunner(t, "")
			r.agentDir = layout(t, dir)
			refused(t, r, dir, ProblemEndpointService, "shares a folder")
		})
	}
	t.Run("not --endpoint-only", func(t *testing.T) {
		t.Parallel()
		r, dir, _ := endpointRunner(t, "")
		rj := endpointService()
		rj.EndpointOnly = false
		writeRunnerJSON(t, dir, rj)
		refused(t, r, dir, ProblemEndpointService, "--endpoint-only")
	})
	t.Run("the agent container's user", func(t *testing.T) {
		t.Parallel()
		r, dir, _ := endpointRunner(t, "")
		r.agentDir = t.TempDir()
		agentRJ := handoffRunner(true)
		writeRunnerJSON(t, r.agentDir, agentRJ) // this test's uid wrote both
		if _, _, ok := fileOwner(mustStat(t, filepath.Join(dir, agentproto.RunnerFile))); !ok {
			t.Skip("no file owners here")
		}
		refused(t, r, dir, ProblemEndpointService, "same user")
		// A stopped agent container says nothing of its user (allowed): one
		// that left an hour-old runner.json, read for the first time.
		r.agentDir = t.TempDir()
		agentRJ.HeartbeatAt = time.Now().Add(-time.Hour)
		writeRunnerJSON(t, r.agentDir, agentRJ)
		if st := r.Status(); !st.Available {
			t.Fatalf("beside a stopped agent container: %+v", st)
		}
	})
	t.Run("a service of its own", func(t *testing.T) {
		t.Parallel()
		r, _, _ := endpointRunner(t, "")
		r.agentDir = t.TempDir() // no runner.json: the agent container isn't running
		if st := r.Status(); !st.Available || st.Problem != "" {
			t.Fatalf("status = %+v", st)
		}
	})
	t.Run("claude's runs to the endpoint's service", func(t *testing.T) {
		t.Parallel()
		r, dir := newTestRunner(t, agent.IDClaude)
		rj := endpointService()
		rj.CLIs["claude"] = agentproto.CLI{Version: "2.1.300", Credential: agentproto.CredentialOAuthToken}
		writeRunnerJSON(t, dir, rj)
		refused(t, r, dir, ProblemEndpointOnly, "agent.staging_dir")
	})
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
