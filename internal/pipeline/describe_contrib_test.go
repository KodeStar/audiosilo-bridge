package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/ebook"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
)

// copiedRun is a run of the ebook's text a description must not carry.
const copiedRun = "the lamplighter climbed the crooked stair and counted every one of the forty steps aloud"

// contribEbook seeds an ebook book at contributing: six chapters, their text (the
// second carrying copiedRun), the fact notes and the run's sidecars. followed maps
// a sidecars.generate job to it. It returns the executor (fake agent, local
// contribution mode, real store) and the book row.
func contribEbook(t *testing.T, fake *fakeRunner, followed bool) (*Executor, store.Book, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work", "book")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	m := audio.Manifest{Source: filepath.Join(dir, "b.epub"), Style: audio.StyleEbook, ChapterCount: 6}
	for i := 1; i <= 6; i++ {
		m.Chapters = append(m.Chapters, audio.Chapter{Chapter: i})
		text := strings.Repeat("filler ", 200)
		if i == 2 {
			text += copiedRun + strings.Repeat(" filler", 200)
		}
		if err := transcript.WriteText(filepath.Join(work, ebook.TextDir), i, text); err != nil {
			t.Fatal(err)
		}
	}
	writeManifestStruct(t, work, m)
	seedDescribeFacts(t, work)
	chars := baseChars("the-carol")
	chars.Sources = []model.Source{{Type: sourceTypeCommunity, Ref: "audiosilo-meta:work:the-carol"}}
	seedWorkSidecars(t, work, chars, baseRecaps("the-carol"))
	db, err := store.Open(context.Background(), filepath.Join(dir, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	book, err := db.CreateBook(context.Background(), store.NewBook{SourcePath: filepath.Join(dir, "carol"), WorkDir: work,
		Title: "A Carol", Authors: []string{"An Author"}, WorkID: "reacher-01", Kind: string(state.KindEbook),
		EbookPath: m.Source, ContribMode: contribModeLocal})
	if err != nil {
		t.Fatal(err)
	}
	if followed {
		if err := db.PutGenerateBook(context.Background(), store.GenerateBook{ServerJobID: 7, BookID: &book.ID,
			LibraryID: 1, RelPath: "carol"}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := withAgentConfig(dir, fake)
	cfg.DB = db
	cfg.ContribMode = contribModeLocal
	cfg.ContribCommunityRepo = testCommunityRepo
	cfg.ExportRoot = t.TempDir()
	e := NewExecutor(cfg)
	e.backoff = []time.Duration{time.Millisecond}
	return e, book, db
}

// contributeOnce runs the contributing stage, which must succeed.
func contributeOnce(t *testing.T, e *Executor, book store.Book) {
	t.Helper()
	if _, err := e.Execute(context.Background(), book, state.Contributing, scheduler.StageReport{}); err != nil {
		t.Fatalf("contributing: %v", err)
	}
}

// describingRuns is the book's describing stage runs.
func describingRuns(t *testing.T, db *store.DB, bookID int64) []store.StageRun {
	t.Helper()
	runs, err := db.ListStageRuns(context.Background(), bookID)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.StageRun
	for _, r := range runs {
		if r.Stage == DescribeStage {
			out = append(out, r)
		}
	}
	return out
}

// An ebook's text is deleted as the book reaches done, before the Bridge's job can
// ask for a description, so the contributing stage writes it first, checked
// against the book text: a run copied from the epub is refused. The job's pass at
// done (the text gone by then) reuses it with no second agent call and keeps the
// check it was made with, never "none".
func TestContributingDescribesAnEbookAgainstItsText(t *testing.T) {
	t.Parallel()
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		text := cleanDescription
		if attempt == 1 {
			text = cleanDescription + " It opens: " + copiedRun + "."
		}
		writeDescription(t, req, text)
		return agent.Result{}, nil
	}
	e, book, db := contribEbook(t, fake, true)
	contributeOnce(t, e, book)
	if p := fake.lastPrompt(DescribeStage); fake.count(DescribeStage) != 2 || !strings.Contains(p, "copies the book word for word") {
		t.Fatalf("calls %d, last prompt:\n%s", fake.count(DescribeStage), p)
	}
	runs := describingRuns(t, db, book.ID)
	var m map[string]any
	if len(runs) != 1 || runs[0].Ok == nil || !*runs[0].Ok || json.Unmarshal(runs[0].Metrics, &m) != nil ||
		m["checked_against"] != CheckedBookText {
		t.Fatalf("describing runs = %+v", runs)
	}
	if !scheduler.SentinelExists(book.WorkDir, string(state.Contributing)) {
		t.Fatal("contributing didn't finish")
	}

	// Done: the ebook's text goes, then the job's pass asks.
	if err := os.RemoveAll(filepath.Join(book.WorkDir, ebook.TextDir)); err != nil {
		t.Fatal(err)
	}
	res, err := e.Describe(context.Background(), book.ID, true)
	if err != nil || !res.Reused || res.CostUSD != 0 || res.CheckedAgainst != CheckedBookText || res.Chapters != 2 {
		t.Fatalf("done's pass = %+v, %v", res, err)
	}
	if fake.count(DescribeStage) != 2 || len(describingRuns(t, db, book.ID)) != 1 {
		t.Fatalf("done's pass called the agent again (%d calls)", fake.count(DescribeStage))
	}
	// Re-composed with the work contributing resolved.
	var d model.Description
	if err := decodeSidecarFile(FinalDescriptionPath(book.WorkDir), &d); err != nil || d.Work != "reacher-01" ||
		d.Text != cleanDescription {
		t.Fatalf("description = %+v, %v", d, err)
	}
}

// A description that fails never fails the contributing stage, and the stage
// doesn't pay for it again when it runs again (a contributing park is run again
// and again); a book no sidecars.generate job follows gets none at all.
func TestContributingSurvivesAFailingDescription(t *testing.T) {
	t.Parallel()
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeDescription(t, req, "Too short.")
		return agent.Result{Usage: agent.Usage{CostUSD: 0.02, CostReported: true}}, nil
	}
	e, book, db := contribEbook(t, fake, true)
	contributeOnce(t, e, book)
	tries := fake.count(DescribeStage)
	runs := describingRuns(t, db, book.ID)
	if tries == 0 || len(runs) != 1 || runs[0].Ok == nil || *runs[0].Ok {
		t.Fatalf("calls %d, describing runs %+v", tries, runs)
	}
	if _, err := os.Stat(FinalDescriptionPath(book.WorkDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed description was kept: %v", err)
	}
	contributeOnce(t, e, book)
	if fake.count(DescribeStage) != tries || len(describingRuns(t, db, book.ID)) != 1 {
		t.Fatalf("the stage paid again: calls %d (was %d)", fake.count(DescribeStage), tries)
	}

	unfollowed := newFakeRunner()
	e, book, db = contribEbook(t, unfollowed, false)
	contributeOnce(t, e, book)
	if unfollowed.count(DescribeStage) != 0 || len(describingRuns(t, db, book.ID)) != 0 {
		t.Fatalf("a book no job follows was described (%d calls)", unfollowed.count(DescribeStage))
	}
}
