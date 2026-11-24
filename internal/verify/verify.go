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
	"log/slog"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/timeutil"
)

// Kind is the job kind.
const Kind = "bridge.verify"

// MaxHold is the longest hold the server accepts (its verifyParams).
const MaxHold = 600

// Libraries resolves a server library and where the Bridge sees it.
type Libraries interface {
	Library(id int64) (link.Library, bool)
}

// The book read's retry: an answer that may change (mapping.BookReadSettled's,
// less the 429 the link has already waited out: no answer, a refused token, a
// 408, a 5xx) is asked again after
// bookRetryFirst, doubling up to bookRetryMax, until bookRetryFor has passed
// since the first ask (the last ask at its end), so a server restarting under a
// verify doesn't fail it, and one that stays down does in a time an admin
// waits for.
const (
	bookRetryFirst = time.Second
	bookRetryMax   = 15 * time.Second
	bookRetryFor   = mapping.BookReadBudget
)

// Handler runs bridge.verify.
type Handler struct {
	libs  Libraries
	mount mapping.MountFunc
	books func() mapping.BookSource
	// retryFirst and retryFor are the book read's first wait and budget
	// (bookRetryFirst, bookRetryFor; shorter in tests).
	retryFirst, retryFor time.Duration
	log                  *slog.Logger
	// compare and probe are mapping.CompareFiles and mapping.ProbeWritable (a
	// test makes one time out).
	compare func(mount string, files []link.BookFile) ([]mapping.FileCheck, bool, string, error)
	probe   func(dir string) error
}

// New returns the handler. books returns the server link (nil while unpaired);
// log takes the book read's retries (nil: the default logger).
func New(libs Libraries, mount mapping.MountFunc, books func() mapping.BookSource, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{libs: libs, mount: mount, books: books, retryFirst: bookRetryFirst, retryFor: bookRetryFor, log: log,
		compare: mapping.CompareFiles, probe: mapping.ProbeWritable}
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
	rep.Progress(jobs.Progress{Step: "mapping", Code: jobs.SayFindingLibrary})
	lib, ok := h.libs.Library(*job.LibraryID)
	if !ok {
		return nil, fmt.Errorf("library %d isn't one the server listed", *job.LibraryID)
	}
	mount, _ := h.mount(lib.ID, lib.Root)
	src := jobs.Link(job, h.books()) // none once paired with another server
	if src == nil {
		return nil, errors.New("the Bridge isn't paired")
	}
	path := ""
	if job.Path != nil {
		path = *job.Path
	}
	book, err := h.fetchBook(ctx, job.ID, rep, src, lib.ID, path)
	if err != nil {
		return nil, fmt.Errorf("could not get the book's files from the server: %w", err)
	}
	res := Result{LibraryID: lib.ID, MountPath: mount, Path: book.Path}
	rep.Progress(jobs.Progress{Step: "stat", Code: jobs.SayComparingFiles, Total: len(book.Files)})
	var checks []mapping.FileCheck
	var problem string
	_ = h.whileSlow(ctx, job.ID, func() error {
		var timeout error
		checks, ok, problem, timeout = h.compare(mount, book.Files)
		return timeout
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	res.Files = checks
	rep.Progress(jobs.Progress{Step: "stat", Code: jobs.SayComparedFiles, Done: len(checks), Total: len(book.Files)})

	if ok {
		rep.Progress(jobs.Progress{Step: "writable", Code: jobs.SayCheckingWritable})
		derr := h.whileSlow(ctx, job.ID, func() error {
			dir, err := bookFolder(mount, book)
			if err == nil {
				err = h.probe(dir)
			}
			return err
		})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		res.Writable = derr == nil
		if derr != nil {
			res.WritableError = derr.Error()
		}
	}

	for i := 1; i <= p.HoldSeconds; i++ {
		if err := timeutil.SleepCtx(ctx, time.Second); err != nil {
			res.ElapsedMS = time.Since(start).Milliseconds()
			return res, err
		}
		rep.Progress(jobs.Progress{Step: "hold", Code: jobs.SayHolding, Done: i, Total: p.HoldSeconds})
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	if !ok {
		return res, errors.New("the Bridge's copy doesn't match the server's: " + problem)
	}
	return res, nil
}

// fetchBook leaves 429 retries to the link. It asks again (retrying)
// while the answer may change (mapping.BookReadSettled, generate's fetchBook's
// rule). A definite answer fails at once, and so does the last transient one
// once the budget is spent.
func (h *Handler) fetchBook(ctx context.Context, jobID int64, rep *jobs.Reporter, src mapping.BookSource, libraryID int64, path string) (*link.Book, error) {
	var book *link.Book
	err := h.retrying(ctx, jobID, "verify: asking the server for the book's files failed; trying again",
		func(err error) bool {
			_, limited := link.RateLimited(err)
			return !limited && !mapping.BookReadSettled(err)
		},
		func() (err error) {
			rep.Progress(jobs.Progress{Step: "lookup", Code: jobs.SayBookFilesLookup})
			book, err = src.Book(ctx, libraryID, path)
			return err
		})
	if err != nil {
		return nil, err
	}
	return book, nil
}

// whileSlow runs look again (retrying) while the library's folder gives it no
// answer within mapping.LookTimeout (fsutil.ErrStatTimeout: a disk spinning
// up, a mount that hangs), which says nothing about the copy: the verify fails
// on a slow folder only once the budget is spent.
func (h *Handler) whileSlow(ctx context.Context, jobID int64, look func() error) error {
	return h.retrying(ctx, jobID, "verify: the library's folder didn't answer in time; looking again",
		func(err error) bool { return errors.Is(err, fsutil.ErrStatTimeout) }, look)
}

// retrying runs try, and again while again says its error may change, backing
// off from retryFirst (at most bookRetryMax) within retryFor, the wait before
// the last try cut to what is left of it so the last try is at its end. Each
// retry is logged as msg: the job's message is the same every round. It
// returns try's last error, or ctx's.
func (h *Handler) retrying(ctx context.Context, jobID int64, msg string, again func(error) bool, try func() error) error {
	deadline := time.Now().Add(h.retryFor)
	wait := h.retryFirst
	for {
		err := try()
		left := time.Until(deadline)
		if err == nil || !again(err) || left <= 0 {
			return err
		}
		h.log.Warn(msg, "job", jobID, "err", err)
		if err := timeutil.SleepCtx(ctx, min(wait, left)); err != nil {
			return err
		}
		wait = min(wait*2, bookRetryMax)
	}
}

// bookFolder is the folder a book's files are in through the mount: the
// book's own folder for a folder book, the folder holding it for a single
// file. Its stat is bounded by mapping.LookTimeout.
func bookFolder(mount string, book *link.Book) (string, error) {
	full, err := mapping.SafeJoin(mount, book.Path)
	if err != nil {
		return "", err
	}
	info, err := fsutil.StatWithin(full, mapping.LookTimeout)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return full, nil
	}
	return filepath.Dir(full), nil
}
