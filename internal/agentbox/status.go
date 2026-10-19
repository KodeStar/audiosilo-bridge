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
	// there with a credential the provider hasn't refused.
	Available bool
	// Refused: all of that, except that the provider refused the credential on the
	// last run and the runner reports the same credential still (Detail says so).
	// Runs still go to the container (a refused call costs nothing, and the next one
	// that works clears it); the page and the heartbeat say the agent isn't ready.
	Refused bool
	// Live: a fresh heartbeat in the protocol this Bridge speaks, whatever its CLIs
	// (Slots is then the runner's own).
	Live bool
	// Handoff: the backend's runs get the vault's credential (the Accounts tab):
	// the runner takes a hand-off and has no credential of its own. CLIs then
	// shows that credential's kind for the backend.
	Handoff bool
	// Detail says why not, in words for the page and the park hint.
	Detail      string
	Version     string // the runner's (the Bridge binary's) version
	Slots       int
	HeartbeatAt time.Time
	// CLIs is runner.json's clis as the runner reported them: each CLI's version
	// ("" = not installed) and credential kind (agentproto.Credential*, or "").
	CLIs map[string]agentproto.CLI
}

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

// Status reads runner.json now. Detail never names a Bridge path: it becomes a
// parked book's reason, which the server shows in its console. The path goes to
// the log instead (once per outage, as Status runs on every heartbeat).
func (r *Runner) Status() Status {
	var st Status
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		if !r.dirMissing.Swap(true) {
			r.log.Warn("agent container: the staging dir can't be opened", "dir", r.dir, "err", err)
		}
		st.Detail = "the agent container isn't running (the Bridge can't open its staging dir: check agent.staging_dir and the staging volume)"
		return st
	}
	r.dirMissing.Store(false)
	defer func() { _ = root.Close() }()
	raw, err := agentproto.ReadRegular(root, agentproto.RunnerFile, agentproto.MaxRunnerBytes)
	if errors.Is(err, fs.ErrNotExist) {
		st.Detail = "the agent container isn't running (it hasn't written its status into the staging dir; check that both containers mount the same staging volume)"
		return st
	}
	if err != nil {
		st.Detail = "the agent container's status can't be read (" + ioProblem(err) + ")"
		return st
	}
	var rj agentproto.Runner
	if err := json.Unmarshal(raw, &rj); err != nil {
		st.Detail = "the agent container's status is unreadable: " + err.Error()
		return st
	}
	st.Live = rj.Protocol == agentproto.Protocol && r.now().Sub(rj.HeartbeatAt) <= agentproto.RunnerFreshFor
	if c, ok := rj.CLIs[r.backend]; ok && c.Version != "" && c.Credential == "" && rj.Handoff {
		if kind, _, since, ok := r.handoffCredential(); ok {
			// The vault's credential stands in for the container's: its kind is what
			// the runs are billed as, and its time moves when the Accounts tab
			// replaces it (which ends a refusal of the old one).
			clis := make(map[string]agentproto.CLI, len(rj.CLIs))
			for k, v := range rj.CLIs {
				clis[k] = v
			}
			c.Credential, c.CredentialSince = kind, since.UTC()
			clis[r.backend] = c
			rj.CLIs, st.Handoff = clis, true
		}
	}
	st.Version, st.Slots, st.HeartbeatAt, st.CLIs = rj.Version, rj.Slots, rj.HeartbeatAt, rj.CLIs
	st.Detail = r.unavailableBecause(rj)
	if st.Detail == "" {
		if refused := r.refusal(rj.CLIs[r.backend]); refused != "" {
			st.Refused, st.Detail = true, refused
		}
	}
	st.Available = st.Detail == ""
	return st
}

// handoffCredential is the vault's credential for the backend (ok false for
// none, or no Credential func).
func (r *Runner) handoffCredential() (kind, value string, since time.Time, ok bool) {
	if r.credential == nil {
		return "", "", time.Time{}, false
	}
	return r.credential(r.backend)
}

// CredentialRefusedDetail is the agent's state while the provider refuses its
// credential.
const CredentialRefusedDetail = "the agent's credential was refused by the provider: replace it on the Bridge's " +
	"Accounts tab (or in the agent container)"

// refusedCredential is a credential the provider refused: the runner's report of
// it then (its kind and since when the runner has had it), and the provider's
// words.
type refusedCredential struct {
	kind   string
	since  time.Time
	detail string
}

// noteRefusal remembers that the provider refused the credential c (the backend's
// entry in runner.json when the run was handed over).
func (r *Runner) noteRefusal(c agentproto.CLI, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused = &refusedCredential{kind: c.Credential, since: c.CredentialSince, detail: detail}
}

// clearRefusal forgets a refusal: a run went through, so the credential works.
func (r *Runner) clearRefusal() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused = nil
}

// refusal is the agent's state while the provider refuses the credential the
// runner reports now as c ("" when it doesn't): a credential the runner reports
// as changed since (another kind, or seen since another time: replaced, or the
// container restarted) clears it.
func (r *Runner) refusal(c agentproto.CLI) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refused == nil {
		return ""
	}
	if c.Credential != r.refused.kind || !c.CredentialSince.Equal(r.refused.since) {
		r.refused = nil
		return ""
	}
	return CredentialRefusedDetail + " (" + r.refused.detail + ")"
}

// unavailableBecause is why runs can't go to the container now, or "". Each
// reason reads on its own: it is the page's status line and a parked book's
// reason. The backend is always claude or codex (the app maps the config onto
// one of them).
func (r *Runner) unavailableBecause(rj agentproto.Runner) string {
	if rj.Protocol != agentproto.Protocol {
		return fmt.Sprintf("the agent container speaks protocol %d and this Bridge %d: run the same version of both images", rj.Protocol, agentproto.Protocol)
	}
	if age := r.now().Sub(rj.HeartbeatAt); age > agentproto.RunnerFreshFor {
		return fmt.Sprintf("the agent container isn't running (its last heartbeat was %s ago)", age.Round(time.Second))
	}
	c := rj.CLIs[r.backend]
	if c.Version == "" {
		return "the agent container has no " + r.backend + " CLI"
	}
	if c.Credential == "" {
		why := "codex has no credential: set an OpenAI API key on the Bridge's Accounts tab, or run " +
			"`audiosilo-bridge agent-login codex` in the agent container"
		other := agent.IDClaude
		if r.backend == agent.IDClaude {
			why = "claude has no credential: set one on the Bridge's Accounts tab (a `claude setup-token` value " +
				"or an Anthropic API key), or give one to the agent container"
			other = agent.IDCodex
		}
		if !rj.Handoff {
			if _, _, _, ok := r.handoffCredential(); ok {
				why = r.backend + " has no credential in the agent container, and the container is too old to take " +
					"the one on the Accounts tab: update the agent image to this Bridge's version"
			}
		}
		// The other CLI may be the one signed in: the Bridge runs the one its
		// agent.backend names (claude unless codex is set), never both.
		if o := rj.CLIs[other]; o.Version != "" && o.Credential != "" {
			why += "; " + other + " is signed in: set agent.backend: " + other + " (AUDIOSILO_BRIDGE_AGENT_BACKEND=" + other + ") on the Bridge and restart it to use " + other
		}
		return why
	}
	return ""
}
