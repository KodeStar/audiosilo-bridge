// Package librivoxjob runs librivox.fetch (LIBRIVOX-PLAN.md L6): one LibriVox
// recording, read from the community metadata database's LibriVox-shaped feed,
// downloaded section by section from the Internet Archive into a staging
// folder in a books library, placed as Author/Title/NN - Section.mp3 with its
// cover.jpg and .audiosilo/librivox.json, then rescanned.
package librivoxjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/importjob"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/transfer"
)

// Kind is the job kind (its family, "librivox", is the Bridge feature the
// server's console and player gate Add from LibriVox on).
const Kind = "librivox.fetch"

// The steps a fetch reports (progress.step), in order.
const (
	StepManifest = "manifest"
	StepDownload = "download"
	StepCover    = "cover"
	StepPlace    = "place"
	StepRescan   = "rescan"
)

// The codes a failed fetch reports (its result {code, message}); any other
// failure has none, only its error.
const (
	CodeNotWritable        = "not_writable"        // not a checked writable books mapping, or no valid path there
	CodeExists             = "exists"              // the title's folder and its fallback are both taken
	CodeDiskFull           = "disk_full"           // the library is short of room
	CodeNotFound           = "not_found"           // meta has no such project (or it lists no files)
	CodeArchiveUnavailable = "archive_unavailable" // a file isn't on archive.org, or archive.org can't be reached
	CodeMetadataOff        = "metadata_off"        // the Bridge has no metadata base URL
)

// Failure is a failed fetch's result and error: a code from the list above and
// a sentence for the console. It never holds a URL.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (f *Failure) Error() string { return f.Message }

func fail(code, format string, args ...any) *Failure {
	return &Failure{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Params are librivox.fetch's params, built by the server: the LibriVox
// project, the work and recording the server resolved it to in meta, and the
// books library. Unknown fields are ignored (the server may add some).
type Params struct {
	LibrivoxID  int    `json:"librivox_id"`
	WorkID      string `json:"work_id"`
	RecordingID string `json:"recording_id"`
	LibraryID   int64  `json:"library_id"`
}

// metaID is the shape of a meta work or recording id (a slug).
var metaID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// ParseParams reads and checks the params.
func ParseParams(raw json.RawMessage) (Params, error) {
	var p Params
	if err := json.Unmarshal(raw, &p); err != nil {
		return Params{}, fmt.Errorf("bad params: %w", err)
	}
	switch {
	case p.LibrivoxID <= 0:
		return Params{}, errors.New("bad params: librivox_id must be a LibriVox project id")
	case !metaID.MatchString(p.WorkID) || !metaID.MatchString(p.RecordingID):
		return Params{}, errors.New("bad params: work_id and recording_id must be meta ids")
	case p.LibraryID <= 0:
		return Params{}, errors.New("bad params: library_id is required")
	}
	return p, nil
}

// Result is a done fetch: the book's path as the server indexed it, the
// project, and the files placed (the sections and the cover) with their bytes.
// Adopted (always sent; additive, so an older server ignores it) is true when
// the book's folder already held this project's librivox.json when the fetch
// planned, so the Bridge downloaded and placed nothing and only rescanned it;
// false when this fetch placed the folder (a resume that finds its own
// unrecorded placement included).
type Result struct {
	LibraryID  int64  `json:"library_id"`
	Path       string `json:"path"`
	LibrivoxID int    `json:"librivox_id"`
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
	Adopted    bool   `json:"adopted"`
}

// Marker is .audiosilo/librivox.json, the book's LibriVox identity in its own
// folder (decision 3; the server doesn't read it yet).
type Marker struct {
	Version     int    `json:"version"`
	LibrivoxID  int    `json:"librivox_id"`
	WorkID      string `json:"work_id"`
	RecordingID string `json:"recording_id"`
	ArchiveID   string `json:"archive_id"`
}

// MarkerVersion is librivox.json's version.
const MarkerVersion = 1

// markerRel is the marker inside the book's folder.
const markerRel = ".audiosilo/librivox.json"

// Meta is the metadata client a fetch reads (*metaops.Client).
type Meta interface {
	LibrivoxAudiobook(ctx context.Context, id int) (metaops.LibrivoxBook, error)
	LibrivoxWork(ctx context.Context, workID string) (metaops.LibrivoxWork, error)
}

// Fetcher is the guarded HTTP client the files come through (*netguard.Client).
type Fetcher interface {
	Stream(ctx context.Context, in netguard.StreamRequest) (*netguard.StreamResponse, error)
	Fetch(ctx context.Context, in netguard.Request) (*netguard.Response, error)
}

// Server is the part of the server link a fetch calls (*link.Client).
type Server interface {
	importjob.BookLister
	Rescan(ctx context.Context, libraryID int64, path string) (string, error)
}

// Libraries is the mapping registry a fetch checks (*mapping.Registry).
type Libraries interface {
	Library(id int64) (link.Library, bool)
	Status(id int64) (link.LibraryStatus, bool)
}

// Options wire the handler.
type Options struct {
	Meta      Meta
	Fetcher   Fetcher
	Server    func() Server
	Libraries Libraries
	Mount     mapping.MountFunc
	// StateDir is <data>/librivox: each fetch's plan and partial downloads'
	// validators (state).
	StateDir string
	// ServerID is the paired server's id ("" when unknown), recorded in each
	// state: a state of another server is never acted on.
	ServerID func() string
	Log      *slog.Logger

	// Seams (tests): the free-space reader and the wait between attempts and
	// between looks at the library's mapping.
	Free func(dir string) (int64, error)
	Wait func(ctx context.Context, d time.Duration) error
	// onProgress sees every progress the job reports (tests).
	onProgress func(jobs.Progress)
}

// Handler runs librivox.fetch on the download lane (capacity 1, shared with
// audible.import and an ordinary podcast.fetch): one book at a time.
type Handler struct {
	o Options

	// mu guards active: the fetches running here (Run), which Abandon and
	// Sweep leave alone, and which a second claim of the same fetch can't join.
	mu     sync.Mutex
	active map[key]bool
}

// New returns the handler.
func New(o Options) *Handler {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Free == nil {
		o.Free = fsutil.FreeBytes
	}
	if o.Wait == nil {
		o.Wait = netguard.Wait
	}
	if o.ServerID == nil {
		o.ServerID = func() string { return "" }
	}
	return &Handler{o: o, active: map[key]bool{}}
}

// Kind is librivox.fetch.
func (h *Handler) Kind() string { return Kind }

// Lane is the download lane.
func (h *Handler) Lane() string { return jobs.LaneDownload }

// mappingPoll is how often a job waiting on its library's mapping looks again.
const mappingPoll = time.Second

// Abandon removes the staging folder of a job the runner ended without
// running it (cancelled before it resumed). A lost claim keeps it: the server
// hands the job out again and the next claim resumes it. A placed book stays.
// The state is the job's library's alone, so a fetch of the same project
// into another library is never touched, and this server's alone: another
// server's state is never followed into this server's same-numbered library.
// A fetch of the same project and library running here meanwhile keeps it.
func (h *Handler) Abandon(ctx context.Context, job jobs.Job) {
	if jobs.Lost(ctx) {
		return
	}
	p, err := ParseParams(job.Params)
	if err != nil {
		return
	}
	k := key{lib: p.LibraryID, id: p.LibrivoxID}
	if !h.begin(k) {
		return
	}
	defer h.end(k)
	st := h.loadState(p.LibraryID, p.LibrivoxID)
	if st == nil || st.Placed {
		return
	}
	// The state goes with the folder it names: while the folder can't be
	// reached or removed (the library isn't listed yet, its mount is away), the
	// state stays and the sweep removes both later.
	lib, ok := h.o.Libraries.Library(st.LibraryID)
	if !ok {
		return
	}
	if mount, _ := h.o.Mount(lib.ID, lib.Root); mount != "" && h.removeStaging(mount, p.LibrivoxID, st) {
		h.dropState(st.LibraryID, p.LibrivoxID)
	}
}

// Run fetches the recording: manifest, download, cover, place, rescan.
func (h *Handler) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	p, err := ParseParams(job.Params)
	if err != nil {
		return nil, err
	}
	if job.LibraryID != nil && *job.LibraryID != p.LibraryID {
		return nil, errors.New("bad params: library_id isn't the job's library")
	}
	k := key{lib: p.LibraryID, id: p.LibrivoxID}
	if err := h.await(ctx, k, rep); err != nil {
		return nil, err
	}
	defer h.end(k)
	// The state a run leaves (a stop, a lost claim, meta or archive.org down)
	// was last used now, however long ago the run began or last saved it (one
	// parked for days): the sweep goes by that.
	defer func() { h.touchState(k.lib, k.id, time.Now()) }()
	res, err := h.run(ctx, job, p, rep)
	var f *Failure
	if errors.As(err, &f) {
		return f, f
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// key is one fetch's state: a library and a project.
type key struct {
	lib int64
	id  int
}

// begin marks the fetch of k as held here (a run, an Abandon or the sweep at
// work on it), false when it already is: the sweep and Abandon leave a running
// fetch's staging alone.
func (h *Handler) begin(k key) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active[k] {
		return false
	}
	h.active[k] = true
	return true
}

// end gives the fetch of k up again.
func (h *Handler) end(k key) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.active, k)
}

// awaitPolls bounds how long a run waits for an earlier run of the same fetch
// to end (mappingPoll apart: about two minutes). That run is one ending: the
// server cancels a parked job at once, and the run hears it at its next
// report, so the re-added job can be claimed before it has stopped.
const awaitPolls = 120

// await marks the fetch of k as running here once no other run of it is.
func (h *Handler) await(ctx context.Context, k key, rep *jobs.Reporter) error {
	for poll := 0; !h.begin(k); poll++ {
		if poll == awaitPolls {
			return errors.New("the Bridge is already adding this recording to this library")
		}
		if poll == 0 {
			h.report(rep, jobs.Progress{Step: StepManifest, Message: "Waiting for the Bridge's earlier run of this fetch to end"})
		}
		if err := h.o.Wait(ctx, mappingPoll); err != nil {
			return err
		}
	}
	return nil
}

// fetch is one run's working set.
type fetch struct {
	p     Params
	lib   link.Library
	mount string
	st    *state
	rep   *jobs.Reporter
}

// staging is the fetch's staging folder (library-relative).
func (f *fetch) staging() string { return f.st.staging(f.p.LibrivoxID) }

// openRoot opens the library at its mount for the fetch's writes.
func (f *fetch) openRoot() (*os.Root, error) {
	root, err := os.OpenRoot(f.mount)
	if err != nil {
		return nil, fail(CodeNotWritable, "The Bridge can't open the library at its mount: %v", err)
	}
	return root, nil
}

func (h *Handler) run(ctx context.Context, job jobs.Job, p Params, rep *jobs.Reporter) (res *Result, err error) {
	var srv Server
	if h.o.Server != nil {
		srv = h.o.Server()
	}
	if srv == nil {
		return nil, errors.New("the Bridge isn't paired with a server")
	}
	lib, mount, err := h.mapped(ctx, p.LibraryID, rep, StepManifest, !job.Resumed)
	if err != nil {
		return nil, err
	}
	f := &fetch{p: p, lib: lib, mount: mount, rep: rep, st: h.loadState(lib.ID, p.LibrivoxID)}
	if f.st != nil {
		h.touchState(lib.ID, p.LibrivoxID, time.Now())
	}
	defer func() {
		// A stop the next start resumes, and a lost claim the server hands out
		// again, keep the staging folder and the state; a cancel or a failure
		// before the book was placed removes both. A placed book stays, and its
		// state with it when the rescan failed (the retry goes on from there).
		var keep *keepStaging
		if err == nil || f.st == nil || f.st.Placed || errors.As(err, &keep) {
			return
		}
		if ctx.Err() != nil && (jobs.Resumes(ctx) || jobs.Lost(ctx)) {
			return
		}
		// A folder that can't be removed now (a cancel while the mount is
		// away) keeps its state, for the sweep.
		if h.removeStaging(f.mount, p.LibrivoxID, f.st) {
			h.dropState(lib.ID, p.LibrivoxID)
		}
	}()

	// An earlier run placed the book and then failed (the rescan): while its
	// folder is still this project's, go on from the rescan.
	if f.st != nil {
		h.adopt(f)
	}
	if f.st != nil && f.st.Placed {
		if h.holdsProject(mount, f.st.Dest, p.LibrivoxID) {
			return h.finish(ctx, f, srv)
		}
		h.dropState(lib.ID, p.LibrivoxID)
		f.st = nil
	}

	h.report(rep, jobs.Progress{Step: StepManifest, Message: "Reading the recording from the metadata database"})
	book, work, err := h.manifest(ctx, p, rep)
	if err != nil {
		return nil, err
	}
	names := sectionNames(book.Sections)
	archiveID := archiveIDOf(p, book, work)

	if f.st == nil {
		if err := h.plan(ctx, f, srv, book, work); err != nil {
			return nil, err
		}
		if f.st.Placed {
			return h.finish(ctx, f, srv) // the folder is already this project's
		}
	}
	if err := h.prepare(f); err != nil {
		return nil, err
	}
	if err := h.download(ctx, f, book.Sections, names); err != nil {
		return nil, err
	}
	// The mapping again before the cover's and the marker's writes.
	if err := h.recheck(ctx, f, StepCover); err != nil {
		return nil, err
	}
	h.cover(ctx, f, book)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := h.writeMarker(f, archiveID); err != nil {
		return nil, err
	}
	if err := h.place(ctx, f, names); err != nil {
		return nil, err
	}
	return h.finish(ctx, f, srv)
}

// manifest reads the project's manifest and its work from meta. A work meta
// doesn't have (another meta base than the server's, decision 5) is no
// failure: the project names the folder then.
//
// meta unavailable (a 5xx, a dropped connection) is never terminal: each read
// is tried metaAttempts times, and still unavailable the run fails with a
// keepStaging error, which keeps the staging folder and the state for the
// next attempt. Meta's definite answers end the fetch: a 404 (not_found), or
// a refusal (metaops.ErrRefused), which no retry would change.
func (h *Handler) manifest(ctx context.Context, p Params, rep *jobs.Reporter) (metaops.LibrivoxBook, *metaops.LibrivoxWork, error) {
	metadataOff := func() error {
		return fail(CodeMetadataOff, "The Bridge has no metadata service set (metadata.base_url), so it can't read LibriVox recordings.")
	}
	if h.o.Meta == nil {
		return metaops.LibrivoxBook{}, nil, metadataOff()
	}
	book, err := metaRead(ctx, h, rep, func() (metaops.LibrivoxBook, error) { return h.o.Meta.LibrivoxAudiobook(ctx, p.LibrivoxID) })
	switch {
	case errors.Is(err, metaops.ErrDisabled):
		return book, nil, metadataOff()
	case errors.Is(err, metaops.ErrNotFound):
		return book, nil, fail(CodeNotFound, "The metadata database has no LibriVox recording %d.", p.LibrivoxID)
	case err != nil:
		return book, nil, fmt.Errorf("reading LibriVox recording %d from the metadata database: %w", p.LibrivoxID, err)
	case len(book.Sections) == 0:
		return book, nil, fail(CodeNotFound, "LibriVox recording %d lists no files in the metadata database.", p.LibrivoxID)
	}
	for i, s := range book.Sections {
		if !allowedRaw(s.ListenURL) {
			return book, nil, fail(CodeArchiveUnavailable, "Section %d of LibriVox recording %d isn't a file on archive.org, the only place the Bridge downloads LibriVox from.", i+1, p.LibrivoxID)
		}
	}
	w, err := metaRead(ctx, h, rep, func() (metaops.LibrivoxWork, error) { return h.o.Meta.LibrivoxWork(ctx, p.WorkID) })
	switch {
	case errors.Is(err, metaops.ErrNotFound):
		return book, nil, nil
	case err != nil:
		return book, nil, fmt.Errorf("reading the work of LibriVox recording %d from the metadata database: %w", p.LibrivoxID, err)
	}
	for _, r := range w.Recordings {
		if r.ID == p.RecordingID && r.LibrivoxID > 0 && r.LibrivoxID != p.LibrivoxID {
			return book, nil, fail(CodeNotFound, "The metadata database's recording %s is LibriVox recording %d now, not %d: search again.",
				p.RecordingID, r.LibrivoxID, p.LibrivoxID)
		}
	}
	return book, &w, nil
}

// metaAttempts bounds one meta read while meta is unavailable; metaWait is
// the first wait between two, doubled each time (5 s, 10 s, 20 s).
const (
	metaAttempts = 4
	metaWait     = 5 * time.Second
)

// keepStaging is a run's failure that leaves the staging folder and the state
// for the next attempt (meta or archive.org unavailable): nothing the fetch
// holds is wrong.
type keepStaging struct{ err error }

func (k *keepStaging) Error() string { return k.err.Error() }
func (k *keepStaging) Unwrap() error { return k.err }

// metaRead is read, tried again while meta is unavailable
// (metaops.ErrUnavailable); still unavailable after metaAttempts, the error is
// a keepStaging. Any other answer (a 404, a refusal, metadata off, the
// context) is returned at once.
func metaRead[T any](ctx context.Context, h *Handler, rep *jobs.Reporter, read func() (T, error)) (T, error) {
	wait := metaWait
	for attempt := 1; ; attempt++ {
		v, err := read()
		if err == nil || !errors.Is(err, metaops.ErrUnavailable) || ctx.Err() != nil {
			return v, err
		}
		if attempt == metaAttempts {
			return v, &keepStaging{err: err}
		}
		h.report(rep, jobs.Progress{Step: StepManifest, Message: "The metadata database isn't answering: trying again"})
		if werr := h.o.Wait(ctx, wait); werr != nil {
			return v, werr
		}
		wait *= 2
	}
}

// archiveIDOf is the project's Internet Archive item: the recording's in the
// work, else the feed's url_iarchive.
func archiveIDOf(p Params, book metaops.LibrivoxBook, work *metaops.LibrivoxWork) string {
	if work != nil {
		for _, r := range work.Recordings {
			if r.ID == p.RecordingID && r.ArchiveID != "" {
				return r.ArchiveID
			}
		}
	}
	return book.ArchiveID()
}

// plan picks the book's folder and records it (f.st): the title's folder under
// the author's, else once "<Title> (LibriVox <id>)", else exists. A folder
// that already holds this project (a placement whose state was lost) is the
// book: f.st comes back Placed.
func (h *Handler) plan(ctx context.Context, f *fetch, srv Server, book metaops.LibrivoxBook, work *metaops.LibrivoxWork) error {
	books, err := importjob.AllBooks(ctx, srv, f.lib.ID)
	if err != nil {
		return fmt.Errorf("reading the library's books on the server: %w", err)
	}
	author, title := bookNames(f.p.LibrivoxID, book, work)
	parent, dest := destination(author, title, books)
	root, err := f.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if parent != "" {
		// The author folder as the library spells it (a case-insensitive disk).
		spelled, err := transfer.OnDisk(transfer.RootFS(root), parent)
		if err != nil {
			return fail(CodeNotWritable, "The Bridge can't place the book under %s in the library: %v", parent, err)
		}
		parent = spelled
		if err := h.parentFree(root, parent, books); err != nil {
			return err
		}
	}
	st := &state{LibraryID: f.lib.ID, ServerID: h.o.ServerID(), Parent: parent}
	dest = path.Join(parent, path.Base(dest)) // under the author folder as spelled on disk
	fallback := fallbackName(dest, f.p.LibrivoxID)
	for _, at := range []string{dest, fallback} {
		if _, err := transfer.CleanRel(at); err != nil {
			return fail(CodeNotWritable, "The book's folder %q isn't a valid path in the library.", at)
		}
		_, err := root.Lstat(at)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			st.Dest = at
		case err != nil:
			return fail(CodeNotWritable, "The Bridge can't look at %s in the library: %v", at, err)
		case holdsProjectIn(root, at, f.p.LibrivoxID):
			// The folder as the disk spells it (a case-insensitive disk finds
			// "johnny crow's garden" for "Johnny Crow's Garden"): the rescan
			// asks the server for the folder it indexes.
			spelled, err := transfer.OnDisk(transfer.RootFS(root), at)
			if err != nil || path.Dir(spelled) != path.Dir(at) {
				spelled = at
			}
			st.Dest, st.Placed, st.Adopted = spelled, true, true
			st.Count, st.Bytes = countFiles(root, spelled)
		}
		if st.Dest != "" {
			break
		}
	}
	if st.Dest == "" {
		return fail(CodeExists, "%s and %s are both taken in the library: nothing was overwritten. Move or rename one of them, then add the recording again.",
			dest, fallback)
	}
	if !st.Placed {
		if _, err := root.Lstat(parent); parent != "" && errors.Is(err, fs.ErrNotExist) {
			st.MadeParent = true
		}
	}
	if err := h.saveState(f.p.LibrivoxID, st); err != nil {
		return fmt.Errorf("recording the fetch's plan in the Bridge's data: %w", err)
	}
	f.st = st
	return nil
}

// scannerAudioExts are the extensions the server's scanner indexes as audio
// (audiosilo-server internal/metadata AudioExtensions, mirrored by hand): an
// author folder holding one is a book of its own to the server.
var scannerAudioExts = map[string]bool{
	".m4b": true, ".m4a": true, ".mp4": true, ".mp3": true,
	".flac": true, ".ogg": true, ".opus": true, ".mka": true,
}

// parentFree refuses an author folder that is itself a book (one the server
// lists, or one holding audio): the book's folder in it would join that book.
func (h *Handler) parentFree(root *os.Root, parent string, books []link.LibraryBook) error {
	for _, b := range books {
		if b.Path == parent {
			return fail(CodeExists, "%s is already the library's book %q, so the recording can't go in a folder inside it.", parent, b.Title)
		}
	}
	entries, err := fs.ReadDir(transfer.RootFS(root), parent)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fail(CodeNotWritable, "The Bridge can't read the folder %s in the library: %v", parent, err)
	}
	for _, e := range entries {
		if !e.IsDir() && scannerAudioExts[strings.ToLower(path.Ext(e.Name()))] {
			return fail(CodeExists, "%s already holds audio (%s), a book of its own, so the recording can't go in a folder inside it.", parent, e.Name())
		}
	}
	return nil
}

// prepare makes the author folder and the staging folder (0755, through the
// mount's root).
func (h *Handler) prepare(f *fetch) error {
	root, err := f.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if f.st.Parent != "" {
		if err := root.MkdirAll(f.st.Parent, 0o755); err != nil {
			return writeFailure(err, f.st.Parent)
		}
	}
	staging := f.staging()
	if err := root.Mkdir(staging, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return writeFailure(err, staging)
	}
	if st, err := root.Lstat(staging); err != nil || !st.IsDir() {
		return fail(CodeNotWritable, "The Bridge can't use %s in the library as its staging folder.", staging)
	}
	return nil
}

// writeFailure is a failed write in the library as a fetch's failure: out of
// space is disk_full, a read-only filesystem or a refusal not_writable.
func writeFailure(err error, rel string) error {
	switch {
	case fsutil.IsNoSpace(err):
		return fail(CodeDiskFull, "The library ran out of space writing %s.", rel)
	case errors.Is(err, fs.ErrPermission), fsutil.IsReadOnly(err):
		return fail(CodeNotWritable, "The Bridge can't write %s in the library: %v", rel, err)
	}
	return fmt.Errorf("writing %s in the library: %w", rel, err)
}

// removeStaging removes the staging folder, and the author folder this fetch
// made once it is empty, through the mount's root. It reports whether the
// staging folder is gone: when it isn't (the mount can't be opened or written
// now), the caller keeps the state, which is all that still names the folder,
// for the sweep.
func (h *Handler) removeStaging(mount string, id int, st *state) bool {
	root, err := os.OpenRoot(mount)
	if err != nil {
		h.o.Log.Warn("librivox: removing a fetch's staging folder failed", "librivox_id", id, "path", st.staging(id), "err", err)
		return false
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(st.staging(id)); err != nil {
		h.o.Log.Warn("librivox: removing a fetch's staging folder failed", "librivox_id", id, "path", st.staging(id), "err", err)
		return false
	}
	if st.MadeParent && st.Parent != "" {
		_ = root.Remove(st.Parent) // only when empty
	}
	return true
}

// writeMarker writes .audiosilo/librivox.json into the staging folder.
func (h *Handler) writeMarker(f *fetch, archiveID string) error {
	b, err := json.Marshal(Marker{Version: MarkerVersion, LibrivoxID: f.p.LibrivoxID, WorkID: f.p.WorkID,
		RecordingID: f.p.RecordingID, ArchiveID: archiveID})
	if err != nil {
		return err
	}
	root, err := f.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	dir := path.Join(f.staging(), path.Dir(markerRel))
	if err := root.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return writeFailure(err, dir)
	}
	if err := fsutil.WriteRootAtomic(root, dir, path.Base(markerRel), b, 0o644, nil); err != nil {
		return writeFailure(err, path.Join(f.staging(), markerRel))
	}
	return nil
}

// holdsProject reports whether the folder rel holds this project's
// librivox.json (a book an earlier run placed).
func (h *Handler) holdsProject(mount, rel string, id int) bool {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	return holdsProjectIn(root, rel, id)
}

func holdsProjectIn(root *os.Root, rel string, id int) bool {
	b, err := root.ReadFile(path.Join(rel, markerRel))
	if err != nil {
		return false
	}
	var m Marker
	return json.Unmarshal(b, &m) == nil && m.LibrivoxID == id
}

// countFiles is how many regular files the book's folder holds outside
// .audiosilo/ (the sections and the cover), and their bytes.
func countFiles(root *os.Root, rel string) (n int, bytes int64) {
	entries, err := fs.ReadDir(transfer.RootFS(root), rel)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if info, err := e.Info(); err == nil {
			n++
			bytes += info.Size()
		}
	}
	return n, bytes
}

// place renames the staging folder to the book's folder: one no-replace
// rename, after the mapping is checked again (not writable now, the job parks
// and waits with what it downloaded). A folder that turned up at the
// destination meanwhile moves the book to the fallback name once, else exists.
func (h *Handler) place(ctx context.Context, f *fetch, names []string) error {
	h.report(f.rep, jobs.Progress{Step: StepPlace, Message: "Placing the book in the library"})
	if err := h.recheck(ctx, f, StepPlace); err != nil {
		return err
	}
	root, err := f.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := sweep(root, f.staging(), names); err != nil {
		return err
	}
	n, bytes := countFiles(root, f.staging())
	primary := f.st.Dest
	for _, dest := range candidates(primary, f.p.LibrivoxID) {
		err = fsutil.RenameNoReplace(root, f.staging(), dest)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return writeFailure(err, dest)
		}
		f.st.Dest, f.st.Placed, f.st.Count, f.st.Bytes = dest, true, n, bytes
		f.st.Files = nil
		if serr := h.saveState(f.p.LibrivoxID, f.st); serr != nil {
			h.o.Log.Warn("librivox: recording a placed book failed", "librivox_id", f.p.LibrivoxID, "err", serr)
		}
		return nil
	}
	return fail(CodeExists, "Something else is already at %s in the library: nothing was overwritten. Move or rename it, then add the recording again.", primary)
}

// sweep leaves the staging folder holding this manifest's files only: its
// sections, the cover and .audiosilo/. Anything else there is an earlier
// run's (a partial, or a section fetched under a title the manifest has
// changed since) and goes, so it is never placed with the book; a section
// that isn't there fails the placement. An entry is the manifest's by its
// name, or by being the same file as the one found under that name: a mount
// may list a name in another form than it was given (NFD for NFC). The
// sections are looked for again after the removals, so a mount that tells
// neither way never has an incomplete book placed.
func sweep(root *os.Root, staging string, names []string) error {
	sections := func() ([]fs.FileInfo, error) {
		infos := make([]fs.FileInfo, 0, len(names)+2)
		for i, name := range names {
			st, err := root.Lstat(path.Join(staging, name))
			if err != nil || !st.Mode().IsRegular() {
				return nil, fmt.Errorf("the staging folder doesn't hold section %d of the recording's %d", i+1, len(names))
			}
			infos = append(infos, st)
		}
		return infos, nil
	}
	keep, err := sections()
	if err != nil {
		return err
	}
	named := map[string]bool{}
	for _, name := range names {
		named[name] = true
	}
	for _, name := range []string{coverName, path.Dir(markerRel)} {
		named[name] = true
		if st, err := root.Lstat(path.Join(staging, name)); err == nil {
			keep = append(keep, st)
		}
	}
	entries, err := fs.ReadDir(transfer.RootFS(root), staging)
	if err != nil {
		return fmt.Errorf("reading the staging folder: %w", err)
	}
	for _, e := range entries {
		if named[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // gone since the listing
		}
		if slices.ContainsFunc(keep, func(k fs.FileInfo) bool { return os.SameFile(k, info) }) {
			continue
		}
		if err := root.RemoveAll(path.Join(staging, e.Name())); err != nil {
			return writeFailure(err, path.Join(staging, e.Name()))
		}
	}
	_, err = sections()
	return err
}

// adopt marks a fetch placed whose staging folder is gone and whose folder
// (or its fallback) holds this project: the rename happened and the state's
// write after it didn't land (a kill between the two, a full data folder).
// Without it the resumed run would download the book again and place it a
// second time under the fallback name.
func (h *Handler) adopt(f *fetch) {
	if f.st.Placed {
		return
	}
	root, err := f.openRoot()
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Lstat(f.staging()); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	for _, at := range candidates(f.st.Dest, f.p.LibrivoxID) {
		if holdsProjectIn(root, at, f.p.LibrivoxID) {
			f.st.Dest, f.st.Placed, f.st.Files = at, true, nil
			f.st.Count, f.st.Bytes = countFiles(root, at)
			return
		}
	}
}

// rescanAttempts bounds the rescan's tries (the book is placed either way).
const rescanAttempts = 3

// finish asks the server to index the placed book and makes the result; the
// state goes once it is made.
func (h *Handler) finish(ctx context.Context, f *fetch, srv Server) (*Result, error) {
	h.report(f.rep, jobs.Progress{Step: StepRescan, Message: "Asking the server to index the book"})
	var bookPath string
	var err error
	for attempt := 1; attempt <= rescanAttempts; attempt++ {
		if bookPath, err = srv.Rescan(ctx, f.lib.ID, f.st.Dest); err == nil {
			break
		}
		if ctx.Err() != nil || attempt == rescanAttempts {
			return nil, fmt.Errorf("the book is in the library at %s, but the server couldn't index it: %w", f.st.Dest, err)
		}
		if werr := h.o.Wait(ctx, time.Duration(attempt)*5*time.Second); werr != nil {
			return nil, werr
		}
	}
	if bookPath == "" {
		bookPath = f.st.Dest
	}
	h.o.Log.Info("librivox: added", "librivox_id", f.p.LibrivoxID, "library", f.lib.ID, "path", bookPath,
		"files", f.st.Count, "bytes", f.st.Bytes)
	h.dropState(f.lib.ID, f.p.LibrivoxID)
	return &Result{LibraryID: f.lib.ID, Path: bookPath, LibrivoxID: f.p.LibrivoxID, Files: f.st.Count, Bytes: f.st.Bytes,
		Adopted: f.st.Adopted}, nil
}

// report records the job's progress.
func (h *Handler) report(rep *jobs.Reporter, pr jobs.Progress) {
	rep.Progress(pr)
	if h.o.onProgress != nil {
		h.o.onProgress(pr)
	}
}

// recheck is mapped for a fetch under way: the library must still be a
// checked, writable mapping (not writable now, the job parks until it is),
// and at the mount the staging folder is in. A mapping moved to another mount
// fails the fetch: what it staged isn't there.
func (h *Handler) recheck(ctx context.Context, f *fetch, step string) error {
	_, mount, err := h.mapped(ctx, f.lib.ID, f.rep, step, false)
	if err != nil {
		return err
	}
	if mount != f.mount {
		return fail(CodeNotWritable, "Library %s moved to another folder on the Bridge during the fetch: add the recording again.", f.lib.Name)
	}
	return nil
}

// mapped is the fetch's books library and its mount once the library is a
// checked, writable mapping there (audiblejob's rule): a job claimed before
// the check waits for it; a mapping that isn't writable fails a new job
// (strict) and parks a resumed one, or one past its download, not_writable
// until it comes back.
//
// A parked fetch gives its download-lane slot back (jobs.Reporter.Yield), so
// the Audible imports and podcast downloads that share the lane run while it
// waits; once the mapping is back it takes a slot again (Reclaim) before it
// writes anything, so downloads still go one at a time.
func (h *Handler) mapped(ctx context.Context, id int64, rep *jobs.Reporter, step string, strict bool) (link.Library, string, error) {
	for {
		lib, ok := h.o.Libraries.Library(id)
		if !ok || lib.LibraryKind() != "books" {
			return link.Library{}, "", fail(CodeNotWritable, "The server lists no books library %d for the Bridge.", id)
		}
		mount, _ := h.o.Mount(lib.ID, lib.Root)
		st, checked := h.o.Libraries.Status(lib.ID)
		why := fmt.Sprintf("Library %s must be mapped and writable on the Bridge: check its mapping on the Bridge's Status tab.", lib.Name)
		switch {
		case checked && st.MountPath == mount && st.Mapped && st.Writable:
			rep.Unpark()
			if !rep.Yielded() {
				return lib, mount, nil
			}
			// Back, but without a slot: take one, then look at the mapping
			// again, since the wait for the slot may have been long.
			h.report(rep, jobs.Progress{Step: step, Message: "Waiting for the Bridge's other download to finish"})
			if err := rep.Reclaim(ctx); err != nil {
				return link.Library{}, "", err
			}
			continue
		case !checked || st.MountPath != mount:
			h.report(rep, jobs.Progress{Step: step, Message: "Waiting for the library's mapping check"})
		case strict:
			return link.Library{}, "", fail(CodeNotWritable, "%s", why)
		default:
			h.report(rep, jobs.Progress{Step: step, Message: "Needs attention: " + why,
				Extra: map[string]any{"park": map[string]any{"code": CodeNotWritable}}})
			rep.Park(CodeNotWritable, why)
			rep.Yield()
		}
		if err := h.o.Wait(ctx, mappingPoll); err != nil {
			return link.Library{}, "", err
		}
	}
}
