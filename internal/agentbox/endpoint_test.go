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
	// The container's own codex login is for OpenAI: it never makes an endpoint
	// "signed in", nor goes to one.
	rj.CLIs["codex"] = agentproto.CLI{Version: "0.162.1", Credential: agentproto.CredentialLogin}
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
	r, dir, _ := endpointRunner(t, "sk-endpoint-vault")
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
}
