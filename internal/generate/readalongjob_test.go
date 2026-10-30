package generate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/readalong"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// epubBytes is a minimal EPUB (one section); drm adds META-INF/rights.xml.
func epubBytes(t *testing.T, text string, drm bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	put := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	put("mimetype", "application/epub+zip")
	put("META-INF/container.xml", `<?xml version="1.0"?><container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="content.opf"/></rootfiles></container>`)
	put("content.opf", `<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="a"/></spine></package>`)
	put("a.xhtml", `<html xmlns="http://www.w3.org/1999/xhtml"><body><p>`+text+`</p></body></html>`)
	if drm {
		put("META-INF/rights.xml", "<rights/>")
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeAlignedMap is readalong_aligning in the test engine: a map of the request's
// ebook (its sha256 now) and files, at the coverage asked (0: 0.95), and the
// stage's report.
func writeAlignedMap(t *testing.T, b store.Book, coverage float64) {
	if coverage == 0 {
		coverage = 0.95
	}
	var req pipeline.ReadalongRequest
	raw, err := os.ReadFile(filepath.Join(b.WorkDir, readalong.RequestPath))
	if err != nil || json.Unmarshal(raw, &req) != nil {
		t.Errorf("no read-along request in the work dir: %v", err)
		return
	}
	ebook, err := os.ReadFile(req.Ebook)
	if err != nil {
		t.Errorf("the request's ebook: %v", err)
		return
	}
	sum := sha256.Sum256(ebook)
	m := readalong.Map{Format: readalong.Format, Ebook: readalong.MapEbook{File: filepath.Base(req.Ebook), SHA256: hex.EncodeToString(sum[:])},
		Granularity: "sentence", Generator: readalong.MapGenerator{Name: "audiosilo-bridge", Version: req.Version, Aligner: readalong.AlignerName, ASR: "fake"},
		GeneratedAt: "2026-10-29T12:00:00Z", Coverage: readalong.Coverage{Text: coverage, Audio: 0.9},
		Sections:  []readalong.MapSection{{Href: "a.xhtml", Spans: []readalong.MapSpan{{CFI: "epubcfi(/6/2!/4/2,/1:0,/1:5)", Start: 1, End: 2}}}},
		AudioGaps: []readalong.TimeRange{}}
	for _, f := range req.Files {
		m.Audio.Files = append(m.Audio.Files, readalong.MapFile{Path: f.Path, Duration: f.Duration})
		m.Audio.Duration += f.Duration
	}
	out, err := readalong.Encode(m)
	if err != nil {
		t.Error(err)
		return
	}
	_ = os.MkdirAll(filepath.Join(b.WorkDir, readalong.WorkDir), 0o750)
	_ = os.WriteFile(filepath.Join(b.WorkDir, readalong.MapPath), out, 0o600)
	_ = os.WriteFile(filepath.Join(b.WorkDir, readalong.ReportPath), []byte(`{"heal_windows":2}`), 0o600)
}

// readalongHarness is the harness with an ebook in the folder book and the
// single-file book, both named by the server, and the files' lengths.
func readalongHarness(t *testing.T) *harness {
	t.Helper()
	hr := newHarness(t)
	mustWrite(t, filepath.Join(hr.root, "Author", "Book", "Book.epub"), string(epubBytes(t, "Hello there.", false)))
	mustWrite(t, filepath.Join(hr.root, "Loose.epub"), string(epubBytes(t, "Loose words.", false)))
	hr.srv.set(func() {
		b := hr.srv.books["Author/Book"]
		b.Files[0].Duration, b.Files[1].Duration = 3600.5, 1800.25
		b.Info.Ebook = &link.Ebook{Path: "Author/Book/Book.epub", Size: 100}
		hr.srv.books["Author/Book"] = b
		l := hr.srv.books["Loose.m4b"]
		l.Files[0].Duration = 600
		l.Info.Ebook = &link.Ebook{Path: "Loose.epub", Size: 100}
		hr.srv.books["Loose.m4b"] = l
	})
	return hr
}

func (hr *harness) readalongResult(id int64) ReadalongResult {
	hr.t.Helper()
	_, final := hr.srv.final(id)
	var res ReadalongResult
	if err := json.Unmarshal(final.Result, &res); err != nil {
		hr.t.Fatalf("result = %s: %v", final.Result, err)
	}
	return res
}

func (hr *harness) failedWith(id int64, want string) {
	hr.t.Helper()
	hr.waitFor("the job failed", func() bool { s, _ := hr.srv.final(id); return s == link.StateFailed })
	if _, final := hr.srv.final(id); final.Error == nil || !strings.Contains(*final.Error, want) {
		hr.t.Fatalf("error = %v, want %q", final.Error, want)
	}
}

func (hr *harness) stagesRun(bookID int64) []string {
	hr.t.Helper()
	runs, err := hr.db.ListStageRuns(context.Background(), bookID)
	if err != nil {
		hr.t.Fatal(err)
	}
	var out []string
	for _, r := range runs {
		out = append(out, r.Stage)
	}
	return out
}

// A folder book with no earlier run: a new engine book of goal readalong (no
// series, no agent stage, no spend reported), the request with the server's
// files relative to the book, the map written as .audiosilo/readalong.json (and
// nothing else), the server asked to read it, the result, and the job's end
// recorded for the retention.
func TestReadalongFolderBook(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	hr.srv.addKind(201, ReadalongKind, "Author/Book", `{}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(201); return s == link.StateDone })
	b := hr.engineBook(201)
	if b.Goal != state.GoalReadalong || b.Series != "" || b.Kind != string(state.KindAudio) {
		t.Fatalf("engine book = %+v", b)
	}
	for _, st := range hr.stagesRun(b.ID) {
		if state.IsAgent(state.State(st)) {
			t.Fatalf("an agent stage ran: %s", st)
		}
	}
	var req pipeline.ReadalongRequest
	raw, _ := os.ReadFile(filepath.Join(b.WorkDir, readalong.RequestPath))
	if err := json.Unmarshal(raw, &req); err != nil || len(req.Files) != 2 || req.Files[0].Path != "01.mp3" || req.Files[1].Duration != 1800.25 ||
		!strings.HasSuffix(req.Ebook, filepath.Join("Author", "Book", "Book.epub")) || req.Version != "0.2.0" {
		t.Fatalf("request = %s, %v", raw, err)
	}
	adir := filepath.Join(hr.root, "Author", "Book", AttachDir)
	entries, _ := os.ReadDir(adir)
	if len(entries) != 1 || entries[0].Name() != readalongName {
		t.Fatalf(".audiosilo/ holds %v, want readalong.json only (bridge.json untouched)", entries)
	}
	if info, err := os.Stat(filepath.Join(adir, readalongName)); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("readalong.json: %v %v", info, err)
	}
	res := hr.readalongResult(201)
	if res.File != "readalong.json" || res.Coverage.Text != 0.95 || res.Spans != 1 || res.Sections != 1 || res.HealWindows != 2 ||
		!res.Transcribed || len(res.EbookSHA256) != 64 || res.Path != "Author/Book" {
		t.Fatalf("result = %+v", res)
	}
	if hr.srv.saw(201, func(_ link.Update, p map[string]any) bool { _, ok := p["cost_usd"]; return ok }) {
		t.Fatal("a read-along reported an agent spend")
	}
	hr.srv.mu.Lock()
	rescans := slices.Clone(hr.srv.rescans)
	hr.srv.mu.Unlock()
	if len(rescans) != 1 || rescans[0]["path"] != "Author/Book" {
		t.Fatalf("rescans = %+v", rescans)
	}
	if g, _ := hr.db.GetGenerateBook(context.Background(), 201); g.EndedAt == "" {
		t.Fatal("the job's end wasn't recorded (the retention counts from it)")
	}
}

// A single-file book's map is <stem>.readalong.json, its request names the file.
func TestReadalongSingleFileBook(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	hr.srv.addKind(202, ReadalongKind, "Loose.m4b", `{}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(202); return s == link.StateDone })
	if _, err := os.Stat(filepath.Join(hr.root, AttachDir, "Loose.readalong.json")); err != nil {
		t.Fatal(err)
	}
	if res := hr.readalongResult(202); res.File != "Loose.readalong.json" {
		t.Fatalf("result = %+v", res)
	}
	var req pipeline.ReadalongRequest
	raw, _ := os.ReadFile(filepath.Join(hr.engineBook(202).WorkDir, readalong.RequestPath))
	if err := json.Unmarshal(raw, &req); err != nil || len(req.Files) != 1 || req.Files[0].Path != "Loose.m4b" {
		t.Fatalf("request = %s", raw)
	}
}

// Refused before the engine gets a book: no ebook, a DRM-protected one, one that
// isn't an EPUB.
func TestReadalongRefusesTheEbook(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.srv.set(func() { l := hr.srv.books["Loose.m4b"]; l.Info.Ebook = nil; hr.srv.books["Loose.m4b"] = l })
	hr.srv.addKind(203, ReadalongKind, "Loose.m4b", `{}`)
	hr.failedWith(203, "this book has no ebook")
	mustWrite(t, filepath.Join(hr.root, "Author", "Book", "Book.epub"), string(epubBytes(t, "x", true)))
	hr.srv.addKind(204, ReadalongKind, "Author/Book", `{}`)
	hr.failedWith(204, readalong.ErrDRM.Error())
	mustWrite(t, filepath.Join(hr.root, "Author", "Book", "Book.epub"), "not a zip")
	hr.srv.addKind(205, ReadalongKind, "Author/Book", `{}`)
	hr.failedWith(205, readalong.ErrNotEPUB.Error())
	for _, id := range []int64{203, 204, 205} {
		if _, err := hr.db.GetGenerateBook(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("job %d got an engine book", id)
		}
	}
}

// A poor match (another edition) fails the job with its coverage and writes
// nothing.
func TestReadalongPoorMatch(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	hr.exec.alignCoverage = 0.31
	hr.srv.addKind(206, ReadalongKind, "Author/Book", `{}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.failedWith(206, "the ebook and the narration don't match: 31% lined up")
	if _, err := os.Stat(filepath.Join(hr.root, "Author", "Book", AttachDir)); err == nil {
		t.Fatal("a poor match wrote into the book")
	}
}

// An ebook replaced while the book was aligned fails the job (run it again) and
// writes nothing.
func TestReadalongEbookChangedMidRun(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	hr.exec.afterAlign = func(store.Book) {
		mustWrite(t, filepath.Join(hr.root, "Author", "Book", "Book.epub"), string(epubBytes(t, "Another edition.", false)))
	}
	hr.srv.addKind(207, ReadalongKind, "Author/Book", `{}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.failedWith(207, "the ebook changed while aligning; run it again")
	if _, err := os.Stat(filepath.Join(hr.root, "Author", "Book", AttachDir, readalongName)); err == nil {
		t.Fatal("a map for another ebook was written")
	}
}

// A book folder that can't be written parks the job book_not_writable at the
// write; a Retry once it can writes the map.
func TestReadalongBookNotWritable(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes through 0555")
	}
	hr := readalongHarness(t)
	book := filepath.Join(hr.root, "Author", "Book")
	hr.exec.afterAlign = func(store.Book) { _ = os.Chmod(book, 0o555) } //nolint:gosec // the test's own folder
	t.Cleanup(func() { _ = os.Chmod(book, 0o755) })                     //nolint:gosec // as above
	hr.srv.addKind(208, ReadalongKind, "Author/Book", `{}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.waitFor("the park", func() bool {
		return hr.srv.saw(208, func(u link.Update, p map[string]any) bool {
			park, _ := p["park"].(map[string]any)
			return u.State != nil && *u.State == link.StateNeedsAttention && park["code"] == "book_not_writable"
		})
	})
	if err := os.Chmod(book, 0o755); err != nil { //nolint:gosec // as above
		t.Fatal(err)
	}
	if err := hr.h.Retry(context.Background(), 208); err != nil {
		t.Fatal(err)
	}
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(208); return s == link.StateDone })
	if _, err := os.Stat(filepath.Join(book, AttachDir, readalongName)); err != nil {
		t.Fatal(err)
	}
}

// A book the server moved while it was aligned gets its map where it is now.
func TestReadalongFollowsAMovedBook(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	gate := hr.exec.gate(string(state.ReadalongAligning))
	hr.srv.addKind(209, ReadalongKind, "Author/Book", `{}`)
	h := hr.handler()
	h.o.Recheck = 20 * time.Millisecond
	stop := hr.runner(h, h.ReadalongJob())
	defer stop()
	hr.waitFor("aligning", func() bool {
		return hr.srv.saw(209, func(_ link.Update, p map[string]any) bool { return p["stage"] == string(state.ReadalongAligning) })
	})
	if err := os.Rename(filepath.Join(hr.root, "Author", "Book"), filepath.Join(hr.root, "Author", "Book (2026)")); err != nil {
		t.Fatal(err)
	}
	hr.srv.set(func() {
		hr.srv.books["Author/Book (2026)"] = link.Book{LibraryID: 1, Path: "Author/Book (2026)", Files: []link.BookFile{
			{Path: "Author/Book (2026)/01.mp3", Size: 9, Duration: 3600.5}, {Path: "Author/Book (2026)/02.mp3", Size: 10, Duration: 1800.25}},
			Info: &link.BookInfo{Title: "The Book", Ebook: &link.Ebook{Path: "Author/Book (2026)/Book.epub", Size: 100}}}
		delete(hr.srv.books, "Author/Book")
		hr.srv.paths = map[int64]string{209: "Author/Book (2026)"}
	})
	// The job hands the stage the ebook where it is now before the stage reads it.
	work := hr.engineBook(209).WorkDir
	hr.waitFor("the request moved with the book", func() bool {
		raw, _ := os.ReadFile(filepath.Join(work, readalong.RequestPath))
		return strings.Contains(string(raw), "Book (2026)")
	})
	close(gate)
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(209); return s == link.StateDone })
	if _, err := os.Stat(filepath.Join(hr.root, "Author", "Book (2026)", AttachDir, readalongName)); err != nil {
		t.Fatal(err)
	}
	if res := hr.readalongResult(209); res.Path != "Author/Book (2026)" {
		t.Fatalf("result = %+v", res)
	}
}

// The reuse table (decision 6), row by row.
func TestReadalongReuseTable(t *testing.T) {
	t.Parallel()
	failed, parked := string(state.StatusFailed), string(state.StatusNeedsAttention)
	for _, tc := range []struct {
		name string
		e    readalongEarlier
		to   state.State
		goal string
		ok   bool
	}{
		{"whole run done", readalongEarlier{State: "done", Current: true}, state.ReadalongAligning, "", true},
		{"whole run parked at spelling", readalongEarlier{State: "spelling_research", Status: parked, Current: true}, state.ReadalongAligning, "", true},
		{"whole run failed at contributing", readalongEarlier{State: "contributing", Status: failed, Current: true}, state.ReadalongAligning, "", true},
		{"read-along done", readalongEarlier{Goal: "readalong", State: "done", Current: true}, state.ReadalongAligning, "readalong", true},
		{"read-along failed aligning", readalongEarlier{Goal: "readalong", State: "readalong_aligning", Status: failed, Current: true}, state.ReadalongAligning, "readalong", true},
		{"chapters done", readalongEarlier{Goal: "chapters", State: "done", Current: true}, state.Sanitizing, "readalong", true},
		{"whole run cancelled at qa_adjudicating", readalongEarlier{State: "qa_adjudicating", Status: failed, Current: true}, state.Sanitizing, "readalong", true},
		{"read-along failed at qa_sweep", readalongEarlier{Goal: "readalong", State: "qa_sweep", Status: failed, Current: true}, state.Sanitizing, "readalong", true},
		{"stopped at asr, continuable", readalongEarlier{Goal: "chapters", State: "asr", Status: failed, Continuable: true}, state.Splitting, "readalong", true},
		{"whole run done, swept", readalongEarlier{State: "done", Swept: true, Current: true}, state.Inspecting, "readalong", true},
		{"whole run done, audio changed", readalongEarlier{State: "done"}, state.Inspecting, "readalong", true},
		{"whole run a read-along stopped aligning", readalongEarlier{State: "readalong_aligning", Status: failed, Current: true}, state.ReadalongAligning, "", true},
		{"whole run stopped aligning, swept", readalongEarlier{State: "readalong_aligning", Status: failed, Swept: true, Current: true}, state.Inspecting, "readalong", true},
		{"read-along from a whole run, failed at splitting", readalongEarlier{Goal: "readalong", State: "splitting", Status: failed, Current: true, FactPass: true}, state.Inspecting, "readalong", true},
		{"read-along from a whole run, done, swept", readalongEarlier{Goal: "readalong", State: "done", Swept: true, Current: true, FactPass: true}, state.Inspecting, "readalong", true},
		// Denied rows: a new book.
		{"stopped at asr, not continuable", readalongEarlier{State: "asr", Status: failed, Current: true}, "", "", false},
		{"read-along done, swept", readalongEarlier{Goal: "readalong", State: "done", Swept: true, Current: true}, "", "", false},
		{"chapters done, audio changed", readalongEarlier{Goal: "chapters", State: "done"}, "", "", false},
		{"whole run failed at inspecting", readalongEarlier{State: "inspecting", Status: failed, Current: true}, "", "", false},
		{"a stage may run", readalongEarlier{State: "spelling_research", Current: true}, "", "", false},
	} {
		to, goal, ok := readalongReuse(tc.e)
		if to != tc.to || goal != tc.goal || ok != tc.ok {
			t.Errorf("%s: (%q, %q, %v), want (%q, %q, %v)", tc.name, to, goal, ok, tc.to, tc.goal, tc.ok)
		}
	}
}

// seedEarlier puts an earlier engine book of the folder book in the engine, as a
// run of goal at st (status) left it: the files manifest and probe of the audio
// now, transcripts, a sentinel per stage before st.
func seedEarlier(hr *harness, goal string, st state.State, status string) store.Book {
	hr.t.Helper()
	return seedEarlierAt(hr, bookSource(hr), goal, st, status)
}

// bookSource is the folder book's engine source path.
func bookSource(hr *harness) string {
	hr.t.Helper()
	sp, ok, err := metaops.AllowedPath(filepath.Join(hr.root, "Author", "Book"), []string{hr.root})
	if err != nil || !ok {
		hr.t.Fatal(err)
	}
	return sp
}

// seedEarlierAt is seedEarlier's run at source path key.
func seedEarlierAt(hr *harness, key, goal string, st state.State, status string) store.Book {
	hr.t.Helper()
	ctx := context.Background()
	b, err := hr.db.CreateBook(ctx, store.NewBook{SourcePath: key, WorkDir: store.DeriveWorkDir(hr.work, key, "The Book"), Title: "The Book",
		Goal: goal, Kind: string(state.KindAudio), State: string(state.Done)}) // never queued: the engine would run it
	if err != nil {
		hr.t.Fatal(err)
	}
	if err := hr.db.SetBookState(ctx, b.ID, string(st), status, "", ""); err != nil {
		hr.t.Fatal(err)
	}
	inspected(hr, b.WorkDir)
	mustWrite(hr.t, filepath.Join(b.WorkDir, "transcripts-json", "ch001.json"), "{}")
	mustWrite(hr.t, filepath.Join(b.WorkDir, "asr.json"), `{"manifest_sha":"the-earlier-manifest"}`)
	for _, s := range readalongPath {
		if state.Order(s) < state.Order(st) || st == state.Done {
			if err := scheduler.WriteSentinel(b.WorkDir, string(s), scheduler.StageResult{QAClean: true}); err != nil {
				hr.t.Fatal(err)
			}
		}
	}
	got, _ := hr.db.GetBook(ctx, b.ID)
	return got
}

// The table's rows reach the engine: a finished whole run is aligned on its kept
// transcript (no speech recognition, its goal kept, never cleared); a finished
// read-along is aligned again; a done chapters run goes on from sanitizing; a
// swept whole run is transcribed again in the same engine book; a run the table
// doesn't reuse is cleared for a new one.
func TestReadalongReusesTheEarlierRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		goal        string
		st          state.State
		status      string
		swept       bool
		sameBook    bool
		firstStage  state.State
		goalAfter   string
		transcribed bool
	}{
		{"whole run done", "", state.Done, "", false, true, state.ReadalongAligning, "", false},
		{"whole run stopped aligning", "", state.ReadalongAligning, string(state.StatusFailed), false, true, state.ReadalongAligning, "", false},
		{"read-along done", state.GoalReadalong, state.Done, "", false, true, state.ReadalongAligning, state.GoalReadalong, false},
		{"chapters done", state.GoalChapters, state.Done, "", false, true, state.Sanitizing, state.GoalReadalong, false},
		{"whole run swept", "", state.Done, "", true, true, state.Inspecting, state.GoalReadalong, true},
		{"read-along failed at inspecting", state.GoalReadalong, state.Inspecting, string(state.StatusFailed), false, false, state.Inspecting, state.GoalReadalong, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hr := readalongHarness(t)
			old := seedEarlier(hr, tc.goal, tc.st, tc.status)
			if tc.swept {
				if err := hr.db.PutGenerateBook(context.Background(), store.GenerateBook{ServerJobID: 99, BookID: &old.ID, LibraryID: 1, RelPath: "Author/Book"}); err != nil {
					t.Fatal(err)
				}
				if err := hr.db.EndGenerateBook(context.Background(), 99, time.Now()); err != nil {
					t.Fatal(err)
				}
				if err := hr.db.MarkGenerateBookSwept(context.Background(), old.ID, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			before := len(hr.stagesRun(old.ID))
			hr.srv.addKind(210, ReadalongKind, "Author/Book", `{}`)
			stop := hr.runner(hr.h, hr.h.ReadalongJob())
			defer stop()
			hr.waitFor("done", func() bool { s, _ := hr.srv.final(210); return s == link.StateDone })
			b := hr.engineBook(210)
			if (b.ID == old.ID) != tc.sameBook || b.Goal != tc.goalAfter {
				t.Fatalf("engine book %d (earlier %d), goal %q", b.ID, old.ID, b.Goal)
			}
			stages := hr.stagesRun(b.ID)
			if b.ID == old.ID {
				stages = stages[before:]
			}
			if len(stages) == 0 || stages[0] != string(tc.firstStage) || stages[len(stages)-1] != string(state.ReadalongAligning) {
				t.Fatalf("stages run %v, want from %s to readalong_aligning", stages, tc.firstStage)
			}
			if res := hr.readalongResult(210); res.Transcribed != tc.transcribed {
				t.Fatalf("transcribed = %v", res.Transcribed)
			}
			if _, err := hr.db.GetBook(context.Background(), old.ID); tc.sameBook == errors.Is(err, store.ErrNotFound) {
				t.Fatalf("the earlier book: %v", err)
			}
			// Going on from inspecting forgets the manifest the old transcripts were
			// made from (asr would park manifest_changed); any other row keeps it.
			if _, err := os.Stat(filepath.Join(b.WorkDir, "asr.json")); tc.sameBook && (err == nil) == (tc.firstStage == state.Inspecting) {
				t.Fatalf("asr.json after going on from %s: %v", tc.firstStage, err)
			}
		})
	}
}

// A read-along that went on from a whole run keeps its notes for the next
// read-along after a Reconcile rewind superseded its fact pass's success: with its
// transcripts swept, the read-along goes on in the same engine book from
// inspecting, never clearing it for a new one.
func TestReadalongKeepsAWholeRunsNotesAfterARewind(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	ctx := context.Background()
	old := seedEarlier(hr, state.GoalReadalong, state.Done, "")
	id, err := hr.db.StartStageRun(ctx, old.ID, string(state.FactPass), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := hr.db.FinishStageRun(ctx, id, true, nil); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(old.WorkDir, "facts", "knowledge-final.md")
	mustWrite(t, notes, "## ROSTER\n")
	// What Reconcile does to the fact pass when it rewinds the book to an earlier
	// stage whose sentinel went missing.
	if err := hr.db.SupersedeStageSuccesses(ctx, old.ID, string(state.FactPass)); err != nil {
		t.Fatal(err)
	}
	if err := hr.db.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: 99, BookID: &old.ID, LibraryID: 1, RelPath: "Author/Book"}); err != nil {
		t.Fatal(err)
	}
	if err := hr.db.EndGenerateBook(ctx, 99, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := hr.db.MarkGenerateBookSwept(ctx, old.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	before := len(hr.stagesRun(old.ID))
	hr.srv.addKind(213, ReadalongKind, "Author/Book", `{}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(213); return s == link.StateDone })
	if b := hr.engineBook(213); b.ID != old.ID || b.Goal != state.GoalReadalong {
		t.Fatalf("engine book %d (earlier %d), goal %q", b.ID, old.ID, b.Goal)
	}
	if stages := hr.stagesRun(old.ID)[before:]; len(stages) == 0 || stages[0] != string(state.Inspecting) {
		t.Fatalf("stages run %v, want from inspecting", stages)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Fatalf("the whole run's notes: %v", err)
	}
}

// A book a generate job is working on refuses the read-along (it would share the
// engine book); the generate job goes on.
func TestReadalongRefusedWhileGenerating(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	gate := hr.exec.gate("asr")
	hr.srv.add(211, "Author/Book", `{"source":"audio"}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.waitFor("generating at asr", func() bool {
		return hr.srv.saw(211, func(_ link.Update, p map[string]any) bool { return p["stage"] == "asr" })
	})
	hr.srv.addKind(212, ReadalongKind, "Author/Book", `{}`)
	hr.failedWith(212, "A generate job is working on this book")
	close(gate)
	hr.waitFor("generated", func() bool { s, _ := hr.srv.final(211); return s == link.StateDone })
}

// A generate job continues a done read-along run at qa_sweep with goal "" (its
// repaired transcript reviewed, never transcribed again), the same engine book.
func TestGenerateContinuesAReadalongRun(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	old := seedEarlier(hr, state.GoalReadalong, state.Done, "")
	before := len(hr.stagesRun(old.ID))
	hr.srv.add(213, "Author/Book", `{"source":"audio"}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(213); return s == link.StateDone })
	b := hr.engineBook(213)
	if b.ID != old.ID || b.Goal != "" || b.Series != "The Saga" {
		t.Fatalf("engine book = %+v (earlier %d)", b, old.ID)
	}
	stages := hr.stagesRun(b.ID)[before:]
	if len(stages) == 0 || stages[0] != string(state.QASweep) || slices.Contains(stages, string(state.ASR)) {
		t.Fatalf("stages run %v, want from qa_sweep, no asr", stages)
	}
	if _, err := os.Stat(filepath.Join(hr.root, "Author", "Book", AttachDir, "characters.json")); err != nil {
		t.Fatal(err)
	}
}

// A read-along run transcribed in windows isn't continued by a generate job: the
// whole pipeline would write characters and recaps by window, its chapters never
// found. The book is transcribed again in a run of its own.
func TestGenerateTranscribesAWindowedReadalongRunAgain(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	old := seedEarlier(hr, state.GoalReadalong, state.Done, "")
	m, err := audio.ReadManifest(old.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	m.Style = audio.StyleWindows
	if err := audio.WriteManifest(old.WorkDir, m); err != nil {
		t.Fatal(err)
	}
	hr.srv.add(214, "Author/Book", `{"source":"audio"}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(214); return s == link.StateDone })
	b := hr.engineBook(214)
	if b.ID == old.ID || !slices.Contains(hr.stagesRun(b.ID), string(state.ASR)) {
		t.Fatalf("engine book %d (earlier %d) ran %v, want a new run with asr", b.ID, old.ID, hr.stagesRun(b.ID))
	}
}

// inspected leaves in workDir what inspect leaves for the folder book: its files
// manifest and probe.
func inspected(hr *harness, workDir string) {
	hr.t.Helper()
	full := filepath.Join(hr.root, "Author", "Book")
	m := audio.Manifest{Source: full, Style: audio.StyleFiles, Duration: 5400, ChapterCount: 2, Chapters: []audio.Chapter{
		{Chapter: 1, Start: 0, End: 3600, Duration: 3600, FilePath: filepath.Join(full, "01.mp3")},
		{Chapter: 2, Start: 3600, End: 5400, Duration: 1800, FilePath: filepath.Join(full, "02.mp3")}}}
	if err := audio.WriteManifest(workDir, m); err != nil {
		hr.t.Fatal(err)
	}
	mustWrite(hr.t, filepath.Join(workDir, audio.ProbeName), `{"format":{"duration":"5400"}}`)
}

// seedEbookRun puts a finished ebook run of the folder book in the engine: its
// own manifest (the ebook's chapters) and notes.
func seedEbookRun(hr *harness) store.Book {
	hr.t.Helper()
	ctx := context.Background()
	sp := bookSource(hr)
	b, err := hr.db.CreateBook(ctx, store.NewBook{SourcePath: sp, WorkDir: store.DeriveWorkDir(hr.work, sp, "The Book"), Title: "The Book",
		Kind: string(state.KindEbook), EbookPath: filepath.Join(sp, "Book.epub"), State: string(state.Done)})
	if err != nil {
		hr.t.Fatal(err)
	}
	mustWrite(hr.t, filepath.Join(b.WorkDir, audio.ManifestName), `{"style":"ebook"}`)
	mustWrite(hr.t, filepath.Join(b.WorkDir, "facts", "knowledge-final.md"), "## ROSTER\n")
	mustWrite(hr.t, filepath.Join(b.WorkDir, "sidecars", "characters.json"), `{"work":"the-book","characters":[]}`)
	return b
}

// ebookRunIntact fails the test unless the ebook run is as seedEbookRun left it.
func ebookRunIntact(hr *harness, e store.Book) {
	hr.t.Helper()
	got, err := hr.db.GetBook(context.Background(), e.ID)
	if err != nil || got.Kind != string(state.KindEbook) || got.EbookPath != e.EbookPath || got.SourcePath != e.SourcePath ||
		got.Goal != "" || got.State != string(state.Done) {
		hr.t.Fatalf("the ebook run = %+v, %v", got, err)
	}
	if raw, err := os.ReadFile(filepath.Join(e.WorkDir, audio.ManifestName)); err != nil || string(raw) != `{"style":"ebook"}` {
		hr.t.Fatalf("the ebook run's manifest = %s, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(e.WorkDir, "facts", "knowledge-final.md")); err != nil {
		hr.t.Fatalf("the ebook run's notes: %v", err)
	}
}

// A read-along never continues nor clears an ebook run (its notes, chapter
// manifest and kind are the description's): it runs beside it, in an audio run of
// its own, which the next read-along reuses. A description reads the ebook run
// still, and a generate job reading the ebook leaves the audio run alone.
func TestReadalongRunsBesideAnEbookRun(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	ctx := context.Background()
	ebook := seedEbookRun(hr)
	d := &fakeDescriber{db: hr.db}
	h := hr.describeHandler(d)
	stop := hr.runner(h, h.ReadalongJob(), h.DescribeJob())
	defer stop()

	hr.srv.addKind(230, ReadalongKind, "Author/Book", `{}`)
	hr.waitFor("the read-along", func() bool { s, _ := hr.srv.final(230); return s == link.StateDone })
	beside := hr.engineBook(230)
	if beside.ID == ebook.ID || beside.SourcePath != store.AudioRunKey(ebook.SourcePath) || beside.Kind != string(state.KindAudio) ||
		beside.Goal != state.GoalReadalong || beside.WorkDir == ebook.WorkDir {
		t.Fatalf("the read-along's run = %+v (ebook run %+v)", beside, ebook)
	}
	if stages := hr.stagesRun(beside.ID); len(stages) == 0 || stages[0] != string(state.Inspecting) {
		t.Fatalf("the read-along ran %v", stages)
	}
	if _, err := os.Stat(filepath.Join(hr.root, "Author", "Book", AttachDir, "readalong.json")); err != nil {
		t.Fatal(err)
	}
	ebookRunIntact(hr, ebook)
	inspected(hr, beside.WorkDir) // as the real inspect leaves it (the test executor doesn't)

	// The description reads the ebook run's notes, ebook-sourced.
	hr.srv.addDescribe(231, "Author/Book")
	hr.waitFor("the description", func() bool { s, _ := hr.srv.final(231); return s == link.StateDone || s == link.StateFailed })
	if s, final := hr.srv.final(231); s != link.StateDone {
		t.Fatalf("the description: %v", *final.Error)
	}
	var prov Provenance
	raw, _ := os.ReadFile(filepath.Join(hr.root, "Author", "Book", AttachDir, "bridge.json"))
	if err := json.Unmarshal(raw, &prov); err != nil || prov.Source != SourceEbook {
		t.Fatalf("bridge.json = %s, %v", raw, err)
	}

	// The next read-along reuses the audio run.
	hr.srv.addKind(232, ReadalongKind, "Author/Book", `{}`)
	hr.waitFor("the second read-along", func() bool { s, _ := hr.srv.final(232); return s == link.StateDone })
	if b := hr.engineBook(232); b.ID != beside.ID {
		t.Fatalf("the second read-along's run = %d, want %d", b.ID, beside.ID)
	}
	ebookRunIntact(hr, ebook)

	// A generate job reading the ebook replaces the ebook run, never taking up
	// nor clearing the audio run.
	hr.srv.add(233, "Author/Book", `{"source":"ebook"}`)
	hr.waitFor("the generate job", func() bool { s, _ := hr.srv.final(233); return s == link.StateDone })
	if b := hr.engineBook(233); b.ID == beside.ID || b.Kind != string(state.KindEbook) || b.SourcePath != ebook.SourcePath {
		t.Fatalf("the generate job's run = %+v", b)
	}
	if got, err := hr.db.GetBook(ctx, beside.ID); err != nil || got.SourcePath != beside.SourcePath || got.Goal != state.GoalReadalong {
		t.Fatalf("the audio run = %+v, %v", got, err)
	}
}

// A generate job reading the audio takes on the audio run beside an ebook run
// (its transcription kept, on at qa_sweep) as the book's own run, the ebook run
// cleared: the book's notes are the audio's from then on.
func TestGenerateTakesOnTheAudioRunBesideAnEbookRun(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	ebook := seedEbookRun(hr)
	beside := seedEarlierAt(hr, store.AudioRunKey(ebook.SourcePath), state.GoalReadalong, state.Done, "")
	before := len(hr.stagesRun(beside.ID))
	hr.srv.add(234, "Author/Book", `{"source":"audio"}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(234); return s == link.StateDone })
	b := hr.engineBook(234)
	if b.ID != beside.ID || b.SourcePath != ebook.SourcePath || b.Goal != "" || b.WorkDir != beside.WorkDir {
		t.Fatalf("engine book = %+v (audio run %d)", b, beside.ID)
	}
	if stages := hr.stagesRun(b.ID)[before:]; len(stages) == 0 || stages[0] != string(state.QASweep) {
		t.Fatalf("stages run %v, want from qa_sweep", stages)
	}
	if _, err := hr.db.GetBook(context.Background(), ebook.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the ebook run: %v", err)
	}
}

// A generate job reading the audio whose continue of the audio run beside an
// ebook run is refused (the engine stays busy with the audio run) leaves the
// ebook run as it was, its notes and manifest kept (bridge#34): the ebook run is
// cleared only once the job is sure to take the audio run on.
func TestRefusedContinueKeepsTheEbookRun(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	ebook := seedEbookRun(hr)
	beside := seedEarlierAt(hr, store.AudioRunKey(ebook.SourcePath), state.GoalReadalong, state.Done, "")
	release, ok := hr.sched.Hold(beside.ID)
	if !ok {
		t.Fatal("the engine wouldn't hold the audio run")
	}
	defer release()
	hr.srv.add(235, "Author/Book", `{"source":"audio"}`)
	stop := hr.runner(hr.h, hr.h.ReadalongJob())
	defer stop()
	hr.waitFor("the generate job ended", func() bool { s, _ := hr.srv.final(235); return s != "" })
	if s, u := hr.srv.final(235); s != link.StateFailed || u.Error == nil || *u.Error != errEarlierRunBusy.Error() {
		t.Fatalf("the generate job ended %s (%q), want failed with %q", s, errOf(u), errEarlierRunBusy)
	}
	ebookRunIntact(hr, ebook)
	if got, err := hr.db.GetBook(context.Background(), beside.ID); err != nil || got.Goal != state.GoalReadalong || got.SourcePath != beside.SourcePath {
		t.Fatalf("the audio run = %+v, %v", got, err)
	}
}

// An admin cancels a parked chapters job and asks for the read-along at once,
// before the parked job's next keepalive would tell the Bridge (BRIDGE-01): the
// read-along asks it to report now and waits for its end rather than refusing
// it as still working on the book.
func TestReadalongRightAfterACancelIsNotRefused(t *testing.T) {
	t.Parallel()
	hr := readalongHarness(t)
	stop := hr.runnerKeepalive(time.Hour, hr.h, hr.h.ChaptersJob(), hr.h.ReadalongJob())
	defer stop()
	parkChapters(hr, 236)
	hr.srv.set(func() { hr.srv.cancel[236] = true })
	hr.srv.addKind(237, ReadalongKind, "Loose.m4b", `{}`)
	hr.waitFor("the read-along job ended", func() bool { s, _ := hr.srv.final(237); return s != "" })
	if s, u := hr.srv.final(237); s == link.StateFailed && strings.Contains(errOf(u), "let it finish or cancel it first") {
		t.Fatalf("the read-along was refused: %q", errOf(u))
	}
	if s, _ := hr.srv.final(236); s != link.StateCancelled {
		t.Fatalf("the cancelled job ended %q, want cancelled", s)
	}
}
