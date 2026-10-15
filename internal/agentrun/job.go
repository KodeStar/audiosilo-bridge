package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// cancelPoll is how often a run checks for the Bridge's cancel file.
const cancelPoll = time.Second

// handle runs one claimed job to done. It never panics the runner: whatever goes
// wrong becomes exit.json's error, and the job is always marked done. job is an
// os.Root over the job dir: the runner's own reads and writes there go through
// it (openForBridge's walk, which never follows a link, is the one exception),
// so a link the Bridge or a CLI left in the job dir can't lead out of it.
func (r *Runner) handle(ctx context.Context, job *os.Root) {
	id := filepath.Base(job.Name())
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("agent runner: a job panicked", "job", id, "panic", p)
			r.finish(job, agentproto.Exit{ExitCode: -1, Error: "the agent runner failed on this job"})
		}
	}()
	touch(job, agentproto.HeartbeatFile)
	exit := r.execute(ctx, job, id)
	r.log.Info("agent runner: job finished", "job", id, "exit_code", exit.ExitCode,
		"timed_out", exit.TimedOut, "cancelled", exit.Cancelled, "duration_ms", exit.DurationMS, "error", exit.Error)
	r.finish(job, exit)
}

// execute validates the request and runs the CLI. Only the run's outcome comes
// back; stdout.txt and stderr.txt are written as it runs.
func (r *Runner) execute(ctx context.Context, job *os.Root, id string) agentproto.Exit {
	raw, err := agentproto.ReadRegular(job, agentproto.RequestFile, agentproto.MaxRequestBytes)
	if err != nil {
		return refused("read request.json: " + err.Error())
	}
	req, err := agentproto.DecodeRequest(raw)
	if err != nil {
		return refused(err.Error())
	}
	cli, ok := r.cli(req.CLI)
	if !ok {
		r.redetect.Store(true) // it may have been installed since the last look
		return unavailable("the agent container has no " + req.CLI + " CLI")
	}
	if r.credentialKind(req.CLI) == "" {
		return unavailable(req.CLI + " has no credential in the agent container")
	}
	prompt, err := agentproto.OpenRegular(job, agentproto.PromptFile)
	if err != nil {
		return refused("open prompt.txt: " + err.Error())
	}
	defer func() { _ = prompt.Close() }()
	if info, err := prompt.Stat(); err != nil || info.Size() > agentproto.MaxPromptBytes {
		return refused(fmt.Sprintf("prompt.txt is missing or over %d bytes", agentproto.MaxPromptBytes))
	}

	runDir := filepath.Join(r.opts.TmpDir, "run-"+id)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		return refused("create the run's temp dir: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(runDir) }()
	if err := os.Mkdir(filepath.Join(runDir, "home"), 0o700); err != nil {
		return refused("create the run's home: " + err.Error())
	}

	work := filepath.Join(job.Name(), agentproto.WorkDir)
	areq := agent.Request{Dir: work, Model: req.Model, Effort: req.Effort, Web: req.Web, NoTools: req.NoTools, MaxTurns: req.MaxTurns}
	var args []string
	lastMsg := filepath.Join(runDir, agentproto.LastMessageFile)
	switch req.CLI {
	case agentproto.CLIClaude:
		if err := os.Mkdir(filepath.Join(runDir, "claude"), 0o700); err != nil {
			return refused("create the run's claude config dir: " + err.Error())
		}
		args = agent.ClaudeArgs(areq)
	case agentproto.CLICodex:
		args = agent.CodexArgs(areq, lastMsg, agent.CodexSandboxOff)
		r.codexStart()
		defer r.codexEnd()
	}

	exit := r.spawn(ctx, job, spec{
		path:    cli.path,
		args:    args,
		dir:     work,
		env:     r.childEnv(req.CLI, runDir),
		stdin:   prompt,
		timeout: time.Duration(req.TimeoutSeconds) * time.Second,
	})
	if exit.Cancelled && ctx.Err() != nil {
		// The container is stopping, not the Bridge cancelling: the run says so as
		// the agent being unavailable, so the book parks and resumes on its own.
		exit.Error, exit.Unavailable = "the agent container stopped during this run", true
	}
	if req.CLI == agentproto.CLICodex {
		r.copyLastMessage(job, runDir, id)
	}
	return exit
}

// copyLastMessage copies codex's --output-last-message file from the run's temp
// dir into the job dir. codex wrote it, so it is read like any untrusted file:
// a link there (to a credential, say) is refused, never followed.
func (r *Runner) copyLastMessage(job *os.Root, runDir, id string) {
	run, err := os.OpenRoot(runDir)
	if err != nil {
		return
	}
	defer func() { _ = run.Close() }()
	b, err := agentproto.ReadRegular(run, agentproto.LastMessageFile, agentproto.MaxStdoutBytes)
	if err != nil {
		return
	}
	if err := writeNew(job, agentproto.LastMessageFile, b); err != nil {
		r.log.Warn("agent runner: write last_message.txt", "job", id, "err", err)
	}
}

// refused is exit.json for a job the runner couldn't run.
func refused(msg string) agentproto.Exit { return agentproto.Exit{ExitCode: -1, Error: msg} }

// unavailable is exit.json for a job the container can't run now (a missing CLI
// or credential): the Bridge parks the book agent_unavailable and self-resumes.
func unavailable(msg string) agentproto.Exit {
	return agentproto.Exit{ExitCode: -1, Error: msg, Unavailable: true}
}

// spec is one CLI process.
type spec struct {
	path    string
	args    []string
	dir     string
	env     []string
	stdin   io.Reader
	timeout time.Duration
}

// spawn runs one CLI in its own process group, stdout and stderr into the job
// dir (capped), touching the heartbeat every 5 s and checking for a cancel every
// second. A cancel, the timeout or the runner stopping sends SIGTERM to the
// group, SIGKILL killDelay later; whatever of the group is left when the CLI
// exits is killed too, so no helper outlives its run.
func (r *Runner) spawn(ctx context.Context, job *os.Root, s spec) agentproto.Exit {
	start := r.now()
	stdout, err := createOutput(job, agentproto.StdoutFile, agentproto.MaxStdoutBytes)
	if err != nil {
		return refused("create stdout.txt: " + err.Error())
	}
	defer stdout.close()
	stderr, err := createOutput(job, agentproto.StderrFile, agentproto.MaxStderrBytes)
	if err != nil {
		return refused("create stderr.txt: " + err.Error())
	}
	defer stderr.close()

	cmd := exec.Command(s.path, s.args...) //nolint:gosec,noctx // the runner's own resolved CLI, argv only; the group kill is managed below
	cmd.Dir = s.dir
	cmd.Env = s.env
	cmd.Stdin = s.stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The binary went (an image updated in place): the CLI is looked for
			// again, and the Bridge parks the book until it is back.
			r.redetect.Store(true)
			return unavailable("start " + filepath.Base(s.path) + ": " + err.Error())
		}
		return refused("start " + filepath.Base(s.path) + ": " + err.Error())
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var exit agentproto.Exit
	deadline := time.NewTimer(s.timeout)
	defer deadline.Stop()
	beat := time.NewTicker(agentproto.HeartbeatInterval)
	defer beat.Stop()
	poll := time.NewTicker(cancelPoll)
	defer poll.Stop()
	var kill <-chan time.Time
	stop := func() {
		if kill == nil {
			signalGroup(pid, syscall.SIGTERM)
			kill = time.After(r.killDelay)
		}
	}
	// The runner stopping is taken once: ctx.Done() stays ready once closed, and a
	// select that kept taking it would spin until the CLI exits.
	stopping := ctx.Done()
	var werr error
wait:
	for {
		select {
		case werr = <-done:
			break wait
		case <-beat.C:
			touch(job, agentproto.HeartbeatFile)
		case <-poll.C:
			if agentproto.Exists(job, agentproto.CancelFile) {
				exit.Cancelled = true
				stop()
			}
		case <-deadline.C:
			exit.TimedOut = true
			stop()
		case <-stopping:
			stopping = nil
			exit.Cancelled = true
			stop()
		case <-kill:
			signalGroup(pid, syscall.SIGKILL)
		}
	}
	signalGroup(pid, syscall.SIGKILL)
	exit.DurationMS = r.now().Sub(start).Milliseconds()
	exit.ExitCode = exitCode(cmd.ProcessState, werr)
	return exit
}

// finish writes exit.json, opens work/out/ for the Bridge, and marks the job done
// (done.tmp renamed to done, so the Bridge never sees a half-finished job).
func (r *Runner) finish(job *os.Root, exit agentproto.Exit) {
	id := filepath.Base(job.Name())
	body, err := json.Marshal(exit)
	if err == nil {
		err = writeNew(job, agentproto.ExitFile, body)
	}
	if err != nil {
		r.log.Warn("agent runner: write exit.json", "job", id, "err", err)
	}
	openForBridge(filepath.Join(job.Name(), agentproto.WorkDir, agentproto.OutDir))
	if err := markDone(job); err != nil {
		r.log.Warn("agent runner: mark done", "job", id, "err", err)
	}
}

// markDone renames done.tmp to done.
func markDone(job *os.Root) error {
	_ = job.Remove(agentproto.DoneTmpFile)
	if err := writeNew(job, agentproto.DoneTmpFile, nil); err != nil {
		return err
	}
	return job.Rename(agentproto.DoneTmpFile, agentproto.DoneFile)
}

// openForBridge makes the run's outputs readable by the Bridge's uid: regular
// files 0644, dirs 0777 (see below). It never follows a link: each entry is
// Lstat'ed and a link skipped. Entries the runner doesn't own (the Bridge's
// copies of a previous attempt's files) can't be changed and don't need to be.
//
// Dirs get 0777, not 0755: the Bridge deletes the job dir after the harvest, and
// emptying a dir the agent created needs write permission on it. The agent uid
// could write them anyway; the staging volume is shared by these two
// containers only.
func openForBridge(out string) {
	_ = filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == out {
			return nil // keep going: one unreadable entry mustn't stop the rest
		}
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		switch {
		case info.IsDir():
			_ = os.Chmod(p, 0o777) //nolint:gosec // the Bridge's uid must empty it (see above)
		case info.Mode().IsRegular():
			_ = os.Chmod(p, 0o644) //nolint:gosec // the Bridge's uid reads it
		}
		return nil
	})
}

// touch bumps a file's mtime in the job dir, creating it when missing.
func touch(job *os.Root, name string) {
	now := time.Now()
	if err := job.Chtimes(name, now, now); errors.Is(err, fs.ErrNotExist) {
		_ = writeNew(job, name, nil)
	}
}

// writeNew writes a file in the job dir that must not exist yet (a link in its
// place fails it), 0644 so the Bridge's uid can read it. O_EXCL, not a temp file
// and a rename, on purpose: the runner never replaces a file in a job dir.
func writeNew(job *os.Root, name string, data []byte) error {
	f, err := job.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Chmod(0o644); werr == nil {
		werr = cerr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// cappedFile is an output file that keeps the first limit bytes and drops the
// rest, so a runaway CLI can't fill the staging volume.
type cappedFile struct {
	f    *os.File
	left int64
}

func createOutput(job *os.Root, name string, limit int64) (*cappedFile, error) {
	f, err := job.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &cappedFile{f: f, left: limit}, nil
}

func (c *cappedFile) Write(p []byte) (int, error) {
	n := len(p)
	if c.left <= 0 {
		return n, nil
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	w, err := c.f.Write(p)
	c.left -= int64(w)
	if err != nil {
		return w, err
	}
	return n, nil
}

func (c *cappedFile) close() { _ = c.f.Close() }
