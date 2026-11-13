package librivoxjob

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/transfer"
)

// state is a fetch's plan and progress, kept in the Bridge's data
// (<StateDir>/<librivox_id>.json) from the moment it picks the folder: a
// resumed run (a restart, a lost claim handed out again) goes on with the same
// staging folder and destination instead of planning again against a library
// that changed meanwhile, Abandon finds the staging folder to remove, and once
// the folder is placed it is the placement marker (a failed rescan retries
// from the rescan, decision 6).
type state struct {
	LibraryID int64 `json:"library_id"`
	// Parent is the folder the book goes in ("" for the library's root), and
	// MadeParent whether this fetch created it (removed again, when empty, with
	// the staging folder).
	Parent     string `json:"parent"`
	MadeParent bool   `json:"made_parent,omitempty"`
	// Staging is <parent>/.audiosilo-librivox-<id>, Dest the book's folder.
	Staging string `json:"staging"`
	Dest    string `json:"dest"`
	// Files is each section's partial download: the strong ETag and the
	// size its response stated, so a resume asks only for the rest.
	Files map[string]partState `json:"files,omitempty"`
	// Placed: the staging folder became Dest, holding Count files of Bytes.
	Placed bool  `json:"placed,omitempty"`
	Count  int   `json:"count,omitempty"`
	Bytes  int64 `json:"bytes,omitempty"`
}

// partState is one section's partial download.
type partState struct {
	ETag  string `json:"etag,omitempty"`
	Total int64  `json:"total,omitempty"`
}

// stagingName is the staging folder's name: a dot folder the server's scan
// never indexes, beside the book's destination so placing it is one rename.
func stagingName(id int) string { return ".audiosilo-librivox-" + strconv.Itoa(id) }

// statePath is the project's state file.
func (h *Handler) statePath(id int) string {
	return filepath.Join(h.o.StateDir, strconv.Itoa(id)+".json")
}

// loadState reads the project's state, nil when there is none. One that
// can't be read, or whose paths aren't this project's staging and a clean
// library path, goes: the run plans afresh.
func (h *Handler) loadState(id int) *state {
	p := h.statePath(id)
	b, err := os.ReadFile(p) //nolint:gosec // G304: the job's own state in the Bridge's data
	if err != nil {
		return nil
	}
	var st state
	if json.Unmarshal(b, &st) != nil || !st.valid(id) {
		_ = os.Remove(p)
		return nil
	}
	return &st
}

// valid reports whether the state's paths are this project's: the staging
// folder and the destination clean library paths, both directly in Parent,
// the staging folder named for the project.
func (st *state) valid(id int) bool {
	if st.LibraryID <= 0 || path.Base(st.Staging) != stagingName(id) {
		return false
	}
	parent := st.Parent
	if parent == "" {
		parent = "."
	} else if clean, err := transfer.CleanRel(parent); err != nil || clean != parent {
		return false
	}
	if clean, err := transfer.CleanRel(st.Dest); err != nil || clean != st.Dest || path.Dir(st.Dest) != parent {
		return false
	}
	// The staging folder's own name is a dot name, which CleanRel refuses:
	// only its folder is checked (it is Parent's).
	return path.Dir(st.Staging) == parent
}

// saveState writes the project's state.
func (h *Handler) saveState(id int, st *state) error {
	if err := os.MkdirAll(h.o.StateDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(h.statePath(id), b, 0o600)
}

// dropState removes the project's state.
func (h *Handler) dropState(id int) {
	if err := os.Remove(h.statePath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		h.o.Log.Warn("librivox: removing a fetch's state failed", "librivox_id", id, "err", err)
	}
}
