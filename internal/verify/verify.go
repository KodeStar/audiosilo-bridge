// Package verify is Phase 0's proof job, bridge.verify (BRIDGE-PLAN.md): it
// stats a book (or, with no path, a library's sample book) through the
// Bridge's mapping, compares sizes with the server's, probes whether the
// Bridge can write in the book's own folder, and reports what it found and how
// long it took. hold_seconds keeps it running that much longer, reporting each
// second, so a cancel and a lease expiry can be exercised on a real stack.
package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
)

// Kind is the job kind.
const Kind = "bridge.verify"

// MaxHold is the longest hold the server accepts (its verifyParams).
const MaxHold = 600

// Libraries resolves a server library and where the Bridge sees it.
type Libraries interface {
	Library(id int64) (link.Library, bool)
}

// The book read's retry: an answer that may change (no answer, a 5xx) is asked
// again after bookRetryFirst, doubling up to bookRetryMax, until bookRetryFor
// has passed since the first ask, so a server restarting under a verify
// doesn't fail it, and one that stays down does in a time an admin waits for.
const (
	bookRetryFirst = time.Second
	bookRetryMax   = 15 * time.Second
	bookRetryFor   = 2 * time.Minute
)

// Handler runs bridge.verify.
type Handler struct {
	libs  Libraries
	mount mapping.MountFunc
	books func() mapping.BookSource
	// retryFirst and retryFor are the book read's first wait and budget
	// (bookRetryFirst, bookRetryFor; shorter in tests).
	retryFirst, retryFor time.Duration
}

// New returns the handler. books returns the server link (nil while unpaired).
func New(libs Libraries, mount mapping.MountFunc, books func() mapping.BookSource) *Handler {
	return &Handler{libs: libs, mount: mount, books: books, retryFirst: bookRetryFirst, retryFor: bookRetryFor}
}

// Kind is bridge.verify.
func (h *Handler) Kind() string { return Kind }

// Lane is the light lane.
func (h *Handler) Lane() string { return jobs.LaneLight }

// Params are bridge.verify's params.
type Params struct {
	HoldSeconds int `json:"hold_seconds"`
}

// Result is what bridge.verify reports.
type Result struct {
	LibraryID int64               `json:"library_id"`
	MountPath string              `json:"mount_path"`
	Path      string              `json:"path"`
	Files     []mapping.FileCheck `json:"files"`
	Writable  bool                `json:"writable"`
	// WritableError is why the book folder isn't writable ("" when it is).
	WritableError string `json:"writable_error,omitempty"`
	ElapsedMS     int64  `json:"elapsed_ms"`
}

// Run verifies the job's book through the mapping.
func (h *Handler) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	start := time.Now()
	var p Params
	if len(job.Params) > 0 {
		if err := json.Unmarshal(job.Params, &p); err != nil {
			return nil, fmt.Errorf("bad params: %w", err)
		}
	}
	if p.HoldSeconds < 0 || p.HoldSeconds > MaxHold {
		return nil, fmt.Errorf("hold_seconds must be 0-%d", MaxHold)
	}
	if job.LibraryID == nil {
		return nil, errors.New("bridge.verify needs a library")
	}
	rep.Progress(jobs.Progress{Step: "mapping", Message: "Finding the library"})
	lib, ok := h.libs.Library(*job.LibraryID)
	if !ok {
		return nil, fmt.Errorf("library %d isn't one the server listed", *job.LibraryID)
	}
	mount, _ := h.mount(lib.ID, lib.Root)
	src := h.books()
	if src == nil {
		return nil, errors.New("the Bridge isn't paired")
	}
	path := ""
	if job.Path != nil {
		path = *job.Path
	}
	book, err := h.fetchBook(ctx, rep, src, lib.ID, path)
	if err != nil {
		return nil, fmt.Errorf("could not get the book's files from the server: %w", err)
	}
	res := Result{LibraryID: lib.ID, MountPath: mount, Path: book.Path}
	rep.Progress(jobs.Progress{Step: "stat", Message: "Comparing the files", Total: len(book.Files)})
	checks, ok, problem := mapping.CompareFiles(mount, book.Files)
	res.Files = checks
	rep.Progress(jobs.Progress{Step: "stat", Message: "Compared the files", Done: len(checks), Total: len(book.Files)})

	if ok {
		rep.Progress(jobs.Progress{Step: "writable", Message: "Checking the book's folder can be written"})
		dir, derr := bookFolder(mount, book)
		if derr == nil {
			derr = mapping.ProbeWritable(dir)
		}
		res.Writable = derr == nil
		if derr != nil {
			res.WritableError = derr.Error()
		}
	}

	for i := 1; i <= p.HoldSeconds; i++ {
		select {
		case <-ctx.Done():
			res.ElapsedMS = time.Since(start).Milliseconds()
			return res, ctx.Err()
		case <-time.After(time.Second):
		}
		rep.Progress(jobs.Progress{Step: "hold", Message: "Holding, as asked", Done: i, Total: p.HoldSeconds})
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	if !ok {
		return res, errors.New("the Bridge's copy doesn't match the server's: " + problem)
	}
	return res, nil
}

// fetchBook asks the server for the book's files, asking again with backoff
// (within retryFor) while the answer may change (mapping.BookReadSettled,
// generate's fetchBook's rule). A definite answer fails at once, and so does
// the last transient one once the budget is spent.
func (h *Handler) fetchBook(ctx context.Context, rep *jobs.Reporter, src mapping.BookSource, libraryID int64, path string) (*link.Book, error) {
	deadline := time.Now().Add(h.retryFor)
	wait := h.retryFirst
	for {
		rep.Progress(jobs.Progress{Step: "lookup", Message: "Asking the server for the book's files"})
		book, err := src.Book(ctx, libraryID, path)
		if err == nil || mapping.BookReadSettled(err) || time.Now().Add(wait).After(deadline) {
			return book, err
		}
		if err := agent.SleepCtx(ctx, wait); err != nil {
			return nil, err
		}
		wait = min(wait*2, bookRetryMax)
	}
}

// bookFolder is the folder a book's files are in through the mount: the
// book's own folder for a folder book, the folder holding it for a single
// file.
func bookFolder(mount string, book *link.Book) (string, error) {
	full, err := mapping.SafeJoin(mount, book.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return full, nil
	}
	return filepath.Dir(full), nil
}
