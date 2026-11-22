package audible

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/progress"
)

// DefaultMaxDownloadBytes caps one book's download when the caller sets no cap.
// The longest high-quality AAXC files are around 2 GB.
const DefaultMaxDownloadBytes int64 = 4 << 30

// ErrTooLarge is a download refused for passing its size cap. Nothing of it is
// kept.
var ErrTooLarge = errors.New("the download is larger than the cap")

// downloadStall ends a download attempt that hears nothing for this long: no
// answer to its request, or no byte of the body (a connection a CDN or a proxy
// keeps open without sending). What arrived stays for the next attempt.
const downloadStall = 2 * time.Minute

// ErrStalled is a download attempt ended for hearing nothing for a while (the
// caller's context still live): a dropped connection, for the caller to resume.
var ErrStalled = errors.New("the download stalled: nothing arrived for a while")

// DownloadOptions shape one Download.
type DownloadOptions struct {
	// MaxBytes caps the whole file (resumed bytes included); 0 is
	// DefaultMaxDownloadBytes.
	MaxBytes int64
	// ExpectedSize is the file's size as the license gave it (0: unknown). A
	// partial file resumes only into a file of the size it was started on.
	ExpectedSize int64
	// BeforeWrite (may be nil) runs once the file's size is known (total, -1
	// when the server sent none) and before a byte is written, with the bytes a
	// resumed partial already holds: the caller's free-space check. Its error
	// ends the download with the partial kept.
	BeforeWrite func(total, held int64) error
	// OnProgress (may be nil) reports bytes held so far and the file's total
	// (-1 when the server sends none), throttled.
	OnProgress func(copied, total int64)
}

// partialState is what Download records beside a file it starts (StatePath):
// the size the server announced and its validators. A partial file resumes only
// with it, so a Range never appends another file's bytes, even under a new
// license's URL: the validator goes out as If-Range (a changed file answers 200,
// the whole of it), and the 206's total must be the size recorded (or the
// license's).
type partialState struct {
	Size         int64  `json:"size,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

// StatePath is where Download records what it knows of the partial file at
// dest (dest + ".state").
func StatePath(dest string) string { return dest + ".state" }

// RemovePartial deletes a partial download and its state file (a cancel, or a
// job's terminal end).
func RemovePartial(dest string) {
	_ = os.Remove(dest)
	_ = os.Remove(StatePath(dest))
}

func readState(dest string) (partialState, bool) {
	b, err := os.ReadFile(StatePath(dest)) //nolint:gosec // G304: the caller's own temp path
	if err != nil {
		return partialState{}, false
	}
	var st partialState
	if json.Unmarshal(b, &st) != nil {
		return partialState{}, false
	}
	return st, true
}

// ifRange is the state's validator for If-Range: a strong ETag, else the
// Last-Modified date ("" for none).
func (st partialState) ifRange() string {
	if st.ETag != "" && !strings.HasPrefix(st.ETag, "W/") {
		return st.ETag
	}
	return st.LastModified
}

// Download fetches a content URL to dest, resuming a partial dest left by an
// earlier attempt, under this URL or another (a new license's), only when it
// can tell it is the same file: the state Download recorded when it started
// the file (StatePath) must exist, agree with opts.ExpectedSize when both
// know a size, and hold a validator (sent as If-Range) or a size. A 206 that
// starts where dest ends, for a file of that size, is appended; a 200 (If-Range
// found another file) starts dest again from its body, and anything else (a
// 206 from elsewhere, a 416 for another size) starts dest again with a new
// request. A failure part way leaves what arrived in dest (a refusal for size
// removes it, and its state), for the next attempt to resume.
//
// The content-delivery URL is served by Audible's CDS and validates the same
// device RSA signature as the API - audible-cli downloads it with the
// authenticated session, not a plain GET, so an unsigned request returns HTTP
// 403. We sign the request (and re-sign on each redirect, since a redirect to a
// presigned CDN ignores the headers but a redirect to another CDS path needs a
// fresh signature for that path).
func (c *Client) Download(ctx context.Context, downloadURL, dest string, opts DownloadOptions) error {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDownloadBytes
	}
	var have int64
	if info, err := os.Stat(dest); err == nil && info.Mode().IsRegular() {
		have = info.Size()
	}
	st, known := readState(dest)
	want := st.Size
	if want <= 0 {
		want = opts.ExpectedSize
	}
	if have > maxBytes {
		RemovePartial(dest)
		return ErrTooLarge
	}
	if have > 0 && (!known || (opts.ExpectedSize > 0 && st.Size > 0 && st.Size != opts.ExpectedSize) ||
		(want <= 0 && st.ifRange() == "") || (want > 0 && have > want)) {
		// Nothing proves this partial is the file the URL serves.
		RemovePartial(dest)
		have = 0
	}
	// A dedicated client with no overall timeout - a large audiobook can exceed the
	// API client's per-request timeout; cancellation is governed by ctx. Redirects
	// (CDS -> CDN) are followed, re-signed for the new URL.
	client := &http.Client{
		Transport: c.set.http.Transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return c.signGet(r)
		},
	}
	// Each attempt runs under its own context, which a watchdog ends when
	// nothing arrives for the stall time (reset by every byte of the body).
	var (
		attemptCtx    context.Context
		attemptCancel context.CancelCauseFunc = func(error) {}
		watchdog      *time.Timer
	)
	endAttempt := func() {
		if watchdog != nil {
			watchdog.Stop()
		}
		attemptCancel(nil)
	}
	defer endAttempt()
	resp, err := sendWithBackoff(ctx, client, c.set.sleep, func() (*http.Request, error) {
		endAttempt()
		var cancel context.CancelCauseFunc
		attemptCtx, cancel = context.WithCancelCause(ctx)
		attemptCancel = cancel
		watchdog = time.AfterFunc(c.set.stall, func() { cancel(ErrStalled) })
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, downloadURL, nil)
		if err != nil {
			return nil, err
		}
		if have > 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
			if v := st.ifRange(); v != "" {
				req.Header.Set("If-Range", v)
			}
		}
		return req, c.signGet(req)
	})
	if err != nil {
		return stalled(ctx, attemptCtx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// restart drops dest and downloads it again from the start, without a
	// Range. A partial it can't remove ends the download instead (another
	// request would ask for the same Range again).
	restart := func() error {
		endAttempt()
		_ = resp.Body.Close()
		if err := os.Remove(dest); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		_ = os.Remove(StatePath(dest))
		return c.Download(ctx, downloadURL, dest, opts)
	}

	// held is what the partial already holds of the file (only when the server
	// sent the rest of it); total is the file's whole size (-1: unknown).
	var held, total int64
	switch {
	case resp.StatusCode == http.StatusPartialContent && have > 0:
		start, size, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != have || (want > 0 && size != want) {
			return restart() // not the rest of our file
		}
		held, total = have, size
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && have > 0:
		// dest may already be whole: the server says how long the file is.
		contentRange := resp.Header.Get("Content-Range")
		if contentRange == "" && want == have {
			return nil // the recorded or licensed length confirms the partial is whole
		}
		if _, size, ok := parseContentRange(contentRange); ok && size == have && (want <= 0 || want == have) {
			return nil
		}
		return restart()
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		total = resp.ContentLength
		if total < 0 && opts.ExpectedSize > 0 {
			// No length sent: the license's size is the file's, so a body cut
			// short still fails (io.ErrUnexpectedEOF) instead of passing whole.
			total = opts.ExpectedSize
		}
	default:
		return &httpError{StatusCode: resp.StatusCode, Method: http.MethodGet, Path: "download"}
	}
	if total > maxBytes {
		RemovePartial(dest)
		return ErrTooLarge
	}
	if opts.BeforeWrite != nil {
		if err := opts.BeforeWrite(total, held); err != nil {
			return err
		}
	}

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if held > 0 {
		flags = os.O_WRONLY | os.O_APPEND
	} else {
		fresh := partialState{Size: max(total, 0), ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
		b, _ := json.Marshal(fresh)
		if err := os.WriteFile(StatePath(dest), b, 0o600); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(dest, flags, 0o600) //nolint:gosec // the caller's own temp path
	if err != nil {
		return err
	}
	// Read at most one byte past the cap, to tell a file at the cap from one over it.
	src := io.LimitReader(&stallReader{r: resp.Body, t: watchdog, d: c.set.stall}, maxBytes-held+1)
	if opts.OnProgress != nil {
		cb := opts.OnProgress
		src = &progress.Reader{R: src, Total: total, CB: func(copied, t int64) { cb(held+copied, t) }}
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if held+n > maxBytes {
		RemovePartial(dest)
		return ErrTooLarge
	}
	if err != nil {
		return stalled(ctx, attemptCtx, err)
	}
	if total >= 0 && held+n != total {
		return fmt.Errorf("download ended at %d of %d bytes: %w", held+n, total, io.ErrUnexpectedEOF)
	}
	return nil
}

// stalled is err, or ErrStalled when the stall watchdog ended the attempt (and
// not the caller's context).
func stalled(ctx, attempt context.Context, err error) error {
	if ctx.Err() == nil && attempt != nil && errors.Is(context.Cause(attempt), ErrStalled) {
		return ErrStalled
	}
	return err
}

// stallReader resets the stall watchdog each time bytes arrive.
type stallReader struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.t.Reset(s.d)
	}
	return n, err
}

// parseContentRange reads "bytes start-end/size" (size "*" is unknown, -1).
func parseContentRange(v string) (start, size int64, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !found {
		return 0, 0, false
	}
	span, sizeStr, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	size = -1
	if sizeStr != "*" {
		n, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		size = n
	}
	if span == "*" {
		return 0, size, size >= 0
	}
	first, _, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	return start, size, true
}

// signGet sets the Audible user agent and an RSA signature for a GET request's
// URL (a redirect's hop doesn't pass through sendWithBackoff).
func (c *Client) signGet(req *http.Request) error {
	req.Header.Set("User-Agent", userAgent)
	return signRequest(req, nil, c.key, c.creds.ADPToken, c.now())
}
