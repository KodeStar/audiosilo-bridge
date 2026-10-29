// Package setup is the Bridge's setup checklist (BRIDGE-PLAN.md Phase 2b,
// FIRST-RUN-PLAN.md Phase F2): paired with a server; every library mapping
// checked and writable (decision 13); speech recognition ready (the Bridge's
// own, or a native worker); the agent's credential set; the monthly budget
// reviewed (decision 7); GitHub signed in (optional, only for contributing,
// decision 17). Checklist is pure: the app gathers the inputs, the heartbeat
// carries the result to the server (link.Setup) and the Bridge's page leads its
// Status tab with it until every required item is done.
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

// DetailAgentNotRunning and DetailAgentNoCredential are the agent item's detail
// while no agent container is running, and while it runs with no credential.
const (
	DetailAgentNotRunning   = "the agent container isn't running yet; " + agentFor
	DetailAgentNoCredential = "set the agent's sign-in on the Accounts tab; " + agentFor
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
	// Running: the agent container is up (a fresh heartbeat in this Bridge's
	// protocol); Refused: the provider refused its credential, Detail saying
	// where to replace it (the item's detail then).
	Running bool
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

	// The item stays required (FIRST-RUN F2); the Accounts tab carries the
	// agent's full state, the checklist only what to do in plain words.
	agentDetail := in.Agent.Backend
	switch {
	case in.Agent.Available:
	case in.Agent.Refused:
		agentDetail = firstNonEmpty(in.Agent.Detail, DetailAgentNoCredential)
	case !in.Agent.Running:
		agentDetail = DetailAgentNotRunning
	default:
		agentDetail = DetailAgentNoCredential
	}
	add(ItemAgent, in.Agent.Available, true, agentDetail)

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
