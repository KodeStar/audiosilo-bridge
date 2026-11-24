package librivoxjob

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/transfer"
)

// state is a fetch's plan and progress, kept in the Bridge's data
// (<StateDir>/<library_id>-<librivox_id>.json, one per library and project, so
// fetches of one project into two libraries never touch each other's staging)
// from the moment it picks the folder: a resumed run (a restart, a lost claim
// handed out again) goes on with the same staging folder and destination
// instead of planning again against a library that changed meanwhile, Abandon
// finds the staging folder to remove, and once the folder is placed it is the
// placement marker (a failed rescan retries from the rescan, decision 6).
type state struct {
	LibraryID int64 `json:"library_id"`
	// ServerID is the paired server's id when the fetch planned ("" when it
	// was unknown, or a state from before it was kept): LibraryID is that
	// server's library. A state of another server is never acted on.
	ServerID string `json:"server_id,omitempty"`
	// JobID is the id of the server's job whose run last planned or resumed
	// the fetch (0 on a state from before it was kept), Attempt that claim's
	// attempt and Pairing the Bridge's pairing it was claimed under (0 and ""
	// on a state from before they were kept): Abandon removes the staging of
	// its own claim only (claimedBy). The id alone doesn't name it: the
	// server hands a job out again at a later attempt, to a pairing made
	// since too, while the earlier claim's release may still be on its way.
	JobID   int64  `json:"job_id,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	Pairing string `json:"pairing,omitempty"`
	// Parent is the folder the book goes in ("" for the library's root), and
	// MadeParent whether this fetch created it (removed again, when empty, with
	// the staging folder).
	Parent     string `json:"parent"`
	MadeParent bool   `json:"made_parent,omitempty"`
	// Dest is the book's folder, directly in Parent (as the staging folder is).
	Dest string `json:"dest"`
	// Files is each section's partial download: the strong ETag and the
	// size its response stated, so a resume asks only for the rest.
	Files map[string]partState `json:"files,omitempty"`
	// Placed: the staging folder became Dest, holding Count files of Bytes.
	Placed bool  `json:"placed,omitempty"`
	Count  int   `json:"count,omitempty"`
	Bytes  int64 `json:"bytes,omitempty"`
	// Unmapped is when the sweep first found the library's mapping gone (the
	// library deleted on the server, or no longer mapped here) with the fetch
	// unplaced: StagingMaxAge after it the state ends (sweepOne). A run that
	// finds the library mapped again clears it at once.
	Unmapped time.Time `json:"unmapped,omitzero"`
}

// claimedBy reports whether the state was last planned or resumed by this
// claim of job, whose server is serverID: its job, attempt and pairing.
func (st *state) claimedBy(job jobs.Job, serverID string) bool {
	return st.ServerID == serverID && st.JobID == job.ID && st.Attempt == job.Attempt && st.Pairing == job.Pairing()
}

// claim records job's claim, whose server is serverID, as the state's.
func (st *state) claim(job jobs.Job, serverID string) {
	st.ServerID, st.JobID, st.Attempt, st.Pairing = serverID, job.ID, job.Attempt, job.Pairing()
}

// partState is one section's partial download.
type partState struct {
	ETag  string `json:"etag,omitempty"`
	Total int64  `json:"total,omitempty"`
}

// stagingPrefix begins every staging folder's name.
const stagingPrefix = transfer.LibrivoxStagePrefix

// stagingName is the staging folder's name: a dot folder the server's scan
// never indexes, beside the book's destination so placing it is one rename.
func stagingName(id int) string { return transfer.LibrivoxStageName(id) }

// staging is the project's staging folder: <parent>/.audiosilo-librivox-<id>.
func (st *state) staging(id int) string { return path.Join(st.Parent, stagingName(id)) }

// statePath is the state file of the project's fetch into library lib.
func (h *Handler) statePath(lib int64, id int) string {
	return transfer.PrivateDir(h.o.StateDir).Base(strconv.FormatInt(lib, 10), strconv.Itoa(id)) + ".json"
}

// loadState reads the state of the project's fetch into library lib, nil when
// there is none. One that can't be read, that names another library, or whose
// paths aren't clean library paths, goes: the run plans afresh. So does one of
// another server (the Bridge was paired with this one since): its library id
// means nothing here, so its staging folder is left where it is, never looked
// for in this server's same-numbered library. serverID is the server the
// reader works for: a job's own (jobs.Job.KeyServer), else the pairing's.
func (h *Handler) loadState(lib int64, id int, serverID string) *state {
	p := h.statePath(lib, id)
	b, err := os.ReadFile(p) //nolint:gosec // G304: the job's own state in the Bridge's data
	if err != nil {
		return nil
	}
	var st state
	if json.Unmarshal(b, &st) != nil || st.LibraryID != lib || !st.valid() {
		_ = os.Remove(p)
		return nil
	}
	if cur := serverID; st.ServerID != "" && cur != "" && st.ServerID != cur {
		h.o.Log.Warn("librivox: a fetch's state is another server's; its staging folder is left as it is",
			"library", lib, "librivox_id", id, "server", st.ServerID, "path", st.staging(id))
		_ = os.Remove(p)
		return nil
	}
	return &st
}

// serverUnknown reports whether st names a server while serverID (the server
// the reader works for, as loadState's) isn't known: the sweep, which removes
// things, leaves such a state alone (Abandon asks more: the state's server
// and job are the job's own), as they can't tell it is this
// server's (bridge#71). A run still resumes from it (loadState's lenient read).
func serverUnknown(st *state, serverID string) bool {
	return st.ServerID != "" && serverID == ""
}

// touchState marks the state of the project's fetch into library lib as in
// use now (nothing when there is none): the sweep goes by the state's age, and
// a run that resumes without saving (everything downloaded, meta down again),
// or one that ends long after its last save (parked for days), must not look
// abandoned. A run touches it when it loads it and when it ends.
func (h *Handler) touchState(lib int64, id int, now time.Time) {
	_ = os.Chtimes(h.statePath(lib, id), now, now)
}

// valid reports whether the state's paths are clean library paths: Parent,
// and the destination directly in it.
func (st *state) valid() bool {
	if st.LibraryID <= 0 {
		return false
	}
	parent := st.Parent
	if parent == "" {
		parent = "."
	} else if clean, err := transfer.CleanRel(parent); err != nil || clean != parent {
		return false
	}
	clean, err := transfer.CleanRel(st.Dest)
	return err == nil && clean == st.Dest && path.Dir(st.Dest) == parent
}

// saveState writes the state of the project's fetch into st's library.
func (h *Handler) saveState(id int, st *state) error {
	if err := transfer.PrivateDir(h.o.StateDir).Create(); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(h.statePath(st.LibraryID, id), b, 0o600)
}

// dropState removes the state of the project's fetch into library lib.
func (h *Handler) dropState(lib int64, id int) {
	if err := os.Remove(h.statePath(lib, id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.o.Log.Warn("librivox: removing a fetch's state failed", "library", lib, "librivox_id", id, "err", err)
	}
}
