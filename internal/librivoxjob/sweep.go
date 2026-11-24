package librivoxjob

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/transfer"
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
	info, err := os.Stat(h.statePath(k.lib, k.id))
	if err != nil || now.Sub(info.ModTime()) < StagingMaxAge {
		return
	}
	serverID := h.o.ServerID()               // no job's: the pairing's
	st := h.loadState(k.lib, k.id, serverID) // gone already when invalid or another server's
	if st == nil || serverUnknown(st, serverID) {
		return
	}
	if !st.Placed {
		mount, ok := h.writableMount(k.lib)
		if !ok && h.mappingGone(k.lib) {
			h.endUnmapped(k, st, now)
			return
		}
		if !ok || !h.removeStaging(mount, k.id, st) {
			// The state stays, so a later sweep finds the folder again.
			h.forgetUnmapped(k, st, info.ModTime())
			return
		}
	}
	h.o.Log.Info("librivox: swept an abandoned fetch", "library", k.lib, "librivox_id", k.id, "placed", st.Placed)
	h.dropState(k.lib, k.id)
}

// mappingGone reports whether library id's mapping is gone: the server no
// longer lists the library, or its last check found it unmapped here with a
// definite look, at the mount the library has now: its folder answers, holds
// something and isn't the library's, or no mapping is configured for it here
// any more and nothing is at the server's own path (MountFor's fallback). A
// library that is mapped (writable or not), not checked yet, whose state no
// answered check backs (the registry's Status false: a check the server
// didn't answer, a state too old), checked at another folder, or whose folder
// doesn't show it is there (missing while a mapping is configured,
// unreadable, not answering, or empty: a share not mounted now leaves an
// empty folder at its mount point, as mapping's proveKept says), isn't gone
// (bridge#65). An empty folder at the server's own path isn't gone either: a
// compose stack mounting the libraries at the same path in both containers
// configures no mapping at all.
func (h *Handler) mappingGone(id int64) bool {
	lib, ok := h.o.Libraries.Library(id)
	if !ok {
		return true
	}
	st, checked := h.o.Libraries.Status(id)
	if !checked || st.Mapped {
		return false
	}
	mount, configured := h.o.Mount(lib.ID, lib.Root)
	if mount != st.MountPath {
		return false // the check was of another folder
	}
	held, err := fsutil.LookWithin("librivox-holds", mount, mapping.LookTimeout, holdsAnything)
	if !configured && errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return err == nil && held
}

// holdsAnything reports whether the folder dir lists at least one entry
// (reading no more than one); a file there is an error, as is a folder that
// can't be read.
func holdsAnything(dir string) (bool, error) {
	f, err := os.Open(dir) //nolint:gosec // G304: a library's mount, only listed
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	return len(names) > 0, err
}

// forgetUnmapped clears the time an unplaced fetch's library was found
// unmapped (endUnmapped) once a check that stands finds it mapped again,
// writable or not, so a later unmapping counts from its own start (bridge#65).
// The state keeps its age: this is no use of it.
func (h *Handler) forgetUnmapped(k key, st *state, used time.Time) {
	if st.Unmapped.IsZero() {
		return
	}
	if ls, checked := h.o.Libraries.Status(k.lib); !checked || !ls.Mapped {
		return
	}
	st.Unmapped = time.Time{}
	if err := h.saveState(k.id, st); err != nil {
		h.o.Log.Warn("librivox: clearing a fetch's unmapped library failed", "library", k.lib, "librivox_id", k.id, "err", err)
		return
	}
	h.touchState(k.lib, k.id, used)
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
// it). A library that stops answering while one of its strays is looked at or
// removed is left for the next walk (sweepStray). bridge#71.
func (h *Handler) sweepStrays(now time.Time, held map[key]bool) {
	for _, ls := range h.o.Libraries.Statuses() {
		mount, ok := h.writableMount(ls.LibraryID)
		if !ok {
			continue
		}
		for _, s := range strayCandidates(mount, h.o.strayWalkMax) {
			if !h.sweepStray(mount, ls.LibraryID, s, now, held) {
				break
			}
		}
	}
}

// strayLookWait bounds each folder read of the walk for stray staging folders
// (fsutil.ReadDirWithin): a library on a mount that hangs is left for the next
// walk rather than holding the sweep (strayWorkWait once a stray is found).
const strayLookWait = 10 * time.Second

// strayWalkTime bounds the walk of one library's author folders for stray
// staging folders, all of its folder reads together: a library of many author
// folders on a slow mount is walked only so far, the rest left for the next
// walk, which takes them in another random order (bridge#65).
const strayWalkTime = 2 * time.Minute

// stray is a staging-named folder the walk found: rel in the library, the
// project id its name carries.
type stray struct {
	rel string
	id  int
}

// strayCandidates are the staging-named folders in the library's top folder
// (mount) and in each folder directly in it. Each read is bounded by
// strayLookWait, and the author folders' reads together by budget, in an
// order picked at random each walk, so a walk cut short reaches others next
// time; the first read that doesn't answer ends the walk of this library. A
// symlink is never followed (its entry isn't a folder).
func strayCandidates(mount string, budget time.Duration) []stray {
	until := time.Now().Add(budget)
	top, err := fsutil.ReadDirWithin(mount, strayLookWait)
	if err != nil {
		return nil
	}
	var out []stray
	var authors []string
	for _, e := range top {
		if !e.IsDir() {
			continue
		}
		if id, ok := stagingID(e.Name()); ok {
			out = append(out, stray{rel: e.Name(), id: id})
		} else if !strings.HasPrefix(e.Name(), ".") {
			authors = append(authors, e.Name())
		}
	}
	rand.Shuffle(len(authors), func(i, j int) { authors[i], authors[j] = authors[j], authors[i] }) //nolint:gosec // G404: the walk's order, no secret
	for _, name := range authors {
		left := time.Until(until)
		if left <= 0 {
			return out
		}
		inner, err := fsutil.ReadDirWithin(filepath.Join(mount, name), min(strayLookWait, left))
		if errors.Is(err, fsutil.ErrStatTimeout) {
			return out
		}
		for _, f := range inner {
			if id, ok := stagingID(f.Name()); ok && f.IsDir() {
				out = append(out, stray{rel: path.Join(name, f.Name()), id: id})
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

// strayWorkWait bounds the sweep's wait for the look at one stray staging
// folder and its removal (sweepStray).
const strayWorkWait = time.Minute

// sweepStray is sweepStrays for one staging folder s of library lib, at mount.
// The project is held in every library first (beginProject), so no fetch of it
// starts anywhere between the look that finds it unknown (projectKnown) and
// the removal: another library's mount may reach this folder (bridge#71). The
// mapping is checked again then: the walk may have taken long. The look and
// the removal (clearStray) run aside, and the sweep waits strayWorkWait for
// them: on a mount that hangs after the walk listed it the sweep goes on
// (answered false, for the caller to leave this library's other strays to the
// next walk), and the removal is given up: it removes nothing more once the
// mount answers, and the project is free again at once, so a fetch of it runs
// rather than failing as already running after awaitPolls (bridge#71). Only a
// removal already under way as the wait ended may still land; a fetch never
// places a book missing a section (sweep looks again).
func (h *Handler) sweepStray(mount string, lib int64, s stray, now time.Time, held map[key]bool) (answered bool) {
	if !h.beginProject(s.id) {
		return true
	}
	if m, ok := h.writableMount(lib); !ok || m != mount || h.projectKnown(s.id, held) {
		h.endProject(s.id)
		return true
	}
	// released is set once, by whichever ends first: the look and removal, or
	// the wait (which gives the removal up: clearStray stops once it is set).
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			h.endProject(s.id)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if h.o.strayEnded != nil {
				h.o.strayEnded(s.rel)
			}
		}()
		defer release()
		h.clearStray(mount, lib, s.rel, now, &released)
	}()
	t := time.NewTimer(h.o.strayWait)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		if !released.CompareAndSwap(false, true) {
			<-done // the look and removal ended as the wait did: not a hang
			return true
		}
		h.endProject(s.id)
		h.o.Log.Warn("librivox: a library stopped answering at a stray staging folder; its others wait for the next walk",
			"library", lib, "path", s.rel)
		return false
	}
}

// beginProject holds project id in every library for the sweep of stray
// staging folders, false while a fetch of it is held here in any library (a
// run, Abandon, the sweep of its state) or the sweep holds it already. Until
// endProject no fetch of the project starts (begin): a run waits for it
// (await), Abandon and the sweep of states leave it to their next turn.
func (h *Handler) beginProject(id int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sweeping[id] {
		return false
	}
	for o := range h.active {
		if o.id == id {
			return false
		}
	}
	h.sweeping[id] = true
	return true
}

// endProject gives project id up again (beginProject).
func (h *Handler) endProject(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sweeping, id)
}

// clearStray removes the staging folder rel of library lib, through the
// mount's root, when it holds only what a fetch writes there, nothing in it
// changed for StagingMaxAge. The folder is opened once and looked at and
// emptied through that (a root of its own), so what is removed is in the
// folder that was looked at, whatever its name leads to by then. It removes
// what it saw, each by its name, a folder only when empty, never anything
// else: a file put there since the look stays, and the folder with it. A
// folder that isn't the one looked at any more (renamed away, a symlink or
// another folder in its place) is left, and so is everything once stopped
// (the sweep gave up waiting for it, sweepStray).
func (h *Handler) clearStray(mount string, lib int64, rel string, now time.Time, stopped *atomic.Bool) {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	dir, err := root.Lstat(rel)
	if err != nil || !dir.IsDir() {
		return
	}
	in, err := root.OpenRoot(rel)
	if err != nil {
		return
	}
	defer func() { _ = in.Close() }()
	if !sameFolder(in, ".", dir) { // a symlink took its name since the Lstat
		return
	}
	seen, newest, only := stagingContents(in)
	if !only {
		h.o.Log.Info("librivox: left a staging-named folder not shown to hold only a fetch's files", "library", lib, "path", rel)
		return
	}
	if dir.ModTime().After(newest) {
		newest = dir.ModTime()
	}
	if now.Sub(newest) < StagingMaxAge {
		return
	}
	if h.o.strayLooked != nil {
		h.o.strayLooked(rel)
	}
	// Again now: the look may have taken long on a mount that hung.
	if stopped.Load() {
		return
	}
	if m, ok := h.writableMount(lib); !ok || m != mount || !sameFolder(root, rel, dir) {
		return
	}
	var failed error
	for _, name := range seen {
		if stopped.Load() {
			return
		}
		if err := in.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) && failed == nil {
			failed = err
		}
	}
	if failed == nil {
		// The folder itself goes by its name in the library, so only while that
		// name is still it (and only empty), and not once stopped meanwhile
		// (asked after the look, which a hung mount may hold past the wait).
		if !sameFolder(root, rel, dir) || stopped.Load() {
			return
		}
		failed = root.Remove(rel)
	}
	if failed != nil {
		h.o.Log.Warn("librivox: removing a stray staging folder failed", "library", lib, "path", rel, "err", failed)
		return
	}
	h.o.Log.Info("librivox: swept a staging folder no fetch names", "library", lib, "path", rel)
}

// sameFolder reports whether name in root is still the folder dir (not
// following a symlink at name).
func sameFolder(root *os.Root, name string, dir fs.FileInfo) bool {
	at, err := root.Lstat(name)
	return err == nil && os.SameFile(dir, at)
}

// projectKnown reports whether a fetch here knows project id in any library:
// a state (whichever folder it names: another library's mount may reach this
// one, and a mount may list a name in another form than the state holds) or a
// job the runner holds. A run is never out: the caller holds the project
// (beginProject), which no run holds meanwhile and none starts while it does,
// so the answer stands until the caller gives the project up. A state folder
// that can't be read counts as knowing it.
func (h *Handler) projectKnown(id int, held map[key]bool) bool {
	for o := range held {
		if o.id == id {
			return true
		}
	}
	entries, err := os.ReadDir(h.o.StateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	} else if err != nil {
		return true
	}
	for _, e := range entries {
		if o, ok := parseStateName(e.Name()); ok && o.id == id {
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

// stagingContents reports whether the staging folder (in, a root at it) holds
// only what a fetch writes there: regular files named as a fetch names them
// (fetchFileName), and the .audiosilo/ folder holding only the marker (and
// its write's temp). It returns what it saw, staging-relative, a folder after
// what it holds, and the newest change among them. Any other entry (a folder,
// a symlink, a file of another name) is a no, and so is anything that can't
// be looked at.
func stagingContents(in *os.Root) (seen []string, newest time.Time, only bool) {
	sub := path.Dir(markerRel)
	var look func(dir string) bool
	look = func(dir string) bool {
		entries, err := fs.ReadDir(transfer.RootFS(in), dir)
		if err != nil {
			return false
		}
		for _, e := range entries {
			at := path.Join(dir, e.Name())
			info, err := in.Lstat(at)
			switch {
			case err != nil:
				return false
			case dir == "." && e.Name() == sub && info.IsDir():
				if !look(sub) {
					return false
				}
			case !info.Mode().IsRegular() || !fetchFileName(e.Name(), dir == "."):
				return false
			}
			seen = append(seen, at)
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		return true
	}
	if !look(".") {
		return nil, time.Time{}, false
	}
	return seen, newest, true
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
