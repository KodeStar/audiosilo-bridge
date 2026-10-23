// Command audiosilo-bridge runs the AudioSilo Bridge: the optional companion
// that pairs with an AudioSilo server, writes to its libraries and runs the
// heavy jobs, so the server can stay read-only (BRIDGE-PLAN.md). It serves its
// own page on the LAN (pairing, library mappings, jobs, logs).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/kodestar/audiosilo-bridge/internal/agentrun"
	"github.com/kodestar/audiosilo-bridge/internal/app"
	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/logring"
)

// version is set at build time: -ldflags "-X main.version=...".
var version = "0.0.0-dev"

// dataEnv names the data directory when --data isn't given (the images set it
// to /bridge-data).
const dataEnv = "AUDIOSILO_BRIDGE_DATA"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	cmd, rest := "serve", args
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, rest = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(rest, stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, version)
		return nil
	case "reset-password":
		return resetPassword(rest, stdout, stderr)
	case "agent-runner":
		return agentRunner(rest, stderr)
	case "agent-login":
		return agentLogin(rest, stdout, stderr)
	case "worker":
		return workerCmd(rest, stdout, stderr)
	case "help":
		usage(stdout)
		return nil
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func serve(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", defaultDataDir(), "data directory (config, database, secrets)")
	listen := fs.String("listen", "", "bind address host:port (default "+config.DefaultListen+"; overrides config.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ring := logring.New(2000)
	log := slog.New(logring.NewHandler(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}), ring))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	b, oneTime, err := app.New(ctx, app.Options{DataDir: *data, Version: version, Listen: *listen, Logger: log, Ring: ring})
	if err != nil {
		return err
	}
	if oneTime != "" {
		// Printed once, never logged: the log ring keeps lines for the page.
		_, _ = fmt.Fprintf(stderr, "\n  AudioSilo Bridge: first start\n  The password for the Bridge's page is: %s\n  It is shown only this once; change it on the page.\n\n", oneTime)
	}
	return b.Run(ctx)
}

// resetPassword gives the page a new password and signs everyone out; the
// pairing and the secrets stay (app.ResetPassword).
func resetPassword(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("reset-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	data := fs.String("data", defaultDataDir(), "the Bridge's data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("reset-password takes no arguments, got %q", fs.Arg(0))
	}
	pw, err := app.ResetPassword(context.Background(), *data)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "\n  AudioSilo Bridge: password reset\n  The new password for the Bridge's page is: %s\n  It is shown only this once; every session was signed out. Change it on the page.\n  The pairing, mappings and stored secrets are unchanged.\n\n", pw)
	return nil
}

// agentRunner is the agent container's entrypoint (decision 15): it runs the
// agent jobs the Bridge drops into the shared staging dir.
func agentRunner(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("agent-runner", flag.ContinueOnError)
	fs.SetOutput(stderr)
	staging := fs.String("staging", "/staging", "the staging dir shared with the Bridge")
	home := fs.String("home", "/agent", "the agent home: secrets/ and codex/ (the agent-config volume)")
	slots := fs.Int("slots", 1, "jobs run at once")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return agentrun.Main(ctx, agentrun.Options{Staging: *staging, Home: *home, Slots: *slots, Version: version, Log: log})
}

// agentLogin signs a CLI in, inside the agent container.
func agentLogin(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("agent-login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "/agent", "the agent home (the agent-config volume)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("agent-login: name the CLI: agent-login claude | agent-login codex")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return agentrun.Login(ctx, agentrun.Options{Home: *home}, fs.Arg(0), os.Stdin, stdout, stderr)
}

// defaultDataDir is $AUDIOSILO_BRIDGE_DATA, else ~/.audiosilo-bridge.
func defaultDataDir() string { return defaultDir(dataEnv, ".audiosilo-bridge") }

// defaultDir is $env, else ~/<name> (else <name> with no home).
func defaultDir(env, name string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return name
	}
	return filepath.Join(home, name)
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `audiosilo-bridge - the AudioSilo Bridge

Usage:
  audiosilo-bridge [serve] [--data DIR] [--listen HOST:PORT]
  audiosilo-bridge version
  audiosilo-bridge reset-password [--data DIR]
  audiosilo-bridge agent-runner [--staging DIR] [--home DIR] [--slots N]
  audiosilo-bridge agent-login [--home DIR] claude|codex
  audiosilo-bridge worker --bridge URL [--token T] [--data DIR] [--backend B]
  audiosilo-bridge worker install --bridge URL --token T [--data DIR] [--backend B] [--print]
  audiosilo-bridge worker uninstall [--print]

Commands:
  serve         Run the Bridge and its page (default).
  version       Print the build version.
  reset-password
                Print a new password for the Bridge's page and sign every
                session out. The pairing, mappings and secrets stay. Run it
                as the data folder's owner (in Docker: docker exec -u
                PUID:PGID <container> audiosilo-bridge reset-password).
  agent-runner  Run agent jobs from the staging dir (the agent container's
                entrypoint; default --staging /staging --home /agent).
  agent-login   Sign a CLI in, inside the agent container: codex runs its
                device login, claude prints how to provide a token.
  worker        Run an ASR worker: transcribe the Bridge's audio here (a Mac's
                Metal, a GPU). It connects out to the Bridge's page address.
                Make its token on the Bridge's page (Workers).
  worker install
                Install the worker as a service that always runs (a launchd
                agent on macOS, a systemd user unit on Linux) and start it.
                The token is kept in the data dir, not in the service file.
                --print shows the service file without installing it.
  worker uninstall
                Stop and remove the service (the data dir stays).

Worker flags:
  --bridge       The Bridge page's address, e.g. http://192.168.1.20:8091.
  --token        The worker token (default: <data>/worker.token, which install
                 writes, else $AUDIOSILO_WORKER_TOKEN).
  --data         The worker's data dir: the speech recognition's tools and the
                 spool (default $AUDIOSILO_WORKER_DATA, else ~/.audiosilo-bridge-worker).
  --backend      auto (mlx-whisper on Apple Silicon, else whisper-cpp),
                 mlx-whisper or whisper-cpp.
  --model        The backend's model (default: large-v3-turbo).
  --whisper-cli  An explicit whisper-cli binary (whisper-cpp).

Serve flags:
  --data     Data directory (default $AUDIOSILO_BRIDGE_DATA, else ~/.audiosilo-bridge).
  --listen   Bind address of the Bridge's page (default :8091). The page answers
             loopback and private addresses only, and asks for its password.
`)
}
