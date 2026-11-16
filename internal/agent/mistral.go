package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

// mistralBinName is the default binary name resolved on PATH when no explicit path
// is configured: Mistral's Vibe CLI.
// vibeTurnFactor scales a stage's turn limit for vibe's finer-grained steps.
const vibeTurnFactor = 4

const mistralBinName = "vibe"

// MistralExportFile is the name of the file `vibe --output-dir` writes its run
// export to.
const MistralExportFile = "export.json"

// vibeTools are the Vibe tools a run may use (--enabled-tools disables the rest:
// no shell, no web, no subagents). Vibe has no glob tool; grep and read_file cover
// the same ground. A NoTools run keeps only the two read tools, as codex's
// read-only sandbox does: edits are prevented, reads are not.
var (
	vibeTools     = []string{"read_file", "write_file", "edit", "grep"}
	vibeReadTools = []string{"read_file", "grep"}
)

// mistralRunner drives the Mistral Vibe CLI in programmatic mode (`vibe -p`). It
// authenticates with an API key only: when a mistral key is present in secrets it
// is injected as MISTRAL_API_KEY into the child environment (never argv). Vibe
// reports tokens and a dollar cost in its run export, so both ride on the Result.
type mistralRunner struct {
	binResolver
	sec secrets.Store
}

func newMistralRunner(explicit string, sec secrets.Store) *mistralRunner {
	return &mistralRunner{binResolver: binResolver{name: mistralBinName, explicit: explicit}, sec: sec}
}

func (r *mistralRunner) ID() string { return IDMistral }

// EnforcesNoTools is false: Vibe can't run with an empty tool catalogue, so a
// NoTools run is read-only rather than tool-free.
func (r *mistralRunner) EnforcesNoTools() bool { return false }

// SupportsWeb is false: Vibe's web_search and web_fetch tools are never enabled,
// so web stages degrade as they do for any backend without web access.
func (r *mistralRunner) SupportsWeb() bool { return false }

// Detect resolves the binary and runs `vibe --version`. The default backend id
// reported is the runner's, not the binary's name.
func (r *mistralRunner) Detect(ctx context.Context) Availability {
	av := r.binResolver.Detect(ctx)
	av.Backend = IDMistral
	return av
}

// MistralEnv is the Vibe configuration every run gets through its environment
// (Vibe reads any config field as VIBE_<FIELD>): nothing that phones home, loads
// connectors or MCP servers, starts subagents or background processes, or updates
// itself. home is the run's private VIBE_HOME. Exported so the agent container's
// runner (internal/agentrun) sets the very same variables.
func MistralEnv(home string) []string {
	return []string{
		"VIBE_HOME=" + home,
		"VIBE_ENABLE_TELEMETRY=false",
		"VIBE_ENABLE_UPDATE_CHECKS=false",
		"VIBE_ENABLE_AUTO_UPDATE=false",
		"VIBE_ENABLE_CONNECTORS=false",
		"VIBE_ENABLE_MCP=false",
		"VIBE_ENABLE_SUBAGENTS=false",
		"VIBE_ENABLE_BACKGROUND_PROCESSES=false",
		"VIBE_ENABLE_NOTIFICATIONS=false",
	}
}

// MistralModelEnv is the VIBE_ACTIVE_MODEL variable for model ("" = none: the
// CLI's default model). The model rides in the environment, not argv, as Vibe has
// no model flag. Effort is not passed: Vibe has no reasoning-effort setting.
func MistralModelEnv(model string) []string {
	if model == "" {
		return nil
	}
	return []string{"VIBE_ACTIVE_MODEL=" + model}
}

// MistralArgs assembles the vibe argv for req (no prompt - that rides on stdin).
// exportDir receives export.json (outcome, usage, cost). -p comes last: it takes
// an optional value, and none follows. Exported so the agent container's runner
// builds the very same flags.
func MistralArgs(req Request, exportDir string) []string {
	tools := vibeTools
	if req.NoTools {
		tools = vibeReadTools
	}
	turns := maxTurns
	if req.MaxTurns > 0 {
		// The stages' limits are tuned to claude and codex; vibe counts every
		// file read, write and edit as a step and has no glob tool.
		turns = min(req.MaxTurns*vibeTurnFactor, 1000)
	}
	args := []string{
		"--output", "text",
		"--output-dir", exportDir,
		"--workdir", req.Dir,
		"--trust",
		"--auto-approve",
		"--max-turns", fmt.Sprintf("%d", turns),
	}
	for _, t := range tools {
		args = append(args, "--enabled-tools", t)
	}
	return append(args, "-p")
}

// Run executes one vibe invocation and interprets it with MistralResult. A parent
// cancel is returned as ctx.Err() (with whatever usage the export reported).
func (r *mistralRunner) Run(ctx context.Context, req Request) (Result, error) {
	p, err := r.resolve()
	if err != nil {
		return Result{}, &NotAvailableError{Backend: IDMistral, Detail: err.Error()}
	}

	home, err := os.MkdirTemp("", "vibe-home-*")
	if err != nil {
		return Result{}, fmt.Errorf("mistral: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(home) }()
	exportDir := filepath.Join(home, "out")
	vibeHome := filepath.Join(home, "vibe")
	if err := os.Mkdir(vibeHome, 0o700); err != nil {
		return Result{}, fmt.Errorf("mistral: temp dir: %w", err)
	}

	key := ""
	if r.sec != nil {
		key, _ = r.sec.Get(secrets.MistralAPIKey)
	}
	env := childEnv(map[string]string{"MISTRAL_API_KEY": key})
	env = append(env, MistralEnv(vibeHome)...)
	env = append(env, MistralModelEnv(req.Model)...)

	stdout, stderr, runErr := runCLI(ctx, cliSpec{
		path:      p,
		args:      MistralArgs(req, exportDir),
		dir:       req.Dir,
		env:       env,
		stdin:     req.Prompt,
		timeout:   req.Timeout,
		heartbeat: req.Heartbeat,
		process:   req.Process,
	})
	out := outcomeOf(stdout, stderr, runErr, req.Timeout)
	if raw, rerr := os.ReadFile(filepath.Join(exportDir, MistralExportFile)); rerr == nil { //nolint:gosec // our own temp dir
		out.Export = string(raw)
	}
	res, err := MistralResult(req.Model, out)
	if out.ExitErr != nil && ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, err
}

// mistralExport is the part of vibe's export.json the Bridge reads. Parsed
// defensively: unknown fields are ignored.
type mistralExport struct {
	Outcome    string        `json:"outcome"`
	StopReason string        `json:"stop_reason,omitempty"`
	Error      *mistralError `json:"error,omitempty"`
	Usage      *struct {
		InputTokens       int64 `json:"input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
	} `json:"usage,omitempty"`
	CostUSD *float64 `json:"cost_usd,omitempty"`
	Steps   int      `json:"steps,omitempty"`
}

type mistralError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// CompactMistralExport keeps only what MistralResult reads of a vibe export.json:
// the full file carries the run's whole configuration, which has no business
// crossing the staging dir.
func CompactMistralExport(raw []byte) ([]byte, error) {
	var ex mistralExport
	if err := json.Unmarshal(raw, &ex); err != nil {
		return nil, err
	}
	return json.Marshal(ex)
}

// MistralResult turns one vibe run's outcome into a Result, wherever the CLI ran
// (here, or in the agent container). Outcome, usage and cost come from the run
// export (o.Export), the text from stdout. A timeout becomes "mistral timed out";
// an invalid-key error a refused credential; a rate-limit signature a
// *RateLimitError; any other unfinished outcome or a nonzero exit an error. The
// usage the export reports rides on the Result even when the error is non-nil.
func MistralResult(model string, o CLIOutcome) (Result, error) {
	var ex mistralExport
	parseErr := json.Unmarshal([]byte(o.Export), &ex)
	haveExport := parseErr == nil && ex.Outcome != ""

	usage := Usage{Model: model, Turns: ex.Steps}
	if ex.Usage != nil {
		usage.Input = max(ex.Usage.InputTokens-ex.Usage.CachedInputTokens, 0)
		usage.Output = ex.Usage.OutputTokens
		usage.CacheRead = ex.Usage.CachedInputTokens
	}
	if haveExport && ex.CostUSD != nil {
		usage.CostUSD, usage.CostReported = *ex.CostUSD, true
	}
	result := Result{Text: strings.TrimSpace(o.Stdout), Usage: usage}
	if o.TimedOut {
		return result, fmt.Errorf("mistral timed out after %s", o.Timeout)
	}

	errText := ""
	if ex.Error != nil {
		errText = firstNonEmpty(ex.Error.Message, ex.Error.Code)
	}
	detail := firstNonEmpty(errText, o.Stderr, o.Stdout)
	failed := o.ExitErr != nil || !haveExport || ex.Outcome != "finished"
	if failed && (isRateLimit(errText) || isRateLimit(o.Stderr)) {
		return result, newRateLimitError(detail, time.Now())
	}
	// Only a failed run's output says a credential was refused: a clean run's
	// answer may quote a 401.
	if failed && (isAuthFailure(errText) || isAuthFailure(o.Stderr) || (ex.Error != nil && ex.Error.Code == "invalid_api_key")) {
		return result, credentialRefused(IDMistral, detail)
	}
	if haveExport && ex.Outcome != "finished" {
		return result, fmt.Errorf("mistral run ended %s: %s", ex.Outcome, truncate(detail))
	}
	if o.ExitErr != nil {
		return result, fmt.Errorf("mistral exited: %w: %s", o.ExitErr, truncate(detail))
	}
	if !haveExport {
		// A clean exit with no export isn't a run to trust: usage and outcome are
		// unknown.
		return result, fmt.Errorf("mistral: no usable run export: %s", truncate(firstNonEmpty(o.Stderr, o.Stdout)))
	}
	return result, nil
}
