//go:build unix

package generate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bridge.json read back for a description-only job is never read through a FIFO
// (the open never blocks) or through a .audiosilo that is a link out of the book's
// folder: either is written fresh.
func TestDescribedProvenanceRefusesSpecialFiles(t *testing.T) {
	t.Parallel()
	fresh := provenance("0.3.0", DescribeKind, 9, time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), SourceAudio, "w", nil)
	fresh.Description = &DescriptionProvenance{JobID: 9, From: descriptionFrom, Chapters: 3, CheckedAgainst: "none"}

	dir := t.TempDir()
	adir := filepath.Join(dir, AttachDir)
	if err := os.MkdirAll(adir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(adir, "bridge.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan map[string]json.RawMessage, 1)
	go func() { done <- readProvenance(adir, "bridge.json") }()
	select {
	case m := <-done:
		if m != nil {
			t.Fatalf("a FIFO read as %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}

	other := t.TempDir()
	mustWrite(t, filepath.Join(other, "bridge.json"), `{"kind":"leaked"}`)
	linked := t.TempDir()
	if err := os.Symlink(other, filepath.Join(linked, AttachDir)); err != nil {
		t.Fatal(err)
	}
	raw, err := describedProvenance(linked, "", fresh)
	if err != nil || strings.Contains(string(raw), "leaked") {
		t.Fatalf("read through a linked .audiosilo: %s, %v", raw, err)
	}
}
