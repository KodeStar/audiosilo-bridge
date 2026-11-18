package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/endpoint"
)

// Settings > Agent (bridge#84): which backend the agent container runs (claude,
// codex, or codex against an OpenAI-compatible endpoint), the endpoint's
// address and models, and a check of the endpoint. The endpoint's API key is an
// agent credential like the others (PUT /api/accounts/agent, kind
// endpoint_api_key): taken, never answered back.

// AgentSettings is GET /api/settings/agent.
type AgentSettings struct {
	// Backend is agent.backend as saved (claude, codex or openai-compatible);
	// Running the one the agent runs now. A change of backend applies when
	// the Bridge restarts; the endpoint's address and models apply at once.
	Backend  string        `json:"backend"`
	Running  string        `json:"running_backend"`
	Backends []string      `json:"backends"`
	Endpoint AgentEndpoint `json:"openai_compatible"`
	// Stages are the stages a model can be set for, in the pipeline's order.
	Stages []string `json:"stages"`
	// KeySet: the endpoint's API key is set (KeySetAt since when).
	KeySet   bool   `json:"key_set"`
	KeySetAt string `json:"key_set_at"`
	// Locked names the settings the environment sets (agent.backend,
	// agent.openai_compatible.base_url, agent.openai_compatible.model).
	Locked []string `json:"locked"`
}

// AgentEndpoint is agent.openai_compatible: the endpoint's base URL, the model
// every stage uses, and the stages' own models (a stage left out uses Model).
type AgentEndpoint struct {
	BaseURL string            `json:"base_url"`
	Model   string            `json:"model"`
	Models  map[string]string `json:"models"`
}

// EndpointCheck is POST /api/settings/agent/test: the endpoint's models (each
// with what the endpoint says of its context and tool calling, when it does),
// whether it offers the Responses API (yes, no, unknown), or Error, the reason
// the models couldn't be listed. It is checked from the Bridge.
type EndpointCheck struct {
	endpoint.Report
	Error string `json:"error"`
}

// AgentBackend is Settings > Agent's part of the Backend.
type AgentBackend interface {
	AgentSettings(ctx context.Context) AgentSettings
	// SaveAgentSettings saves the backend and the endpoint (ErrInvalid: a
	// value config refuses, said; ErrLocked: a change to a setting the
	// environment sets).
	SaveAgentSettings(ctx context.Context, s AgentSettings) error
	// CheckEndpoint lists baseURL's models with the endpoint key set
	// (ErrInvalid: an address CheckBaseURL refuses). The endpoint failing is
	// EndpointCheck.Error, not an error.
	CheckEndpoint(ctx context.Context, baseURL string) (EndpointCheck, error)
}

func (a *API) agentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings/agent", a.requireAuth(a.handleAgentSettings))
	mux.HandleFunc("PUT /api/settings/agent", a.requireAuth(a.handleSaveAgentSettings))
	mux.HandleFunc("POST /api/settings/agent/test", a.requireAuth(a.handleCheckEndpoint))
}

func (a *API) handleAgentSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.b.AgentSettings(r.Context()))
}

func (a *API) handleSaveAgentSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Backend  string        `json:"backend"`
		Endpoint AgentEndpoint `json:"openai_compatible"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s := AgentSettings{Backend: strings.TrimSpace(req.Backend), Endpoint: req.Endpoint}
	if err := a.b.SaveAgentSettings(r.Context(), s); err != nil {
		a.writeBackendError(w, err, "save the agent settings")
		return
	}
	writeJSON(w, http.StatusOK, a.b.AgentSettings(r.Context()))
}

func (a *API) handleCheckEndpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"base_url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := a.b.CheckEndpoint(r.Context(), strings.TrimSpace(req.BaseURL))
	if err != nil {
		a.writeBackendError(w, err, "check the endpoint")
		return
	}
	writeJSON(w, http.StatusOK, res)
}
