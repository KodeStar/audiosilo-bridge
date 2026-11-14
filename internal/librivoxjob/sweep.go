package librivoxjob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StagingMaxAge is how long the sweep keeps a fetch's staging folder and state
// after the state was last used (audible.import's PartialMaxAge): a lost claim
// the server never handed out again, a run that failed while meta or
// archive.org was down and was never retried.
const StagingMaxAge = 7 * 24 * time.Hour

// parseStateName reads <library_id>-<librivox_id>.json.
func parseStateName(name string) (key, bool) {
	stem, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return key{}, false
	}
	l, i, ok := strings.Cut(stem, "-")
	if !ok {
		return key{}, false
	}
	lib, err1 := strconv.ParseInt(l, 10, 64)
	id, err2 := strconv.Atoi(i)
	if err1 != nil || err2 != nil || lib <= 0 || id <= 0 {
		return key{}, false
	}
	return key{lib: lib, id: id}, true
}

// Sweep removes what abandoned fetches left (the app runs it after a mapping
// check, when the libraries' mounts are known): a state unused for
// StagingMaxAge goes with its staging folder and the author folder it made,
// when empty. open is the params of the librivox.fetch jobs the runner still
// holds (its running rows): one waiting for its lane after a restart, or one a
// refused token stopped, has no run here yet and resumes from its state
// however old, so it is never swept. A fetch running here is left alone; a
// placed book stays (only its state goes); a state of another server goes
// without its library being touched; one whose library isn't a checked,
// writable mapping now, or whose staging folder can't be removed, is kept for
// a later sweep. Any other regular file in the state folder as old (a temp
// file a crash left) goes.
func (h *Handler) Sweep(now time.Time, open []json.RawMessage) {
	entries, err := os.ReadDir(h.o.StateDir)
	if err != nil {
		return
	}
	held := map[key]bool{}
	for _, raw := range open {
		if p, err := ParseParams(raw); err == nil {
			held[key{lib: p.LibraryID, id: p.LibrivoxID}] = true
		}
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < StagingMaxAge {
			continue
		}
		k, ok := parseStateName(e.Name())
		if !ok || e.Name() != filepath.Base(h.statePath(k.lib, k.id)) {
			if err := os.Remove(filepath.Join(h.o.StateDir, e.Name())); err != nil {
				h.o.Log.Warn("librivox: sweeping the state folder failed", "name", e.Name(), "err", err)
			}
			continue
		}
		if !held[k] {
			h.sweepOne(k, now)
		}
	}
}

// sweepOne is Sweep for one old state.
func (h *Handler) sweepOne(k key, now time.Time) {
	if !h.begin(k) {
		return
	}
	defer h.end(k) // a run of this fetch waits meanwhile (await)

	// The age again, under the reservation: a run may have used the state (and
	// ended) since Sweep listed it.
	if info, err := os.Stat(h.statePath(k.lib, k.id)); err != nil || now.Sub(info.ModTime()) < StagingMaxAge {
		return
	}
	st := h.loadState(k.lib, k.id) // gone already when invalid or another server's
	if st == nil {
		return
	}
	if !st.Placed {
		mount, ok := h.writableMount(k.lib)
		if !ok || !h.removeStaging(mount, k.id, st) {
			return // the state stays, so a later sweep finds the folder again
		}
	}
	h.o.Log.Info("librivox: swept an abandoned fetch", "library", k.lib, "librivox_id", k.id, "placed", st.Placed)
	h.dropState(k.lib, k.id)
}

// writableMount is the books library's mount while it is a checked, writable
// mapping there (mapped's rule, without waiting).
func (h *Handler) writableMount(id int64) (string, bool) {
	lib, ok := h.o.Libraries.Library(id)
	if !ok || lib.LibraryKind() != "books" {
		return "", false
	}
	mount, _ := h.o.Mount(lib.ID, lib.Root)
	st, checked := h.o.Libraries.Status(lib.ID)
	if mount == "" || !checked || st.MountPath != mount || !st.Mapped || !st.Writable {
		return "", false
	}
	return mount, true
}
