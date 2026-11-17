package librivoxjob

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
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
// removes the staging folders of projects no fetch here knows (sweepStrays).
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

// sweepStrays removes the staging folders of projects no fetch here knows (a
// state ended by endUnmapped, another server's, one lost with the Bridge's
// data) unchanged for StagingMaxAge, in every checked, writable books library.
// A staging folder sits in the library's root or in an author folder
// (destination), so only those two levels are looked at. Only a folder named
// exactly as a fetch names its staging folder goes, and only while it holds
// nothing but what a fetch writes there (stagingContents). A project with a
// state in any library, a run here or a job the runner holds is left alone
// (projectKnown); the author folder is kept (no state says the fetch made
// it). bridge#71.
func (h *Handler) sweepStrays(now time.Time, held map[key]bool) {
	for _, ls := range h.o.Libraries.Statuses() {
		mount, ok := h.writableMount(ls.LibraryID)
		if !ok {
			continue
		}
		strays := strayCandidates(mount)
		if len(strays) == 0 {
			continue
		}
		root, err := os.OpenRoot(mount)
		if err != nil {
			continue
		}
		for _, s := range strays {
			h.sweepStray(root, mount, ls.LibraryID, s, now, held)
		}
		_ = root.Close()
	}
}

// strayLookWait bounds each folder read of the walk for stray staging folders
// (fsutil.ReadDirWithin): a library on a mount that hangs is left for the next
// walk rather than holding the sweep.
const strayLookWait = 10 * time.Second

// stray is a staging-named folder the walk found: rel in the library, the
// project id its name carries.
type stray struct {
	rel string
	id  int
}

// strayCandidates are the staging-named folders in the library's top folder
// (mount) and in each folder directly in it. Each read is bounded by
// strayLookWait; the first that doesn't answer ends the walk of this library.
// A symlink is never followed (its entry isn't a folder).
func strayCandidates(mount string) []stray {
	top, err := fsutil.ReadDirWithin(mount, strayLookWait)
	if err != nil {
		return nil
	}
	var out []stray
	for _, e := range top {
		if !e.IsDir() {
			continue
		}
		if id, ok := stagingID(e.Name()); ok {
			out = append(out, stray{rel: e.Name(), id: id})
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		inner, err := fsutil.ReadDirWithin(filepath.Join(mount, e.Name()), strayLookWait)
		if errors.Is(err, fsutil.ErrStatTimeout) {
			return out
		}
		for _, f := range inner {
			if id, ok := stagingID(f.Name()); ok && f.IsDir() {
				out = append(out, stray{rel: path.Join(e.Name(), f.Name()), id: id})
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

// sweepStray is sweepStrays for one staging folder s of library lib, at mount
// (root). The mapping is checked again first: the walk may have taken long.
func (h *Handler) sweepStray(root *os.Root, mount string, lib int64, s stray, now time.Time, held map[key]bool) {
	k := key{lib: lib, id: s.id}
	if !h.begin(k) {
		return
	}
	defer h.end(k)
	if m, ok := h.writableMount(lib); !ok || m != mount || h.projectKnown(k, held) {
		return
	}
	info, err := root.Lstat(s.rel)
	if err != nil || !info.IsDir() {
		return
	}
	newest, only := stagingContents(filepath.Join(mount, filepath.FromSlash(s.rel)))
	if !only {
		h.o.Log.Info("librivox: left a staging-named folder not shown to hold only a fetch's files", "library", lib, "path", s.rel)
		return
	}
	if info.ModTime().After(newest) {
		newest = info.ModTime()
	}
	if now.Sub(newest) < StagingMaxAge {
		return
	}
	if err := root.RemoveAll(s.rel); err != nil {
		h.o.Log.Warn("librivox: removing a stray staging folder failed", "library", lib, "path", s.rel, "err", err)
		return
	}
	h.o.Log.Info("librivox: swept a staging folder no fetch names", "library", lib, "path", s.rel)
}

// projectKnown reports whether a fetch here knows project k.id in any library:
// a state (whichever folder it names: another library's mount may reach this
// one, and a mount may list a name in another form than the state holds), a
// run (k itself is the caller's own reservation), or a job the runner holds.
// A state folder that can't be read counts as knowing it.
func (h *Handler) projectKnown(k key, held map[key]bool) bool {
	for o := range held {
		if o.id == k.id {
			return true
		}
	}
	h.mu.Lock()
	for o := range h.active {
		if o.id == k.id && o != k {
			h.mu.Unlock()
			return true
		}
	}
	h.mu.Unlock()
	entries, err := os.ReadDir(h.o.StateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	} else if err != nil {
		return true
	}
	for _, e := range entries {
		if o, ok := parseStateName(e.Name()); ok && o.id == k.id {
			return true
		}
	}
	return false
}

// sectionFile matches a section's file a fetch names (sectionNames: "NN -
// <title>.<ext>"), or its partial download.
var sectionFile = func() *regexp.Regexp {
	exts := make([]string, 0, len(audioExts))
	for ext := range audioExts {
		exts = append(exts, regexp.QuoteMeta(strings.TrimPrefix(ext, ".")))
	}
	slices.Sort(exts)
	return regexp.MustCompile(`^[0-9]{2,} - .+\.(?:` + strings.Join(exts, "|") + `)(?:` + regexp.QuoteMeta(partSuffix) + `)?$`)
}()

// fetchFileName reports whether name is one a fetch gives a file in its
// staging folder (top: a section or its partial, the cover, an atomic write's
// temp) or in the staging folder's .audiosilo/ (the marker, its write's temp).
func fetchFileName(name string, top bool) bool {
	switch {
	case strings.HasPrefix(name, fsutil.RootTempPrefix):
		return true
	case !top:
		return name == path.Base(markerRel)
	}
	return name == coverName || sectionFile.MatchString(name)
}

// stagingContents reports whether the staging folder dir (its path on the
// mount) holds only what a fetch writes there: regular files named as a fetch
// names them (fetchFileName), and the .audiosilo/ folder holding only the
// marker (and its write's temp); and the newest change among them. Any other
// entry (a folder, a symlink, a file of another name) is a no, and so is a
// folder read that fails or doesn't answer within strayLookWait.
func stagingContents(dir string) (newest time.Time, only bool) {
	return fetchFilesIn(dir, true)
}

func fetchFilesIn(dir string, top bool) (newest time.Time, only bool) {
	entries, err := fsutil.ReadDirWithin(dir, strayLookWait)
	if err != nil {
		return time.Time{}, false
	}
	for _, e := range entries {
		switch {
		case top && e.IsDir() && e.Name() == path.Dir(markerRel):
			inner, ok := fetchFilesIn(filepath.Join(dir, e.Name()), false)
			if !ok {
				return time.Time{}, false
			}
			if inner.After(newest) {
				newest = inner
			}
		case !e.Type().IsRegular() || !fetchFileName(e.Name(), top):
			return time.Time{}, false
		}
		info, err := e.Info()
		if err != nil {
			return time.Time{}, false
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest, true
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
