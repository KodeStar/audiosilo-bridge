//go:build unix

package agentbox

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// spoiledWarned is how often the log warned of a spoiled probe.
func spoiledWarned(logged string) int {
	return strings.Count(logged, "another uid than the runner's claimed or answered the group probe")
}

// A probe another uid takes (claims it, takes the credential, answers) while
// the runner is still denied the group's file never ends the fallback, so the
// next run isn't denied all over again (the flap); it is warned of once,
// naming that uid, and the page says the hand-off is shared until a probe
// gets the runner's own answer (allowed: the runner's take ends the fallback
// and the warning; denied: another uid's take ends neither). Under
// agent.require_group the block's detail says so instead.
func TestProbeTakenByAnotherUID(t *testing.T) {
	t.Parallel()
	for _, require := range []bool{false, true} {
		t.Run("require_group="+strconv.FormatBool(require), func(t *testing.T) {
			t.Parallel()
			r, dir := vaultHandoff(t, []int{os.Getegid()})
			r.requireGroup = require
			var logged strings.Builder
			r.log = slog.New(slog.NewTextHandler(&logged, nil))
			m := &mappedRunner{}
			m.deny.Store(true)
			stop := (&fakeRunner{fn: m.fn(t)}).start(t, dir)
			_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
			var na *agent.NotAvailableError
			if require != errors.As(err, &na) || !require && err != nil {
				t.Fatalf("the denied run: %v", err)
			}
			runner := int64(os.Geteuid())
			if !r.holdsFallback() || r.runnerUID.Load() != runner {
				t.Fatalf("after the denial: fallback %v, runner uid %d", r.holdsFallback(), r.runnerUID.Load())
			}

			// The test's uid takes the probe now, but the runner is another.
			m.deny.Store(false)
			r.runnerUID.Store(otherUID())
			for range 2 {
				if got := r.probeGroup(context.Background()); got != probeSpoiled || !r.holdsFallback() {
					t.Fatalf("another uid's take = %v, fallback %v", got, r.holdsFallback())
				}
			}
			if s := r.spoiled.Load(); s == nil || int64(s.uid) != runner {
				t.Fatalf("spoiled by %+v, want uid %d", s, runner)
			}
			if n := spoiledWarned(logged.String()); n != 1 || !strings.Contains(logged.String(), "uid="+strconv.FormatInt(runner, 10)) {
				t.Fatalf("warned %d times, want once naming the uid:\n%s", n, logged.String())
			}

			// Status as the runner (the pin back on the test's uid) reads it.
			r.runnerUID.Store(runner)
			st := r.Status()
			if require {
				if st.Available || st.Problem != ProblemGroupRequired || !strings.HasSuffix(st.Detail, spoiledBlocked) || st.Warning != "" {
					t.Fatalf("blocked, spoiled: %+v", st)
				}
			} else if !st.Available || st.HandoffMode != HandoffWorld || st.Warning != SpoiledWarning {
				t.Fatalf("shared, spoiled: %+v", st)
			}

			// The runner's own take ends both.
			if got := r.probeGroup(context.Background()); got != probeTaken || r.holdsFallback() || r.spoiled.Load() != nil {
				t.Fatalf("the runner's take = %v, fallback %v, spoiled %+v", got, r.holdsFallback(), r.spoiled.Load())
			}
			if st := r.Status(); !st.Available || st.HandoffMode != HandoffGroup || st.Warning != "" ||
				strings.Contains(st.Detail, spoiledBlocked) {
				t.Fatalf("after the runner's take: %+v", st)
			}
			stop()
			want := 1 // the denied run, staged again for every uid
			if require {
				want = 0
			}
			if m.ran != want {
				t.Fatalf("the CLI ran %d times, want %d (never for a probe)", m.ran, want)
			}
			assertNoJobDirs(t, dir)
		})
	}
}

// A probe whose dir holds what the runner could not have left (an answer with
// no claim, a claim open to other uids, an answer older than the claim, the
// credential gone before the runner looked) is spoiled, with no uid to name,
// and keeps the fallback, each as the runner's own uid would leave it
// otherwise (allowed: none; denied: each keeps the fallback and is noted).
func TestProbeAnswerTheRunnerCouldNotHaveWritten(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(job string) agentproto.Exit{
		"answered with no claim": takeProbe,
		"a claim open to every uid": func(job string) agentproto.Exit {
			claim := filepath.Join(job, agentproto.ClaimDir)
			_ = os.Mkdir(claim, 0o700)
			_ = os.Chmod(claim, 0o777)
			return takeProbe(job)
		},
		// The runner's refusal of another job, moved in before its claim here:
		// its exit.json is older than the claim.
		"an answer older than the claim": func(job string) agentproto.Exit {
			claim := filepath.Join(job, agentproto.ClaimDir)
			_ = os.Mkdir(claim, 0o700)
			later := time.Now().Add(time.Hour)
			_ = os.Chtimes(claim, later, later)
			return takeProbe(job)
		},
		"the credential taken first": func(job string) agentproto.Exit {
			_ = os.Mkdir(filepath.Join(job, agentproto.ClaimDir), 0o700)
			_ = os.Remove(filepath.Join(job, agentproto.CredentialDir, agentproto.CredentialFile))
			return agentproto.Exit{ExitCode: -1, Unavailable: true,
				Error: "claude has no credential: set one on the Bridge's Accounts tab, or give one to the agent container"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, dir := vaultHandoff(t, []int{os.Getegid()})
			r.groupDenied.Store(true)
			r.runnerUID.Store(int64(os.Geteuid()))
			stop := (&fakeRunner{fn: answer, noClaim: true}).start(t, dir)
			got := r.probeGroup(context.Background())
			stop()
			if got != probeSpoiled || !r.groupDenied.Load() {
				t.Fatalf("probe = %v, group denied %v", got, r.groupDenied.Load())
			}
			if s := r.spoiled.Load(); s == nil || s.uid != noUID {
				t.Fatalf("spoiled by %+v, want no uid named", s)
			}
			if st := r.Status(); st.Warning != SpoiledWarning {
				t.Fatalf("status: %+v", st)
			}
			assertNoJobDirs(t, dir)
		})
	}
}

// A clean answer after a spoiled probe ends the warning, the fallback kept:
// the runner still denied says so itself (allowed: a denial clears the
// warning; denied: it never ends the fallback).
func TestProbeDeniedClearsTheSpoil(t *testing.T) {
	t.Parallel()
	r, dir := vaultHandoff(t, []int{os.Getegid()})
	r.groupDenied.Store(true)
	r.runnerUID.Store(int64(os.Geteuid()))
	r.spoiled.Store(&probeSpoil{uid: 4242})
	m := &mappedRunner{}
	m.deny.Store(true)
	stop := (&fakeRunner{fn: m.fn(t)}).start(t, dir)
	got := r.probeGroup(context.Background())
	stop()
	if got != probeDenied || !r.groupDenied.Load() || r.spoiled.Load() != nil {
		t.Fatalf("probe = %v, group denied %v, spoiled %+v", got, r.groupDenied.Load(), r.spoiled.Load())
	}
	if st := r.Status(); st.Warning != "" || st.HandoffMode != HandoffWorld {
		t.Fatalf("status: %+v", st)
	}
	assertNoJobDirs(t, dir)
}
