package agent

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// The agent container's runner builds codex's argv with CodexArgs too, with
// codex's own sandbox off; the local runner keeps its choice.
func TestCodexArgsSandbox(t *testing.T) {
	t.Parallel()
	sandbox := func(args []string) string {
		i := slices.Index(args, "--sandbox")
		if i < 0 || i+1 >= len(args) {
			return ""
		}
		return args[i+1]
	}
	req := Request{Dir: "/staging/x/work"}
	if got := sandbox(CodexArgs(req, "/tmp/last", CodexSandboxDefault)); got != "workspace-write" {
		t.Errorf("default = %q", got)
	}
	req.NoTools = true
	if got := sandbox(CodexArgs(req, "/tmp/last", CodexSandboxDefault)); got != "read-only" {
		t.Errorf("no tools = %q", got)
	}
	args := CodexArgs(req, "/tmp/last", CodexSandboxOff)
	if got := sandbox(args); got != "danger-full-access" {
		t.Errorf("off = %q", got)
	}
	if i := slices.Index(args, "--cd"); i < 0 || args[i+1] != "/staging/x/work" {
		t.Errorf("--cd = %q", args)
	}
}

func TestClaudeArgsNoTools(t *testing.T) {
	t.Parallel()
	args := ClaudeArgs(Request{NoTools: true, Web: true})
	if i := slices.Index(args, "--tools"); i < 0 || args[i+1] != "" {
		t.Fatalf("a NoTools run has tools: %q", args)
	}
	if i := slices.Index(args, "--max-turns"); args[i+1] != "200" {
		t.Fatalf("max turns = %q", args[i+1])
	}
}

// ClaudeResult and CodexResult classify an outcome from anywhere: a container's
// exit code and timeout come in as CLIOutcome fields.
func TestResultsFromAnOutcome(t *testing.T) {
	t.Parallel()
	exit2 := errors.New("exit status 2")
	for name, tc := range map[string]struct {
		codex bool
		out   CLIOutcome
		want  string
		rate  bool
	}{
		"claude timed out":   {out: CLIOutcome{TimedOut: true, Timeout: time.Minute}, want: "claude timed out after 1m0s"},
		"claude exit":        {out: CLIOutcome{ExitErr: exit2, Stderr: "boom"}, want: "claude exited: exit status 2: boom"},
		"claude rate stderr": {out: CLIOutcome{ExitErr: exit2, Stderr: "HTTP 429 Too Many Requests"}, rate: true},
		"claude bad json":    {out: CLIOutcome{Stdout: "nope"}, want: "claude: parse result json"},
		"codex timed out":    {codex: true, out: CLIOutcome{TimedOut: true, Timeout: time.Second}, want: "codex timed out after 1s"},
		"codex turn failed":  {codex: true, out: CLIOutcome{Stdout: `{"type":"turn.failed","error":"bad"}`}, want: "codex turn failed: bad"},
		"codex usage limit":  {codex: true, out: CLIOutcome{Stdout: `{"type":"turn.failed","error":"usage limit reached"}`}, rate: true},
		"codex exit":         {codex: true, out: CLIOutcome{ExitErr: exit2, Stderr: "401 Unauthorized"}, want: "codex exited: exit status 2: 401 Unauthorized"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.codex {
				_, err = CodexResult("m", tc.out)
			} else {
				_, err = ClaudeResult("m", tc.out)
			}
			var rl *RateLimitError
			if tc.rate != errors.As(err, &rl) {
				t.Fatalf("rate limit = %v, err %v", !tc.rate, err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCodexResultPrefersLastMessage(t *testing.T) {
	t.Parallel()
	stream := `{"type":"item.completed","item":{"type":"agent_message","text":"streamed"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":5,"cached_input_tokens":1,"output_tokens":2}}`
	res, err := CodexResult("gpt", CLIOutcome{Stdout: stream, LastMessage: "  final \n"})
	if err != nil || res.Text != "final" || res.Usage.Input != 5 || res.Usage.Model != "gpt" {
		t.Fatalf("res = %+v, %v", res, err)
	}
	res, _ = CodexResult("gpt", CLIOutcome{Stdout: stream})
	if res.Text != "streamed" {
		t.Fatalf("fallback text = %q", res.Text)
	}
}
