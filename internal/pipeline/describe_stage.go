package pipeline

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-meta/pkg/canonical"
	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
)

// DescribeStage names the description step's stage_runs (BRIDGE-PLAN.md Phase 1c).
// It is NOT a state of the engine's state machine: the step runs once a book's
// sidecars are final (at the start of contributing for a book a sidecars.generate
// job follows, then called by the Bridge's sidecars.generate and sidecars.describe
// jobs, never dispatched by the scheduler), so the readers of stage_runs that drive
// the state machine (reconcile, the supervisor) skip it. Its runs count wherever spend is summed (the budgets, the estimates).
// config's model maps route it under the same name (state.Describing).
const DescribeStage = string(state.Describing)

// What the description's no-verbatim check measured against
// (DescribeResult.CheckedAgainst, bridge.json's description.checked_against).
const (
	CheckedTranscripts = "transcripts"
	CheckedBookText    = "book text"
	// CheckedNone: every source layer was gone, so nothing was checked: an audio
	// book's transcripts after the retention purged them, and an ebook's text,
	// which the engine deletes as the book reaches done (a licensing rule; the
	// contributing stage describes a generated book before that, so only a later
	// sidecars.describe meets it). The agent never saw any of them either.
	CheckedNone = "none"
)

const (
	descriptionFileName  = "description.json"
	openingNotesName     = "opening.md"
	chapterPositionsName = "chapter_positions.json"
	// The description schema's bounds on the text, in code points (a drift test
	// holds them to meta's description.schema.json).
	descriptionMinRunes = 200
	descriptionMaxRunes = 1500
	// descriptionMaxParagraphs: one paragraph, two at the outside (AUTHORING.md).
	descriptionMaxParagraphs = 2
	// descriptionOutCap bounds the read of the agent's out/description.json.
	descriptionOutCap = 64 << 10
)

// ErrNoNotes is a book whose work dir holds nothing to describe it from: no
// validated characters.json (whose work and sources the description carries), no
// manifest, or no fact notes for its opening chapters.
var ErrNoNotes = errors.New("the book has no notes to describe it from")

// ErrOneChapter is a book with a single story chapter: its opening is the whole
// book, ending included, so the step writes no description for it (the window
// never stages the last chapter).
var ErrOneChapter = errors.New("the book has a single chapter, so no opening leaves out its ending")

// DescribeResult is a written (or reused) description.
type DescribeResult struct {
	// Path is the composed model.Description in the work dir
	// (FinalDescriptionPath), canonical JSON.
	Path string
	// Chapters is K, how many opening chapters' notes the agent saw.
	Chapters       int
	CheckedAgainst string
	// CostUSD is this call's spend on the budget basis (0 for a reuse).
	CostUSD     float64
	GeneratedAt time.Time
	// Reused: the work dir already held the description, so nothing was spent.
	Reused bool
}

// FinalDescriptionPath is where a book's description lives in its work dir once
// the description step wrote it: what the Bridge copies into the library.
func FinalDescriptionPath(workDir string) string {
	return filepath.Join(workDir, sidecarsDir, descriptionFileName)
}

// descriptionPromptData feeds description.md (missingkey=error).
type descriptionPromptData struct {
	Title        string
	Authors      string
	Series       string
	SeriesPos    string
	Chapters     int
	ChapterCount int
	IsEbook      bool
}

// Describe writes the book's spoiler-free, own-words description from the fact
// notes of its opening chapters ONLY: never a transcript, never knowledge-final.md
// (which carries the ending), never a later chapter's notes. That staging is the
// spoiler bound, by construction. The text is validated (shape, length, voice, the
// description schema, the no-verbatim check against whatever transcript layer is
// still there), composed into a model.Description carrying the run's work and
// sources, and kept in the work dir. A description already there is reused, so a
// resumed job doesn't pay twice.
//
// It is not an engine stage: it opens its own stage_run (DescribeStage) on the
// book so the spend counts, and runs through the shared agent driver, whose parks
// (rate limit, budget, credential, unavailable, validation exhausted) come back as
// *scheduler.ParkError for the caller to handle. noWait parks a rate limit at once
// instead of waiting it out in-process: sidecars.generate's description must never
// hold its job.
func (e *Executor) Describe(ctx context.Context, bookID int64, noWait bool) (DescribeResult, error) {
	if e.db == nil {
		return DescribeResult{}, errors.New("describe: no store")
	}
	book, err := e.db.GetBook(ctx, bookID)
	if err != nil {
		return DescribeResult{}, err
	}
	chars, err := describeBasis(book.WorkDir)
	if err != nil {
		return DescribeResult{}, err
	}
	if res, ok := e.reuseDescription(ctx, book, chars); ok {
		return res, nil
	}
	// The run's own work and sources must make a valid description before any
	// spend: no agent answer could repair them.
	if err := checkComposable(chars); err != nil {
		return DescribeResult{}, err
	}
	// The preparation (local reads) before the budgets, so a call the budget parks
	// still keeps the chapter positions while the transcripts are there; the budgets
	// before the stage run and any spend: a budget-parked retry costs nothing.
	sel, err := e.openingSelection(book)
	if err != nil {
		return DescribeResult{}, err
	}
	if sel.Logical == 1 {
		return DescribeResult{}, ErrOneChapter
	}
	notes, err := buildOpeningNotes(book.WorkDir, sel.Files)
	if err != nil {
		return DescribeResult{}, err
	}
	if notes == "" {
		return DescribeResult{}, fmt.Errorf("%w (no fact notes for the opening chapters)", ErrNoNotes)
	}
	if err := e.budgetPreflight(ctx, book, state.State(DescribeStage)); err != nil {
		return DescribeResult{}, err
	}

	attempt, err := e.db.NextStageAttempt(ctx, book.ID, DescribeStage)
	if err != nil {
		return DescribeResult{}, fmt.Errorf("describe: number its stage run: %w", err)
	}
	runID, err := e.db.StartStageRun(ctx, book.ID, DescribeStage, attempt)
	if err != nil {
		return DescribeResult{}, fmt.Errorf("describe: open its stage run: %w", err)
	}
	m := map[string]any{"chapters": sel.K, "logical_chapters": sel.Logical, "basis": sel.Basis}
	finish := func(ok bool, extra map[string]any) float64 {
		for k, v := range extra {
			m[k] = v
		}
		bg := context.WithoutCancel(ctx)
		if ferr := e.db.FinishStageRun(bg, runID, ok, metrics(m)); ferr != nil {
			e.log.Warn("describe: finish its stage run", "book", book.ID, "err", ferr)
		}
		cost, cerr := e.db.StageRunBudgetCost(bg, runID)
		if cerr != nil {
			e.log.Warn("describe: read its cost", "book", book.ID, "err", cerr)
		}
		return cost
	}
	// fail finishes the run failed, err recorded with extra.
	fail := func(err error, extra map[string]any) float64 {
		m["error"] = err.Error()
		return finish(false, extra)
	}

	st, err := agent.New(book.WorkDir, DescribeStage, attempt)
	if err == nil {
		err = st.WriteFile(openingNotesName, []byte(notes))
	}
	if err != nil {
		fail(err, nil)
		return DescribeResult{}, fmt.Errorf("describe: stage the notes: %w", err)
	}

	var composed []byte
	var checked string
	// checkErr is a local fault of the no-verbatim check (the disk, a transcript
	// that can't be read): nothing the agent could fix, so the answer is taken and
	// the run fails after it, never retried (and paid) with the Bridge's paths in
	// the agent's prompt.
	var checkErr error
	validate := func(_ agent.Result, s *agent.Staging) error {
		text, err := readDescriptionOut(s.OutDir())
		if err != nil {
			return err
		}
		if problems := descriptionProblems(text, book.Title, book.Series); len(problems) > 0 {
			return fmt.Errorf("out/%s: %s", descriptionFileName, strings.Join(problems, "; "))
		}
		raw, err := composedDescription(chars, text)
		if err != nil {
			return err
		}
		findings, layers, err := descriptionNGram(book, raw)
		if err != nil {
			checkErr = err
			return nil
		}
		if len(findings) > 0 {
			return fmt.Errorf("out/%s copies the book word for word; rewrite these runs in your own words: %s",
				descriptionFileName, joinFindings(findings))
		}
		composed, checked = raw, checkedAgainst(layers)
		return nil
	}
	data := descriptionPromptData{
		Title: book.Title, Authors: authors(book), Series: book.Series, SeriesPos: book.SeriesPos,
		Chapters: sel.K, ChapterCount: sel.Logical, IsEbook: state.ParseKind(book.Kind) == state.KindEbook,
	}
	backoff := e.backoff
	if noWait {
		backoff = []time.Duration{}
	}
	usage, err := e.runAgentWith(ctx, book, state.State(DescribeStage), scheduler.StageReport{}, st,
		"description.md", data, false, validate, backoff)
	usageMetrics := usage.metricsMap()
	if err == nil && checkErr != nil {
		err = fmt.Errorf("describe: the no-verbatim check could not run: %w", checkErr)
	}
	if err != nil {
		return DescribeResult{CostUSD: fail(err, usageMetrics)}, err
	}
	path := FinalDescriptionPath(book.WorkDir)
	if err := fsutil.WriteFileAtomic(path, composed, 0o644); err != nil {
		return DescribeResult{CostUSD: fail(err, usageMetrics)}, fmt.Errorf("describe: keep the description: %w", err)
	}
	usageMetrics["checked_against"] = checked
	cost := finish(true, usageMetrics)
	return DescribeResult{Path: path, Chapters: sel.K, CheckedAgainst: checked, CostUSD: cost,
		GeneratedAt: time.Now().UTC()}, nil
}

// describeAtContributing runs the description step at the start of the
// contributing stage, while the book's source text is still there: an ebook's text
// goes as the book reaches done, before the Bridge's job can ask, so a description
// first written then is checked against nothing. The job's own pass later finds it
// in the work dir and reuses it (re-composed with the work contributing resolved,
// nothing spent), keeping this check's checked_against from the run's metrics.
//
// Best effort, and only for a book a sidecars.generate job follows (the one reader
// of a description): once per book (a describing run already recorded, ok or not,
// is never paid for again, since a contributing park runs the stage again), with no
// rate-limit wait, under the stage's heartbeat, and any error only logged. The
// book is the scheduler's in-flight book here, so no hold is taken.
func (e *Executor) describeAtContributing(ctx context.Context, book store.Book, r scheduler.StageReport) {
	if e.db == nil {
		return
	}
	followed, err := e.db.GenerateBookFollowed(ctx, book.ID)
	if err != nil || !followed {
		if err != nil {
			e.log.Warn("contributing: no description (the generate job couldn't be looked up)", "book", book.ID, "err", err)
		}
		return
	}
	if n, err := e.db.CountStageRuns(ctx, book.ID, DescribeStage); err != nil || n > 0 {
		return
	}
	var res DescribeResult
	err = runWithStageHeartbeat(ctx, r, nil, func() error {
		var derr error
		res, derr = e.Describe(ctx, book.ID, true)
		return derr
	})
	if err != nil {
		e.log.Warn("contributing: no description; the stage goes on without one", "book", book.ID, "err", err)
		return
	}
	e.log.Info("contributing: wrote the description", "book", book.ID, "checked_against", res.CheckedAgainst)
}

// describeBasis reads the run's validated characters.json: the description
// carries its work and sources. Missing is ErrNoNotes (no finished run here).
func describeBasis(workDir string) (*model.Characters, error) {
	path, _ := FinalSidecarsPaths(workDir)
	var chars model.Characters
	if err := decodeSidecarFile(path, &chars); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w (no characters.json)", ErrNoNotes)
		}
		return nil, fmt.Errorf("describe: read characters.json: %w", err)
	}
	return &chars, nil
}

// composeDescription is the description file: the text with the run's work and
// sources and the share-alike license.
func composeDescription(chars *model.Characters, text string) model.Description {
	return model.Description{Work: chars.Work, Text: text, License: sidecarLicenseContent, Sources: chars.Sources}
}

// composedDescription composes the description and checks it against meta's
// description schema, returning its canonical JSON.
func composedDescription(chars *model.Characters, text string) ([]byte, error) {
	raw, err := json.Marshal(composeDescription(chars, text))
	if err != nil {
		return nil, err
	}
	if raw, err = canonical.Format(raw); err != nil {
		return nil, err
	}
	schemas, err := sidecarSchemas()
	if err != nil {
		return nil, err
	}
	if v, _ := schemaViolation(schemas["description"], raw); v != "" {
		return nil, fmt.Errorf("the description fails the description schema: %s", v)
	}
	return raw, nil
}

// checkComposable says whether the run's work and sources can carry a description
// at all, by composing one around a placeholder text that satisfies the schema.
func checkComposable(chars *model.Characters) error {
	if _, err := composedDescription(chars, strings.Repeat("placeholder ", 20)); err != nil {
		return fmt.Errorf("describe: the run's characters.json can't carry a description: %w", err)
	}
	return nil
}

// reuseDescription returns the description already in the work dir, re-composed
// with the run's current work and sources (a later stage may have changed them),
// when a successful describing run recorded how it was made. Anything less is
// written again.
func (e *Executor) reuseDescription(ctx context.Context, book store.Book, chars *model.Characters) (DescribeResult, bool) {
	path := FinalDescriptionPath(book.WorkDir)
	cur, err := os.ReadFile(path) //nolint:gosec // the book's work dir
	if err != nil {
		return DescribeResult{}, false
	}
	var d model.Description
	if err := decodeSidecarBytes(cur, &d); err != nil || len(descriptionProblems(d.Text, book.Title, book.Series)) > 0 {
		return DescribeResult{}, false
	}
	run, err := e.db.LatestOkStageRun(ctx, book.ID, DescribeStage)
	if err != nil {
		return DescribeResult{}, false
	}
	raw, err := composedDescription(chars, d.Text)
	if err != nil {
		return DescribeResult{}, false
	}
	if !bytes.Equal(cur, raw) {
		if err := fsutil.WriteFileAtomic(path, raw, 0o644); err != nil {
			return DescribeResult{}, false
		}
	}
	var m struct {
		Chapters       int    `json:"chapters"`
		CheckedAgainst string `json:"checked_against"`
	}
	_ = json.Unmarshal(run.Metrics, &m)
	at, _ := time.Parse(time.RFC3339Nano, run.FinishedAt)
	return DescribeResult{Path: path, Chapters: m.Chapters, CheckedAgainst: cmp.Or(m.CheckedAgainst, CheckedNone),
		GeneratedAt: at.UTC(), Reused: true}, true
}

// openingSelection is which audio files' (or ebook chapters') notes the agent
// sees: the files whose LOGICAL position is 0 (front matter) or among the book's
// first K story positions, K = max(2, ceil(logical/4)) capped at logical-1 (never
// the last chapter), logical the count of story positions the files hold (capped
// at the classifier's count).
type openingSelection struct {
	Files   []int // manifest chapter numbers, ascending
	K       int
	Logical int
	// Basis is where the positions came from: "edges" (classified now), "saved"
	// (classified while the transcripts were there), "files" (neither: file
	// numbers, which never sit below their story position, so the bound holds).
	Basis string
}

// openingWindow is K for a book of logical story chapters: a quarter of them,
// two at least, and never the last one (its ending), so a book of two gets its
// first only and a book of one none.
func openingWindow(logical int) int {
	return max(0, min(max(2, (logical+3)/4), logical-1))
}

// openingSelection classifies the book's chapters as the sidecar stages did
// (classifyBookEdges), so the bound is the same logical chapters the recaps use.
// The classification reads the transcripts, which the retention deletes; the
// positions are kept beside the notes each time they are classified, and a book
// whose transcripts are gone uses those, else its file numbers (a file's number
// is never below its story chapter, so that bound is safe, only less generous).
func (e *Executor) openingSelection(book store.Book) (openingSelection, error) {
	m, err := audio.ReadManifest(book.WorkDir)
	var fp string
	if err == nil {
		// The kept positions are keyed by the pipeline's one meaning of "the
		// chapter layout changed".
		fp, err = manifestFingerprint(book.WorkDir)
	}
	if err != nil {
		return openingSelection{}, fmt.Errorf("%w (no manifest: %v)", ErrNoNotes, err)
	}
	chapters := make([]int, 0, len(m.Chapters))
	for _, c := range m.Chapters {
		chapters = append(chapters, c.Chapter)
	}
	ebook := state.ParseKind(book.Kind) == state.KindEbook
	switch {
	case ebook || transcriptsPresent(book.WorkDir, chapters):
		class, err := classifyBookEdges(book)
		if err != nil {
			return openingSelection{}, err
		}
		if !ebook {
			if err := savePositions(book.WorkDir, fp, class); err != nil {
				e.log.Warn("describe: keeping the chapter positions failed", "book", book.ID, "err", err)
			}
		}
		return selectOpening(chapters, class, "edges"), nil
	default:
		if class, ok := loadPositions(book.WorkDir, fp); ok {
			return selectOpening(chapters, class, "saved"), nil
		}
		return selectOpening(chapters, fileClassification(book.WorkDir, len(chapters)), "files"), nil
	}
}

// selectOpening picks the opening files from a classification: front matter
// (position 0) and the files at the book's first K story positions. Excluded edge
// files (an Audible intro, closing credits) are never story; end matter has no
// position. K counts the story positions the files hold, not the classifier's
// LogicalCount alone: narrated numbering takes that from the highest announcement
// it believed, so one late or misheard announcement ("Forty." opening the last of
// twelve files) would widen the window far past the opening, and a sequel's
// continued numbering (chapters 45 to 56) would leave no position inside 1..K. The
// logical count still caps it: the "files" basis's positions are file numbers,
// which can run past the story's count.
func selectOpening(chapters []int, class edgeClassification, basis string) openingSelection {
	excluded := map[int]bool{}
	for _, c := range slices.Concat(class.ExcludedLeading, class.ExcludedTrailing) {
		excluded[c] = true
	}
	positions := map[int]int{}
	var story []int // the story positions (1 on) the retained files hold, distinct
	for _, c := range chapters {
		pos, ok := class.PositionOf(c)
		if excluded[c] || !ok || pos < 0 {
			continue
		}
		positions[c] = pos
		if pos >= 1 && !slices.Contains(story, pos) {
			story = append(story, pos)
		}
	}
	slices.Sort(story)
	logical := min(len(story), class.LogicalCount)
	sel := openingSelection{Logical: logical, K: openingWindow(logical), Basis: basis}
	if sel.K < 1 {
		return sel
	}
	last := story[sel.K-1]
	for _, c := range chapters {
		if pos, ok := positions[c]; ok && pos <= last {
			sel.Files = append(sel.Files, c)
		}
	}
	slices.Sort(sel.Files)
	return sel
}

// transcriptsPresent reports whether any chapter still has a transcript (the
// retention deletes them all at once).
func transcriptsPresent(workDir string, chapters []int) bool {
	for _, c := range chapters {
		if _, ok := transcript.ChapterTextPath(workDir, c); ok {
			return true
		}
	}
	return false
}

// savedPositions is chapter_positions.json: the part of a classification the
// opening selection needs, with the manifest it was made from.
type savedPositions struct {
	Manifest     string      `json:"manifest"`
	LogicalCount int         `json:"logical_count"`
	Excluded     []int       `json:"excluded,omitempty"`
	Offset       int         `json:"chapter_offset"`
	Positions    map[int]int `json:"positions,omitempty"`
}

// savePositions keeps a classification of the manifest fingerprinted fp beside the
// work dir's other durables (the transcript retention leaves it); an unchanged one
// isn't written again.
func savePositions(workDir, fp string, class edgeClassification) error {
	raw, err := json.Marshal(savedPositions{Manifest: fp, LogicalCount: class.LogicalCount,
		Excluded: slices.Concat(class.ExcludedLeading, class.ExcludedTrailing), Offset: class.ChapterOffset,
		Positions: class.Positions})
	if err != nil {
		return err
	}
	path := filepath.Join(workDir, chapterPositionsName)
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, raw) { //nolint:gosec // the book's work dir
		return nil
	}
	return fsutil.WriteFileAtomic(path, raw, 0o644)
}

// loadPositions reads the kept classification, when it was made from the
// manifest the work dir holds now (fingerprinted fp).
func loadPositions(workDir, fp string) (edgeClassification, bool) {
	raw, err := os.ReadFile(filepath.Join(workDir, chapterPositionsName)) //nolint:gosec // the book's work dir
	if err != nil {
		return edgeClassification{}, false
	}
	var s savedPositions
	if err := json.Unmarshal(raw, &s); err != nil || s.LogicalCount < 1 {
		return edgeClassification{}, false
	}
	if fp != s.Manifest {
		return edgeClassification{}, false
	}
	return edgeClassification{LogicalCount: s.LogicalCount, ExcludedLeading: s.Excluded, ChapterOffset: s.Offset,
		Positions: s.Positions}, true
}

// fileClassification is the fallback with no transcripts and no kept positions:
// every file at its own number, the logical count from the recaps (their last
// chaptered entry runs through the last story chapter), else the file count.
func fileClassification(workDir string, files int) edgeClassification {
	logical := files
	_, recapsPath := FinalSidecarsPaths(workDir)
	var recs model.Recaps
	if err := decodeSidecarFile(recapsPath, &recs); err == nil {
		last := 0
		for _, r := range recs.Recaps {
			last = max(last, r.Through.Chapter)
		}
		if last > 0 && last < files {
			logical = last
		}
	}
	return edgeClassification{LogicalCount: logical}
}

// sectionBreakRe ends a fact section: any level-1 or level-2 heading.
var sectionBreakRe = regexp.MustCompile(`^#{1,2}(\s|$)`)

// buildOpeningNotes is opening.md: the `## Chapter N` sections of the chosen files,
// copied mechanically from the fact chunks with their file numbering kept, in
// chapter order. The chunks are the fact pass's own (chunk_plan.json, named by
// factsChunkName), and only those covering one of the files are read; nothing
// else from facts/ (knowledge-final.md, the spelling sheets, a chunk's preamble)
// is copied. "" when none of the files has notes.
func buildOpeningNotes(workDir string, files []int) (string, error) {
	want := map[int]bool{}
	for _, f := range files {
		want[f] = true
	}
	plan, err := loadChunkPlan(workDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sections := map[int]string{}
	for _, c := range plan.Chunks {
		if !slices.ContainsFunc(files, func(f int) bool { return f >= c.From && f <= c.To }) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(factsDirPath(workDir), factsChunkName(c.From, c.To))) //nolint:gosec // the book's work dir
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		for ch, text := range factSections(string(raw)) {
			if want[ch] && sections[ch] == "" {
				sections[ch] = text
			}
		}
	}
	var b strings.Builder
	for _, f := range files {
		if s := sections[f]; s != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(s)
		}
	}
	return b.String(), nil
}

// factSections splits a fact chunk into its `## Chapter N` sections (heading
// included), each running to the next level-1 or level-2 heading. A manifest may
// number its files from 0 (a "Chapter 0" prologue), so 0 is a section like any
// other; -1 is "in none".
func factSections(text string) map[int]string {
	out := map[int]string{}
	cur := -1
	var b strings.Builder
	flush := func() {
		if cur >= 0 {
			if s := strings.TrimRight(b.String(), " \t\r\n"); s != "" {
				out[cur] = s + "\n"
			}
		}
		cur = -1
		b.Reset()
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		if sectionBreakRe.MatchString(trimmed) {
			flush()
			if m := factsHeadingRe.FindStringSubmatch(trimmed); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					cur = n
				}
			}
		}
		if cur >= 0 {
			b.WriteString(line)
		}
	}
	flush()
	return out
}

// readDescriptionOut reads the agent's out/description.json: exactly {"text": ...}
// (no other key, nothing after it), its text trimmed.
func readDescriptionOut(outDir string) (string, error) {
	path := filepath.Join(outDir, descriptionFileName)
	f, err := os.Open(path) //nolint:gosec // the staged out/ dir
	if err != nil {
		return "", fmt.Errorf("out/%s: %v", descriptionFileName, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	raw, err := io.ReadAll(io.LimitReader(f, descriptionOutCap+1))
	if err != nil {
		return "", fmt.Errorf("out/%s: %v", descriptionFileName, err)
	}
	if len(raw) > descriptionOutCap {
		return "", fmt.Errorf("out/%s is larger than %d bytes", descriptionFileName, descriptionOutCap)
	}
	var out struct {
		Text *string `json:"text"`
	}
	shape := `write exactly {"text": "..."} and nothing else`
	if err := decodeSidecarBytes(raw, &out); err != nil {
		return "", fmt.Errorf("out/%s: %v (%s)", descriptionFileName, err, shape)
	}
	if out.Text == nil {
		return "", fmt.Errorf("out/%s has no text (%s)", descriptionFileName, shape)
	}
	return strings.TrimSpace(*out.Text), nil
}

// The voice rules a description is held to (AUTHORING.md's `description`
// contract): no second person, no rhetorical question, no sales words.
var (
	// Word edges by Unicode letter, mark and digit: RE2's \b is ASCII only, so
	// "Xīyou" would hold a "you".
	secondPersonRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{M}\p{N}_])(you|your|yours|yourself|yourselves)(?:[^\p{L}\p{M}\p{N}_]|$)`)
	// A question mark ending a sentence: followed (after any closing quote or
	// bracket) by a space or the end of the text.
	questionRe  = regexp.MustCompile(`\?["'\x{201D}\x{2019})\]]*(\s|$)`)
	salesWordRe = regexp.MustCompile(`(?i)\b(gripping|unputdownable|page-turners?|must-reads?|masterpieces?|breathtaking|captivating|riveting|thrilling|unforgettable)\b`)
	// A blank line, a no-break space (\p{Zs}) on it included.
	paragraphBreakRe = regexp.MustCompile(`\n[\s\p{Zs}]*\n`)
)

// descriptionProblems lists what is wrong with a (trimmed) description text, none
// when it passes. Each is worded for the agent's retry. names are the book's own
// (its title, its series), which the voice rules skip: "Me Before You" is no second
// person, "Do Androids Dream of Electric Sheep?" no rhetorical question and "The
// Masterpiece" no sales word, and a description may name its book.
func descriptionProblems(text string, names ...string) []string {
	var out []string
	// The line and paragraph separators too: only a plain newline separates.
	if strings.ContainsFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' || r == '\u2028' || r == '\u2029'
	}) {
		out = append(out, "the text holds a control character (only plain newlines between paragraphs are allowed)")
	}
	if hasEmDash(text) {
		out = append(out, "the text uses an em dash; use a hyphen or restructure the sentence")
	}
	if n := utf8.RuneCountInString(text); n < descriptionMinRunes || n > descriptionMaxRunes {
		out = append(out, fmt.Sprintf("the text is %d characters; it must be %d to %d (aim for 80 to 150 words)",
			n, descriptionMinRunes, descriptionMaxRunes))
	}
	if n := len(paragraphBreakRe.Split(text, -1)); n > descriptionMaxParagraphs {
		out = append(out, fmt.Sprintf("the text has %d paragraphs; write one, two at most", n))
	}
	voice := withoutNames(text, names)
	if m := secondPersonRe.FindStringSubmatch(voice); m != nil {
		out = append(out, fmt.Sprintf("the text addresses the reader (%q); write in the third person", m[1]))
	}
	if questionRe.MatchString(voice) {
		out = append(out, "a sentence ends in a question mark; state the premise instead of asking")
	}
	if words := salesWordRe.FindAllString(voice, -1); len(words) > 0 {
		out = append(out, fmt.Sprintf("the text uses sales language (%s); keep a neutral reference-guide voice",
			strings.Join(words, ", ")))
	}
	return out
}

// withoutNames is text with each of names, and a name's part before a subtitle
// colon ("Me Before You: A Novel"), replaced by a space (case-insensitively, as
// whole words where a name starts or ends with one), for the voice rules.
func withoutNames(text string, names []string) string {
	for _, name := range names {
		short, _, _ := strings.Cut(name, ":")
		for _, n := range []string{name, short} {
			if n = strings.TrimSpace(n); n == "" {
				continue
			}
			pat := regexp.QuoteMeta(n)
			if r, _ := utf8.DecodeRuneInString(n); isASCIIWordRune(r) {
				pat = `\b` + pat
			}
			if r, _ := utf8.DecodeLastRuneInString(n); isASCIIWordRune(r) {
				pat += `\b`
			}
			text = regexp.MustCompile(`(?i)`+pat).ReplaceAllString(text, " ")
		}
	}
	return text
}

// isASCIIWordRune reports whether r is a word character to RE2's \b.
func isASCIIWordRune(r rune) bool {
	return r == '_' || r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// descriptionNGram runs the no-verbatim check over a composed description against
// the book's source layers, returning the findings and the layers measured.
func descriptionNGram(book store.Book, raw []byte) (findings, layers []string, err error) {
	dir, err := os.MkdirTemp(book.WorkDir, ".describe-check-")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, descriptionFileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return nil, nil, err
	}
	return ngramScan(book, []string{path})
}

// checkedAgainst words the layers a check measured.
func checkedAgainst(layers []string) string {
	switch {
	case len(layers) == 0:
		return CheckedNone
	case slices.Contains(layers, CheckedBookText):
		return CheckedBookText
	default:
		return CheckedTranscripts
	}
}
