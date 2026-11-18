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
	"github.com/kodestar/audiosilo-bridge/internal/execfile"
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
	if err := execfile.WriteFile(bin, []byte(script), 0o755); err != nil {
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
		if ended(pid) {
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("the CLI's helper outlived the cancelled run")
}

// ended reports whether pid has exited: gone, or a zombie nobody reaps. Its
// shell dies with it, so it is reparented to pid 1, which in a container run
// without an init (docker run without --init, the test binary or go as pid 1)
// never reaps it, and kill(pid, 0) still finds a zombie.
func ended(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")) // Linux; elsewhere kill alone
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return false
	}
	f := strings.Fields(string(b[i+1:]))
	return len(f) > 0 && f[0] == "Z"
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
	// Its groups, its own first: the Bridge hands a credential to its group
	// alone when they hold it.
	if len(rj.Groups) == 0 || rj.Groups[0] != os.Getegid() {
		t.Fatalf("groups = %v, want %d first", rj.Groups, os.Getegid())
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
	for _, o := range []Options{{Home: "/agent"}, {Staging: "/staging"}, {Staging: "/s", Home: "/a", Slots: -1}, {Staging: "/s", Home: "/a", Slots: MaxSlots + 1},
		{Staging: "/s", Home: "/a", Slots: 2, EndpointOnly: true}} {
		if _, err := New(o); err == nil {
			t.Errorf("New(%+v) accepted", o)
		}
	}
	// The endpoint's service runs one job at a time (allowed: one slot).
	if _, err := New(Options{Staging: "/s", Home: "/a", Slots: 1, EndpointOnly: true}); err != nil {
		t.Errorf("one slot refused: %v", err)
	}
}

// The endpoint's service starts only as its container's first process: any
// other there shares its user (denied: a test process, never pid 1; allowed
// is the agent image's own start, which the verifier runs).
func TestEndpointServiceStartsOnlyAsPID1(t *testing.T) {
	t.Parallel()
	if !supported || os.Getpid() == 1 {
		t.Skip("runs where the runner runs, and not as pid 1")
	}
	err := Main(context.Background(), Options{Staging: t.TempDir(), Home: filepath.Join(t.TempDir(), "agent"), EndpointOnly: true})
	if err == nil || !strings.Contains(err.Error(), "first process") {
		t.Fatalf("Main = %v, want the pid 1 refusal", err)
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
	if err := execfile.WriteFile(late, body, 0o755); err != nil {
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

// A credential.json the runner's uid may not take (handed to a group the
// volume's ids don't give it, as under a user namespace that maps them
// differently) is refused as credential_denied before the CLI starts, so the
// Bridge stages the job again for every uid; a malformed one never is
// (TestHandoffRefused).
func TestHandoffCredentialDenied(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root takes the file whatever its modes")
	}
	env := parentEnv()
	delete(env, "CLAUDE_CODE_OAUTH_TOKEN")
	te := newTestEnv(t, env)
	job, cred := te.submitWithCredential(t, claudeReq, `{"cli":"claude","kind":"oauth_token","value":"sk-ant-oat01-handed"}`)
	credDir := filepath.Dir(cred)
	if err := os.Chmod(credDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(credDir, 0o700) })
	te.start(t)
	ex := waitDone(t, job, 30*time.Second)
	if !ex.CredentialDenied || ex.Unavailable || ex.Error == "" || strings.Contains(ex.Error, "sk-ant") {
		t.Fatalf("exit = %+v", ex)
	}
	if _, err := os.Stat(filepath.Join(te.claudeCap, "argv")); err == nil {
		t.Fatal("the CLI ran")
	}
}

// The Bridge's group probe is taken (deleted) and refused by name: the Bridge
// reads an exit.json error naming the probe's kind as the runner's take
// (agentbox's probeJob), so the refusal must keep naming it (allowed: the
// kind is quoted in the error; denied: the CLI never runs on it).
func TestGroupProbeRefusalNamesTheKind(t *testing.T) {
	t.Parallel()
	env := parentEnv()
	delete(env, "CLAUDE_CODE_OAUTH_TOKEN")
	te := newTestEnv(t, env)
	te.start(t)
	probe := `{"cli":"claude","kind":"` + agentproto.CredentialGroupProbe + `","value":"` + agentproto.CredentialGroupProbe + `"}`
	job, cred := te.submitWithCredential(t, claudeReq, probe)
	ex := waitDone(t, job, 30*time.Second)
	if ex.Unavailable || ex.CredentialDenied || !strings.Contains(ex.Error, strconv.Quote(agentproto.CredentialGroupProbe)) {
		t.Fatalf("exit = %+v, want a refusal naming %q", ex, agentproto.CredentialGroupProbe)
	}
	if _, err := os.Lstat(cred); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the probe's credential.json is deleted")
	}
	if _, err := os.Stat(filepath.Join(te.claudeCap, "argv")); err == nil {
		t.Fatal("the CLI ran")
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
	if ex.Error == "" || ex.Unavailable || ex.CredentialDenied || strings.Contains(ex.Error, "sk-SECRET") {
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

const endpointReq = `{"protocol":1,"cli":"codex","model":"qwen3:32b","effort":"","web":true,"no_tools":false,"max_turns":0,"timeout_seconds":60,` +
	`"endpoint":{"base_url":"http://192.168.1.20:11434/v1"}}`

// endpointParentEnv is parentEnv without the container's own credentials: the
// endpoint's service holds none.
func endpointParentEnv() map[string]string {
	env := parentEnv()
	delete(env, "CLAUDE_CODE_OAUTH_TOKEN")
	delete(env, "OPENAI_API_KEY")
	delete(env, "AUDIOSILO_BRIDGE_SECRET_KEY") // the Bridge's vault: refused there too
	return env
}

// newEndpointEnv is newTestEnv as the endpoint's own service
// (--endpoint-only): no agent home (te.home names one that doesn't exist),
// no mount to look through (mounts_test.go gives it some), sweep counts the
// sweeps instead of killing anything (the tests run in parallel, so a real
// sweep could reach another test's CLI; TestEndpointSweepEndsStrays runs the
// real one alone), and the runner's uid is taken for another than this
// test's, which writes the jobs as the Bridge
// (TestEndpointServiceRunsOnlyTheBridgesJobs puts it back).
func newEndpointEnv(t *testing.T, env map[string]string) (te *testEnv, sweeps *atomic.Int32) {
	t.Helper()
	te = newTestEnv(t, env)
	te.home = filepath.Join(t.TempDir(), "agent")
	te.r.opts.Home, te.r.opts.EndpointOnly, te.r.selfUID = te.home, true, -1
	te.r.mounts = func() ([]mount, error) { return nil, nil }
	sweeps = new(atomic.Int32)
	te.r.sweep = func(func(int) bool) int { sweeps.Add(1); return 0 }
	return te, sweeps
}

// An endpoint run (bridge#84) in the endpoint's service gets the handed
// endpoint key in its own variable, named to codex by env_key, never in argv;
// its own empty CODEX_HOME; nothing else of the runner's environment; and a
// sweep before its CLI starts and after it ends.
func TestEndpointRun(t *testing.T) {
	t.Parallel()
	te, sweeps := newEndpointEnv(t, endpointParentEnv())
	_ = os.WriteFile(filepath.Join(te.codexCap, "behavior"), []byte(`ls -A "$CODEX_HOME" > "$CAP/home"`+"\n"), 0o644)
	if st := te.r.status(); !st.Endpoints || !st.EndpointOnly || st.OwnCredentials {
		t.Fatalf("runner.json = %+v, want an endpoint-only runner with no credential of its own", st)
	}
	te.start(t)
	job, cred := te.submitWithCredential(t, endpointReq, `{"cli":"codex","kind":"endpoint_api_key","value":"sk-endpoint-SECRET"}`)
	if ex := waitDone(t, job, 30*time.Second); ex.Error != "" || ex.ExitCode != 0 {
		t.Fatalf("exit = %+v", ex)
	}
	if _, err := os.Lstat(cred); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("credential.json is deleted before the CLI starts")
	}
	argv := readLines(t, filepath.Join(te.codexCap, "argv"))
	joined := strings.Join(argv, "\n")
	if strings.Contains(joined, "SECRET") {
		t.Fatalf("the key is in argv: %q", argv)
	}
	for _, want := range []string{`model_provider="audiosilo_endpoint"`,
		`model_providers.audiosilo_endpoint.base_url="http://192.168.1.20:11434/v1"`,
		`model_providers.audiosilo_endpoint.wire_api="responses"`,
		`model_providers.audiosilo_endpoint.env_key="AUDIOSILO_ENDPOINT_API_KEY"`, `web_search="disabled"`} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv lacks %s: %q", want, argv)
		}
	}
	if i := slices.Index(argv, "--model"); i < 0 || argv[i+1] != "qwen3:32b" {
		t.Errorf("--model: %q", argv)
	}
	got := envMap(readLines(t, filepath.Join(te.codexCap, "env")))
	if got["AUDIOSILO_ENDPOINT_API_KEY"] != "sk-endpoint-SECRET" {
		t.Errorf("the endpoint key isn't in its variable: %v", got)
	}
	for _, k := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "GITHUB_TOKEN"} {
		if _, ok := got[k]; ok {
			t.Errorf("an endpoint run got %s", k)
		}
	}
	if !strings.HasPrefix(got["CODEX_HOME"], te.tmp) {
		t.Errorf("CODEX_HOME = %q, want the run's own under %s", got["CODEX_HOME"], te.tmp)
	}
	if home, _ := os.ReadFile(filepath.Join(te.codexCap, "home")); strings.TrimSpace(string(home)) != "" {
		t.Errorf("the run's CODEX_HOME held %q, want it empty", home)
	}
	if _, err := os.Lstat(te.home); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the endpoint's service made an agent home")
	}
	if n := sweeps.Load(); n != 2 {
		t.Errorf("%d sweeps, want one before the CLI and one after", n)
	}
}

// An endpoint's model is whoever answers at the address, and the CLI runs its
// commands as the runner's uid with its sandbox off: while the endpoint's
// service holds a credential in its environment, or can reach the agent home
// (where it could read the subscriptions' secrets or plant a config.toml a
// later subscription run loads), the run is refused as the agent unavailable,
// the hand-off taken and deleted all the same and no CLI started (denied);
// runner.json says so for the Bridge. A home that is there but closed to the
// runner's uid (the agent container's 0700 one, as an anonymous volume) is
// out of reach (allowed).
func TestEndpointRunRefusedBesideOwnCredentials(t *testing.T) {
	t.Parallel()
	for name, hold := range map[string]func(t *testing.T, env map[string]string, home string){
		"claude's token in the environment": func(_ *testing.T, env map[string]string, _ string) {
			env["CLAUDE_CODE_OAUTH_TOKEN"] = "sk-ant-oat01-claude"
		},
		"an Anthropic key in the environment": func(_ *testing.T, env map[string]string, _ string) {
			env["ANTHROPIC_API_KEY"] = "sk-ant-api03-x"
		},
		"an OpenAI key in the environment": func(_ *testing.T, env map[string]string, _ string) {
			env["OPENAI_API_KEY"] = "sk-openai"
		},
		"a codex key in the environment": func(_ *testing.T, env map[string]string, _ string) {
			env["CODEX_API_KEY"] = "sk-codex"
		},
		"the Bridge's vault key in the environment": func(_ *testing.T, env map[string]string, _ string) {
			env["AUDIOSILO_BRIDGE_SECRET_KEY"] = "bridge-key"
		},
		"the Bridge's GitHub token in the environment": func(_ *testing.T, env map[string]string, _ string) {
			env["AUDIOSILO_BRIDGE_GITHUB_TOKEN"] = "ghp_bridge"
		},
		"the agent home mounted, empty": func(t *testing.T, _ map[string]string, home string) {
			if err := os.Mkdir(home, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"the agent home mounted, writable but not listable": func(t *testing.T, _ map[string]string, home string) {
			if err := os.Mkdir(home, 0o300); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := endpointParentEnv()
			te, _ := newEndpointEnv(t, env)
			if te.r.credentialProblem() != "" || te.r.status().OwnCredentials {
				t.Fatal("a service with no credential and no home reports one")
			}
			hold(t, env, te.home)
			if !te.r.status().OwnCredentials {
				t.Fatal("runner.json doesn't say the service holds a credential")
			}
			te.start(t)
			job, cred := te.submitWithCredential(t, endpointReq, `{"cli":"codex","kind":"endpoint_api_key","value":"sk-endpoint-SECRET"}`)
			ex := waitDone(t, job, 30*time.Second)
			if !ex.Unavailable || !strings.Contains(ex.Error, "credential of its own") || strings.Contains(ex.Error, "SECRET") {
				t.Fatalf("exit = %+v, want the run refused as unavailable", ex)
			}
			if _, err := os.Lstat(cred); !errors.Is(err, fs.ErrNotExist) {
				t.Fatal("the endpoint key's credential.json is deleted all the same")
			}
			if _, err := os.Stat(filepath.Join(te.codexCap, "argv")); err == nil {
				t.Fatal("the CLI ran beside the service's own credential")
			}
		})
	}
	t.Run("the agent home there but closed", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root reaches every dir")
		}
		te, _ := newEndpointEnv(t, endpointParentEnv())
		if err := os.Mkdir(te.home, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(te.home, 0o700) })
		if te.r.status().OwnCredentials {
			t.Fatal("a home closed to the runner counts as reachable")
		}
		te.start(t)
		if ex := waitDone(t, te.submit(t, endpointReq), 30*time.Second); ex.Error != "" || ex.ExitCode != 0 {
			t.Fatalf("exit = %+v", ex)
		}
	})
}

// Each kind of run goes to its own service: the agent container (with the
// subscriptions) refuses an endpoint run, and the endpoint's service a CLI's
// own run, each as the agent unavailable, the hand-off deleted and no CLI
// started (denied); each runs its own kind (allowed: TestEndpointRun,
// TestClaudeRun). runner.json tells them apart.
func TestRunsGoToTheirOwnService(t *testing.T) {
	t.Parallel()
	t.Run("an endpoint run in the agent container", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t, endpointParentEnv())
		if st := te.r.status(); !st.Endpoints || st.EndpointOnly || st.OwnCredentials {
			t.Fatalf("runner.json = %+v", st)
		}
		te.start(t)
		job, cred := te.submitWithCredential(t, endpointReq, `{"cli":"codex","kind":"endpoint_api_key","value":"sk-endpoint-SECRET"}`)
		ex := waitDone(t, job, 30*time.Second)
		if !ex.Unavailable || !strings.Contains(ex.Error, "agent.endpoint_staging_dir") || strings.Contains(ex.Error, "SECRET") {
			t.Fatalf("exit = %+v", ex)
		}
		if _, err := os.Lstat(cred); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("the endpoint key's credential.json is deleted all the same")
		}
		if _, err := os.Stat(filepath.Join(te.codexCap, "argv")); err == nil {
			t.Fatal("codex ran against the endpoint in the agent container")
		}
	})
	t.Run("a claude run in the endpoint's service", func(t *testing.T) {
		t.Parallel()
		te, _ := newEndpointEnv(t, endpointParentEnv())
		te.start(t)
		job, cred := te.submitWithCredential(t, claudeReq, `{"cli":"claude","kind":"oauth_token","value":"sk-ant-oat01-HANDED"}`)
		ex := waitDone(t, job, 30*time.Second)
		if !ex.Unavailable || !strings.Contains(ex.Error, "only OpenAI-compatible endpoints") || strings.Contains(ex.Error, "HANDED") {
			t.Fatalf("exit = %+v", ex)
		}
		if _, err := os.Lstat(cred); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("the handed token's credential.json is deleted all the same")
		}
		if _, err := os.Stat(filepath.Join(te.claudeCap, "argv")); err == nil {
			t.Fatal("claude ran in the endpoint's service")
		}
	})
}

// The endpoint's service runs only the Bridge's jobs: a job dir its own uid
// made (a run's commands share that uid and can write the staging dir, so it
// is one a run left there to run again after it) is refused as the agent
// unavailable, its credential.json left alone and no CLI started (denied),
// even with the oldest request; a job of another uid's making runs (allowed:
// TestEndpointRun, where the runner's uid is another than the job's). The
// agent container's runner is unchanged (TestClaudeRun: a job of its own uid).
func TestEndpointServiceRunsOnlyTheBridgesJobs(t *testing.T) {
	t.Parallel()
	te, _ := newEndpointEnv(t, endpointParentEnv())
	te.r.selfUID = os.Geteuid() // this test made the job, as a run would
	te.start(t)
	job, cred := te.submitWithCredential(t, endpointReq, `{"cli":"codex","kind":"endpoint_api_key","value":"sk-planted"}`)
	old := time.Unix(1, 0)
	_ = os.Chtimes(filepath.Join(job, agentproto.RequestFile), old, old)
	ex := waitDone(t, job, 30*time.Second)
	if !ex.Unavailable || !strings.Contains(ex.Error, "only the jobs the Bridge stages") {
		t.Fatalf("exit = %+v, want the planted job refused", ex)
	}
	if _, err := os.Lstat(cred); err != nil {
		t.Fatal("a planted job's credential.json was taken")
	}
	if _, err := os.Stat(filepath.Join(te.codexCap, "argv")); err == nil {
		t.Fatal("codex ran a planted job")
	}
}

// onlyStrays: the atomic kill(-1) is for a pid namespace holding nothing but
// init, the runner and the strays; anything else in it (a CLI running now,
// the runner's own --version, a docker exec) keeps the sweep to its strays.
func TestOnlyStrays(t *testing.T) {
	t.Parallel()
	const self = 100
	strays := []proc{{pid: 300, ppid: self, sid: 300}, {pid: 301, ppid: 300, sid: 301}}
	base := []proc{{pid: 1, ppid: 0, sid: 1}, {pid: self, ppid: 1, sid: 1}}
	if !onlyStrays(append(slices.Clone(base), strays...), self, strays) {
		t.Error("init, the runner and its strays alone: the namespace is the strays'")
	}
	for name, other := range map[string]proc{
		"a CLI running now":      {pid: 200, ppid: self, sid: 200},
		"the runner's --version": {pid: 500, ppid: self, sid: 1},
		"a docker exec":          {pid: 600, ppid: 0, sid: 600},
	} {
		if onlyStrays(append(append(slices.Clone(base), strays...), other), self, strays) {
			t.Errorf("%s beside the strays: the namespace isn't theirs alone", name)
		}
	}
}

// The sweep ends the runner's children in a session it doesn't keep, with
// everything under them; a kept session's processes (a CLI running now, the
// runner's own) are spared with their descendants, whatever sessions those
// started.
func TestStrays(t *testing.T) {
	t.Parallel()
	const self = 100
	procs := []proc{
		{pid: 1, ppid: 0, sid: 1},
		{pid: self, ppid: 1, sid: 1},
		{pid: 200, ppid: self, sid: 200},               // a CLI running now
		{pid: 201, ppid: 200, sid: 200},                // its helper
		{pid: 202, ppid: 201, sid: 202},                // its helper's own session, still under it
		{pid: 300, ppid: self, sid: 300},               // an ended run's leftover, reparented
		{pid: 301, ppid: 300, sid: 301},                // what it started
		{pid: 400, ppid: self, sid: 400, zombie: true}, // exited, unreaped
		{pid: 500, ppid: self, sid: 1},                 // the runner's own --version
		{pid: 600, ppid: 1, sid: 600},                  // not the runner's
	}
	keep := func(sid int) bool { return sid == 200 || sid == 1 }
	var got []int
	for _, p := range strays(procs, self, keep) {
		got = append(got, p.pid)
	}
	if want := []int{300, 301, 400}; !slices.Equal(got, want) {
		t.Fatalf("strays = %v, want %v", got, want)
	}
}

// An endpoint without a key (a local server) runs with no env_key at all; a
// handed OpenAI key is refused for it (allowed: keyless; denied: a key of
// another kind, and an endpoint key for a run against OpenAI).
func TestEndpointRunKeys(t *testing.T) {
	t.Parallel()
	te, _ := newEndpointEnv(t, endpointParentEnv())
	te.start(t)
	job := te.submit(t, endpointReq)
	if ex := waitDone(t, job, 30*time.Second); ex.Error != "" || ex.ExitCode != 0 {
		t.Fatalf("keyless endpoint: exit = %+v", ex)
	}
	argv := readLines(t, filepath.Join(te.codexCap, "argv"))
	if slices.ContainsFunc(argv, func(a string) bool { return strings.Contains(a, "env_key") }) {
		t.Errorf("a keyless run names an env_key (codex would fail on the missing variable): %q", argv)
	}
	_ = os.Remove(filepath.Join(te.codexCap, "argv"))

	job, _ = te.submitWithCredential(t, endpointReq, `{"cli":"codex","kind":"api_key","value":"sk-openai-SECRET"}`)
	if ex := waitDone(t, job, 30*time.Second); ex.Error == "" || ex.Unavailable || strings.Contains(ex.Error, "SECRET") {
		t.Fatalf("an OpenAI key for an endpoint: exit = %+v", ex)
	}
	if _, err := os.Stat(filepath.Join(te.codexCap, "argv")); err == nil {
		t.Fatal("the CLI ran on a refused credential")
	}
	sub := newTestEnv(t, endpointParentEnv())
	sub.start(t)
	job, _ = sub.submitWithCredential(t, codexReq, `{"cli":"codex","kind":"endpoint_api_key","value":"sk-endpoint-SECRET"}`)
	if ex := waitDone(t, job, 30*time.Second); ex.Error == "" || strings.Contains(ex.Error, "SECRET") {
		t.Fatalf("an endpoint key for OpenAI: exit = %+v", ex)
	}
	if _, err := os.Stat(filepath.Join(sub.codexCap, "argv")); err == nil {
		t.Fatal("the CLI ran on a refused credential")
	}
}
