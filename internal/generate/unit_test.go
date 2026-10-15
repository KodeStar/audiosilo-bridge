package generate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

func TestParseParams(t *testing.T) {
	t.Parallel()
	p, err := ParseParams(nil)
	if err != nil || p.Source != SourceAuto || p.Contribute != ContributeLocal || p.WorkID != "" {
		t.Fatalf("defaults = %+v, %v", p, err)
	}
	p, err = ParseParams(json.RawMessage(`{"source":"ebook","contribute":"issue","work_id":" the-way-of-kings ","later":1}`))
	if err != nil || p.Source != SourceEbook || p.Contribute != ContributeIssue || p.WorkID != "the-way-of-kings" {
		t.Fatalf("parsed = %+v, %v (a field a later server adds is ignored)", p, err)
	}
	for name, raw := range map[string]string{
		"bad source":     `{"source":"video"}`,
		"bad contribute": `{"contribute":"pr"}`,
		"long work id":   `{"work_id":"` + strings.Repeat("a", maxWorkID+1) + `"}`,
		"control char":   `{"work_id":"a\u0007b"}`,
		"not an object":  `[1]`,
	} {
		if _, err := ParseParams(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDescribe(t *testing.T) {
	t.Parallel()
	b := store.Book{ID: 1, State: string(state.ASR)}
	pr := describe(view{book: b, progress: []store.Progress{{Stage: "splitting", Done: 9, Total: 9}, {Stage: "asr", Done: 12, Total: 40}},
		eta: 5400, hasETA: true, costUSD: 3.4249, source: SourceAudio})
	if pr.Step != "asr" || pr.Done != 12 || pr.Total != 40 || !strings.Contains(pr.Message, "Transcribing (12 of 40)") {
		t.Fatalf("progress = %+v", pr)
	}
	if pr.Extra["stage"] != "asr" || pr.Extra["eta_seconds"] != int64(5400) || pr.Extra["cost_usd"] != 3.42 || pr.Extra["source"] != "audio" {
		t.Fatalf("extra = %+v", pr.Extra)
	}
	if _, ok := pr.Extra["park"]; ok {
		t.Fatal("a running book has a park")
	}

	queued := describe(view{book: store.Book{State: "queued"}, source: SourceEbook})
	if queued.Step != stepWaiting || queued.Extra["stage"] != "queued" {
		t.Fatalf("queued = %+v", queued)
	}

	parked := describe(view{book: store.Book{State: "fact_pass", Status: "needs_attention", ParkCode: "agent_unavailable",
		RetryAt: "2026-10-15T12:00:00Z", Error: "agent backend unavailable"}, eta: 9, hasETA: true})
	park, _ := parked.Extra["park"].(map[string]any)
	if park["code"] != "agent_unavailable" || park["retry_at"] != "2026-10-15T12:00:00Z" || park["hint"] != ParkHint("agent_unavailable") ||
		park["hint"] == "" {
		t.Fatalf("park = %+v", park)
	}
	if _, ok := parked.Extra["eta_seconds"]; ok {
		t.Fatal("a parked book reports an ETA")
	}
}

// Every engine park code has a hint, so a park never reaches the console bare.
func TestEveryParkCodeHasAHint(t *testing.T) {
	t.Parallel()
	for _, code := range []state.ParkCode{state.ParkAgentUnavailable, state.ParkAgentRateLimited, state.ParkAgentValidationExhausted,
		state.ParkMarkersNotConfident, state.ParkQANoConverge, state.ParkSpellingGateFailure, state.ParkMediaToolsUnavailable,
		state.ParkASRUnavailable, state.ParkManifestChanged, state.ParkFixLoopExhausted, state.ParkASRDecodeTimeout,
		state.ParkContribUnavailable, state.ParkCoreNeeded, state.ParkCorePending, state.ParkEbookUnreadable,
		state.ParkEbookNoChapters, state.ParkEbookChaptersNotConfident, state.ParkBudgetExceeded,
		state.ParkSupervisorEscalated, state.ParkSupervisorBudget, parkBookNotWritable, parkLibraryNotWritable} {
		if ParkHint(string(code)) == "" {
			t.Errorf("park code %q has no hint", code)
		}
	}
	for _, s := range state.All() {
		if stageLabels[s] == "" {
			t.Errorf("stage %q has no label", s)
		}
	}
}

func TestLocateAndEpub(t *testing.T) {
	root := t.TempDir()
	book := filepath.Join(root, "Author", "Book")
	mustWrite(t, filepath.Join(book, "01.mp3"), "audio")
	p, err := locate(root, "Author/Book")
	if err != nil || p.single || p.dir != book || p.prefix != "" {
		t.Fatalf("folder book = %+v, %v", p, err)
	}
	if _, err := locate(root, "../etc"); err == nil {
		t.Fatal("a path leaving the mount was located")
	}
	mustWrite(t, filepath.Join(root, "Loose.Book.m4b"), "audio")
	p, err = locate(root, "Loose.Book.m4b")
	if err != nil || !p.single || p.dir != root || p.prefix != "Loose.Book." {
		t.Fatalf("single file = %+v, %v", p, err)
	}

	// readEpub is a package hook; this test isn't parallel.
	old := readEpub
	t.Cleanup(func() { readEpub = old })
	readEpub = func(path string) error {
		if strings.Contains(path, "drm") {
			return errors.New("encrypted")
		}
		return nil
	}

	fp, _ := locate(root, "Author/Book")
	if src, epub, note, err := chooseSource(fp, SourceAuto); err != nil || src != SourceAudio || epub != "" || note != "" {
		t.Fatalf("no epub, auto = %s %s %q %v", src, epub, note, err)
	}
	if _, _, _, err := chooseSource(fp, SourceEbook); !errors.Is(err, errNoEbook) {
		t.Fatalf("no epub, ebook = %v", err)
	}
	mustWrite(t, filepath.Join(book, "._Book.epub"), "resource fork")
	mustWrite(t, filepath.Join(book, "extras", "Other.epub"), "not this book's")
	mustWrite(t, filepath.Join(book, "Book.epub"), "epub")
	if src, epub, _, err := chooseSource(fp, SourceAuto); err != nil || src != SourceEbook || epub != filepath.Join(book, "Book.epub") {
		t.Fatalf("an epub in the folder, auto = %s %s %v", src, epub, err)
	}
	if src, _, _, err := chooseSource(fp, SourceAudio); err != nil || src != SourceAudio {
		t.Fatalf("audio forced = %s %v", src, err)
	}
	mustWrite(t, filepath.Join(book, "Second.epub"), "epub")
	if src, _, note, err := chooseSource(fp, SourceAuto); err != nil || src != SourceAudio || note == "" {
		t.Fatalf("two epubs, auto = %s %q %v (falls back, saying why)", src, note, err)
	}
	if _, _, _, err := chooseSource(fp, SourceEbook); err == nil {
		t.Fatal("two epubs, ebook: accepted")
	}
	_ = os.Remove(filepath.Join(book, "Second.epub"))
	_ = os.Rename(filepath.Join(book, "Book.epub"), filepath.Join(book, "drm.epub"))
	if src, _, note, err := chooseSource(fp, SourceAuto); err != nil || src != SourceAudio || !strings.Contains(note, "DRM") {
		t.Fatalf("an unreadable epub, auto = %s %q %v", src, note, err)
	}
	if _, _, _, err := chooseSource(fp, SourceEbook); err == nil || !strings.Contains(err.Error(), "DRM") {
		t.Fatalf("an unreadable epub, ebook = %v", err)
	}

	// A single-file book pairs only with "<stem>.epub" beside it.
	sp, _ := locate(root, "Loose.Book.m4b")
	mustWrite(t, filepath.Join(root, "Someone Else.epub"), "epub")
	if src, _, _, _ := chooseSource(sp, SourceAuto); src != SourceAudio {
		t.Fatal("a single-file book took another book's epub")
	}
	mustWrite(t, filepath.Join(root, "Loose.Book.epub"), "epub")
	if src, epub, _, err := chooseSource(sp, SourceAuto); err != nil || src != SourceEbook || epub != filepath.Join(root, "Loose.Book.epub") {
		t.Fatalf("the stem's epub = %s %s %v", src, epub, err)
	}
}

func TestWriteAttachments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := sidecars{characters: []byte(`{"work":"w","characters":[]}`), recaps: []byte(`{"work":"w","recaps":[]}`)}
	prov := provenance("0.2.0", 12, time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC), SourceAudio, "w",
		[]InputFile{{Path: "A/B/01.mp3", Size: 3, ModTime: 1700000000}})
	names, err := writeAttachments(dir, "", s, prov)
	if err != nil || strings.Join(names, ",") != "characters.json,recaps.json,bridge.json" {
		t.Fatalf("names = %v, %v", names, err)
	}
	adir := filepath.Join(dir, AttachDir)
	if info, _ := os.Stat(adir); info.Mode().Perm() != 0o755 {
		t.Fatalf(".audiosilo is %v, want 0755", info.Mode().Perm())
	}
	for _, n := range names {
		info, err := os.Stat(filepath.Join(adir, n))
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("%s = %v, %v (want 0644)", n, info, err)
		}
	}
	entries, _ := os.ReadDir(adir)
	if len(entries) != 3 {
		t.Fatalf("a temp file was left behind: %v", entries)
	}
	var got Provenance
	raw, _ := os.ReadFile(filepath.Join(adir, "bridge.json"))
	if err := json.Unmarshal(raw, &got); err != nil || got.Generator != "audiosilo-bridge" || got.Kind != Kind || got.JobID != 12 ||
		got.GeneratedAt != "2026-10-15T09:00:00Z" || got.Inputs[0].Path != "A/B/01.mp3" {
		t.Fatalf("bridge.json = %s, %v", raw, err)
	}

	// A single-file book's names carry its stem; a recaps-less run writes no recaps.
	only := sidecars{characters: s.characters}
	names, err = writeAttachments(dir, "Loose.", only, prov)
	if err != nil || strings.Join(names, ",") != "Loose.characters.json,Loose.bridge.json" {
		t.Fatalf("single-file names = %v, %v", names, err)
	}

	// Denied: a .audiosilo that is a link (or a file) is never written through.
	elsewhere := t.TempDir()
	linked := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(linked, AttachDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := writeAttachments(linked, "", s, prov); err == nil {
		t.Fatal("wrote through a linked .audiosilo")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("files landed through the link: %v", entries)
	}
	file := t.TempDir()
	mustWrite(t, filepath.Join(file, AttachDir), "not a folder")
	if _, err := writeAttachments(file, "", s, prov); err == nil {
		t.Fatal("wrote into a .audiosilo that is a file")
	}
}

func TestReadSidecars(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	if _, err := readSidecars(wd); err == nil {
		t.Fatal("no sidecars read as some")
	}
	mustWrite(t, filepath.Join(wd, "sidecars", "characters.json"), `{"work":"the-b","characters":[{"id":"a","name":"A"},{"id":"b","name":"B"}]}`)
	mustWrite(t, filepath.Join(wd, "sidecars", "recaps.json"), `{"work":"the-b","recaps":[{"through":{"chapter":1},"text":"x"}]}`)
	s, err := readSidecars(wd)
	if err != nil || s.nChars != 2 || s.nRecaps != 1 || s.workID != "the-b" {
		t.Fatalf("sidecars = %+v, %v", s, err)
	}
	mustWrite(t, filepath.Join(wd, "sidecars", "recaps.json"), `not json`)
	if _, err := readSidecars(wd); err == nil {
		t.Fatal("a broken recaps.json was read")
	}
}

func TestPurgeTranscripts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wd := filepath.Join(root, "book-1234")
	for _, f := range []string{
		"transcripts-raw/ch001.json", "transcripts-json/ch001.json", "transcripts-text/ch001.txt",
		"transcripts-repaired/ch001.json", "transcripts-corrected/ch001.txt", "chapters/ch001.flac", "_runs/x/in.txt",
		"spelling_candidates.json", "qa_report.md", "qa_report.json", "corrections.log",
		"spelling-refs/ch001.txt", "spelling-refs/prior-spellings.json", "spelling-refs/prior-marker_titles.txt",
		"spelling-refs/series-glossary.txt",
		"facts/knowledge-final.md", "sidecars/characters.json", "manifest.json", "spellings.json", "corrections.json",
		"_done/asr.json",
	} {
		mustWrite(t, filepath.Join(wd, f), "x")
	}
	if !hasTranscripts(wd) {
		t.Fatal("hasTranscripts missed them")
	}
	if err := PurgeTranscripts(root, wd, state.KindAudio); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"transcripts-raw", "transcripts-json", "transcripts-text", "transcripts-repaired",
		"transcripts-corrected", "chapters", "_runs", "spelling_candidates.json", "qa_report.md", "qa_report.json",
		"corrections.log", "spelling-refs/ch001.txt"} {
		if _, err := os.Stat(filepath.Join(wd, gone)); err == nil {
			t.Errorf("%s survived the purge", gone)
		}
	}
	for _, kept := range []string{"facts/knowledge-final.md", "sidecars/characters.json", "manifest.json", "spellings.json",
		"corrections.json", "spelling-refs/prior-spellings.json", "spelling-refs/prior-marker_titles.txt",
		"spelling-refs/series-glossary.txt", "_done/asr.json"} {
		if _, err := os.Stat(filepath.Join(wd, kept)); err != nil {
			t.Errorf("%s was purged: it is carryover or the book's result", kept)
		}
	}
	if hasTranscripts(wd) {
		t.Fatal("hasTranscripts still finds some")
	}
	// Denied: a work dir outside the work root is never touched.
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "transcripts-raw", "x"), "x")
	if err := PurgeTranscripts(root, outside, state.KindAudio); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "transcripts-raw", "x")); err != nil {
		t.Fatal("a purge reached outside the work root")
	}
}

// The sweep deletes the transcripts of finished books whose job ended at least
// the retention ago, and leaves a recent one and an unfinished one alone.
func TestSweepTranscripts(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	root := t.TempDir()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	h := New(Options{DB: db, WorkRoot: root, Now: func() time.Time { return now }})
	mk := func(job int64, name, st, status string, ended time.Time) store.Book {
		wd := filepath.Join(root, name)
		mustWrite(t, filepath.Join(wd, "transcripts-raw", "ch001.json"), "x")
		b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/" + name, WorkDir: wd, Title: name})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.SetBookState(ctx, b.ID, st, status, "", "")
		_ = db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: job, BookID: &b.ID, LibraryID: 1, RelPath: name})
		if !ended.IsZero() {
			_ = db.EndGenerateBook(ctx, job, ended)
		}
		return b
	}
	old := mk(1, "old", "done", "", now.Add(-31*24*time.Hour))
	recent := mk(2, "recent", "done", "", now.Add(-time.Hour))
	failed := mk(3, "failed", "fact_pass", "failed", now.Add(-40*24*time.Hour))
	parked := mk(4, "parked", "fact_pass", "needs_attention", now.Add(-40*24*time.Hour))
	open := mk(5, "open", "done", "", time.Time{})

	n, err := h.SweepTranscripts(ctx, 30*24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("swept %d, %v; want the old done book and the failed one", n, err)
	}
	for _, c := range []struct {
		b    store.Book
		want bool
	}{{old, false}, {recent, true}, {failed, false}, {parked, true}, {open, true}} {
		if hasTranscripts(c.b.WorkDir) != c.want {
			t.Errorf("%s: transcripts present = %v, want %v", c.b.Title, !c.want, c.want)
		}
	}
	// Retention 0: everything ended is due.
	if n, _ := h.SweepTranscripts(ctx, 0); n != 1 || hasTranscripts(recent.WorkDir) {
		t.Fatalf("a zero retention swept %d", n)
	}
}

func TestEstimateFrom(t *testing.T) {
	t.Parallel()
	e := estimateFrom(nil, 0, false)
	if e.ASRSecondsPerAudioHour != SeedASRSecondsPerAudioHour || e.AgentUSDPerAudioHour != nil || e.AgentUSDPerEbook != nil || e.Samples != 0 {
		t.Fatalf("no history = %+v", e)
	}
	books := []store.FinishedBook{
		{Kind: "audio", DurationSec: 3 * 3600, Chapters: 30, CostUSD: 9},
		{Kind: "audio", DurationSec: 1 * 3600, Chapters: 10, CostUSD: 3},
		{Kind: "audio", DurationSec: 0, CostUSD: 50}, // never inspected: no hours to divide by
		{Kind: "ebook", CostUSD: 4},
		{Kind: "ebook", CostUSD: 6},
	}
	e = estimateFrom(books, 45, true)
	if *e.AgentUSDPerAudioHour != 3 || *e.AgentUSDPerEbook != 5 || e.Samples != 4 {
		t.Fatalf("estimates = %+v", e)
	}
	if e.ASRSecondsPerAudioHour != 450 { // 45 s/chapter x 10 chapters/hour
		t.Fatalf("asr = %v", e.ASRSecondsPerAudioHour)
	}
}

func TestEstimates(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e, err := Estimates(context.Background(), db, AgentState{Backend: "claude", Ready: true, Subscription: true}, 100, time.Now())
	if err != nil || e.AgentBackend != "claude" || !e.AgentReady || !e.Subscription || e.MonthlyBudgetUSD != 100 ||
		e.MonthSpentUSD != 0 || e.ASRSecondsPerAudioHour != SeedASRSecondsPerAudioHour {
		t.Fatalf("estimates = %+v, %v", e, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}
