//go:build linux

package agentrun

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// sweepRounds bounds a sweep: killing a stray hands its children to the
// runner, so it looks again until none is left.
const sweepRounds = 20

// endpointHardening makes the runner the child subreaper of everything its
// runs start (a process that detaches becomes its child, never init's) and
// not dumpable, so a run's commands, its own uid with no capabilities, can
// neither ptrace it nor read its memory or environment through /proc (the
// keys of later runs pass through it).
func endpointHardening() error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return err
	}
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}

// ownSession is the runner's session id (read once: it never changes).
var ownSession = sync.OnceValue(func() int {
	sid, err := unix.Getsid(0)
	if err != nil {
		return -1
	}
	return sid
})

// killStrays kills the runner's strays (strays, read from /proc) and reaps
// those that are its children, round after round until none is left, and
// returns how many it ended. Killing them one by one, as soon as /proc is
// read, races a stray that forks and exits in between (its child wasn't
// listed), so where the pid namespace holds nothing else (onlyStrays: the
// endpoint's own container, no CLI running) a kill(-1) follows, which the
// kernel delivers to every process at once, past which no fork slips; init
// and the runner are never among its targets.
func killStrays(keep func(sid int) bool) int {
	self, ended := os.Getpid(), 0
	for range sweepRounds {
		procs := readProcs("/proc")
		found := strays(procs, self, keep)
		if len(found) == 0 {
			break
		}
		for _, p := range found {
			if !p.zombie {
				_ = unix.Kill(p.pid, unix.SIGKILL)
			}
		}
		if onlyStrays(procs, self, found) {
			_ = unix.Kill(-1, unix.SIGKILL)
		}
		deadline := time.Now().Add(2 * time.Second)
		for _, p := range found {
			if p.ppid == self {
				reap(p.pid, deadline)
			}
		}
		ended += len(found)
	}
	return ended
}

// reap waits for the runner's child pid to exit (it was just killed), until
// deadline: one stuck in the kernel is reaped by a later sweep.
func reap(pid int, deadline time.Time) {
	for {
		got, err := unix.Wait4(pid, nil, unix.WNOHANG, nil)
		if got == pid || (err != nil && !errors.Is(err, unix.EINTR)) || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// readProcs lists the processes under dir (/proc): pid, parent, session and
// whether each has exited unreaped. One that goes while being read is left
// out.
func readProcs(dir string) []proc {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "stat")) //nolint:gosec // /proc/<pid>/stat, the pid a number
		if err != nil {
			continue
		}
		if p, ok := parseStat(pid, string(b)); ok {
			out = append(out, p)
		}
	}
	return out
}

// parseStat reads /proc/<pid>/stat: "pid (comm) state ppid pgrp session ...",
// where comm may hold spaces and parentheses (so the fields follow its last
// ')').
func parseStat(pid int, stat string) (proc, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return proc{}, false
	}
	f := strings.Fields(stat[i+1:])
	if len(f) < 4 {
		return proc{}, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	sid, err2 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil {
		return proc{}, false
	}
	return proc{pid: pid, ppid: ppid, sid: sid, zombie: f[0] == "Z"}, true
}
