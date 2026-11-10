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
// worker (asr.worker_fallback: wait). The page matches it (web/src/lib/setup.ts)
// to link the Workers tab, so keep the words stable.
const DetailASRWaiting = "waiting for an ASR worker"

// DetailLibrariesChecking is the libraries item's detail from pairing until the
// first heartbeat has listed the server's libraries and each has been checked
// (BRIDGE-14): before then the Bridge knows none, which is not the server having
// none.
const DetailLibrariesChecking = "checking the libraries..."

// agentFor is what the agent is for, in the agent item's detail while it isn't
// ready (BRIDGE-07: plain words, never the staging volume).
const agentFor = "the agent signs in to Claude or ChatGPT and is needed for characters, recaps and descriptions"

// The agent item's detail while no agent container is running, while its image
// isn't this Bridge's version (another protocol, older or newer, or too old to
// take the Accounts tab's credential), and while it runs with no credential;
// agentNoCLI and agentOtherBackend name the backends.
const (
	DetailAgentNotRunning   = "the agent container isn't running yet; " + agentFor
	DetailAgentOutdated     = "the agent image isn't this Bridge's version: run the same version of both images; " + agentFor
	DetailAgentNoCredential = "set the agent's sign-in on the Accounts tab; " + agentFor
)

// agentNoCLI is the detail while the agent image has no CLI for backend.
func agentNoCLI(backend string) string {
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
	AgentNotRunning   AgentProblem = "not_running"
	AgentOutdated     AgentProblem = "outdated"
	AgentNoCLI        AgentProblem = "no_cli"
	AgentOtherBackend AgentProblem = "other_backend"
	AgentNoCredential AgentProblem = "no_credential"
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
}

// GitHub is the contribution credential.
type GitHub struct {
	CanContribute bool
	// Login is the signed-in user; From where the token comes from (env, device, gh).
	Login string
	From  string
}

// Audible is the Audible accounts: how many are signed in, how many Audible
// refuses (their backups and places wait for a sign-in again), and how many
// more have a device on an older profile (signed in, but Amazon denies their
// licenses until they sign in again). Phase 4c: SyncFailing counts the
// accounts (not refused) whose last position sync failed, SyncError the first
// one's reason; a sync failure never makes the item not done.
type Audible struct {
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
	out := link.Setup{CanContribute: in.GitHub.CanContribute, GitHubLogin: in.GitHub.Login}
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
	add(ItemASR, asrDone, true, asrDetail)

	// Optional (the owner's call on bridge#36, reversing FIRST-RUN F2): podcasts,
	// read-along, chapters and Audible need no agent. The Accounts tab carries the
	// agent's full state, the checklist only what to do in plain words.
	agentDetail := in.Agent.Backend
	switch {
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
