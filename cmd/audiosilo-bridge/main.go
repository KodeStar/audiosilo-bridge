// Command audiosilo-bridge runs the AudioSilo Bridge: the optional companion
// that pairs with an AudioSilo server, writes to its libraries and runs the
// heavy jobs, so the server can stay read-only (BRIDGE-PLAN.md). It serves its
// own page on the LAN (pairing, library mappings, jobs, logs).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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

Commands:
  serve      Run the Bridge and its page (default).
  version    Print the build version.

Serve flags:
  --data     Data directory (default $AUDIOSILO_BRIDGE_DATA, else ~/.audiosilo-bridge).
  --listen   Bind address of the Bridge's page (default :8091). The page answers
             loopback and private addresses only, and asks for its password.
`)
}
