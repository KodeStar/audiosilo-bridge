package agentbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
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
	// ShareGroup: a handed credential is the Bridge's group's alone, never
	// every uid's: the runner is in that group (runner.json's groups), or a
	// runner.json naming no group isn't the runner's (trust.go); false once
	// the runner couldn't take one so (Run), until a group probe finds it
	// can (probe.go).
	ShareGroup bool
	// HandoffMode is how a handed credential reaches the runner now
	// (HandoffGroup, HandoffWorld, HandoffGroupRequired, HandoffFailed), ""
	// with no live runner taking a hand-off: the heartbeat's setup block
	// carries it, for the console (bridge#79).
	HandoffMode string
	// Detail says why not, in words for the page and the park hint.
	Detail string
	// Warning, for an agent that is otherwise as described, is what the page
	// says beside it: the handed credential goes to every uid because
	// something else on the staging volume spoiled the group probe that
	// could have ended that (SpoiledWarning), "" otherwise.
	Warning string
	// Problem is why not as a kind (Problem*: "" when Available or Refused), and
	// Other, for ProblemOtherBackend, the CLI that is signed in: the setup
	// checklist words them for a person (internal/setup).
	Problem     Problem
	Other       string
	Version     string // the runner's (the Bridge binary's) version
	Slots       int
	HeartbeatAt time.Time
	// CLIs is runner.json's clis as the runner reported them: each CLI's version
	// ("" = not installed) and credential kind (agentproto.Credential*, or "").
	CLIs map[string]agentproto.CLI

	// groupListed: runner.json's groups hold the Bridge's, so a probe can
	// tell whether the group's file reaches the runner now (probe.go).
	groupListed bool
}

// The hand-off modes (Status.HandoffMode): the credential is the Bridge's
// group's alone; readable by every uid on the staging volume until the
// runner takes it (the runner isn't in the group, the volume refused the
// group, or the runner was denied the group's file); agent.require_group
// is on and can't be met, so nothing is handed over; or the Bridge couldn't
// give the runner its group's file (ProblemHandoffFailed), so nothing is
// handed over until a group probe gets it through.
const (
	HandoffGroup         = "group"
	HandoffWorld         = "world"
	HandoffGroupRequired = "group_required"
	HandoffFailed        = "failed"
)

// SpoiledWarning is Status.Warning while another uid on the staging volume
// spoils the group probe, and spoiledBlocked what agent.require_group's block
// adds then: neither names the uid (the log does).
const (
	SpoiledWarning = "the agent's credential is readable by every user on the staging volume until the agent container " +
		"takes it, not kept to the Bridge's group: something else on the staging volume interfered with the Bridge's " +
		"check that the agent container can take it there (the Bridge's log names its uid); find what else writes " +
		"the staging volume, and the next check ends this"
	spoiledBlocked = "; something else on the staging volume interfered with the Bridge's last check, so it can't " +
		"tell whether the fix is in place (the Bridge's log names its uid): find what else writes the staging volume"
)

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

// Problem is why runs can't go to the agent container, as a kind.
type Problem string

// The problems: no container running (no status, an unreadable one, or a stale
// heartbeat); a container that must be updated to this Bridge's version (another
// protocol, or too old to take the Accounts tab's credential); no CLI for the
// backend; another backend's CLI signed in instead; no credential at all.
const (
	ProblemNotRunning   Problem = "not_running"
	ProblemOutdated     Problem = "outdated"
	ProblemNoCLI        Problem = "no_cli"
	ProblemOtherBackend Problem = "other_backend"
	ProblemNoCredential Problem = "no_credential"
	// ProblemGroupRequired: agent.require_group is on and the handed
	// credential can't be kept to the Bridge's group (Detail says why).
	ProblemGroupRequired Problem = "group_required"
	// ProblemHandoffFailed: the Bridge couldn't give the handed credential to
	// its group, and not because the staging volume refuses that (Detail says
	// why), so no job can be handed over.
	ProblemHandoffFailed Problem = "handoff_failed"
	// ProblemNoEndpoint: the backend is openai-compatible, and no endpoint
	// address is set (bridge#84).
	ProblemNoEndpoint Problem = "no_endpoint"
	// ProblemOwnCredentials: the backend is openai-compatible, and the
	// endpoint's agent service holds a credential of its own or can reach the
	// agent home, which an endpoint's run could read or change (the runner
	// refuses such a run too).
	ProblemOwnCredentials Problem = "own_credentials"
	// ProblemEndpointService: the backend is openai-compatible, and its runs
	// have no agent service of their own apart from the agent container
	// (service.go: none set, a staging dir or a uid shared with the agent
	// container, or a runner that isn't --endpoint-only).
	ProblemEndpointService Problem = "endpoint_service"
	// ProblemEndpointOnly: the backend is claude or codex, and the runner in
	// agent.staging_dir is the endpoint's service, which runs nothing else.
	ProblemEndpointOnly Problem = "endpoint_only"
)

// noEndpointDetail is the agent's state while the openai-compatible backend
// has no endpoint address.
const noEndpointDetail = "no OpenAI-compatible endpoint is set: set its address in Settings > Agent on the Bridge's page"

// ownCredentialsDetail is the agent's state while the endpoint's agent
// service holds a credential of its own or can reach the agent home. An
// endpoint's model is whoever answers at the address, and codex runs its
// commands as the runner's uid with its sandbox off, so the run could read a
// token, key or login and send it anywhere, or plant a config.toml a later
// subscription run loads.
const ownCredentialsDetail = "the endpoint's agent service holds a credential of its own or can reach the agent " +
	"home (/agent), which a run against an OpenAI-compatible endpoint could read or change: give that service no " +
	"credential variables and no /agent volume, and a user of its own (the compose example's agent-endpoint), or " +
	"choose claude or codex in Settings > Agent"

// ownCredentials reports whether the container holds a credential of its own:
// the runner says so, or one of its CLIs has one (backend's entry is the
// Bridge's own making, Status).
func ownCredentials(rj agentproto.Runner, backend string) bool {
	if rj.OwnCredentials {
		return true
	}
	for name, c := range rj.CLIs {
		if name != backend && c.Credential != "" {
			return true
		}
	}
	return false
}

// Status reads runner.json now. Detail never names a Bridge path: it becomes a
// parked book's reason, which the server shows in its console. The path goes to
// the log instead (once per outage, as Status runs on every heartbeat).
func (r *Runner) Status() Status {
	if r.backend == agent.IDOpenAICompatible {
		if why := r.endpointDirProblem(); why != "" {
			return Status{Problem: ProblemEndpointService, Detail: why}
		}
	}
	st := Status{Problem: ProblemNotRunning}
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
	raw, info, err := agentproto.ReadRegularInfo(root, agentproto.RunnerFile, agentproto.MaxRunnerBytes)
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
	if r.cli != r.backend {
		// An endpoint (bridge#84): codex runs it, with the endpoint's key from
		// the vault when one is set and none of the container's own, so its
		// entry starts as codex's version alone and the hand-off below fills in
		// the key (a runner that can't run an endpoint is outdated, below).
		if rj.CLIs == nil {
			rj.CLIs = map[string]agentproto.CLI{}
		}
		rj.CLIs[r.backend] = agentproto.CLI{Version: rj.CLIs[r.cli].Version}
	}
	if c, ok := rj.CLIs[r.backend]; ok && c.Version != "" && c.Credential == "" && rj.Handoff && (r.cli == r.backend || rj.Endpoints) {
		if kind, _, since, ok := r.handoffCredential(r.endpointNow()); ok {
			// The vault's credential stands in for the container's: its kind is what
			// the runs are billed as, and its time moves when the Accounts tab
			// replaces it (which ends a refusal of the old one).
			// (rj was decoded for this call alone, so its map is ours to change.)
			c.Credential, c.CredentialSince = kind, since.UTC()
			rj.CLIs[r.backend], st.Handoff = c, true
		}
	}
	st.Version, st.Slots, st.HeartbeatAt, st.CLIs = rj.Version, rj.Slots, rj.HeartbeatAt, rj.CLIs
	st.groupListed = slices.Contains(rj.Groups, r.gid)
	st.ShareGroup = r.shareGroup(st.groupListed, info, st.Live)
	st.Problem, st.Other, st.Detail = r.unavailableBecause(rj, info)
	if st.Detail == "" && st.Handoff {
		if why := r.groupRequired(st.ShareGroup); why != "" {
			st.Problem, st.Detail = ProblemGroupRequired, why
		}
	}
	if st.Detail == "" && st.Handoff && st.ShareGroup && st.groupListed {
		// Only where a group probe can end it (Probe's gate): a runner.json
		// that isn't the runner's and names no group leaves each job to say so.
		if why := r.groupFailed.Load(); why != nil {
			st.Problem, st.Detail = ProblemHandoffFailed, *why
		}
	}
	if st.Handoff && st.Live {
		st.HandoffMode = r.handoffMode(st)
		if r.spoiled.Load() != nil {
			// Only for the fallbacks a probe could have ended (noteSpoiled).
			switch {
			case st.HandoffMode == HandoffWorld && r.groupDenied.Load():
				st.Warning = SpoiledWarning
			case st.Problem == ProblemGroupRequired && r.groupBlocked.Load() != nil:
				st.Detail += spoiledBlocked
			}
		}
	}
	if st.Detail == "" {
		if refused := r.refusal(rj.CLIs[r.backend], st.Handoff); refused != "" {
			st.Refused, st.Detail = true, refused
		}
	}
	st.Available = st.Detail == ""
	return st
}

// shareGroup reports whether a handed credential goes to the Bridge's group
// alone, by runner.json (read from info; listed when its groups hold the
// Bridge's; live when its heartbeat is fresh in this protocol): when listed,
// and also when another uid than the runner's wrote it (trust.go), so a
// forged one can't move the credential to every uid. Never while the runner
// is denied the group's credential (Run, until a group probe ends it).
func (r *Runner) shareGroup(listed bool, info fs.FileInfo, live bool) bool {
	// Read (and pinned) first whatever the answer: a group probe believes a
	// denial only from the pinned runner (probe.go).
	uid, own := r.fromRunner(info, live)
	if r.groupDenied.Load() {
		return false
	}
	if listed {
		return true
	}
	if !own {
		r.notFromRunner("runner.json naming no Bridge group", uid)
	}
	return !own
}

// handoffMode is st's HandoffMode, for a live runner taking a hand-off.
func (r *Runner) handoffMode(st Status) string {
	switch {
	case st.Problem == ProblemGroupRequired:
		return HandoffGroupRequired
	case st.Problem == ProblemHandoffFailed:
		return HandoffFailed
	case st.ShareGroup && !r.volumeRefused.Load():
		return HandoffGroup
	}
	return HandoffWorld
}

// groupRequired is why agent.require_group stops a handed credential now,
// or "": a run learned the group can't be had (groupBlocked), or the runner
// isn't in the group (share, shareGroup's answer).
func (r *Runner) groupRequired(share bool) string {
	if !r.requireGroup {
		return ""
	}
	if why := r.groupBlocked.Load(); why != nil {
		return *why
	}
	if !share {
		return "agent.require_group is on, and the agent container isn't in the Bridge's group: " + remedyGroupAdd
	}
	return ""
}

// handoffCredential is the vault's credential for the backend (ok false for
// none, or no Credential func), on the openai-compatible backend the key
// entered for the address endpoint ("" on the others).
func (r *Runner) handoffCredential(endpoint string) (kind, value string, since time.Time, ok bool) {
	if r.credential == nil {
		return "", "", time.Time{}, false
	}
	return r.credential(r.backend, endpoint)
}

// CredentialRefusedDetail starts the agent's state while the provider refuses
// its credential; where to replace it follows (refusalAdvice).
const CredentialRefusedDetail = "the agent's credential was refused by the provider"

// refusalAdvice is where a refused credential is replaced: on the Accounts tab
// when it is the one handed over, else in the agent container, whose own
// credential wins over the Accounts tab's; an endpoint's key in Settings >
// Agent, whether one is set or not (a local server may take none).
func (r *Runner) refusalAdvice(handoff bool) string {
	if r.backend == agent.IDOpenAICompatible {
		return ": check the endpoint's API key in Settings > Agent on the Bridge's page"
	}
	if handoff {
		return ": replace it on the Bridge's Accounts tab"
	}
	return ": replace it in the agent container (claude setup-token / agent-login codex), or remove it there " +
		"and set one on the Bridge's Accounts tab"
}

// refusedCredential is a credential the provider refused: the runner's report of
// it then (its kind and since when the runner has had it), the provider's
// words, and the endpoint's address the run went to (openai-compatible's: ""
// for the others).
type refusedCredential struct {
	kind     string
	since    time.Time
	detail   string
	endpoint string
}

// noteRefusal remembers that the provider refused the credential c (the backend's
// entry in runner.json when the run was handed over), at the endpoint address
// the run went to ("" off the openai-compatible backend).
func (r *Runner) noteRefusal(c agentproto.CLI, detail, endpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused = &refusedCredential{kind: c.Credential, since: c.CredentialSince, detail: detail, endpoint: endpoint}
}

// endpointNow is the endpoint's address now on the openai-compatible backend,
// "" on the others (whose refusals an endpoint edit never clears).
func (r *Runner) endpointNow() string {
	if r.backend != agent.IDOpenAICompatible {
		return ""
	}
	return r.endpoint()
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
// container restarted), or another endpoint address (Settings > Agent: another
// provider, maybe one that takes no key), clears it. handoff says c is the
// Accounts tab's credential, handed over, rather than the container's own.
func (r *Runner) refusal(c agentproto.CLI, handoff bool) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refused == nil {
		return ""
	}
	if c.Credential != r.refused.kind || !c.CredentialSince.Equal(r.refused.since) || r.endpointNow() != r.refused.endpoint {
		r.refused = nil
		return ""
	}
	return CredentialRefusedDetail + r.refusalAdvice(handoff) + " (" + r.refused.detail + ")"
}

// unavailableBecause is why runs can't go to the container now, or "". Each
// reason reads on its own: it is the page's status line and a parked book's
// reason. The backend is claude, codex or openai-compatible (the app maps the
// config onto one of them).
func (r *Runner) unavailableBecause(rj agentproto.Runner, info fs.FileInfo) (problem Problem, other, why string) {
	if rj.Protocol != agentproto.Protocol {
		return ProblemOutdated, "", fmt.Sprintf("the agent container speaks protocol %d and this Bridge %d: run the same version of both images", rj.Protocol, agentproto.Protocol)
	}
	if age := r.now().Sub(rj.HeartbeatAt); age > agentproto.RunnerFreshFor {
		return ProblemNotRunning, "", fmt.Sprintf("the agent container isn't running (its last heartbeat was %s ago)", age.Round(time.Second))
	}
	if rj.EndpointOnly && r.backend != agent.IDOpenAICompatible {
		return ProblemEndpointOnly, "", endpointOnlyDetail
	}
	c := rj.CLIs[r.backend]
	if c.Version == "" {
		return ProblemNoCLI, "", "the agent container has no " + r.cli + " CLI"
	}
	if r.backend == agent.IDOpenAICompatible {
		switch {
		case !rj.Endpoints:
			return ProblemOutdated, "", "the agent container is too old to run an OpenAI-compatible endpoint: update the agent image to this Bridge's version"
		case !rj.EndpointOnly:
			return ProblemEndpointService, "", notEndpointOnlyDetail
		case r.sameUserAsAgent(info):
			return ProblemEndpointService, "", sameUserDetail
		case r.endpoint() == "":
			return ProblemNoEndpoint, "", noEndpointDetail
		case ownCredentials(rj, r.backend):
			return ProblemOwnCredentials, "", ownCredentialsDetail
		}
		return "", "", "" // an endpoint may take no key (a local server)
	}
	if c.Credential != "" {
		return "", "", ""
	}
	why = "codex has no credential: set an OpenAI API key on the Bridge's Accounts tab, or run " +
		"`audiosilo-bridge agent-login codex` in the agent container"
	other = agent.IDClaude
	if r.backend == agent.IDClaude {
		why = "claude has no credential: set one on the Bridge's Accounts tab (a `claude setup-token` value " +
			"or an Anthropic API key), or give one to the agent container"
		other = agent.IDCodex
	}
	tooOld := false
	if !rj.Handoff {
		_, _, _, tooOld = r.handoffCredential("")
	}
	if tooOld {
		why = r.backend + " has no credential in the agent container, and the container is too old to take " +
			"the one on the Accounts tab: update the agent image to this Bridge's version"
	}
	// The other CLI may be the one signed in: the Bridge runs the one its
	// agent.backend names (claude unless codex is set), never both.
	o := rj.CLIs[other]
	otherSignedIn := o.Version != "" && o.Credential != ""
	if otherSignedIn {
		why += "; " + other + " is signed in: set agent.backend: " + other + " (AUDIOSILO_BRIDGE_AGENT_BACKEND=" + other + ") on the Bridge and restart it to use " + other
	}
	switch {
	case tooOld:
		return ProblemOutdated, "", why
	case otherSignedIn:
		return ProblemOtherBackend, other, why
	}
	return ProblemNoCredential, "", why
}
