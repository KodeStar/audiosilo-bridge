package agentbox

import (
	"io/fs"
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
//   - A runner.json owned by the Bridge's own uid pins nothing: a first-start
//     chown of the volume, or both containers running as one user, where the
//     group decides nothing anyway.
//   - A file with more than one link is not the runner's: a link to another
//     job's file made in this job's dir.
//
// A file from any other uid can't move the credential off the group: a
// runner.json naming no group still gets it in the group's modes, and its
// credential_denied is the job refused. Before the first runner.json is read
// nothing is pinned yet, so a runner.json written ahead of the runner's is
// pinned in its place; agent.require_group (Options.RequireGroup) is for an
// operator who wants no move off the group at all.

// noUID is runnerUID before a runner.json pinned it.
const noUID = -1

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
		return uid, false
	}
	if pin && uid != r.uid && r.runnerUID.CompareAndSwap(noUID, int64(uid)) {
		r.log.Info("agent container: the runner's uid on the staging volume", "uid", uid)
	}
	return uid, int64(uid) == r.runnerUID.Load()
}

// notFromRunner logs, once a process, a file in the staging dir that would
// have moved the handed credential off the Bridge's group but isn't the
// runner's (fromRunner), so it didn't.
func (r *Runner) notFromRunner(what string, uid int) {
	if r.warnedForeign.Swap(true) {
		return
	}
	r.log.Warn("agent container: "+what+" isn't the runner's (another uid wrote it), so the handed credential stays the "+
		"Bridge's group's alone; if the agent container now runs as another user, restart the Bridge",
		"owner_uid", uid, "runner_uid", r.runnerUID.Load())
}
