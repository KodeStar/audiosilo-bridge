package agentbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// Status is the agent container as runner.json describes it now.
type Status struct {
	// Available: a fresh heartbeat, the protocol matches, and the backend's CLI is
	// there with a credential.
	Available bool
	// Detail says why not, in words for the page and the park hint.
	Detail      string
	Version     string // the runner's (the Bridge binary's) version
	Slots       int
	HeartbeatAt time.Time
	CLIs        map[string]CLIStatus
}

// CLIStatus is one CLI in the container: its version ("" = not installed) and the
// kind of credential it has ("oauth_token", "login", "api_key" or "" for none).
type CLIStatus struct{ Version, Credential string }

// Subscription reports whether backend runs on a subscription credential (a
// claude OAuth token or a codex login), which bills no tokens, rather than an
// API key.
func (s Status) Subscription(backend string) bool {
	switch s.CLIs[backend].Credential {
	case agentproto.CredentialOAuthToken, agentproto.CredentialLogin:
		return true
	}
	return false
}

// Status reads runner.json now.
func (r *Runner) Status() Status {
	var st Status
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		st.Detail = "the agent container isn't running (the staging dir " + r.dir + " isn't there)"
		return st
	}
	defer func() { _ = root.Close() }()
	raw, err := readFile(root, agentproto.RunnerFile, agentproto.MaxRunnerBytes)
	if errors.Is(err, fs.ErrNotExist) {
		st.Detail = "the agent container isn't running (it hasn't written its status into the staging dir; check that both containers mount the same staging volume)"
		return st
	}
	if err != nil {
		st.Detail = "the agent container's status can't be read: " + err.Error()
		return st
	}
	var rj agentproto.Runner
	if err := json.Unmarshal(raw, &rj); err != nil {
		st.Detail = "the agent container's status is unreadable: " + err.Error()
		return st
	}
	st.Version, st.Slots, st.HeartbeatAt = rj.Version, rj.Slots, rj.HeartbeatAt
	st.CLIs = make(map[string]CLIStatus, len(rj.CLIs))
	for name, c := range rj.CLIs {
		st.CLIs[name] = CLIStatus{Version: c.Version, Credential: c.Credential}
	}
	st.Detail = r.unavailableBecause(rj)
	st.Available = st.Detail == ""
	return st
}

// unavailableBecause is why runs can't go to the container now, or "".
func (r *Runner) unavailableBecause(rj agentproto.Runner) string {
	if rj.Protocol != agentproto.Protocol {
		return fmt.Sprintf("the agent container speaks protocol %d and this Bridge %d: run the same version of both images", rj.Protocol, agentproto.Protocol)
	}
	if age := r.now().Sub(rj.HeartbeatAt); age > statusFreshFor {
		return fmt.Sprintf("the agent container isn't running (its last heartbeat was %s ago)", age.Round(time.Second))
	}
	if r.backend != agent.IDClaude && r.backend != agent.IDCodex {
		return fmt.Sprintf("unknown agent backend %q (want claude or codex)", r.backend)
	}
	c := rj.CLIs[r.backend]
	if c.Version == "" {
		return "the agent container has no " + r.backend + " CLI"
	}
	if c.Credential == "" {
		if r.backend == agent.IDClaude {
			return "claude has no credential: run `claude setup-token` on any machine and give the token to the agent container as CLAUDE_CODE_OAUTH_TOKEN (or in /agent/secrets/claude_code_oauth_token), or set ANTHROPIC_API_KEY"
		}
		return "codex has no credential: run `audiosilo-bridge agent-login codex` in the agent container, or set OPENAI_API_KEY"
	}
	return ""
}
