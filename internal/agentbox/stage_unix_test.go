//go:build unix

package agentbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentproto"
)

// Only a volume that refuses the group (EPERM, ENOTSUP) gets the every-uid
// hand-off: any other failure to give the group (an I/O error) fails the
// stage as the agent being unavailable, the job never opened to the runner,
// in both modes (allowed: a refusal still falls back,
// TestHandoffCredentialFallsBackWhenTheGroupIsRefused; denied: an EIO never
// does). Not parallel: it replaces chownGroup.
func TestHandoffCredentialOtherChownErrorFails(t *testing.T) {
	saved := chownGroup
	t.Cleanup(func() { chownGroup = saved })
	chownGroup = func(*os.Root, string, int) error {
		return &fs.PathError{Op: "chownat", Path: "credential.json", Err: syscall.EIO}
	}
	for _, strict := range []bool{false, true} {
		r, dir := vaultHandoff(t, []int{os.Getegid()})
		r.requireGroup = strict
		fake := &fakeRunner{fn: func(string) agentproto.Exit { return agentproto.Exit{} }}
		stop := fake.start(t, dir)
		_, err := r.Run(context.Background(), agent.Request{Dir: stagedDir(t), Prompt: "p", Timeout: time.Minute})
		stop()
		var na *agent.NotAvailableError
		if !errors.As(err, &na) || !strings.Contains(na.Detail, "input/output error") || fake.claimed.Load() != 0 {
			t.Fatalf("strict %v: err = %v, %d jobs claimed", strict, err, fake.claimed.Load())
		}
		if r.groupBlocked.Load() != nil || !r.Status().Available {
			t.Fatalf("strict %v: an I/O error blocked the group for the process", strict)
		}
		assertNoJobDirs(t, dir)
	}
}

// groupRefused is a refusal (EPERM, ENOTSUP, EOPNOTSUPP, wrapped or not), never
// another error.
func TestGroupRefused(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]bool{
		syscall.EPERM: true,
		&fs.PathError{Op: "chownat", Path: "x", Err: syscall.ENOTSUP}:    true,
		&fs.PathError{Op: "chownat", Path: "x", Err: syscall.EOPNOTSUPP}: true,
		&fs.PathError{Op: "chownat", Path: "x", Err: syscall.EIO}:        false,
		syscall.ENOSPC:          false,
		syscall.EACCES:          false,
		errors.New("something"): false,
	} {
		if got := groupRefused(err); got != want {
			t.Errorf("groupRefused(%v) = %v, want %v", err, got, want)
		}
	}
}
