package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
)

type libs map[int64]link.Library

func (l libs) Library(id int64) (link.Library, bool) { v, ok := l[id]; return v, ok }

type books map[string]*link.Book

func (b books) Book(_ context.Context, _ int64, path string) (*link.Book, error) {
	if bk, ok := b[path]; ok {
		return bk, nil
	}
	return nil, link.ErrNoBook
}

// setup writes a library with a folder book and a single-file book.
func setup(t *testing.T) (*Handler, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Author", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) link.BookFile {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return link.BookFile{Path: rel, Size: int64(len(body))}
	}
	folder := &link.Book{LibraryID: 1, Path: "Author/Book", Files: []link.BookFile{write("Author/Book/1.mp3", "abc"), write("Author/Book/2.mp3", "defg")}}
	single := &link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{write("Loose.m4b", "xyz")}}
	src := books{"": folder, "Author/Book": folder, "Loose.m4b": single}
	h := New(libs{1: {ID: 1, Name: "Books", Root: "/books"}},
		func(int64, string) (string, bool) { return root, true },
		func() mapping.BookSource { return src }, nil)
	return h, root
}

func ptr[T any](v T) *T { return &v }

func TestVerifyFolderBook(t *testing.T) {
	t.Parallel()
	h, root := setup(t)
	rep := jobs.LocalReporter()
	out, err := h.Run(context.Background(), jobs.Job{ID: 1, Kind: Kind, LibraryID: ptr(int64(1)), Path: ptr("Author/Book")}, rep)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(Result)
	if res.Path != "Author/Book" || len(res.Files) != 2 || !res.Files[1].OK || !res.Writable || res.MountPath != root {
		t.Fatalf("result = %+v", res)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "Author", "Book", mapping.ProbePrefix+"*")); len(left) != 0 {
		t.Fatalf("probe left: %v", left)
	}
	if rep.Last().Step == "" {
		t.Fatal("no progress recorded")
	}
	// The result is what the console reads.
	raw, _ := json.Marshal(res)
	for _, key := range []string{`"library_id":1`, `"mount_path"`, `"files":[`, `"expected_size":3`, `"writable":true`, `"elapsed_ms"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("result JSON lacks %s: %s", key, raw)
		}
	}
}

func TestVerifySampleAndSingleFile(t *testing.T) {
	t.Parallel()
	h, _ := setup(t)
	out, err := h.Run(context.Background(), jobs.Job{LibraryID: ptr(int64(1))}, jobs.LocalReporter())
	if err != nil || out.(Result).Path != "Author/Book" {
		t.Fatalf("sample = %+v, %v", out, err)
	}
	out, err = h.Run(context.Background(), jobs.Job{LibraryID: ptr(int64(1)), Path: ptr("Loose.m4b")}, jobs.LocalReporter())
	if err != nil || !out.(Result).Writable {
		t.Fatalf("single file = %+v, %v", out, err)
	}
}

func TestVerifyMismatchFailsWithAResult(t *testing.T) {
	t.Parallel()
	h, root := setup(t)
	if err := os.WriteFile(filepath.Join(root, "Author", "Book", "2.mp3"), []byte("changed size"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := h.Run(context.Background(), jobs.Job{LibraryID: ptr(int64(1)), Path: ptr("Author/Book")}, jobs.LocalReporter())
	if err == nil || !strings.Contains(err.Error(), "size differs") {
		t.Fatalf("err = %v", err)
	}
	if res := out.(Result); res.Files[1].OK || res.Writable {
		t.Fatalf("a mismatched copy = %+v", res)
	}
}

func TestVerifyRefusals(t *testing.T) {
	t.Parallel()
	h, _ := setup(t)
	for name, job := range map[string]jobs.Job{
		"no library":      {},
		"unknown library": {LibraryID: ptr(int64(9))},
		"bad params":      {LibraryID: ptr(int64(1)), Params: json.RawMessage(`[1]`)},
		"hold too long":   {LibraryID: ptr(int64(1)), Params: json.RawMessage(`{"hold_seconds":601}`)},
		"no such book":    {LibraryID: ptr(int64(1)), Path: ptr("Nope")},
	} {
		if _, err := h.Run(context.Background(), job, jobs.LocalReporter()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	h.books = func() mapping.BookSource { return nil }
	if _, err := h.Run(context.Background(), jobs.Job{LibraryID: ptr(int64(1))}, jobs.LocalReporter()); err == nil {
		t.Error("unpaired: no error")
	}
}

func TestVerifyHoldStopsOnCancel(t *testing.T) {
	t.Parallel()
	h, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	rep := jobs.LocalReporter()
	go func() {
		for rep.Last().Step != "hold" {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	out, err := h.Run(ctx, jobs.Job{LibraryID: ptr(int64(1)), Params: json.RawMessage(`{"hold_seconds":60}`)}, rep)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 10*time.Second {
		t.Fatalf("hold: %v after %v", err, time.Since(start))
	}
	if last := rep.Last(); last.Done < 1 || last.Total != 60 {
		t.Fatalf("hold progress = %+v", last)
	}
	if out.(Result).ElapsedMS <= 0 {
		t.Fatal("no elapsed time")
	}
}

// flaky answers the book read with err for its first fails asks, then as src.
type flaky struct {
	src   mapping.BookSource
	err   error
	fails int
	asks  int
}

func (f *flaky) Book(ctx context.Context, lib int64, path string) (*link.Book, error) {
	f.asks++
	if f.asks <= f.fails {
		return nil, f.err
	}
	return f.src.Book(ctx, lib, path)
}

// An answer that may change from the server's book read (a 5xx in a restart,
// no answer, a refused token the runner strands the job for, the request rate's
// 429) is asked again with backoff and the verify goes on; one that lasts past
// the budget fails it with the server's answer, and a definite 4xx fails it at
// once.
func TestVerifyRetriesATransientBookRead(t *testing.T) {
	t.Parallel()
	h, _ := setup(t)
	// A budget no stall on a loaded runner reaches: the waits are 1 and 2 ms.
	h.retryFirst, h.retryFor = time.Millisecond, time.Minute
	var logged bytes.Buffer
	h.log = slog.New(slog.NewTextHandler(&logged, nil))
	src := h.books()
	job := jobs.Job{LibraryID: ptr(int64(1)), Path: ptr("Author/Book")}
	use := func(f *flaky) { h.books = func() mapping.BookSource { return f } }

	for name, transient := range map[string]error{
		"500":       &link.APIError{Status: http.StatusInternalServerError},
		"429":       &link.APIError{Status: http.StatusTooManyRequests},
		"no answer": errors.New("dial tcp: connection refused"),
		"refused":   link.ErrUnauthorized,
		"timed out": context.DeadlineExceeded,
		"503":       &link.APIError{Status: http.StatusServiceUnavailable},
	} {
		down := &flaky{src: src, err: transient, fails: 2}
		use(down)
		if out, err := h.Run(context.Background(), job, jobs.LocalReporter()); err != nil || !out.(Result).Writable {
			t.Fatalf("after two %s = %+v, %v; want verified", name, out, err)
		}
		if n := down.asks; n != 3 {
			t.Fatalf("%s: asks = %d, want 3", name, n)
		}
		// Each retry is in the log: the job's message doesn't change.
		if n := strings.Count(logged.String(), "level=WARN"); n != 2 || !strings.Contains(logged.String(), "trying again") {
			t.Fatalf("%s: %d retries logged, want 2: %s", name, n, logged.String())
		}
		logged.Reset()
	}

	h.retryFor = 50 * time.Millisecond
	gone := &flaky{src: src, err: &link.APIError{Status: http.StatusBadGateway}, fails: 1 << 30}
	use(gone)
	start := time.Now()
	var apiErr *link.APIError
	if _, err := h.Run(context.Background(), job, jobs.LocalReporter()); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway {
		t.Fatalf("a lasting 502 = %v; want it as the failure", err)
	}
	if n := gone.asks; n < 2 {
		t.Fatalf("a lasting 502 was asked %d times, want retries", n)
	}
	// The last ask is at the budget's end, not a backoff step short of it.
	if took := time.Since(start); took < h.retryFor {
		t.Fatalf("a lasting 502 gave up after %v, before the %v budget", took, h.retryFor)
	}

	forbidden := &flaky{src: src, err: &link.APIError{Status: http.StatusForbidden}, fails: 1}
	use(forbidden)
	if _, err := h.Run(context.Background(), job, jobs.LocalReporter()); !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("a 403 = %v; want it as the failure", err)
	}
	if n := forbidden.asks; n != 1 {
		t.Fatalf("a 403 was asked %d times, want once", n)
	}
}

// bookFunc is a BookSource from a func.
type bookFunc func(ctx context.Context, lib int64, path string) (*link.Book, error)

func (f bookFunc) Book(ctx context.Context, lib int64, path string) (*link.Book, error) {
	return f(ctx, lib, path)
}

// A cancel during the book read's backoff ends the verify at once, not after
// the wait.
func TestVerifyBookReadBackoffStopsOnCancel(t *testing.T) {
	t.Parallel()
	h, _ := setup(t)
	h.retryFirst, h.retryFor = time.Hour, 2*time.Hour
	asked := make(chan struct{}, 1)
	h.books = func() mapping.BookSource {
		return bookFunc(func(context.Context, int64, string) (*link.Book, error) {
			asked <- struct{}{}
			return nil, &link.APIError{Status: http.StatusServiceUnavailable}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.Run(ctx, jobs.Job{LibraryID: ptr(int64(1))}, jobs.LocalReporter())
		done <- err
	}()
	<-asked
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the verify waited out its backoff after a cancel")
	}
}
