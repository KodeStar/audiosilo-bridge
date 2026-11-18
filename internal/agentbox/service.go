package agentbox

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
)

// The endpoint's own agent service (bridge#84). An endpoint's model is
// whoever answers at the address, and codex runs its commands as the runner's
// uid with its sandbox off: in the agent container such a run could read the
// subscriptions' credentials, plant a config.toml in /agent/codex that a later
// subscription run loads, or leave a process behind for the next run. So the
// openai-compatible backend's runs go to a service of their own (agent-runner
// --endpoint-only: the agent image with no /agent volume, its own user and its
// own staging volume, which kills whatever a run leaves behind), and Status
// refuses every setup that isn't that: no such service set, a staging dir
// shared with the agent container, a runner that isn't --endpoint-only, or one
// running as the agent container's user. What the Bridge can't see (the
// service's other mounts) the runner checks itself (own_credentials).

// The details for ProblemEndpointService and ProblemEndpointOnly. None names a
// Bridge path (a Detail becomes a parked book's reason in the console).
const (
	endpointServiceHow = "run the agent image as agent-runner --endpoint-only with a user and a staging volume of " +
		"its own and no /agent volume (the compose example's agent-endpoint)"
	noEndpointServiceDetail = "runs against an OpenAI-compatible endpoint need an agent service of their own, apart " +
		"from the agent container: " + endpointServiceHow + ", and set agent.endpoint_staging_dir " +
		"(AUDIOSILO_BRIDGE_AGENT_ENDPOINT_STAGING_DIR) on the Bridge to that staging volume"
	sharedDirDetail = "agent.endpoint_staging_dir shares a folder with agent.staging_dir: the endpoint's agent " +
		"service needs a staging volume of its own, which the agent container doesn't mount"
	notEndpointOnlyDetail = "the agent service at agent.endpoint_staging_dir isn't the endpoint's own (it would run " +
		"claude and codex too): " + endpointServiceHow
	sameUserDetail = "the endpoint's agent service runs as the same user as the agent container: give it a user " +
		"of its own (user: in the compose example's agent-endpoint)"
	endpointOnlyDetail = "agent.staging_dir is the OpenAI-compatible endpoint's agent service, which runs nothing " +
		"else: point it at the agent container's staging volume, or choose openai-compatible in Settings > Agent"
)

// endpointDirProblem is why the openai-compatible backend's staging dir can't
// be the endpoint's own service, read before its runner.json: none is set, or
// it shares a dir with the agent container's.
func (r *Runner) endpointDirProblem() string {
	if r.dir == "" {
		return noEndpointServiceDetail
	}
	if r.agentDir != "" && sharesDir(r.dir, r.agentDir) {
		return sharedDirDetail
	}
	return ""
}

// sharesDir reports whether a and b are one dir, or one holds the other, by
// path (each with its links resolved, so a link to a dir inside the other
// counts) or (one dir under two names) by the dirs themselves.
func sharesDir(a, b string) bool {
	if fsutil.Within(a, b) || fsutil.Within(b, a) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil && (fsutil.Within(ra, rb) || fsutil.Within(rb, ra)) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// sameUserAsAgent reports whether the endpoint's runner.json (info) and a live
// one in the agent container's staging dir have one owner: the two services
// run as one user. A missing, stale or unreadable one there (the agent
// container isn't running) says nothing; nor does a volume showing no owner.
func (r *Runner) sameUserAsAgent(info fs.FileInfo) bool {
	if r.agentDir == "" || info == nil {
		return false
	}
	uid, _, ok := fileOwner(info)
	if !ok {
		return false
	}
	root, err := os.OpenRoot(r.agentDir)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	raw, other, err := agentproto.ReadRegularInfo(root, agentproto.RunnerFile, agentproto.MaxRunnerBytes)
	if err != nil {
		return false
	}
	var rj agentproto.Runner
	if json.Unmarshal(raw, &rj) != nil || !r.live(rj) {
		return false
	}
	otherUID, _, ok := fileOwner(other)
	return ok && otherUID == uid
}
