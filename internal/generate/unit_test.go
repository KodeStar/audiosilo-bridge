package generate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/events"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
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
	for _, code := range []state.ParkCode{state.ParkAgentUnavailable, state.ParkAgentRateLimited, state.ParkAgentCredentialRefused,
		state.ParkAgentValidationExhausted,
		state.ParkMarkersNotConfident, state.ParkChaptersNotDetected, state.ParkQANoConverge, state.ParkSpellingGateFailure, state.ParkMediaToolsUnavailable,
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
		_ = db.EndGenerateBook(ctx, job, time.Now().Add(-time.Hour))
		return b
	}
	own := mk(1, "own", "done")
	other := mk(2, "other", "done")
	working := mk(3, "working", "fact_pass")
	if err := h.purgeEnded(ctx, own.ID); err != nil || present(own.WorkDir) {
		t.Fatalf("the job's own book: %v, present %v", err, present(own.WorkDir))
	}
	if err := h.purgeEnded(ctx, working.ID); err != nil || !present(working.WorkDir) {
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
	_ = db.EndGenerateBook(ctx, 1, time.Now().Add(-time.Hour))

	eng.held[b.ID] = true
	if n, err := h.SweepTranscripts(ctx, 0); err != nil || n != 0 || !present(wd) {
		t.Fatalf("a held book: swept %d, %v, present %v", n, err, present(wd))
	}
	if err := h.purgeEnded(ctx, b.ID); err != nil || !present(wd) {
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
	_ = db.EndGenerateBook(ctx, 1, time.Now().Add(-time.Hour))

	readmitted = true
	if err := h.purgeEnded(ctx, b.ID); err != nil || !present(wd) {
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
	if err := h.purgeEnded(ctx, b.ID); err != nil || !present(wd) {
		t.Fatalf("a book its job follows again lost its transcripts: %v", err)
	}
	_ = db.EndGenerateBook(ctx, 1, time.Now().Add(-time.Hour))
	if n, err := h.SweepTranscripts(ctx, 0); err != nil || n != 1 || present(wd) {
		t.Fatalf("a finished book: swept %d, %v, present %v", n, err, present(wd))
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
// each Retry reads it: the job's end racing a person's Retry of the parked job.
type cancelFirstEngine struct{ *scheduler.Scheduler }

func (e cancelFirstEngine) Retry(ctx context.Context, id int64) error {
	_ = e.Cancel(ctx, id)
	return e.Scheduler.Retry(ctx, id)
}

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
	deadline := time.Now().Add(10 * time.Second)
	for got, _ = db.GetBook(ctx, b.ID); !scheduler.IsCancelled(got); got, _ = db.GetBook(ctx, b.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("the book runs with no job once the engine is up: status %q", got.Status)
		}
		time.Sleep(time.Millisecond)
	}
	// Each case below is a job whose end Abandon's release recorded.
	abandoned := func(job int64, slug string) store.Book {
		b := newBook(job, slug)
		if err := db.EndGenerateBook(ctx, job, time.Now()); err != nil {
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
