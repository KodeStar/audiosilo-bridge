package librivoxjob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/transfer"
)

// downloadAttempts bounds a section's attempts in a row that add nothing to
// its partial (a dropped connection resumes from it each time; an attempt that
// added to it starts the count again).
const downloadAttempts = 3

// maxFileBytes caps one section's file (a LibriVox section is tens of MB).
const maxFileBytes int64 = 2 << 30

// maxCoverBytes caps the cover.
const maxCoverBytes int64 = 20 << 20

// maxRetryAfter bounds how long a Retry-After is waited out.
const maxRetryAfter = 5 * time.Minute

// partSuffix names a section's partial download in the staging folder.
const partSuffix = ".part"

// download fetches every section not in the staging folder yet: a file there
// under its own name is finished (it is renamed from its .part only once
// whole), so a resumed run skips it and goes on with the next, resuming that
// one's .part with Range/If-Range when its strong ETag was recorded.
func (h *Handler) download(ctx context.Context, f *fetch, sections []metaops.LibrivoxSection, names []string) error {
	root, err := f.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	n := len(sections)
	for i, s := range sections {
		// The mapping again before each section's writes: one that went away
		// parks the job (not_writable) with what it fetched, until it is back.
		if _, _, err := h.mapped(ctx, f.lib.ID, f.rep, StepDownload, false); err != nil {
			return err
		}
		h.report(f.rep, jobs.Progress{Step: StepDownload, Message: fmt.Sprintf("Downloading section %d of %d from archive.org", i+1, n),
			Done: i, Total: n})
		if st, err := root.Lstat(path.Join(f.staging(), names[i])); err == nil && st.Mode().IsRegular() {
			continue
		}
		if err := h.section(ctx, root, f, i, s.ListenURL, names[i]); err != nil {
			return err
		}
	}
	h.report(f.rep, jobs.Progress{Step: StepDownload, Message: "Downloaded from archive.org", Done: n, Total: n})
	return nil
}

// retry is an attempt that may be tried again: after RetryAfter when the
// server said so, and whether the partial holds more than before it (a cut
// 200 that started the file again from zero adds nothing until it passes
// what the partial held).
type retry struct {
	retryAfter int
	added      bool
	cause      string
}

func (r *retry) Error() string { return r.cause }

// section downloads one section to <staging>/<name>, trying again while
// attempts add to the partial (downloadAttempts in a row that add nothing
// fail archive_unavailable).
func (h *Handler) section(ctx context.Context, root *os.Root, f *fetch, i int, rawURL, name string) error {
	failures := 0
	for {
		err := h.attempt(ctx, root, f, i, rawURL, name)
		var r *retry
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case !errors.As(err, &r):
			return err
		}
		if r.added {
			failures = 0
		}
		if failures++; failures >= downloadAttempts {
			return fail(CodeArchiveUnavailable, "archive.org didn't deliver section %d of LibriVox recording %d (%s): try again later.",
				i+1, f.p.LibrivoxID, r.cause)
		}
		h.o.Log.Info("librivox: a section's download failed; trying again", "librivox_id", f.p.LibrivoxID, "section", i+1,
			"attempt", failures, "cause", r.cause)
		wait := max(time.Duration(failures*failures)*2*time.Second, min(time.Duration(r.retryAfter)*time.Second, maxRetryAfter))
		if werr := h.o.Wait(ctx, wait); werr != nil {
			return werr
		}
	}
}

// attempt is one GET of a section: a 200 writes the file from its start, a
// 206 proven to continue the partial (netguard.ResumeAction) appends to it,
// anything unproven starts again. The file is renamed from its .part once it
// holds the size the response stated.
func (h *Handler) attempt(ctx context.Context, root *os.Root, f *fetch, i int, rawURL, name string) error {
	final := path.Join(f.staging(), name)
	part := final + partSuffix
	offset := int64(0)
	if st, err := root.Lstat(part); err == nil && st.Mode().IsRegular() {
		offset = st.Size()
	}
	prev := f.st.Files[name]
	if offset == 0 {
		prev = partState{}
	}
	resp, err := h.o.Fetcher.Stream(ctx, netguard.StreamRequest{URL: rawURL, Offset: offset, ETag: prev.ETag, Allow: AllowedURL})
	if err != nil {
		e := netguard.Classify(err)
		if netguard.Retryable(err) {
			return &retry{retryAfter: e.RetryAfterSeconds, cause: e.Code}
		}
		return fail(CodeArchiveUnavailable, "The Bridge can't download section %d of LibriVox recording %d from archive.org (%s).",
			i+1, f.p.LibrivoxID, e.Code)
	}
	defer func() { _ = resp.Body.Close() }()

	appending := false
	start, total := int64(0), int64(0)
	switch {
	case resp.Encoding != "" && resp.Encoding != "identity":
		return fail(CodeArchiveUnavailable, "archive.org sent section %d of LibriVox recording %d encoded (%s).", i+1, f.p.LibrivoxID, resp.Encoding)
	case resp.Status == http.StatusOK:
		total = max(resp.ContentLength, 0)
	case resp.Status == http.StatusPartialContent && netguard.ResumeAction(offset, prev.Total, prev.ETag, resp) == "append":
		appending, start, total = true, offset, prev.Total
	case resp.Status == http.StatusPartialContent, resp.Status == http.StatusRequestedRangeNotSatisfiable:
		// Not a continuation the Bridge can prove: the partial goes and the
		// next attempt asks for the whole file.
		_ = root.Remove(part)
		delete(f.st.Files, name)
		return &retry{cause: fmt.Sprintf("http_%d", resp.Status)}
	case resp.Status == http.StatusRequestTimeout, resp.Status == http.StatusTooManyRequests, resp.Status >= 500:
		return &retry{retryAfter: resp.RetryAfterSeconds, cause: fmt.Sprintf("http_%d", resp.Status)}
	default:
		return fail(CodeArchiveUnavailable, "archive.org answered HTTP %d for section %d of LibriVox recording %d.", resp.Status, i+1, f.p.LibrivoxID)
	}
	if total > maxFileBytes {
		return fmt.Errorf("section %d is larger than the Bridge's cap for one file", i+1)
	}
	if total > 0 {
		need := int64(math.Ceil(transfer.DefaultHeadroom * float64(total-start)))
		if free, err := h.o.Free(filepath.Join(f.mount, filepath.FromSlash(f.staging()))); err == nil && free < need {
			return fail(CodeDiskFull, "The library is short of space for section %d of the recording.", i+1)
		}
	}
	// The validators go into the state before the body: a stop during the
	// body resumes this file from what it holds.
	if f.st.Files == nil {
		f.st.Files = map[string]partState{}
	}
	ps := partState{Total: total}
	if netguard.StrongETag(resp.ETag) {
		ps.ETag = resp.ETag
	}
	f.st.Files[name] = ps
	if err := h.saveState(f.p.LibrivoxID, f.st); err != nil {
		return fmt.Errorf("recording a download's progress in the Bridge's data: %w", err)
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if appending {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	file, err := root.OpenFile(part, flags, 0o644)
	if err != nil {
		return writeFailure(err, part)
	}
	w := &writeErr{w: file}
	n, cerr := io.Copy(w, io.LimitReader(resp.Body, maxFileBytes-start+1))
	if err := file.Sync(); err != nil && w.err == nil {
		w.err = err
	}
	if err := file.Close(); err != nil && w.err == nil {
		w.err = err
	}
	size := start + n
	switch {
	case w.err != nil:
		return writeFailure(w.err, part)
	case size > maxFileBytes:
		_ = root.Remove(part)
		return fmt.Errorf("section %d is larger than the Bridge's cap for one file", i+1)
	case cerr != nil:
		return &retry{added: size > offset, cause: "network"}
	case total > 0 && size != total:
		if size > total {
			_ = root.Remove(part)
			delete(f.st.Files, name)
		}
		return &retry{added: size > offset, cause: "short"}
	}
	if err := root.Rename(part, final); err != nil {
		return writeFailure(err, final)
	}
	// The file under its own name is the record that it is finished: its
	// entry goes with the next save.
	delete(f.st.Files, name)
	return nil
}

// writeErr remembers a write's failure, so a failed copy tells the library's
// fault (disk_full, not_writable) from the connection's (try again).
type writeErr struct {
	w   io.Writer
	err error
}

func (w *writeErr) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

// cover fetches the project's cover into the staging folder as cover.jpg:
// best effort (a book without one is still a book), only from archive.org,
// and only when it is a JPEG.
func (h *Handler) cover(ctx context.Context, f *fetch, book metaops.LibrivoxBook) {
	if book.CoverartJPG == nil || *book.CoverartJPG == "" {
		return
	}
	root, err := os.OpenRoot(f.mount)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Lstat(path.Join(f.staging(), coverName)); err == nil {
		return
	}
	h.report(f.rep, jobs.Progress{Step: StepCover, Message: "Downloading the cover from archive.org"})
	log := h.o.Log.With("librivox_id", f.p.LibrivoxID)
	if !allowedRaw(*book.CoverartJPG) {
		log.Info("librivox: the cover isn't on archive.org; the book goes without one")
		return
	}
	resp, err := h.o.Fetcher.Fetch(ctx, netguard.Request{URL: *book.CoverartJPG, MaxBytes: maxCoverBytes, Allow: AllowedURL})
	switch {
	case err != nil:
		log.Info("librivox: the cover couldn't be downloaded; the book goes without one", "cause", netguard.Classify(err).Code)
		return
	case resp.Status != http.StatusOK || http.DetectContentType(resp.Body) != "image/jpeg":
		log.Info("librivox: the cover isn't a JPEG; the book goes without one")
		return
	}
	if err := fsutil.WriteRootAtomic(root, f.staging(), coverName, resp.Body, 0o644, nil); err != nil && !errors.Is(err, fs.ErrExist) {
		log.Warn("librivox: writing the cover failed; the book goes without one", "err", err)
	}
}

// coverName is the cover's file in the book's folder.
const coverName = "cover.jpg"
