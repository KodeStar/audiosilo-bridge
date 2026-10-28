package generate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/chapterdetect"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
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
		d.SourceSize, d.SourceModTime = info.Size(), info.ModTime().UnixNano()
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
	hr.srv.set(func() {
		hr.srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{{Path: "Loose.m4b", Size: 6}},
			Info: &link.BookInfo{Title: "Loose", Series: "The Saga", SeriesIndex: "1"}}
	})
	hr.srv.addKind(36, ChaptersKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	hr.waitFor("the chapters job done", func() bool { s, _ := hr.srv.final(36); return s == link.StateDone })
	first := hr.engineBook(36)
	// A chapters-only run takes no series: it would hold the series' lock (later
	// volumes' authoring stages wait for the lowest unfinished volume) for a run
	// that makes nothing they carry on from.
	if first.Series != "" || first.SeriesPos != "" {
		t.Fatalf("the chapters book is in a series: %+v", first)
	}

	// The book was matched since: the continue takes the generate job's identity.
	hr.srv.set(func() {
		hr.srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{{Path: "Loose.m4b", Size: 6}},
			Info: &link.BookInfo{Title: "The Loose One", Authors: []string{"An Author"}, Series: "The Saga", SeriesIndex: "2", ASIN: "B0LOOSE"}}
	})
	hr.srv.add(37, "Loose.m4b", `{"contribute":"local","work_id":"loose-work"}`)
	hr.waitFor("the generate job done", func() bool { s, _ := hr.srv.final(37); return s == link.StateDone })
	b := hr.engineBook(37)
	if b.ID != first.ID || b.Goal != "" || b.WorkID != "loose-work" || b.ContribMode != "local" || b.Title != "The Loose One" ||
		b.Series != "The Saga" || b.SeriesPos != "2" || b.ASIN != "B0LOOSE" || len(b.Authors) != 1 || b.WorkDir != first.WorkDir {
		t.Fatalf("the generate job's book = %+v, want the chapters run (%d) continued with the job's identity", b, first.ID)
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
	mustWrite(t, b.SourcePath, "abcdef")
	if refreshed, ok := New(Options{}).refreshChapters(&run{}, b); ok || len(refreshed.Chapters) != 0 {
		t.Fatal("a list before there is one")
	}
	detectChapters(t, b)
	h := New(Options{})
	r := &run{files: []link.BookFile{{Path: "Loose.m4b", Size: 6}}, rel: "Loose.m4b", place: place{single: true}}
	d, ok := h.refreshChapters(r, b)
	if raw, err := r.chaptersJSON(true); !ok || len(d.Chapters) != 3 || raw == nil || err != nil {
		t.Fatalf("first look = %+v, %v (chapters %s, %v)", d, ok, raw, err)
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
	// Garbled is unreadable: put the list back as it was before the next steps.
	mustWrite(t, path, string(raw))
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	r.detectedStamp = ""
	if _, ok := h.refreshChapters(r, b); !ok {
		t.Fatal("the restored list isn't current")
	}
	// The audio out of sight for a moment (a mount gone): not current, and not
	// kept so, the list unchanged: it is current again once the file is back.
	src, _ := os.Stat(b.SourcePath)
	hidden := b.SourcePath + ".away"
	if err := os.Rename(b.SourcePath, hidden); err != nil {
		t.Fatal(err)
	}
	r.detectedStamp = ""
	if _, ok := h.refreshChapters(r, b); ok {
		t.Fatal("a list whose audio can't be looked at is current")
	}
	if err := os.Rename(hidden, b.SourcePath); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.refreshChapters(r, b); !ok {
		t.Fatalf("the audio is back (%v), but the list stayed not current", src.ModTime())
	}
	mustWrite(t, path, garbled)
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

// moveLooseMidRun runs job id (a chapters job when chapters, else a generate job)
// on Loose.m4b until asr, renames the file to Moved.m4b and lets the run go on;
// the server carries the job to the new path (the answers to its reports say so)
// after tell, so a write before then finds the book gone. It returns once the job
// ended done, failing the test on any other end.
func moveLooseMidRun(hr *harness, id int64, chapters bool, tell time.Duration) {
	hr.t.Helper()
	hr.exec.windowed = true
	gate := hr.exec.gate("asr")
	h := hr.handler()
	h.o.Recheck = 20 * time.Millisecond
	var stop func()
	if chapters {
		hr.srv.addKind(id, ChaptersKind, "Loose.m4b", `{}`)
		stop = hr.runner(h, h.ChaptersJob())
	} else {
		hr.srv.set(func() {
			hr.srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{{Path: "Loose.m4b", Size: 6}},
				Info: &link.BookInfo{Title: "Loose"}}
		})
		hr.srv.add(id, "Loose.m4b", `{}`)
		stop = hr.runner(h)
	}
	defer stop()
	hr.waitFor("asr", func() bool {
		return hr.srv.saw(id, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	if err := os.Rename(filepath.Join(hr.root, "Loose.m4b"), filepath.Join(hr.root, "Moved.m4b")); err != nil {
		hr.t.Fatal(err)
	}
	carry := func() {
		hr.srv.set(func() {
			hr.srv.books["Moved.m4b"] = link.Book{LibraryID: 1, Path: "Moved.m4b", Files: []link.BookFile{{Path: "Moved.m4b", Size: 6}},
				Info: &link.BookInfo{Title: "Loose"}}
			delete(hr.srv.books, "Loose.m4b")
			hr.srv.paths = map[int64]string{id: "Moved.m4b"}
		})
	}
	if tell == 0 {
		// A report's answer carries the new path before the run goes on.
		carry()
		time.Sleep(3 * h.o.Poll)
	}
	close(gate)
	if tell > 0 {
		time.Sleep(tell)
		carry()
	}
	hr.waitFor("the job ended", func() bool { s, _ := hr.srv.final(id); return s != "" })
	if s, u := hr.srv.final(id); s != link.StateDone {
		msg := ""
		if u.Error != nil {
			msg = *u.Error
		}
		hr.t.Fatalf("the job ended %s (%s), want done", s, msg)
	}
}

// A book the server moved mid-run keeps its chapters: they are checked against the
// audio where the job has the book now, not where it was at the claim. Before, a
// chapters job failed at its done ("the engine kept no chapters for the book") and
// a generate job finished without them. A move the server tells only after the
// run is done waits for it, parked, as a save that can't write does.
func TestChaptersFollowABookMovedMidRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		chapters bool
		tell     time.Duration
	}{
		{"chapters job", true, 0},
		{"chapters job, the move told late", true, 500 * time.Millisecond},
		{"generate job", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hr := newHarness(t)
			moveLooseMidRun(hr, 45, tc.chapters, tc.tell)
			if _, err := os.Stat(filepath.Join(hr.root, AttachDir, "Moved.chapters.json")); err != nil {
				t.Fatalf("no chapters at the new path: %v", err)
			}
		})
	}
}

// A list whose audio can't be looked at now (a mount gone for a moment) is neither
// written nor dropped: a write that can wait parks until the audio is back, one
// that can't returns errAudioUnseen, and the run is never cleared for it.
func TestAwaitChaptersWaitsForTheAudio(t *testing.T) {
	t.Parallel()
	b := store.Book{WorkDir: t.TempDir(), SourcePath: filepath.Join(t.TempDir(), "Loose.m4b")}
	mustWrite(t, b.SourcePath, "abcdef")
	detectChapters(t, b)
	h := New(Options{Recheck: 20 * time.Millisecond})
	r := &run{files: []link.BookFile{{Path: "Loose.m4b", Size: 6}}, rel: "Loose.m4b", place: place{single: true},
		rep: jobs.LocalReporter(), poke: make(chan struct{}, 1)}
	hidden := b.SourcePath + ".away"
	if err := os.Rename(b.SourcePath, hidden); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.awaitChapters(context.Background(), r, b, false); ok || !errors.Is(err, errAudioUnseen) {
		t.Fatalf("no wait, the audio out of sight = %v, %v, want errAudioUnseen", ok, err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.Rename(hidden, b.SourcePath)
	}()
	d, ok, err := h.awaitChapters(context.Background(), r, b, true)
	if !ok || err != nil || len(d.Chapters) != 3 {
		t.Fatalf("waited for the audio = %+v, %v, %v", d, ok, err)
	}
	if r.ownPark {
		t.Fatal("the wait's park outlived it")
	}
}

// A generate job continues a finished chapters run only while its list is current
// for the audio on disk: a file replaced since is transcribed again, never sent on
// with the old file's transcripts and chapters.
func TestGenerateDoesNotContinueAReplacedFile(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.windowed = true
	hr.srv.addKind(40, ChaptersKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	hr.waitFor("the chapters job done", func() bool { s, _ := hr.srv.final(40); return s == link.StateDone })
	first := hr.engineBook(40)

	mustWrite(t, filepath.Join(hr.root, "Loose.m4b"), "another encode")
	hr.srv.set(func() {
		hr.srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{{Path: "Loose.m4b", Size: 14}},
			Info: &link.BookInfo{Title: "Loose"}}
	})
	hr.srv.add(41, "Loose.m4b", `{}`)
	hr.waitFor("the generate job done", func() bool { s, _ := hr.srv.final(41); return s == link.StateDone })
	if b := hr.engineBook(41); b.ID == first.ID {
		t.Fatalf("the generate job continued the old file's run (book %d)", b.ID)
	}
}

// windowLayer leaves what a chapterless book's transcription leaves before
// chapter_detecting: the probe (the file's size), the windows manifest, every
// window's raw transcript and its FLAC.
func windowLayer(t *testing.T, b store.Book) {
	t.Helper()
	info, err := os.Stat(b.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(b.WorkDir, audio.ProbeName), fmt.Sprintf(`{"format":{"duration":"30","size":"%d"}}`, info.Size()))
	m := audio.Manifest{Source: b.SourcePath, Style: audio.StyleWindows, Duration: 30, ChapterCount: 2, Chapters: []audio.Chapter{
		{Chapter: 1, Start: 0, End: 15, Duration: 15}, {Chapter: 2, Start: 15, End: 30, Duration: 15}}}
	if err := audio.WriteManifest(b.WorkDir, m); err != nil {
		t.Fatal(err)
	}
	for w := 1; w <= 2; w++ {
		mustWrite(t, filepath.Join(b.WorkDir, transcript.RawDir, transcript.RawName(w)), `{"text":"and on","segments":[]}`)
		mustWrite(t, filepath.Join(b.WorkDir, audio.ChaptersDir, audio.ChapterFileName(w)), strings.Repeat("f", 512))
	}
}

// parkChapters runs a chapters job (id) on Loose.m4b until it parks at
// chapter_detecting (chapters_not_detected) with its window layer kept, and
// returns its engine book.
func parkChapters(hr *harness, id int64) store.Book {
	hr.t.Helper()
	hr.exec.windowed = true
	hr.exec.parks[string(state.ChapterDetecting)] = 1
	hr.srv.addKind(id, ChaptersKind, "Loose.m4b", `{}`)
	hr.waitFor("the chapters job parked", func() bool { return hr.srv.newParks(id, string(state.ParkChaptersNotDetected)) > 0 })
	b := hr.engineBook(id)
	windowLayer(hr.t, b)
	return b
}

// stageRuns counts the engine book's runs of stage, and its successes as the
// scheduler counts them (a superseded one left out).
func stageRuns(hr *harness, bookID int64, stage state.State) (runs, ok int) {
	hr.t.Helper()
	ctx := context.Background()
	runs, err := hr.db.CountStageRuns(ctx, bookID, string(stage))
	if err == nil {
		ok, err = hr.db.CountStageSuccesses(ctx, bookID, string(stage))
	}
	if err != nil {
		hr.t.Fatal(err)
	}
	return runs, ok
}

// A generate job queued for a book whose chapters job parked at chapter_detecting
// (the server cancels that job as it queues this one; the Bridge hears it at the
// job's next report) waits for it to give way, then continues its engine book at
// chapter_detecting: no new transcription, the detection run again (this test's
// detection gets past the park the second time, as one does once what parked it
// is fixed: TestGenerateContinuesAnUndetectedRunToItsPark has the detection that
// still finds nothing), then the whole pipeline with the job's identity.
func TestGenerateContinuesACancelledChaptersRun(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.h.o.Poll = 250 * time.Millisecond // holdTries polls: five seconds to give way in
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	first := parkChapters(hr, 51)
	if asr, _ := stageRuns(hr, first.ID, state.ASR); asr != 1 {
		t.Fatalf("asr runs = %d", asr)
	}
	// The generate job is claimed before the chapters job hears its cancel: it waits.
	hr.srv.set(func() {
		hr.srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{{Path: "Loose.m4b", Size: 6}},
			Info: &link.BookInfo{Title: "The Loose One", Series: "The Saga", SeriesIndex: "3"}}
	})
	hr.srv.add(52, "Loose.m4b", `{}`)
	hr.waitFor("the generate job waiting for the chapters job", func() bool {
		return hr.srv.saw(52, func(_ link.Update, p map[string]any) bool {
			return p["message"] == "Waiting for the book's chapters job to give way"
		})
	})
	hr.srv.set(func() { hr.srv.cancel[51] = true })
	hr.waitFor("the chapters job cancelled", func() bool { s, _ := hr.srv.final(51); return s == link.StateCancelled })
	hr.waitFor("the generate job done", func() bool { s, _ := hr.srv.final(52); return s == link.StateDone })
	b := hr.engineBook(52)
	if b.ID != first.ID || b.Goal != "" || b.Title != "The Loose One" || b.Series != "The Saga" || b.SeriesPos != "3" {
		t.Fatalf("the generate job's book = %+v, want the chapters run (%d) continued", b, first.ID)
	}
	if asr, _ := stageRuns(hr, b.ID, state.ASR); asr != 1 {
		t.Fatalf("asr runs = %d: the continue transcribed again", asr)
	}
	if runs, ok := stageRuns(hr, b.ID, state.ChapterDetecting); runs < 2 || ok != 1 {
		t.Fatalf("chapter_detecting runs = %d (%d ok), want the detection run again", runs, ok)
	}
	if !scheduler.SentinelExists(b.WorkDir, string(state.Sanitizing)) {
		t.Fatal("the continued book didn't run the whole pipeline")
	}
	_, final := hr.srv.final(52)
	var res Result
	if err := json.Unmarshal(final.Result, &res); err != nil ||
		strings.Join(res.Files, ",") != "Loose.characters.json,Loose.recaps.json,Loose.chapters.json,Loose.bridge.json" {
		t.Fatalf("result = %s", final.Result)
	}
}

// parkChaptersAtASR runs a chapters job (id) on Loose.m4b until it parks at asr
// waiting for an ASR worker (asr_worker_waiting), with its first window
// transcribed, and returns its engine book.
func parkChaptersAtASR(hr *harness, id int64) store.Book {
	hr.t.Helper()
	hr.exec.windowed = true
	hr.exec.parks[string(state.ASR)] = 1
	hr.srv.addKind(id, ChaptersKind, "Loose.m4b", `{}`)
	hr.waitFor("the chapters job parked at asr", func() bool { return hr.srv.newParks(id, string(state.ParkASRWorkerWaiting)) > 0 })
	b := hr.engineBook(id)
	windowLayer(hr.t, b)
	// Only the first window was transcribed before the worker went.
	if err := os.Remove(filepath.Join(b.WorkDir, transcript.RawDir, transcript.RawName(2))); err != nil {
		hr.t.Fatal(err)
	}
	return b
}

// A generate job queued for a book whose chapters job parked at asr waiting for a
// worker (the server cancels that job as it queues this one) continues its engine
// book instead of clearing it: the window it transcribed is kept, and the run goes
// on from splitting (passed straight on, its window FLACs whole) through asr, the
// detection and the whole pipeline, with the job's identity (bridge#24).
func TestGenerateContinuesAChaptersRunParkedAtASR(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.h.o.Poll = 250 * time.Millisecond // holdTries polls: five seconds to give way in
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	first := parkChaptersAtASR(hr, 71)
	kept := filepath.Join(first.WorkDir, transcript.RawDir, transcript.RawName(1))
	want, err := os.ReadFile(kept) //nolint:gosec // test
	if err != nil {
		t.Fatal(err)
	}
	hr.srv.set(func() {
		hr.srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{{Path: "Loose.m4b", Size: 6}},
			Info: &link.BookInfo{Title: "The Loose One", Series: "The Saga", SeriesIndex: "4"}}
	})
	hr.srv.add(72, "Loose.m4b", `{}`)
	hr.waitFor("the generate job waiting for the chapters job", func() bool {
		return hr.srv.saw(72, func(_ link.Update, p map[string]any) bool {
			return p["message"] == "Waiting for the book's chapters job to give way"
		})
	})
	hr.srv.set(func() { hr.srv.cancel[71] = true })
	hr.waitFor("the chapters job cancelled", func() bool { s, _ := hr.srv.final(71); return s == link.StateCancelled })
	waitDone(hr, 72)
	b := hr.engineBook(72)
	if b.ID != first.ID || b.WorkDir != first.WorkDir || b.Goal != "" || b.Title != "The Loose One" || b.SeriesPos != "4" {
		t.Fatalf("the generate job's book = %+v, want the chapters run (%d) continued", b, first.ID)
	}
	if got, err := os.ReadFile(kept); err != nil || string(got) != string(want) { //nolint:gosec // test
		t.Fatalf("the transcribed window = %q, %v: the continue threw it away", got, err)
	}
	// Splitting's kept sentinel passes it straight on (its window FLACs are whole;
	// a purge that took them takes the sentinel too, so they would be cut again).
	if inspect, _ := stageRuns(hr, b.ID, state.Inspecting); inspect != 1 {
		t.Fatalf("inspecting runs = %d: the continue started over", inspect)
	}
	if split, _ := stageRuns(hr, b.ID, state.Splitting); split != 1 {
		t.Fatalf("splitting runs = %d: the continue split again", split)
	}
	if asr, _ := stageRuns(hr, b.ID, state.ASR); asr != 2 {
		t.Fatalf("asr runs = %d, want the parked run and the continued one", asr)
	}
	if _, ok := stageRuns(hr, b.ID, state.ChapterDetecting); ok != 1 {
		t.Fatal("the continued run didn't find the chapters")
	}
	if !scheduler.SentinelExists(b.WorkDir, string(state.Sanitizing)) {
		t.Fatal("the continued book didn't run the whole pipeline")
	}
}

// A chapters run parked at asr is never continued once its file was replaced (or
// its windows manifest is gone): the generate job starts its own run.
func TestGenerateDoesNotContinueAnASRRunItCantTrust(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		spoil func(hr *harness, b store.Book)
	}{
		{"replaced", func(hr *harness, _ store.Book) {
			mustWrite(hr.t, filepath.Join(hr.root, "Loose.m4b"), "another encode")
			hr.srv.set(func() {
				hr.srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{{Path: "Loose.m4b", Size: 14}},
					Info: &link.BookInfo{Title: "Loose"}}
			})
		}},
		{"no windows manifest", func(hr *harness, b store.Book) {
			if err := os.Remove(filepath.Join(b.WorkDir, audio.ManifestName)); err != nil {
				hr.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hr := newHarness(t)
			stop := hr.runner(hr.h, hr.h.ChaptersJob())
			defer stop()
			first := parkChaptersAtASR(hr, 73)
			hr.srv.set(func() { hr.srv.cancel[73] = true })
			hr.waitFor("the engine book cancelled", func() bool {
				b, err := hr.db.GetBook(context.Background(), first.ID)
				return err == nil && scheduler.IsCancelled(b)
			})
			tc.spoil(hr, first)
			hr.srv.add(74, "Loose.m4b", `{}`)
			waitDone(hr, 74)
			if b := hr.engineBook(74); b.ID == first.ID {
				t.Fatalf("the generate job continued the chapters run (book %d)", b.ID)
			}
		})
	}
}

// A run parked chapters_not_detected that a generate job continues runs the same
// detection over the same windows, so it parks there again, now under the generate
// job (with the hint to add chapter markers), nothing transcribed again.
func TestGenerateContinuesAnUndetectedRunToItsPark(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	first := parkChapters(hr, 55)
	hr.srv.set(func() { hr.srv.cancel[55] = true })
	hr.waitFor("the engine book cancelled", func() bool {
		b, err := hr.db.GetBook(context.Background(), first.ID)
		return err == nil && scheduler.IsCancelled(b)
	})
	hr.exec.mu.Lock()
	hr.exec.parks[string(state.ChapterDetecting)] = 1
	hr.exec.mu.Unlock()
	hr.srv.add(56, "Loose.m4b", `{}`)
	hr.waitFor("the generate job parked chapters_not_detected", func() bool {
		return hr.srv.newParks(56, string(state.ParkChaptersNotDetected)) > 0
	})
	b := hr.engineBook(56)
	if b.ID != first.ID || b.Goal != "" {
		t.Fatalf("the generate job's book = %+v, want the chapters run (%d) continued", b, first.ID)
	}
	if asr, _ := stageRuns(hr, b.ID, state.ASR); asr != 1 {
		t.Fatalf("asr runs = %d: the continue transcribed again", asr)
	}
}

// A generate job cancelled while it takes a chapters run over (reading its
// transcripts) leaves the run as it was: the transaction is the job's, so the run
// stays a chapters run the next job continues.
func TestContinueChaptersForACancelledJobLeavesTheRun(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	first := parkChapters(hr, 57)
	hr.srv.set(func() { hr.srv.cancel[57] = true })
	var b store.Book
	hr.waitFor("the engine book cancelled", func() bool {
		var err error
		b, err = hr.db.GetBook(context.Background(), first.ID)
		return err == nil && scheduler.IsCancelled(b)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &run{job: jobs.Job{ID: 58}, lib: link.Library{ID: 1}, rel: "Loose.m4b", source: SourceAudio, place: place{single: true}}
	if ok, err := hr.h.continueChapters(ctx, hr.sched, r, b); ok || err == nil {
		t.Fatalf("continue for a cancelled job = %v, %v", ok, err)
	}
	if got, _ := hr.db.GetBook(context.Background(), b.ID); got.Goal != state.GoalChapters || !scheduler.IsCancelled(got) {
		t.Fatalf("the run was taken over: %+v", got)
	}
	if _, err := hr.db.GetGenerateBook(context.Background(), 58); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the cancelled job was mapped to the run: %v", err)
	}
}

// A cancelled chapters run is never continued once its transcripts were swept or
// its file replaced: the generate job starts its own run.
func TestGenerateDoesNotContinueACancelledRunItCantTrust(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		spoil func(hr *harness, b store.Book)
	}{
		{"swept", func(hr *harness, b store.Book) {
			if err := hr.db.MarkGenerateBookSwept(context.Background(), b.ID, time.Now()); err != nil {
				hr.t.Fatal(err)
			}
		}},
		{"replaced", func(hr *harness, _ store.Book) {
			mustWrite(hr.t, filepath.Join(hr.root, "Loose.m4b"), "another encode")
			hr.srv.set(func() {
				hr.srv.books["Loose.m4b"] = link.Book{LibraryID: 1, Path: "Loose.m4b", Files: []link.BookFile{{Path: "Loose.m4b", Size: 14}},
					Info: &link.BookInfo{Title: "Loose"}}
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hr := newHarness(t)
			stop := hr.runner(hr.h, hr.h.ChaptersJob())
			defer stop()
			first := parkChapters(hr, 53)
			hr.srv.set(func() { hr.srv.cancel[53] = true })
			hr.waitFor("the engine book cancelled", func() bool {
				b, err := hr.db.GetBook(context.Background(), first.ID)
				return err == nil && scheduler.IsCancelled(b)
			})
			tc.spoil(hr, first)
			hr.srv.add(54, "Loose.m4b", `{}`)
			hr.waitFor("the generate job done", func() bool { s, _ := hr.srv.final(54); return s == link.StateDone })
			if b := hr.engineBook(54); b.ID == first.ID {
				t.Fatalf("the generate job continued the chapters run (book %d)", b.ID)
			}
		})
	}
}

// waitDone waits for job id to end and fails the test, with the job's error,
// unless it ended done.
func waitDone(hr *harness, id int64) {
	hr.t.Helper()
	hr.waitFor(fmt.Sprintf("job %d to end", id), func() bool { s, _ := hr.srv.final(id); return s != "" })
	if s, u := hr.srv.final(id); s != link.StateDone {
		msg := ""
		if u.Error != nil {
			msg = *u.Error
		}
		hr.t.Fatalf("job %d ended %s: %s", id, s, msg)
	}
}

// orphanChapters parks a chapters job (id) at chapter_detecting, then ends it here
// without its end running (cancelled while it waited for the engine after a
// restart, say): the book stays as it was and its mapping open, with no job
// following it. It returns the book and a new runner's stop. The first runner
// claims only chapters jobs: a claim it had in flight as it stopped, which the
// server can answer late, never takes the generate job the test queues next.
func orphanChapters(hr *harness, id int64) (store.Book, func()) {
	hr.t.Helper()
	stop := hr.runnerOf(hr.h.ChaptersJob())
	b := parkChapters(hr, id)
	stop() // the Bridge stopping: the job would resume, so its end doesn't run
	ctx := context.Background()
	sj, err := hr.db.GetServerJob(ctx, id)
	if err != nil {
		hr.t.Fatal(err)
	}
	if err := hr.db.FinishServerJob(ctx, id, sj.Attempt, store.JobCancelled, nil, ""); err != nil {
		hr.t.Fatal(err)
	}
	if err := hr.db.MarkServerJobReported(ctx, id, sj.Attempt); err != nil {
		hr.t.Fatal(err)
	}
	return b, hr.runner(hr.h, hr.h.ChaptersJob())
}

// A generate job claimed beside a WORKING chapters job an admin just cancelled
// (the server queues it at once: a job being cancelled refuses nothing) waits for
// that job to hear its cancel and stop the book, then continues the run at
// chapter_detecting, instead of refusing as if the chapters job still ran.
func TestGenerateWaitsForACancelledWorkingChaptersJob(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.h.o.Poll = 250 * time.Millisecond // holdTries polls: five seconds to give way in
	hr.exec.windowed = true
	gate := hr.exec.gate(string(state.ChapterDetecting))
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	hr.srv.addKind(61, ChaptersKind, "Loose.m4b", `{}`)
	hr.waitFor("the chapters job detecting", func() bool {
		return hr.srv.saw(61, func(_ link.Update, p map[string]any) bool { return p["stage"] == string(state.ChapterDetecting) })
	})
	first := hr.engineBook(61)
	windowLayer(t, first)
	hr.srv.add(62, "Loose.m4b", `{}`)
	hr.waitFor("the generate job waiting for the chapters job", func() bool {
		return hr.srv.saw(62, func(_ link.Update, p map[string]any) bool {
			return p["message"] == "Waiting for the book's chapters job to give way"
		})
	})
	hr.srv.set(func() { hr.srv.cancel[61] = true })
	hr.waitFor("the chapters job cancelled", func() bool { s, _ := hr.srv.final(61); return s == link.StateCancelled })
	close(gate)
	waitDone(hr, 62)
	b := hr.engineBook(62)
	if b.ID != first.ID {
		t.Fatalf("the generate job's book = %d, want the chapters run (%d) continued", b.ID, first.ID)
	}
	if asr, _ := stageRuns(hr, b.ID, state.ASR); asr != 1 {
		t.Fatalf("asr runs = %d: the continue transcribed again", asr)
	}
}

// A cancelled chapters run the engine still holds for a moment (the stage its
// cancel stopped still finishing, a purge) is waited for and continued, never
// deleted once the hold goes (the delete waits for the same hold).
func TestGenerateContinuesABrieflyHeldChaptersRun(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.h.o.Poll = 250 * time.Millisecond // holdTries polls: five seconds for the hold to go
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	first := parkChapters(hr, 63)
	hr.srv.set(func() { hr.srv.cancel[63] = true })
	hr.waitFor("the engine book cancelled", func() bool {
		b, err := hr.db.GetBook(context.Background(), first.ID)
		return err == nil && scheduler.IsCancelled(b)
	})
	release, ok := hr.sched.Hold(first.ID)
	if !ok {
		t.Fatal("the engine wouldn't hold the book")
	}
	hr.srv.add(64, "Loose.m4b", `{}`)
	hr.waitFor("the generate job claimed", func() bool { return hr.srv.saw(64, func(link.Update, map[string]any) bool { return true }) })
	time.Sleep(200 * time.Millisecond)
	release()
	waitDone(hr, 64)
	if b := hr.engineBook(64); b.ID != first.ID {
		t.Fatalf("the generate job's book = %d, want the briefly held chapters run (%d) continued", b.ID, first.ID)
	}
}

// The stage a cancel stopped can finish before it lets go (its last steps don't
// stop for a cancel): the run, done by the time the generate job holds it, is
// continued from splitting, never deleted for no longer being where it was.
func TestGenerateContinuesARunThatFinishedWhileHeld(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.h.o.Poll = 250 * time.Millisecond // holdTries polls: five seconds for the hold to go
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	first := parkChapters(hr, 65)
	hr.srv.set(func() { hr.srv.cancel[65] = true })
	hr.waitFor("the engine book cancelled", func() bool {
		b, err := hr.db.GetBook(context.Background(), first.ID)
		return err == nil && scheduler.IsCancelled(b)
	})
	release, ok := hr.sched.Hold(first.ID)
	if !ok {
		t.Fatal("the engine wouldn't hold the book")
	}
	hr.srv.add(66, "Loose.m4b", `{}`)
	hr.waitFor("the generate job claimed", func() bool { return hr.srv.saw(66, func(link.Update, map[string]any) bool { return true }) })
	time.Sleep(200 * time.Millisecond)
	// The stage finished after all: its list written and the book done, the
	// cancel's status kept (the advance writes only the state).
	detectChapters(t, first)
	if err := hr.db.SetBookState(context.Background(), first.ID, string(state.Done), string(state.StatusFailed), "cancelled by user", ""); err != nil {
		t.Fatal(err)
	}
	release()
	waitDone(hr, 66)
	if b := hr.engineBook(66); b.ID != first.ID {
		t.Fatalf("the generate job's book = %d, want the run that finished (%d) continued", b.ID, first.ID)
	}
}

// A run the engine stays busy with past the wait fails the generate job, to be
// tried again, and is kept: it is never deleted unseen once the engine lets go.
func TestGenerateKeepsARunTheEngineStaysBusyWith(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	first := parkChapters(hr, 67)
	hr.srv.set(func() { hr.srv.cancel[67] = true })
	hr.waitFor("the engine book cancelled", func() bool {
		b, err := hr.db.GetBook(context.Background(), first.ID)
		return err == nil && scheduler.IsCancelled(b)
	})
	release, ok := hr.sched.Hold(first.ID)
	if !ok {
		t.Fatal("the engine wouldn't hold the book")
	}
	hr.srv.add(68, "Loose.m4b", `{}`)
	// The engine lets go once the job gave up, or once a delete waits for it.
	hr.waitFor("the generate job to give up on the hold", func() bool {
		s, _ := hr.srv.final(68)
		return s != "" || hr.srv.saw(68, func(_ link.Update, p map[string]any) bool {
			return p["message"] == "Waiting for the engine to let go of the book's earlier run"
		})
	})
	release()
	hr.waitFor("the generate job ended", func() bool { s, _ := hr.srv.final(68); return s != "" })
	if s, u := hr.srv.final(68); s != link.StateFailed || u.Error == nil || *u.Error != errEarlierRunBusy.Error() {
		t.Fatalf("the generate job ended %s (%+v), want failed with %q", s, u.Error, errEarlierRunBusy)
	}
	if b, err := hr.db.GetBook(context.Background(), first.ID); err != nil || b.Goal != state.GoalChapters {
		t.Fatalf("the chapters run wasn't kept: %+v, %v", b, err)
	}
}

// A folder book that has gained an audio file since its chapters run is a
// multi-file book now: the run, which covers the one file it was, isn't continued
// however rerunnable its windows are.
func TestContinueChaptersRefusesAFolderThatGainedAFile(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	ctx := context.Background()
	dir := filepath.Join(hr.root, "Solo")
	mustWrite(t, filepath.Join(dir, "solo.m4b"), "single")
	b, err := hr.db.CreateBook(ctx, store.NewBook{SourcePath: dir, WorkDir: filepath.Join(hr.work, "solo"), Title: "Solo", Goal: state.GoalChapters})
	if err != nil {
		t.Fatal(err)
	}
	if err := hr.db.SetBookState(ctx, b.ID, string(state.ChapterDetecting), string(state.StatusFailed), "cancelled by user", ""); err != nil {
		t.Fatal(err)
	}
	layer := b
	layer.SourcePath = filepath.Join(dir, "solo.m4b") // the one file the windows were cut from
	windowLayer(t, layer)
	if !pipeline.DetectionRerunnable(b.WorkDir) {
		t.Fatal("the fixture's windows aren't rerunnable")
	}
	mustWrite(t, filepath.Join(dir, "solo part 2.m4b"), "second")
	if b, err = hr.db.GetBook(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	r := &run{job: jobs.Job{ID: 59}, lib: link.Library{ID: 1}, rel: "Solo", source: SourceAudio}
	if ok, err := hr.h.continueChapters(ctx, hr.sched, r, b); ok || err != nil {
		t.Fatalf("continue a folder that gained a file = %v, %v", ok, err)
	}
	if got, _ := hr.db.GetBook(ctx, b.ID); got.Goal != state.GoalChapters {
		t.Fatalf("the run was taken over: %+v", got)
	}
}

// A chapters run its job ended without stopping (no job follows it, so nothing
// else will): the generate job waits its budget for it to give way, then stops it
// as that end would have, and continues it instead of deleting it.
func TestGenerateStopsAChaptersRunNoJobFollows(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	first, stop := orphanChapters(hr, 71)
	defer stop()
	hr.srv.add(72, "Loose.m4b", `{}`)
	waitDone(hr, 72)
	b := hr.engineBook(72)
	if b.ID != first.ID {
		t.Fatalf("the generate job's book = %d, want the chapters run (%d) continued", b.ID, first.ID)
	}
	if asr, _ := stageRuns(hr, b.ID, state.ASR); asr != 1 {
		t.Fatalf("asr runs = %d: the continue transcribed again", asr)
	}
}

// The chapters job's end reaching its book late (Abandon releasing it once the
// engine has started, after a restart) leaves the run a generate job continued
// meanwhile to that job: another open job follows it now.
func TestALateChaptersEndLeavesTheContinuedRunAlone(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	first, stop := orphanChapters(hr, 81)
	defer stop()
	ctx := context.Background()
	if err := hr.sched.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	gate := hr.exec.gate(string(state.Sanitizing))
	hr.srv.add(82, "Loose.m4b", `{}`)
	hr.waitFor("the continued run working", func() bool {
		return hr.srv.saw(82, func(_ link.Update, p map[string]any) bool { return p["stage"] == string(state.Sanitizing) })
	})
	sj, err := hr.db.GetServerJob(ctx, 81)
	if err != nil {
		t.Fatal(err)
	}
	lib, path := int64(1), "Loose.m4b"
	hr.h.ChaptersJob().Abandon(ctx, jobs.Job{ID: 81, Kind: ChaptersKind, LibraryID: &lib, Path: &path, Attempt: sj.Attempt})
	close(gate)
	waitDone(hr, 82)
	if b := hr.engineBook(82); b.ID != first.ID {
		t.Fatalf("the generate job's book = %d, want the chapters run (%d) continued", b.ID, first.ID)
	}
}

// chaptersJSON composes the file for the book where it is when it is written: the
// server moves a job with its book (a rename during the run), and the list named the
// old file's name would never be applied. The bytes stay the same for an unchanged
// book (generated_at is when the run first composed them).
func TestChaptersJSONFollowsTheBook(t *testing.T) {
	t.Parallel()
	b := store.Book{WorkDir: t.TempDir(), SourcePath: filepath.Join(t.TempDir(), "Loose.m4b")}
	mustWrite(t, b.SourcePath, "single")
	detectChapters(t, b)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	h := New(Options{Now: func() time.Time { return now }})
	r := &run{files: []link.BookFile{{Path: "Loose.m4b", Size: 6}}, rel: "Loose.m4b", place: place{single: true}}
	if _, ok := h.refreshChapters(r, b); !ok {
		t.Fatal("no current list")
	}
	first, err := r.chaptersJSON(true)
	if err != nil || first == nil {
		t.Fatalf("first = %s, %v", first, err)
	}
	now = now.Add(time.Hour)
	if again, _ := r.chaptersJSON(true); string(again) != string(first) {
		t.Fatalf("an unchanged book composed new bytes:\n%s\n%s", first, again)
	}
	// The book is renamed; the server moved the job with it (adoptMove).
	r.files, r.rel = []link.BookFile{{Path: "New.m4b", Size: 6}}, "New.m4b"
	raw, err := r.chaptersJSON(true)
	var f ChaptersFile
	if err != nil || json.Unmarshal(raw, &f) != nil || len(f.Files) != 1 || f.Files[0].Path != "New.m4b" {
		t.Fatalf("after the move = %s, %v", raw, err)
	}
}

// A chapters job never writes chapters.json beside characters and recaps made
// without chapters found in the narration (a run of the file as one chapter, the
// ebook's chapters): the server would gate their positions by the new list, opening a
// whole-book recap with the second chapter. Beside ones made with them, it does.
func TestChaptersDetectRefusedBesideOtherSidecars(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.windowed = true
	adir := filepath.Join(hr.root, AttachDir)
	mustWrite(t, filepath.Join(adir, "Loose.characters.json"), `{}`)
	mustWrite(t, filepath.Join(adir, "Loose.bridge.json"), `{"kind":"sidecars.generate","job_id":7}`)
	hr.srv.addKind(42, ChaptersKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	hr.waitFor("refused", func() bool { s, _ := hr.srv.final(42); return s == link.StateFailed })
	if _, final := hr.srv.final(42); final.Error == nil || *final.Error != string(errChaptersBesideSidecars) {
		t.Fatalf("error = %v", final.Error)
	}
	if _, err := hr.db.GetGenerateBook(context.Background(), 42); err == nil {
		t.Fatal("the refused job started a run")
	}
	if _, err := os.Stat(filepath.Join(adir, "Loose.chapters.json")); err == nil {
		t.Fatal("chapters.json written beside the other sidecars")
	}
	// Allowed: sidecars made with chapters found in the narration.
	mustWrite(t, filepath.Join(adir, "Loose.bridge.json"), `{"kind":"sidecars.generate","job_id":7,"chapters":{"job_id":7,"from":"narration","chapters":3}}`)
	hr.srv.addKind(43, ChaptersKind, "Loose.m4b", `{}`)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(43); return s == link.StateDone })
	if _, err := os.Stat(filepath.Join(adir, "Loose.chapters.json")); err != nil {
		t.Fatal(err)
	}
}

// A finished whole run of the book is never cleared for a chapters run: its notes
// carry a series on and describe the book.
func TestChaptersDetectKeepsAFinishedRun(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	ctx := context.Background()
	sp, ok, err := metaops.AllowedPath(filepath.Join(hr.root, "Loose.m4b"), []string{hr.root})
	if err != nil || !ok {
		t.Fatal(sp, ok, err)
	}
	old, err := hr.db.CreateBook(ctx, store.NewBook{SourcePath: sp, WorkDir: filepath.Join(hr.work, "loose"), Title: "Loose"})
	if err != nil {
		t.Fatal(err)
	}
	if err := hr.db.SetBookState(ctx, old.ID, string(state.Done), "", "", ""); err != nil {
		t.Fatal(err)
	}
	hr.srv.addKind(44, ChaptersKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	defer stop()
	hr.waitFor("refused", func() bool { s, _ := hr.srv.final(44); return s == link.StateFailed })
	if _, final := hr.srv.final(44); final.Error == nil || *final.Error != string(errChaptersKeepsRun) {
		t.Fatalf("error = %v", final.Error)
	}
	if got, err := hr.db.GetBook(ctx, old.ID); err != nil || got.State != string(state.Done) {
		t.Fatalf("the finished run = %+v, %v", got, err)
	}
}

// The early save never waits: while the library isn't writable the job goes on
// relaying the engine book, and the chapters go in once it is.
func TestEarlySaveNeverWaitsForTheLibrary(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.windowed = true
	asr := hr.exec.gate("asr")
	facts := hr.exec.gate("fact_pass")
	hr.srv.add(45, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h)
	defer stop()
	hr.waitFor("asr", func() bool {
		return hr.srv.saw(45, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	hr.libs.writable.Store(false)
	close(asr)
	hr.waitFor("fact_pass relayed while the library isn't writable", func() bool {
		return hr.srv.saw(45, func(_ link.Update, p map[string]any) bool { return p["stage"] == "fact_pass" })
	})
	chapters := filepath.Join(hr.root, AttachDir, "Loose.chapters.json")
	if _, err := os.Stat(chapters); err == nil {
		t.Fatal("chapters.json written into a library that isn't writable")
	}
	hr.libs.writable.Store(true)
	hr.waitFor("chapters.json once writable", func() bool { _, err := os.Stat(chapters); return err == nil })
	close(facts)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(45); return s == link.StateDone })
}

// hasChaptersFailure reads the inspecting run's own error: a failure the supervisor
// escalated into a park (its own message on the book) still ends the job with it.
func TestHasChaptersFailure(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	h := New(Options{DB: db})
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/x.m4b", WorkDir: "/w/x", Title: "X", Goal: state.GoalChapters})
	if err != nil {
		t.Fatal(err)
	}
	fail := func(msg string) {
		id, err := db.StartStageRun(ctx, b.ID, string(state.Inspecting), 1)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]string{"error": msg})
		if err := db.FinishStageRun(ctx, id, false, raw); err != nil {
			t.Fatal(err)
		}
	}
	fail("inspect: ffprobe failed")
	if _, ok := h.hasChaptersFailure(ctx, b.ID); ok {
		t.Fatal("another failure read as the book having chapters")
	}
	for _, refusal := range pipeline.ChaptersRefusals {
		fail(refusal.Error())
		if msg, ok := h.hasChaptersFailure(ctx, b.ID); !ok || msg != refusal.Error() {
			t.Fatalf("= %q, %v, want %q", msg, ok, refusal)
		}
	}
}

// Writing an earlier run's chapters, a job whose book's folder can't be written
// parks once and stays parked until the save goes through: never lifted between two
// tries, which the server would hear (and notify) as a new park every Recheck.
func TestChaptersFromEarlierParksOnce(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.exec.windowed = true
	hr.srv.addKind(46, ChaptersKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ChaptersJob())
	hr.waitFor("the first job done", func() bool { s, _ := hr.srv.final(46); return s == link.StateDone })
	stop()
	// .audiosilo becomes a file: every save fails.
	adir := filepath.Join(hr.root, AttachDir)
	if err := os.RemoveAll(adir); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, adir, "not a folder")
	h := hr.handler()
	h.o.Recheck = 5 * time.Millisecond
	hr.srv.addKind(47, ChaptersKind, "Loose.m4b", `{}`)
	stop2 := hr.runner(h, h.ChaptersJob())
	defer stop2()
	hr.waitFor("parked", func() bool {
		return hr.srv.saw(47, func(u link.Update, _ map[string]any) bool {
			return u.State != nil && *u.State == link.StateNeedsAttention
		})
	})
	time.Sleep(100 * time.Millisecond) // many Rechecks
	parks := 0
	hr.srv.mu.Lock()
	ups := hr.srv.updates[47]
	for i, u := range ups {
		if u.State != nil && *u.State == link.StateNeedsAttention && (i == 0 || ups[i-1].State == nil || *ups[i-1].State != link.StateNeedsAttention) {
			parks++
		}
	}
	hr.srv.mu.Unlock()
	if parks != 1 {
		t.Fatalf("parked %d times, want once", parks)
	}
	if _, err := hr.db.GetGenerateBook(context.Background(), 47); err == nil {
		t.Fatal("the job mapped an engine book of its own")
	}
}
