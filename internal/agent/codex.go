package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

// codexBinName is the default binary name resolved on PATH when no explicit path is
// configured.
const codexBinName = "codex"

// codexRunner drives the OpenAI Codex CLI in non-interactive (`codex exec`) mode. It
// authenticates by the CLI's own ChatGPT login by default; when an openai key is
// present in secrets it is injected as CODEX_API_KEY and OPENAI_API_KEY into the
// child environment (never argv). Codex reports token usage but no dollar cost, so
// Usage.CostUSD stays 0.
type codexRunner struct {
	binResolver
	sec secrets.Store
}

func newCodexRunner(explicit string, sec secrets.Store) *codexRunner {
	return &codexRunner{binResolver: binResolver{name: codexBinName, explicit: explicit}, sec: sec}
}

func (r *codexRunner) ID() string { return IDCodex }

// Codex read-only sandboxing prevents edits but still permits file inspection, so
// it is not an enforced no-tools boundary for bounded model supervision.
func (r *codexRunner) EnforcesNoTools() bool { return false }

// SupportsWeb is true: codex exec enables live web search via the documented config
// override `-c web_search="live"` (the current top-level `web_search` key, string
// modes disabled|cached|indexed|live). Verified against the Codex config reference
// at https://learn.chatgpt.com/docs/config-file/config-reference (2026-07-16).
func (r *codexRunner) SupportsWeb() bool { return true }

// Detect resolves the binary and runs `codex --version` (via the shared resolver).
func (r *codexRunner) Detect(ctx context.Context) Availability {
	return r.binResolver.Detect(ctx)
}

// codexEvent is one line of the JSONL event stream. Parsed defensively; unknown
// fields and event types are ignored.
type codexEvent struct {
	Type  string      `json:"type"`
	Usage *codexUsage `json:"usage"`
	Item  *codexItem  `json:"item"`
	Error string      `json:"error"`
}

type codexUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

type codexItem struct {
	Type string `json:"type"` // "agent_message" carries assistant text
	Text string `json:"text"`
}

// Codex sandbox modes CodexArgs takes. CodexSandboxDefault keeps the local
// runner's choice (read-only for a NoTools run, else workspace-write);
// CodexSandboxOff is for the agent container, where codex's own sandbox can't
// start under Docker's stock seccomp and isn't a read boundary anyway: the
// container is the boundary there (decision 15).
const (
	CodexSandboxDefault = ""
	CodexSandboxOff     = "danger-full-access"
)

// CodexArgs assembles the codex argv for req. --model is omitted when empty (CLI
// default). lastMsgFile receives the final assistant message. Web search is
// enabled via the documented `-c web_search="live"` override. Exported so the
// agent container's runner (internal/agentrun) builds the very same flags.
func CodexArgs(req Request, lastMsgFile, sandbox string) []string {
	if sandbox == CodexSandboxDefault {
		sandbox = map[bool]string{true: "read-only", false: "workspace-write"}[req.NoTools]
	}
	args := []string{
		"exec",
		"--json",
		// Pipeline invocations are disposable ETL jobs. Do not load a user's coding
		// profiles/rules or persist session history - both add irrelevant context and
		// filesystem churn, while auth remains available by CLI contract.
		"--ephemeral",
		"--ignore-user-config",
		"--ignore-rules",
		"--cd", req.Dir,
		"--sandbox", sandbox,
		"--skip-git-repo-check",
		"--output-last-message", lastMsgFile,
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", req.Effort))
	}
	if req.Web {
		args = append(args, "-c", `web_search="live"`)
	}
	return args
}

// Run executes one codex invocation and interprets it with CodexResult, the
// final text coming from the --output-last-message file. A parent cancel is
// returned as ctx.Err() (with whatever usage the stream reported).
func (r *codexRunner) Run(ctx context.Context, req Request) (Result, error) {
	p, err := r.resolve()
	if err != nil {
		return Result{}, &NotAvailableError{Backend: IDCodex, Detail: err.Error()}
	}

	lastMsg, err := os.CreateTemp("", "codex-last-*.txt")
	if err != nil {
		return Result{}, fmt.Errorf("codex: temp file: %w", err)
	}
	lastMsgPath := lastMsg.Name()
	_ = lastMsg.Close()
	defer func() { _ = os.Remove(lastMsgPath) }()

	key := ""
	if r.sec != nil {
		key, _ = r.sec.Get(secrets.OpenAIAPIKey)
	}
	env := childEnv(map[string]string{"CODEX_API_KEY": key, "OPENAI_API_KEY": key})

	stdout, stderr, runErr := runCLI(ctx, cliSpec{
		path:      p,
		args:      CodexArgs(req, lastMsgPath, CodexSandboxDefault),
		dir:       req.Dir,
		env:       env,
		stdin:     req.Prompt,
		timeout:   req.Timeout,
		heartbeat: req.Heartbeat,
		process:   req.Process,
	})
	out := outcomeOf(stdout, stderr, runErr, req.Timeout)
	if fileText, rerr := os.ReadFile(lastMsgPath); rerr == nil { //nolint:gosec // lastMsgPath is our own os.CreateTemp file
		out.LastMessage = string(fileText)
	}
	res, err := CodexResult(req.Model, out)
	if runErr != nil && !out.TimedOut && ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, err
}

// CodexResult turns one codex run's outcome into a Result, wherever the CLI ran
// (here, or in the agent container). Usage comes from turn.completed events (last
// event wins - see parseCodexStream) and the text from o.LastMessage, falling back
// to the last agent_message item. A timeout becomes "codex timed out"; a
// turn.failed event or a nonzero exit an error; a rate-limit signature a
// *RateLimitError. The usage rides on the Result even when the error is non-nil.
func CodexResult(model string, o CLIOutcome) (Result, error) {
	usage, lastAgentMsg, failDetail := parseCodexStream(o.Stdout)
	usage.Model = model
	result := Result{Text: lastAgentMsg, Usage: usage}
	if o.TimedOut {
		return result, fmt.Errorf("codex timed out after %s", o.Timeout)
	}

	if failDetail != "" {
		if isRateLimit(failDetail) {
			return result, newRateLimitError(failDetail, time.Now())
		}
		if isAuthFailure(failDetail) {
			return result, credentialRefused(IDCodex, failDetail)
		}
		return result, fmt.Errorf("codex turn failed: %s", truncate(failDetail))
	}
	if isRateLimit(o.Stderr) {
		return result, newRateLimitError(firstNonEmpty(o.Stderr, o.Stdout), time.Now())
	}
	if isAuthFailure(o.Stderr) {
		return result, credentialRefused(IDCodex, firstNonEmpty(o.Stderr, o.Stdout))
	}
	if o.ExitErr != nil {
		return result, fmt.Errorf("codex exited: %w: %s", o.ExitErr, truncate(firstNonEmpty(o.Stderr, o.Stdout)))
	}

	if text := strings.TrimSpace(o.LastMessage); text != "" {
		result.Text = text
	}
	return result, nil
}

// parseCodexStream reads the JSONL events, taking token usage from turn.completed
// events and tracking the last agent_message text and any turn.failed detail.
// Malformed lines are skipped.
//
// Token usage on codex turn.completed events is CUMULATIVE session totals, not
// per-turn deltas (openai/codex exec's event processor emits ThreadTokenUsage.total,
// never .last - see openai/codex issue #17539), so each event REPLACES the running
// Input/Output/CacheRead rather than adding to them: last event wins. Turns still
// increments per event to count the invocation's turns.
func parseCodexStream(stdout string) (usage Usage, lastAgentMsg, failDetail string) {
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "turn.completed":
			if ev.Usage != nil {
				usage.Input = ev.Usage.InputTokens
				usage.Output = ev.Usage.OutputTokens
				usage.CacheRead = ev.Usage.CachedInputTokens
				usage.Turns++
			}
		case "turn.failed":
			failDetail = firstNonEmpty(ev.Error, failDetail, "codex reported a failed turn")
		case "item.completed":
			if ev.Item != nil && ev.Item.Type == "agent_message" && strings.TrimSpace(ev.Item.Text) != "" {
				lastAgentMsg = strings.TrimSpace(ev.Item.Text)
			}
		}
	}
	return usage, lastAgentMsg, failDetail
}
