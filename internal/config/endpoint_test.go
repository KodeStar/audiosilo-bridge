package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// endpointConfig is a valid openai-compatible configuration (bridge#84).
func endpointConfig() Config {
	cfg := Default()
	cfg.Agent.Backend = AgentBackendOpenAICompatible
	cfg.Agent.OpenAICompatible = AgentEndpointConfig{BaseURL: "http://192.168.1.20:11434/v1", Model: "qwen3:32b",
		Models: map[string]string{"fact_pass": "qwen3:8b"}}
	return cfg
}

func TestEndpointModels(t *testing.T) {
	cfg := endpointConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	m := cfg.Agent.EndpointModels()
	stages := ModelStages()
	if len(stages) == 0 || len(m) != len(stages) {
		t.Fatalf("models %v for stages %v: every stage resolves", m, stages)
	}
	if m["fact_pass"] != "qwen3:8b" || m["synthesizing"] != "qwen3:32b" || m["describing"] != "qwen3:32b" {
		t.Fatalf("models %v: a stage's own, else the endpoint's model", m)
	}
	cfg.Agent.OpenAICompatible.Model = ""
	if got := cfg.Agent.EndpointModels(); len(got) != 1 {
		t.Fatalf("no model: %v (only the stage with its own)", got)
	}
}

func TestEndpointValidate(t *testing.T) {
	// Allowed: no endpoint at all on another backend; a keyless LAN address; the
	// endpoint set while another backend runs (kept for a later switch).
	for name, mutate := range map[string]func(*Config){
		"claude, no endpoint":   func(c *Config) { c.Agent.Backend = ""; c.Agent.OpenAICompatible = AgentEndpointConfig{} },
		"claude, endpoint kept": func(c *Config) { c.Agent.Backend = AgentBackendClaude },
		"every stage its own": func(c *Config) {
			c.Agent.OpenAICompatible.Model = ""
			c.Agent.OpenAICompatible.Models = map[string]string{}
			for _, st := range ModelStages() {
				c.Agent.OpenAICompatible.Models[st] = "m"
			}
		},
	} {
		cfg := endpointConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, c := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"unknown backend": {func(c *Config) { c.Agent.Backend = "ollama" }, "openai-compatible"},
		"no address":      {func(c *Config) { c.Agent.OpenAICompatible.BaseURL = "" }, "base_url"},
		"no model":        {func(c *Config) { c.Agent.OpenAICompatible.Model = "" }, "agent.openai_compatible.model"},
		"credentials in the address": {func(c *Config) { c.Agent.OpenAICompatible.BaseURL = "https://me:sk-x@h/v1" },
			"user name or password"},
		"a key in the query": {func(c *Config) { c.Agent.OpenAICompatible.BaseURL = "https://h/v1?api_key=x" }, "query"},
		"not http":           {func(c *Config) { c.Agent.OpenAICompatible.BaseURL = "unix:///run/ollama.sock" }, "http"},
		"bad model":          {func(c *Config) { c.Agent.OpenAICompatible.Model = "--sandbox" }, "not a model name"},
		"bad stage":          {func(c *Config) { c.Agent.OpenAICompatible.Models = map[string]string{"asr": "m"} }, "not an agent stage"},
		"bad stage model":    {func(c *Config) { c.Agent.OpenAICompatible.Models = map[string]string{"fact_pass": "a b"} }, "not a model name"},
		"bad address, other backend": {func(c *Config) {
			c.Agent.Backend = AgentBackendClaude
			c.Agent.OpenAICompatible.BaseURL = "ftp://h"
		}, "base_url"},
	} {
		cfg := endpointConfig()
		c.mutate(&cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Validate() = %v, want %q", name, err, c.want)
		}
	}
}

// The environment's endpoint settings lock them on the page and stay out of
// config.yaml; an empty variable counts as unset; config.yaml's values are
// trimmed (a trailing slash, a blank stage model).
func TestEndpointEnvAndFile(t *testing.T) {
	dir := t.TempDir()
	yaml := "agent:\n  backend: openai-compatible\n  openai_compatible:\n    base_url: \" http://192.168.1.20:11434/v1/ \"\n" +
		"    model: qwen3:32b\n    models:\n      fact_pass: \"  \"\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Agent.OpenAICompatible
	if e.BaseURL != "http://192.168.1.20:11434/v1" || e.Model != "qwen3:32b" || e.Models != nil {
		t.Fatalf("normalized %+v", e)
	}
	if cfg.EnvSet(KeyAgentBackend) || cfg.EnvSet(KeyAgentEndpointBaseURL) {
		t.Fatal("nothing set by the environment yet")
	}

	t.Setenv("AUDIOSILO_BRIDGE_AGENT_BACKEND", "")
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_OPENAI_COMPATIBLE_BASE_URL", "https://openrouter.ai/api/v1")
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_OPENAI_COMPATIBLE_MODEL", "openai/gpt-oss-120b")
	cfg, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnvSet(KeyAgentBackend) || cfg.Agent.Backend != AgentBackendOpenAICompatible {
		t.Fatalf("an empty AUDIOSILO_BRIDGE_AGENT_BACKEND is unset: %q locked=%v", cfg.Agent.Backend, cfg.EnvSet(KeyAgentBackend))
	}
	if !cfg.EnvSet(KeyAgentEndpointBaseURL) || !cfg.EnvSet(KeyAgentEndpointModel) ||
		cfg.Agent.OpenAICompatible.BaseURL != "https://openrouter.ai/api/v1" || cfg.Agent.OpenAICompatible.Model != "openai/gpt-oss-120b" {
		t.Fatalf("env %+v", cfg.Agent.OpenAICompatible)
	}
	if err := SaveFile(dir, func(c *Config) error {
		c.Agent.OpenAICompatible.Models = map[string]string{"auditing": "x"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, FileName))
	if strings.Contains(string(raw), "openrouter") || strings.Contains(string(raw), "gpt-oss") || !strings.Contains(string(raw), "auditing: x") {
		t.Fatalf("config.yaml = %s", raw)
	}
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_BACKEND", "codex")
	if cfg, err = Load(dir); err != nil || !cfg.EnvSet(KeyAgentBackend) || cfg.Agent.Backend != AgentBackendCodex {
		t.Fatalf("backend from the environment: %v %q", err, cfg.Agent.Backend)
	}
}
