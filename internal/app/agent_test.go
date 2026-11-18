package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/accounts"
	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/config"
)

// Settings > Agent (bridge#84): the endpoint saves and applies at once, a new
// backend at the next start; a value config refuses is ErrInvalid, a change to
// one the environment sets ErrLocked.
func TestAgentSettingsSave(t *testing.T) {
	ctx := context.Background()
	b := newTestBridge(t)
	s := b.AgentSettings(ctx)
	if s.Backend != "claude" || s.Running != "claude" || s.KeySet || len(s.Locked) != 0 ||
		len(s.Backends) != 3 || len(s.Stages) == 0 || s.Stages[len(s.Stages)-1] != "describing" {
		t.Fatalf("defaults = %+v", s)
	}
	if err := b.SaveAgentSettings(ctx, api.AgentSettings{Backend: "openai-compatible"}); !errors.Is(err, api.ErrInvalid) ||
		!strings.Contains(err.Error(), "base_url") {
		t.Fatalf("no address = %v", err)
	}
	if err := b.SaveAgentSettings(ctx, api.AgentSettings{Backend: "openai-compatible",
		Endpoint: api.AgentEndpoint{BaseURL: "https://me:sk-x@h/v1", Model: "m"}}); !errors.Is(err, api.ErrInvalid) ||
		strings.Contains(err.Error(), "sk-x") {
		t.Fatalf("credentials in the address = %v", err)
	}
	err := b.SaveAgentSettings(ctx, api.AgentSettings{Backend: "openai-compatible",
		Endpoint: api.AgentEndpoint{BaseURL: " http://192.168.1.20:11434/v1/ ", Model: "qwen3:32b",
			Models: map[string]string{"fact_pass": "qwen3:8b"}}})
	if err != nil {
		t.Fatal(err)
	}
	s = b.AgentSettings(ctx)
	if s.Backend != "openai-compatible" || s.Running != "claude" || s.Endpoint.BaseURL != "http://192.168.1.20:11434/v1" ||
		s.Endpoint.Model != "qwen3:32b" || s.Endpoint.Models["fact_pass"] != "qwen3:8b" {
		t.Fatalf("saved = %+v (the backend applies at the next start)", s)
	}
	raw, _ := os.ReadFile(filepath.Join(b.dataDir, config.FileName))
	if !strings.Contains(string(raw), "openai_compatible:") || !strings.Contains(string(raw), "backend: openai-compatible") {
		t.Fatalf("config.yaml = %s", raw)
	}
	if err := b.SetAgentCredential(ctx, accounts.AgentEndpointKey, "sk-endpoint"); err != nil {
		t.Fatal(err)
	}
	if s = b.AgentSettings(ctx); !s.KeySet || s.KeySetAt == "" {
		t.Fatalf("key = %+v", s)
	}
	if strings.Contains(string(mustRead(t, filepath.Join(b.dataDir, config.FileName))), "sk-endpoint") {
		t.Fatal("the key reached config.yaml")
	}

	// A restart runs the saved backend.
	b2, _, err := New(ctx, Options{DataDir: b.dataDir, Version: "0.2.0", Listen: "127.0.0.1:0", Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b2.db.Close() }()
	if got := b2.AgentSettings(ctx); got.Running != "openai-compatible" {
		t.Fatalf("after a restart = %+v", got)
	}
}

func TestAgentSettingsLocked(t *testing.T) {
	ctx := context.Background()
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_BACKEND", "codex")
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_OPENAI_COMPATIBLE_BASE_URL", "http://10.0.0.5:1234/v1")
	b := newTestBridge(t)
	s := b.AgentSettings(ctx)
	if s.Backend != "codex" || len(s.Locked) != 2 {
		t.Fatalf("locked = %+v", s)
	}
	if err := b.SaveAgentSettings(ctx, api.AgentSettings{Backend: "claude",
		Endpoint: api.AgentEndpoint{BaseURL: "http://10.0.0.5:1234/v1"}}); !errors.Is(err, api.ErrLocked) {
		t.Fatalf("a locked backend changed: %v", err)
	}
	if err := b.SaveAgentSettings(ctx, api.AgentSettings{Backend: "codex",
		Endpoint: api.AgentEndpoint{BaseURL: "http://10.0.0.6:1234/v1"}}); !errors.Is(err, api.ErrLocked) {
		t.Fatalf("a locked address changed: %v", err)
	}
	// Sent back as they are, with a model of the page's own: saved, and the
	// environment's values never reach the file.
	if err := b.SaveAgentSettings(ctx, api.AgentSettings{Backend: "codex",
		Endpoint: api.AgentEndpoint{BaseURL: "http://10.0.0.5:1234/v1", Model: "m"}}); err != nil {
		t.Fatal(err)
	}
	raw := string(mustRead(t, filepath.Join(b.dataDir, config.FileName)))
	if strings.Contains(raw, "10.0.0.5") || strings.Contains(raw, "backend: codex") || !strings.Contains(raw, "model: m") {
		t.Fatalf("config.yaml = %s", raw)
	}
}

// Test connection: from the Bridge, with the endpoint's key from the vault in
// the Authorization header only (allowed); an address config would refuse is
// never requested (denied); an endpoint that fails is the result's Error.
func TestCheckEndpoint(t *testing.T) {
	ctx := context.Background()
	b := newTestBridge(t)
	var mu sync.Mutex
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:32b"},{"id":"tiny","context_length":4096,"supported_parameters":[]}]}`))
		case "/v1/responses":
			w.WriteHeader(http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := b.SetAgentCredential(ctx, accounts.AgentEndpointKey, "sk-endpoint-vault"); err != nil {
		t.Fatal(err)
	}
	res, err := b.CheckEndpoint(ctx, srv.URL+"/v1/")
	if err != nil || res.Error != "" || len(res.Models) != 2 || res.Responses != "yes" {
		t.Fatalf("check = %+v, %v", res, err)
	}
	if m := res.Models[1]; m.ID != "tiny" || m.ContextTokens != 4096 || m.Tools == nil || *m.Tools {
		t.Fatalf("tiny = %+v", m)
	}
	mu.Lock()
	for _, a := range auth {
		if a != "Bearer sk-endpoint-vault" {
			t.Errorf("Authorization %q", a)
		}
	}
	n := len(auth)
	mu.Unlock()
	for _, bad := range []string{"", "ftp://h/v1", "http://u:p@" + strings.TrimPrefix(srv.URL, "http://") + "/v1"} {
		if _, err := b.CheckEndpoint(ctx, bad); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("CheckEndpoint(%q) = %v", bad, err)
		}
	}
	res, err = b.CheckEndpoint(ctx, srv.URL+"/nope")
	if err != nil || res.Error == "" || len(res.Models) != 0 || res.Models == nil {
		t.Fatalf("a wrong path = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auth) != n+1 {
		t.Fatalf("requests = %d, want only the wrong path's one more (a refused address is never requested)", len(auth)-n)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
