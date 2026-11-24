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
	// CodeNotWritable is the failure and park code for a library the job may not write.
	CodeNotWritable = "not_writable"
	// MappingPoll is how often a job waiting on its library's mapping looks again.
	MappingPoll = time.Second
)

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
// A job claimed before the library's mapping check has a result waits for it.
// A strict first attempt fails at a known unwritable mapping; a resumed job, or
// one past its download, parks not_writable and waits, keeping what it holds.
//
// A parked job, or one waiting on a check that doesn't stand (the server didn't
// answer it, or it is too old), gives its download-lane slot back (Yield), so
// the lane's other downloads run meanwhile. Once the mapping is back it takes a
// slot again (Reclaim) and looks at pairing and mapping again before handing
// the mount to its writer, since the wait for the slot may have been long.
//
// The run is bound to the pairing that claimed job (jobs.Job.Under): once the
// Bridge is paired otherwise (unpaired, paired again, or with another server,
// whose libraries share ids with this one's), it ends with PairingGone and
// writes nothing to the library the registry lists now.
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
		case mapping.WritableAt(st, checked, mount):
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
				// A check that doesn't stand may wait out an outage: free the lane.
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
