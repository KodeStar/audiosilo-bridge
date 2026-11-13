package app

import (
	"log/slog"
	"path/filepath"

	"github.com/kodestar/audiosilo-bridge/internal/librivoxjob"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
)

// newLibrivox builds librivox.fetch (LIBRIVOX-PLAN.md L6): the manifest from
// the Bridge's own metadata base (decision 5), the files from archive.org
// through a guarded client of its own (decision 7), placed in a mapped books
// library and rescanned. Its state lives in <data>/librivox.
func (b *Bridge) newLibrivox(meta *metaops.Client, log *slog.Logger) *librivoxjob.Handler {
	b.librivoxHTTP = netguard.New(netguard.Options{Version: b.version})
	return librivoxjob.New(librivoxjob.Options{Meta: meta, Fetcher: b.librivoxHTTP, Libraries: b.libs, Mount: b.mount,
		StateDir: filepath.Join(b.dataDir, "librivox"), Log: log,
		Server: func() librivoxjob.Server {
			if cl := b.conn.Client(); cl != nil {
				return cl
			}
			return nil
		}})
}
