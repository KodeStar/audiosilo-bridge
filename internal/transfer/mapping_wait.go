package transfer

import (
	"context"
	"fmt"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
)

// Libraries is the mapping registry as a download reads it.
type Libraries interface {
	Library(int64) (link.Library, bool)
	Status(int64) (link.LibraryStatus, bool)
}

const (
	CodeNotWritable = "not_writable"
	MappingPoll     = time.Second
)

// WritableAt reports whether the current check permits a write at mount.
func WritableAt(st link.LibraryStatus, checked bool, mount string) bool {
	return checked && st.MountPath == mount && st.Mapped && st.Writable
}

// MappingWait wires the live mapping and the job's progress and failure types.
type MappingWait struct {
	Libraries   Libraries
	Mount       mapping.MountFunc
	Pairing     func() jobs.Pairing
	PairingGone error
	NotWritable func(string) error
	Wait        func(context.Context, time.Duration) error
	Report      func(*jobs.Reporter, jobs.Progress)
}

// Mapped waits for a checked, writable books library under the claim's pairing.
// A strict first attempt fails at a known unwritable mapping. A resumable job
// parks there and returns its download slot. After reclaiming the slot it
// checks both pairing and mapping again before handing the mount to its writer.
func (w MappingWait) Mapped(ctx context.Context, job jobs.Job, id int64, rep *jobs.Reporter, step string, strict bool) (link.Library, string, error) {
	for {
		if !job.Under(w.Pairing()) {
			return link.Library{}, "", w.PairingGone
		}
		lib, ok := w.Libraries.Library(id)
		if !ok || lib.LibraryKind() != "books" {
			return link.Library{}, "", w.NotWritable(fmt.Sprintf("The server lists no books library %d for the Bridge.", id))
		}
		mount, _ := w.Mount(lib.ID, lib.Root)
		st, checked := w.Libraries.Status(lib.ID)
		switch {
		case WritableAt(st, checked, mount):
			rep.Unpark()
			if !rep.Yielded() {
				return lib, mount, nil
			}
			w.Report(rep, jobs.Progress{Step: step, Code: jobs.SayDownloadWait})
			if err := rep.Reclaim(ctx); err != nil {
				return link.Library{}, "", err
			}
			continue
		case !checked || st.MountPath != mount:
			w.Report(rep, jobs.Progress{Step: step, Code: jobs.SayMappingWait})
			if !checked && st.Error != "" {
				rep.Yield()
			}
		case strict:
			_, why := jobs.LibraryNotWritable(lib.Name)
			return link.Library{}, "", w.NotWritable(why)
		default:
			said, why := jobs.LibraryNotWritable(lib.Name)
			w.Report(rep, jobs.Progress{Step: step, Code: jobs.SayLibraryNotWritable, Params: said,
				Extra: map[string]any{"park": map[string]any{"code": CodeNotWritable}}})
			rep.Park(CodeNotWritable, why)
			rep.Yield()
		}
		if err := w.Wait(ctx, MappingPoll); err != nil {
			return link.Library{}, "", err
		}
	}
}
