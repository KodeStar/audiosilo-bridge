// Package agentproto is the staging protocol between the Bridge and the agent
// container (decision 15): the names of the files in the staging dir and in a job
// dir, the JSON both sides read and write, and the strict request validation.
// internal/agentbox (the Bridge side) and internal/agentrun (the runner in the
// agent container) both use it, so the two sides can't drift. A leaf: standard
// library only.
//
// A job dir <staging>/<id>/ goes through these steps:
//
//	Bridge:  work/ (the staged copy; work/out/ writable by the agent uid),
//	         prompt.txt, then request.json.tmp renamed to request.json (ready)
//	Runner:  mkdir claim (atomic), heartbeat touched every 5 s while the CLI runs,
//	         stdout.txt, stderr.txt, last_message.txt (codex), exit.json,
//	         work/out/ opened for the Bridge to read, done.tmp renamed to done
//	Bridge:  cancel (to stop a run), harvest of work/out/ after done, then the
//	         job dir is deleted
//
// The runner rewrites <staging>/runner.json every RunnerInterval (15 s); the
// Bridge treats the container as up while it is under RunnerFreshFor (30 s) old.
//
// Nothing in a job dir is trusted by the side that didn't write it: the runner
// validates the request, the Bridge treats every output as untrusted, and both
// read the other's files with OpenRegular/ReadRegular (files.go).
package agentproto

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// Protocol is the staging protocol's version. Both sides refuse another: the
// Bridge reports the container unavailable, the runner fails the job.
const Protocol = 1

// File names in the staging dir and in a job dir.
const (
	RunnerFile      = "runner.json" // <staging>/runner.json: the runner's liveness
	RequestFile     = "request.json"
	RequestTmpFile  = "request.json.tmp"
	PromptFile      = "prompt.txt"
	WorkDir         = "work"
	OutDir          = "out" // under WorkDir: the agent's only writable place
	ClaimDir        = "claim"
	HeartbeatFile   = "heartbeat"
	CancelFile      = "cancel"
	StdoutFile      = "stdout.txt"
	StderrFile      = "stderr.txt"
	LastMessageFile = "last_message.txt"
	ExitFile        = "exit.json"
	DoneFile        = "done"
	DoneTmpFile     = "done.tmp"
)

// The CLIs the runner runs.
const (
	CLIClaude = "claude"
	CLICodex  = "codex"
)

// The credential kinds runner.json reports per CLI ("" = none). oauth_token and
// login are subscription credentials (no per-token bill); api_key is billed.
const (
	CredentialOAuthToken = "oauth_token" // claude: CLAUDE_CODE_OAUTH_TOKEN
	CredentialLogin      = "login"       // codex: a device login in CODEX_HOME/auth.json
	CredentialAPIKey     = "api_key"     // ANTHROPIC_API_KEY / OPENAI_API_KEY
)

// Bounds on what a request may ask for, and on the files each side reads back.
const (
	MaxTurns          = 1000
	MaxTimeoutSeconds = 24 * 60 * 60
	MaxRequestBytes   = 64 << 10
	MaxPromptBytes    = 32 << 20
	MaxRunnerBytes    = 64 << 10
	MaxExitBytes      = 64 << 10
	MaxStdoutBytes    = 64 << 20
	MaxStderrBytes    = 8 << 20
)

// Intervals both sides agree on.
const (
	// RunnerInterval is how often the runner rewrites runner.json.
	RunnerInterval = 15 * time.Second
	// RunnerFreshFor is how young runner.json's heartbeat_at must be for the Bridge
	// to treat the container as up. Two intervals, so one slow or missed rewrite
	// doesn't flap the container to unavailable.
	RunnerFreshFor = 2 * RunnerInterval
	// HeartbeatInterval is how often the runner touches a running job's heartbeat
	// file, and so how often the Bridge needs to look at it.
	HeartbeatInterval = 5 * time.Second
)

// Runner is runner.json: the runner's liveness and what it can run.
type Runner struct {
	Protocol    int            `json:"protocol"`
	Version     string         `json:"version"`
	HeartbeatAt time.Time      `json:"heartbeat_at"`
	Slots       int            `json:"slots"`
	CLIs        map[string]CLI `json:"clis"`
}

// CLI is one CLI's entry in runner.json. Version is empty when the binary isn't
// there; Credential is one of the Credential* kinds, or empty. Never a token.
// CredentialSince is when the runner first saw the credential it has now (zero
// with none): it moves when the credential is replaced (or the runner restarts),
// which is how the Bridge knows a credential the provider refused may work again.
// The runner compares the credentials in memory; nothing of one is written.
type CLI struct {
	Version         string    `json:"version"`
	Credential      string    `json:"credential"`
	CredentialSince time.Time `json:"credential_since,omitzero"`
}

// Request is request.json: a typed request the runner turns into the CLI's argv
// itself. The Bridge never sends argv.
type Request struct {
	Protocol       int    `json:"protocol"`
	CLI            string `json:"cli"`
	Model          string `json:"model"`
	Effort         string `json:"effort"`
	Web            bool   `json:"web"`
	NoTools        bool   `json:"no_tools"`
	MaxTurns       int    `json:"max_turns"`       // 0 = the CLI runner's default ceiling
	TimeoutSeconds int    `json:"timeout_seconds"` // 1 to MaxTimeoutSeconds
}

// Exit is exit.json: how the run ended. Error is set when the runner couldn't run
// the job at all (a malformed request, a missing prompt, the CLI's credential
// missing) or stopped it itself (the container stopping or restarting);
// Unavailable marks the kinds the Bridge should treat as the agent being
// unavailable (park and self-resume) rather than a failed run.
type Exit struct {
	ExitCode    int    `json:"exit_code"`
	TimedOut    bool   `json:"timed_out"`
	Cancelled   bool   `json:"cancelled"`
	DurationMS  int64  `json:"duration_ms"`
	Error       string `json:"error,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
}

// modelRe allows the model names the CLIs take ("opus", "claude-opus-4-1[1m]",
// "gpt-5.1-codex"), never a leading dash (it would read as a flag).
var modelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,127}$`)

// effortRe allows an effort name (low, medium, high, xhigh, max, minimal). It goes
// into a codex `-c model_reasoning_effort="..."` override, so letters only.
var effortRe = regexp.MustCompile(`^[a-z]{1,16}$`)

// DecodeRequest reads request.json strictly: an unknown field, trailing data or
// an oversized file is refused, then Validate.
func DecodeRequest(data []byte) (Request, error) {
	if len(data) > MaxRequestBytes {
		return Request{}, fmt.Errorf("request.json is over %d bytes", MaxRequestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Request
	if err := dec.Decode(&r); err != nil {
		return Request{}, fmt.Errorf("request.json: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Request{}, errors.New("request.json: trailing data after the object")
	}
	if err := r.Validate(); err != nil {
		return Request{}, err
	}
	return r, nil
}

// Validate checks a request's values against the protocol's bounds.
func (r Request) Validate() error {
	if r.Protocol != Protocol {
		return fmt.Errorf("request protocol %d, the runner speaks %d", r.Protocol, Protocol)
	}
	if r.CLI != CLIClaude && r.CLI != CLICodex {
		return fmt.Errorf("request cli %q must be %q or %q", r.CLI, CLIClaude, CLICodex)
	}
	if r.Model != "" && !modelRe.MatchString(r.Model) {
		return fmt.Errorf("request model %q is not a model name", r.Model)
	}
	if r.Effort != "" && !effortRe.MatchString(r.Effort) {
		return fmt.Errorf("request effort %q is not an effort name", r.Effort)
	}
	if r.MaxTurns < 0 || r.MaxTurns > MaxTurns {
		return fmt.Errorf("request max_turns %d must be between 0 and %d", r.MaxTurns, MaxTurns)
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > MaxTimeoutSeconds {
		return fmt.Errorf("request timeout_seconds %d must be between 1 and %d", r.TimeoutSeconds, MaxTimeoutSeconds)
	}
	return nil
}

// jobIDRe is a job dir's name: 16 lowercase hex digits.
var jobIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ValidJobID reports whether name is a job dir's name. The runner ignores
// anything else in the staging dir.
func ValidJobID(name string) bool { return jobIDRe.MatchString(name) }

// NewJobID returns a fresh random job id.
func NewJobID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
