package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

// claudeBinName is the default binary name resolved on PATH when no explicit path
// is configured.
const claudeBinName = "claude"

// claudeRunner drives the Claude Code CLI in headless mode. It authenticates by the
// CLI's own login by default; when an anthropic key is present in secrets it is
// injected as ANTHROPIC_API_KEY into the child environment (never argv).
type claudeRunner struct {
	binResolver
	sec secrets.Store
}

func newClaudeRunner(explicit string, sec secrets.Store) *claudeRunner {
	return &claudeRunner{binResolver: binResolver{name: claudeBinName, explicit: explicit}, sec: sec}
}

func (c *claudeRunner) ID() string { return IDClaude }

func (c *claudeRunner) EnforcesNoTools() bool { return true }

// SupportsWeb is always true for claude: WebSearch/WebFetch are first-class tools
// enabled by adding them to --allowedTools.
func (c *claudeRunner) SupportsWeb() bool { return true }

// Detect resolves the binary and runs `claude --version` (via the shared resolver).
func (c *claudeRunner) Detect(ctx context.Context) Availability {
	return c.binResolver.Detect(ctx)
}

// claudeResult is the single JSON result envelope claude -p --output-format json
// prints on stdout. Parsed defensively: unknown fields are ignored.
type claudeResult struct {
	Type         string      `json:"type"`
	Subtype      string      `json:"subtype"`
	IsError      bool        `json:"is_error"`
	Result       string      `json:"result"`
	NumTurns     int         `json:"num_turns"`
	TotalCostUSD float64     `json:"total_cost_usd"`
	Usage        claudeUsage `json:"usage"`
}

type claudeUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
}

// ClaudeArgs assembles the claude argv for req (no prompt - that rides on stdin).
// --model is omitted when empty so the CLI default applies. Exported so the agent
// container's runner (internal/agentrun) builds the very same flags: one copy, so
// the two paths can't drift.
func ClaudeArgs(req Request) []string {
	tools := baseAllowedTools
	if req.NoTools {
		tools = ""
	} else if req.Web {
		tools += "," + webTools
	}
	turns := req.MaxTurns
	if turns < 1 {
		turns = maxTurns
	}
	args := []string{
		"-p",
		"--output-format", "json",
		"--max-turns", fmt.Sprintf("%d", turns),
		// --tools is the actual availability boundary; --allowedTools only
		// pre-approves tools. Supplying both prevents permission prompts without
		// leaving unrelated coding tools in the model's tool catalogue.
		"--tools", tools,
		"--allowedTools", tools,
		// These invocations are disposable ETL jobs, not coding sessions. Do not
		// persist a resumable session. Deliberately avoid --bare: it disables keychain
		// reads, including the Claude subscription login this runner relies on.
		"--no-session-persistence",
		"--system-prompt", "You are a deterministic document-processing worker. Follow the stdin task exactly, use only the provided tools and paths, write the requested artifacts, and stop.",
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	return args
}

// Run executes one claude invocation and interprets it with ClaudeResult. A
// parent cancel is returned as ctx.Err() (with whatever usage the CLI reported).
func (c *claudeRunner) Run(ctx context.Context, req Request) (Result, error) {
	p, err := c.resolve()
	if err != nil {
		return Result{}, &NotAvailableError{Backend: IDClaude, Detail: err.Error()}
	}

	key := ""
	if c.sec != nil {
		key, _ = c.sec.Get(secrets.AnthropicAPIKey)
	}
	env := childEnv(map[string]string{"ANTHROPIC_API_KEY": key})

	stdout, stderr, runErr := runCLI(ctx, cliSpec{
		path:      p,
		args:      ClaudeArgs(req),
		dir:       req.Dir,
		env:       env,
		stdin:     req.Prompt,
		timeout:   req.Timeout,
		heartbeat: req.Heartbeat,
		process:   req.Process,
	})
	out := outcomeOf(stdout, stderr, runErr, req.Timeout)
	res, err := ClaudeResult(req.Model, out)
	if runErr != nil && !out.TimedOut && ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, err
}

// ClaudeResult turns one claude run's outcome into a Result, wherever the CLI ran
// (here, or in the agent container: internal/agentbox reads the outcome back from
// the staging dir). A timeout becomes "claude timed out"; a rate-limit signature
// in the result or stderr a *RateLimitError (classified before anything else: it
// can arrive as is_error with a rate-limit result, or as a nonzero exit with the
// message on stderr); is_error, a nonzero exit, or an unparseable envelope an error
// carrying truncated detail. The usage the envelope reports rides on the Result
// even when the error is non-nil, so spend is captured on a failed run too.
func ClaudeResult(model string, o CLIOutcome) (Result, error) {
	var res claudeResult
	parseErr := json.Unmarshal([]byte(o.Stdout), &res)

	usage := Usage{
		Model:        model,
		Input:        res.Usage.InputTokens,
		Output:       res.Usage.OutputTokens,
		CacheRead:    res.Usage.CacheReadInputTokens,
		CostUSD:      res.TotalCostUSD,
		CostReported: parseErr == nil && res.Type == "result",
		Turns:        res.NumTurns,
	}
	result := Result{Text: res.Result, Usage: usage}
	if o.TimedOut {
		return result, fmt.Errorf("claude timed out after %s", o.Timeout)
	}

	detail := firstNonEmpty(res.Result, o.Stderr, o.Stdout)
	if isRateLimit(res.Result) || isRateLimit(o.Stderr) {
		return result, newRateLimitError(detail, time.Now())
	}
	if isAuthFailure(o.Stdout) || isAuthFailure(o.Stderr) {
		return result, credentialRefused(IDClaude, detail)
	}

	if o.ExitErr != nil {
		return result, fmt.Errorf("claude exited: %w: %s", o.ExitErr, truncate(detail))
	}
	if parseErr != nil {
		return Result{}, fmt.Errorf("claude: parse result json: %w: %s", parseErr, truncate(o.Stdout))
	}
	if res.IsError {
		return result, fmt.Errorf("claude reported error: %s", truncate(firstNonEmpty(res.Result, res.Subtype)))
	}

	return result, nil
}
