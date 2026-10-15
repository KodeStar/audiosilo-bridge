package agentrun

import (
	"io"
	"os"
	"path/filepath"
	"strings"

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
func (r *Runner) childEnv(cli, runDir string) []string {
	env := r.baseEnv(runDir)
	switch cli {
	case agentproto.CLIClaude:
		env = append(env, "CLAUDE_CONFIG_DIR="+filepath.Join(runDir, "claude"))
		switch r.credentialKind(cli) {
		case agentproto.CredentialOAuthToken:
			env = append(env, envClaudeToken+"="+r.secret(envClaudeToken))
		case agentproto.CredentialAPIKey:
			env = append(env, envAnthropic+"="+r.secret(envAnthropic))
		}
	case agentproto.CLICodex:
		env = append(env, "CODEX_HOME="+r.codexHome())
		if r.credentialKind(cli) == agentproto.CredentialAPIKey {
			key := r.openAIKey()
			env = append(env, envOpenAI+"="+key, envCodex+"="+key)
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
