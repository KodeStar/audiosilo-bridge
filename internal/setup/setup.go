// Package setup is the Bridge's setup checklist (BRIDGE-PLAN.md Phase 2b,
// FIRST-RUN-PLAN.md Phase F2): paired with a server; every library mapping
// checked and writable (decision 13); speech recognition ready (the Bridge's
// own, or a native worker); the agent's credential set (optional since
// bridge#36: only characters, recaps and descriptions need the agent, so a
// Bridge for podcasts, read-along, chapters or Audible completes without it);
// the monthly budget reviewed (decision 7); GitHub signed in (optional, only for
// contributing, decision 17); an Audible account signed in (optional, Phase
// 4a). Checklist
// is pure: the app gathers the inputs, the heartbeat carries the result to the
// server (link.Setup) and the Bridge's page leads its Status tab with it until
// every required item is done.
package setup

import (
	"fmt"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// The items' ids, in the checklist's order (the docs' setup page follows it).
const (
	ItemPaired    = "paired"
	ItemLibraries = "libraries"
	ItemASR       = "asr"
	ItemAgent     = "agent"
	ItemBudget    = "budget"
	ItemGitHub    = "github"
	// ItemAudible (Phase 4a, optional): an Audible account signed in and
	// working. An additive id: an older server or page leaves it out.
	ItemAudible = "audible"
)

// DetailASRWaiting is the asr item's detail while the engine waits for an ASR
// worker (asr.worker_fallback: wait); the item says so in NeedsWorker too, which
// the page and the console link the Workers tab by (bridge#28). A console older
// than that flag matches these words, so keep them stable.
const DetailASRWaiting = "waiting for an ASR worker"

// DetailLibrariesChecking is the libraries item's detail from pairing until the
// first heartbeat has listed the server's libraries and each has been checked
// (BRIDGE-14): before then the Bridge knows none, which is not the server having
// none.
const DetailLibrariesChecking = "checking the libraries..."

// DetailGitHubRefused is the github item's detail while GitHub refuses the
// sign-in (bridge#25: not the same as never signed in).
const DetailGitHubRefused = "GitHub refused the sign-in: sign in again on the Accounts tab"

// agentFor is what the agent is for, in the agent item's detail while it isn't
// ready (BRIDGE-07: plain words, never the staging volume).
const agentFor = "the agent signs in to Claude or ChatGPT and is needed for characters, recaps and descriptions"

// The agent item's detail while no agent container is running, while its image
// isn't this Bridge's version (another protocol, older or newer, or too old to
// take the Accounts tab's credential), and while it runs with no credential;
// agentNoCLI and agentOtherBackend name the backends.
const (
	DetailAgentNotRunning = "the agent container isn't running yet; " + agentFor
	// DetailAgentEndpointNotRunning: the backend is openai-compatible and its
	// own agent service (the compose example's agent-endpoint, in the
	// endpoint profile) isn't running.
	DetailAgentEndpointNotRunning = "the endpoint's agent service (agent-endpoint) isn't running yet: start it with " +
		"docker compose --profile endpoint up -d agent-endpoint; the agent is needed for characters, recaps and descriptions"
	DetailAgentOutdated     = "the agent image isn't this Bridge's version: run the same version of both images; " + agentFor
	DetailAgentNoCredential = "set the agent's sign-in on the Accounts tab; " + agentFor
	// DetailAgentGroupRequired: agent.require_group is on and the credential
	// can't be kept to the Bridge's group (the Accounts tab says why).
	DetailAgentGroupRequired = "agent.require_group is on, and the agent's credential can't be kept to the Bridge's group: " +
		"the Accounts tab says why"
	// DetailAgentShared follows the backend while the agent is ready but its
	// credential goes to every user on the staging volume because something
	// else there interfered with the Bridge's check (the Accounts tab says
	// more; the checklist never names the staging volume, BRIDGE-07).
	DetailAgentShared = ", but its credential is shared with every user on the agent's volume, because something else " +
		"there interfered with the Bridge's check: the Accounts tab says more"
	// DetailAgentHandoffFailed: the Bridge couldn't hand the Accounts tab's
	// credential to the agent container (the Accounts tab says why).
	DetailAgentHandoffFailed = "the Bridge couldn't hand the agent its credential: the Accounts tab says why; " + agentFor
	// DetailAgentNoEndpoint: the backend is openai-compatible and no endpoint
	// address is set (bridge#84).
	DetailAgentNoEndpoint = "set the OpenAI-compatible endpoint's address in Settings > Agent; the agent is needed for " +
		"characters, recaps and descriptions"
	// DetailAgentOwnCredentials: the backend is openai-compatible and the
	// endpoint's agent service holds a credential or reaches the agent home,
	// which an endpoint's run could read or change, so none is run (the
	// Accounts tab says more).
	DetailAgentOwnCredentials = "the endpoint's agent service holds a credential or can reach /agent, which an " +
		"OpenAI-compatible endpoint's run could read: give it neither, or choose claude or codex in Settings > Agent"
	// DetailAgentEndpointService: the backend is openai-compatible and its
	// runs have no agent service of their own apart from the agent container
	// (the Accounts tab says what is wrong).
	DetailAgentEndpointService = "an OpenAI-compatible endpoint needs an agent service of its own, apart from the agent " +
		"container (agent-runner --endpoint-only, its own user, no /agent): the Accounts tab says what to change"
	// DetailAgentEndpointOnly: the backend is claude or codex, and the
	// Bridge's agent dir is the endpoint's agent service.
	DetailAgentEndpointOnly = "for claude or codex the Bridge reaches the OpenAI-compatible endpoint's agent service, " +
		"not the agent container: the Accounts tab says what to change"
)

// agentNoCLI is the detail while the agent image has no CLI for backend
// (codex for an OpenAI-compatible endpoint, which codex runs: bridge#84).
func agentNoCLI(backend string) string {
	if backend == "openai-compatible" {
		backend = "codex"
	}
	return "the agent image has no " + backend + " CLI: use this Bridge's agent image; " + agentFor
}

// agentOtherBackend is the detail while other's CLI is signed in but the Bridge
// runs backend's.
func agentOtherBackend(backend, other string) string {
	return other + " is signed in, but this Bridge runs " + backend + ": set agent.backend: " + other +
		" and restart the Bridge, or set " + backend + "'s sign-in on the Accounts tab"
}

// AgentProblem is why the agent isn't ready: the agent container's state
// (agentbox.Problem, the same strings, converted by the app).
type AgentProblem string

// The problems the checklist words.
const (
	AgentNotRunning     AgentProblem = "not_running"
	AgentOutdated       AgentProblem = "outdated"
	AgentNoCLI          AgentProblem = "no_cli"
	AgentOtherBackend   AgentProblem = "other_backend"
	AgentNoCredential   AgentProblem = "no_credential"
	AgentGroupRequired  AgentProblem = "group_required"
	AgentHandoffFailed  AgentProblem = "handoff_failed"
	AgentNoEndpoint     AgentProblem = "no_endpoint"
	AgentOwnCredentials AgentProblem = "own_credentials"
	AgentEndpointSvc    AgentProblem = "endpoint_service"
	AgentEndpointOnly   AgentProblem = "endpoint_only"
)

// maxDetail bounds an item's detail (the server keeps 256 characters).
const maxDetail = 200

// Library is one server library and its last mapping check.
type Library struct {
	ID       int64
	Mapped   bool
	Writable bool
}

// ASR is where speech recognition runs.
type ASR struct {
	// Started: the engine is up (its first start may be downloading tools).
	Started bool
	// Available: something can transcribe now; Remote that it is a worker.
	Available bool
	Remote    bool
	// Waiting: segments wait for a worker (the fallback is wait, or nothing local).
	Waiting bool
	Detail  string
}

// Agent is the agent container and its credential.
type Agent struct {
	Backend   string
	Available bool
	// Problem: why it isn't ready (unknown reads as not running); Other, for
	// AgentOtherBackend, the CLI that is signed in. Refused: the provider
	// refused its credential, Detail saying where to replace it (the item's
	// detail then).
	Problem AgentProblem
	Other   string
	Refused bool
	Detail  string
	// Handoff is how the Accounts tab's credential reaches the agent
	// container (agentbox.Status.HandoffMode, the same strings as
	// link.AgentCredential*), "" with no hand-off in play.
	Handoff string
	// Shared: the credential goes to every user on the staging volume
	// because something else there spoiled the Bridge's group check
	// (agentbox.Status.Warning).
	Shared bool
}

// GitHub is the contribution credential.
type GitHub struct {
	CanContribute bool
	// Login is the signed-in user; From where the token comes from (env, device, gh).
	Login string
	From  string
	// State, when no token resolves, is why a sign-in can't be used
	// (link.GitHubUnrefreshed, link.GitHubRefused; "" for none), and Why,
	// for an unrefreshed one, why its refresh failed (accounts' words).
	State string
	Why   string
}

// Audible is the Audible accounts: how many are signed in, how many Audible
// refuses (their backups and places wait for a sign-in again), and how many
// more have a device on an older profile (signed in, but Amazon denies their
// licenses until they sign in again). Phase 4c: SyncFailing counts the
// accounts (not refused) whose last position sync failed, SyncError the first
// one's reason; a sync failure never makes the item not done. OtherServer
// counts the accounts mapped on another server than the paired one (not in
// Accounts: they neither poll nor sync here until mapped on this one).
type Audible struct {
	OtherServer int
	Accounts    int
	Refused     int
	OldProfile  int
	SyncFailing int
	SyncError   string
}

// Inputs are what the checklist is made from.
type Inputs struct {
	Paired bool
	// LibrariesPending: paired, but no heartbeat has listed the server's
	// libraries yet, or one listed has no check yet.
	LibrariesPending bool
	Libraries        []Library
	ASR              ASR
	Agent            Agent
	BudgetReviewed   bool
	MonthlyBudgetUSD float64
	GitHub           GitHub
	Audible          Audible
}

// Checklist is the checklist for in, its items in order.
func Checklist(in Inputs) link.Setup {
	out := link.Setup{CanContribute: in.GitHub.CanContribute, GitHubLogin: in.GitHub.Login,
		AgentCredential: agentCredential(in.Agent.Handoff)}
	add := func(id string, done, required bool, detail string) *link.SetupItem {
		out.Items = append(out.Items, link.SetupItem{ID: id, Done: done, Required: required, Detail: cut(detail)})
		return &out.Items[len(out.Items)-1]
	}
	add(ItemPaired, in.Paired, true, "")

	ready, missing := 0, []int64{}
	for _, l := range in.Libraries {
		if l.Mapped && l.Writable {
			ready++
		} else {
			missing = append(missing, l.ID)
		}
	}
	pending := in.Paired && in.LibrariesPending
	libDetail := fmt.Sprintf("%d of %d libraries writable", ready, len(in.Libraries))
	switch {
	case len(in.Libraries) == 0 && !in.Paired:
		// Before pairing there is no server to have libraries. (A refused token
		// isn't paired either but keeps the libraries it had: those are counted.)
		libDetail = "pair with a server first"
	case pending:
		libDetail = DetailLibrariesChecking
	case len(in.Libraries) == 0:
		libDetail = "the server has no libraries yet"
	}
	libs := add(ItemLibraries, !pending && len(in.Libraries) > 0 && len(missing) == 0, true, libDetail)
	if len(missing) > 0 && !pending {
		libs.LibraryIDs = missing
	}

	asrDone := in.ASR.Started && in.ASR.Available && !in.ASR.Waiting
	asrDetail := "local"
	switch {
	case !in.ASR.Started:
		asrDetail = "the engine is starting"
	case in.ASR.Remote && asrDone:
		asrDetail = "worker"
	case in.ASR.Waiting:
		asrDetail = DetailASRWaiting
	case !asrDone:
		asrDetail = firstNonEmpty(in.ASR.Detail, "speech recognition isn't available")
	}
	add(ItemASR, asrDone, true, asrDetail).NeedsWorker = in.ASR.Started && in.ASR.Waiting

	// Optional (the owner's call on bridge#36, reversing FIRST-RUN F2): podcasts,
	// read-along, chapters and Audible need no agent. The Accounts tab carries the
	// agent's full state, the checklist only what to do in plain words.
	agentDetail := in.Agent.Backend
	switch {
	case in.Agent.Available && in.Agent.Shared:
		agentDetail += DetailAgentShared
	case in.Agent.Available:
	case in.Agent.Refused:
		agentDetail = firstNonEmpty(in.Agent.Detail, DetailAgentNoCredential)
	case in.Agent.Problem == AgentOutdated:
		agentDetail = DetailAgentOutdated
	case in.Agent.Problem == AgentNoCLI:
		agentDetail = agentNoCLI(in.Agent.Backend)
	case in.Agent.Problem == AgentOtherBackend && in.Agent.Other != "":
		agentDetail = agentOtherBackend(in.Agent.Backend, in.Agent.Other)
	case in.Agent.Problem == AgentNoCredential, in.Agent.Problem == AgentOtherBackend:
		agentDetail = DetailAgentNoCredential
	case in.Agent.Problem == AgentGroupRequired:
		agentDetail = DetailAgentGroupRequired
	case in.Agent.Problem == AgentHandoffFailed:
		agentDetail = DetailAgentHandoffFailed
	case in.Agent.Problem == AgentNoEndpoint:
		agentDetail = DetailAgentNoEndpoint
	case in.Agent.Problem == AgentOwnCredentials:
		agentDetail = DetailAgentOwnCredentials
	case in.Agent.Problem == AgentEndpointSvc:
		agentDetail = DetailAgentEndpointService
	case in.Agent.Problem == AgentEndpointOnly:
		agentDetail = DetailAgentEndpointOnly
	case in.Agent.Backend == "openai-compatible":
		agentDetail = DetailAgentEndpointNotRunning
	default:
		agentDetail = DetailAgentNotRunning
	}
	add(ItemAgent, in.Agent.Available, false, agentDetail)

	budget := "no monthly cap"
	if in.MonthlyBudgetUSD > 0 {
		budget = fmt.Sprintf("$%s a month", strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", in.MonthlyBudgetUSD), "00"), "."))
	}
	add(ItemBudget, in.BudgetReviewed, true, budget)

	gh := ""
	switch {
	case in.GitHub.CanContribute && in.GitHub.Login != "":
		gh = in.GitHub.Login
	case in.GitHub.CanContribute && in.GitHub.From == "env":
		gh = "a token from the environment"
	case in.GitHub.CanContribute && in.GitHub.From == "gh":
		gh = "the gh CLI's token"
	case in.GitHub.CanContribute:
		// A token resolves with nothing more to say: the State cases are for none.
	case in.GitHub.State == link.GitHubUnrefreshed:
		// Signed in still (bridge#25): it waits for GitHub, not for a sign-in.
		out.GitHubState = link.GitHubUnrefreshed
		gh = "signed in, but the sign-in couldn't be refreshed"
		if in.GitHub.Login != "" {
			gh = "signed in as " + in.GitHub.Login + ", but the sign-in couldn't be refreshed"
		}
		if in.GitHub.Why != "" {
			gh += ": " + in.GitHub.Why
		}
		gh += "; the Bridge tries again every minute"
	case in.GitHub.State == link.GitHubRefused:
		out.GitHubState = link.GitHubRefused
		gh = DetailGitHubRefused
	}
	add(ItemGitHub, in.GitHub.CanContribute, false, gh)

	audible := ""
	a := in.Audible
	switch again := a.Refused + a.OldProfile; {
	case again > 0:
		audible = plural(again, "account needs", "accounts need") + " signing in again"
		if a.OldProfile > 0 {
			audible += " to keep backups working"
		}
		if a.Refused > 0 {
			audible += "; places don't sync until then"
		}
	case a.Accounts > 0:
		audible = plural(a.Accounts, "account", "accounts") + " signed in"
	case a.OtherServer > 0:
		// Every account is another server's (bridge#64): say so, or the item
		// reads as if none was ever signed in.
		audible = plural(a.OtherServer, "account is", "accounts are") +
			" mapped on another server: map one on this server on the Accounts tab"
	}
	if a.SyncFailing > 0 {
		audible += "; places not syncing for " + plural(a.SyncFailing, "account", "accounts")
		if a.SyncError != "" {
			audible += ": " + a.SyncError
		}
	}
	add(ItemAudible, a.Accounts > 0 && a.Refused == 0 && a.OldProfile == 0, false, audible)
	return out
}

// agentCredential is the setup block's agent_credential for a hand-off mode
// ("" for none, or one this package doesn't know).
func agentCredential(mode string) string {
	switch mode {
	case link.AgentCredentialGroup, link.AgentCredentialWorld, link.AgentCredentialGroupRequired, link.AgentCredentialFailed:
		return mode
	}
	return ""
}

// Complete reports whether every required item is done.
func Complete(s link.Setup) bool {
	for _, it := range s.Items {
		if it.Required && !it.Done {
			return false
		}
	}
	return true
}

// plural is "1 <one>" or "n <many>".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// cut bounds a detail at a character boundary.
func cut(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxDetail {
		return s
	}
	return strings.ToValidUTF8(s[:maxDetail-3], "") + "..."
}
