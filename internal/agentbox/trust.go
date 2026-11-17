package agentbox

import (
	"io/fs"
	"os"

	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// Who may move the handed credential off the Bridge's group. The staging dir
// is open to every uid that reaches the volume, so any of them can write a
// runner.json (one naming no group) or a job's exit.json (credential_denied),
// and either would hand the next credential, or every later one, to every uid.
// Those two signals are believed only from the runner's own uid as the Bridge
// sees it on the volume (the files' owner: a user namespace or a squashing
// NFS export shows it as whatever uid it maps to, so no fixed uid would do):
//
//   - The uid is pinned at the first live runner.json this process reads, and
//     kept until the Bridge restarts (a runner restarted under the same user
//     keeps it; an agent container moved to another user needs a Bridge
//     restart to be believed).
//   - A live runner.json from a second uid (neither the pinned one nor the
//     Bridge's) ends the pin: two uids claim to be the runner, so neither is
//     believed, and no file moves the credential off the group until the
//     Bridge restarts. A forger who won the pin at a start (below) loses it
//     as soon as the Bridge reads the real runner's file.
//   - A runner.json owned by the Bridge's own uid pins nothing: a first-start
//     chown of the volume, or both containers running as one user, where the
//     group decides nothing anyway.
//   - A file with more than one link is not the runner's: a link to another
//     job's file made in this job's dir.
//   - An exit.json's credential_denied also needs the runner's own claim on
//     the job (claimedByRunner) and the credential left in place (deniedHere).
//     The owner alone says only that the runner wrote the file somewhere, and
//     any uid can have it write one: the runner runs whatever job dir it finds
//     in the staging dir, so a dir of another uid's making, holding a
//     credential the runner may not delete, gets a credential_denied exit.json
//     of the runner's own that its maker can move into a real job. A dir can't
//     be moved that way (only a uid that may write a dir can give it another
//     parent, and the claim is 0700), and the runner makes its claim inside
//     the job dir it then runs.
//
// A file from any other uid can't move the credential off the group: a
// runner.json naming no group still gets it in the group's modes, and its
// credential_denied is the job refused. Before the first runner.json is read
// nothing is pinned yet, so a runner.json written ahead of the runner's is
// pinned in its place (and the staging dir isn't sticky, so any uid can put
// one there just before a Bridge restart) until the real runner's next write
// is read, which a forger that puts its own back after each of them can put
// off for as long as it likes. Other limits: a uid unmapped in the
// Bridge's user namespace shows as the overflow uid, the same for every such
// uid (where the runner's own is one, each of them counts as the runner's),
// and the CLI runs as the runner's uid, so whatever it writes counts as
// the runner's: a file it left in work/out/ (open to every uid) can be moved
// into the staging dir or another job's dir, and a CLI told to (a prompt
// injected by a book's text, or a job another uid put in the staging dir with
// a credential of its own) can write a runner.json or claim a job itself.
// No owner check can tell the two apart, and none would keep the credential
// from such a CLI: it holds its own run's in its environment, and it is in
// the runner's groups, so it reads a waiting job's in the group's modes too.
// agent.require_group (Options.RequireGroup) is for an operator who wants no
// move off the group at all: there the same forgeries can only make the agent
// unavailable until the Bridge restarts.

// noUID is runnerUID before a runner.json pinned it, and contestedUID once a
// second uid wrote a live one (never a file's owner, so nothing is the
// runner's).
const (
	noUID        = -1
	contestedUID = -2
)

// fromRunner reports whether info, a file the runner writes in the staging
// dir (runner.json, a job's exit.json), was written by the runner's uid. pin
// (a live runner.json) pins the runner's uid to its owner when none is yet.
// Off Unix, where a file has no owner, every file counts as the runner's.
func (r *Runner) fromRunner(info fs.FileInfo, pin bool) (uid int, own bool) {
	uid, links, ok := fileOwner(info)
	if !ok {
		return 0, true
	}
	if links != 1 {
		// Said once: before a pin notFromRunner can't, and a volume that never
		// shows one link would otherwise refuse every denial silently.
		if pin && !r.warnedForeign.Swap(true) {
			r.log.Warn("agent container: runner.json has more than one link, so it can't name the runner's uid "+
				"and the handed credential stays the Bridge's group's alone", "links", links, "owner_uid", uid)
		}
		return uid, false
	}
	if pin && uid != r.uid {
		r.pin(uid)
	}
	return uid, int64(uid) == r.runnerUID.Load()
}

// pin takes uid, the owner of a live runner.json that isn't the Bridge's, as
// the runner's when none is pinned yet, and ends the pin (contestedUID) when
// another uid holds it.
func (r *Runner) pin(uid int) {
	for {
		cur := r.runnerUID.Load()
		switch {
		case cur == int64(uid) || cur == contestedUID:
			return
		case cur == noUID:
			if r.runnerUID.CompareAndSwap(noUID, int64(uid)) {
				r.log.Info("agent container: the runner's uid on the staging volume", "uid", uid)
				return
			}
		case r.runnerUID.CompareAndSwap(cur, contestedUID):
			r.log.Warn("agent container: a second uid wrote a live runner.json, so neither is believed to be the runner's "+
				"and the handed credential stays the Bridge's group's alone until the Bridge restarts; if the agent "+
				"container now runs as another user, restart the Bridge, else find what else writes the staging dir",
				"runner_uid", cur, "other_uid", uid)
			return
		}
	}
}

// claimedByRunner reports whether the runner itself claimed the job under
// root: its claim is a dir of the runner's uid that no other uid may write,
// so not one made by another uid, nor a dir the CLI left open in work/out/
// and another uid moved into place. Off Unix, where a file has no owner,
// every claim counts as the runner's.
func (r *Runner) claimedByRunner(root *os.Root) bool {
	info, err := root.Lstat(agentproto.ClaimDir)
	if err != nil || !info.IsDir() {
		return false
	}
	uid, _, ok := fileOwner(info)
	if !ok {
		return true
	}
	return int64(uid) == r.runnerUID.Load() && info.Mode().Perm()&0o022 == 0
}

// notFromRunner logs, once a process, a file in the staging dir that would
// have moved the handed credential off the Bridge's group but isn't the
// runner's (fromRunner), so it didn't. Not before a runner's uid is pinned
// (nothing to tell it from: a stale runner.json at the start) or once it was
// contested (pin said so), nor for the Bridge's own uid (both containers as
// one user, where the group decides nothing).
func (r *Runner) notFromRunner(what string, uid int) {
	if pinned := r.runnerUID.Load(); uid == r.uid || pinned == noUID || pinned == contestedUID || r.warnedForeign.Swap(true) {
		return
	}
	r.log.Warn("agent container: "+what+" isn't the runner's (another uid wrote it, or it has a second link), so the handed credential stays the "+
		"Bridge's group's alone; if the agent container now runs as another user, restart the Bridge",
		"owner_uid", uid, "runner_uid", r.runnerUID.Load())
}
