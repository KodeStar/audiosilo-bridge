package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/librivoxjob"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// newLibrivox builds librivox.fetch (LIBRIVOX-PLAN.md L6): the manifest from
// the Bridge's own metadata base (decision 5), the files from archive.org
// through a guarded client of its own (decision 7), placed in a mapped books
// library and rescanned. Its state lives in <data>/librivox, each recording
// the paired server's id; sweepLibrivox clears what abandoned fetches left.
func (b *Bridge) newLibrivox(meta *metaops.Client, log *slog.Logger, serverID func() string) *librivoxjob.Handler {
	b.librivoxHTTP = netguard.New(netguard.Options{Version: b.version})
	b.librivox = librivoxjob.New(librivoxjob.Options{Meta: meta, Fetcher: b.librivoxHTTP, Libraries: b.libs, Mount: b.mount,
		StateDir: filepath.Join(b.dataDir, "librivox"), ServerID: serverID, Log: log,
		Server: func() librivoxjob.Server {
			if cl := b.conn.Client(); cl != nil {
				return cl
			}
			return nil
		}})
	return b.librivox
}

// sweepLibrivox removes the staging folders and state of fetches abandoned for
// librivoxjob.StagingMaxAge (audible.import's 7 days). It runs after a mapping
// check, not at start-up: the staging folders are in the libraries, whose
// mounts are known only once the server listed them and the check ran. The
// fetches whose jobs the runner still holds (running rows: one waiting for its
// lane after a restart, one a refused token stopped) are named to the sweep,
// which leaves them; when they can't be read, nothing is swept this time.
func (b *Bridge) sweepLibrivox(ctx context.Context) {
	if b.librivox == nil || ctx.Err() != nil { // an interrupted check (the Bridge is stopping) sweeps nothing
		return
	}
	rows, err := b.db.OpenServerJobs(ctx)
	if err != nil {
		b.log.Warn("bridge: reading the open jobs for the LibriVox sweep failed", "err", err)
		return
	}
	var open []json.RawMessage
	for _, sj := range rows {
		if sj.Kind == librivoxjob.Kind && sj.State == store.JobRunning {
			open = append(open, sj.Params)
		}
	}
	b.librivox.Sweep(time.Now(), open)
}
