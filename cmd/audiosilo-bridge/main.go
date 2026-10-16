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
	case "agent-runner":
		return agentRunner(rest, stderr)
	case "agent-login":
		return agentLogin(rest, stdout, stderr)
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
func defaultDataDir() string {
	if v := os.Getenv(dataEnv); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".audiosilo-bridge"
	}
	return filepath.Join(home, ".audiosilo-bridge")
}

func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `audiosilo-bridge - the AudioSilo Bridge

Usage:
  audiosilo-bridge [serve] [--data DIR] [--listen HOST:PORT]
  audiosilo-bridge version
  audiosilo-bridge agent-runner [--staging DIR] [--home DIR] [--slots N]
  audiosilo-bridge agent-login [--home DIR] claude|codex

Commands:
  serve         Run the Bridge and its page (default).
  version       Print the build version.
  agent-runner  Run agent jobs from the staging dir (the agent container's
                entrypoint; default --staging /staging --home /agent).
  agent-login   Sign a CLI in, inside the agent container: codex runs its
                device login, claude prints how to provide a token.

Serve flags:
  --data     Data directory (default $AUDIOSILO_BRIDGE_DATA, else ~/.audiosilo-bridge).
  --listen   Bind address of the Bridge's page (default :8091). The page answers
             loopback and private addresses only, and asks for its password.
`)
}
