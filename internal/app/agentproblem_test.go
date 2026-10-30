package app

import (
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/agentbox"
	"github.com/kodestar/audiosilo-bridge/internal/setup"
)

// Every agent container problem reaches the checklist as its own kind.
func TestAgentProblem(t *testing.T) {
	for in, want := range map[agentbox.Problem]setup.AgentProblem{
		agentbox.ProblemNotRunning:   setup.AgentNotRunning,
		agentbox.ProblemOutdated:     setup.AgentOutdated,
		agentbox.ProblemNoCLI:        setup.AgentNoCLI,
		agentbox.ProblemOtherBackend: setup.AgentOtherBackend,
		agentbox.ProblemNoCredential: setup.AgentNoCredential,
		"":                           "",
	} {
		if got := agentProblem(in); got != want {
			t.Errorf("agentProblem(%q) = %q, want %q", in, got, want)
		}
	}
}
