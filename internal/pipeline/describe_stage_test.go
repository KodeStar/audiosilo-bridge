package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/spelling"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
)

// cleanDescription passes every rule: 200-1500 characters, one paragraph, neutral
// voice, nothing a transcript of "word word word" could share.
const cleanDescription = "A miser who keeps a counting house in a cold city refuses every claim on his " +
	"money or his time, including his nephew's dinner and two collectors for the poor. On the night " +
	"before the holiday his dead partner returns in chains to warn him that three spirits will visit, " +
	"and the book follows whether a man that settled can still be moved."

// Opening selection: front matter (position 0) and the first K logical chapters,
// never an excluded edge file, never past K, and the file numbers kept.
func TestSelectOpening(t *testing.T) {
	t.Parallel()
	chapters := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		name  string
		class edgeClassification
		want  []int
		k     int
	}{
		{
			// An Audible intro (file 1, excluded), an unnumbered prologue (file 2,
			// front matter at position 0), chapters 1-8 in files 3-10.
			name: "intro excluded, front matter kept",
			class: edgeClassification{LogicalCount: 8, ExcludedLeading: []int{1}, FrontMatter: []int{2}, ChapterOffset: 2,
				Positions: map[int]int{2: 0, 3: 1, 4: 2, 5: 3, 6: 4, 7: 5, 8: 6, 9: 7, 10: 8}},
			want: []int{2, 3, 4}, k: 2,
		},
		{
			// No narrated numbering: the offset alone, the excluded intro at "position 0".
			name:  "offset only",
			class: edgeClassification{LogicalCount: 9, ExcludedLeading: []int{1}, ChapterOffset: 1},
			want:  []int{2, 3, 4}, k: 3,
		},
		{
			name:  "end matter has no position",
			class: edgeClassification{LogicalCount: 2, ExcludedTrailing: []int{10}, EndMatter: []int{3}, Positions: map[int]int{1: 1, 2: 2}},
			want:  []int{1, 2}, k: 2,
		},
		{
			name:  "a whole book of ten",
			class: edgeClassification{LogicalCount: 10},
			want:  []int{1, 2, 3}, k: 3,
		},
	}
	for _, tc := range cases {
		sel := selectOpening(chapters, tc.class, "edges")
		if !reflect.DeepEqual(sel.Files, tc.want) || sel.K != tc.k {
			t.Errorf("%s: files %v K %d, want %v K %d", tc.name, sel.Files, sel.K, tc.want, tc.k)
		}
	}
}

func TestOpeningWindow(t *testing.T) {
	t.Parallel()
	for logical, want := range map[int]int{1: 1, 2: 2, 3: 2, 8: 2, 9: 3, 12: 3, 13: 4, 40: 10, 49: 13} {
		if got := openingWindow(logical); got != want {
			t.Errorf("openingWindow(%d) = %d, want %d", logical, got, want)
		}
	}
}

// writeFacts writes facts/<name> under work.
func writeFacts(t *testing.T, work, name, text string) {
	t.Helper()
	dir := filepath.Join(work, spelling.FactsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil { //nolint:gosec // test artifact
		t.Fatal(err)
	}
}

// seedDescribeFacts writes two fact chunks over files 1-6 (each section carrying a
// marker naming its file), the whole-book sheet and a spelling sheet.
func seedDescribeFacts(t *testing.T, work string) {
	t.Helper()
	writeFacts(t, work, "facts-ch1-3.md", "# Facts, audio files 1-3\n\nNote: PREAMBLE-NOTE.\n\n"+
		"## Chapter 1\n- Non-narrative file (opening announcement). MARK-F1\n\n"+
		"## Chapter 2\nEVENTS\n- A miser refuses his nephew. MARK-F2\n### Sub\n- detail MARK-F2B\n\n"+
		"## Chapter 3\n- His partner's ghost warns him. MARK-F3\n")
	writeFacts(t, work, "facts-ch4-6.md", "# Facts, audio files 4-6\n\n"+
		"## Chapter 4\n- The first spirit. MARK-F4\n\n## Chapter 5\n- MARK-F5\n\n## Chapter 6\n- He changes. MARK-F6\n")
	writeFacts(t, work, knowledgeFinalName, "# Final\n## ENDING\nENDING-SECRET\n")
	writeFacts(t, work, "spellings-through-ch6.md", "SPELLING-SHEET\n")
}

// The notes are copied mechanically: only the chosen sections, headings kept as
// file numbers, no preamble, no other file.
func TestBuildOpeningNotes(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	seedDescribeFacts(t, work)
	notes, err := buildOpeningNotes(work, []int{2, 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Chapter 2\n", "MARK-F2", "### Sub", "MARK-F2B", "## Chapter 3\n", "MARK-F3"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	for _, leak := range []string{"MARK-F1", "MARK-F4", "MARK-F5", "MARK-F6", "PREAMBLE", "ENDING-SECRET", "SPELLING-SHEET", "## Chapter 1", "## Chapter 4", "# Facts"} {
		if strings.Contains(notes, leak) {
			t.Errorf("notes leak %q:\n%s", leak, notes)
		}
	}
	if strings.Index(notes, "MARK-F2") > strings.Index(notes, "MARK-F3") {
		t.Errorf("sections out of order:\n%s", notes)
	}
	if empty, err := buildOpeningNotes(t.TempDir(), []int{1}); err != nil || empty != "" {
		t.Fatalf("no facts dir = %q, %v", empty, err)
	}
}

// describeBook seeds a finished audio book: 6 files (file 1 a short Audible-style
// intro, 2-6 story chapters), their transcripts, the fact notes and the run's
// characters.json and recaps.json. It returns the executor (fake agent, real
// store) and the book row.
func describeBook(t *testing.T, fake *fakeRunner, budget float64) (*Executor, store.Book, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work", "book")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	seedWordsManifest(t, work, audio.StyleFiles, []int{3, 3000, 3000, 3000, 3000, 3000})
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
		Title: "A Carol", Authors: []string{"An Author"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := withAgentConfig(dir, fake)
	cfg.DB = db
	cfg.BookBudgetUSD = budget
	e := NewExecutor(cfg)
	e.backoff = []time.Duration{time.Millisecond}
	return e, book, db
}

// writeDescription scripts the agent's answer.
func writeDescription(t *testing.T, req agent.Request, text string) {
	t.Helper()
	writeOut(t, req, descriptionFileName, map[string]string{"text": text})
}

// The happy path, and the spoiler bound by construction: the staged dir holds
// only opening.md (and out/), and opening.md holds only the opening chapters'
// notes: no transcript, no knowledge-final.md, no later chapter, no excluded intro.
func TestDescribeStagesOnlyTheOpeningNotes(t *testing.T) {
	t.Parallel()
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeDescription(t, req, "  "+cleanDescription+"\n")
		return agent.Result{Usage: agent.Usage{Model: "sonnet", Input: 900, Output: 120, CostUSD: 0.07, CostReported: true}}, nil
	}
	e, book, db := describeBook(t, fake, 0)
	res, err := e.Describe(context.Background(), book.ID, DescribeReport{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	req, ok := fake.lastRequest(DescribeStage)
	if !ok || req.Web || req.MaxTurns != 10 {
		t.Fatalf("request = %+v", req)
	}
	var staged []string
	_ = filepath.WalkDir(req.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != req.Dir {
			rel, _ := filepath.Rel(req.Dir, p)
			if rel != filepath.Join(agent.OutDirName, descriptionFileName) {
				staged = append(staged, rel)
			}
		}
		return nil
	})
	slices.Sort(staged)
	if !reflect.DeepEqual(staged, []string{openingNotesName, agent.OutDirName}) {
		t.Fatalf("staged dir holds %v, want only opening.md and out/", staged)
	}
	notes, _ := os.ReadFile(filepath.Join(req.Dir, openingNotesName))
	// 5 logical chapters (file 1 is the excluded intro) -> K = 2 -> files 2 and 3.
	for _, want := range []string{"## Chapter 2", "MARK-F2", "## Chapter 3", "MARK-F3"} {
		if !strings.Contains(string(notes), want) {
			t.Errorf("opening.md lacks %q:\n%s", want, notes)
		}
	}
	for _, leak := range []string{"MARK-F1", "MARK-F4", "MARK-F5", "MARK-F6", "ENDING-SECRET", "SPELLING-SHEET", "word word"} {
		if strings.Contains(string(notes), leak) {
			t.Errorf("opening.md leaks %q", leak)
		}
	}
	if !strings.Contains(req.Prompt, "first\n  2 of 5 chapters") && !strings.Contains(req.Prompt, "first 2 of 5 chapters") {
		t.Errorf("prompt doesn't state the bound:\n%s", req.Prompt)
	}

	if res.Chapters != 2 || res.LogicalChapters != 5 || res.CheckedAgainst != CheckedTranscripts || res.Reused || res.CostUSD != 0.07 {
		t.Fatalf("result = %+v", res)
	}
	var d model.Description
	if err := decodeSidecarFile(FinalDescriptionPath(book.WorkDir), &d); err != nil {
		t.Fatal(err)
	}
	if d.Work != "the-carol" || d.Text != cleanDescription || d.License != "CC-BY-SA-4.0" ||
		len(d.Sources) != 1 || d.Sources[0].Ref != "audiosilo-meta:work:the-carol" {
		t.Fatalf("description = %+v", d)
	}
	// The spend sits on a describing stage run of the book, which the budgets sum.
	runs, _ := db.ListStageRuns(context.Background(), book.ID)
	if len(runs) != 1 || runs[0].Stage != DescribeStage || runs[0].Ok == nil || !*runs[0].Ok || runs[0].CostUSD != 0.07 {
		t.Fatalf("runs = %+v", runs)
	}
	if spent, _, _ := db.SumStageRunBudgetCost(context.Background(), book.ID); spent != 0.07 {
		t.Fatalf("book spend = %v", spent)
	}
	if _, err := os.Stat(filepath.Join(book.WorkDir, chapterPositionsName)); err != nil {
		t.Fatalf("positions not kept: %v", err)
	}

	// Again: the description is reused, nothing is spent.
	again, err := e.Describe(context.Background(), book.ID, DescribeReport{})
	if err != nil || !again.Reused || again.CostUSD != 0 || again.Chapters != 2 || again.CheckedAgainst != CheckedTranscripts ||
		fake.count(DescribeStage) != 1 || again.GeneratedAt.IsZero() {
		t.Fatalf("again = %+v, %v (calls %d)", again, err, fake.count(DescribeStage))
	}
}

// A copied run is a validation failure: the retry's prompt carries the overlap.
func TestDescribeNGramHitRetries(t *testing.T) {
	t.Parallel()
	fake := newFakeRunner()
	copied := "the quick brown fox jumps over the lazy dog near the river bank"
	fake.act = func(_ *fakeRunner, req agent.Request, attempt int) (agent.Result, error) {
		text := cleanDescription
		if attempt == 1 {
			text = cleanDescription + " It opens: " + copied + "."
		}
		writeDescription(t, req, text)
		return agent.Result{}, nil
	}
	e, book, _ := describeBook(t, fake, 0)
	if err := transcript.WriteText(filepath.Join(book.WorkDir, transcript.TextDir), 2,
		strings.Repeat("filler ", 200)+copied+strings.Repeat(" filler", 200)); err != nil {
		t.Fatal(err)
	}
	res, err := e.Describe(context.Background(), book.ID, DescribeReport{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if fake.count(DescribeStage) != 2 || res.CheckedAgainst != CheckedTranscripts {
		t.Fatalf("calls %d, result %+v", fake.count(DescribeStage), res)
	}
	if p := fake.lastPrompt(DescribeStage); !strings.Contains(p, "copies the book word for word") || !strings.Contains(p, "quick brown fox") {
		t.Fatalf("retry prompt lacks the overlap:\n%s", p)
	}
}

// With every source layer gone (the retention purged the transcripts), the check
// is skipped and says so, and the opening still comes from the kept positions.
func TestDescribeAfterTheTranscriptsAreGone(t *testing.T) {
	t.Parallel()
	fake := newFakeRunner()
	fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
		writeDescription(t, req, cleanDescription)
		return agent.Result{}, nil
	}
	e, book, _ := describeBook(t, fake, 0)
	live, err := e.openingSelection(book) // classifies and keeps the positions
	if err != nil || live.Basis != "edges" || !reflect.DeepEqual(live.Files, []int{2, 3}) {
		t.Fatalf("live = %+v, %v", live, err)
	}
	if err := os.RemoveAll(filepath.Join(book.WorkDir, transcript.TextDir)); err != nil {
		t.Fatal(err)
	}
	saved, err := e.openingSelection(book)
	if err != nil || saved.Basis != "saved" || !reflect.DeepEqual(saved.Files, live.Files) || saved.K != live.K {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
	res, err := e.Describe(context.Background(), book.ID, DescribeReport{})
	if err != nil || res.CheckedAgainst != CheckedNone {
		t.Fatalf("Describe = %+v, %v", res, err)
	}

	// No kept positions either: file numbers, the logical count from the recaps
	// (through chapter 3 here), so files 1-2 only (never past a story position).
	if err := os.Remove(filepath.Join(book.WorkDir, chapterPositionsName)); err != nil {
		t.Fatal(err)
	}
	files, err := e.openingSelection(book)
	if err != nil || files.Basis != "files" || files.Logical != 3 || !reflect.DeepEqual(files.Files, []int{1, 2}) {
		t.Fatalf("files = %+v, %v", files, err)
	}
}

// Validation exhausted, a budget park before any spend, and a rate limit that
// parks at once with NoWait: each comes back as the driver's park.
func TestDescribeParks(t *testing.T) {
	t.Parallel()
	t.Run("validation exhausted", func(t *testing.T) {
		t.Parallel()
		fake := newFakeRunner()
		fake.act = func(_ *fakeRunner, req agent.Request, _ int) (agent.Result, error) {
			writeDescription(t, req, "Too short.")
			return agent.Result{}, nil
		}
		e, book, db := describeBook(t, fake, 0)
		_, err := e.Describe(context.Background(), book.ID, DescribeReport{})
		var park *scheduler.ParkError
		if !errors.As(err, &park) || park.Code != state.ParkAgentValidationExhausted {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(FinalDescriptionPath(book.WorkDir)); !os.IsNotExist(statErr) {
			t.Fatal("a failed description was kept")
		}
		runs, _ := db.ListStageRuns(context.Background(), book.ID)
		if len(runs) != 1 || runs[0].Ok == nil || *runs[0].Ok {
			t.Fatalf("runs = %+v", runs)
		}
	})
	t.Run("budget", func(t *testing.T) {
		t.Parallel()
		fake := newFakeRunner()
		e, book, db := describeBook(t, fake, 1)
		ctx := context.Background()
		id, _ := db.StartStageRun(ctx, book.ID, "fact_pass", 1)
		_ = db.AddOpenStageRunUsage(ctx, book.ID, "fact_pass", "opus", 1, 1, 2)
		_ = db.FinishStageRun(ctx, id, true, nil)
		_, err := e.Describe(ctx, book.ID, DescribeReport{})
		var park *scheduler.ParkError
		if !errors.As(err, &park) || park.Code != state.ParkBudgetExceeded || fake.count(DescribeStage) != 0 {
			t.Fatalf("err = %v, calls %d", err, fake.count(DescribeStage))
		}
	})
	t.Run("rate limit, no wait", func(t *testing.T) {
		t.Parallel()
		fake := newFakeRunner()
		fake.act = func(*fakeRunner, agent.Request, int) (agent.Result, error) {
			return agent.Result{}, &agent.RateLimitError{Detail: "usage limit"}
		}
		e, book, _ := describeBook(t, fake, 0)
		e.backoff = []time.Duration{time.Hour} // NoWait must not sleep it out
		_, err := e.Describe(context.Background(), book.ID, DescribeReport{NoWait: true})
		var park *scheduler.ParkError
		if !errors.As(err, &park) || park.Code != state.ParkAgentRateLimited || park.RetryAfter.IsZero() || fake.count(DescribeStage) != 1 {
			t.Fatalf("err = %v, calls %d", err, fake.count(DescribeStage))
		}
	})
}

// Nothing to describe from: no characters.json, or no notes for the opening.
func TestDescribeNoNotes(t *testing.T) {
	t.Parallel()
	fake := newFakeRunner()
	e, book, _ := describeBook(t, fake, 0)
	if err := os.RemoveAll(filepath.Join(book.WorkDir, spelling.FactsDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Describe(context.Background(), book.ID, DescribeReport{}); !errors.Is(err, ErrNoNotes) {
		t.Fatalf("no facts: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(book.WorkDir, sidecarsDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Describe(context.Background(), book.ID, DescribeReport{}); !errors.Is(err, ErrNoNotes) {
		t.Fatalf("no characters.json: %v", err)
	}
	if fake.count(DescribeStage) != 0 {
		t.Fatal("the agent ran with nothing to read")
	}
}

// A run whose characters.json can't carry a description (no sources) fails before
// any spend: no answer could fix it.
func TestDescribeRefusesAnUncomposableRun(t *testing.T) {
	t.Parallel()
	fake := newFakeRunner()
	e, book, _ := describeBook(t, fake, 0)
	chars := baseChars("the-carol")
	chars.Sources = nil
	seedWorkSidecars(t, book.WorkDir, chars, baseRecaps("the-carol"))
	_, err := e.Describe(context.Background(), book.ID, DescribeReport{})
	if err == nil || errors.Is(err, ErrNoNotes) || !strings.Contains(err.Error(), "can't carry a description") || fake.count(DescribeStage) != 0 {
		t.Fatalf("err = %v, calls %d", err, fake.count(DescribeStage))
	}
}

// Each voice and shape rule, allowed and denied.
func TestDescriptionProblems(t *testing.T) {
	t.Parallel()
	if p := descriptionProblems(cleanDescription); len(p) != 0 {
		t.Fatalf("clean text refused: %v", p)
	}
	two := cleanDescription + "\n\nA second paragraph widens the frame to the clerk and his family at home."
	if p := descriptionProblems(two); len(p) != 0 {
		t.Fatalf("two paragraphs refused: %v", p)
	}
	// "Your" inside a word, a question mark inside a title, a hyphen: all fine.
	fine := cleanDescription + " Its youthful clerk asks whether the work ever ends (\"Why?\"), in a well-known story."
	if p := descriptionProblems(fine); len(p) != 0 {
		t.Fatalf("allowed text refused: %v", p)
	}
	denied := map[string]string{
		"control character":                  cleanDescription + "\tTabbed.",
		"em dash":                            strings.Replace(cleanDescription, ", including", " \u2014 including", 1),
		"characters; it must be 200 to 1500": "Too short to say anything.",
		"paragraphs":                         two + "\n\nAnd a third paragraph is one too many for a description.",
		"addresses the reader":               cleanDescription + " You will not forget it.",
		"question mark":                      cleanDescription + " Can he change?",
		"sales language":                     cleanDescription + " A gripping masterpiece.",
	}
	for want, text := range denied {
		p := descriptionProblems(text)
		if len(p) == 0 || !strings.Contains(strings.Join(p, "; "), want) {
			t.Errorf("%q: problems %v, want one naming %q", want, p, want)
		}
	}
	if p := descriptionProblems(strings.Repeat("word ", 400)); len(p) == 0 {
		t.Error("an over-long text passed")
	}
}

// The agent's answer is exactly {"text": ...}.
func TestReadDescriptionOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, descriptionFileName), []byte(s), 0o644); err != nil { //nolint:gosec // test artifact
			t.Fatal(err)
		}
	}
	write(`{"text": "  A paragraph.  "}`)
	if text, err := readDescriptionOut(dir); err != nil || text != "A paragraph." {
		t.Fatalf("allowed: %q, %v", text, err)
	}
	for _, bad := range []string{`{"text": "x", "title": "y"}`, `{"text": "x"} {}`, `{}`, `["x"]`, `not json`, `{"text": 3}`} {
		write(bad)
		if _, err := readDescriptionOut(dir); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	write(`{"text": "` + strings.Repeat("x", descriptionOutCap) + `"}`)
	if _, err := readDescriptionOut(dir); err == nil {
		t.Error("an oversized answer was accepted")
	}
}

// The composed file passes meta's description schema; a bad work slug doesn't.
func TestComposedDescriptionSchema(t *testing.T) {
	t.Parallel()
	chars := baseChars("the-carol")
	raw, err := composedDescription(chars, cleanDescription)
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil || d["work"] != "the-carol" || d["license"] != "CC-BY-SA-4.0" {
		t.Fatalf("composed = %s, %v", raw, err)
	}
	if _, err := composedDescription(baseChars("Not A Slug"), cleanDescription); err == nil {
		t.Fatal("a bad slug passed the schema")
	}
}

// The ebook path: chapters are the manifest's own, no intro, nothing kept.
func TestDescribeEbookOpening(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	m := audio.Manifest{Source: "/x/b.epub", Style: audio.StyleEbook, ChapterCount: 12}
	for i := 1; i <= 12; i++ {
		m.Chapters = append(m.Chapters, audio.Chapter{Chapter: i})
	}
	writeManifestStruct(t, work, m)
	e := NewExecutor(Config{})
	sel, err := e.openingSelection(store.Book{WorkDir: work, Kind: string(state.KindEbook)})
	if err != nil || sel.Basis != "edges" || !reflect.DeepEqual(sel.Files, []int{1, 2, 3}) {
		t.Fatalf("ebook = %+v, %v", sel, err)
	}
	if _, err := os.Stat(filepath.Join(work, chapterPositionsName)); !os.IsNotExist(err) {
		t.Fatal("an ebook kept positions it can always derive")
	}
}
