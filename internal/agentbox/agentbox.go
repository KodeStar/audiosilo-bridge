// Package agentbox is the Bridge's side of the agent container (decision 15): an
// agent.Runner that doesn't run a CLI itself but drops each invocation into the
// staging dir the agent container shares, waits for the runner there
// (internal/agentrun) to finish it, and harvests the outputs back into the
// pipeline's staged dir. The protocol (file names, JSON shapes) is
// internal/agentproto; the output parsing is internal/agent's own
// (ClaudeResult/CodexResult), so a run in the container is classified exactly like
// a local one.
//
// Everything the container writes is untrusted: files are read with
// agentproto.ReadRegular (inside an os.Root, regular files only, size-capped),
// and the harvest copies regular files only under count, depth and size limits.
package agentbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// RetryAfter is how long a book parked on the agent container waits before it
// looks again by itself: the container is usually just restarting (an image
// update, a host reboot), so it comes back without anyone pressing Retry.
const RetryAfter = 10 * time.Minute

// Timings of the Bridge side (contract section 7). The default* ones are copied
// into Runner's fields by New so a test can shorten them without a
// package-level race; sweepAge stays a constant. The container counts as up
// while runner.json is under agentproto.RunnerFreshFor old.
const (
	defaultStaleFor  = 60 * time.Second             // a claimed job's heartbeat older than this = the runner died
	defaultPoll      = time.Second                  // polling, not inotify: shfs may not deliver events
	defaultBeatCheck = agentproto.HeartbeatInterval // the runner's heartbeat can't change faster than it touches it
	defaultBeatEvery = 60 * time.Second             // Request.Heartbeat cadence, as the local runners
	defaultCancelFor = 30 * time.Second             // how long a cancel waits for done
	sweepAge         = 24 * time.Hour               // job dirs older than this are removed on New
)

// Options configures a Runner.
type Options struct {
	Dir     string // the staging dir (agent.staging_dir)
	Backend string // "claude" | "codex": which CLI the runs use
	// ClaimTimeout is agent.claim_timeout_seconds, which config defaults and
	// validates; the app always passes it.
	ClaimTimeout time.Duration
	Log          *slog.Logger
	// Credential is the vault's agent credential for a CLI (the Accounts tab,
	// decision 19): its kind (agentproto.Credential*), value and when it was set,
	// ok false for none. nil: nothing to hand over. It is handed to a run only
	// when the runner takes a hand-off and has no credential of its own.
	Credential func(cli string) (kind, value string, since time.Time, ok bool)
}

// Runner implements agent.Runner over the staging protocol.
type Runner struct {
	dir          string
	backend      string
	claimTimeout time.Duration
	log          *slog.Logger
	credential   func(cli string) (kind, value string, since time.Time, ok bool)

	now       func() time.Time
	poll      time.Duration
	staleFor  time.Duration
	beatCheck time.Duration
	beatEvery time.Duration
	cancelFor time.Duration

	// mu guards refused: the credential the provider refused last, until a run
	// goes through or the runner reports another (status.go).
	mu      sync.Mutex
	refused *refusedCredential

	// dirMissing is set while the staging dir can't be opened, so Status logs
	// its path once per outage rather than on every call.
	dirMissing atomic.Bool
}

var _ agent.Runner = (*Runner)(nil)

// New returns a Runner over opts.Dir. It creates the staging dir when missing
// (world-writable, so the agent uid can write runner.json and claim jobs) and
// ends the job dirs an earlier process left behind (see prepare).
func New(opts Options) *Runner {
	r := &Runner{
		dir:          opts.Dir,
		backend:      opts.Backend,
		claimTimeout: opts.ClaimTimeout,
		log:          opts.Log,
		credential:   opts.Credential,
		now:          time.Now,
		poll:         defaultPoll,
		staleFor:     defaultStaleFor,
		beatCheck:    defaultBeatCheck,
		beatEvery:    defaultBeatEvery,
		cancelFor:    defaultCancelFor,
	}
	if r.claimTimeout <= 0 {
		// Only a caller that skipped config gets here: the same 2 minutes as
		// config's default, rather than a job that waits for a claim forever.
		r.claimTimeout = 2 * time.Minute
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	r.prepare()
	return r
}

// prepare makes the staging dir usable by both containers and deals with the job
// dirs an earlier process left: one older than a day is removed; any other is a
// run nothing waits for any more (one Bridge per staging dir, and this one has
// just started), so it is withdrawn, or stopped if the runner has it. Best
// effort: a staging dir the Bridge doesn't own (a volume root created by Docker)
// keeps its mode, and Status then explains a runner that can't write.
func (r *Runner) prepare() {
	if r.dir == "" {
		return
	}
	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		r.log.Warn("agent container: create the staging dir", "dir", r.dir, "err", err)
		return
	}
	// Not sticky, unlike a job dir: the Bridge image's entrypoint may chown this
	// volume to PUID:PGID on a first start, and the runner must still replace a
	// runner.json that changed owner that way.
	if err := os.Chmod(r.dir, 0o777); err != nil { //nolint:gosec // shared with the agent uid by design (decision 15)
		r.log.Debug("agent container: staging dir mode left as is", "dir", r.dir, "err", err)
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !agentproto.ValidJobID(e.Name()) {
			continue
		}
		job := filepath.Join(r.dir, e.Name())
		if info, err := e.Info(); err == nil && r.now().Sub(info.ModTime()) >= sweepAge {
			r.remove(job)
			continue
		}
		r.reclaim(job)
	}
}

// reclaim ends a job dir a previous Bridge process left (it crashed or was
// killed, so its Run never cancelled or removed it): a finished or unclaimed one
// is removed at once (taking the claim first, so a runner can't start it
// meanwhile); one the runner is running gets a cancel, so the CLI doesn't run on,
// paid and holding a slot, for output nothing will read. It finishes the job and
// the dir goes at a later start.
func (r *Runner) reclaim(job string) {
	root, err := os.OpenRoot(job)
	if err != nil {
		return
	}
	done := agentproto.Exists(root, agentproto.DoneFile) || r.unclaimable(root)
	if !done {
		if err := root.WriteFile(agentproto.CancelFile, nil, 0o644); err != nil {
			r.log.Warn("agent container: cancel an earlier process's run", "dir", job, "err", err)
		}
	}
	_ = root.Close()
	if done {
		r.remove(job)
	}
}

// ID is the backend the runs use, so telemetry and model routing see the CLI.
func (r *Runner) ID() string { return r.backend }

// SupportsWeb is true: both CLIs can search the web, and the agent container
// has its own internet-only network.
func (r *Runner) SupportsWeb() bool { return true }

// EnforcesNoTools reports what the fronted CLI does: claude removes its tool
// catalogue for a NoTools run, codex can't (its read-only sandbox still reads).
func (r *Runner) EnforcesNoTools() bool { return r.backend == agent.IDClaude }

// Detect maps Status onto the engine's Availability. RetryAfter is always set:
// whatever makes the container unavailable (down, restarting, too busy to claim
// a job in time) is expected to pass by itself, so a book parked on it looks
// again after RetryAfter, with Status's Detail as the reason. A refused credential
// counts as available here: the engine still runs (each run that's refused parks
// its book agent_credential_refused, and the first one that works clears it).
func (r *Runner) Detect(context.Context) agent.Availability {
	st := r.Status()
	av := agent.Availability{Backend: r.backend, Available: st.Available || st.Refused, Detail: st.Detail,
		Path: "agent container", RetryAfter: RetryAfter}
	if c, ok := st.CLIs[r.backend]; ok {
		av.Version = c.Version
	}
	return av
}

// Run performs one invocation in the agent container: stage the job, wait for the
// runner, read the outcome, harvest out/. See the package comment and contract
// section 7. req.Process is never called: a pid in another container means
// nothing here.
//
// The Bridge's own trouble with the staging dir (it can't write the job there, or
// read back what the runner left) is the agent being unavailable, never a failed
// stage: the book parks and looks again by itself.
func (r *Runner) Run(ctx context.Context, req agent.Request) (agent.Result, error) {
	st := r.Status()
	if !st.Available && !st.Refused {
		return agent.Result{}, &agent.NotAvailableError{Backend: r.backend, Detail: st.Detail}
	}
	if req.Dir == "" {
		return agent.Result{}, errors.New("agent container: the request has no staged dir")
	}
	id, err := agentproto.NewJobID()
	if err != nil {
		return agent.Result{}, fmt.Errorf("agent container: job id: %w", err)
	}
	job := filepath.Join(r.dir, id)
	defer r.remove(job)
	timeout := requestTimeout(req.Timeout)
	var handed *agentproto.Credential
	if st.Handoff {
		if kind, value, _, ok := r.handoffCredential(); ok {
			handed = &agentproto.Credential{CLI: r.backend, Kind: kind, Value: value}
		}
	}
	root, err := stage(job, req, r.backend, timeout, handed)
	if err != nil {
		return agent.Result{}, r.stagingFailed("write the job into", err)
	}
	defer func() { _ = root.Close() }()

	done, waitErr := r.wait(ctx, root, req.Heartbeat)
	if !done {
		return agent.Result{}, waitErr
	}
	res, runErr := r.outcome(root, req.Model, time.Duration(timeout)*time.Second)
	r.noteCredential(st, runErr)
	if waitErr != nil {
		// Cancelled: the runner stopped the CLI and finished the job, so the usage
		// it reported is still worth recording; the outputs aren't harvested.
		return res, waitErr
	}
	var unavailable *agent.NotAvailableError
	if errors.As(runErr, &unavailable) {
		return res, runErr
	}
	// Harvest after any finished run, failed ones too: a validation retry patches
	// the previous attempt's out/, exactly as a local run leaves it in place.
	if herr := harvest(root, req.Dir); herr != nil {
		if runErr != nil {
			r.log.Warn("agent container: harvest after a failed run", "err", herr)
			return res, runErr
		}
		var limit *harvestLimitError
		if !errors.As(herr, &limit) {
			return res, r.stagingFailed("read the outputs from", herr)
		}
		return res, herr
	}
	return res, runErr
}

// noteCredential remembers a credential the provider refused, and forgets one
// once a run shows it works (an answer, or a rate limit: the provider took the
// credential then). st is the runner's report when the run was handed over.
func (r *Runner) noteCredential(st Status, runErr error) {
	var na *agent.NotAvailableError
	var rl *agent.RateLimitError
	switch {
	case errors.As(runErr, &na) && na.CredentialRefused:
		r.noteRefusal(st.CLIs[r.backend], na.Detail)
	case runErr == nil || errors.As(runErr, &rl):
		r.clearRefusal()
	}
}

// stagingFailed is the Bridge failing to use the staging dir (doing what) as the
// agent being unavailable: a full or read-only volume, a dir the Bridge's user
// can't write. The error is worded without the Bridge's own paths.
func (r *Runner) stagingFailed(what string, err error) error {
	r.log.Warn("agent container: the staging dir failed", "what", what, "err", err)
	return &agent.NotAvailableError{Backend: r.backend,
		Detail: "the Bridge can't " + what + " the staging dir it shares with the agent container (" + ioProblem(err) + ")"}
}

// ioProblem words a file error without its path.
func ioProblem(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Op + ": " + le.Err.Error()
	}
	return err.Error()
}

// requestTimeout is the request's timeout in whole seconds, rounded up; no
// timeout becomes the protocol's ceiling, since a run in the container is always
// bounded.
func requestTimeout(d time.Duration) int {
	if d <= 0 {
		return agentproto.MaxTimeoutSeconds
	}
	s := int((d + time.Second - 1) / time.Second)
	return min(max(s, 1), agentproto.MaxTimeoutSeconds)
}

// wait polls the job until done. It reports done=false with the reason when the
// job never finished: the claim timeout (a NotAvailableError, so the pipeline
// parks agent_unavailable and self-resumes), a stale heartbeat, or a cancel the
// runner didn't finish in time. A cancel the runner did finish is done=true with
// ctx's error.
//
// done is looked for every poll, so a finished run is picked up at once; the
// runner's heartbeat only every beatCheck, as it can't change faster than the
// runner touches it.
func (r *Runner) wait(ctx context.Context, root *os.Root, beat func(time.Duration)) (bool, error) {
	start := r.now()
	var claimedAt, lastBeat, lastCheck time.Time
	tick := time.NewTicker(r.poll)
	defer tick.Stop()
	for {
		if agentproto.Exists(root, agentproto.DoneFile) {
			return true, nil
		}
		now := r.now()
		if claimedAt.IsZero() {
			if agentproto.Exists(root, agentproto.ClaimDir) {
				claimedAt, lastBeat, lastCheck = now, now, now
			} else if now.Sub(start) >= r.claimTimeout {
				if r.unclaimable(root) {
					return false, &agent.NotAvailableError{Backend: r.backend,
						Detail: fmt.Sprintf("the agent container didn't pick the job up within %s", r.claimTimeout)}
				}
				claimedAt, lastBeat, lastCheck = now, now, now // the runner claimed it just now
			}
		}
		if !claimedAt.IsZero() {
			if now.Sub(lastCheck) >= r.beatCheck {
				lastCheck = now
				if hb := r.heartbeatAt(root, claimedAt); now.Sub(hb) > r.staleFor {
					_ = root.WriteFile(agentproto.CancelFile, nil, 0o644)
					return false, fmt.Errorf("agent container: the runner stopped answering (no heartbeat for %s)", now.Sub(hb).Round(time.Second))
				}
			}
			if beat != nil && now.Sub(lastBeat) >= r.beatEvery {
				lastBeat = now
				beat(now.Sub(claimedAt))
			}
		}
		select {
		case <-ctx.Done():
			return r.cancel(root, claimedAt.IsZero(), ctx.Err())
		case <-tick.C:
		}
	}
}

// unclaimable takes the job's claim itself, so a runner can't pick it up after
// the Bridge gave up on it: mkdir is atomic, so exactly one side wins. false
// means the runner won the race and the job is running after all.
func (r *Runner) unclaimable(root *os.Root) bool {
	err := root.Mkdir(agentproto.ClaimDir, 0o700)
	return err == nil || !errors.Is(err, fs.ErrExist)
}

// cancel stops a job on a ctx cancel: an unclaimed one is taken back (it never
// runs), a running one gets a cancel file and up to cancelFor to finish.
func (r *Runner) cancel(root *os.Root, unclaimed bool, cause error) (bool, error) {
	if unclaimed && r.unclaimable(root) {
		return false, cause
	}
	if err := root.WriteFile(agentproto.CancelFile, nil, 0o644); err != nil {
		r.log.Warn("agent container: write cancel", "err", err)
	}
	deadline := r.now().Add(r.cancelFor)
	for r.now().Before(deadline) {
		if agentproto.Exists(root, agentproto.DoneFile) {
			return true, cause
		}
		time.Sleep(r.poll)
	}
	return false, cause
}

// heartbeatAt is the job's last sign of life: the heartbeat file's mtime, else the
// claim's, else when the Bridge saw the claim.
func (r *Runner) heartbeatAt(root *os.Root, claimedAt time.Time) time.Time {
	latest := claimedAt
	for _, name := range []string{agentproto.HeartbeatFile, agentproto.ClaimDir} {
		if info, err := root.Lstat(name); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

// outcome reads exit.json and the CLI's output and interprets them with the same
// code the local runners use. An unavailable exit still reports the usage the
// CLI's output shows, so a paid run the container cut short is counted.
func (r *Runner) outcome(root *os.Root, model string, timeout time.Duration) (agent.Result, error) {
	raw, err := agentproto.ReadRegular(root, agentproto.ExitFile, agentproto.MaxExitBytes)
	if err != nil {
		return agent.Result{}, r.stagingFailed("read the run's exit.json from", err)
	}
	var ex agentproto.Exit
	if err := json.Unmarshal(raw, &ex); err != nil {
		return agent.Result{}, fmt.Errorf("agent container: parse exit.json: %w", err)
	}
	if ex.Error != "" {
		if ex.Unavailable {
			// The container stopped or restarted mid-run (or couldn't start the CLI):
			// a run that got going may have spent tokens, so whatever usage its
			// stdout reports rides on the Result beside the error and the budgets
			// count it. Nothing else of a cut-off run is trusted.
			res, _ := r.parse(root, model, agent.CLIOutcome{ExitErr: errors.New(ex.Error), Timeout: timeout})
			return agent.Result{Usage: res.Usage}, &agent.NotAvailableError{Backend: r.backend, Detail: ex.Error}
		}
		return agent.Result{}, fmt.Errorf("agent container refused the job: %s", ex.Error)
	}
	out := agent.CLIOutcome{TimedOut: ex.TimedOut, Timeout: timeout}
	switch {
	case ex.TimedOut:
	case ex.Cancelled:
		out.ExitErr = errors.New("cancelled")
	case ex.ExitCode != 0:
		out.ExitErr = exitStatus(ex.ExitCode)
	}
	return r.parse(root, model, out)
}

// parse reads the CLI's output files into out and interprets them with the
// backend's shared result mapping (ClaudeResult/CodexResult). A missing or
// unreadable file reads as empty.
func (r *Runner) parse(root *os.Root, model string, out agent.CLIOutcome) (agent.Result, error) {
	stdout, _ := agentproto.ReadRegular(root, agentproto.StdoutFile, agentproto.MaxStdoutBytes)
	stderr, _ := agentproto.ReadRegular(root, agentproto.StderrFile, agentproto.MaxStderrBytes)
	out.Stdout, out.Stderr = string(stdout), string(stderr)
	if r.backend == agent.IDCodex {
		last, _ := agentproto.ReadRegular(root, agentproto.LastMessageFile, agentproto.MaxStdoutBytes)
		out.LastMessage = string(last)
		return agent.CodexResult(model, out)
	}
	return agent.ClaudeResult(model, out)
}

// exitStatus is a nonzero exit code from the container, worded like the local
// runners' *exec.ExitError.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// remove deletes a job dir, best effort.
func (r *Runner) remove(job string) {
	if err := os.RemoveAll(job); err != nil {
		r.log.Warn("agent container: remove a job dir", "dir", job, "err", err)
	}
}
