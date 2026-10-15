package agentrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// Main is `audiosilo-bridge agent-runner`: it sets the umask every CLI inherits
// and serves the staging dir until ctx ends.
//
// The umask is 077 for the whole runner process, not per child: os/exec can't set
// a child's umask between fork and exec, and a wrapper would put a shell in front
// of every run. The runner relies on no default mode itself: what the Bridge must
// read (runner.json, stdout.txt, stderr.txt, exit.json, last_message.txt) is
// fchmod'ed 0644 on the open file, the claim and the per-run dirs are 0700 on
// purpose, and work/out/ is opened explicitly after each run. So 077 costs the
// runner nothing and makes everything a CLI or its helpers write private by
// default (their config, caches, temp files and outputs).
func Main(ctx context.Context, opts Options) error {
	if !supported {
		return errors.New("the agent runner runs in the agent container (Linux)")
	}
	setUmask(0o077)
	r, err := New(opts)
	if err != nil {
		return err
	}
	return r.Run(ctx)
}

// Login is `audiosilo-bridge agent-login <cli>`, run once inside the agent
// container. codex: `codex login --device-auth` with CODEX_HOME on the agent
// volume, the terminal attached (it prints a URL and a code to enter in a
// browser; the login lands in CODEX_HOME/auth.json, 0600). claude: how to give
// the container a token. Neither echoes a credential.
func Login(ctx context.Context, opts Options, cli string, stdin io.Reader, stdout, stderr io.Writer) error {
	if opts.Staging == "" {
		opts.Staging = "-" // not used by a login
	}
	r, err := New(opts)
	if err != nil {
		return err
	}
	switch cli {
	case agentproto.CLIClaude:
		_, err := fmt.Fprintf(stdout, `The claude CLI signs in with a long-lived token from your Claude account:

  1. On any machine with the claude CLI, run:  claude setup-token
  2. Give the token to this container, either
       - as the environment variable CLAUDE_CODE_OAUTH_TOKEN (compose: in the
         agent service's environment, from your .env file), or
       - in the file %s (mode 0600).
  3. The Bridge's page shows claude as ready within about 10 seconds.

Or set ANTHROPIC_API_KEY instead (billed per token). Use the Bridge's own
account, not a shared one.
`, r.secretFile(envClaudeToken))
		return err
	case agentproto.CLICodex:
		return r.loginCodex(ctx, stdin, stdout, stderr)
	default:
		return fmt.Errorf("agent-login: unknown CLI %q (want claude or codex)", cli)
	}
}

// loginCodex runs codex's device login with the allow-listed environment and
// CODEX_HOME on the agent volume.
func (r *Runner) loginCodex(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	path, err := r.lookPath(r.opts.CodexBin)
	if err != nil {
		return fmt.Errorf("agent-login: %w", err)
	}
	if err := os.MkdirAll(r.codexHome(), 0o700); err != nil {
		return fmt.Errorf("agent-login: create %s: %w", r.codexHome(), err)
	}
	tmp, err := os.MkdirTemp(r.opts.TmpDir, "login-")
	if err != nil {
		return fmt.Errorf("agent-login: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := os.Mkdir(filepath.Join(tmp, "home"), 0o700); err != nil {
		return fmt.Errorf("agent-login: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, "login", "--device-auth") //nolint:gosec // the runner's own resolved codex binary
	cmd.Env = append(r.baseEnv(tmp), "CODEX_HOME="+r.codexHome())
	cmd.Dir = tmp
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("agent-login: codex login: %w", err)
	}
	return nil
}
