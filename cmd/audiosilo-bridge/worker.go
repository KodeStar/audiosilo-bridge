package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/kodestar/audiosilo-bridge/internal/asr"
	"github.com/kodestar/audiosilo-bridge/internal/worker"
)

// The worker's environment: its data dir and token when the flags don't
// give them.
const (
	workerDataEnv  = "AUDIOSILO_WORKER_DATA"
	workerTokenEnv = "AUDIOSILO_WORKER_TOKEN"
)

// workerFlags are the flags `worker` and `worker install` share.
type workerFlags struct {
	bridge, token, data, backend, model, whisperCLI *string
}

func addWorkerFlags(fs *flag.FlagSet) workerFlags {
	return workerFlags{
		bridge:     fs.String("bridge", "", "the Bridge page's address, e.g. http://192.168.1.20:8091"),
		token:      fs.String("token", "", "the worker token (default <data>/"+worker.TokenFileName+", else $"+workerTokenEnv+")"),
		data:       fs.String("data", defaultWorkerDataDir(), "the worker's data dir (tools and spool)"),
		backend:    fs.String("backend", "auto", "auto, mlx-whisper or whisper-cpp"),
		model:      fs.String("model", "", "the backend's model (default large-v3-turbo)"),
		whisperCLI: fs.String("whisper-cli", "", "an explicit whisper-cli binary (whisper-cpp)"),
	}
}

// passthrough are the flags the service runs with besides --data and --bridge.
func (f workerFlags) passthrough() []string {
	var out []string
	if b := strings.TrimSpace(*f.backend); b != "" && b != "auto" {
		out = append(out, "--backend", b)
	}
	if m := strings.TrimSpace(*f.model); m != "" {
		out = append(out, "--model", m)
	}
	if p := strings.TrimSpace(*f.whisperCLI); p != "" {
		out = append(out, "--whisper-cli", p)
	}
	return out
}

func workerCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return workerInstall(args[1:], stdout, stderr, false)
		case "uninstall":
			return workerInstall(args[1:], stdout, stderr, true)
		}
	}
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := addWorkerFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*f.bridge) == "" {
		return errors.New("worker: --bridge is required (the Bridge page's address)")
	}
	token := f.tokenIn(*f.data)
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("worker: starting", "version", version, "bridge", *f.bridge, "data", *f.data)
	return worker.Run(ctx, worker.Options{BridgeURL: *f.bridge, Token: token, DataDir: *f.data, Version: version, Log: log,
		ASR: asr.SelectConfig{Backend: *f.backend, Model: *f.model, WhisperCLIPath: *f.whisperCLI, AutoDownload: true}})
}

func workerInstall(args []string, stdout, stderr io.Writer, remove bool) error {
	name := "worker install"
	if remove {
		name = "worker uninstall"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := addWorkerFlags(fs)
	printOnly := fs.Bool("print", false, "show the service file and do nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// An update (or an uninstall) keeps what the installed service runs with
	// unless the command line says otherwise.
	kept, keepErr := carryForward(fs, worker.Service{GOOS: runtime.GOOS, Home: home})
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	data, err := filepath.Abs(*f.data)
	if err != nil {
		return err
	}
	svc := worker.Service{GOOS: runtime.GOOS, Home: home, UID: os.Getuid(), Binary: bin, DataDir: data,
		BridgeURL: strings.TrimSpace(*f.bridge), Args: f.passthrough(), Path: os.Getenv("PATH"),
		Run: func(name string, args ...string) error {
			cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // launchctl/systemctl with fixed verbs
			cmd.Stdout, cmd.Stderr = stdout, stderr
			if len(args) > 0 && (args[0] == "bootout" || args[0] == "print") {
				// Unloading a job that isn't loaded (a first install) only says so, and
				// print (waiting for the old job to go) dumps launchd's state.
				cmd.Stdout, cmd.Stderr = nil, nil
			}
			return cmd.Run()
		}}
	if remove {
		if *printOnly {
			path, err := svc.File()
			_, _ = fmt.Fprintln(stdout, "would stop the service and remove", path)
			return err
		}
		path, err := svc.Uninstall()
		if err == nil {
			// The service's --data (read from its file, or given): name the folder
			// only when it is known.
			note := "Its data dir (the --data it ran with, ~/.audiosilo-bridge-worker by default) stays."
			if dataSet(fs) {
				note = "Its data dir stays: " + data
			}
			_, _ = fmt.Fprintln(stdout, "Removed the worker's service ("+path+"). "+note)
		}
		return err
	}
	// Said before the --bridge check: an unreadable service is why an update
	// suddenly needs --bridge.
	if keepErr != nil {
		_, _ = fmt.Fprintln(stderr, "worker install: the installed service's settings can't be read, so it gets the defaults "+
			"and the flags given here ("+keepErr.Error()+")")
	} else if len(kept) > 0 && !*printOnly {
		_, _ = fmt.Fprintln(stdout, "Keeping the installed service's "+strings.Join(kept, ", ")+" (give a flag to change it).")
	}
	if svc.BridgeURL == "" {
		return errors.New("worker install: --bridge is required (the Bridge page's address)")
	}
	if *printOnly {
		text, err := svc.Render()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(stdout, text)
		return nil
	}
	token := f.tokenIn(data)
	if token == "" {
		return errors.New("worker install: --token is required the first time (make one on the Bridge's page, Workers)")
	}
	if err := worker.SaveToken(data, token); err != nil {
		return err
	}
	path, err := svc.Install()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "Installed and started the worker ("+path+").")
	switch runtime.GOOS {
	case "darwin":
		_, _ = fmt.Fprintln(stdout, "Its log: "+filepath.Join(data, worker.LogFileName))
	case "linux":
		_, _ = fmt.Fprintln(stdout, "Its log: journalctl --user -u "+worker.SystemdUnit+" -f")
		_, _ = fmt.Fprintln(stdout, "To keep it running when you are logged out: loginctl enable-linger "+os.Getenv("USER"))
	}
	return nil
}

// carryForward sets each worker flag but --token (which never goes in the
// service file) the command line (fs, parsed) didn't give to the installed
// service's value, read back from svc's file and parsed with the flags that
// wrote it, so an update keeps a customised worker's --data, --backend,
// --model, --whisper-cli and --bridge. It returns the flags
// it kept as "--name". No service file is a first install (nothing kept, no
// error); one that doesn't read as `worker install` writes it is an error, and
// nothing is kept (the defaults apply).
func carryForward(fs *flag.FlagSet, svc worker.Service) ([]string, error) {
	argv, err := svc.InstalledArgs()
	if errors.Is(err, iofs.ErrNotExist) || errors.Is(err, worker.ErrUnsupported) {
		return nil, nil // a first install, or no service manager (Install says so)
	}
	if err != nil {
		return nil, err
	}
	if len(argv) < 2 || argv[1] != "worker" {
		return nil, errors.New("the service doesn't run `audiosilo-bridge worker`")
	}
	prev := flag.NewFlagSet("installed service", flag.ContinueOnError)
	prev.SetOutput(io.Discard)
	addWorkerFlags(prev)
	if err := prev.Parse(argv[2:]); err != nil {
		return nil, fmt.Errorf("the service's flags: %w", err)
	}
	if prev.NArg() > 0 {
		return nil, fmt.Errorf("the service's flags: unexpected %q", prev.Arg(0))
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var kept []string
	prev.Visit(func(f *flag.Flag) {
		if given[f.Name] || f.Name == "token" {
			return
		}
		_ = fs.Set(f.Name, f.Value.String()) // string flags: Set can't fail
		kept = append(kept, "--"+f.Name)
	})
	return kept, nil
}

// defaultWorkerDataDir is $AUDIOSILO_WORKER_DATA, else ~/.audiosilo-bridge-worker.
func defaultWorkerDataDir() string { return defaultDir(workerDataEnv, ".audiosilo-bridge-worker") }

// tokenIn is --token, else the data dir's token file, else $AUDIOSILO_WORKER_TOKEN.
func (f workerFlags) tokenIn(dataDir string) string {
	if t := strings.TrimSpace(*f.token); t != "" {
		return t
	}
	if t := worker.LoadToken(dataDir); t != "" {
		return t
	}
	return strings.TrimSpace(os.Getenv(workerTokenEnv))
}

// dataSet reports whether --data is known: given on the command line, or kept
// from the installed service (carryForward sets it).
func dataSet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "data" })
	return set
}
