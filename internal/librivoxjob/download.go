package librivoxjob

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
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

// copyBufBytes is how much of a section's body is gathered before a write.
const copyBufBytes = 1 << 20

// download fetches every section not in the staging folder yet: a file there
// under its own name is finished (it is renamed from its .part only once
// whole), so a resumed run skips it and goes on with the next, resuming that
// one's .part with Range/If-Range when its strong ETag was recorded.
func (h *Handler) download(ctx context.Context, f *fetch, sections []metaops.LibrivoxSection, names []string) error {
	n := len(sections)
	for i, s := range sections {
		// The mapping again before each section's writes: one that went away
		// parks the job (not_writable) with what it fetched, until it is back.
		if err := h.recheck(ctx, f, StepDownload); err != nil {
			return err
		}
		h.report(f.rep, jobs.Progress{Step: StepDownload, Message: fmt.Sprintf("Downloading section %d of %d from archive.org", i+1, n),
			Done: i, Total: n})
		if err := h.fetchSection(ctx, f, i, s.ListenURL, names[i]); err != nil {
			return err
		}
	}
	h.report(f.rep, jobs.Progress{Step: StepDownload, Message: "Downloaded from archive.org", Done: n, Total: n})
	return nil
}

// fetchSection downloads section i unless the staging folder holds it already.
// The library is opened for each section: a handle opened before a park may
// be on a mount that went away since.
func (h *Handler) fetchSection(ctx context.Context, f *fetch, i int, rawURL, name string) error {
	root, err := f.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if st, err := root.Lstat(path.Join(f.staging(), name)); err == nil && st.Mode().IsRegular() {
		return nil
	}
	return h.section(ctx, root, f, i, rawURL, name)
}

// retry is an attempt that may be tried again: after retryAfter when the
// server said so. size is what the partial holds after it (0 when the attempt
// wrote nothing or the partial went).
type retry struct {
	retryAfter int
	size       int64
	cause      string
}

func (r *retry) Error() string { return r.cause }

// partSize is what a section's partial holds, 0 when there is none.
func partSize(root *os.Root, part string) int64 {
	if st, err := root.Lstat(part); err == nil && st.Mode().IsRegular() {
		return st.Size()
	}
	return 0
}

// section downloads one section to <staging>/<name>, trying again while
// attempts add to the partial (downloadAttempts in a row that add nothing
// fail archive_unavailable).
func (h *Handler) section(ctx context.Context, root *os.Root, f *fetch, i int, rawURL, name string) error {
	// high is the most the partial has held. Only an attempt that takes it
	// past that added to it: one that started the file again from zero adds
	// nothing until it does, however its size compares with the last cut's.
	high := partSize(root, path.Join(f.staging(), name)+partSuffix)
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
		if r.size > high {
			high, failures = r.size, 0
		} else {
			failures++
		}
		if failures >= downloadAttempts {
			return fail(CodeArchiveUnavailable, "archive.org didn't deliver section %d of LibriVox recording %d (%s): try again later.",
				i+1, f.p.LibrivoxID, r.cause)
		}
		h.o.Log.Info("librivox: a section's download failed; trying again", "librivox_id", f.p.LibrivoxID, "section", i+1,
			"failures", failures, "cause", r.cause)
		backoff := max(failures, 1)
		wait := max(time.Duration(backoff*backoff)*2*time.Second, min(time.Duration(r.retryAfter)*time.Second, maxRetryAfter))
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
	offset := partSize(root, part)
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
	if !audioType(resp.ContentType) {
		// A page where a file was asked for (an error or a login page with a
		// 200): never written, tried again like a dropped connection.
		return &retry{size: offset, cause: "not_audio"}
	}
	if total > maxFileBytes {
		return fmt.Errorf("section %d is larger than the Bridge's cap for one file", i+1)
	}
	// A body from its start replaces the partial, which goes before the new
	// validators are recorded: a stop between the two must never leave the old
	// bytes under validators that would prove a later 206 continues them.
	if !appending {
		if err := root.Remove(part); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return writeFailure(err, part)
		}
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
	// The ETag only beside a stated size: without one no 206 could be proven
	// to continue the partial, so the next attempt asks for the whole file.
	ps := partState{Total: total}
	if total > 0 && netguard.StrongETag(resp.ETag) {
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
	// Buffered: a body arrives in reads of a TLS record at most, and each as
	// its own write is a round trip on a network mount. The flush runs after a
	// cut connection too, so the partial holds all that arrived.
	w := &writeErr{w: file}
	buf := bufio.NewWriterSize(w, copyBufBytes)
	n, cerr := io.Copy(buf, io.LimitReader(resp.Body, maxFileBytes-start+1))
	if err := buf.Flush(); err != nil && w.err == nil {
		w.err = err
	}
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
		return &retry{size: size, cause: "network"}
	case size == 0, total > 0 && size != total:
		// An empty body is never a section, whatever length the response
		// stated; one longer than stated can't be trusted either.
		if size > total {
			_ = root.Remove(part)
			delete(f.st.Files, name)
			size = 0
		}
		return &retry{size: size, cause: "short"}
	}
	if !audioHead(root, part) {
		// Whole but not audio: it goes, and the section starts again.
		_ = root.Remove(part)
		delete(f.st.Files, name)
		return &retry{cause: "not_audio"}
	}
	if err := root.Rename(part, final); err != nil {
		return writeFailure(err, final)
	}
	// The file under its own name is the record that it is finished: its
	// entry goes with the next save.
	delete(f.st.Files, name)
	return nil
}

// audioType reports whether a response's Content-Type may carry a section:
// none stated, audio/*, or a generic binary type. A text, HTML, JSON or other
// typed answer is a page, not the file.
func audioType(contentType string) bool {
	if strings.TrimSpace(contentType) == "" {
		return true
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mt, "audio/") || mt == "application/octet-stream" || mt == "binary/octet-stream" ||
		mt == "application/ogg"
}

// audioHead reports whether a finished section's first bytes are audio: an
// ID3 tag or an MPEG frame sync (LibriVox's MP3s), else the Ogg, FLAC or MP4
// signature a section of another extension starts with.
func audioHead(root *os.Root, name string) bool {
	file, err := root.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, 12)
	n, _ := io.ReadFull(file, head)
	return isAudioHead(head[:n])
}

func isAudioHead(b []byte) bool {
	switch {
	case bytes.HasPrefix(b, []byte("ID3")), bytes.HasPrefix(b, []byte("OggS")), bytes.HasPrefix(b, []byte("fLaC")):
		return true
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return true
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return true
	}
	return false
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
	root, err := f.openRoot()
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
