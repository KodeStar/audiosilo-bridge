// Package agentbox is a TEMPORARY stand-in for the agent container runner
// (decision 15), which is written in a parallel worktree: it has the real
// package's API so the wiring compiles, and it never runs anything (every run is
// a NotAvailableError, so a book parks agent_unavailable). The integrator deletes
// this file and drops in the real package.
package agentbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
)

// Options configure the runner.
type Options struct {
	Dir          string
	Backend      string
	ClaimTimeout time.Duration
	Log          *slog.Logger
}

// Runner is the Bridge side of the agent container protocol.
type Runner struct {
	opts Options
}

// New returns a Runner over the staging directory.
func New(o Options) *Runner { return &Runner{opts: o} }

// ID is the backend the runner asks the container to run.
func (r *Runner) ID() string { return r.opts.Backend }

// Detect reports the container unavailable (the stub has none).
func (r *Runner) Detect(context.Context) agent.Availability {
	return agent.Availability{Backend: r.opts.Backend, Detail: "the agent container runner is not built in"}
}

// Run never runs: the stub has no container.
func (r *Runner) Run(context.Context, agent.Request) (agent.Result, error) {
	return agent.Result{}, &agent.NotAvailableError{Backend: r.opts.Backend, Detail: "the agent container runner is not built in"}
}

// SupportsWeb is true for both CLIs.
func (r *Runner) SupportsWeb() bool { return true }

// Status is the container as the Bridge last saw it.
func (r *Runner) Status() Status {
	return Status{Detail: "the agent container runner is not built in", CLIs: map[string]CLIStatus{}}
}

// Status is the agent container's liveness and credentials.
type Status struct {
	Available   bool
	Detail      string
	Version     string
	Slots       int
	HeartbeatAt time.Time
	CLIs        map[string]CLIStatus
}

// CLIStatus is one CLI in the container.
type CLIStatus struct {
	Version, Credential string
}

// Subscription reports whether backend's credential is a subscription login.
func (s Status) Subscription(backend string) bool {
	c := s.CLIs[backend].Credential
	return c == "oauth_token" || c == "login"
}
