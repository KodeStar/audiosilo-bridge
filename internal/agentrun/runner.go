// Package agentrun is the agent container's side of the staging protocol
// (decision 15; internal/agentproto): `audiosilo-bridge agent-runner`. It is the
// only thing that runs in the agent container. It sees the staging dir (shared
// with the Bridge) and its own home (/agent: the CLIs' credentials and codex's
// login), never a library or the Bridge's data, and runs each job the Bridge
// drops into the staging dir: claim, run the CLI with an argv it builds itself
// and an environment from an allow-list, heartbeat, honour a cancel, write the
// outcome, open work/out/ for the Bridge to read, mark done.
//
// The runner never logs a credential: it reports only which kind each CLI has.
package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// MaxSlots bounds --slots: each slot is a concurrent CLI run.
const MaxSlots = 8

// scanInterval is how often the staging dir is listed for new jobs.
const scanInterval = time.Second

// orphanAfter is how old a claimed, unfinished job's heartbeat must be before a
// starting runner treats it as a run an earlier runner left behind.
const orphanAfter = 30 * time.Second

// Options configures the runner.
type Options struct {
	Staging string // the staging dir (/staging)
	Home    string // the agent home (/agent): secrets/, codex/
	Slots   int    // concurrent jobs; 0 = 1
	Version string // reported in runner.json
	Log     *slog.Logger

	// Getenv reads the runner's own environment (os.LookupEnv when nil); a test
	// injects one so it never touches the process environment.
	Getenv func(string) (string, bool)
	// TmpDir holds the per-run dirs (/tmp when empty).
	TmpDir string
	// ClaudeBin and CodexBin are the CLIs (looked up on PATH as claude and codex
	// when empty).
	ClaudeBin, CodexBin string
}

// Runner runs the jobs in one staging dir.
type Runner struct {
	opts  Options
	log   *slog.Logger
	slots chan struct{}

	// clis holds what detect found: the resolved binary and its --version.
	clis map[string]cliInfo

	// codexMu guards codexActive, the codex runs in progress: CODEX_HOME is
	// shared (the login lives there), so it's wiped only when no run uses it.
	codexMu     sync.Mutex
	codexActive int

	killDelay time.Duration // TERM to KILL on a cancel or a timeout
	scanEvery time.Duration
	now       func() time.Time
}

type cliInfo struct{ path, version string }

// New checks the options and returns a Runner. It doesn't touch the staging dir
// until Run.
func New(opts Options) (*Runner, error) {
	if opts.Staging == "" || opts.Home == "" {
		return nil, errors.New("agent runner: --staging and --home are required")
	}
	if opts.Slots == 0 {
		opts.Slots = 1
	}
	if opts.Slots < 1 || opts.Slots > MaxSlots {
		return nil, fmt.Errorf("agent runner: --slots must be between 1 and %d", MaxSlots)
	}
	if opts.Getenv == nil {
		opts.Getenv = os.LookupEnv
	}
	if opts.TmpDir == "" {
		opts.TmpDir = os.TempDir()
	}
	if opts.ClaudeBin == "" {
		opts.ClaudeBin = agentproto.CLIClaude
	}
	if opts.CodexBin == "" {
		opts.CodexBin = agentproto.CLICodex
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		opts:      opts,
		log:       log,
		slots:     make(chan struct{}, opts.Slots),
		clis:      map[string]cliInfo{},
		killDelay: 10 * time.Second,
		scanEvery: scanInterval,
		now:       time.Now,
	}, nil
}

// Run serves the staging dir until ctx ends, then stops the runs in progress
// (each is cancelled and finished, so the Bridge gets an answer) and returns.
func (r *Runner) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Join(r.opts.Home, "codex"), 0o700); err != nil {
		r.log.Warn("agent runner: create the codex home", "err", err)
	}
	r.detect(ctx)
	r.recoverOrphans()
	r.writeStatus()
	r.log.Info("agent runner: started", "staging", r.opts.Staging, "slots", r.opts.Slots)

	status := time.NewTicker(agentproto.RunnerInterval)
	defer status.Stop()
	scan := time.NewTicker(r.scanEvery)
	defer scan.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("agent runner: stopping")
			return nil
		case <-status.C:
			r.writeStatus()
		case <-scan.C:
			r.scan(ctx, &wg)
		}
	}
}

// detect resolves each CLI and asks its version, once: the binaries are pinned
// in the image.
func (r *Runner) detect(ctx context.Context) {
	for name, bin := range map[string]string{agentproto.CLIClaude: r.opts.ClaudeBin, agentproto.CLICodex: r.opts.CodexBin} {
		path, err := r.lookPath(bin)
		if err != nil {
			r.log.Warn("agent runner: CLI not found", "cli", name, "err", err)
			continue
		}
		ver, err := r.version(ctx, path)
		if err != nil {
			r.log.Warn("agent runner: CLI --version failed", "cli", name, "err", err)
			continue
		}
		r.clis[name] = cliInfo{path: path, version: ver}
		r.log.Info("agent runner: CLI found", "cli", name, "version", ver)
	}
}

// lookPath resolves bin on the runner's PATH (an absolute or relative path is
// taken as is).
func (r *Runner) lookPath(bin string) (string, error) {
	if strings.ContainsRune(bin, filepath.Separator) {
		return exec.LookPath(bin)
	}
	for _, dir := range filepath.SplitList(r.path()) {
		if p, err := exec.LookPath(filepath.Join(dir, bin)); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH", bin)
}

// version runs `<bin> --version` with the allow-listed environment and no
// credential, in a throwaway home.
func (r *Runner) version(ctx context.Context, path string) (string, error) {
	dir, err := os.MkdirTemp(r.opts.TmpDir, "version-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = r.baseEnv(dir)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(line), nil
}

// status is runner.json now: the CLIs found and the kind of credential each has.
func (r *Runner) status() agentproto.Runner {
	st := agentproto.Runner{
		Protocol:    agentproto.Protocol,
		Version:     r.opts.Version,
		HeartbeatAt: r.now().UTC(),
		Slots:       r.opts.Slots,
		CLIs:        map[string]agentproto.CLI{},
	}
	for _, name := range []string{agentproto.CLIClaude, agentproto.CLICodex} {
		st.CLIs[name] = agentproto.CLI{Version: r.clis[name].version, Credential: r.credentialKind(name)}
	}
	return st
}

// writeStatus rewrites runner.json (a temp file renamed into place, 0644 so the
// Bridge's uid can read it).
func (r *Runner) writeStatus() {
	body, err := json.Marshal(r.status())
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(r.opts.Staging, "."+agentproto.RunnerFile+".tmp-*")
	if err != nil {
		r.log.Warn("agent runner: can't write runner.json; can the agent uid write the staging dir?", "dir", r.opts.Staging, "err", err)
		return
	}
	_, werr := tmp.Write(body)
	if cerr := tmp.Chmod(0o644); werr == nil {
		werr = cerr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), filepath.Join(r.opts.Staging, agentproto.RunnerFile))
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		r.log.Warn("agent runner: write runner.json", "err", werr)
	}
}

// scan claims ready jobs while a slot is free, oldest request first.
func (r *Runner) scan(ctx context.Context, wg *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}
	for _, job := range r.readyJobs() {
		select {
		case r.slots <- struct{}{}:
		default:
			return // every slot is busy
		}
		if err := os.Mkdir(filepath.Join(job, agentproto.ClaimDir), 0o700); err != nil {
			<-r.slots // taken by another runner, or withdrawn by the Bridge
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-r.slots }()
			r.handle(ctx, job)
		}()
	}
}

// readyJobs lists the job dirs with a request and no claim or done, oldest
// request first.
func (r *Runner) readyJobs() []string {
	entries, err := os.ReadDir(r.opts.Staging)
	if err != nil {
		r.log.Warn("agent runner: list the staging dir", "err", err)
		return nil
	}
	type ready struct {
		dir string
		at  time.Time
	}
	var jobs []ready
	for _, e := range entries {
		if !agentproto.ValidJobID(e.Name()) || e.Type()&fs.ModeType != fs.ModeDir {
			continue
		}
		dir := filepath.Join(r.opts.Staging, e.Name())
		if present(dir, agentproto.DoneFile) || present(dir, agentproto.ClaimDir) {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, agentproto.RequestFile))
		if err != nil {
			continue
		}
		jobs = append(jobs, ready{dir, info.ModTime()})
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].at.Before(jobs[j].at) })
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.dir
	}
	return out
}

// recoverOrphans finishes the jobs an earlier runner claimed and never finished
// (the container restarted mid-run), so the Bridge gets an answer at once
// instead of waiting for the heartbeat to go stale.
func (r *Runner) recoverOrphans() {
	entries, err := os.ReadDir(r.opts.Staging)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !agentproto.ValidJobID(e.Name()) || e.Type()&fs.ModeType != fs.ModeDir {
			continue
		}
		dir := filepath.Join(r.opts.Staging, e.Name())
		if present(dir, agentproto.DoneFile) || !present(dir, agentproto.RequestFile) {
			continue
		}
		claim, err := os.Lstat(filepath.Join(dir, agentproto.ClaimDir))
		if err != nil {
			continue
		}
		last := claim.ModTime()
		if hb, err := os.Lstat(filepath.Join(dir, agentproto.HeartbeatFile)); err == nil && hb.ModTime().After(last) {
			last = hb.ModTime()
		}
		if r.now().Sub(last) < orphanAfter {
			continue
		}
		r.log.Warn("agent runner: finishing a run an earlier runner left", "job", e.Name())
		r.finish(dir, agentproto.Exit{ExitCode: -1, Error: "the agent container restarted during this run", Unavailable: true})
	}
}

// present reports whether name exists in dir (nothing followed).
func present(dir, name string) bool {
	_, err := os.Lstat(filepath.Join(dir, name))
	return err == nil
}
