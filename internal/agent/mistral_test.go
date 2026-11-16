package agent

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMistralArgs(t *testing.T) {
	args := MistralArgs(Request{Dir: "/work", MaxTurns: 7}, "/out")
	if args[len(args)-1] != "-p" {
		t.Errorf("-p must come last: %v", args)
	}
	for _, want := range []string{"--auto-approve", "--trust", "write_file", "edit"} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %q in %v", want, args)
		}
	}
	ro := MistralArgs(Request{Dir: "/work", NoTools: true}, "/out")
	for _, bad := range []string{"write_file", "edit", "bash", "web_fetch"} {
		if slices.Contains(ro, bad) {
			t.Errorf("NoTools run enables %q: %v", bad, ro)
		}
	}
}

func TestMistralResult(t *testing.T) {
	ok := `{"outcome":"finished","usage":{"input_tokens":100,"output_tokens":20,"cached_input_tokens":30},"cost_usd":0.25,"steps":3}`
	res, err := MistralResult("m", CLIOutcome{Stdout: " done \n", Export: ok})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.Usage.Input != 70 || res.Usage.CacheRead != 30 || res.Usage.Output != 20 || !res.Usage.CostReported || res.Usage.CostUSD != 0.25 {
		t.Errorf("result = %+v", res)
	}

	exit := errors.New("exit status 1")
	_, err = MistralResult("m", CLIOutcome{ExitErr: exit, Export: `{"outcome":"config_error","error":{"code":"invalid_api_key","message":"Invalid API key"}}`})
	if err == nil || !strings.Contains(err.Error(), "mistral") {
		t.Errorf("refused credential: %v", err)
	}
	var rl *RateLimitError
	_, err = MistralResult("m", CLIOutcome{ExitErr: exit, Export: `{"outcome":"infra_error","error":{"message":"429 rate limit exceeded"}}`})
	if !errors.As(err, &rl) {
		t.Errorf("rate limit: %v", err)
	}
	if _, err = MistralResult("m", CLIOutcome{Export: `{"outcome":"turn_limit"}`}); err == nil {
		t.Error("unfinished outcome must fail")
	}
	if _, err = MistralResult("m", CLIOutcome{Stdout: "hi"}); err == nil {
		t.Error("missing export must fail")
	}
	if _, err = MistralResult("m", CLIOutcome{TimedOut: true, Timeout: time.Second}); err == nil {
		t.Error("timeout must fail")
	}
}

func TestMistralEnvNoKey(t *testing.T) {
	for _, e := range MistralEnv("/h") {
		if strings.HasPrefix(e, "MISTRAL_API_KEY") {
			t.Errorf("MistralEnv carries the key: %s", e)
		}
	}
}

func TestMistralArgsScalesTurns(t *testing.T) {
	args := MistralArgs(Request{Dir: "/w", MaxTurns: 32}, "/o")
	i := slices.Index(args, "--max-turns")
	if i < 0 || args[i+1] != "128" {
		t.Errorf("max-turns = %v", args)
	}
}
