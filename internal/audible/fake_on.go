//go:build audiblefake

package audible

import (
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/kodestar/audiosilo-bridge/internal/audible/audiblefake"
)

// FakeURLEnv points every Audible and Amazon endpoint at an Audible fake (the
// audiblefake package, served by `go run -tags audiblefake ./cmd/audiblefake`).
// It exists only in a build with the audiblefake tag; release images never carry
// the tag, so there the variable does nothing.
const FakeURLEnv = "AUDIOSILO_BRIDGE_AUDIBLE_FAKE_URL"

// RecordDirEnv, in an audiblefake build, records every Audible exchange into a
// JSON-lines file in that directory, scrubbed of credentials as it is written
// (audiblefake.Recorder): the owner's recording run that later fixtures come
// from.
const RecordDirEnv = "AUDIOSILO_BRIDGE_AUDIBLE_RECORD_DIR"

// FakeURL is the fake's base URL from FakeURLEnv, "" when unset.
func FakeURL() string { return strings.TrimRight(strings.TrimSpace(os.Getenv(FakeURLEnv)), "/") }

// recorders is the process's one Recorder per recording dir: every signed call
// builds its own client, and a run is one file, not one per call (each left
// open until the collector closes it).
var recorders sync.Map // dir -> *audiblefake.Recorder

// wrapTransport records through audiblefake.Recorder when RecordDirEnv is set.
func wrapTransport(rt http.RoundTripper) http.RoundTripper {
	if dir := strings.TrimSpace(os.Getenv(RecordDirEnv)); dir != "" {
		r, _ := recorders.LoadOrStore(dir, audiblefake.NewRecorder(rt, dir))
		return r.(*audiblefake.Recorder)
	}
	return rt
}
