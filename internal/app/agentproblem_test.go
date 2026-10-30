package app

import (
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/agentbox"
	"github.com/kodestar/audiosilo-bridge/internal/setup"
)

// The app converts agentbox.Problem to setup.AgentProblem as is: every agent
// container problem must have the checklist's spelling.
func TestAgentProblem(t *testing.T) {
	for in, want := range map[agentbox.Problem]setup.AgentProblem{
		agentbox.ProblemNotRunning:   setup.AgentNotRunning,
		agentbox.ProblemOutdated:     setup.AgentOutdated,
		agentbox.ProblemNoCLI:        setup.AgentNoCLI,
		agentbox.ProblemOtherBackend: setup.AgentOtherBackend,
		agentbox.ProblemNoCredential: setup.AgentNoCredential,
	} {
		if got := setup.AgentProblem(in); got != want {
			t.Errorf("setup.AgentProblem(%q) = %q, want %q", in, got, want)
		}
	}
}
