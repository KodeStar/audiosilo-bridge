//go:build unix

package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// fakeCLI writes a shell script standing in for claude or codex. On a run it
// records its argv, environment, cwd, umask and stdin into capture/, then runs
// capture/behavior (a shell snippet) when a test left one there. The capture
// dir is baked into the script: the child's environment is allow-listed, so a
// variable can't carry it.
func fakeCLI(t *testing.T, name string) (bin, capture string) {
	t.Helper()
	dir := t.TempDir()
	capture = filepath.Join(dir, "capture")
	if err := os.Mkdir(capture, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
CAP='` + capture + `'
if [ "$1" = "--version" ]; then echo "9.9.9 (fake ` + name + `)"; exit 0; fi
printf '%s\n' "$@" > "$CAP/argv"
env | sort > "$CAP/env"
pwd > "$CAP/cwd"
umask > "$CAP/umask"
cat > "$CAP/stdin"
if [ -f "$CAP/behavior" ]; then . "$CAP/behavior"; fi
printf '%s' '{"type":"result","is_error":false,"result":"ok"}'
`
	bin = filepath.Join(dir, name)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// Run it once here: the first exec of a new file can be slow on a loaded
	// machine (macOS checks it), and the runner's own --version is capped and
	// some tests time the run.
	if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil { //nolint:gosec // the test's own script
		t.Fatalf("the fake %s doesn't run: %v: %s", name, err, out)
	}
	return bin, capture
}

type testEnv struct {
	r                       *Runner
	staging, home, tmp      string
	claudeCap, codexCap     string
	env                     map[string]string
	cancel                  context.CancelFunc
	stopped                 chan struct{}
	claudeBin, codexBinPath string
}

// newTestEnv is a Runner over temp dirs with fake CLIs and an injected parent
// environment (which carries variables a child must never see).
func newTestEnv(t *testing.T, env map[string]string) *testEnv {
	t.Helper()
	te := &testEnv{staging: t.TempDir(), home: t.TempDir(), tmp: t.TempDir(), env: env}
	te.claudeBin, te.claudeCap = fakeCLI(t, "claude")
	te.codexBinPath, te.codexCap = fakeCLI(t, "codex")
	if te.env == nil {
		te.env = map[string]string{}
	}
	if _, ok := te.env["PATH"]; !ok {
		te.env["PATH"] = os.Getenv("PATH")
	}
	r, err := New(Options{Staging: te.staging, Home: te.home, Version: "0.2.0-test", TmpDir: te.tmp,
		ClaudeBin: te.claudeBin, CodexBin: te.codexBinPath,
		Getenv: func(k string) (string, bool) { v, ok := te.env[k]; return v, ok }})
	if err != nil {
		t.Fatal(err)
	}
	r.scanEvery = 5 * time.Millisecond
	r.killDelay = time.Second
	te.r = r
	return te
}

func (te *testEnv) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	te.cancel, te.stopped = cancel, make(chan struct{})
	go func() { defer close(te.stopped); _ = te.r.Run(ctx) }()
	t.Cleanup(te.stop)
}

func (te *testEnv) stop() {
	if te.cancel != nil {
		te.cancel()
		<-te.stopped
		te.cancel = nil
	}
}

// submit writes a job dir as the Bridge does and returns it.
func (te *testEnv) submit(t *testing.T, request string) string {
	t.Helper()
	return writeJob(t, te.staging, request)
}

// writeJob writes a job dir under parent as the Bridge does and returns it.
func writeJob(t *testing.T, parent, request string) string {
	t.Helper()
	id, _ := agentproto.NewJobID()
	job := filepath.Join(parent, id)
	for _, d := range []string{job, filepath.Join(job, "work"), filepath.Join(job, "work", "out")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(d, 0o777)
	}
	if err := os.WriteFile(filepath.Join(job, "work", "input.txt"), []byte("in"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, agentproto.PromptFile), []byte("the prompt"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, agentproto.RequestTmpFile), []byte(request), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(job, agentproto.RequestTmpFile), filepath.Join(job, agentproto.RequestFile)); err != nil {
		t.Fatal(err)
	}
	return job
}

// waitDone waits for the job's done and returns exit.json. Whenever done exists,
// exit.json and stdout.txt must already be complete (done is the last write).
func waitDone(t *testing.T, job string, within time.Duration) agentproto.Exit {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(filepath.Join(job, agentproto.DoneFile)); err == nil {
			raw, err := os.ReadFile(filepath.Join(job, agentproto.ExitFile))
			if err != nil {
				t.Fatalf("done before exit.json: %v", err)
			}
			var ex agentproto.Exit
			if err := json.Unmarshal(raw, &ex); err != nil {
				t.Fatalf("exit.json = %q: %v", raw, err)
			}
			if _, err := os.Lstat(filepath.Join(job, agentproto.DoneTmpFile)); err == nil {
				t.Error("done.tmp left beside done")
			}
			return ex
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", filepath.Base(job))
	return agentproto.Exit{}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

const claudeReq = `{"protocol":1,"cli":"claude","model":"opus","effort":"high","web":true,"no_tools":false,"max_turns":40,"timeout_seconds":60}`
const codexReq = `{"protocol":1,"cli":"codex","model":"","effort":"","web":false,"no_tools":false,"max_turns":0,"timeout_seconds":60}`

// The parent environment's secrets: none may reach a child, and each CLI's
// credential only its own run.
func parentEnv() map[string]string {
	return map[string]string{
		"LANG":                        "C.UTF-8",
		"TZ":                          "Europe/London",
		"GITHUB_TOKEN":                "ghp_parent",
		"AUDIOSILO_BRIDGE_SECRET_KEY": "bridge-key",
		"AWS_SECRET_ACCESS_KEY":       "aws",
		"CLAUDE_CODE_OAUTH_TOKEN":     "sk-ant-oat01-claude",
		"OPENAI_API_KEY":              "sk-openai",
	}
}

func TestClaudeRun(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	te.start(t)
	job := te.submit(t, claudeReq)
	ex := waitDone(t, job, 30*time.Second)
	if ex.Error != "" || ex.ExitCode != 0 || ex.TimedOut || ex.Cancelled {
		t.Fatalf("exit = %+v", ex)
	}
	work := filepath.Join(job, "work")
	want := agent.ClaudeArgs(agent.Request{Dir: work, Model: "opus", Effort: "high", Web: true, MaxTurns: 40})
	if got := readLines(t, filepath.Join(te.claudeCap, "argv")); !slices.Equal(got, want) {
		t.Fatalf("argv = %q\nwant %q", got, want)
	}
	if cwd := readLines(t, filepath.Join(te.claudeCap, "cwd"))[0]; !sameDir(cwd, work) {
		t.Errorf("cwd = %q", cwd)
	}
	if stdin := readLines(t, filepath.Join(te.claudeCap, "stdin")); stdin[0] != "the prompt" {
		t.Errorf("stdin = %q", stdin)
	}
	stdout, _ := os.ReadFile(filepath.Join(job, agentproto.StdoutFile))
	if !strings.Contains(string(stdout), `"result":"ok"`) {
		t.Errorf("stdout.txt = %q", stdout)
	}
	for _, name := range []string{agentproto.StdoutFile, agentproto.StderrFile, agentproto.ExitFile} {
		if info, err := os.Stat(filepath.Join(job, name)); err != nil || info.Mode().Perm() != 0o644 {
			t.Errorf("%s mode = %v, %v", name, info.Mode(), err)
		}
	}
	if entries, _ := os.ReadDir(te.tmp); len(entries) != 0 {
		t.Errorf("the run's temp dir was left: %v", entries)
	}
}

func sameDir(a, b string) bool {
	ra, _ := filepath.EvalSymlinks(a)
	rb, _ := filepath.EvalSymlinks(b)
	return ra == rb
}

// The env allow-list: a claude run sees only the allow-listed variables and its
// own token; a codex run its own key, never claude's; neither the parent's
// GITHUB_TOKEN or the Bridge's key.
func TestChildEnvAllowList(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	te.start(t)
	waitDone(t, te.submit(t, claudeReq), 30*time.Second)
	waitDone(t, te.submit(t, codexReq), 30*time.Second)

	claudeEnv := envMap(readLines(t, filepath.Join(te.claudeCap, "env")))
	codexEnv := envMap(readLines(t, filepath.Join(te.codexCap, "env")))
	for _, env := range []map[string]string{claudeEnv, codexEnv} {
		for _, k := range []string{"GITHUB_TOKEN", "AUDIOSILO_BRIDGE_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"} {
			if _, ok := env[k]; ok {
				t.Errorf("%s reached a child", k)
			}
		}
		if env["DISABLE_AUTOUPDATER"] != "1" || env["LANG"] != "C.UTF-8" || env["TZ"] != "Europe/London" {
			t.Errorf("base env = %v", env)
		}
		if !strings.HasPrefix(env["TMPDIR"], filepath.Join(te.tmp, "run-")) || env["HOME"] != filepath.Join(env["TMPDIR"], "home") {
			t.Errorf("TMPDIR %q HOME %q", env["TMPDIR"], env["HOME"])
		}
	}
	if claudeEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-claude" || claudeEnv["CLAUDE_CONFIG_DIR"] != filepath.Join(claudeEnv["TMPDIR"], "claude") {
		t.Errorf("claude env = %v", claudeEnv)
	}
	if _, ok := claudeEnv["OPENAI_API_KEY"]; ok {
		t.Error("codex's key reached a claude run")
	}
	if codexEnv["OPENAI_API_KEY"] != "sk-openai" || codexEnv["CODEX_API_KEY"] != "sk-openai" || codexEnv["CODEX_HOME"] != filepath.Join(te.home, "codex") {
		t.Errorf("codex env = %v", codexEnv)
	}
	if _, ok := codexEnv["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Error("claude's token reached a codex run")
	}
	if len(claudeEnv) > 9 || len(codexEnv) > 10 {
		t.Errorf("more than the allow-list: claude %v codex %v", claudeEnv, codexEnv)
	}
}

func envMap(lines []string) map[string]string {
	m := map[string]string{}
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, "="); ok && k != "PWD" && k != "SHLVL" && k != "_" && k != "OLDPWD" {
			m[k] = v
		}
	}
	return m
}

// Credentials: subscription first, files under <home>/secrets, a codex login
// stops the key from being passed, and the kind is reported by presence only.
func TestCredentials(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api"})
	if got := te.r.credentialKind("claude"); got != "api_key" {
		t.Fatalf("claude = %q", got)
	}
	if got := te.r.credentialKind("codex"); got != "" {
		t.Fatalf("codex = %q", got)
	}
	secrets := filepath.Join(te.home, "secrets")
	_ = os.MkdirAll(secrets, 0o700)
	_ = os.WriteFile(filepath.Join(secrets, "claude_code_oauth_token"), []byte("sk-ant-oat01-file\n"), 0o600)
	_ = os.WriteFile(filepath.Join(secrets, "openai_api_key"), []byte("sk-file"), 0o600)
	if got := te.r.credentialKind("claude"); got != "oauth_token" {
		t.Fatalf("claude with a token file = %q", got)
	}
	env := envMap(te.r.childEnv("claude", "/tmp/run-x", nil))
	if env["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-file" || env["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("claude env = %v", env)
	}
	if te.r.credentialKind("codex") != "api_key" || envMap(te.r.childEnv("codex", "/tmp/run-x", nil))["OPENAI_API_KEY"] != "sk-file" {
		t.Fatal("codex key from a file")
	}
	_ = os.MkdirAll(filepath.Join(te.home, "codex"), 0o700)
	_ = os.WriteFile(filepath.Join(te.home, "codex", "auth.json"), []byte(`{"tokens":{}}`), 0o600)
	if te.r.credentialKind("codex") != "login" {
		t.Fatal("a codex login is the credential")
	}
	if _, ok := envMap(te.r.childEnv("codex", "/tmp/run-x", nil))["OPENAI_API_KEY"]; ok {
		t.Fatal("a key passed beside the login")
	}
	raw, _ := json.Marshal(te.r.status())
	for _, secret := range []string{"sk-ant-api", "sk-ant-oat01-file", "sk-file", "tokens"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("runner.json carries a credential: %s", raw)
		}
	}
}

func TestCodexRunArgvAndHomeWipe(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	codexHome := filepath.Join(te.home, "codex")
	_ = os.MkdirAll(filepath.Join(codexHome, "sessions", "2026"), 0o700)
	for name, body := range map[string]string{"auth.json": "{}", "config.toml": "", "history.jsonl": "old", "sessions/2026/s.jsonl": "x"} {
		_ = os.WriteFile(filepath.Join(codexHome, name), []byte(body), 0o600)
	}
	// The fake codex leaves what the real one does: the whole environment in a
	// shell snapshot, and the last message where --output-last-message says.
	behavior := `mkdir -p "$CODEX_HOME/shell_snapshots"; env > "$CODEX_HOME/shell_snapshots/snap.sh"
ls "$CODEX_HOME" > "$CAP/home_before"
prev=''; for a in "$@"; do if [ "$prev" = "--output-last-message" ]; then printf 'final words' > "$a"; fi; prev="$a"; done
`
	_ = os.WriteFile(filepath.Join(te.codexCap, "behavior"), []byte(behavior), 0o644)
	te.start(t)
	job := te.submit(t, codexReq)
	if ex := waitDone(t, job, 30*time.Second); ex.Error != "" || ex.ExitCode != 0 {
		t.Fatalf("exit = %+v", ex)
	}
	argv := readLines(t, filepath.Join(te.codexCap, "argv"))
	work := filepath.Join(job, "work")
	if i := slices.Index(argv, "--sandbox"); i < 0 || argv[i+1] != "danger-full-access" {
		t.Errorf("sandbox not off: %q", argv)
	}
	if i := slices.Index(argv, "--cd"); i < 0 || argv[i+1] != work {
		t.Errorf("--cd not the job's work dir: %q", argv)
	}
	if b, _ := os.ReadFile(filepath.Join(job, agentproto.LastMessageFile)); string(b) != "final words" {
		t.Errorf("last_message.txt = %q", b)
	}
	if before := readLines(t, filepath.Join(te.codexCap, "home_before")); !slices.Equal(before, []string{"auth.json", "config.toml", "shell_snapshots"}) {
		t.Errorf("CODEX_HOME at the run's start = %q (want it wiped but for the login and config)", before)
	}
	entries, _ := os.ReadDir(codexHome)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if !slices.Equal(left, []string{"auth.json", "config.toml"}) {
		t.Errorf("CODEX_HOME after the run = %q", left)
	}
}

// codex's last message is the CLI's own output: a link where it should be (to a
// credential, say) is never followed into last_message.txt for the Bridge.
func TestCodexLastMessageLinkRefused(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("a credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	behavior := `prev=''; for a in "$@"; do if [ "$prev" = "--output-last-message" ]; then rm -f "$a"; ln -s '` + secret + `' "$a"; fi; prev="$a"; done
`
	_ = os.WriteFile(filepath.Join(te.codexCap, "behavior"), []byte(behavior), 0o644)
	te.start(t)
	job := te.submit(t, codexReq)
	if ex := waitDone(t, job, 30*time.Second); ex.Error != "" || ex.ExitCode != 0 {
		t.Fatalf("exit = %+v", ex)
	}
	if b, err := os.ReadFile(filepath.Join(job, agentproto.LastMessageFile)); err == nil {
		t.Fatalf("last_message.txt = %q: the link was followed", b)
	}
}

// A malformed request is finished with an error, never run and never a crash;
// the runner goes on to the next job.
func TestMalformedRequests(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	te.start(t)
	for _, req := range []string{
		`{"protocol":1,"cli":"claude","timeout_seconds":60,"argv":["--dangerously-skip-permissions"]}`,
		`{"protocol":1,"cli":"sh","timeout_seconds":60}`,
		`{"protocol":1,"cli":"claude","model":"--tools","timeout_seconds":60}`,
		`not json at all`,
		`{"protocol":1,"cli":"claude","timeout_seconds":999999}`,
	} {
		ex := waitDone(t, te.submit(t, req), 30*time.Second)
		if ex.Error == "" || ex.Unavailable || ex.ExitCode != -1 {
			t.Errorf("%s: exit = %+v", req, ex)
		}
	}
	if _, err := os.Stat(filepath.Join(te.claudeCap, "argv")); err == nil {
		t.Fatal("a malformed request ran the CLI")
	}
	if ex := waitDone(t, te.submit(t, claudeReq), 30*time.Second); ex.Error != "" {
		t.Fatalf("the runner didn't go on: %+v", ex)
	}
}

// The runner reads what the Bridge wrote through the job dir's os.Root: a
// request.json or prompt.txt that is a link (here to a file outside the job dir,
// one the runner's uid can read) fails the job, the CLI never runs, and the
// link's target is never read into it.
func TestLinkedInputsRefused(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	te.start(t)
	outside := t.TempDir()
	secretPrompt := filepath.Join(outside, "prompt")
	secretRequest := filepath.Join(outside, "request")
	if err := os.WriteFile(secretPrompt, []byte("SECRET PROMPT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretRequest, []byte(claudeReq), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{agentproto.PromptFile: secretPrompt, agentproto.RequestFile: secretRequest} {
		t.Run(name, func(t *testing.T) {
			// Swapping a ready job's file would race the runner's claim, so the job
			// is built beside the staging dir and moved in whole.
			staged := writeJob(t, t.TempDir(), claudeReq)
			if err := os.Remove(filepath.Join(staged, name)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(staged, name)); err != nil {
				t.Fatal(err)
			}
			job := filepath.Join(te.staging, filepath.Base(staged))
			if err := os.Rename(staged, job); err != nil {
				t.Skip("can't move a job dir across these temp dirs:", err)
			}
			ex := waitDone(t, job, 30*time.Second)
			if ex.Error == "" || ex.ExitCode != -1 || !strings.Contains(ex.Error, "not a regular file") {
				t.Fatalf("exit = %+v", ex)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(te.claudeCap, "argv")); err == nil {
		t.Fatal("a job with a linked input ran the CLI")
	}
}

func TestNoCredentialIsUnavailable(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, nil)
	te.start(t)
	ex := waitDone(t, te.submit(t, claudeReq), 30*time.Second)
	if !ex.Unavailable || !strings.Contains(ex.Error, "no credential") {
		t.Fatalf("exit = %+v", ex)
	}
}

// A cancel file stops the run: SIGTERM to the process group (a helper the CLI
// started dies too), the job finished as cancelled.
func TestCancel(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	_ = os.WriteFile(filepath.Join(te.claudeCap, "behavior"), []byte(`sleep 60 & echo $! > "$CAP/helper"; sleep 60`), 0o644)
	te.start(t)
	job := te.submit(t, claudeReq)
	helperFile := filepath.Join(te.claudeCap, "helper")
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if b, err := os.ReadFile(helperFile); err == nil && len(b) > 0 {
			break
		}
	}
	if err := os.WriteFile(filepath.Join(job, agentproto.CancelFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ex := waitDone(t, job, 30*time.Second)
	if !ex.Cancelled || ex.TimedOut || ex.ExitCode == 0 {
		t.Fatalf("exit = %+v", ex)
	}
	b, _ := os.ReadFile(helperFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 {
		t.Fatalf("helper pid %q", b)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("the CLI's helper outlived the cancelled run")
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	_ = os.WriteFile(filepath.Join(te.claudeCap, "behavior"), []byte(`trap '' TERM; sleep 30`), 0o644)
	te.start(t)
	start := time.Now()
	ex := waitDone(t, te.submit(t, strings.Replace(claudeReq, `"timeout_seconds":60`, `"timeout_seconds":1`, 1)), 30*time.Second)
	if !ex.TimedOut || ex.Cancelled || ex.ExitCode == 0 {
		t.Fatalf("exit = %+v", ex)
	}
	// TERM was ignored, so KILL came killDelay (1 s) after the 1 s timeout.
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("took %s", d)
	}
}

// work/out/ is opened for the Bridge's uid without following a link.
func TestOpenForBridge(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("x"), 0o600)
	_ = os.MkdirAll(filepath.Join(out, "sub"), 0o700)
	_ = os.WriteFile(filepath.Join(out, "a.json"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(out, "sub", "b.json"), nil, 0o600)
	_ = os.Symlink(outside, filepath.Join(out, "link"))
	_ = os.Chmod(out, 0o700)
	openForBridge(out)
	for path, want := range map[string]os.FileMode{
		filepath.Join(out, "a.json"):        0o644,
		filepath.Join(out, "sub"):           0o777,
		filepath.Join(out, "sub", "b.json"): 0o644,
		outside:                             0o600,
		out:                                 0o700, // the Bridge's own dir isn't touched
	} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v (%v)", path, info.Mode().Perm(), want, err)
		}
	}
}

// A job an earlier runner claimed and never finished is finished at start.
func TestRecoverOrphans(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	orphan := te.submit(t, claudeReq)
	fresh := te.submit(t, claudeReq)
	for _, job := range []string{orphan, fresh} {
		_ = os.Mkdir(filepath.Join(job, agentproto.ClaimDir), 0o700)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(orphan, agentproto.ClaimDir), old, old)
	te.start(t)
	ex := waitDone(t, orphan, 30*time.Second)
	if !strings.Contains(ex.Error, "restarted") || !ex.Unavailable {
		t.Fatalf("exit = %+v", ex)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Lstat(filepath.Join(fresh, agentproto.DoneFile)); err == nil {
		t.Fatal("a job another runner is running was finished")
	}
}

// An earlier runner's run that was still fresh when this one started (a crash
// and a quick restart) is finished, as the agent being unavailable, once it is
// old enough: the Bridge gets that answer instead of a stale heartbeat.
func TestRecoverOrphansLater(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	te.r.statusEvery = 10 * time.Millisecond
	var shift atomic.Int64
	te.r.now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
	job := te.submit(t, claudeReq)
	if err := os.Mkdir(filepath.Join(job, agentproto.ClaimDir), 0o700); err != nil {
		t.Fatal(err)
	}
	te.start(t)
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Lstat(filepath.Join(job, agentproto.DoneFile)); err == nil {
		t.Fatal("a fresh claim was finished")
	}
	shift.Store(int64(time.Hour)) // its last sign of life is an hour old now
	ex := waitDone(t, job, 30*time.Second)
	if !ex.Unavailable || !strings.Contains(ex.Error, "restarted") {
		t.Fatalf("exit = %+v", ex)
	}
}

// The recovery never finishes this runner's own run, however old its claim
// looks (here: a clock an hour ahead while the CLI runs).
func TestRecoverOrphansSkipsOwnRuns(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	te.r.statusEvery = 5 * time.Millisecond
	var shift atomic.Int64
	te.r.now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
	_ = os.WriteFile(filepath.Join(te.claudeCap, "behavior"), []byte(`sleep 0.3`), 0o644)
	te.start(t)
	job := te.submit(t, claudeReq)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(te.claudeCap, "stdin")); err == nil {
			break
		}
	}
	shift.Store(int64(time.Hour))
	ex := waitDone(t, job, 30*time.Second)
	if ex.Error != "" || ex.ExitCode != 0 {
		t.Fatalf("the runner's own run was finished as an orphan: %+v", ex)
	}
}

func TestRunnerJSON(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-x"})
	te.start(t)
	var rj agentproto.Runner
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if raw, err := os.ReadFile(filepath.Join(te.staging, agentproto.RunnerFile)); err == nil && json.Unmarshal(raw, &rj) == nil {
			break
		}
	}
	if rj.Protocol != 1 || rj.Version != "0.2.0-test" || rj.Slots != 1 || time.Since(rj.HeartbeatAt) > time.Minute {
		t.Fatalf("runner.json = %+v", rj)
	}
	claude, codex := rj.CLIs["claude"], rj.CLIs["codex"]
	if claude.Version != "9.9.9 (fake claude)" || claude.Credential != "oauth_token" || claude.CredentialSince.IsZero() ||
		codex != (agentproto.CLI{Version: "9.9.9 (fake codex)"}) {
		t.Fatalf("clis = %+v", rj.CLIs)
	}
	if info, err := os.Stat(filepath.Join(te.staging, agentproto.RunnerFile)); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("runner.json mode = %v, %v", info.Mode(), err)
	}
}

func TestNewRefusesBadOptions(t *testing.T) {
	t.Parallel()
	for _, o := range []Options{{Home: "/agent"}, {Staging: "/staging"}, {Staging: "/s", Home: "/a", Slots: -1}, {Staging: "/s", Home: "/a", Slots: MaxSlots + 1}} {
		if _, err := New(o); err == nil {
			t.Errorf("New(%+v) accepted", o)
		}
	}
}

// The umask Main sets reaches the CLI: run in a child test process, since the
// umask is process-wide.
func TestChildUmask(t *testing.T) {
	t.Parallel()
	if os.Getenv("AGENTRUN_UMASK_HELPER") == "1" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUmaskHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "AGENTRUN_UMASK_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "UMASK=0077") {
		t.Fatalf("helper: %v\n%s", err, out)
	}
}

func TestUmaskHelper(t *testing.T) {
	if os.Getenv("AGENTRUN_UMASK_HELPER") != "1" {
		t.Skip("run by TestChildUmask")
	}
	setUmask(0o077)
	te := newTestEnv(t, parentEnv())
	te.start(t)
	waitDone(t, te.submit(t, claudeReq), 30*time.Second)
	t.Logf("UMASK=%s", readLines(t, filepath.Join(te.claudeCap, "umask"))[0])
}

// The runner stopping (the container going down) finishes its runs as the agent
// being unavailable, so the Bridge parks the book and resumes it on its own.
func TestStopFinishesRunsAsUnavailable(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	_ = os.WriteFile(filepath.Join(te.claudeCap, "behavior"), []byte(`sleep 60`), 0o644)
	te.start(t)
	job := te.submit(t, claudeReq)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(te.claudeCap, "stdin")); err == nil {
			break
		}
	}
	te.stop()
	ex := waitDone(t, job, time.Second)
	if !ex.Cancelled || !ex.Unavailable || !strings.Contains(ex.Error, "stopped") {
		t.Fatalf("exit = %+v", ex)
	}
}

// credential_since moves when the credential changes (and only then), so the
// Bridge can tell a replaced credential from the one the provider refused; the
// credential itself never reaches runner.json.
func TestCredentialSinceFollowsTheCredential(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-first"})
	var shift atomic.Int64
	te.r.now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
	first := te.r.status().CLIs["claude"].CredentialSince
	shift.Store(int64(time.Minute))
	if again := te.r.status().CLIs["claude"].CredentialSince; first.IsZero() || !again.Equal(first) {
		t.Fatalf("the same credential: since %v then %v", first, again)
	}
	te.env["CLAUDE_CODE_OAUTH_TOKEN"] = "sk-ant-oat01-second"
	shift.Store(int64(2 * time.Minute))
	st := te.r.status()
	if replaced := st.CLIs["claude"].CredentialSince; !replaced.After(first) {
		t.Fatalf("a replaced credential: since %v, was %v", replaced, first)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), "sk-ant") {
		t.Fatalf("runner.json carries the credential: %s", raw)
	}
	delete(te.env, "CLAUDE_CODE_OAUTH_TOKEN")
	if c := te.r.status().CLIs["claude"]; c.Credential != "" || !c.CredentialSince.IsZero() {
		t.Fatalf("no credential: %+v", c)
	}
}

// A CLI missing at the start (installed later, or too slow to answer --version
// then) is found without a restart: a run that finds it missing asks for a look,
// and the runner looks again now and then anyway.
func TestRedetectFindsALateCLI(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv())
	late := filepath.Join(t.TempDir(), "claude")
	te.r.opts.ClaudeBin = late
	te.r.statusEvery = 5 * time.Millisecond
	te.r.detectEvery = time.Hour // only the run's ask brings the look forward
	te.start(t)
	readClaude := func() agentproto.CLI {
		var rj agentproto.Runner
		raw, _ := os.ReadFile(filepath.Join(te.staging, agentproto.RunnerFile))
		_ = json.Unmarshal(raw, &rj)
		return rj.CLIs["claude"]
	}
	for deadline := time.Now().Add(30 * time.Second); readClaude().Credential == "" && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if c := readClaude(); c.Version != "" {
		t.Fatalf("claude before it was installed = %+v", c)
	}
	body, err := os.ReadFile(te.claudeBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(late, body, 0o755); err != nil { //nolint:gosec // the test's own script
		t.Fatal(err)
	}
	ex := waitDone(t, te.submit(t, claudeReq), 30*time.Second)
	if !ex.Unavailable || !strings.Contains(ex.Error, "no claude CLI") {
		t.Fatalf("the run before the look = %+v", ex)
	}
	for deadline := time.Now().Add(30 * time.Second); readClaude().Version == "" && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if c := readClaude(); c.Version != "9.9.9 (fake claude)" {
		t.Fatalf("claude after the look = %+v", c)
	}
	if ex := waitDone(t, te.submit(t, claudeReq), 30*time.Second); ex.Error != "" || ex.ExitCode != 0 {
		t.Fatalf("a run after the look = %+v", ex)
	}
}

// writeCredential puts the Bridge's credential.json into a job dir (before its
// request, as the Bridge does).
func writeCredential(t *testing.T, job, body string) string {
	t.Helper()
	dir := filepath.Join(job, agentproto.CredentialDir)
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, agentproto.CredentialFile)
	if err := os.WriteFile(p, []byte(body), 0o444); err != nil {
		t.Fatal(err)
	}
	return p
}

// submitWithCredential writes a job whose credential.json is in place before
// request.json appears.
func (te *testEnv) submitWithCredential(t *testing.T, request, credential string) (job, credPath string) {
	t.Helper()
	tmp := t.TempDir()
	job = writeJob(t, tmp, request)
	writeCredential(t, job, credential)
	dst := filepath.Join(te.staging, filepath.Base(job))
	if err := os.Rename(job, dst); err != nil {
		t.Fatal(err)
	}
	return dst, filepath.Join(dst, agentproto.CredentialDir, agentproto.CredentialFile)
}

// The hand-off: a container with no credential takes the Bridge's, deletes the
// file before the CLI starts, and passes it to that run only.
func TestHandoffCredential(t *testing.T) {
	t.Parallel()
	env := parentEnv()
	delete(env, "CLAUDE_CODE_OAUTH_TOKEN")
	te := newTestEnv(t, env)
	if !te.r.status().Handoff {
		t.Fatal("runner.json advertises the hand-off")
	}
	_ = os.WriteFile(filepath.Join(te.claudeCap, "behavior"),
		[]byte(`ls "$PWD/../credential" > "$CAP/credential_dir" 2>&1`+"\n"), 0o644)
	te.start(t)
	job, cred := te.submitWithCredential(t, claudeReq, `{"cli":"claude","kind":"oauth_token","value":"sk-ant-oat01-handed"}`)
	ex := waitDone(t, job, 30*time.Second)
	if ex.Error != "" || ex.ExitCode != 0 {
		t.Fatalf("exit = %+v", ex)
	}
	got := envMap(readLines(t, filepath.Join(te.claudeCap, "env")))
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-handed" || got["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("claude env = %v", got)
	}
	if _, err := os.Lstat(cred); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("credential.json is deleted before the CLI starts")
	}
	if listing, _ := os.ReadFile(filepath.Join(te.claudeCap, "credential_dir")); strings.Contains(string(listing), "credential.json") {
		t.Fatalf("the CLI saw credential.json: %s", listing)
	}
}

// The container's own credential wins over the hand-off (decision 19), and the
// handed file is deleted all the same.
func TestHandoffContainerWins(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t, parentEnv()) // CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-claude
	te.start(t)
	job, cred := te.submitWithCredential(t, claudeReq, `{"cli":"claude","kind":"api_key","value":"sk-ant-api-handed"}`)
	if ex := waitDone(t, job, 30*time.Second); ex.Error != "" {
		t.Fatalf("exit = %+v", ex)
	}
	got := envMap(readLines(t, filepath.Join(te.claudeCap, "env")))
	if got["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-claude" || got["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("claude env = %v", got)
	}
	if _, err := os.Lstat(cred); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("credential.json is deleted")
	}
}

// A malformed hand-off is refused (no run) and still deleted; no credential at
// all is the agent being unavailable.
func TestHandoffRefused(t *testing.T) {
	t.Parallel()
	env := parentEnv()
	delete(env, "CLAUDE_CODE_OAUTH_TOKEN")
	te := newTestEnv(t, env)
	te.start(t)
	job, cred := te.submitWithCredential(t, claudeReq, `{"cli":"codex","kind":"api_key","value":"sk-SECRET-x"}`)
	ex := waitDone(t, job, 30*time.Second)
	if ex.Error == "" || ex.Unavailable || strings.Contains(ex.Error, "sk-SECRET") {
		t.Fatalf("exit = %+v", ex)
	}
	if _, err := os.Lstat(cred); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused credential.json is deleted too")
	}
	if _, err := os.Stat(filepath.Join(te.claudeCap, "argv")); err == nil {
		t.Fatal("the CLI ran")
	}
	job = te.submit(t, claudeReq)
	if ex := waitDone(t, job, 30*time.Second); !ex.Unavailable {
		t.Fatalf("no credential at all: %+v", ex)
	}
}
