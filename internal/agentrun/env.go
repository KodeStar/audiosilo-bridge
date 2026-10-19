package agentrun

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
func (r *Runner) codexHome() string { return filepath.Join(r.opts.Home, "codex") }

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
	return filepath.Join(r.opts.Home, "secrets", strings.ToLower(name))
}

// hasSecret reports a credential's presence: a non-empty variable, else a
// non-empty secrets file. The value isn't read.
func (r *Runner) hasSecret(name string) bool {
	if v, ok := r.opts.Getenv(name); ok && strings.TrimSpace(v) != "" {
		return true
	}
	info, err := os.Stat(r.secretFile(name))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// secret reads a credential for a run. It goes into that run's environment and
// nowhere else: never argv, a log or a file the Bridge can read.
func (r *Runner) secret(name string) string {
	if v, ok := r.opts.Getenv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
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
