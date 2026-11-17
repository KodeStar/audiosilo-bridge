package librivoxjob

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
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
// without its library being touched, and one naming a server while the paired
// server's id isn't known yet is left for a later sweep; one whose library
// isn't a checked, writable mapping now, or whose staging folder can't be
// removed, is kept for a later sweep, but no longer than StagingMaxAge once
// its library's mapping is gone (sweepOne). Any other regular file in the
// state folder as old (a temp file a crash left) goes. Once a day it also
// removes the staging folders no state names (sweepStrays).
func (h *Handler) Sweep(now time.Time, open []json.RawMessage) {
	held := map[key]bool{}
	for _, raw := range open {
		if p, err := ParseParams(raw); err == nil {
			held[key{lib: p.LibraryID, id: p.LibrivoxID}] = true
		}
	}
	h.sweepStates(now, held)
	h.mu.Lock()
	due := now.Sub(h.strayWalk) >= strayWalkEvery
	if due {
		h.strayWalk = now
	}
	h.mu.Unlock()
	if due {
		h.sweepStrays(now, held)
	}
}

// sweepStates is Sweep over the state folder.
func (h *Handler) sweepStates(now time.Time, held map[key]bool) {
	entries, err := os.ReadDir(h.o.StateDir)
	if err != nil {
		return
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
	if st == nil || h.serverUnknown(st) {
		return
	}
	if !st.Placed {
		mount, ok := h.writableMount(k.lib)
		if !ok && h.mappingGone(k.lib) {
			h.endUnmapped(k, st, now)
			return
		}
		if !ok || !h.removeStaging(mount, k.id, st) {
			return // the state stays, so a later sweep finds the folder again
		}
	}
	h.o.Log.Info("librivox: swept an abandoned fetch", "library", k.lib, "librivox_id", k.id, "placed", st.Placed)
	h.dropState(k.lib, k.id)
}

// mappingGone reports whether library id's mapping is gone: the server no
// longer lists the library, or its last check found it unmapped here. A
// library that is mapped but not writable now, or not checked yet, isn't gone.
func (h *Handler) mappingGone(id int64) bool {
	if _, ok := h.o.Libraries.Library(id); !ok {
		return true
	}
	st, checked := h.o.Libraries.Status(id)
	return checked && !st.Mapped
}

// endUnmapped is sweepOne for an unplaced fetch whose library's mapping is
// gone: the first sweep that finds it so records when (the state saved anew,
// so the sweep looks again StagingMaxAge later), and a sweep StagingMaxAge
// after that drops the state, its staging folder left where it is (the sweep
// of stray staging folders removes it if the library comes back). A state
// whose library never returns is kept no longer (bridge#71).
func (h *Handler) endUnmapped(k key, st *state, now time.Time) {
	if st.Unmapped.IsZero() {
		st.Unmapped = now
		if err := h.saveState(k.id, st); err != nil {
			h.o.Log.Warn("librivox: recording a fetch's unmapped library failed", "library", k.lib, "librivox_id", k.id, "err", err)
			return
		}
		h.touchState(k.lib, k.id, now)
		return
	}
	if now.Sub(st.Unmapped) < StagingMaxAge {
		return
	}
	h.o.Log.Info("librivox: ended a fetch whose library's mapping is gone", "library", k.lib, "librivox_id", k.id,
		"path", st.staging(k.id), "unmapped_since", st.Unmapped)
	h.dropState(k.lib, k.id)
}

// strayWalkEvery is how often Sweep looks for stray staging folders: each look
// lists the top two levels of every writable library.
const strayWalkEvery = 24 * time.Hour

// sweepStrays removes the staging folders no state names (a state ended by
// endUnmapped, another server's, one lost with the Bridge's data) whose
// folder is older than StagingMaxAge, in every checked, writable books
// library. A staging folder sits in the library's root or in an author folder
// (destination), so only those two levels are looked at. A fetch running here
// or held by the runner is left alone, and so is the staging folder its state
// names (sweepOne's); the author folder is kept (no state says the fetch
// made it). bridge#71.
func (h *Handler) sweepStrays(now time.Time, held map[key]bool) {
	for _, ls := range h.o.Libraries.Statuses() {
		mount, ok := h.writableMount(ls.LibraryID)
		if !ok {
			continue
		}
		root, err := os.OpenRoot(mount)
		if err != nil {
			continue
		}
		for _, rel := range strayCandidates(root) {
			h.sweepStray(root, ls.LibraryID, rel, now, held)
		}
		_ = root.Close()
	}
}

// strayCandidates are the staging-named folders in root's top folder and in
// each folder directly in it.
func strayCandidates(root *os.Root) []string {
	top, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range top {
		if !e.IsDir() {
			continue
		}
		if _, ok := stagingID(e.Name()); ok {
			out = append(out, e.Name())
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		inner, err := fs.ReadDir(root.FS(), e.Name())
		if err != nil {
			continue
		}
		for _, f := range inner {
			if _, ok := stagingID(f.Name()); ok && f.IsDir() {
				out = append(out, path.Join(e.Name(), f.Name()))
			}
		}
	}
	return out
}

// stagingID is the LibriVox project id a staging folder's name carries.
func stagingID(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, stagingPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(rest)
	return id, err == nil && id > 0 && stagingName(id) == name
}

// sweepStray is sweepStrays for one staging folder rel of library lib.
func (h *Handler) sweepStray(root *os.Root, lib int64, rel string, now time.Time, held map[key]bool) {
	id, _ := stagingID(path.Base(rel))
	k := key{lib: lib, id: id}
	if held[k] || !h.begin(k) {
		return
	}
	defer h.end(k)
	if st := h.loadState(lib, id); st != nil && st.staging(id) == rel {
		return
	}
	info, err := root.Lstat(rel)
	if err != nil || !info.IsDir() || now.Sub(info.ModTime()) < StagingMaxAge {
		return
	}
	if err := root.RemoveAll(rel); err != nil {
		h.o.Log.Warn("librivox: removing a stray staging folder failed", "library", lib, "path", rel, "err", err)
		return
	}
	h.o.Log.Info("librivox: swept a staging folder no fetch names", "library", lib, "path", rel)
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
