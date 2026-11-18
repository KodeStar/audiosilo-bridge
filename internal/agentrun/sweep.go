package agentrun

import "sort"

// The endpoint's service (agent-runner --endpoint-only) runs a model whoever
// answers at the endpoint's address directs, as its own uid: a command it runs
// may fork into the background, leave its process group, start a session of
// its own and so outlive the run, to read the next run's key from that run's
// environment (another address's, after a change in Settings > Agent) or
// change its files. So that service's runner is the child subreaper of
// everything its runs start (Main): whatever a run leaves behind becomes the
// runner's child, however it detaches, and the sweep before each CLI starts and
// after it ends kills every such stray. It spares the runner's own session
// (its CLIs' --version) and the session of each CLI running now (spawn starts
// each in its own, and no process can join a group outside its session).

// proc is one process as the sweep sees it.
type proc struct {
	pid, ppid, sid int
	zombie         bool
}

// strays are the processes a sweep ends: the runner's children (self) in a
// session it doesn't keep, with everything they started, whatever their own
// sessions. A kept child's descendants are its run's (or the runner's own) and
// are spared with it.
func strays(procs []proc, self int, keep func(sid int) bool) []proc {
	children := map[int][]proc{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p)
	}
	var out []proc
	var walk func(pid int)
	walk = func(pid int) {
		for _, c := range children[pid] {
			if c.pid == pid {
				continue
			}
			out = append(out, c)
			walk(c.pid)
		}
	}
	for _, c := range children[self] {
		if c.pid == self || keep(c.sid) {
			continue
		}
		out = append(out, c)
		walk(c.pid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pid < out[j].pid })
	return out
}

// setSession records whether a CLI's session (its pid) is running now.
func (r *Runner) setSession(pid int, on bool) {
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	if on {
		r.sessions[pid] = true
	} else {
		delete(r.sessions, pid)
	}
}

// keepSession reports whether a sweep spares the session sid: a CLI's
// running now, or the runner's own.
func (r *Runner) keepSession(sid int) bool {
	if sid == ownSession() {
		return true
	}
	r.activeMu.Lock()
	defer r.activeMu.Unlock()
	return r.sessions[sid]
}

// sweepStrays ends what earlier runs left behind (an endpoint-only runner's,
// set by Main; nothing for the agent container's).
func (r *Runner) sweepStrays() {
	if r.sweep == nil {
		return
	}
	if n := r.sweep(r.keepSession); n > 0 {
		r.log.Warn("agent runner: ended processes a run left behind", "count", n)
	}
}
