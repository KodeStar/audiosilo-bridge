package generate

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/events"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/testwait"
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
	for _, code := range []state.ParkCode{state.ParkAgentUnavailable, state.ParkAgentRateLimited, state.ParkAgentCredentialRefused,
		state.ParkAgentValidationExhausted,
		state.ParkMarkersNotConfident, state.ParkChaptersNotDetected, state.ParkQANoConverge, state.ParkSpellingGateFailure, state.ParkMediaToolsUnavailable,
		state.ParkASRUnavailable, state.ParkManifestChanged, state.ParkFixLoopExhausted, state.ParkASRDecodeTimeout,
		state.ParkContribUnavailable, state.ParkShareUnconfirmed, state.ParkCoreNeeded, state.ParkCorePending,
		state.ParkEbookUnreadable,
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

// No hint sends anyone to generate the book again without cancelling this job
// first: the server's enqueue finds the open (parked) job again, so that request
// would change nothing. Nor to a "Settings" other than the Bridge page's Settings
// tab (where the monthly budget is set), or `gh auth login`, which the Bridge
// doesn't have.
func TestParkHintsNameAnActionThatWorks(t *testing.T) {
	t.Parallel()
	for code, hint := range parkHints {
		low := strings.ToLower(hint)
		if strings.Contains(low, "generate") && !strings.Contains(low, "cancel this job") {
			t.Errorf("%s: %q sends the admin to generate again without cancelling the job", code, hint)
		}
		if strings.Contains(strings.ReplaceAll(hint, "Bridge's Settings tab", ""), "Settings") ||
			strings.Contains(hint, "gh auth login") {
			t.Errorf("%s: %q names something the Bridge doesn't have", code, hint)
		}
	}
	if h := ParkHint(string(state.ParkAgentCredentialRefused)); !strings.Contains(h, "claude setup-token / agent-login codex") {
		t.Errorf("credential refused hint = %q", h)
	}
	// A server that couldn't be asked about the share isn't a GitHub sign-in: each
	// park's hint names only its own cause, and the share's says it goes on by
	// itself, and what to do when the server refuses the Bridge's token (pair it
	// again).
	if h := ParkHint(string(state.ParkContribUnavailable)); strings.Contains(h, "server") {
		t.Errorf("contrib unavailable hint = %q", h)
	}
	if h := ParkHint(string(state.ParkShareUnconfirmed)); strings.Contains(h, "GitHub") ||
		!strings.Contains(h, "by itself") || !strings.Contains(h, "refuses the Bridge's token") ||
		!strings.Contains(h, "pair it again") {
		t.Errorf("share unconfirmed hint = %q", h)
	}
	// A chapters.detect job parks this too: the hint can't call it a generate job.
	if h := ParkHint(string(state.ParkChaptersNotDetected)); strings.Contains(h, "generate") || !strings.Contains(h, "match the book to a recording") {
		t.Errorf("chapters not detected hint = %q", h)
	}
}

// The budget park's hint is one for every job kind and in every place a park is
// reported (a generate job's progress and its description step's here, a describe
// job's in TestDescribeJobParks):
// a monthly cap resumes on the 1st or once raised on the page; a cap set in
// config.yaml or the environment takes a restart, after which the job carries on
// (a restart re-admits a budget-parked engine book, and runs a describe job
// again), so it asks for no Retry.
func TestBudgetParkHint(t *testing.T) {
	t.Parallel()
	code := string(state.ParkBudgetExceeded)
	hint := ParkHint(code)
	for _, phrase := range []string{"resumes on the 1st", "raised on the Bridge's Settings tab", "agent.book_budget_usd",
		"AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD", "AUDIOSILO_BRIDGE_AGENT_BOOK_BUDGET_USD", "the Bridge restarted",
		"carries on by itself, with no Retry needed"} {
		if !strings.Contains(hint, phrase) {
			t.Errorf("budget hint %q lacks %q", hint, phrase)
		}
	}
	if strings.Contains(hint, "press Retry") {
		t.Errorf("budget hint %q asks for a Retry", hint)
	}
	parked := store.Book{State: string(state.FactPass), Status: string(state.StatusNeedsAttention), ParkCode: code}
	for name, extra := range map[string]map[string]any{"describe": describe(view{book: parked}).Extra,
		"bookExtra": (&run{}).bookExtra(parked)} {
		if park, _ := extra["park"].(map[string]any); park["hint"] != hint {
			t.Errorf("%s: park = %+v, want the budget hint", name, park)
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
	prov := provenance("0.2.0", Kind, 12, time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC), SourceAudio, "w",
		[]InputFile{{Path: "A/B/01.mp3", Size: 3, ModTime: 1700000000}})
	if err := writeAttachments(dir, "", s, prov); err != nil {
		t.Fatal(err)
	}
	names := attachmentNames("", s)
	if strings.Join(names, ",") != "characters.json,recaps.json,bridge.json" {
		t.Fatalf("names = %v", names)
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
	if err := writeAttachments(dir, "Loose.", only, prov); err != nil {
		t.Fatal(err)
	}
	if names := attachmentNames("Loose.", only); strings.Join(names, ",") != "Loose.characters.json,Loose.bridge.json" {
		t.Fatalf("single-file names = %v", names)
	}
	for _, n := range []string{"Loose.characters.json", "Loose.bridge.json"} {
		if _, err := os.Stat(filepath.Join(adir, n)); err != nil {
			t.Fatalf("%s wasn't written: %v", n, err)
		}
	}

	// Denied: a .audiosilo that is a link (or a file) is never written through.
	elsewhere := t.TempDir()
	linked := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(linked, AttachDir)); err != nil {
		t.Fatal(err)
	}
	if err := writeAttachments(linked, "", s, prov); err == nil {
		t.Fatal("wrote through a linked .audiosilo")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("files landed through the link: %v", entries)
	}
	file := t.TempDir()
	mustWrite(t, filepath.Join(file, AttachDir), "not a folder")
	if err := writeAttachments(file, "", s, prov); err == nil {
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

// A book parked at contributing is looked at every poll: unchanged sidecar files
// are not read again, changed ones are.
func TestLoadSidecarsSkipsUnchangedFiles(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	chars := filepath.Join(wd, "sidecars", "characters.json")
	mustWrite(t, chars, `{"work":"w","characters":[{"id":"a","name":"A"}]}`)
	r := &run{}
	s, err := r.loadSidecars(wd)
	if err != nil || s.nChars != 1 || r.sideSig == "" {
		t.Fatalf("first read = %+v, %v (sig %q)", s, err, r.sideSig)
	}
	// Same size and time, different bytes: proof the file wasn't read again.
	info, _ := os.Stat(chars)
	mustWrite(t, chars, `{"work":"w","characters":[{"id":"b","name":"B"}]}`)
	if err := os.Chtimes(chars, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if s2, _ := r.loadSidecars(wd); s2.hash() != s.hash() {
		t.Fatal("unchanged files were read again")
	}
	later := info.ModTime().Add(time.Second)
	if err := os.Chtimes(chars, later, later); err != nil {
		t.Fatal(err)
	}
	if s3, _ := r.loadSidecars(wd); s3.hash() == s.hash() {
		t.Fatal("a changed file wasn't read again")
	}
}

// The epub's path in bridge.json is relative to the mount only when it lies
// inside it; a name that merely starts with ".." is inside.
func TestInputsOfEpub(t *testing.T) {
	t.Parallel()
	mount := t.TempDir()
	inside := filepath.Join(mount, "..foo.epub")
	mustWrite(t, inside, "epub")
	if in := inputsOf(mount, nil, inside); len(in) != 1 || in[0].Path != "..foo.epub" || in[0].Size != 4 {
		t.Fatalf("inside = %+v", in)
	}
	outside := filepath.Join(t.TempDir(), "B.epub")
	if in := inputsOf(mount, nil, outside); in[0].Path != "B.epub" {
		t.Fatalf("outside = %+v, want the bare name", in)
	}
}

// writeAttachments writes the attachments as a generate job's save does.
func writeAttachments(dir, prefix string, s sidecars, prov Provenance) error {
	files, err := attachmentFiles(s, prov)
	if err != nil {
		return err
	}
	return writeAttachFiles(dir, prefix, files)
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
			_ = db.EndGenerateBook(ctx, store.StopGuard{JobID: job, Attempt: 1}, ended)
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
		if present(c.b.WorkDir) != c.want {
			t.Errorf("%s: transcripts present = %v, want %v", c.b.Title, !c.want, c.want)
		}
	}
	// A swept book is never looked at again (swept_at), so the same sweep again
	// finds nothing.
	if n, err := h.SweepTranscripts(ctx, 30*24*time.Hour); err != nil || n != 0 {
		t.Fatalf("a second sweep swept %d, %v", n, err)
	}
	// Retention 0: everything ended is due.
	if n, _ := h.SweepTranscripts(ctx, 0); n != 1 || present(recent.WorkDir) {
		t.Fatalf("a zero retention swept %d", n)
	}
}

// With retention 0, a job's end purges its own book only: another ended job's
// book waits for the sweep, and a book still working is never purged.
func TestPurgeEndedIsTheJobsBookOnly(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	root := t.TempDir()
	h := New(Options{DB: db, WorkRoot: root})
	mk := func(job int64, name, st string) store.Book {
		wd := filepath.Join(root, name)
		mustWrite(t, filepath.Join(wd, "transcripts-raw", "ch001.json"), "x")
		b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/" + name, WorkDir: wd, Title: name})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.SetBookState(ctx, b.ID, st, "", "", "")
		_ = db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: job, BookID: &b.ID, LibraryID: 1, RelPath: name})
		_ = db.EndGenerateBook(ctx, store.StopGuard{JobID: job, Attempt: 1}, time.Now().Add(-time.Hour))
		return b
	}
	own := mk(1, "own", "done")
	other := mk(2, "other", "done")
	working := mk(3, "working", "fact_pass")
	if err := h.purgeEnded(ctx, jobs.Job{ID: 1, Attempt: 1}, own.ID); err != nil || present(own.WorkDir) {
		t.Fatalf("the job's own book: %v, present %v", err, present(own.WorkDir))
	}
	if err := h.purgeEnded(ctx, jobs.Job{ID: 3, Attempt: 1}, working.ID); err != nil || !present(working.WorkDir) {
		t.Fatalf("a working book was purged: %v", err)
	}
	if !present(other.WorkDir) {
		t.Fatal("another job's book was purged at this job's end")
	}
	// The own book is recorded swept: the sweep finds only the other one.
	if n, err := h.SweepTranscripts(ctx, 0); err != nil || n != 1 || present(other.WorkDir) {
		t.Fatalf("sweep after = %d, %v", n, err)
	}
}

// holdEngine is an engine whose holds are refused for the books in held.
type holdEngine struct {
	Engine
	held map[int64]bool
}

func (e holdEngine) Hold(id int64) (func(), bool) { return func() {}, !e.held[id] }

// A book the engine holds (a description reading it) isn't swept: the sweep leaves
// it, unmarked, for the next one.
func TestSweepSkipsAHeldBook(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	root := t.TempDir()
	eng := holdEngine{held: map[int64]bool{}}
	h := New(Options{DB: db, WorkRoot: root, Engine: func() Engine { return eng }, Poll: time.Millisecond})
	wd := filepath.Join(root, "held")
	mustWrite(t, filepath.Join(wd, "transcripts-raw", "ch001.json"), "x")
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/held", WorkDir: wd, Title: "held"})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetBookState(ctx, b.ID, "done", "", "", "")
	_ = db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "held"})
	_ = db.EndGenerateBook(ctx, store.StopGuard{JobID: 1, Attempt: 1}, time.Now().Add(-time.Hour))

	eng.held[b.ID] = true
	if n, err := h.SweepTranscripts(ctx, 0); err != nil || n != 0 || !present(wd) {
		t.Fatalf("a held book: swept %d, %v, present %v", n, err, present(wd))
	}
	if err := h.purgeEnded(ctx, jobs.Job{ID: 1, Attempt: 1}, b.ID); err != nil || !present(wd) {
		t.Fatalf("a held book purged at its job's end: %v", err)
	}
	delete(eng.held, b.ID)
	if n, err := h.SweepTranscripts(ctx, 0); err != nil || n != 1 || present(wd) {
		t.Fatalf("once released: swept %d, %v", n, err)
	}
}

// readmitEngine is an engine that re-admits a book as it hands out the hold: the
// Retry of a resumed job that landed while a purge waited for the hold.
type readmitEngine struct {
	Engine
	readmit func(id int64)
}

func (e readmitEngine) Hold(id int64) (func(), bool) {
	e.readmit(id)
	return func() {}, true
}

// A purge reads the book again under the hold: one re-admitted while it waited
// runs on its transcripts, so neither the job's end nor the sweep deletes them
// (denied); a book still finished under the hold is purged (allowed).
func TestPurgeRechecksTheBookUnderTheHold(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	root := t.TempDir()
	readmitted := false
	eng := readmitEngine{readmit: func(id int64) {
		if readmitted {
			_ = db.SetBookState(ctx, id, "fact_pass", "", "", "")
		}
	}}
	h := New(Options{DB: db, WorkRoot: root, Engine: func() Engine { return eng }, Poll: time.Millisecond})
	wd := filepath.Join(root, "readmitted")
	mustWrite(t, filepath.Join(wd, "transcripts-raw", "ch001.json"), "x")
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/readmitted", WorkDir: wd, Title: "readmitted"})
	if err != nil {
		t.Fatal(err)
	}
	// Cancelled by its job's end: failed, so finished when the purge looks.
	_ = db.SetBookState(ctx, b.ID, "fact_pass", "failed", "cancelled", "")
	_ = db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "readmitted"})
	_ = db.EndGenerateBook(ctx, store.StopGuard{JobID: 1, Attempt: 1}, time.Now().Add(-time.Hour))

	readmitted = true
	if err := h.purgeEnded(ctx, jobs.Job{ID: 1, Attempt: 1}, b.ID); err != nil || !present(wd) {
		t.Fatalf("a book re-admitted under the job end's purge lost its transcripts: %v", err)
	}
	_ = db.SetBookState(ctx, b.ID, "fact_pass", "failed", "cancelled", "")
	if n, err := h.SweepTranscripts(ctx, 0); err != nil || n != 0 || !present(wd) {
		t.Fatalf("a book re-admitted under the sweep: swept %d, %v, present %v", n, err, present(wd))
	}
	readmitted = false
	_ = db.SetBookState(ctx, b.ID, "fact_pass", "failed", "cancelled", "")
	// Its job's mapping open again (the later claim re-opened it, its Retry not
	// landed yet): still followed, so still not purged.
	_ = db.ReopenGenerateBook(ctx, 1)
	if err := h.purgeEnded(ctx, jobs.Job{ID: 1, Attempt: 1}, b.ID); err != nil || !present(wd) {
		t.Fatalf("a book its job follows again lost its transcripts: %v", err)
	}
	_ = db.EndGenerateBook(ctx, store.StopGuard{JobID: 1, Attempt: 1}, time.Now().Add(-time.Hour))
	if n, err := h.SweepTranscripts(ctx, 0); err != nil || n != 1 || present(wd) {
		t.Fatalf("a finished book: swept %d, %v, present %v", n, err, present(wd))
	}
}

// A job end's purge leaves the transcripts to a later claim of the job recorded
// before its attach reopened the mapping (denied: that attach re-admits the book
// it read cancelled, its stages' transcripts counted done); the claim's own end
// purges them (allowed).
func TestPurgeAtEndKeepsALaterClaimsTranscripts(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	root := t.TempDir()
	h := New(Options{DB: db, WorkRoot: root})
	wd := filepath.Join(root, "reclaimed")
	mustWrite(t, filepath.Join(wd, "transcripts-raw", "ch001.json"), "x")
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/reclaimed", WorkDir: wd, Title: "reclaimed"})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetBookState(ctx, b.ID, "fact_pass", "failed", "cancelled by user", "")
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 1, Kind: Kind, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	_ = db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "reclaimed"})
	if err := db.EndGenerateBook(ctx, store.StopGuard{JobID: 1, Attempt: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 1, Kind: Kind, Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	if err := h.purgeEnded(ctx, jobs.Job{ID: 1, Attempt: 1}, b.ID); err != nil || !present(wd) {
		t.Fatalf("attempt 1's end purged the book attempt 2 follows: %v", err)
	}
	if err := h.purgeEnded(ctx, jobs.Job{ID: 1, Attempt: 2}, b.ID); err != nil || present(wd) {
		t.Fatalf("attempt 2's own end: %v, present %v", err, present(wd))
	}
}

// present reports whether a book's raw transcript layer is still on disk.
func present(workDir string) bool {
	_, err := os.Stat(filepath.Join(workDir, "transcripts-raw"))
	return err == nil
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

// racedRetryEngine is an engine whose Retry meets the book parked again
// between its read and its write (the scheduler's store.ErrStatusChanged).
type racedRetryEngine struct{ Engine }

func (racedRetryEngine) RetryAsRead(context.Context, store.Book) error { return store.ErrStatusChanged }

// A person's Retry that races a re-park answers "the book changed, try again"
// (a conflict on the page), never the store's raw error (bridge#65); one that
// lands re-admits as before.
func TestRetryRacingAReparkSaysTryAgain(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/parked", WorkDir: t.TempDir(), Title: "parked"})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetBookState(ctx, b.ID, "fact_pass", "needs_attention", "the fact pass kept failing its checks", "agent_validation_exhausted")
	_ = db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "parked"})

	raced := New(Options{DB: db, Engine: func() Engine { return racedRetryEngine{} }, Poll: time.Millisecond})
	if err := raced.Retry(ctx, 1); !errors.Is(err, ErrBookChanged) {
		t.Fatalf("a retry racing a re-park = %v, want ErrBookChanged", err)
	}
	var retried int64
	landed := New(Options{DB: db, Engine: func() Engine { return retryEngine{retried: &retried} }, Poll: time.Millisecond})
	if err := landed.Retry(ctx, 1); err != nil || retried != b.ID {
		t.Fatalf("a retry that lands = %v (book %d)", err, retried)
	}
}

// retryEngine records the book a Retry re-admits.
type retryEngine struct {
	Engine
	retried *int64
}

func (e retryEngine) RetryAsRead(_ context.Context, b store.Book) error {
	*e.retried = b.ID
	return nil
}

// cancelFirstEngine is the engine with a Cancel landing on the book just before
// each re-admit: the job's end racing a person's Retry of the parked job.
type cancelFirstEngine struct{ *scheduler.Scheduler }

func (e cancelFirstEngine) RetryAsRead(ctx context.Context, b store.Book) error {
	_ = e.Cancel(ctx, b.ID)
	return e.Scheduler.RetryAsRead(ctx, b)
}

// bridge#65 (P1): a person's Retry of a parked job reads the book parked; a
// Cancel landing before the engine re-admits it (the job's end) leaves the book
// cancelled and the Retry says the book changed, where the engine's own read used
// to find a cancelled book and take it up again with no job left to follow it.
func TestRetryNeverTakesUpABookCancelledAfterItsRead(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/parked", WorkDir: t.TempDir(), Title: "parked"})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetBookState(ctx, b.ID, "fact_pass", "needs_attention", "the fact pass kept failing its checks", "agent_validation_exhausted")
	_ = db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "parked"})
	sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
	h := New(Options{DB: db, Engine: func() Engine { return cancelFirstEngine{sched} }, Poll: time.Millisecond})
	if err := h.Retry(ctx, 1); !errors.Is(err, ErrBookChanged) {
		t.Fatalf("a Retry over a cancel = %v, want ErrBookChanged", err)
	}
	if got, _ := db.GetBook(ctx, b.ID); !scheduler.IsCancelled(got) {
		t.Fatalf("the cancelled book was taken up again: status %q error %q", got.Status, got.Error)
	}
}

// bridge#65: the runner records a job lost (another server's job at a start)
// only after Abandon returns, and a job waiting for that end reads the row. With
// the engine not started yet, Abandon used to leave the book to a goroutine that
// waited for it; now the book is cancelled in the store and the end recorded
// before Abandon returns. Once the engine is up, a status written back over the
// cancel (a stage its scheduler began before the cancel landed parked the book)
// is stopped again, unless another job, or a later claim of the same job, follows
// the book.
func TestAbandonBeforeTheEngineStartsStopsTheBookAtOnce(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	newBook := func(job int64, slug string) store.Book {
		b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/" + slug, WorkDir: t.TempDir(), Title: slug})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.SetBookState(ctx, b.ID, "asr", "", "", "")
		if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: job, BookID: &b.ID, LibraryID: 1, RelPath: slug}); err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := newBook(5, "dropped")
	sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
	var up atomic.Bool
	h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond, Engine: func() Engine {
		if up.Load() {
			return sched
		}
		return nil
	}})
	h.Abandon(ctx, jobs.Job{ID: 5, Kind: Kind, Attempt: 1})
	got, _ := db.GetBook(ctx, b.ID)
	g, _ := db.GetGenerateBook(ctx, 5)
	if !scheduler.IsCancelled(got) || g.EndedAt == "" {
		t.Fatalf("after Abandon with no engine: status %q error %q, ended %q; want cancelled and ended",
			got.Status, got.Error, g.EndedAt)
	}
	// A stage the engine began before the cancel writes its status over it.
	if err := db.SetBookStatus(ctx, b.ID, "needs_attention", "parked", "agent_unavailable"); err != nil {
		t.Fatal(err)
	}
	up.Store(true)
	if !poll(func() bool { got, _ = db.GetBook(ctx, b.ID); return scheduler.IsCancelled(got) }) {
		t.Fatalf("the book runs with no job once the engine is up: status %q", got.Status)
	}
	// Each case below is a job whose end Abandon's release recorded.
	abandoned := func(job int64, slug string) store.Book {
		b := newBook(job, slug)
		if err := db.EndGenerateBook(ctx, store.StopGuard{JobID: job, Attempt: 1}, time.Now()); err != nil {
			t.Fatal(err)
		}
		return b
	}
	kept := func(what string, job jobs.Job, b store.Book) {
		t.Helper()
		h.stopStartedStage(ctx, job, b.ID)
		if got, _ := db.GetBook(ctx, b.ID); got.Status != "" {
			t.Fatalf("%s was stopped: status %q", what, got.Status)
		}
	}
	// A book another open job follows now (a new job took it up) is left to it.
	taken := abandoned(6, "taken")
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 7, Kind: Kind, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 7, BookID: &taken.ID, LibraryID: 1, RelPath: "taken"}); err != nil {
		t.Fatal(err)
	}
	kept("a book another job follows", jobs.Job{ID: 6, Kind: Kind, Attempt: 1}, taken)
	// So is one a later claim of the same job follows: one running here already
	// (its attempt recorded), or one whose attach reopened the job's mapping.
	later := abandoned(8, "later")
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 8, Kind: Kind, Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	kept("a book a later attempt follows", jobs.Job{ID: 8, Kind: Kind, Attempt: 1}, later)
	reopened := abandoned(9, "reopened")
	if err := db.ReopenGenerateBook(ctx, 9); err != nil {
		t.Fatal(err)
	}
	kept("a book whose job's mapping was reopened", jobs.Job{ID: 9, Kind: Kind, Attempt: 1}, reopened)
}

// With transcripts.retention_days 0 and no engine yet, Abandon cancels the book
// in the store and records the job's end at once, but leaves the transcripts:
// nothing holds the book against a stage its starting scheduler may begin (hold
// takes none without an engine). They go once the engine is up and its own
// Cancel has stopped the book, in the task Abandon handed to Background, which
// ends with that task's context while it still waits for an engine.
func TestAbandonBeforeTheEngineStartsPurgesOnlyOnceItIsUp(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	root := t.TempDir()
	sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, root, false)
	var up atomic.Bool
	var tasks []func(context.Context)
	h := New(Options{DB: db, WorkRoot: root, RetentionDays: 0, Poll: time.Millisecond,
		Background: func(fn func(context.Context)) { tasks = append(tasks, fn) },
		Engine: func() Engine {
			if up.Load() {
				return sched
			}
			return nil
		}})
	wd := filepath.Join(root, "dropped")
	mustWrite(t, filepath.Join(wd, "transcripts-raw", "ch001.json"), "x")
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/dropped", WorkDir: wd, Title: "dropped"})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetBookState(ctx, b.ID, "asr", "", "", "")
	if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 5, BookID: &b.ID, LibraryID: 1, RelPath: "dropped"}); err != nil {
		t.Fatal(err)
	}
	job := jobs.Job{ID: 5, Kind: Kind, Attempt: 1}
	h.Abandon(ctx, job)
	got, _ := db.GetBook(ctx, b.ID)
	g, _ := db.GetGenerateBook(ctx, 5)
	if !scheduler.IsCancelled(got) || g.EndedAt == "" {
		t.Fatalf("after Abandon with no engine: status %q error %q, ended %q; want cancelled and ended",
			got.Status, got.Error, g.EndedAt)
	}
	if !present(wd) || g.SweptAt != "" {
		t.Fatalf("the transcripts went with no engine to hold the book: present %v, swept %q", present(wd), g.SweptAt)
	}
	if len(tasks) != 1 {
		t.Fatalf("Abandon started %d background tasks, want the wait for the engine", len(tasks))
	}
	// The Bridge stopping ends the wait, with nothing touched.
	stopped, stop := context.WithCancel(ctx)
	stop()
	tasks[0](stopped)
	if !present(wd) {
		t.Fatal("a wait the Bridge's stop ended deleted the transcripts")
	}
	// The engine up: the book is stopped through it, then its transcripts go.
	up.Store(true)
	tasks[0](ctx)
	if g, _ = db.GetGenerateBook(ctx, 5); present(wd) || g.SweptAt == "" {
		t.Fatalf("once the engine is up: transcripts present %v, swept %q; want them deleted", present(wd), g.SweptAt)
	}
	if got, _ = db.GetBook(ctx, b.ID); !scheduler.IsCancelled(got) {
		t.Fatalf("the book is no longer cancelled: status %q error %q", got.Status, got.Error)
	}
}

// claimFirstEngine is the engine with a later claim of the job landing just
// before each guarded cancel: the claim recorded, then its attach reopening the
// job's mapping and re-admitting the book it read cancelled.
type claimFirstEngine struct {
	*scheduler.Scheduler
	db *store.DB
}

func (e claimFirstEngine) CancelUnlessFollowed(ctx context.Context, id int64, g store.StopGuard) error {
	if err := e.db.RecordClaim(ctx, store.ServerJob{ID: g.JobID, Kind: Kind, Attempt: g.Attempt + 1}); err != nil {
		return err
	}
	if err := e.db.ReopenGenerateBook(ctx, g.JobID); err != nil {
		return err
	}
	if b, err := e.db.GetBook(ctx, id); err == nil && scheduler.IsCancelled(b) {
		if err := e.RetryAsRead(ctx, b); err != nil {
			return err
		}
	}
	return e.Scheduler.CancelUnlessFollowed(ctx, id, g)
}

// A later claim of a job landing between an earlier attempt's checks and its
// cancel (the claim recorded, its attach reopening the mapping and re-admitting
// the book) keeps the book: the cancel, and release's end, are written only while
// no later attempt is recorded, in the one statement. Both ends: the stop of a
// stage the engine began before an abandoned job's cancel, and release.
func TestJobEndNeverStopsALaterClaimLandingBeforeItsCancel(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	newBook := func(job int64, slug string) store.Book {
		b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/" + slug, WorkDir: t.TempDir(), Title: slug})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.SetBookState(ctx, b.ID, "asr", "", "", "")
		if err := db.RecordClaim(ctx, store.ServerJob{ID: job, Kind: Kind, Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: job, BookID: &b.ID, LibraryID: 1, RelPath: slug}); err != nil {
			t.Fatal(err)
		}
		return b
	}
	sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
	h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond,
		Engine: func() Engine { return claimFirstEngine{sched, db} }})
	kept := func(t *testing.T, job int64, b store.Book) {
		t.Helper()
		got, _ := db.GetBook(ctx, b.ID)
		g, _ := db.GetGenerateBook(ctx, job)
		if got.Status != "" || g.EndedAt != "" {
			t.Fatalf("status %q error %q, ended %q; want the later claim's book admitted and its mapping open",
				got.Status, got.Error, g.EndedAt)
		}
	}
	t.Run("an abandoned job's late stop", func(t *testing.T) {
		abandoned := newBook(1, "abandoned")
		if err := scheduler.CancelStored(ctx, db, abandoned.ID); err != nil {
			t.Fatal(err)
		}
		if err := db.EndGenerateBook(ctx, store.StopGuard{JobID: 1, Attempt: 1}, time.Now()); err != nil {
			t.Fatal(err)
		}
		h.stopStartedStage(ctx, jobs.Job{ID: 1, Kind: Kind, Attempt: 1}, abandoned.ID)
		kept(t, 1, abandoned)
	})
	t.Run("release", func(t *testing.T) {
		running := newBook(2, "running")
		if h.release(ctx, jobs.Job{ID: 2, Kind: Kind, Attempt: 1}, running.ID, true) {
			t.Error("release reported the end of a job a later claim follows")
		}
		kept(t, 2, running)
	})
}

// otherServerFirstEngine is the engine with another server's claim of the
// job's id and attempt (the Bridge paired since) recorded just before each
// guarded cancel.
type otherServerFirstEngine struct {
	*scheduler.Scheduler
	db *store.DB
}

func (e otherServerFirstEngine) CancelUnlessFollowed(ctx context.Context, id int64, g store.StopGuard) error {
	if err := e.db.RecordClaim(ctx, store.ServerJob{ID: g.JobID, Kind: Kind, Attempt: g.Attempt, ServerID: "b", Pairing: "p2"}); err != nil {
		return err
	}
	return e.Scheduler.CancelUnlessFollowed(ctx, id, g)
}

// Job ids are each server's own: once the Bridge is paired with another
// server, that server's claim may hold the row under a dropped job's id and
// attempt, its own book mapped there. The dropped claim's release never stops
// that claim's book nor ends its mapping (denied: the mapping read by the id
// alone, and a guard of id and attempt only, which another server's claim of
// the same attempt passes), even when the claim lands between release's check
// and its writes (denied); the claim's own release stops its book (allowed).
func TestReleaseLeavesAnotherServersClaimOfTheSameJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	open := func(t *testing.T) *store.DB {
		db, err := store.Open(ctx, ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	claim := func(t *testing.T, db *store.DB, server, pairing, slug string) store.Book {
		t.Helper()
		if err := db.RecordClaim(ctx, store.ServerJob{ID: 5, Kind: Kind, Attempt: 1, ServerID: server, Pairing: pairing}); err != nil {
			t.Fatal(err)
		}
		b, err := db.CreateMappedBook(ctx, store.NewBook{SourcePath: "/lib/" + slug, WorkDir: t.TempDir(), Title: slug},
			store.GenerateBook{ServerJobID: 5, LibraryID: 1, RelPath: slug})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetBookState(ctx, b.ID, "asr", "", "", ""); err != nil {
			t.Fatal(err)
		}
		return b
	}
	running := func(t *testing.T, db *store.DB, b store.Book) {
		t.Helper()
		got, _ := db.GetBook(ctx, b.ID)
		g, _ := db.GetGenerateBook(ctx, 5)
		if got.Status != "" || g.EndedAt != "" {
			t.Fatalf("status %q error %q, ended %q; want the other server's claim's book running and its mapping open",
				got.Status, got.Error, g.EndedAt)
		}
	}
	dropped := jobs.ClaimedUnder(jobs.Job{ID: 5, Kind: Kind, Attempt: 1}, "a", "p1")
	t.Run("abandoned after the claim mapped its book", func(t *testing.T) {
		t.Parallel()
		db := open(t)
		sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
		h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond, Engine: func() Engine { return sched }})
		b := claim(t, db, "b", "p2", "theirs")
		h.Abandon(ctx, dropped)
		running(t, db, b)
		h.Abandon(ctx, jobs.ClaimedUnder(jobs.Job{ID: 5, Kind: Kind, Attempt: 1}, "b", "p2"))
		if got, _ := db.GetBook(ctx, b.ID); !scheduler.IsCancelled(got) {
			t.Fatalf("the claim's own release didn't stop its book: status %q", got.Status)
		}
	})
	t.Run("the claim landing before release's writes", func(t *testing.T) {
		t.Parallel()
		db := open(t)
		sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
		h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond,
			Engine: func() Engine { return otherServerFirstEngine{sched, db} }})
		b := claim(t, db, "a", "p1", "ours")
		if h.release(ctx, dropped, b.ID, true) {
			t.Error("release reported the end of a job another server's claim holds the row of")
		}
		running(t, db, b)
	})
	// Allowed: another server's claim of a kind that follows no engine book
	// (its podcast.fetch) leaves the mapping's book the dropped claim's, which
	// would otherwise run on with its mapping open and nothing to stop it.
	t.Run("another server's claim of a kind with no engine book", func(t *testing.T) {
		t.Parallel()
		db := open(t)
		sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
		h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond, Engine: func() Engine { return sched }})
		b := claim(t, db, "a", "p1", "ours")
		if err := db.RecordClaim(ctx, store.ServerJob{ID: 5, Kind: "podcast.fetch", Attempt: 1, ServerID: "b", Pairing: "p2"}); err != nil {
			t.Fatal(err)
		}
		h.Abandon(ctx, dropped)
		if got, _ := db.GetBook(ctx, b.ID); !scheduler.IsCancelled(got) {
			t.Fatalf("the dropped claim's book wasn't stopped: status %q", got.Status)
		}
		if g, _ := db.GetGenerateBook(ctx, 5); g.EndedAt == "" {
			t.Fatal("the dropped claim's mapping wasn't ended")
		}
	})
	// Allowed: another server's claim of an engine kind in another library
	// stops the dropped claim's book its id maps to, which that claim's own
	// book replaces in the mapping (denied: left to run on unmapped, as the
	// dropped claim's release leaves it to that claim). Its own library's book
	// is resumed, not stopped.
	t.Run("another server's claim in another library", func(t *testing.T) {
		t.Parallel()
		db := open(t)
		sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
		h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond, Engine: func() Engine { return sched }})
		b := claim(t, db, "a", "p1", "ours")
		for _, kind := range []string{Kind, ChaptersKind, ReadalongKind} {
			theirs := store.ServerJob{ID: 5, Kind: kind, Attempt: 1, ServerID: "b", Pairing: "p2"}
			if err := db.RecordClaim(ctx, theirs); err != nil {
				t.Fatal(err)
			}
			if other, err := db.OtherClaim(ctx, stopGuard(dropped, false)); err != nil || !other {
				t.Fatalf("%s: OtherClaim = %v %v; want the engine kind's claim to follow the book", kind, other, err)
			}
		}
		same, other := int64(1), int64(2)
		job := jobs.ClaimedUnder(jobs.Job{ID: 5, Kind: ReadalongKind, Attempt: 1, LibraryID: &same}, "b", "p2")
		if _, resumed, err := h.resumeMapping(ctx, job); err != nil || !resumed {
			t.Fatalf("its own library: resumed %v, %v", resumed, err)
		}
		running(t, db, b)
		job.LibraryID = &other
		if _, resumed, err := h.resumeMapping(ctx, job); err != nil || resumed {
			t.Fatalf("another library: resumed %v, %v", resumed, err)
		}
		if got, _ := db.GetBook(ctx, b.ID); !scheduler.IsCancelled(got) {
			t.Fatalf("another library's book its id maps to wasn't stopped: status %q", got.Status)
		}
	})
}

// failFirstEngine is the engine with a stage failing the book just after the
// job's end read it running and before its guarded cancel. err records what the
// cancel returned.
type failFirstEngine struct {
	*scheduler.Scheduler
	db  *store.DB
	err *error
}

func (e failFirstEngine) CancelUnlessFollowed(ctx context.Context, id int64, g store.StopGuard) error {
	if err := e.db.SetBookStatus(ctx, id, "failed", "asr: out of memory", ""); err != nil {
		return err
	}
	*e.err = e.Scheduler.CancelUnlessFollowed(ctx, id, g)
	return *e.err
}

// A stage's failure landing between a job's end reading its engine book and the
// cancel keeps its own error (store.ErrSettled, not ErrFollowed): the end is
// recorded as the job's, and nothing is logged as a failed cancel.
func TestJobEndNeverCancelsOverAFailureWrittenSinceItsRead(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/failing", WorkDir: t.TempDir(), Title: "failing"})
	if err != nil {
		t.Fatal(err)
	}
	_ = db.SetBookState(ctx, b.ID, "asr", "", "", "")
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 1, Kind: Kind, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 1, BookID: &b.ID, LibraryID: 1, RelPath: "failing"}); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	var cancelErr error
	sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
	h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		Engine: func() Engine { return failFirstEngine{sched, db, &cancelErr} }})
	if !h.release(ctx, jobs.Job{ID: 1, Kind: Kind, Attempt: 1}, b.ID, true) {
		t.Error("release didn't record the end of the job's own attempt")
	}
	if !errors.Is(cancelErr, store.ErrSettled) {
		t.Errorf("the cancel = %v, want store.ErrSettled", cancelErr)
	}
	if got, _ := db.GetBook(ctx, b.ID); got.Status != "failed" || got.Error != "asr: out of memory" {
		t.Fatalf("the failure was overwritten: status %q error %q", got.Status, got.Error)
	}
	if strings.Contains(logs.String(), "cancelling the engine book failed") {
		t.Errorf("a settled book logged a failed cancel: %s", logs.String())
	}
}

// parkFirstEngine is the engine with a stage the cancel interrupted parking the
// book just before attach's re-admit.
type parkFirstEngine struct {
	*scheduler.Scheduler
	db *store.DB
}

func (e parkFirstEngine) RetryAsRead(ctx context.Context, b store.Book) error {
	if err := e.db.SetBookStatus(ctx, b.ID, "needs_attention", "parked", "agent_unavailable"); err != nil {
		return err
	}
	return e.Scheduler.RetryAsRead(ctx, b)
}

// bridge#65: attach takes up the cancel it read, not the book's status at the
// re-admit: a status changed since is followed as it is, and a cancel still
// there is re-admitted.
func TestAttachReadmitsTheCancelItRead(t *testing.T) {
	t.Parallel()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
	cancelled := func(slug string) store.Book {
		b, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/lib/" + slug, WorkDir: t.TempDir(), Title: slug})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.SetBookState(ctx, b.ID, "asr", "", "", "")
		if err := sched.Cancel(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
		return b
	}
	h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond})

	parked := cancelled("parked")
	r := &run{job: jobs.Job{ID: 1, Kind: Kind, Attempt: 2}, bookID: parked.ID}
	if err := h.attach(ctx, parkFirstEngine{sched, db}, r, store.GenerateBook{ServerJobID: 1, BookID: &parked.ID}); err != nil {
		t.Fatalf("attach over a status changed since its read = %v, want the book followed", err)
	}
	if got, _ := db.GetBook(ctx, parked.ID); got.ParkCode != "agent_unavailable" {
		t.Fatalf("the park written since was re-admitted: status %q park %q", got.Status, got.ParkCode)
	}

	back := cancelled("back")
	r = &run{job: jobs.Job{ID: 2, Kind: Kind, Attempt: 2}, bookID: back.ID}
	if err := h.attach(ctx, sched, r, store.GenerateBook{ServerJobID: 2, BookID: &back.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetBook(ctx, back.ID); got.Status != "" {
		t.Fatalf("the job's own cancel wasn't re-admitted: status %q error %q", got.Status, got.Error)
	}
}

// A contribute=issue sidecars.generate run is woken the moment its job hears
// share_off (the store has the book local by then), so its look lifts a share
// park at once rather than an idle poll later. Denied: an answer after the
// first doesn't wake it again (every answer carries share_off while the
// module is off), a run not asked to share isn't woken, nor one whose call was
// dropped.
func TestShareOffWakesTheRun(t *testing.T) {
	t.Parallel()
	h := New(Options{WorkRoot: t.TempDir()})
	newRun := func(contribute string) *run {
		return &run{job: jobs.Job{ID: 1}, kind: Kind, rep: jobs.LocalReporter(), poke: make(chan struct{}, 1),
			params: Params{Contribute: contribute}}
	}
	woken := func(r *run) bool {
		select {
		case <-r.poke:
			return true
		default:
			return false
		}
	}
	r := newRun(ContributeIssue)
	stop := h.wakeOnShareOff(r)
	if woken(r) {
		t.Fatal("woken before share_off")
	}
	jobs.HearShareOff(r.rep)
	if !woken(r) {
		t.Fatal("the run wasn't woken to lift the park")
	}
	jobs.HearShareOff(r.rep)
	if woken(r) {
		t.Fatal("woken again by a later answer that carries share_off")
	}
	stop()
	jobs.HearShareOff(r.rep)
	if woken(r) {
		t.Fatal("woken after its call was dropped")
	}

	local := newRun(ContributeLocal)
	defer h.wakeOnShareOff(local)()
	jobs.HearShareOff(local.rep)
	if woken(local) {
		t.Fatal("a run not asked to share was woken")
	}
}

// fakeAsker answers ShareOff per job: an error, else off. asked, when set,
// counts the jobs asked.
type fakeAsker struct {
	off   map[int64]bool
	errs  map[int64]error
	asked *atomic.Int32
}

func (a fakeAsker) ShareOff(_ context.Context, id int64, _ bool) (bool, error) {
	if a.asked != nil {
		a.asked.Add(1)
	}
	return a.off[id], a.errs[id]
}

// A book that no open generate-and-share job follows stays local (fail
// closed, no_share_job), with no job asked: none follows it, or only a
// contribute=local job, which is never asked (denied: never "share on"). A
// share job that says its share still stands keeps it shared (allowed).
func TestShareOffWithNoShareJobKeepsTheBookLocal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	b, err := db.CreateBook(ctx, store.NewBook{Title: "B", SourcePath: "/src/b", WorkDir: t.TempDir(),
		ContribMode: store.ContribModeIssue})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{DB: db, WorkRoot: t.TempDir()})
	var asked atomic.Int32
	h.AskThrough(fakeAsker{asked: &asked})
	if why, err := h.ShareOff(ctx, b.ID, true); why != ShareSkippedNoShareJob || err != nil {
		t.Fatalf("no job follows the book = %q %v, want no_share_job", why, err)
	}
	follow := func(id int64, params string) {
		t.Helper()
		if err := db.RecordClaim(ctx, store.ServerJob{ID: id, Kind: Kind, Params: json.RawMessage(params), Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: id, BookID: &b.ID, LibraryID: 1, RelPath: "B"}); err != nil {
			t.Fatal(err)
		}
	}
	follow(1, `{"contribute":"local"}`)
	if why, err := h.ShareOff(ctx, b.ID, true); why != ShareSkippedNoShareJob || err != nil || asked.Load() != 0 {
		t.Fatalf("a local job only = %q %v (asked %d), want no_share_job with none asked", why, err, asked.Load())
	}
	follow(2, `{"contribute":"issue"}`)
	if why, err := h.ShareOff(ctx, b.ID, true); why != "" || err != nil || asked.Load() != 1 {
		t.Fatalf("a share job still sharing = %q %v (asked %d), want shared, the share job asked", why, err, asked.Load())
	}
}

// keepShareLocal says contributions_off for a book kept local with no reason
// recorded, and for a no_share_job book once the run heard share_off (it wins
// over the book's reason, and the run owes no share). A share_off stored on
// the book is its reason too (TestFollowedAgainSharesANoShareJobBook).
func TestKeepShareLocalTakesTheBooksReason(t *testing.T) {
	t.Parallel()
	h := New(Options{WorkRoot: t.TempDir()})
	for _, c := range []struct {
		recorded string
		heard    bool
		want     string
	}{
		{"", false, ShareSkippedContributionsOff},
		{store.ShareSkippedNoShareJob, true, ShareSkippedContributionsOff},
	} {
		r := &run{job: jobs.Job{ID: 1, Kind: Kind}, kind: Kind, rep: jobs.LocalReporter(),
			params: Params{Contribute: ContributeIssue}}
		if c.heard {
			jobs.HearShareOff(r.rep)
		}
		b := store.Book{ID: 1, State: string(state.Done), ContribMode: store.ContribModeLocal, ShareSkipped: c.recorded}
		h.keepShareLocal(context.Background(), nil, r, &b)
		if r.shareSkipped != c.want || r.params.Contribute != ContributeLocal {
			t.Errorf("recorded %q: share skipped %q (contribute %q), want %q", c.recorded, r.shareSkipped, r.params.Contribute, c.want)
		}
	}
}

// runningMappedBook is a store with server job 1 (sidecars.generate, attempt
// 1) mapped to an issue-mode engine book that is running (asr).
func runningMappedBook(t *testing.T) (*store.DB, store.Book) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.RecordClaim(ctx, store.ServerJob{ID: 1, Kind: Kind, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateMappedBook(ctx, store.NewBook{SourcePath: "/lib/b", WorkDir: t.TempDir(), Title: "b",
		ContribMode: store.ContribModeIssue}, store.GenerateBook{ServerJobID: 1, LibraryID: 1, RelPath: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetBookState(ctx, b.ID, "asr", "", "", ""); err != nil {
		t.Fatal(err)
	}
	return db, b
}

// A share job that follows its engine book again (claimed again after a lease
// lapsed, or resumed) shares a book the stage kept local only because no share
// job followed it (no_share_job): one not done is turned back to issue mode for
// its contributing stage (allowed), a done one is filed by the job itself
// (shareOwed, allowed); a share_off on the book still wins, and nothing is
// filed (denied).
func TestFollowedAgainSharesANoShareJobBook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	newRun := func() *run {
		return &run{job: jobs.Job{ID: 1, Kind: Kind, Attempt: 1}, kind: Kind, rep: jobs.LocalReporter(),
			params: Params{Contribute: ContributeIssue}}
	}
	setup := func(t *testing.T, done, shareOff bool) (*store.DB, *Handler, *fakeSharer, store.Book) {
		t.Helper()
		db, b := runningMappedBook(t)
		if err := db.KeepBookLocal(ctx, b.ID, store.ShareSkippedNoShareJob); err != nil {
			t.Fatal(err)
		}
		if done {
			if err := db.SetBookState(ctx, b.ID, string(state.Done), "", "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if shareOff {
			// Another share job on the book heard share_off.
			if err := db.RecordClaim(ctx, store.ServerJob{ID: 2, Kind: Kind, Attempt: 1, ShareOff: true}); err != nil {
				t.Fatal(err)
			}
			if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 2, BookID: &b.ID, LibraryID: 1, RelPath: "b"}); err != nil {
				t.Fatal(err)
			}
		}
		sharer := &fakeSharer{}
		h := New(Options{DB: db, WorkRoot: t.TempDir(), Log: slog.New(slog.DiscardHandler),
			Sharer: func() Sharer { return sharer }})
		got, err := db.GetBook(ctx, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		return db, h, sharer, got
	}
	book := func(t *testing.T, db *store.DB, id int64) store.Book {
		t.Helper()
		b, err := db.GetBook(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	t.Run("not done", func(t *testing.T) {
		t.Parallel()
		db, h, _, b := setup(t, false, false)
		r := newRun()
		h.keepShareLocal(ctx, nil, r, &b)
		if r.shareSkipped != "" || r.params.Contribute != ContributeIssue {
			t.Fatalf("the run dropped its share: %q %q", r.shareSkipped, r.params.Contribute)
		}
		if got := book(t, db, b.ID); got.ContribMode != store.ContribModeIssue || got.ShareSkipped != "" {
			t.Fatalf("book = %q %q, want issue with no reason", got.ContribMode, got.ShareSkipped)
		}
	})
	t.Run("not done, share_off", func(t *testing.T) {
		t.Parallel()
		db, h, _, b := setup(t, false, true)
		r := newRun()
		h.keepShareLocal(ctx, nil, r, &b)
		// The share_off is the book's reason now (keepSharedOffLocal).
		if r.shareSkipped != ShareSkippedContributionsOff || r.params.Contribute != ContributeLocal {
			t.Fatalf("a share_off on the book: share skipped %q (contribute %q), want contributions_off, local",
				r.shareSkipped, r.params.Contribute)
		}
		if got := book(t, db, b.ID); got.ContribMode != store.ContribModeLocal {
			t.Fatalf("book mode %q, want local", got.ContribMode)
		}
	})
	t.Run("done", func(t *testing.T) {
		t.Parallel()
		db, h, sharer, b := setup(t, true, false)
		r := newRun()
		if h.keepShareLocal(ctx, nil, r, &b); r.shareSkipped != "" {
			t.Fatalf("the run dropped the share it owes: %q", r.shareSkipped)
		}
		if !r.owesShare(&b) {
			t.Fatal("the run owes no share")
		}
		filed, err := h.shareOwed(ctx, r, b)
		if err != nil || !filed || len(sharer.calls) != 1 {
			t.Fatalf("shareOwed = %v %v (calls %v), want one share filed", filed, err, sharer.calls)
		}
		if got := book(t, db, b.ID); got.ContribMode != store.ContribModeIssue || got.ShareSkipped != "" {
			t.Fatalf("book = %q %q, want shared", got.ContribMode, got.ShareSkipped)
		}
	})
	t.Run("done since the look", func(t *testing.T) {
		// The look read the book running; its stage filed locally and it is done
		// by the hold: it isn't turned back (finish owes the share, allowed).
		t.Parallel()
		db, h, _, stale := setup(t, false, false)
		if err := db.SetBookState(ctx, stale.ID, string(state.Done), "", "", ""); err != nil {
			t.Fatal(err)
		}
		r := newRun()
		h.keepShareLocal(ctx, nil, r, &stale)
		got := book(t, db, stale.ID)
		if got.ContribMode != store.ContribModeLocal || got.ShareSkipped != store.ShareSkippedNoShareJob ||
			r.shareSkipped != "" || !r.owesShare(&got) {
			t.Fatalf("book = %q %q, run skipped %q owes %v; want it left local for finish to file",
				got.ContribMode, got.ShareSkipped, r.shareSkipped, r.owesShare(&got))
		}
	})
	t.Run("done, the share stopped standing by the hold", func(t *testing.T) {
		// A share_off stored after shareOwed's look (another job's on the book)
		// still wins: asked under the hold, nothing is filed (denied).
		t.Parallel()
		_, h, sharer, b := setup(t, true, false)
		r := newRun()
		_, err := h.shareConfirmed(ctx, r, sharer, b.ID, ContributeParams{},
			func(context.Context) (bool, error) { return false, nil })
		if !errors.Is(err, errShareOffNow) || len(sharer.calls) != 0 {
			t.Fatalf("shareConfirmed = %v (calls %v), want errShareOffNow with nothing filed", err, sharer.calls)
		}
	})
	t.Run("done, no GitHub sign-in", func(t *testing.T) {
		// A share it owes that can't file for want of a sign-in parks the job
		// as the contributing stage parks the book (contrib_unavailable), with
		// nothing filed (denied), and files once a sign-in wakes it (allowed),
		// rather than failing the whole job.
		t.Parallel()
		db, h, sharer, b := setup(t, true, false)
		sharer.err = &scheduler.ParkError{Code: state.ParkContribUnavailable, Reason: pipeline.ContribUnavailableMsg,
			Cause: contrib.ErrNoCredential}
		r := newRun()
		r.poke = make(chan struct{}, 1)
		type owed struct {
			filed bool
			err   error
		}
		out := make(chan owed, 1)
		go func() {
			filed, err := h.shareOwed(ctx, r, b)
			out <- owed{filed, err}
		}()
		parkedWith := func() string {
			p, _ := r.rep.Last().Extra["park"].(map[string]any)
			code, _ := p["code"].(string)
			return code
		}
		if !poll(func() bool { return parkedWith() == string(state.ParkContribUnavailable) }) {
			t.Fatalf("the job never parked contrib_unavailable (progress %+v)", r.rep.Last())
		}
		select {
		case o := <-out:
			t.Fatalf("shareOwed ended while parked: %v %v", o.filed, o.err)
		default:
		}
		sharer.mu.Lock()
		sharer.err = nil
		sharer.mu.Unlock()
		if n := h.WakeParked(state.ParkContribUnavailable); n != 1 {
			t.Fatalf("a sign-in woke %d jobs, want the parked share", n)
		}
		o := <-out
		if o.err != nil || !o.filed || len(sharer.calls) != 2 {
			t.Fatalf("shareOwed = %v %v (calls %v), want filed on the second try", o.filed, o.err, sharer.calls)
		}
		if got := book(t, db, b.ID); got.ContribMode != store.ContribModeIssue {
			t.Fatalf("book mode %q, want shared", got.ContribMode)
		}
	})
	t.Run("done, share_off heard while a try parks", func(t *testing.T) {
		// A share_off heard while a try runs that then parks with a retry_at
		// (a sign-in whose refresh failed) drops the share at once (allowed):
		// the park's drain of a Retry pressed meanwhile takes the run's one
		// wake-up for share_off, and the job would otherwise sit parked over a
		// share that is off until the retry_at (denied).
		t.Parallel()
		db, h, sharer, b := setup(t, true, false)
		r := newRun()
		r.poke = make(chan struct{}, 1)
		defer h.wakeOnShareOff(r)()
		sharer.err = &scheduler.ParkError{Code: state.ParkContribUnavailable, Reason: pipeline.ContribUnrefreshedMsg,
			Cause: contrib.ErrSignInUnrefreshed, RetryAfter: time.Now().Add(time.Hour)}
		sharer.filing = func() { jobs.HearShareOff(r.rep) }
		type owed struct {
			filed bool
			err   error
		}
		out := make(chan owed, 1)
		go func() {
			filed, err := h.shareOwed(ctx, r, b)
			out <- owed{filed, err}
		}()
		select {
		case o := <-out:
			if o.err != nil || o.filed {
				t.Fatalf("shareOwed = %v %v, want the share dropped", o.filed, o.err)
			}
		case <-time.After(testwait.Scaled(10 * time.Second)):
			t.Fatalf("the job stayed parked over a share that is off (progress %+v)", r.rep.Last())
		}
		if r.shareSkipped != ShareSkippedContributionsOff || r.params.Contribute != ContributeLocal {
			t.Fatalf("share skipped %q (contribute %q), want contributions_off, local", r.shareSkipped, r.params.Contribute)
		}
		if got := book(t, db, b.ID); got.ContribMode != store.ContribModeLocal {
			t.Fatalf("book mode %q, want local", got.ContribMode)
		}
	})
	t.Run("done, share_off", func(t *testing.T) {
		t.Parallel()
		db, h, sharer, b := setup(t, true, true)
		r := newRun()
		filed, err := h.shareOwed(ctx, r, b)
		if err != nil || filed || len(sharer.calls) != 0 {
			t.Fatalf("shareOwed = %v %v (calls %v), want nothing filed", filed, err, sharer.calls)
		}
		if r.shareSkipped != ShareSkippedContributionsOff || r.params.Contribute != ContributeLocal {
			t.Fatalf("share skipped %q (contribute %q), want contributions_off, local", r.shareSkipped, r.params.Contribute)
		}
		if got := book(t, db, b.ID); got.ContribMode != store.ContribModeLocal {
			t.Fatalf("book mode %q, want local", got.ContribMode)
		}
	})
}

// failingCancelEngine is the engine with its guarded cancel failing while
// fails is above zero (each call takes one).
type failingCancelEngine struct {
	*scheduler.Scheduler
	fails *atomic.Int32
}

func (e failingCancelEngine) CancelUnlessFollowed(ctx context.Context, id int64, g store.StopGuard) error {
	if e.fails.Add(-1) >= 0 {
		return errors.New("database is locked")
	}
	return e.Scheduler.CancelUnlessFollowed(ctx, id, g)
}

// A job's end whose cancel of its engine book fails tries it again (allowed:
// a passing blip still stops the book); one that keeps failing turns the book
// local with no_share_job (denied: a book running on with no job following it
// never files in issue mode).
func TestJobEndThatCantStopItsBookKeepsItLocal(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		fails     int32
		cancelled bool
	}{{"a blip", 1, true}, {"failing", 100, false}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db, b := runningMappedBook(t)
			fails := &atomic.Int32{}
			fails.Store(c.fails)
			sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
			h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond, Log: slog.New(slog.DiscardHandler),
				Engine: func() Engine { return failingCancelEngine{sched, fails} }})
			if !h.release(ctx, jobs.Job{ID: 1, Kind: Kind, Attempt: 1}, b.ID, true) {
				t.Fatal("release didn't record the job's end")
			}
			got, err := db.GetBook(ctx, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scheduler.IsCancelled(got) != c.cancelled {
				t.Fatalf("cancelled = %v, want %v (book %+v)", scheduler.IsCancelled(got), c.cancelled, got)
			}
			wantMode, wantWhy := store.ContribModeIssue, ""
			if !c.cancelled {
				wantMode, wantWhy = store.ContribModeLocal, store.ShareSkippedNoShareJob
			}
			if got.ContribMode != wantMode || got.ShareSkipped != wantWhy {
				t.Fatalf("mode %q why %q, want %q %q", got.ContribMode, got.ShareSkipped, wantMode, wantWhy)
			}
		})
	}
}

// A job whose earlier run's mapping can't be read tries again (allowed: a
// passing blip resumes it); one that still can't is released as Abandon does,
// once Abandon's own read (the same store read) works (denied: the engine book
// an earlier run made never goes on with no job to follow or stop it). A store
// that can't read it at all leaves the book to its contributing stage, which
// keeps it local (ShareOff: no_share_job).
func TestResumeMappingReadErrorStopsTheEarlierBook(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		fails   int32
		stopped bool
	}{{"a blip", 1, false}, {"outlasting the tries", stopTries, true}, {"down", 100, false}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db, b := runningMappedBook(t)
			sched := scheduler.New(db, events.NewHub(8), scheduler.NewStubExecutor(0, 0), 1, "", false)
			h := New(Options{DB: db, RetentionDays: 30, Poll: time.Millisecond, Log: slog.New(slog.DiscardHandler),
				Engine: func() Engine { return sched }})
			var fails atomic.Int32
			fails.Store(c.fails)
			h.readMapping = func(ctx context.Context, id int64) (store.GenerateBook, error) {
				if fails.Add(-1) >= 0 {
					return store.GenerateBook{}, errors.New("database is locked")
				}
				return db.GetGenerateBook(ctx, id)
			}
			lib := int64(1)
			mapped, resumed, err := h.resumeMapping(ctx, jobs.Job{ID: 1, Kind: Kind, Attempt: 1, LibraryID: &lib})
			got, gerr := db.GetBook(ctx, b.ID)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if c.fails == 1 {
				if err != nil || !resumed || mapped.BookID == nil || *mapped.BookID != b.ID || scheduler.IsCancelled(got) {
					t.Fatalf("a blip = %+v %v %v, cancelled %v; want the mapping resumed", mapped, resumed, err,
						scheduler.IsCancelled(got))
				}
				return
			}
			if err == nil || scheduler.IsCancelled(got) != c.stopped {
				t.Fatalf("%s = %v, cancelled %v; want the error, cancelled %v", c.name, err, scheduler.IsCancelled(got), c.stopped)
			}
		})
	}
}

// ShareOff answers true when any share job following the book is off, even
// when one asked before it couldn't be (the book is local whatever the others
// say); with none off, a job that couldn't be asked is the error, so the stage
// waits (denied: never "not off").
func TestShareOffAnyJobOffWinsOverOneUnasked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	b, err := db.CreateBook(ctx, store.NewBook{Title: "B", SourcePath: "/src/b", WorkDir: t.TempDir(),
		ContribMode: store.ContribModeIssue})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := db.RecordClaim(ctx, store.ServerJob{ID: id, Kind: Kind, Params: json.RawMessage(`{"contribute":"issue"}`),
			Attempt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: id, BookID: &b.ID, LibraryID: 1, RelPath: "B"}); err != nil {
			t.Fatal(err)
		}
	}
	h := New(Options{DB: db, WorkRoot: t.TempDir()})
	unasked := map[int64]error{1: jobs.ErrShareNotRunning}
	h.AskThrough(fakeAsker{off: map[int64]bool{2: true}, errs: unasked})
	if why, err := h.ShareOff(ctx, b.ID, true); why != ShareSkippedContributionsOff || err != nil {
		t.Fatalf("one job off after one that couldn't be asked = %q %v, want contributions_off", why, err)
	}
	h.AskThrough(fakeAsker{errs: unasked})
	if why, err := h.ShareOff(ctx, b.ID, true); why != "" || !errors.Is(err, jobs.ErrShareNotRunning) {
		t.Fatalf("none off, one couldn't be asked = %q %v, want ErrShareNotRunning", why, err)
	}
}

// A book parked because the server couldn't be asked about its share is a
// share park: a share_off lifts it into local mode like a sign-in's (allowed),
// while a park that isn't over the share is left alone (denied).
func TestShareUnconfirmedIsASharePark(t *testing.T) {
	t.Parallel()
	na := string(state.StatusNeedsAttention)
	if !state.IsParkedWith(na, string(state.ParkShareUnconfirmed), shareParks...) {
		t.Error("share_unconfirmed isn't a share park")
	}
	if state.IsParkedWith(na, string(state.ParkAgentValidationExhausted), shareParks...) {
		t.Error("agent_validation_exhausted counts as a share park")
	}
}

// A share job never runs on a claim the store couldn't record (its share_off
// lives on the row): contribute issue, or params that don't read, need the
// row; a local job doesn't.
func TestNeedsClaimRowForShareJobs(t *testing.T) {
	t.Parallel()
	h := New(Options{WorkRoot: t.TempDir()})
	for params, want := range map[string]bool{
		`{"contribute":"issue"}`: true,
		`{"contribute":"bogus"}`: true,
		`{"contribute":"local"}`: false,
		`{}`:                     false,
	} {
		if got := h.NeedsClaimRow(jobs.Job{Kind: Kind, Params: json.RawMessage(params)}); got != want {
			t.Errorf("NeedsClaimRow(%s) = %v, want %v", params, got, want)
		}
	}
}

// An owed share's check before each dimension it files (stoppedBy) stops it
// on stands answering false, and parks it share_unconfirmed, as the
// contributing stage parks a book, when the store can't say whether it stands
// (denied: nothing more is filed either way); one that stands goes on
// (allowed).
func TestStoppedByParksAShareItCantConfirm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	answers := []error{nil, errors.New("database is locked")}
	stands := func(context.Context) (bool, error) {
		err := answers[0]
		answers = answers[1:]
		return true, err
	}
	stopped := stoppedBy(ctx, &run{rep: jobs.LocalReporter()}, stands)
	if stop, err := stopped(); stop || err != nil {
		t.Fatalf("a share that stands: %v %v, want it to go on", stop, err)
	}
	_, err := stopped()
	var park *scheduler.ParkError
	if !errors.As(err, &park) || park.Code != state.ParkShareUnconfirmed || park.Reason != pipeline.ShareUnreadMsg ||
		park.RetryAfter.IsZero() {
		t.Fatalf("stands unread: %v, want a share_unconfirmed park that looks again by itself", err)
	}
	off := stoppedBy(ctx, &run{rep: jobs.LocalReporter()}, func(context.Context) (bool, error) { return false, nil })
	if stop, err := off(); !stop || err != nil {
		t.Fatalf("stands false: %v %v, want the share stopped", stop, err)
	}
}

func TestRecoverInterruptedTranscriptReplacement(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := New(Options{DB: db, WorkRoot: root})
	old := store.NewBook{SourcePath: "/Book", WorkDir: filepath.Join(root, "old"), Title: "Book", Kind: "audio", State: "done"}
	b, err := db.CreateBook(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.BatchID, next.WorkDir = "replacement", filepath.Join(root, "new")
	if err := db.EnsureBatch(ctx, next.BatchID, time.Now()); err != nil {
		t.Fatal(err)
	}
	key := store.TranscriptCleanupPrefix + next.BatchID
	mustWrite(t, filepath.Join(old.WorkDir, "raw"), "original")
	stage := func() {
		t.Helper()
		if err := db.SetSetting(ctx, key, next.WorkDir); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(next.WorkDir, "raw"), "staged")
	}
	recoverAndCheck := func(kept, removed string) {
		t.Helper()
		if err := h.RecoverTranscriptDirs(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(kept, "raw")); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(removed); !os.IsNotExist(err) {
			t.Fatalf("disposable directory %s: %v", removed, err)
		}
	}
	stage()
	recoverAndCheck(old.WorkDir, next.WorkDir)
	stage()
	if _, err := db.ReplaceMappedBook(ctx, &b, next, store.GenerateBook{ServerJobID: 1, LibraryID: 1, RelPath: "Book"}); err != nil {
		t.Fatal(err)
	}
	recoverAndCheck(next.WorkDir, old.WorkDir)
	outside := t.TempDir()
	for _, dir := range []string{next.WorkDir, outside} {
		if err := db.SetSetting(ctx, key, dir); err != nil {
			t.Fatal(err)
		}
		err := h.RecoverTranscriptDirs(ctx)
		if (dir == outside) != (err != nil) {
			t.Fatalf("cleanup of %s: %v", dir, err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatal(err)
		}
	}
}

// A relative --data makes a relative work root: the cleanup still removes the
// folder recorded inside it.
func TestTranscriptCleanupWithARelativeWorkRoot(t *testing.T) {
	ctx := t.Context()
	t.Chdir(t.TempDir())
	db, err := store.Open(ctx, "bridge.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := New(Options{DB: db, WorkRoot: "work"})
	dir := filepath.Join("work", "batch-staged")
	mustWrite(t, filepath.Join(dir, "raw"), "staged")
	key := store.TranscriptCleanupPrefix + "batch-staged"
	if err := db.SetSetting(ctx, key, dir); err != nil {
		t.Fatal(err)
	}
	if err := h.RecoverTranscriptDirs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("staged folder: %v", err)
	}
	if _, found, err := db.GetSetting(ctx, key); err != nil || found {
		t.Fatalf("cleanup key kept: %v %v", found, err)
	}
}

// An earlier run the job can't reuse is no reason to fail it: one whose folder
// lies outside the work root (a data folder moved since), or one gone while the
// job waited for the engine's hold, leaves create to clear it and start over.
func TestKeptTranscriptNotReusableFallsThrough(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := New(Options{DB: db, WorkRoot: filepath.Join(t.TempDir(), "work"), RetentionDays: 30})
	r := &run{rep: jobs.LocalReporter()}
	outside, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/Moved", WorkDir: filepath.Join(t.TempDir(), "old-data", "work", "x"),
		Title: "Moved", Kind: "audio", State: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fromKeptTranscript(ctx, nil, r, outside, store.NewBook{}); ok || err != nil {
		t.Fatalf("outside the work root: %v %v", ok, err)
	}
	gone, err := db.CreateBook(ctx, store.NewBook{SourcePath: "/Gone", WorkDir: filepath.Join(h.o.WorkRoot, "gone"),
		Title: "Gone", Kind: "audio", State: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteBook(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.fromKeptTranscript(ctx, nil, r, gone, store.NewBook{}); ok || err != nil {
		t.Fatalf("gone meanwhile: %v %v", ok, err)
	}
}
