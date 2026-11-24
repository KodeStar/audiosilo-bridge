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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
	"github.com/kodestar/audiosilo-bridge/internal/timeutil"
)

// RetryAfter is how long a book parked on the agent container waits before it
// looks again by itself: the container is usually just restarting (an image
// update, a host reboot), so it comes back without anyone pressing Retry.
const RetryAfter = 10 * time.Minute

// Timings of the Bridge side (contract section 7). The default* ones are copied
// into Runner's fields by New so a test can shorten them without a
// package-level race; sweepAge stays a constant. The container counts as up
// while runner.json's heartbeat last changed under agentproto.RunnerFreshFor
// ago on the Bridge's clock (status.go's heartbeatAge).
const (
	defaultStaleFor  = 60 * time.Second             // a claimed job's heartbeat unchanged this long = the runner died
	defaultPoll      = time.Second                  // polling, not inotify: shfs may not deliver events
	defaultBeatCheck = agentproto.HeartbeatInterval // the runner's heartbeat can't change faster than it touches it
	defaultBeatEvery = 60 * time.Second             // Request.Heartbeat cadence, as the local runners
	defaultCancelFor = 30 * time.Second             // how long a cancel waits for done
	defaultTakeFor   = 2 * time.Second              // how long a runner that claimed a job may take to take its credential
	sweepAge         = 24 * time.Hour               // job dirs older than this are removed on New
)

// Options configures a Runner.
type Options struct {
	// Dir is the staging dir the runs go to: the agent container's
	// (agent.staging_dir), or for the openai-compatible backend the
	// endpoint's own agent service's (agent.endpoint_staging_dir; "" for
	// none, which Status reports as ProblemEndpointService).
	Dir string
	// AgentDir, on the openai-compatible backend, is the agent container's
	// staging dir: the endpoint's service must share no dir and no uid with
	// it (Status).
	AgentDir string
	// Backend is "claude" or "codex", the CLI the runs use, or
	// "openai-compatible": codex against the endpoint Endpoint names (bridge#84).
	Backend string
	// Endpoint is the OpenAI-compatible endpoint's base URL now ("" for none
	// set), read for every status and run so the page's change applies at
	// once. Only the openai-compatible backend reads it.
	Endpoint func() string
	// ClaimTimeout is agent.claim_timeout_seconds, which config defaults and
	// validates; the app always passes it.
	ClaimTimeout time.Duration
	Log          *slog.Logger
	// Credential is the vault's agent credential for a CLI (the Accounts tab,
	// decision 19): its kind (agentproto.Credential*), value and when it was set,
	// ok false for none. nil: nothing to hand over. It is handed to a run only
	// when the runner takes a hand-off and has no credential of its own.
	// endpoint is the address an openai-compatible run goes to ("" for the
	// others): the key comes back only when it was entered for exactly that
	// address, so a run never carries another address's key.
	Credential func(cli, endpoint string) (kind, value string, since time.Time, ok bool)
	// RequireGroup (agent.require_group) keeps a handed credential to the
	// Bridge's group in every case: a runner outside the group, a staging
	// volume that refuses the chown and a runner denied the group's file each
	// make the agent unavailable rather than hand it to every uid.
	RequireGroup bool
}

// Runner implements agent.Runner over the staging protocol.
type Runner struct {
	dir      string
	agentDir string
	backend  string
	// cli is the CLI the runs use: the backend's own, codex for an endpoint.
	cli          string
	endpoint     func() string
	claimTimeout time.Duration
	log          *slog.Logger
	credential   func(cli, endpoint string) (kind, value string, since time.Time, ok bool)
	requireGroup bool

	now       func() time.Time
	poll      time.Duration
	staleFor  time.Duration
	beatCheck time.Duration
	beatEvery time.Duration
	cancelFor time.Duration
	takeFor   time.Duration

	// mu guards refused: the credential the provider refused last, until a run
	// goes through or the runner reports another (status.go).
	mu      sync.Mutex
	refused *refusedCredential

	// beats are the runner.json heartbeats read, by staging dir, and when each
	// last changed on the Bridge's clock (guarded by beatMu; status.go's
	// heartbeatAge).
	beatMu sync.Mutex
	beats  map[string]beatSeen

	// dirMissing is set while the staging dir can't be opened, so Status logs
	// its path once per outage rather than on every call.
	dirMissing atomic.Bool
	// gid is the Bridge's group (its effective one: the group its new files
	// get and the one it may always chown to), the one a handed credential
	// goes to alone when the runner is in it.
	gid int
	// warnedGroup is set once the log has said a handed credential goes to
	// every uid because the runner isn't in the Bridge's group, warnedVolume
	// because the staging volume refused the group: each once per process.
	warnedGroup, warnedVolume atomic.Bool
	// volumeRefused: the last job staged for the group got the every-uid
	// modes because the staging volume refused the group (Status.HandoffMode).
	volumeRefused atomic.Bool
	// groupDenied is set once the runner, in the Bridge's group by its ids,
	// couldn't take a credential handed to that group: later runs hand it to
	// every uid (Run) until a group probe ends it (probe.go).
	groupDenied atomic.Bool
	// groupBlocked, with agent.require_group on, is why the credential can't
	// be kept to the Bridge's group (the runner was denied the group's file,
	// or the volume refused the chown), learned by a run and kept until a
	// group probe ends it (probe.go): Status reports the agent unavailable
	// with it.
	groupBlocked atomic.Pointer[string]
	// groupFailed is why the last credential staged for the Bridge's group
	// couldn't be given to it, when that wasn't the volume refusing (an I/O
	// error, an errno groupRefused doesn't list: stage's groupFailedError).
	// Kept until the group can be given again (a group probe, probe.go, or a
	// job staged meanwhile): Status reports the agent unavailable with it, so
	// the page and the checklist don't call ready an agent whose every job
	// fails at the hand-off.
	groupFailed atomic.Pointer[string]
	// probeAfter (unix nanoseconds) is when the next group probe may run,
	// and probing is set while one runs (probe.go).
	probeAfter atomic.Int64
	probing    atomic.Bool
	// probes counts the probes in flight, for Wait.
	probes sync.WaitGroup
	// spoiled is who spoiled the last group probe (probe.go), nil once a
	// probe gets the runner's own answer: Status says so.
	spoiled atomic.Pointer[probeSpoil]
	// uid is the Bridge's own (effective) uid, and runnerUID the runner's as
	// the staging volume shows it, pinned by the first live runner.json
	// (noUID until then): only the runner's files may move a handed
	// credential off the group (trust.go). warnedForeign is set once the log
	// has said one from another uid (or a runner.json with a second link)
	// didn't.
	uid           int
	runnerUID     atomic.Int64
	warnedForeign atomic.Bool
}

var _ agent.Runner = (*Runner)(nil)

// New returns a Runner over opts.Dir. It creates the staging dir when missing
// (world-writable, so the agent uid can write runner.json and claim jobs) and
// ends the job dirs an earlier process left behind (see prepare).
func New(opts Options) *Runner {
	r := &Runner{
		dir:          opts.Dir,
		agentDir:     opts.AgentDir,
		backend:      opts.Backend,
		cli:          opts.Backend,
		endpoint:     opts.Endpoint,
		claimTimeout: opts.ClaimTimeout,
		log:          opts.Log,
		credential:   opts.Credential,
		requireGroup: opts.RequireGroup,
		gid:          os.Getegid(),
		uid:          os.Geteuid(),
		now:          time.Now,
		poll:         defaultPoll,
		staleFor:     defaultStaleFor,
		beatCheck:    defaultBeatCheck,
		beatEvery:    defaultBeatEvery,
		cancelFor:    defaultCancelFor,
		takeFor:      defaultTakeFor,
		beats:        map[string]beatSeen{},
	}
	if r.claimTimeout <= 0 {
		// Only a caller that skipped config gets here: the same 2 minutes as
		// config's default, rather than a job that waits for a claim forever.
		r.claimTimeout = 2 * time.Minute
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	if r.backend == agent.IDOpenAICompatible {
		r.cli = agentproto.CLICodex
	}
	if r.endpoint == nil {
		r.endpoint = func() string { return "" }
	}
	r.runnerUID.Store(noUID)
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

// Dir is the staging dir the runs go to ("" for an endpoint's service not set).
func (r *Runner) Dir() string { return r.dir }

// ID is the backend the runs use, so telemetry, model routing and pricing see
// it (openai-compatible for an endpoint, though codex runs it).
func (r *Runner) ID() string { return r.backend }

// SupportsWeb is true for both CLIs, which can search the web from the agent
// container's own network; false for an endpoint, as web search is a tool
// OpenAI hosts (CodexArgs turns it off there).
func (r *Runner) SupportsWeb() bool { return r.backend != agent.IDOpenAICompatible }

// EnforcesNoTools reports what the fronted CLI does: claude removes its tool
// catalogue for a NoTools run, codex can't (its read-only sandbox still reads).
func (r *Runner) EnforcesNoTools() bool { return r.cli == agent.IDClaude }

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
	// addr is the endpoint address this run goes to, read once: its key and
	// the refusal it may earn both belong to it, whatever the address is by now.
	var addr string
	if r.backend == agent.IDOpenAICompatible {
		base := r.endpoint()
		if base == "" {
			return agent.Result{}, &agent.NotAvailableError{Backend: r.backend, Detail: noEndpointDetail}
		}
		if req.Model == "" {
			// The page's models apply at once and a backend at the next start, so
			// with another backend saved they may be cleared under this one; the
			// runner would refuse the request (an endpoint has no default model).
			stage := cmp.Or(req.Stage, "this stage")
			return agent.Result{}, &agent.NotAvailableError{Backend: r.backend,
				Detail: "the OpenAI-compatible endpoint has no model set for " + stage + ": set one in Settings > Agent on the Bridge's page"}
		}
		req.Endpoint = &agent.Endpoint{BaseURL: base}
		addr = base
	}
	var handed *agentproto.Credential
	if st.Handoff {
		if kind, value, _, ok := r.handoffCredential(addr); ok {
			handed = &agentproto.Credential{CLI: r.cli, Kind: kind, Value: value}
		}
	}
	group := noGroup
	if st.ShareGroup {
		group = r.gid
	}
	// Judged by the runner's report this run goes on (st), not by groupDenied
	// as it is now: a probe may have ended a denial since Status read it.
	if handed != nil && !st.ShareGroup && !st.groupListed && !r.warnedGroup.Swap(true) {
		r.log.Warn("agent container: the runner isn't in the Bridge's group, so the handed credential is readable by every uid "+
			"until the runner takes it; "+remedyGroupAdd+" (an older one reports no groups)", "bridge_gid", r.gid)
	}
	res, err := r.runJob(ctx, req, st, handed, group)
	var denied *credentialDeniedError
	if errors.As(err, &denied) {
		// The runner reports the Bridge's gid among its groups, yet its uid may
		// not take the file that group was given: the two containers map the
		// ids on the staging volume differently (a user namespace, as rootless
		// Podman sets up). The CLI never started, so nothing of the job ran: it
		// is staged again (its first dir already gone) as it was before there
		// was a group, and every later run goes that way until a group probe
		// finds the runner takes the group's file (probe.go).
		if r.requireGroup {
			return res, r.blockGroup("the agent container couldn't take the credential kept to the Bridge's group: "+
				remedyMapping+lookAgain+noRestart, denied.detail)
		}
		if !r.groupDenied.Swap(true) {
			r.deferProbe()
			r.log.Warn("agent container: the runner couldn't take the credential handed to the Bridge's group alone, so from "+
				"now on the handed credential is readable by every uid until the runner takes it; "+remedyMapping+
				" to keep it to that group"+lookAgain+noRestart, "bridge_gid", r.gid, "detail", denied.detail)
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res, err = r.runJob(ctx, req, st, handed, noGroup)
	}
	return res, err
}

// runJob is one job in the container: stage it (handed, when set, goes to
// group as stage hands it), wait for the runner, read the outcome, harvest
// out/. The job dir is gone when it returns. st is the runner's report the run
// was handed over on. A credentialDeniedError comes back only for a credential
// the group alone was given that the runner claimed and left in place
// (deniedHere): any other denial is the job refused.
func (r *Runner) runJob(ctx context.Context, req agent.Request, st Status, handed *agentproto.Credential, group int) (agent.Result, error) {
	timeout := requestTimeout(req.Timeout)
	id, err := agentproto.NewJobID()
	if err != nil {
		return agent.Result{}, fmt.Errorf("agent container: job id: %w", err)
	}
	job := filepath.Join(r.dir, id)
	defer r.remove(job)
	root, groupErr, err := stage(job, req, r.cli, timeout, handed, group, r.requireGroup)
	var refused *groupRefusedError
	if errors.As(err, &refused) {
		return agent.Result{}, r.blockGroup("the staging volume refused to give the credential to the Bridge's group ("+
			ioProblem(refused.err)+"): use "+remedyVolume+recreateBoth+lookAgain+volumeInPlace, refused.err.Error())
	}
	var failed *groupFailedError
	if errors.As(err, &failed) {
		return agent.Result{}, r.failGroup(failed.err)
	}
	if err == nil && handed != nil && group != noGroup {
		r.volumeRefused.Store(groupErr != nil)
		r.groupFailed.Store(nil)
	}
	if groupErr != nil && !r.warnedVolume.Swap(true) {
		r.log.Warn("agent container: the staging volume refused to hand the credential to the Bridge's group alone, so it is "+
			"readable by every uid until the runner takes it; "+remedyVolume+" keeps it to that group", "bridge_gid", r.gid,
			"err", groupErr)
	}
	if err != nil {
		return agent.Result{}, r.stagingFailed("write the job into", err)
	}
	defer func() { _ = root.Close() }()

	done, waitErr := r.wait(ctx, root, req.Heartbeat)
	if !done {
		return agent.Result{}, waitErr
	}
	secret := ""
	if handed != nil {
		secret = handed.Value
	}
	res, runErr := r.outcome(root, req.Model, time.Duration(timeout)*time.Second, secret)
	endpoint := ""
	if req.Endpoint != nil {
		endpoint = req.Endpoint.BaseURL
	}
	r.noteCredential(st, runErr, endpoint)
	if waitErr != nil {
		// Cancelled: the runner stopped the CLI and finished the job, so the usage
		// it reported is still worth recording; the outputs aren't harvested.
		return res, waitErr
	}
	var denied *credentialDeniedError
	if errors.As(runErr, &denied) {
		// Only a group-only credential the runner left in place is one its uid
		// couldn't take: a volume that refused the group got the every-uid
		// modes already, and an exit.json the job's own dir doesn't bear out
		// must not turn the group off for the rest of the process.
		borneOut := handed != nil && group != noGroup && groupErr == nil && r.deniedHere(ctx, root, r.runnerUID.Load())
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !borneOut {
			return res, errors.New(runErr.Error())
		}
		return res, runErr
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

// deniedHere reports whether the job's own dir bears out an exit.json saying
// the runner was denied this job's credential. The exit.json's owner says only
// that the runner wrote it somewhere (trust.go): any uid can put a job dir of
// its own making in the staging dir, with a credential the runner can't
// delete, and move the exit.json the runner then writes into this one. So the
// runner (its uid) must have claimed this very job (claimedBy) and left the
// credential in place, still so takeFor later: a runner that claimed the job
// this instant takes the credential well within that, which a done marker put
// there meanwhile by another uid would otherwise pass off as left.
func (r *Runner) deniedHere(ctx context.Context, root *os.Root, runner int64) bool {
	credential := filepath.Join(agentproto.CredentialDir, agentproto.CredentialFile)
	if !agentproto.Exists(root, credential) || !claimedBy(root, runner) {
		return false
	}
	if timeutil.SleepCtx(ctx, r.takeFor) != nil {
		return false
	}
	return agentproto.Exists(root, credential)
}

// The fixes for a handed credential that can't be kept to the Bridge's group,
// worded once for the log's warnings and agent.require_group's details.
const (
	remedyGroupAdd = "add that group to the agent container (group_add with the Bridge's PGID, as in the compose example) " +
		"and recreate it, on an agent image as new as this Bridge"
	remedyMapping = "the two containers see different ids on the staging volume (different user-namespace mappings, as " +
		"rootless Podman can set up): give both the same mapping"
	remedyVolume = "a staging volume that allows chown (a local one, not NFS or CIFS)"
	// lookAgain ends each fix's words: the group probe (probe.go) ends the
	// fallback once the fix is in place. noRestart follows it only for a fix
	// that leaves the Bridge's container alone (the agent container's id
	// mapping). Another staging volume means recreating both containers
	// (recreateBoth), so there the probe only ever finds a volume changed in
	// place (volumeInPlace: an export's options).
	lookAgain     = "; the Bridge looks again every few minutes"
	noRestart     = ", no Bridge restart needed"
	recreateBoth  = " and recreate both containers on it"
	volumeInPlace = ", for a volume fixed in place"
)

// blockGroup keeps, until a group probe ends it (probe.go), why
// agent.require_group can't be met (groupBlocked: Status reports the agent
// unavailable with it, so no later job is staged), logs it once with cause,
// and returns it as this run's error.
func (r *Runner) blockGroup(why, cause string) error {
	why = "agent.require_group is on, and " + why
	if r.groupBlocked.CompareAndSwap(nil, &why) {
		r.deferProbe()
		r.log.Warn("agent container: "+why, "bridge_gid", r.gid, "cause", cause)
	} else if kept := r.groupBlocked.Load(); kept != nil {
		// The reason an earlier run kept. Read once, and this run's own stands
		// when a probe ended the block in between (probeGroup).
		why = *kept
	}
	return &agent.NotAvailableError{Backend: r.backend, Detail: why}
}

// failGroup keeps, until the group can be given again (a group probe,
// probe.go, or a job staged meanwhile), that the Bridge couldn't give a handed
// credential to its group for a reason that isn't the volume refusing it
// (groupFailed: Status reports the agent unavailable with it), logs it once
// with err, and returns it as this run's error. Worded without the Bridge's
// own paths, as Status's Detail must be.
func (r *Runner) failGroup(err error) error {
	why := "the Bridge couldn't give the agent's credential to its own group on the staging volume it shares with the " +
		"agent container (" + ioProblem(err) + "): check that volume" + lookAgain
	if r.groupFailed.Swap(&why) == nil {
		r.deferProbe()
		r.log.Warn("agent container: "+why, "bridge_gid", r.gid, "err", err)
	}
	return &agent.NotAvailableError{Backend: r.backend, Detail: why}
}

// credentialDeniedError is a job the runner refused, before the CLI started,
// because its uid may not take the handed credential (Exit.CredentialDenied).
type credentialDeniedError struct{ detail string }

func (e *credentialDeniedError) Error() string { return refusedJob + e.detail }

// refusedJob starts the error of a job the runner refused (exit.json's error).
const refusedJob = "agent container refused the job: "

// notRunnersDenial ends the refusal of a job whose credential_denied another
// uid than the runner's wrote (trust.go), so the book's reason says what the
// log says only once.
const notRunnersDenial = " (not from the runner's uid on the staging volume, so the credential stayed the Bridge's " +
	"group's: if the agent container now runs as another user, restart the Bridge)"

// noteCredential remembers a credential the provider refused, and forgets one
// once a run shows it works (an answer, or a rate limit: the provider took the
// credential then). st is the runner's report when the run was handed over,
// endpoint the address the run went to ("" off the openai-compatible backend):
// a refusal is that address's, whatever the address is by now.
func (r *Runner) noteCredential(st Status, runErr error, endpoint string) {
	var na *agent.NotAvailableError
	var rl *agent.RateLimitError
	switch {
	case errors.As(runErr, &na) && na.CredentialRefused:
		r.noteRefusal(st.CLIs[r.backend], na.Detail, endpoint)
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
// runner touches it. The heartbeat counts as a sign of life when it CHANGES,
// timed on the Bridge's clock: its mtime is the runner's (or its file
// system's) clock, which may be off from this one.
func (r *Runner) wait(ctx context.Context, root *os.Root, beat func(time.Duration)) (bool, error) {
	start := r.now()
	var claimedAt, lastBeat, lastCheck time.Time
	// hb is the job's signs of life as last read (the runner's clock), alive
	// when this Bridge last saw one change (its own).
	var hb [2]time.Time
	var alive time.Time
	tick := time.NewTicker(r.poll)
	defer tick.Stop()
	for {
		if agentproto.Exists(root, agentproto.DoneFile) {
			return true, nil
		}
		now := r.now()
		if claimedAt.IsZero() {
			if agentproto.Exists(root, agentproto.ClaimDir) {
				claimedAt, lastBeat, lastCheck, alive = now, now, now, now
			} else if now.Sub(start) >= r.claimTimeout {
				if r.unclaimable(root) {
					return false, &agent.NotAvailableError{Backend: r.backend,
						Detail: fmt.Sprintf("the agent container didn't pick the job up within %s", r.claimTimeout)}
				}
				claimedAt, lastBeat, lastCheck, alive = now, now, now, now // the runner claimed it just now
			}
		}
		if !claimedAt.IsZero() {
			if now.Sub(lastCheck) >= r.beatCheck {
				lastCheck = now
				if at := r.heartbeatAt(root); at != hb {
					hb, alive = at, now
				}
				if now.Sub(alive) > r.staleFor {
					_ = root.WriteFile(agentproto.CancelFile, nil, 0o644)
					return false, fmt.Errorf("agent container: the runner stopped answering (no heartbeat for %s)", now.Sub(alive).Round(time.Second))
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

// heartbeatAt is the job's signs of life as its files show them, on the
// runner's clock: the heartbeat file's mtime and the claim's (zero for one
// missing). wait looks for either to change, never at their values. They are
// stripped of the monotonic reading and the location, so == compares the
// instants alone.
func (r *Runner) heartbeatAt(root *os.Root) [2]time.Time {
	var out [2]time.Time
	for i, name := range []string{agentproto.HeartbeatFile, agentproto.ClaimDir} {
		if info, err := root.Lstat(name); err == nil {
			out[i] = info.ModTime().UTC().Round(0)
		}
	}
	return out
}

// outcome reads exit.json and the CLI's output and interprets them with the same
// code the local runners use. An unavailable exit still reports the usage the
// CLI's output shows, so a paid run the container cut short is counted. secret
// is the credential handed to the run ("" for none), cut from the output
// (parse).
func (r *Runner) outcome(root *os.Root, model string, timeout time.Duration, secret string) (agent.Result, error) {
	raw, info, err := agentproto.ReadRegularInfo(root, agentproto.ExitFile, agentproto.MaxExitBytes)
	if err != nil {
		return agent.Result{}, r.stagingFailed("read the run's exit.json from", err)
	}
	var ex agentproto.Exit
	if err := json.Unmarshal(raw, &ex); err != nil {
		return agent.Result{}, fmt.Errorf("agent container: parse exit.json: %w", err)
	}
	if ex.Error != "" {
		if ex.CredentialDenied {
			uid, own := r.fromRunner(info, false)
			if own {
				return agent.Result{}, &credentialDeniedError{detail: ex.Error}
			}
			r.notFromRunner("a job's exit.json saying the runner was denied the credential", uid)
			if !ex.Unavailable {
				return agent.Result{}, errors.New(refusedJob + ex.Error + notRunnersDenial)
			}
		}
		if ex.Unavailable {
			// The container stopped or restarted mid-run (or couldn't start the CLI):
			// a run that got going may have spent tokens, so whatever usage its
			// stdout reports rides on the Result beside the error and the budgets
			// count it. Nothing else of a cut-off run is trusted.
			res, _ := r.parse(root, model, agent.CLIOutcome{ExitErr: errors.New(ex.Error), Timeout: timeout}, secret)
			return agent.Result{Usage: res.Usage}, &agent.NotAvailableError{Backend: r.backend, Detail: ex.Error}
		}
		return agent.Result{}, errors.New(refusedJob + ex.Error)
	}
	out := agent.CLIOutcome{TimedOut: ex.TimedOut, Timeout: timeout}
	switch {
	case ex.TimedOut:
	case ex.Cancelled:
		out.ExitErr = errors.New("cancelled")
	case ex.ExitCode != 0:
		out.ExitErr = exitStatus(ex.ExitCode)
	}
	return r.parse(root, model, out, secret)
}

// minCutSecret is the shortest handed credential parse cuts from a run's
// output. A local server that takes any key is often given a short stand-in
// ("x", "none", "ollama"): cutting that from the CLI's JSON stream and its
// last message would cut it out of ordinary words, token counts and event
// names, so the run's usage, answer and failure would all be misread. A key
// that short keeps nothing safe; every provider's real key is far longer.
const minCutSecret = 8

// parse reads the CLI's output files into out and interprets them with the
// backend's shared result mapping (ClaudeResult/CodexResult). A missing or
// unreadable file reads as empty. The handed credential (secret, "" for none;
// one of minCutSecret bytes or more)
// is cut from them first: a provider may repeat the key it refused in its
// error (an OpenAI-compatible endpoint is anyone's server), and the CLI's
// words become the run's error, a parked book's reason and the log.
func (r *Runner) parse(root *os.Root, model string, out agent.CLIOutcome, secret string) (agent.Result, error) {
	read := func(name string, limit int64) string {
		b, _ := agentproto.ReadRegular(root, name, limit)
		if len(secret) < minCutSecret {
			return string(b)
		}
		return strings.ReplaceAll(string(b), secret, "[the key]")
	}
	out.Stdout, out.Stderr = read(agentproto.StdoutFile, agentproto.MaxStdoutBytes), read(agentproto.StderrFile, agentproto.MaxStderrBytes)
	if r.cli == agent.IDCodex {
		out.LastMessage = read(agentproto.LastMessageFile, agentproto.MaxStdoutBytes)
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
