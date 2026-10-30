package setup

import (
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

func item(t *testing.T, s link.Setup, id string) link.SetupItem {
	t.Helper()
	for _, it := range s.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %q in %+v", id, s.Items)
	return link.SetupItem{}
}

func done() Inputs {
	return Inputs{
		Paired:         true,
		Libraries:      []Library{{ID: 1, Mapped: true, Writable: true}, {ID: 3, Mapped: true, Writable: true}},
		ASR:            ASR{Started: true, Available: true},
		Agent:          Agent{Backend: "claude", Available: true},
		BudgetReviewed: true, MonthlyBudgetUSD: 100,
	}
}

func TestChecklistOrderAndComplete(t *testing.T) {
	s := Checklist(done())
	var ids []string
	for _, it := range s.Items {
		ids = append(ids, it.ID)
	}
	if strings.Join(ids, ",") != "paired,libraries,asr,agent,budget,github" {
		t.Fatalf("order = %v", ids)
	}
	if !Complete(s) {
		t.Fatalf("complete = false: %+v", s.Items)
	}
	if gh := item(t, s, ItemGitHub); gh.Done || gh.Required {
		t.Fatalf("github = %+v (optional)", gh)
	}
	if b := item(t, s, ItemBudget); b.Detail != "$100 a month" {
		t.Fatalf("budget = %+v", b)
	}
	if l := item(t, s, ItemLibraries); l.Detail != "2 of 2 libraries writable" || l.LibraryIDs != nil {
		t.Fatalf("libraries = %+v", l)
	}
	if a := item(t, s, ItemASR); a.Detail != "local" {
		t.Fatalf("asr = %+v", a)
	}
}

func TestChecklistMissing(t *testing.T) {
	in := done()
	in.Libraries = []Library{{ID: 1, Mapped: true, Writable: true}, {ID: 3, Mapped: true}}
	in.Agent = Agent{Backend: "claude", Problem: AgentNoCredential, Detail: "claude has no credential: set one on the Bridge's Accounts tab"}
	in.ASR = ASR{Started: true, Waiting: true, Available: true}
	in.BudgetReviewed, in.MonthlyBudgetUSD = false, 12.5
	s := Checklist(in)
	if Complete(s) {
		t.Fatal("complete with missing items")
	}
	l := item(t, s, ItemLibraries)
	if l.Done || l.Detail != "1 of 2 libraries writable" || len(l.LibraryIDs) != 1 || l.LibraryIDs[0] != 3 {
		t.Fatalf("libraries = %+v", l)
	}
	if a := item(t, s, ItemAgent); a.Done || !a.Required || a.Detail != DetailAgentNoCredential {
		t.Fatalf("agent = %+v", a)
	}
	if a := item(t, s, ItemASR); a.Done || a.Detail != "waiting for an ASR worker" { // the page matches these words
		t.Fatalf("asr = %+v", a)
	}
	if b := item(t, s, ItemBudget); b.Done || b.Detail != "$12.50 a month" {
		t.Fatalf("budget = %+v", b)
	}
	none := done()
	none.Libraries, none.Paired = nil, false
	none.ASR = ASR{}
	s = Checklist(none)
	if item(t, s, ItemLibraries).Done || item(t, s, ItemLibraries).Detail != "pair with a server first" {
		t.Fatalf("unpaired libraries = %+v", item(t, s, ItemLibraries))
	}
	none.Paired = true
	if l := item(t, Checklist(none), ItemLibraries); l.Done || l.Detail != "the server has no libraries yet" {
		t.Fatalf("no libraries = %+v", l)
	}
	if item(t, s, ItemPaired).Done || item(t, s, ItemASR).Detail != "the engine is starting" {
		t.Fatalf("%+v", s.Items)
	}
	// A refused token isn't paired but keeps its libraries: they are counted,
	// not called missing a pairing.
	refused := done()
	refused.Paired = false
	if l := item(t, Checklist(refused), ItemLibraries); l.Detail != "2 of 2 libraries writable" {
		t.Fatalf("refused token's libraries = %+v", l)
	}
}

func TestChecklistGitHub(t *testing.T) {
	in := done()
	in.GitHub = GitHub{CanContribute: true, Login: "octocat", From: "device"}
	s := Checklist(in)
	if gh := item(t, s, ItemGitHub); !gh.Done || gh.Detail != "octocat" || !s.CanContribute || s.GitHubLogin != "octocat" {
		t.Fatalf("github = %+v, %+v", gh, s)
	}
	in.GitHub = GitHub{CanContribute: true, From: "env"}
	if gh := item(t, Checklist(in), ItemGitHub); gh.Detail != "a token from the environment" {
		t.Fatalf("github = %+v", gh)
	}
	in.MonthlyBudgetUSD = 0
	if b := item(t, Checklist(in), ItemBudget); b.Detail != "no monthly cap" {
		t.Fatalf("budget = %+v", b)
	}
}

func TestDetailIsBounded(t *testing.T) {
	in := done()
	in.Agent = Agent{Refused: true, Detail: strings.Repeat("é", 300)}
	if d := item(t, Checklist(in), ItemAgent).Detail; len(d) > maxDetail || !strings.HasSuffix(d, "...") {
		t.Fatalf("detail = %d bytes", len(d))
	}
}

// The agent item says in plain words what to do and what the agent is for, never
// the staging volume, and stays required (BRIDGE-07); a refusal keeps the words
// saying where to replace the credential.
func TestChecklistAgentWords(t *testing.T) {
	in := done()
	in.Agent = Agent{Backend: "claude", Detail: "the agent container isn't running (it hasn't written its status into the staging dir; check that both containers mount the same staging volume)"}
	a := item(t, Checklist(in), ItemAgent)
	if a.Done || !a.Required || a.Detail != DetailAgentNotRunning {
		t.Fatalf("no agent container: %+v", a)
	}
	for _, d := range []string{DetailAgentNotRunning, DetailAgentOutdated, DetailAgentNoCredential, agentNoCLI("claude"),
		agentOtherBackend("claude", "codex")} {
		if strings.Contains(d, "staging") || len(d) > maxDetail {
			t.Errorf("detail %q", d)
		}
	}
	for _, d := range []string{DetailAgentNotRunning, DetailAgentOutdated, DetailAgentNoCredential, agentNoCLI("claude")} {
		if strings.Contains(d, "staging") || !strings.Contains(d, "Claude or ChatGPT") ||
			!strings.Contains(d, "characters, recaps and descriptions") || len(d) > maxDetail {
			t.Errorf("detail %q", d)
		}
	}
	in.Agent = Agent{Backend: "claude", Refused: true, Detail: "the agent's credential was refused by the provider: replace it on the Bridge's Accounts tab"}
	if a := item(t, Checklist(in), ItemAgent); a.Done || a.Detail != in.Agent.Detail {
		t.Fatalf("refused: %+v", a)
	}
	in.Agent = Agent{Backend: "codex", Available: true}
	if a := item(t, Checklist(in), ItemAgent); !a.Done || a.Detail != "codex" {
		t.Fatalf("ready: %+v", a)
	}
}

// From pairing until the first heartbeat lists the libraries and each is
// checked, the item says it is checking, not that the server has none
// (BRIDGE-14).
func TestChecklistLibrariesChecking(t *testing.T) {
	in := done()
	in.Libraries, in.LibrariesPending = nil, true
	if l := item(t, Checklist(in), ItemLibraries); l.Done || l.Detail != DetailLibrariesChecking || l.LibraryIDs != nil {
		t.Fatalf("right after pairing: %+v", l)
	}
	// One library checked, another not yet: still checking, nothing called missing.
	in.Libraries = []Library{{ID: 1}}
	if l := item(t, Checklist(in), ItemLibraries); l.Done || l.Detail != DetailLibrariesChecking || l.LibraryIDs != nil {
		t.Fatalf("checking: %+v", l)
	}
	// Unpaired, the pairing comes first.
	in.Paired, in.Libraries = false, nil
	if l := item(t, Checklist(in), ItemLibraries); l.Detail != "pair with a server first" {
		t.Fatalf("unpaired: %+v", l)
	}
}

// Each kind of agent problem gets its own plain hint: only a missing credential
// asks for a sign-in; an old agent container asks for an update, a missing CLI
// for this Bridge's agent image, a mismatched backend names both.
func TestChecklistAgentProblems(t *testing.T) {
	for _, c := range []struct {
		agent Agent
		want  string
	}{
		{Agent{Backend: "claude"}, DetailAgentNotRunning},
		{Agent{Backend: "claude", Problem: AgentNotRunning}, DetailAgentNotRunning},
		{Agent{Backend: "claude", Problem: AgentOutdated}, DetailAgentOutdated},
		{Agent{Backend: "codex", Problem: AgentNoCLI}, "the agent image has no codex CLI: use this Bridge's agent image; " + agentFor},
		{Agent{Backend: "claude", Problem: AgentOtherBackend, Other: "codex"},
			"codex is signed in, but this Bridge runs claude: set agent.backend: codex and restart the Bridge, or set claude's sign-in on the Accounts tab"},
		{Agent{Backend: "claude", Problem: AgentOtherBackend}, DetailAgentNoCredential},
		{Agent{Backend: "claude", Problem: AgentNoCredential}, DetailAgentNoCredential},
	} {
		in := done()
		in.Agent = c.agent
		a := item(t, Checklist(in), ItemAgent)
		if a.Done || !a.Required || a.Detail != c.want {
			t.Errorf("%+v: item %+v, want detail %q", c.agent, a, c.want)
		}
		if c.agent.Problem != AgentNoCredential && c.agent.Problem != AgentOtherBackend && strings.Contains(a.Detail, "sign-in") {
			t.Errorf("%+v asks for a sign-in: %q", c.agent, a.Detail)
		}
	}
}
