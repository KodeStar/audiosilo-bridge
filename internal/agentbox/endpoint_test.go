package agentbox

import (
	"context"
	"errors"
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
// address the test sets (base), with a vault holding key ("" for none).
func endpointRunner(t *testing.T, key string) (*Runner, string, *atomic.Value) {
	t.Helper()
	r, dir := newTestRunner(t, agent.IDOpenAICompatible)
	base := &atomic.Value{}
	base.Store("http://192.168.1.20:11434/v1")
	r.endpoint = func() string { return base.Load().(string) }
	since := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	r.credential = func(backend string) (string, string, time.Time, bool) {
		if backend != agent.IDOpenAICompatible || key == "" {
			return "", "", time.Time{}, false
		}
		return agentproto.CredentialEndpointKey, key, since, true
	}
	rj := handoffRunner(true)
	rj.Endpoints = true
	// The container has no credential of its own: an endpoint's run could read
	// one (TestEndpointRefusedBesideOwnCredentials).
	writeRunnerJSON(t, dir, rj)
	return r, dir, base
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
// commands as the runner's uid: while the container holds a credential of its
// own (a CLI's, or whatever else the runner reports) the agent isn't ready and
// nothing is staged (denied); without one it is (allowed, TestEndpointStatus).
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
		rj := handoffRunner(true)
		rj.Endpoints = true
		if st := r.Status(); !st.Available {
			t.Fatalf("%s: no credential of its own: %+v", name, st)
		}
		hold(&rj)
		writeRunnerJSON(t, dir, rj)
		st := r.Status()
		if st.Available || st.Refused || st.Problem != ProblemOwnCredentials || !strings.Contains(st.Detail, "credential of its own") {
			t.Fatalf("%s: status = %+v", name, st)
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
