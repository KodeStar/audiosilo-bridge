package agentbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// The group probe (bridge#79). A run that finds the handed credential can't be
// kept to the Bridge's group holds its fallback for the rest of the process: a
// runner denied the group's file hands every later credential to every uid
// (groupDenied), and with agent.require_group the agent stays unavailable
// (groupBlocked). The operator's fix (the same id mapping for both containers,
// a staging volume that allows chown) then needed a Bridge restart to be seen.
// Probe looks again, at the heartbeat, with a job that hands over no
// credential: a credential.json in the group's modes holding a probe object no
// runner takes as one, and no prompt.txt, so the runner refuses the job
// whatever it reads and no CLI ever starts. The runner taking that file (it
// deletes the file before it decodes it) shows the group's file reaches it now,
// and the fallback ends; anything else leaves it as it is.

// probeEvery spaces the probes: the first comes this long after the fallback
// began, each later one this long after the last.
const probeEvery = 5 * time.Minute

// probeKind is the probe's credential.json kind: no CLI takes it
// (agentproto.DecodeCredential refuses it), so a runner never runs on it.
const probeKind = "group_probe"

// probeOutcome is what one probe found.
type probeOutcome int

const (
	// probeUnknown: nothing to go on (no claim in time, a staging failure, a
	// denial the job's dir doesn't bear out): the fallback stays.
	probeUnknown probeOutcome = iota
	// probeTaken: the runner took the group's file; the fallback ended.
	probeTaken
	// probeDenied: the runner was denied the group's file again.
	probeDenied
	// probeRefused: the staging volume still refuses the group.
	probeRefused
)

// holdsFallback reports whether a run's fallback from the group holds for the
// process (groupDenied, groupBlocked): what a probe can end.
func (r *Runner) holdsFallback() bool {
	return r.groupDenied.Load() || r.groupBlocked.Load() != nil
}

// deferProbe puts the next probe probeEvery from now.
func (r *Runner) deferProbe() {
	r.probeAfter.Store(r.now().Add(probeEvery).UnixNano())
}

// Probe starts a group probe in the background (ctx bounds it) when a run's
// fallback from the Bridge's group holds, one is due, none is running, and a
// live runner that takes a hand-off lists the group: the app calls it at every
// heartbeat. Nothing to do otherwise.
func (r *Runner) Probe(ctx context.Context) {
	if !r.holdsFallback() || r.now().UnixNano() < r.probeAfter.Load() {
		return
	}
	if st := r.Status(); !st.Live || !st.Handoff || !st.groupListed {
		return
	}
	if !r.probing.CompareAndSwap(false, true) {
		return
	}
	r.deferProbe()
	go func() {
		defer r.probing.Store(false)
		r.probeGroup(ctx)
	}()
}

// probeGroup runs one probe job and ends the fallback when the runner took the
// group's file.
func (r *Runner) probeGroup(ctx context.Context) probeOutcome {
	out := r.probeJob(ctx)
	switch out {
	case probeTaken:
		denied, blocked := r.groupDenied.Swap(false), r.groupBlocked.Swap(nil)
		r.volumeRefused.Store(false)
		if denied || blocked != nil {
			r.log.Info("agent container: the runner takes a file kept to the Bridge's group again, so a handed credential "+
				"goes to that group alone from now on", "bridge_gid", r.gid)
		}
	default:
		r.log.Debug("agent container: the group probe found the fallback still needed", "outcome", int(out))
	}
	return out
}

// probeJob stages the probe and reads what the runner made of it. The job dir
// is gone when it returns.
func (r *Runner) probeJob(ctx context.Context) probeOutcome {
	id, err := agentproto.NewJobID()
	if err != nil {
		return probeUnknown
	}
	job := filepath.Join(r.dir, id)
	defer r.remove(job)
	if err := os.Mkdir(job, 0o700); err != nil {
		return probeUnknown
	}
	root, err := openMadeDir(job)
	if err != nil {
		return probeUnknown
	}
	defer func() { _ = root.Close() }()
	if err := mkdir(root, agentproto.WorkDir, inputDir); err != nil {
		return probeUnknown
	}
	if err := mkdir(root, filepath.Join(agentproto.WorkDir, agentproto.OutDir), outDirMode); err != nil {
		return probeUnknown
	}
	// strict: a volume that refuses the group says so, rather than the probe
	// falling back to the every-uid modes it is there to leave.
	probe := agentproto.Credential{CLI: r.backend, Kind: probeKind, Value: probeKind}
	var refused *groupRefusedError
	if _, err := writeCredential(root, probe, r.gid, true); errors.As(err, &refused) {
		return probeRefused
	} else if err != nil {
		return probeUnknown
	}
	if err := openJob(root, agentproto.Request{Protocol: agentproto.Protocol, CLI: r.backend, TimeoutSeconds: 60}); err != nil {
		return probeUnknown
	}
	if done, _ := r.wait(ctx, root, nil); !done || ctx.Err() != nil {
		return probeUnknown
	}
	raw, info, err := agentproto.ReadRegularInfo(root, agentproto.ExitFile, agentproto.MaxExitBytes)
	if err != nil {
		return probeUnknown
	}
	var ex agentproto.Exit
	if err := json.Unmarshal(raw, &ex); err != nil {
		return probeUnknown
	}
	credential := filepath.Join(agentproto.CredentialDir, agentproto.CredentialFile)
	switch {
	case ex.CredentialDenied:
		// Believed as a run's denial is (trust.go): the runner's own exit.json,
		// borne out by its claim and the file left in place.
		if _, own := r.fromRunner(info, false); own && r.deniedHere(ctx, root) {
			return probeDenied
		}
	case r.claimedByRunner(root) && !agentproto.Exists(root, credential):
		return probeTaken
	}
	return probeUnknown
}
