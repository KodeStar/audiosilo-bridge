package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/endpoint"
)

// fakeAgent is the fake Backend's Settings > Agent.
type fakeAgent struct {
	saved   []AgentSettings
	saveErr error
	checked []string
}

func (b *backend) AgentSettings(context.Context) AgentSettings {
	s := AgentSettings{Backend: "claude", Running: "claude", Backends: []string{"claude", "codex", "openai-compatible"},
		Stages: []string{"fact_pass"}, Locked: []string{}}
	if n := len(b.agent.saved); n > 0 {
		s.Backend, s.Endpoint = b.agent.saved[n-1].Backend, b.agent.saved[n-1].Endpoint
	}
	return s
}
func (b *backend) SaveAgentSettings(_ context.Context, s AgentSettings) error {
	if b.agent.saveErr != nil {
		return b.agent.saveErr
	}
	b.agent.saved = append(b.agent.saved, s)
	return nil
}
func (b *backend) CheckEndpoint(_ context.Context, baseURL string) (EndpointCheck, error) {
	b.agent.checked = append(b.agent.checked, baseURL)
	if !strings.HasPrefix(baseURL, "http") {
		return EndpointCheck{}, Errorf(ErrInvalid, "the endpoint's address must start with http:// or https://")
	}
	yes := true
	return EndpointCheck{Report: endpoint.Report{Models: []endpoint.Model{{ID: "qwen3:32b", ContextTokens: 40960, Tools: &yes}}, Responses: "yes"}}, nil
}

func TestAgentSettingsRoutes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.do(http.MethodGet, "/api/settings/agent", e.token, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"backends":["claude","codex","openai-compatible"]`) {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}

	body := `{"backend":" openai-compatible ","openai_compatible":{"base_url":"http://192.168.1.20:11434/v1","model":"qwen3:32b","models":{"fact_pass":"qwen3:8b"}}}`
	w = e.do(http.MethodPut, "/api/settings/agent", e.token, body)
	var got AgentSettings
	if err := json.Unmarshal(w.Body.Bytes(), &got); w.Code != http.StatusOK || err != nil {
		t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
	}
	saved := e.b.agent.saved[0]
	if saved.Backend != "openai-compatible" || saved.Endpoint.BaseURL != "http://192.168.1.20:11434/v1" ||
		saved.Endpoint.Model != "qwen3:32b" || saved.Endpoint.Models["fact_pass"] != "qwen3:8b" {
		t.Fatalf("saved %+v", saved)
	}
	if got.Backend != "openai-compatible" {
		t.Fatalf("answer %+v", got)
	}
	if w = e.do(http.MethodPut, "/api/settings/agent", e.token, `{"backend":"claude","key":"sk-x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field (a key never comes this way) = %d", w.Code)
	}
	e.b.agent.saveErr = Errorf(ErrLocked, "AUDIOSILO_BRIDGE_AGENT_BACKEND sets agent.backend")
	if w = e.do(http.MethodPut, "/api/settings/agent", e.token, `{"backend":"codex"}`); w.Code != http.StatusConflict {
		t.Fatalf("locked = %d %s", w.Code, w.Body.String())
	}
	e.b.agent.saveErr = Errorf(ErrInvalid, "agent.openai_compatible.base_url: the endpoint's address has no host")
	if w = e.do(http.MethodPut, "/api/settings/agent", e.token, `{"backend":"openai-compatible"}`); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "has no host") {
		t.Fatalf("invalid = %d %s", w.Code, w.Body.String())
	}

	w = e.do(http.MethodPost, "/api/settings/agent/test", e.token, `{"base_url":" http://192.168.1.20:11434/v1 "}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"qwen3:32b"`) ||
		!strings.Contains(w.Body.String(), `"tools":true`) || e.b.agent.checked[0] != "http://192.168.1.20:11434/v1" {
		t.Fatalf("test = %d %s %v", w.Code, w.Body.String(), e.b.agent.checked)
	}
	if w = e.do(http.MethodPost, "/api/settings/agent/test", e.token, `{"base_url":"ftp://x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("test of a bad address = %d", w.Code)
	}
}

// Security: Settings > Agent's routes refuse a request without a session, and a
// public address even with one (denied); the test connection never runs then.
func TestAgentSettingsRoutesDenied(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	body := `{"backend":"codex","base_url":"http://192.168.1.20:11434/v1"}`
	for _, rt := range [][2]string{{"GET", "/api/settings/agent"}, {"PUT", "/api/settings/agent"},
		{"POST", "/api/settings/agent/test"}} {
		if w := e.do(rt[0], rt[1], "", body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated = %d", rt[0], rt[1], w.Code)
		}
		if w := e.do(rt[0], rt[1], e.token, body, "203.0.113.9:1"); w.Code != http.StatusForbidden {
			t.Errorf("%s %s from a public address = %d", rt[0], rt[1], w.Code)
		}
	}
	if len(e.b.agent.saved) > 0 || len(e.b.agent.checked) > 0 {
		t.Fatalf("a denied request reached the backend: %+v", e.b.agent)
	}
}
