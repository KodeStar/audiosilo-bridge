package generate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/chapterdetect"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// detectChapters leaves what chapter_detecting leaves for a single-file book: the
// detected manifest and a list of three chapters current for it.
func detectChapters(t *testing.T, b store.Book) {
	t.Helper()
	m := audio.Manifest{Source: b.SourcePath, Style: audio.StyleMarkers, Detected: true, Duration: 30, ChapterCount: 3}
	for i, title := range []string{"Prologue / Chapter 1", "Chapter 2", "Chapter 3"} {
		m.Chapters = append(m.Chapters, audio.Chapter{Chapter: i + 1, Title: title, Start: float64(i * 10), End: float64(i*10 + 10), Duration: 10})
	}
	if err := audio.WriteManifest(b.WorkDir, m); err != nil {
		t.Error(err)
		return
	}
	raw, _ := os.ReadFile(filepath.Join(b.WorkDir, audio.ManifestName)) //nolint:gosec // test
	sum := sha256.Sum256(raw)
	d := pipeline.DetectedChapters{ManifestSHA: hex.EncodeToString(sum[:]), DurationMs: 30000}
	if info, err := os.Stat(b.SourcePath); err == nil {
		d.SourceSize = info.Size()
	}
	for i, c := range m.Chapters {
		d.Chapters = append(d.Chapters, pipeline.DetectedChapter{Title: c.Title, StartMs: int64(i) * 10000, LengthMs: 10000, Parts: []int{i + 1}})
	}
	out, _ := json.Marshal(d)
	if err := os.WriteFile(pipeline.DetectedChaptersPath(b.WorkDir), out, 0o600); err != nil {
		t.Error(err)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// The chapters job runs an engine book with goal chapters to done (no agent stage),
// writes the book's chapters.json and bridge.json's chapters object, and asks the
// server to read them; a second job finds the chapters already found and writes them
// with no new run.
func TestChaptersDetectJob(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.windowed = true
	hr.srv.addKind(31, ChaptersKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(31); return s == link.StateDone })

	b := hr.engineBook(31)
	if b.Goal != state.GoalChapters || b.State != string(state.Done) {
		t.Fatalf("engine book = %+v", b)
	}
	if scheduler.SentinelExists(b.WorkDir, string(state.Sanitizing)) {
		t.Fatal("a chapters job ran past chapter_detecting")
	}
	_, final := hr.srv.final(31)
	var res ChaptersJobResult
	if err := json.Unmarshal(final.Result, &res); err != nil || res.Chapters != 3 || res.Source != "audio" ||
		strings.Join(res.Files, ",") != "Loose.chapters.json,Loose.bridge.json" {
		t.Fatalf("result = %s, %v", final.Result, err)
	}
	if hr.srv.saw(31, func(_ link.Update, p map[string]any) bool { _, ok := p["cost_usd"]; return ok }) {
		t.Fatal("a chapters job reported a spend")
	}
	var f ChaptersFile
	readJSON(t, filepath.Join(hr.root, AttachDir, "Loose.chapters.json"), &f)
	if f.Version != 1 || f.From != "narration" || f.Generator != "audiosilo-bridge" || len(f.Files) != 1 ||
		f.Files[0] != (ChaptersFileItem{Path: "Loose.m4b", Size: 6, DurationMs: 30000}) || len(f.Chapters) != 3 ||
		f.Chapters[0] != (ChaptersItem{Title: "Prologue / Chapter 1", StartMs: 0, LengthMs: 10000}) {
		t.Fatalf("chapters.json = %+v", f)
	}
	var prov Provenance
	readJSON(t, filepath.Join(hr.root, AttachDir, "Loose.bridge.json"), &prov)
	if prov.Kind != ChaptersKind || prov.JobID != 31 || prov.Chapters == nil || prov.Chapters.JobID != 31 ||
		prov.Chapters.From != "narration" || prov.Chapters.Chapters != 3 {
		t.Fatalf("bridge.json = %+v", prov)
	}
	hr.srv.mu.Lock()
	rescans := len(hr.srv.rescans)
	hr.srv.mu.Unlock()
	if rescans != 1 {
		t.Fatalf("rescans = %d", rescans)
	}

	// Again: the engine already has them, so they are written with no new run.
	hr.srv.addKind(32, ChaptersKind, "Loose.m4b", `{}`)
	hr.waitFor("the second done", func() bool { s, _ := hr.srv.final(32); return s == link.StateDone })
	if _, err := hr.db.GetGenerateBook(context.Background(), 32); err == nil {
		t.Fatal("the second job made an engine run")
	}
	readJSON(t, filepath.Join(hr.root, AttachDir, "Loose.bridge.json"), &prov)
	if prov.JobID != 31 || prov.Chapters == nil || prov.Chapters.JobID != 32 {
		t.Fatalf("bridge.json after the second job = %+v (the first job's fields kept, the chapters the second's)", prov)
	}
}

// A book whose audio already has chapters fails with the inspect's sentence as it
// is.
func TestChaptersDetectBookWithChapters(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.inspectErr = pipeline.ErrHasMarkerChapters
	hr.srv.addKind(33, ChaptersKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	hr.waitFor("failed", func() bool { s, _ := hr.srv.final(33); return s == link.StateFailed })
	_, final := hr.srv.final(33)
	if final.Error == nil || *final.Error != pipeline.ErrHasMarkerChapters.Error() {
		t.Fatalf("error = %v, want the sentence", final.Error)
	}
}

// A chapters job for a book a generate job is working on is refused (the generate
// run brings the chapters).
func TestChaptersDetectRefusedWhileGenerating(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	gate := hr.exec.gate("asr")
	defer close(gate)
	hr.srv.add(34, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	hr.waitFor("the generate job at asr", func() bool {
		return hr.srv.saw(34, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	hr.srv.addKind(35, ChaptersKind, "Loose.m4b", `{}`)
	hr.waitFor("refused", func() bool { s, _ := hr.srv.final(35); return s == link.StateFailed })
	if _, final := hr.srv.final(35); final.Error == nil || *final.Error != string(errChaptersGenerating) {
		t.Fatalf("error = %v", final.Error)
	}
}

// A generate job for a book a chapters job finished continues that engine book (its
// transcription kept) instead of deleting it, and lists the chapters with the rest.
func TestGenerateContinuesAChaptersRun(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.windowed = true
	hr.srv.addKind(36, ChaptersKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	hr.waitFor("the chapters job done", func() bool { s, _ := hr.srv.final(36); return s == link.StateDone })
	first := hr.engineBook(36)

	hr.srv.add(37, "Loose.m4b", `{"contribute":"local","work_id":"loose-work"}`)
	hr.waitFor("the generate job done", func() bool { s, _ := hr.srv.final(37); return s == link.StateDone })
	b := hr.engineBook(37)
	if b.ID != first.ID || b.Goal != "" || b.WorkID != "loose-work" || b.ContribMode != "local" {
		t.Fatalf("the generate job's book = %+v, want the chapters run (%d) continued", b, first.ID)
	}
	_, final := hr.srv.final(37)
	var res Result
	if err := json.Unmarshal(final.Result, &res); err != nil ||
		strings.Join(res.Files, ",") != "Loose.characters.json,Loose.recaps.json,Loose.chapters.json,Loose.bridge.json" {
		t.Fatalf("result = %s", final.Result)
	}
	var prov Provenance
	readJSON(t, filepath.Join(hr.root, AttachDir, "Loose.bridge.json"), &prov)
	if prov.Kind != Kind || prov.JobID != 37 || prov.Chapters == nil || prov.Chapters.JobID != 37 {
		t.Fatalf("bridge.json = %+v", prov)
	}
}

// A generate run of a chapterless book writes chapters.json as soon as the engine
// found the chapters, before the characters and recaps exist.
func TestGenerateWritesChaptersEarly(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.windowed = true
	gate := hr.exec.gate("fact_pass")
	hr.srv.add(38, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	chapters := filepath.Join(hr.root, AttachDir, "Loose.chapters.json")
	hr.waitFor("chapters.json before the facts", func() bool {
		_, err := os.Stat(chapters)
		_, perr := os.Stat(filepath.Join(hr.root, AttachDir, "Loose.bridge.json")) // written after it
		return err == nil && perr == nil
	})
	if _, err := os.Stat(filepath.Join(hr.root, AttachDir, "Loose.characters.json")); err == nil {
		t.Fatal("characters.json was there already")
	}
	var prov Provenance
	readJSON(t, filepath.Join(hr.root, AttachDir, "Loose.bridge.json"), &prov)
	if prov.Kind != Kind || prov.Chapters == nil || prov.Chapters.JobID != 38 {
		t.Fatalf("early bridge.json = %+v", prov)
	}
	close(gate)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(38); return s == link.StateDone })
	readJSON(t, filepath.Join(hr.root, AttachDir, "Loose.bridge.json"), &prov)
	if prov.Chapters == nil || prov.Chapters.Chapters != 3 {
		t.Fatalf("final bridge.json lost the chapters: %+v", prov)
	}
}

func TestComposeChapters(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	d := pipeline.DetectedChapters{DurationMs: 20000, Chapters: []pipeline.DetectedChapter{
		{Title: "One\x07", StartMs: 0, LengthMs: 10000}, {Title: "  ", StartMs: 10000, LengthMs: 10000}}}
	raw, err := composeChapters(d, []link.BookFile{{Path: "Author/Book/book.mp3", Size: 9}}, "Author/Book", false, now)
	if err != nil {
		t.Fatal(err)
	}
	var f ChaptersFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.GeneratedAt != "2026-10-17T12:00:00Z" || f.Files[0].Path != "book.mp3" || f.Files[0].DurationMs != 20000 ||
		f.Chapters[0].Title != "One" || f.Chapters[1].Title != "Chapter 2" {
		t.Fatalf("chapters.json = %s", raw)
	}
	// A folder book's paths are relative to its folder, subfolders kept, in the
	// server's order (the server compares count, order, path and size).
	raw, _ = composeChapters(d, []link.BookFile{{Path: "A/B/CD1/01.mp3", Size: 1}, {Path: "A/B/CD2/01.mp3", Size: 2}}, "A/B", false, now)
	f = ChaptersFile{}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Files) != 2 || f.Files[0].Path != "CD1/01.mp3" ||
		f.Files[1] != (ChaptersFileItem{Path: "CD2/01.mp3", Size: 2}) {
		t.Fatalf("folder files = %+v, %v", f.Files, err)
	}
	// A one-chapter book gets no file.
	d.Chapters = d.Chapters[:1]
	if raw, err := composeChapters(d, []link.BookFile{{Path: "x.mp3"}}, "x.mp3", true, now); raw != nil || err != nil {
		t.Fatalf("one chapter: %s, %v", raw, err)
	}
	if got := chapterTitle(strings.Repeat("é", 150), 1); len(got) > chapterdetect.MaxTitleBytes || !strings.HasPrefix(got, "é") {
		t.Fatalf("long title %d bytes", len(got))
	}
}

// keptObject vouches only for a chapters.json in place beside the bridge.json
// that names it.
func TestKeptChapters(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, AttachDir, "bridge.json"), `{"kind":"sidecars.generate","chapters":{"job_id":4,"from":"narration","chapters":12}}`)
	if c := keptObject[ChaptersProvenance](dir, "", chaptersName, "chapters"); c != nil {
		t.Fatalf("no chapters.json, but kept %+v", c)
	}
	mustWrite(t, filepath.Join(dir, AttachDir, "chapters.json"), `{}`)
	if c := keptObject[ChaptersProvenance](dir, "", chaptersName, "chapters"); c == nil || c.JobID != 4 || c.Chapters != 12 {
		t.Fatalf("kept = %+v", c)
	}
}

// refreshChapters reads and checks the detected list again only once its files
// changed: the generate job looks at every poll.
func TestRefreshChaptersOnlyWhenChanged(t *testing.T) {
	t.Parallel()
	b := store.Book{WorkDir: t.TempDir(), SourcePath: filepath.Join(t.TempDir(), "Loose.m4b")}
	if refreshed, ok := New(Options{}).refreshChapters(&run{}, b); ok || len(refreshed.Chapters) != 0 {
		t.Fatal("a list before there is one")
	}
	detectChapters(t, b)
	h := New(Options{})
	r := &run{files: []link.BookFile{{Path: "Loose.m4b", Size: 6}}, rel: "Loose.m4b", place: place{single: true}}
	d, ok := h.refreshChapters(r, b)
	if !ok || len(d.Chapters) != 3 || r.chapters == nil {
		t.Fatalf("first look = %+v, %v (chapters %s)", d, ok, r.chapters)
	}
	// Same size and time, different bytes: proof the list wasn't read again.
	path := pipeline.DetectedChaptersPath(b.WorkDir)
	info, _ := os.Stat(path)
	raw, _ := os.ReadFile(path) //nolint:gosec // test
	garbled := strings.Repeat("x", len(raw))
	mustWrite(t, path, garbled)
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if again, ok := h.refreshChapters(r, b); !ok || len(again.Chapters) != 3 {
		t.Fatalf("an unchanged list was read again: %+v, %v", again, ok)
	}
	later := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.refreshChapters(r, b); ok {
		t.Fatal("a changed list wasn't read again")
	}
}

// A chapters job that comes back (the Bridge restarted) to a book that moved while
// it was away follows the book and records the move, as a generate job does.
func TestChaptersDetectFollowsABookMovedWhileAway(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.windowed = true
	gate := hr.exec.gate("asr")
	hr.srv.addKind(39, ChaptersKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	hr.waitFor("asr", func() bool {
		return hr.srv.saw(39, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	stop()
	if err := os.Rename(filepath.Join(hr.root, "Loose.m4b"), filepath.Join(hr.root, "Moved.m4b")); err != nil {
		t.Fatal(err)
	}
	hr.srv.set(func() {
		hr.srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Moved.m4b", Files: []link.BookFile{{Path: "Moved.m4b", Size: 6}},
			Info: &link.BookInfo{Title: "Loose"}}
	})
	close(gate)
	h := hr.handler()
	stop2 := hr.runner(h, h.ChaptersJob())
	defer stop2()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(39); return s == link.StateDone })
	if g, _ := hr.db.GetGenerateBook(context.Background(), 39); g.RelPath != "Moved.m4b" {
		t.Fatalf("mapping = %+v, want the new path", g)
	}
	if _, err := os.Stat(filepath.Join(hr.root, AttachDir, "Moved.chapters.json")); err != nil {
		t.Fatal(err)
	}
}
