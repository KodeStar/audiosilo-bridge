package agentbox

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// The group probe (bridge#79). A run that finds the handed credential can't be
// kept to the Bridge's group holds its fallback for the rest of the process: a
// runner denied the group's file hands every later credential to every uid
// (groupDenied), and with agent.require_group the agent stays unavailable
// (groupBlocked). The operator's fix (the same id mapping for both containers,
// a staging volume that allows chown) then needed a Bridge restart to be seen.
// A credential the Bridge couldn't give its group at all (groupFailed: an I/O
// error, not a refusal) holds the agent unavailable the same way.
// Probe looks again, at the heartbeat, with a job that hands over no
// credential: a credential.json in the group's modes holding a probe object no
// runner takes as one, and no prompt.txt, so the runner refuses the job
// whatever it reads and no CLI ever starts. The runner taking that file (it
// deletes the file before it decodes it, and its refusal names the probe's
// kind) shows the group's file reaches it now, and the fallback ends; anything
// else leaves it as it is.
//
// The staging dir is open to every uid on the volume, so another one can get
// to a probe first: claim it, delete its credential, or write its exit.json or
// done. A uid in the Bridge's group that took the probe so would end the
// fallback while the runner is still denied the group's file, and the next run
// would start it again (a flap, warned of every probeEvery); any uid could
// keep each probe from ever ending it. Only the runner's take ends it
// (probeRunner, spoiler): anything else in the probe's dir is a spoiled probe,
// warned of with the uid that did it and shown on the page (Status.Warning,
// or the require_group block's detail) until a probe gets the runner's answer.
// Ending the fallback only ever moves the credential onto the group, so the
// rule is about telling the operator, never about who may read the credential.

// probeEvery spaces the probes: the first comes this long after the fallback
// began, each later one this long after the last.
const probeEvery = 5 * time.Minute

// probeKind is the probe's credential.json kind: no CLI takes it
// (agentproto.DecodeCredential refuses it), so a runner never runs on it.
const probeKind = agentproto.CredentialGroupProbe

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
	// probeSpoiled: another uid than the runner's claimed or answered the
	// probe, or took its credential: the fallback stays, and the page says
	// so (Runner.spoiled).
	probeSpoiled
)

// probeSpoil is who spoiled the last group probe: the uid that claimed or
// answered it, or noUID when what is in its dir names no other uid (a done
// with no claim, a credential gone before the runner read it).
type probeSpoil struct{ uid int }

// spoiledBy is the probeSpoil for a file of uid's that isn't the runner's
// (runner): no uid to name when it is the runner's own.
func spoiledBy(uid, runner int) *probeSpoil {
	if uid == runner {
		return &probeSpoil{uid: noUID}
	}
	return &probeSpoil{uid: uid}
}

// holdsFallback reports whether a run's fallback from the group holds for the
// process (groupDenied, groupBlocked, groupFailed): what a probe can end.
func (r *Runner) holdsFallback() bool {
	return r.groupDenied.Load() || r.groupBlocked.Load() != nil || r.groupFailed.Load() != nil
}

// deferProbe puts the next probe probeEvery from now.
func (r *Runner) deferProbe() {
	r.probeAfter.Store(r.now().Add(probeEvery).UnixNano())
}

// Probe starts a group probe in the background (ctx bounds it, Wait waits for
// it) when a run's fallback from the Bridge's group holds, one is due, none
// is running, and a live runner that takes a hand-off lists the group: the
// app calls it at every heartbeat. Nothing to do otherwise.
func (r *Runner) Probe(ctx context.Context) {
	if !r.holdsFallback() || r.now().UnixNano() < r.probeAfter.Load() || !r.probing.CompareAndSwap(false, true) {
		return
	}
	// Deferred whatever the gate below finds, so a runner that can't take a
	// probe yet is read again only once per probeEvery.
	r.deferProbe()
	if st := r.Status(); !st.Live || !st.Handoff || !st.groupListed {
		r.probing.Store(false)
		return
	}
	r.probes.Go(func() {
		defer r.probing.Store(false)
		r.probeGroup(ctx)
	})
}

// Wait returns once no probe is in flight: the app calls it as it stops (the
// heartbeat that calls Probe already ended, its ctx cancelled), so a probe cut
// short still removes its job dir before the process goes.
func (r *Runner) Wait() { r.probes.Wait() }

// probeGroup runs one probe job and ends the fallback when the runner took the
// group's file.
func (r *Runner) probeGroup(ctx context.Context) probeOutcome {
	out, spoil := r.probeJob(ctx)
	if out == probeTaken || out == probeDenied || out == probeRefused {
		// The runner's own answer: whatever spoiled an earlier probe didn't this one.
		r.spoiled.Store(nil)
	}
	switch out {
	case probeTaken:
		denied, blocked := r.groupDenied.Swap(false), r.groupBlocked.Swap(nil)
		r.volumeRefused.Store(false)
		if denied || blocked != nil {
			r.log.Info("agent container: the runner takes a file kept to the Bridge's group again, so a handed credential "+
				"goes to that group alone from now on", "bridge_gid", r.gid)
		}
	case probeSpoiled:
		r.noteSpoiled(spoil)
	default:
		r.log.Debug("agent container: the group probe found the fallback still needed", "outcome", int(out))
	}
	return out
}

// noteSpoiled keeps who spoiled a probe, for Status, while a fallback the
// probe could have ended still holds (a group failure alone is ended by the
// probe's own write, whoever answers), and warns of it: once for each uid in a
// row, so a spoiler at every probe doesn't fill the log.
func (r *Runner) noteSpoiled(s *probeSpoil) {
	if !r.groupDenied.Load() && r.groupBlocked.Load() == nil {
		return
	}
	if prev := r.spoiled.Swap(s); prev != nil && *prev == *s {
		r.log.Debug("agent container: the group probe was spoiled again", "uid", s.uid)
		return
	}
	held := "the handed credential stays readable by every uid on the staging volume until the runner takes it"
	if r.requireGroup {
		held = "agent.require_group keeps the agent unavailable"
	}
	who := []any{"runner_uid", r.runnerUID.Load()}
	if s.uid != noUID {
		who = append(who, "uid", s.uid)
	}
	r.log.Warn("agent container: another uid than the runner's claimed or answered the group probe, or took its "+
		"credential, so the Bridge can't tell whether the runner takes a file kept to its group: "+held+
		"; find what else writes the staging dir", who...)
}

// probeJob stages the probe and reads what the runner made of it, and who
// spoiled it for probeSpoiled. The job dir is gone when it returns.
func (r *Runner) probeJob(ctx context.Context) (probeOutcome, *probeSpoil) {
	// Bounded: a claimed probe that never finishes (a heartbeat kept fresh
	// in its dir) would otherwise hold probing, and so every later probe, for
	// the rest of the process. The runner takes and refuses a probe at once,
	// so the claim timeout and then staleFor are plenty.
	bounded, cancel := context.WithTimeout(ctx, r.claimTimeout+r.staleFor)
	defer cancel()
	id, err := agentproto.NewJobID()
	if err != nil {
		return probeUnknown, nil
	}
	job := filepath.Join(r.dir, id)
	defer r.remove(job)
	root, err := makeJob(job)
	if err != nil {
		return probeUnknown, nil
	}
	defer func() { _ = root.Close() }()
	if err := mkdir(root, agentproto.WorkDir, inputDir); err != nil {
		return probeUnknown, nil
	}
	if err := mkdir(root, filepath.Join(agentproto.WorkDir, agentproto.OutDir), outDirMode); err != nil {
		return probeUnknown, nil
	}
	// strict: a volume that refuses the group says so, rather than the probe
	// falling back to the every-uid modes it is there to leave.
	probe := agentproto.Credential{CLI: r.backend, Kind: probeKind, Value: probeKind}
	var refused *groupRefusedError
	_, err = writeCredential(root, probe, r.gid, true)
	if (err == nil || errors.As(err, &refused)) && r.groupFailed.Swap(nil) != nil {
		// The group was given, or the volume now refuses it outright, which
		// the next run answers by itself (the every-uid modes, or
		// agent.require_group's block): either way the failure that was
		// neither (groupFailed) is over, whatever the runner makes of the
		// probe.
		r.log.Info("agent container: giving a handed credential to the Bridge's group no longer fails as it did, so "+
			"jobs go to the agent container again", "bridge_gid", r.gid, "volume_refuses", refused != nil)
	}
	if refused != nil {
		return probeRefused, nil
	} else if err != nil {
		return probeUnknown, nil
	}
	if err := openJob(root, agentproto.Request{Protocol: agentproto.Protocol, CLI: r.backend, TimeoutSeconds: 60}); err != nil {
		return probeUnknown, nil
	}
	done, _ := r.wait(bounded, root, nil)
	if ctx.Err() != nil {
		return probeUnknown, nil
	}
	runner, ok := r.probeRunner()
	if !ok {
		return probeUnknown, nil
	}
	if s := r.spoiler(root, runner); s != nil {
		return probeSpoiled, s
	}
	if !done || bounded.Err() != nil {
		return probeUnknown, nil
	}
	raw, err := agentproto.ReadRegular(root, agentproto.ExitFile, agentproto.MaxExitBytes)
	if err != nil {
		return probeUnknown, nil
	}
	var ex agentproto.Exit
	if err := json.Unmarshal(raw, &ex); err != nil {
		return probeUnknown, nil
	}
	// The claim, exit.json and done are the runner's from here (spoiler).
	credential := filepath.Join(agentproto.CredentialDir, agentproto.CredentialFile)
	switch {
	case ex.CredentialDenied:
		// Believed as a run's denial is (trust.go): the runner's own exit.json,
		// borne out by its claim and the file left in place.
		if r.deniedHere(bounded, root, int64(runner)) {
			return probeDenied, nil
		}
	case !gone(root, credential):
		// The file is still there (or unreadable): no take, whatever the
		// runner says.
	case strings.Contains(ex.Error, strconv.Quote(probeKind)):
		// The runner's refusal of the probe's kind: it read the group's file
		// and deleted it first (agentrun's takeCredential), which only a uid
		// the volume lets into the Bridge's group can. The runner's uid as
		// the volume shows it may well have changed with the fix (a new id
		// mapping contests the pin): probeRunner follows runner.json then.
		return probeTaken, nil
	case ex.Unavailable:
		// The runner found no credential to take: another uid in the group
		// deleted it first, which would have passed for the runner's take.
		return probeSpoiled, &probeSpoil{uid: noUID}
	}
	return probeUnknown, nil
}

// probeRunner is the runner's uid as a probe judges it: the pinned one, else
// (none pinned: runner.json is the Bridge's own uid's, or has a second link;
// or the pin is contested, as an id mapping's fix leaves it) the owner of
// runner.json as it is now. ok false when there is none to read (or a second
// link names no one); off Unix, where a file has no owner, any uid does.
// runner.json can be forged by any uid, so an unpinned runner is believed
// only for the probe, which can only move the credential onto the group.
func (r *Runner) probeRunner() (uid int, ok bool) {
	if pinned := r.runnerUID.Load(); pinned >= 0 {
		return int(pinned), true
	}
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return 0, false
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(agentproto.RunnerFile)
	if err != nil {
		return 0, false
	}
	uid, links, owned := fileOwner(info)
	if !owned {
		return 0, true
	}
	return uid, links == 1
}

// spoiler is who interfered with the probe under root (runner the runner's
// uid, probeRunner): a claim that isn't the runner's (another uid's, or one
// open to other uids: a dir the CLI left in work/out/ and moved in), and an
// exit.json or done that isn't (another uid's, a second link, or with no claim
// of the runner's: it claims before it answers). A claim of the Bridge's own
// uid is wait taking an unclaimed probe back. nil when nothing did, and off
// Unix, where a file has no owner.
func (r *Runner) spoiler(root *os.Root, runner int) *probeSpoil {
	runnerClaimed := false
	if info, err := root.Lstat(agentproto.ClaimDir); err == nil {
		uid, _, ok := fileOwner(info)
		if !ok {
			return nil
		}
		runnerClaimed = runnersClaim(info, int64(runner))
		if !runnerClaimed && uid != r.uid {
			return spoiledBy(uid, runner)
		}
	}
	for _, name := range []string{agentproto.ExitFile, agentproto.DoneFile} {
		info, err := root.Lstat(name)
		if err != nil {
			continue
		}
		uid, links, ok := fileOwner(info)
		if !ok {
			return nil
		}
		if uid != runner || links != 1 || !runnerClaimed {
			return spoiledBy(uid, runner)
		}
	}
	return nil
}

// gone reports whether name (under root) is no longer there: only a
// not-exist answer counts, never another error reading it (an I/O error on
// the volume), which would otherwise pass for the runner's take.
func gone(root *os.Root, name string) bool {
	_, err := root.Lstat(name)
	return errors.Is(err, fs.ErrNotExist)
}
