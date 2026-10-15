package verify

import (
	"context"
	"encoding/json"
	"errors"
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
		func() mapping.BookSource { return src })
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
