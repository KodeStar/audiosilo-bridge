package app

import (
	"context"
	"maps"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/accounts"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/endpoint"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

// Settings > Agent (bridge#84): the backend, and the OpenAI-compatible endpoint
// the openai-compatible backend runs codex against.

// agentBackends are the agent.backend values the page offers, in its order.
var agentBackends = []string{config.AgentBackendClaude, config.AgentBackendCodex, config.AgentBackendOpenAICompatible}

// AgentSettings is Settings > Agent now.
func (b *Bridge) AgentSettings(ctx context.Context) api.AgentSettings {
	cfg := b.config()
	e := cfg.Agent.OpenAICompatible
	models := map[string]string{}
	maps.Copy(models, e.Models)
	s := api.AgentSettings{Backend: agentBackend(cfg), Running: b.box.ID(), Backends: agentBackends,
		Endpoint: api.AgentEndpoint{BaseURL: e.BaseURL, Model: e.Model, Models: models},
		Stages:   config.ModelStages(), Locked: []string{}}
	st := b.agentCreds.State(ctx)
	s.KeySet, s.KeySetAt = st.Set[accounts.AgentEndpointKey], st.SetAtByKind[accounts.AgentEndpointKey]
	for _, k := range []string{config.KeyAgentBackend, config.KeyAgentEndpointBaseURL, config.KeyAgentEndpointModel} {
		if cfg.EnvSet(k) {
			s.Locked = append(s.Locked, k)
		}
	}
	return s
}

// SaveAgentSettings saves the backend and the endpoint. A setting the
// environment sets must keep its value, and keeps config.yaml's own. The
// endpoint applies at once (the agent box reads its address for every run, the
// engine takes the models now); a new backend at the next start.
func (b *Bridge) SaveAgentSettings(ctx context.Context, s api.AgentSettings) error {
	cur := b.config()
	if s.Backend == "" {
		s.Backend = agentBackend(cur)
	}
	e := config.AgentEndpointConfig{BaseURL: strings.TrimSpace(s.Endpoint.BaseURL), Model: strings.TrimSpace(s.Endpoint.Model),
		Models: s.Endpoint.Models}
	e.BaseURL = strings.TrimRight(e.BaseURL, "/")
	if cur.EnvSet(config.KeyAgentBackend) && s.Backend != agentBackend(cur) {
		return api.Errorf(api.ErrLocked, "AUDIOSILO_BRIDGE_AGENT_BACKEND sets agent.backend")
	}
	if cur.EnvSet(config.KeyAgentEndpointBaseURL) && e.BaseURL != cur.Agent.OpenAICompatible.BaseURL {
		return api.Errorf(api.ErrLocked, "AUDIOSILO_BRIDGE_AGENT_OPENAI_COMPATIBLE_BASE_URL sets agent.openai_compatible.base_url")
	}
	if cur.EnvSet(config.KeyAgentEndpointModel) && e.Model != cur.Agent.OpenAICompatible.Model {
		return api.Errorf(api.ErrLocked, "AUDIOSILO_BRIDGE_AGENT_OPENAI_COMPATIBLE_MODEL sets agent.openai_compatible.model")
	}
	err := b.save(func(c *config.Config) error {
		if !cur.EnvSet(config.KeyAgentBackend) {
			c.Agent.Backend = s.Backend
		}
		if !cur.EnvSet(config.KeyAgentEndpointBaseURL) {
			c.Agent.OpenAICompatible.BaseURL = e.BaseURL
		}
		if !cur.EnvSet(config.KeyAgentEndpointModel) {
			c.Agent.OpenAICompatible.Model = e.Model
		}
		c.Agent.OpenAICompatible.Models = e.Models
		return nil
	})
	if err != nil {
		return err
	}
	next := b.config()
	if eng := b.engine.get(); eng != nil {
		eng.exec.SetCompatibleModels(next.Agent.EndpointModels())
	}
	if next.Agent.OpenAICompatible.BaseURL != cur.Agent.OpenAICompatible.BaseURL && b.box.ID() == config.AgentBackendOpenAICompatible {
		// The books parked on the agent (no endpoint yet, or one that refused)
		// try the new address.
		b.agentChanged("the agent's endpoint changed in Settings > Agent")
	} else {
		b.Kick()
	}
	return nil
}

// CheckEndpoint lists baseURL's models with the endpoint key set now, from the
// Bridge (endpoint.Check). The endpoint failing comes back as the result's
// Error for the page to show.
func (b *Bridge) CheckEndpoint(ctx context.Context, baseURL string) (api.EndpointCheck, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if err := agentproto.CheckBaseURL(baseURL); err != nil {
		return api.EndpointCheck{}, api.Errorf(api.ErrInvalid, "%v", err)
	}
	key, err := b.vault.Get(secrets.EndpointAPIKey)
	if err != nil {
		return api.EndpointCheck{}, api.Errorf(api.ErrFailed, "the endpoint's API key can't be read from the vault")
	}
	out := api.EndpointCheck{Models: []api.EndpointModel{}}
	rep, err := endpoint.Check(ctx, b.endpointClient, baseURL, key)
	if err != nil {
		out.Error = err.Error()
		return out, nil
	}
	for _, m := range rep.Models {
		out.Models = append(out.Models, api.EndpointModel{ID: m.ID, ContextTokens: m.ContextTokens, Tools: m.Tools})
	}
	out.Responses, out.ResponsesDetail = rep.Responses, rep.ResponsesDetail
	return out, nil
}
