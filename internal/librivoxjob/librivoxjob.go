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
	"strings"
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
type Result struct {
	LibraryID  int64  `json:"library_id"`
	Path       string `json:"path"`
	LibrivoxID int    `json:"librivox_id"`
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
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
	Log      *slog.Logger

	// Seams (tests): the free-space reader and the wait between attempts and
	// between looks at the library's mapping.
	Free func(dir string) (int64, error)
	Wait func(ctx context.Context, d time.Duration) error
	// onProgress sees every progress the job reports (tests).
	onProgress func(jobs.Progress)
}

// Handler runs librivox.fetch on the download lane (capacity 1, shared with
// audible.import): one book at a time.
type Handler struct {
	o Options
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
	return &Handler{o: o}
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
func (h *Handler) Abandon(ctx context.Context, job jobs.Job) {
	if jobs.Lost(ctx) {
		return
	}
	p, err := ParseParams(job.Params)
	if err != nil {
		return
	}
	st := h.loadState(p.LibrivoxID)
	if st == nil || st.Placed {
		return
	}
	if lib, ok := h.o.Libraries.Library(st.LibraryID); ok {
		if mount, _ := h.o.Mount(lib.ID, lib.Root); mount != "" {
			h.removeStaging(mount, p.LibrivoxID, st)
		}
	}
	h.dropState(p.LibrivoxID)
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

// fetch is one run's working set.
type fetch struct {
	p     Params
	lib   link.Library
	mount string
	st    *state
	rep   *jobs.Reporter
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
	f := &fetch{p: p, lib: lib, mount: mount, rep: rep, st: h.loadState(p.LibrivoxID)}
	if f.st != nil && f.st.LibraryID != lib.ID {
		// An earlier fetch of the project for another library: its staging
		// folder goes (best effort), and this one plans afresh.
		h.Abandon(context.WithoutCancel(ctx), jobs.Job{Params: job.Params})
		f.st = nil
	}
	defer func() {
		// A stop the next start resumes, and a lost claim the server hands out
		// again, keep the staging folder and the state; a cancel or a failure
		// before the book was placed removes both. A placed book stays, and its
		// state with it when the rescan failed (the retry goes on from there).
		if err == nil || f.st == nil || f.st.Placed {
			return
		}
		if ctx.Err() != nil && (jobs.Resumes(ctx) || jobs.Lost(ctx)) {
			return
		}
		h.removeStaging(f.mount, p.LibrivoxID, f.st)
		h.dropState(p.LibrivoxID)
	}()

	// An earlier run placed the book and then failed (the rescan): while its
	// folder is still this project's, go on from the rescan.
	if f.st != nil && f.st.Placed {
		if h.holdsProject(mount, f.st.Dest, p.LibrivoxID) {
			return h.finish(ctx, f, srv)
		}
		h.dropState(p.LibrivoxID)
		f.st = nil
	}

	h.report(rep, jobs.Progress{Step: StepManifest, Message: "Reading the recording from the metadata database"})
	book, work, err := h.manifest(ctx, p)
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
func (h *Handler) manifest(ctx context.Context, p Params) (metaops.LibrivoxBook, *metaops.LibrivoxWork, error) {
	if h.o.Meta == nil {
		return metaops.LibrivoxBook{}, nil, fail(CodeMetadataOff, "The Bridge has no metadata service set (metadata.base_url), so it can't read LibriVox recordings.")
	}
	book, err := h.o.Meta.LibrivoxAudiobook(ctx, p.LibrivoxID)
	switch {
	case errors.Is(err, metaops.ErrDisabled):
		return book, nil, fail(CodeMetadataOff, "The Bridge has no metadata service set (metadata.base_url), so it can't read LibriVox recordings.")
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
	w, err := h.o.Meta.LibrivoxWork(ctx, p.WorkID)
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
	root, err := os.OpenRoot(f.mount)
	if err != nil {
		return fail(CodeNotWritable, "The Bridge can't open the library at its mount: %v", err)
	}
	defer func() { _ = root.Close() }()
	if parent != "" {
		// The author folder as the library spells it (a case-insensitive disk).
		if parent, err = transfer.OnDisk(transfer.RootFS(root), parent); err != nil {
			return fail(CodeNotWritable, "The Bridge can't place the book under %s in the library: %v", parent, err)
		}
		if err := h.parentFree(root, parent, books); err != nil {
			return err
		}
	}
	st := &state{LibraryID: f.lib.ID, Parent: parent, Staging: path.Join(parent, stagingName(f.p.LibrivoxID))}
	for i, name := range []string{path.Base(dest), fallbackName(path.Base(dest), f.p.LibrivoxID)} {
		at := path.Join(parent, name)
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
			st.Dest, st.Placed = at, true
			st.Count, st.Bytes = countFiles(root, at)
		case i == 1:
			return fail(CodeExists, "%s and %s are both taken in the library: nothing was overwritten. Move or rename one of them, then add the recording again.",
				path.Join(parent, path.Base(dest)), at)
		default:
			continue
		}
		break
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
		if !e.IsDir() && audioExts[strings.ToLower(path.Ext(e.Name()))] {
			return fail(CodeExists, "%s already holds audio (%s), a book of its own, so the recording can't go in a folder inside it.", parent, e.Name())
		}
	}
	return nil
}

// prepare makes the author folder and the staging folder (0755, through the
// mount's root).
func (h *Handler) prepare(f *fetch) error {
	root, err := os.OpenRoot(f.mount)
	if err != nil {
		return fail(CodeNotWritable, "The Bridge can't open the library at its mount: %v", err)
	}
	defer func() { _ = root.Close() }()
	if f.st.Parent != "" {
		if err := root.MkdirAll(f.st.Parent, 0o755); err != nil {
			return writeFailure(err, f.st.Parent)
		}
	}
	if err := root.Mkdir(f.st.Staging, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return writeFailure(err, f.st.Staging)
	}
	if st, err := root.Lstat(f.st.Staging); err != nil || !st.IsDir() {
		return fail(CodeNotWritable, "The Bridge can't use %s in the library as its staging folder.", f.st.Staging)
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
// made once it is empty, through the mount's root (best effort).
func (h *Handler) removeStaging(mount string, id int, st *state) {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	if path.Base(st.Staging) == stagingName(id) {
		if err := root.RemoveAll(st.Staging); err != nil {
			h.o.Log.Warn("librivox: removing a fetch's staging folder failed", "librivox_id", id, "path", st.Staging, "err", err)
		}
	}
	if st.MadeParent && st.Parent != "" {
		_ = root.Remove(st.Parent) // only when empty
	}
}

// writeMarker writes .audiosilo/librivox.json into the staging folder.
func (h *Handler) writeMarker(f *fetch, archiveID string) error {
	b, err := json.Marshal(Marker{Version: MarkerVersion, LibrivoxID: f.p.LibrivoxID, WorkID: f.p.WorkID,
		RecordingID: f.p.RecordingID, ArchiveID: archiveID})
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(f.mount)
	if err != nil {
		return fail(CodeNotWritable, "The Bridge can't open the library at its mount: %v", err)
	}
	defer func() { _ = root.Close() }()
	dir := path.Join(f.st.Staging, path.Dir(markerRel))
	if err := root.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return writeFailure(err, dir)
	}
	if err := fsutil.WriteRootAtomic(root, dir, path.Base(markerRel), b, 0o644, nil); err != nil {
		return writeFailure(err, path.Join(f.st.Staging, markerRel))
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
	if _, _, err := h.mapped(ctx, f.lib.ID, f.rep, StepPlace, false); err != nil {
		return err
	}
	root, err := os.OpenRoot(f.mount)
	if err != nil {
		return fail(CodeNotWritable, "The Bridge can't open the library at its mount: %v", err)
	}
	defer func() { _ = root.Close() }()
	n, bytes := countFiles(root, f.st.Staging)
	if n < len(names) {
		return fmt.Errorf("the staging folder holds %d files, %d expected", n, len(names))
	}
	primary := f.st.Dest
	fallback := path.Join(f.st.Parent, fallbackName(path.Base(primary), f.p.LibrivoxID))
	if strings.HasSuffix(primary, fmt.Sprintf(" (LibriVox %d)", f.p.LibrivoxID)) {
		fallback = ""
	}
	for _, dest := range []string{primary, fallback} {
		if dest == "" {
			break
		}
		err = fsutil.RenameNoReplace(root, f.st.Staging, dest)
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
	h.dropState(f.p.LibrivoxID)
	return &Result{LibraryID: f.lib.ID, Path: bookPath, LibrivoxID: f.p.LibrivoxID, Files: f.st.Count, Bytes: f.st.Bytes}, nil
}

// report records the job's progress.
func (h *Handler) report(rep *jobs.Reporter, pr jobs.Progress) {
	rep.Progress(pr)
	if h.o.onProgress != nil {
		h.o.onProgress(pr)
	}
}

// mapped is the fetch's books library and its mount once the library is a
// checked, writable mapping there (audiblejob's rule): a job claimed before
// the check waits for it; a mapping that isn't writable fails a new job
// (strict) and parks a resumed one, or one past its download, not_writable
// until it comes back.
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
			return lib, mount, nil
		case !checked || st.MountPath != mount:
			h.report(rep, jobs.Progress{Step: step, Message: "Waiting for the library's mapping check"})
		case strict:
			return link.Library{}, "", fail(CodeNotWritable, "%s", why)
		default:
			h.report(rep, jobs.Progress{Step: step, Message: "Needs attention: " + why,
				Extra: map[string]any{"park": map[string]any{"code": CodeNotWritable}}})
			rep.Park(CodeNotWritable, why)
		}
		if err := h.o.Wait(ctx, mappingPoll); err != nil {
			return link.Library{}, "", err
		}
	}
}
