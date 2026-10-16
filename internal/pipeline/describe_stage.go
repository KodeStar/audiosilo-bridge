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
// sidecars are final (done, or parked at contributing), called by the Bridge's
// sidecars.generate and sidecars.describe jobs and never by the scheduler, so the
// readers of stage_runs that drive the state machine (reconcile, the supervisor)
// skip it. Its runs count wherever spend is summed (the budgets, the estimates).
const DescribeStage = "describing"

// describeState is DescribeStage where the shared agent driver takes a state.
const describeState = state.State(DescribeStage)

// What the description's no-verbatim check measured against
// (DescribeResult.CheckedAgainst, bridge.json's description.checked_against).
const (
	CheckedTranscripts = "transcripts"
	CheckedBookText    = "book text"
	// CheckedNone: every source layer was gone (the transcript retention purged
	// them), so nothing was checked. The agent never saw them either.
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

// DescribeReport is how the caller hears from the description step.
type DescribeReport struct {
	// Note receives the liveness notes while the agent runs (nil drops them).
	Note func(msg string)
	// NoWait parks a rate limit at once instead of waiting it out in-process:
	// sidecars.generate's description must never hold its job.
	NoWait bool
}

// DescribeResult is a written (or reused) description.
type DescribeResult struct {
	// Path is the composed model.Description in the work dir
	// (FinalDescriptionPath), canonical JSON.
	Path string
	// Chapters is K, how many opening chapters' notes the agent saw;
	// LogicalChapters the book's story-chapter count it was taken from.
	Chapters        int
	LogicalChapters int
	CheckedAgainst  string
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
// *scheduler.ParkError for the caller to handle.
func (e *Executor) Describe(ctx context.Context, bookID int64, report DescribeReport) (DescribeResult, error) {
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
	sel, err := e.openingSelection(book)
	if err != nil {
		return DescribeResult{}, err
	}
	notes, err := buildOpeningNotes(book.WorkDir, sel.Files)
	if err != nil {
		return DescribeResult{}, err
	}
	if notes == "" {
		return DescribeResult{}, fmt.Errorf("%w (no fact notes for the opening chapters)", ErrNoNotes)
	}

	attempt := e.stageAttempt(ctx, book, describeState)
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

	st, err := agent.New(book.WorkDir, DescribeStage, attempt)
	if err == nil {
		err = st.WriteFile(openingNotesName, []byte(notes))
	}
	if err != nil {
		finish(false, map[string]any{"error": err.Error()})
		return DescribeResult{}, fmt.Errorf("describe: stage the notes: %w", err)
	}

	var composed []byte
	var checked string
	validate := func(_ agent.Result, s *agent.Staging) error {
		text, err := readDescriptionOut(s.OutDir())
		if err != nil {
			return err
		}
		if problems := descriptionProblems(text); len(problems) > 0 {
			return fmt.Errorf("out/%s: %s", descriptionFileName, strings.Join(problems, "; "))
		}
		raw, err := composedDescription(chars, text)
		if err != nil {
			return err
		}
		findings, layers, err := descriptionNGram(book, raw)
		if err != nil {
			return fmt.Errorf("the no-verbatim check could not run: %v", err)
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
	if report.NoWait {
		backoff = []time.Duration{}
	}
	usage, err := e.runAgentWith(ctx, book, describeState, scheduler.StageReport{Note: report.Note}, st,
		"description.md", data, false, validate, backoff)
	usageMetrics := usage.metricsMap()
	if err != nil {
		usageMetrics["error"] = err.Error()
		return DescribeResult{CostUSD: finish(false, usageMetrics)}, err
	}
	path := FinalDescriptionPath(book.WorkDir)
	if err := fsutil.WriteFileAtomic(path, composed, 0o644); err != nil {
		usageMetrics["error"] = err.Error()
		return DescribeResult{CostUSD: finish(false, usageMetrics)}, fmt.Errorf("describe: keep the description: %w", err)
	}
	usageMetrics["checked_against"] = checked
	cost := finish(true, usageMetrics)
	return DescribeResult{Path: path, Chapters: sel.K, LogicalChapters: sel.Logical, CheckedAgainst: checked,
		CostUSD: cost, GeneratedAt: time.Now().UTC()}, nil
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
	var d model.Description
	if err := decodeSidecarFile(path, &d); err != nil {
		return DescribeResult{}, false
	}
	run, ok := e.lastDescribeRun(ctx, book.ID)
	if !ok || len(descriptionProblems(d.Text)) > 0 {
		return DescribeResult{}, false
	}
	raw, err := composedDescription(chars, d.Text)
	if err != nil {
		return DescribeResult{}, false
	}
	if cur, err := os.ReadFile(path); err != nil || !bytes.Equal(cur, raw) { //nolint:gosec // the book's work dir
		if err := fsutil.WriteFileAtomic(path, raw, 0o644); err != nil {
			return DescribeResult{}, false
		}
	}
	var m struct {
		Chapters        int    `json:"chapters"`
		LogicalChapters int    `json:"logical_chapters"`
		CheckedAgainst  string `json:"checked_against"`
	}
	_ = json.Unmarshal(run.Metrics, &m)
	at, _ := time.Parse(time.RFC3339Nano, run.FinishedAt)
	return DescribeResult{Path: path, Chapters: m.Chapters, LogicalChapters: m.LogicalChapters,
		CheckedAgainst: cmp.Or(m.CheckedAgainst, CheckedNone), GeneratedAt: at.UTC(), Reused: true}, true
}

// lastDescribeRun is the book's latest successful describing run.
func (e *Executor) lastDescribeRun(ctx context.Context, bookID int64) (store.StageRun, bool) {
	runs, err := e.db.ListStageRuns(ctx, bookID)
	if err != nil {
		return store.StageRun{}, false
	}
	for i := len(runs) - 1; i >= 0; i-- {
		if r := runs[i]; r.Stage == DescribeStage && r.Ok != nil && *r.Ok {
			return r, true
		}
	}
	return store.StageRun{}, false
}

// openingSelection is which audio files' (or ebook chapters') notes the agent
// sees: the files whose LOGICAL position is 0 (front matter) or 1..K, with
// K = max(2, ceil(logical/4)) capped at the logical count.
type openingSelection struct {
	Files   []int // manifest chapter numbers, ascending
	K       int
	Logical int
	// Basis is where the positions came from: "edges" (classified now), "saved"
	// (classified while the transcripts were there), "files" (neither: file
	// numbers, which never sit below their story position, so the bound holds).
	Basis string
}

// openingWindow is K for a book of logical story chapters.
func openingWindow(logical int) int {
	return min(max(2, (logical+3)/4), logical)
}

// openingSelection classifies the book's chapters as the sidecar stages did
// (classifyBookEdges), so the bound is the same logical chapters the recaps use.
// The classification reads the transcripts, which the retention deletes; the
// positions are kept beside the notes each time they are classified, and a book
// whose transcripts are gone uses those, else its file numbers (a file's number
// is never below its story chapter, so that bound is safe, only less generous).
func (e *Executor) openingSelection(book store.Book) (openingSelection, error) {
	m, err := audio.ReadManifest(book.WorkDir)
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
			if err := savePositions(book.WorkDir, class); err != nil {
				e.log.Warn("describe: keeping the chapter positions failed", "book", book.ID, "err", err)
			}
		}
		return selectOpening(chapters, class, "edges"), nil
	default:
		if class, ok := loadPositions(book.WorkDir); ok {
			return selectOpening(chapters, class, "saved"), nil
		}
		return selectOpening(chapters, fileClassification(book.WorkDir, len(chapters)), "files"), nil
	}
}

// selectOpening picks the opening files from a classification. Excluded edge files
// (an Audible intro, closing credits) are never story; end matter has no position.
func selectOpening(chapters []int, class edgeClassification, basis string) openingSelection {
	sel := openingSelection{Logical: class.LogicalCount, K: openingWindow(class.LogicalCount), Basis: basis}
	if sel.Logical < 1 {
		return sel
	}
	excluded := map[int]bool{}
	for _, c := range slices.Concat(class.ExcludedLeading, class.ExcludedTrailing) {
		excluded[c] = true
	}
	for _, c := range chapters {
		if excluded[c] {
			continue
		}
		if pos, ok := class.PositionOf(c); ok && pos >= 0 && pos <= sel.K {
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

// savePositions keeps a classification beside the work dir's other durables (the
// transcript retention leaves it).
func savePositions(workDir string, class edgeClassification) error {
	fp, err := manifestFingerprint(workDir)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(savedPositions{Manifest: fp, LogicalCount: class.LogicalCount,
		Excluded: slices.Concat(class.ExcludedLeading, class.ExcludedTrailing), Offset: class.ChapterOffset,
		Positions: class.Positions})
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(workDir, chapterPositionsName), raw, 0o644)
}

// loadPositions reads the kept classification, when it was made from the
// manifest the work dir holds now.
func loadPositions(workDir string) (edgeClassification, bool) {
	raw, err := os.ReadFile(filepath.Join(workDir, chapterPositionsName)) //nolint:gosec // the book's work dir
	if err != nil {
		return edgeClassification{}, false
	}
	var s savedPositions
	if err := json.Unmarshal(raw, &s); err != nil || s.LogicalCount < 1 {
		return edgeClassification{}, false
	}
	if fp, err := manifestFingerprint(workDir); err != nil || fp != s.Manifest {
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
// copied mechanically from facts/facts-ch*.md with their file numbering kept, in
// chapter order. Nothing else from facts/ (knowledge-final.md, the spelling
// sheets, a chunk's preamble) is copied. "" when none of the files has notes.
func buildOpeningNotes(workDir string, files []int) (string, error) {
	want := map[int]bool{}
	for _, f := range files {
		want[f] = true
	}
	entries, err := os.ReadDir(factsDirPath(workDir))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sections := map[int]string{}
	for _, ent := range entries {
		name := ent.Name()
		if !ent.Type().IsRegular() || !strings.HasPrefix(name, factsChunkPrefix) || !strings.HasSuffix(name, ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(factsDirPath(workDir), name)) //nolint:gosec // the book's work dir
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
// included), each running to the next level-1 or level-2 heading.
func factSections(text string) map[int]string {
	out := map[int]string{}
	cur := 0
	var b strings.Builder
	flush := func() {
		if cur > 0 {
			if s := strings.TrimRight(b.String(), " \t\r\n"); s != "" {
				out[cur] = s + "\n"
			}
		}
		cur = 0
		b.Reset()
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		if sectionBreakRe.MatchString(trimmed) {
			flush()
			if m := factsHeadingRe.FindStringSubmatch(trimmed); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
					cur = n
				}
			}
		}
		if cur > 0 {
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	shape := `write exactly {"text": "..."} and nothing else`
	if err := dec.Decode(&out); err != nil {
		return "", fmt.Errorf("out/%s: %v (%s)", descriptionFileName, err, shape)
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", fmt.Errorf("out/%s has content after the JSON object (%s)", descriptionFileName, shape)
	}
	if out.Text == nil {
		return "", fmt.Errorf("out/%s has no text (%s)", descriptionFileName, shape)
	}
	return strings.TrimSpace(*out.Text), nil
}

// The voice rules a description is held to (AUTHORING.md's `description`
// contract): no second person, no rhetorical question, no sales words.
var (
	secondPersonRe = regexp.MustCompile(`(?i)\b(you|your|yours|yourself|yourselves)\b`)
	// A question mark ending a sentence: followed (after any closing quote or
	// bracket) by a space or the end of the text.
	questionRe       = regexp.MustCompile(`\?["'\x{201D}\x{2019})\]]*(\s|$)`)
	salesWordRe      = regexp.MustCompile(`(?i)\b(gripping|unputdownable|page-turners?|must-reads?|masterpieces?|breathtaking|captivating|riveting|thrilling|unforgettable)\b`)
	paragraphBreakRe = regexp.MustCompile(`\n\s*\n`)
)

// descriptionProblems lists what is wrong with a (trimmed) description text, none
// when it passes. Each is worded for the agent's retry.
func descriptionProblems(text string) []string {
	var out []string
	if strings.ContainsFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) {
		out = append(out, "the text holds a control character (only plain newlines between paragraphs are allowed)")
	}
	if strings.ContainsRune(text, emDash) {
		out = append(out, "the text uses an em dash; use a hyphen or restructure the sentence")
	}
	if n := utf8.RuneCountInString(text); n < descriptionMinRunes || n > descriptionMaxRunes {
		out = append(out, fmt.Sprintf("the text is %d characters; it must be %d to %d (aim for 80 to 150 words)",
			n, descriptionMinRunes, descriptionMaxRunes))
	}
	if n := len(paragraphBreakRe.Split(text, -1)); n > descriptionMaxParagraphs {
		out = append(out, fmt.Sprintf("the text has %d paragraphs; write one, two at most", n))
	}
	if w := secondPersonRe.FindString(text); w != "" {
		out = append(out, fmt.Sprintf("the text addresses the reader (%q); write in the third person", w))
	}
	if questionRe.MatchString(text) {
		out = append(out, "a sentence ends in a question mark; state the premise instead of asking")
	}
	if words := salesWordRe.FindAllString(text, -1); len(words) > 0 {
		out = append(out, fmt.Sprintf("the text uses sales language (%s); keep a neutral reference-guide voice",
			strings.Join(words, ", ")))
	}
	return out
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
