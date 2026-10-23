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
	in.Agent = Agent{Backend: "claude", Detail: "claude has no credential: set one on the Bridge's Accounts tab"}
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
	if a := item(t, s, ItemAgent); a.Done || !strings.Contains(a.Detail, "Accounts tab") {
		t.Fatalf("agent = %+v", a)
	}
	if a := item(t, s, ItemASR); a.Done || a.Detail != "waiting for an ASR worker" {
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
	in.Agent = Agent{Detail: strings.Repeat("é", 300)}
	if d := item(t, Checklist(in), ItemAgent).Detail; len(d) > maxDetail || !strings.HasSuffix(d, "...") {
		t.Fatalf("detail = %d bytes", len(d))
	}
}
