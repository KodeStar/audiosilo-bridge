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
// (<StateDir>/<library_id>-<librivox_id>.json, one per library and project, so
// fetches of one project into two libraries never touch each other's staging)
// from the moment it picks the folder: a
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
	// Dest is the book's folder, directly in Parent (as the staging folder is).
	Dest string `json:"dest"`
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

// staging is the project's staging folder: <parent>/.audiosilo-librivox-<id>.
func (st *state) staging(id int) string { return path.Join(st.Parent, stagingName(id)) }

// statePath is the state file of the project's fetch into library lib.
func (h *Handler) statePath(lib int64, id int) string {
	return filepath.Join(h.o.StateDir, strconv.FormatInt(lib, 10)+"-"+strconv.Itoa(id)+".json")
}

// loadState reads the state of the project's fetch into library lib, nil when
// there is none. One that can't be read, that names another library, or whose
// paths aren't clean library paths, goes: the run plans afresh.
func (h *Handler) loadState(lib int64, id int) *state {
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
	return &st
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
	if err := os.MkdirAll(h.o.StateDir, 0o700); err != nil {
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
