package agentrun

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// The credential variables. The runner reads each from its own environment, else
// from <home>/secrets/<lowercase name> (0600 files on the agent volume).
const (
	envClaudeToken = "CLAUDE_CODE_OAUTH_TOKEN"
	envAnthropic   = "ANTHROPIC_API_KEY"
	envOpenAI      = "OPENAI_API_KEY"
	envCodex       = "CODEX_API_KEY"
)

// credentialVars are all four, which an endpoint-only runner must not hold.
var credentialVars = []string{envClaudeToken, envAnthropic, envOpenAI, envCodex}

// defaultPath is the child's PATH when the runner has none.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// passThrough are the only runner variables a child sees (besides what baseEnv
// and childEnv set themselves).
var passThrough = []string{"LANG", "TZ"}

// codexKeep are the only files CODEX_HOME keeps between runs: the device login
// and the config. Everything else (shell_snapshots/, sessions/, logs, history)
// is wiped before and after each run: codex writes the whole shell environment
// into shell_snapshots/ (spike 2).
var codexKeep = map[string]bool{"auth.json": true, "config.toml": true}

// codexDir and secretsDir are the agent home's dirs for codex's CODEX_HOME and
// the credentials' files (the endpoint's service looks for both under its
// mounts: mounts.go).
const (
	codexDir   = "codex"
	secretsDir = "secrets"
)

// maxSecretBytes bounds a secrets file read.
const maxSecretBytes = 64 << 10

// path is the runner's PATH, or the default.
func (r *Runner) path() string {
	if v, ok := r.opts.Getenv("PATH"); ok && v != "" {
		return v
	}
	return defaultPath
}

// codexHome is CODEX_HOME: on the agent volume, since codex keeps its login
// there (and refuses a CODEX_HOME under /tmp).
func (r *Runner) codexHome() string { return filepath.Join(r.opts.Home, codexDir) }

// baseEnv is the allow-listed environment every child starts from: PATH, LANG,
// TZ, a private TMPDIR and HOME under runDir, and the autoupdater off. Nothing
// else of the runner's environment reaches a child.
func (r *Runner) baseEnv(runDir string) []string {
	env := []string{"PATH=" + r.path()}
	for _, k := range passThrough {
		if v, ok := r.opts.Getenv(k); ok && v != "" {
			env = append(env, k+"="+v)
		}
	}
	return append(env,
		"TMPDIR="+runDir,
		"HOME="+filepath.Join(runDir, "home"),
		"DISABLE_AUTOUPDATER=1",
	)
}

// childEnv is the environment of one CLI run: baseEnv plus that CLI's own config
// dir and its one credential, never the other CLI's.
//
// handed is the Bridge's credential for the run (credential.json), used only
// when the container has none of its own for cli.
func (r *Runner) childEnv(cli, runDir string, handed *agentproto.Credential) []string {
	env := r.baseEnv(runDir)
	own := r.credentialKind(cli)
	switch cli {
	case agentproto.CLIClaude:
		env = append(env, "CLAUDE_CONFIG_DIR="+filepath.Join(runDir, "claude"))
		switch {
		case own == agentproto.CredentialOAuthToken:
			env = append(env, envClaudeToken+"="+r.secret(envClaudeToken))
		case own == agentproto.CredentialAPIKey:
			env = append(env, envAnthropic+"="+r.secret(envAnthropic))
		case handed != nil && handed.Kind == agentproto.CredentialOAuthToken:
			env = append(env, envClaudeToken+"="+handed.Value)
		case handed != nil && handed.Kind == agentproto.CredentialAPIKey:
			env = append(env, envAnthropic+"="+handed.Value)
		}
	case agentproto.CLICodex:
		env = append(env, "CODEX_HOME="+r.codexHome())
		switch {
		case own == agentproto.CredentialAPIKey:
			key := r.openAIKey()
			env = append(env, envOpenAI+"="+key, envCodex+"="+key)
		case own == "" && handed != nil && handed.Kind == agentproto.CredentialAPIKey:
			env = append(env, envOpenAI+"="+handed.Value, envCodex+"="+handed.Value)
		}
	}
	return env
}

// endpointEnv is the environment of a codex run against an OpenAI-compatible
// endpoint (bridge#84): baseEnv, a CODEX_HOME of its own under runDir (empty:
// no device login), and the endpoint's key, when handed one, in
// agent.EndpointKeyEnv alone. None of the container's own credentials: they are
// for OpenAI and must never reach another endpoint.
func (r *Runner) endpointEnv(runDir string, handed *agentproto.Credential) []string {
	env := append(r.baseEnv(runDir), "CODEX_HOME="+endpointCodexHome(runDir))
	if handed != nil && handed.Kind == agentproto.CredentialEndpointKey {
		env = append(env, agent.EndpointKeyEnv+"="+handed.Value)
	}
	return env
}

// endpointCodexHome is an endpoint run's own CODEX_HOME, under its runDir.
func endpointCodexHome(runDir string) string { return filepath.Join(runDir, "codex") }

// credentialKind is the credential a CLI's runs use, by presence only (a value
// is never read for this): a subscription credential first (claude's OAuth
// token, codex's device login), else an API key, else "". A run gets exactly the
// one reported, so the kind on the Bridge's page is what is billed.
func (r *Runner) credentialKind(cli string) string {
	switch cli {
	case agentproto.CLIClaude:
		if r.hasSecret(envClaudeToken) {
			return agentproto.CredentialOAuthToken
		}
		if r.hasSecret(envAnthropic) {
			return agentproto.CredentialAPIKey
		}
	case agentproto.CLICodex:
		if info, err := os.Stat(filepath.Join(r.codexHome(), "auth.json")); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return agentproto.CredentialLogin
		}
		if r.hasSecret(envOpenAI) || r.hasSecret(envCodex) {
			return agentproto.CredentialAPIKey
		}
	}
	return ""
}

// credentialProblem (an endpoint-only runner's) says what puts a credential of
// the container's own within reach of a run against an endpoint, "" when
// nothing does (the CLI runs as the runner's uid with its sandbox off; execute
// refuses an endpoint run while there is one, and spawn ends one when the next
// check finds one): one of the four credential variables or the Bridge's
// vault variables in the runner's environment (a run reads
// /proc/<runner>/environ only while the runner is dumpable, but the variables
// are a configuration mistake either way), the agent home (homeReachable), or
// a mount that is the agent volume or holds a credential's file
// (mountProblem, mounts.go). It names a variable or a path, never a value: the
// log and runner.json say it.
//
// The mounts' walk is the slow part: each status tick walks them again
// (refreshMounts), as does execute before each endpoint run starts, and every
// other call takes the last walk's answer (the first walks them when there is
// none yet). So a run in progress, which spawn checks every second, ends at
// the tick after a credential appears.
func (r *Runner) credentialProblem() string {
	for _, k := range ownVars {
		if r.envSecret(k) != "" {
			return "the variable " + k + " is set"
		}
	}
	if r.homeReachable() {
		return r.opts.Home + " (the agent home) is reachable"
	}
	return r.mountProblem()
}

// ownVars are the variables an endpoint-only runner must not hold: the CLIs'
// credentials and the Bridge's vault's.
var ownVars = slices.Concat(credentialVars, vaultVars)

// mountProblem is the last walk of the mounts' answer, walking them first
// when there is none yet.
func (r *Runner) mountProblem() string {
	if last := r.mountLast.Load(); last != nil {
		return *last
	}
	r.mountMu.Lock()
	defer r.mountMu.Unlock()
	if r.mountLast.Load() == nil {
		r.walkMountsLocked()
	}
	return *r.mountLast.Load()
}

// refreshMounts walks the mounts now: the status tick's, so each interval has
// one fresh look.
func (r *Runner) refreshMounts() {
	r.mountMu.Lock()
	defer r.mountMu.Unlock()
	r.walkMountsLocked()
}

// walkMountsLocked walks the mounts (mounts.go) and keeps the answer; mountMu
// is held, so two callers never walk at once.
func (r *Runner) walkMountsLocked() {
	var why string
	if ms, err := r.mounts(); err != nil {
		why = "the mounts can't be read: " + err.Error()
	} else {
		why = (&mountWalk{looks: r.looks, deadline: time.Now().Add(walkBudget)}).credentialMount(ms)
	}
	r.mountLast.Store(&why)
}

// homeReachable reports whether the runner can list, enter or write the agent
// home (the subscriptions' secrets, codex's login and its config.toml, which a
// later subscription run loads). The endpoint's service mounts no agent volume,
// and runs as another uid than the agent container, whose 0700 home (the
// image's, which docker gives the service as an anonymous volume) is then
// closed to it. Anything but the home missing or closed counts as reachable:
// the answer must never be a wrong no (a look that doesn't answer in time
// included).
func (r *Runner) homeReachable() bool {
	if _, err := r.looks.lstat(r.opts.Home); err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	if f, err := os.Open(r.opts.Home); err == nil {
		_ = f.Close()
		return true
	}
	return enterable(r.opts.Home)
}

// endpointBlocked is exit.json's error for an endpoint run the container's own
// credentials stop (the page's status line and a parked book's reason).
const endpointBlocked = agentproto.OwnCredentialsReason + ": give that service no credential variables and " +
	"none of those volumes, and run it as its own user (the compose example's agent-endpoint)"

// endpointOnlyRefusal is exit.json's error for a CLI's own run sent to the
// endpoint's agent service, and endpointElsewhere for an endpoint run sent to
// the agent container (each staging dir has its own runner: agentbox's Status
// says which setting points at the wrong one).
const (
	endpointOnlyRefusal = "this agent service runs only OpenAI-compatible endpoints (agent-runner --endpoint-only): " +
		"point agent.staging_dir at the agent container's staging volume"
	endpointElsewhere = "runs against an OpenAI-compatible endpoint go to their own agent service, never to the agent " +
		"container that holds the subscriptions: set agent.endpoint_staging_dir to its staging volume (the compose " +
		"example's agent-endpoint)"
)

// credentialSince is when the runner first saw the credential cli has now (kind
// is its credentialKind; zero with none). The credential is compared by a
// fingerprint kept in memory: a replaced token, key or login moves the time, so
// the Bridge stops treating it as one the provider refused.
func (r *Runner) credentialSince(cli, kind string) time.Time {
	fp := ""
	if kind != "" {
		fp = r.credentialFingerprint(cli, kind)
	}
	r.credMu.Lock()
	defer r.credMu.Unlock()
	if fp == "" {
		delete(r.creds, cli)
		return time.Time{}
	}
	seen, ok := r.creds[cli]
	if !ok || seen.fp != fp {
		seen = credSeen{fp: fp, since: r.now().UTC()}
		r.creds[cli] = seen
	}
	return seen.since
}

// credentialFingerprint hashes the credential a run of cli gets (kind and value):
// the runner's own comparison, never written or logged.
func (r *Runner) credentialFingerprint(cli, kind string) string {
	var value string
	switch {
	case cli == agentproto.CLIClaude && kind == agentproto.CredentialOAuthToken:
		value = r.secret(envClaudeToken)
	case cli == agentproto.CLIClaude:
		value = r.secret(envAnthropic)
	case kind == agentproto.CredentialLogin:
		if f, err := os.Open(filepath.Join(r.codexHome(), "auth.json")); err == nil {
			b, _ := io.ReadAll(io.LimitReader(f, maxSecretBytes))
			_ = f.Close()
			value = string(b)
		}
	default:
		value = r.openAIKey()
	}
	sum := sha256.Sum256([]byte(cli + "\x00" + kind + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

// openAIKey is OPENAI_API_KEY, else CODEX_API_KEY.
func (r *Runner) openAIKey() string {
	if v := r.secret(envOpenAI); v != "" {
		return v
	}
	return r.secret(envCodex)
}

// secretFile is where a credential's file lives.
func (r *Runner) secretFile(name string) string {
	return filepath.Join(r.opts.Home, secretsDir, strings.ToLower(name))
}

// envSecret is a credential variable's value, trimmed ("" when unset or blank).
func (r *Runner) envSecret(name string) string {
	v, _ := r.opts.Getenv(name)
	return strings.TrimSpace(v)
}

// hasSecret reports a credential's presence: a non-empty variable, else a
// non-empty secrets file. The value isn't read.
func (r *Runner) hasSecret(name string) bool {
	if r.envSecret(name) != "" {
		return true
	}
	info, err := os.Stat(r.secretFile(name))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// secret reads a credential for a run. It goes into that run's environment and
// nowhere else: never argv, a log or a file the Bridge can read.
func (r *Runner) secret(name string) string {
	if v := r.envSecret(name); v != "" {
		return v
	}
	f, err := os.Open(r.secretFile(name))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// wipeCodexHome removes everything in CODEX_HOME but the login and the config.
func (r *Runner) wipeCodexHome() {
	entries, err := os.ReadDir(r.codexHome())
	if err != nil {
		return
	}
	for _, e := range entries {
		if codexKeep[e.Name()] && e.Type().IsRegular() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(r.codexHome(), e.Name())); err != nil {
			r.log.Warn("agent runner: wipe CODEX_HOME", "entry", e.Name(), "err", err)
		}
	}
}

// codexStart and codexEnd bracket a codex run: CODEX_HOME is wiped when the first
// concurrent codex run starts and when the last one ends, never under a run.
func (r *Runner) codexStart() {
	r.codexMu.Lock()
	defer r.codexMu.Unlock()
	if r.codexActive == 0 {
		r.wipeCodexHome()
	}
	r.codexActive++
}

func (r *Runner) codexEnd() {
	r.codexMu.Lock()
	defer r.codexMu.Unlock()
	r.codexActive--
	if r.codexActive == 0 {
		r.wipeCodexHome()
	}
}
