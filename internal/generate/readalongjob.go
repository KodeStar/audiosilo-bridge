package generate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/readalong"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/scratch"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Read-along (Bridge Phase 3c-1). readalong.align lines the book's ebook (the
// server's pick, book.ebook) up with its narration and writes the map as the
// book's .audiosilo/readalong.json (a single-file book's <stem>.readalong.json):
// speech recognition, a mechanical loop repair and the alignment, no agent. The
// engine book runs goal readalong; an earlier run of the same audio is reused
// where it can be (readalongReuse), never a finished whole run cleared.

// ReadalongKind is the read-along job.
const ReadalongKind = "readalong.align"

// readalongName is the map's file in .audiosilo/.
const readalongName = "readalong.json"

// minReadalongCoverage is the share of the text that must line up for a map to be
// written (decision 14): less is another edition, or another book.
const minReadalongCoverage = 0.50

// The read-along job's refusals, in words for the console.
const (
	errNoReadalongEbook   describeFailure = "this book has no ebook"
	errEbookChanged       describeFailure = "the ebook changed while aligning; run it again"
	errReadalongGenerated describeFailure = "A generate job is working on this book; make the read-along once it has finished."
)

// errNoMatch is a map whose text coverage is under minReadalongCoverage.
func errNoMatch(coverage float64) error {
	return describeFailure(fmt.Sprintf("the ebook and the narration don't match: %d%% lined up", int(math.Floor(100*coverage))))
}

// ReadalongJob runs readalong.align. It shares the generate handler's parts: the
// checked mapping, the book's folder, the engine book it follows, its parks, its
// Retry, the retention.
type ReadalongJob struct{ h *Handler }

// ReadalongJob is the readalong.align handler beside this one.
func (h *Handler) ReadalongJob() *ReadalongJob { return &ReadalongJob{h: h} }

// Kind is readalong.align.
func (c *ReadalongJob) Kind() string { return ReadalongKind }

// Lane is the pipeline lane: the job follows an engine book for hours.
func (c *ReadalongJob) Lane() string { return jobs.LanePipeline }

// Abandon releases the engine book of a job the runner ended without running it,
// as a generate job's.
func (c *ReadalongJob) Abandon(ctx context.Context, job jobs.Job) { c.h.Abandon(ctx, job) }

// ReadalongResult is what a done read-along job reports.
type ReadalongResult struct {
	LibraryID   int64              `json:"library_id"`
	Path        string             `json:"path"`
	File        string             `json:"file"`
	Coverage    readalong.Coverage `json:"coverage"`
	Sections    int                `json:"sections"`
	Spans       int                `json:"spans"`
	AudioGaps   int                `json:"audio_gaps"`
	HealWindows int                `json:"heal_windows"`
	Transcribed bool               `json:"transcribed"`
	EbookSHA256 string             `json:"ebook_sha256"`
}

// Run checks the book as a generate job does (strictly: its library mapped and
// writable), then reuses or makes the engine book of goal readalong and follows
// it to done, where the map is written. Its params ask for nothing (the server
// sends {}).
func (c *ReadalongJob) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	h := c.h
	if err := parseObjectParams(job.Params); err != nil {
		return nil, err
	}
	r, stop, err := h.startRun(job, rep, ReadalongKind)
	if err != nil {
		return nil, err
	}
	defer stop()
	r.source = SourceAudio
	eng, err := h.waitEngine(ctx, r)
	if err != nil {
		return nil, err
	}
	mapped, resumed, err := h.resumeMapping(ctx, job)
	if err != nil {
		return nil, err
	}
	return h.runEngineBook(ctx, eng, r, mapped, resumed, nil)
}

// readalongEbook is the book's ebook as the server names it, through the checked
// mapping: its path on the Bridge, a regular file of at most
// readalong.MaxEbookBytes. errNoReadalongEbook when the server names none.
func (r *run) readalongEbook() (string, error) {
	if r.info == nil || r.info.Ebook == nil || strings.TrimSpace(r.info.Ebook.Path) == "" {
		return "", errNoReadalongEbook
	}
	full, err := mapping.SafeJoin(r.mount, r.info.Ebook.Path)
	if err != nil {
		return "", errors.New("the book's ebook leaves the library's folder")
	}
	info, err := os.Stat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("the book's ebook isn't at %s through the library's mapping", r.info.Ebook.Path)
	case err != nil:
		return "", fmt.Errorf("the book's ebook can't be read through the library's mapping (%s)", writeProblem(err))
	case !info.Mode().IsRegular():
		return "", readalong.ErrNotEPUB
	case info.Size() > readalong.MaxEbookBytes:
		return "", readalong.ErrEbookTooBig
	}
	return full, nil
}

// readalongRequest is the engine stage's request for the run: the ebook, the
// server's files relative to the book (a single-file book's by name) in its order.
func (h *Handler) readalongRequest(r *run, ebook string) pipeline.ReadalongRequest {
	req := pipeline.ReadalongRequest{Ebook: ebook, Version: h.o.Version}
	for _, f := range r.files {
		req.Files = append(req.Files, pipeline.ReadalongFile{Path: bookRelPath(f.Path, r.rel, r.place.single), Duration: f.Duration})
	}
	return req
}

// writeReadalongRequest writes the run's request into the engine book's work dir.
func (h *Handler) writeReadalongRequest(r *run, workDir string) error {
	ebook, err := r.readalongEbook()
	if err != nil {
		return err
	}
	if err := pipeline.WriteReadalongRequest(workDir, h.readalongRequest(r, ebook)); err != nil {
		return fmt.Errorf("could not hand the engine the read-along request: %w", err)
	}
	r.readalongAsked = true
	return nil
}

// readalongAsk hands the engine book the run's request once a run (a resumed
// job's too: the server's answer, the ebook and the files, is this run's) and
// again when the server moved the job with its book: the ebook moved with it.
// The stage reads it as it starts, so a move after that fails the stage (the
// ebook isn't where it was), to be run again.
func (h *Handler) readalongAsk(ctx context.Context, r *run, b store.Book) error {
	if cur := r.rep.Path(); cur != "" && cur != r.jobPath && h.adoptMove(ctx, r) {
		h.recordMove(ctx, r)
		r.readalongAsked = false // adoptMove took the move: asked again below, or at a later poll
	}
	if r.readalongAsked {
		return nil
	}
	if err := r.relocate(); err != nil {
		return nil // not there yet as the server says: looked at again at the next poll
	}
	return h.writeReadalongRequest(r, b.WorkDir)
}

// readalongEarlier is what decision 6's table reads of the book's earlier engine
// book.
type readalongEarlier struct {
	Goal, State, Status string
	// Swept: the retention deleted its transcripts. Current: its audio is still
	// the book's (pipeline.AudioCurrent). Continuable: stopped at asr, it can go on
	// transcribing from what it kept (pipeline.TranscriptionContinuable). FactPass:
	// a fact pass ran on it (a read-along that went on from a whole run).
	Swept, Current, Continuable, FactPass bool
}

// readalongReuse is the reuse table (decision 6): where the book's earlier audio
// engine book (never an ebook run: audioRun) goes on for a read-along (to, with
// goal), or ok false for a new one (the earlier cleared). Every row but the last
// needs the transcripts kept and the audio unchanged:
//   - a whole run (goal "") past qa_adjudicating (a finished one an earlier
//     read-along stopped at readalong_aligning too): on at readalong_aligning, its
//     goal kept (its transcripts are repaired already);
//   - a read-along, done or stopped at readalong_aligning: aligned again there;
//   - transcribed but not repaired (a chapters run done; any run stopped from
//     sanitizing to retranscribing): on from sanitizing, goal readalong;
//   - stopped at asr with what it transcribed kept: on from splitting;
//   - a finished whole run (one a read-along took on included) whose transcripts
//     went or whose audio changed: on from inspecting, goal readalong (its notes,
//     knowledge and ledger kept: a whole run is never cleared, errChaptersKeepsRun's
//     rule).
func readalongReuse(e readalongEarlier) (to state.State, goal string, ok bool) {
	st, status := state.State(e.State), state.Status(e.Status)
	if st != state.Done && status != state.StatusFailed && status != state.StatusNeedsAttention && status != state.StatusPaused {
		return "", "", false
	}
	kept := !e.Swept && e.Current
	order := state.Order(st)
	switch {
	case kept && e.Goal == "" && order >= state.Order(state.ReadalongAligning):
		return state.ReadalongAligning, "", true
	case kept && e.Goal == state.GoalReadalong && (st == state.Done || st == state.ReadalongAligning):
		return state.ReadalongAligning, state.GoalReadalong, true
	case kept && e.Goal == state.GoalChapters && st == state.Done,
		kept && st != state.Done && order >= state.Order(state.Sanitizing) && order <= state.Order(state.Retranscribing):
		return state.Sanitizing, state.GoalReadalong, true
	case st == state.ASR && e.Continuable:
		return state.Splitting, state.GoalReadalong, true
	case holdsWholeRun(e.Goal, st, e.FactPass):
		return state.Inspecting, state.GoalReadalong, true
	}
	return "", "", false
}

// holdsWholeRun reports whether an engine book (goal, at st, factPass: a fact pass
// ran on it) holds a finished whole run's notes, knowledge and ledger (a sequel's
// carryover, the description's source): a whole run (goal "") done, or stopped at
// readalong_aligning where a read-along took it on, or a read-along that went on
// from one. Neither a read-along nor a chapters job clears it.
func holdsWholeRun(goal string, st state.State, factPass bool) bool {
	switch goal {
	case "":
		return st == state.Done || st == state.ReadalongAligning
	case state.GoalReadalong:
		return factPass
	}
	return false
}

// readalongFromWholeRun reports whether engine book b is a read-along that went on
// from a whole run: goal readalong with a fact pass's notes behind it. Read from
// the notes on disk, or failing that from any fact pass that ever succeeded on it:
// never from a live success alone, which a Reconcile rewind after a crash (to a
// read-along stage whose sentinel went) supersedes with every later stage's, while
// the notes stay.
func (h *Handler) readalongFromWholeRun(ctx context.Context, b store.Book) (bool, error) {
	if b.Goal != state.GoalReadalong {
		return false, nil
	}
	if pipeline.HoldsNotes(b.WorkDir) {
		return true, nil
	}
	// LatestOkStageRun reads superseded successes too: the history, not scheduling.
	_, err := h.o.DB.LatestOkStageRun(ctx, b.ID, string(state.FactPass))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// keepsWholeRun is holdsWholeRun for engine book b.
func (h *Handler) keepsWholeRun(ctx context.Context, b store.Book) (bool, error) {
	factPass, err := h.readalongFromWholeRun(ctx, b)
	return err == nil && holdsWholeRun(b.Goal, state.State(b.State), factPass), err
}

// readalongPath are the stages a read-along run visits.
var readalongPath = []state.State{state.Inspecting, state.Splitting, state.ASR, state.Sanitizing, state.QASweep,
	state.Retranscribing, state.ReadalongAligning}

// prepareContinue readies held engine book b's work dir to go on at to: the
// sentinels of to (splitting's excepted: a kept one passes a book stopped at asr
// straight on, its FLACs cut again only if a purge took the sentinel too) and of
// the read-along stages after it go, their successes with them, and the
// successes of every other stage whose sentinel a purge took are retired (a
// restart would rewind the book to them: Reconcile; an ebook run's extracting and
// chapter_mapping sort after the read-along's stages). A book going on before
// qa_sweep gets its loop-repair round again; one going on from inspecting forgets
// the manifest its old transcripts were made from.
func (h *Handler) prepareContinue(ctx context.Context, b store.Book, to state.State) error {
	for _, s := range state.All() {
		if !state.IsStage(s) {
			continue
		}
		reset := (s == to && to != state.Splitting) ||
			(slices.Contains(readalongPath, s) && state.Order(s) > state.Order(to))
		purged := !scheduler.SentinelExists(b.WorkDir, string(s))
		if !reset && !purged {
			continue
		}
		if reset {
			if err := os.Remove(scheduler.SentinelPath(b.WorkDir, string(s))); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if err := h.o.DB.SupersedeStageSuccesses(ctx, b.ID, string(s)); err != nil {
			return err
		}
	}
	if to == state.Inspecting {
		if err := pipeline.ForgetTranscription(b.WorkDir); err != nil {
			return err
		}
	}
	if state.Order(to) < state.Order(state.QASweep) {
		return pipeline.ResetReadalongRepair(b.WorkDir)
	}
	return nil
}

// createReadalong makes (or reuses) the engine book of a read-along job. The
// ebook is checked first (none, DRM, not an EPUB, too big: refused before any
// transcription). The earlier engine book is audioRun's (an ebook run is never
// touched: the read-along runs beside it). One another open job follows refuses
// the job; one a stage runs on makes it try again in a minute; otherwise the
// reuse table decides, under the engine's hold, and what it doesn't reuse is
// cleared for a new book (goal readalong, no series: nothing a later volume
// carries on from).
func (h *Handler) createReadalong(ctx context.Context, eng Engine, r *run) error {
	ebook, err := r.readalongEbook()
	if err != nil {
		return err
	}
	if _, err := readalong.OpenEPUB(ebook); err != nil {
		return pipeline.ReadalongEbookErr(err)
	}
	// The map's times are the server's file lengths (decision 10): one it doesn't
	// know would fail the alignment only after the whole transcription.
	for _, f := range r.files {
		if !(f.Duration > 0) {
			return fmt.Errorf("the server has no length for %s, so the read-along can't be timed; rescan the book, then run it again", f.Path)
		}
	}
	sp, ok, err := metaops.AllowedPath(r.place.full, []string{r.mount})
	if err != nil || !ok {
		return errors.New("the book's path leaves the library's folder")
	}
	old, key, err := h.audioRun(ctx, sp)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	default:
		if done, err := h.reuseForReadalong(ctx, eng, r, old); done || err != nil {
			return err
		}
		if err := h.clearEarlier(ctx, eng, r, old.ID); err != nil {
			return err
		}
	}
	nb := h.newBook(r, key, "")
	nb.Goal = state.GoalReadalong
	nb.Series, nb.SeriesPos, nb.ContribMode = "", "", ""
	if err := h.writeReadalongRequest(r, nb.WorkDir); err != nil {
		return err
	}
	return h.queueBook(ctx, eng, r, nb)
}

// audioRun is the engine book a read-along or chapters job of the book at source
// path sp works on, and the source path a new one takes (key): the book's own run,
// unless that is an ebook run, which neither job ever continues, converts nor
// clears (its notes, chapter manifest and kind are the description's and a
// contribution's): then the audio run beside it (store.AudioRunKey), which is
// also the one found when the ebook run went since. store.ErrNotFound (with the
// key) for none.
func (h *Handler) audioRun(ctx context.Context, sp string) (store.Book, string, error) {
	own, err := h.o.DB.GetBookBySourcePath(ctx, sp)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Book{}, "", err
	}
	if err == nil && own.Kind != string(state.KindEbook) {
		return own, sp, nil
	}
	ebookRun := err == nil
	key := store.AudioRunKey(sp)
	beside, err := h.o.DB.GetBookBySourcePath(ctx, key)
	switch {
	case err == nil:
		return beside, key, nil
	case !errors.Is(err, store.ErrNotFound):
		return store.Book{}, "", err
	case !ebookRun:
		// No run at all: a new one is the book's own.
		key = sp
	}
	return store.Book{}, key, store.ErrNotFound
}

// reuseForReadalong continues the book's earlier engine book old for the job when
// the reuse table says so, and reports whether it did; an error refuses the job.
// An open job the server has just cancelled is waited for first (awaitOpenJobEnd,
// BRIDGE-01), as a generate or chapters job does.
func (h *Handler) reuseForReadalong(ctx context.Context, eng Engine, r *run, old store.Book) (bool, error) {
	if id, kind, err := h.awaitOpenJobEnd(ctx, r, old.ID); err != nil {
		return false, err
	} else if id != 0 {
		switch kind {
		case Kind:
			return false, errReadalongGenerated
		case ReadalongKind:
			return false, fmt.Errorf("job %d is already making this book's read-along", id)
		}
		return false, fmt.Errorf("job %d is working on this book; let it finish or cancel it first", id)
	}
	release, err := h.holdBook(ctx, old.ID)
	if errors.Is(err, errBookBusy) {
		return false, errEarlierRunBusy
	} else if err != nil {
		return false, err
	}
	defer release()
	if old, err = h.o.DB.GetBook(ctx, old.ID); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	// The audio out of sight for a moment: the run is kept for the next job,
	// never cleared unseen.
	if _, err := os.Stat(r.place.full); err != nil {
		return false, fmt.Errorf("%w; try again in a minute", errAudioUnseen)
	}
	swept, err := h.o.DB.GenerateBookSwept(ctx, old.ID)
	if err != nil {
		return false, err
	}
	// An audio file that can't be looked at now (not one that went) is a moment's
	// trouble, never "the audio changed": that would purge the kept transcripts.
	current, err := pipeline.AudioCurrent(old.WorkDir, r.place.full)
	if pe := (*fs.PathError)(nil); errors.As(err, &pe) && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("%w; try again in a minute", errAudioUnseen)
	}
	e := readalongEarlier{Goal: old.Goal, State: old.State, Status: old.Status, Swept: swept, Current: current}
	if state.State(old.State) == state.ASR {
		e.Continuable = pipeline.TranscriptionContinuable(old.WorkDir)
	}
	if e.FactPass, err = h.readalongFromWholeRun(ctx, old); err != nil {
		return false, err
	}
	to, goal, ok := readalongReuse(e)
	if !ok {
		return false, nil
	}
	if to == state.Inspecting && !swept {
		// Another audio now: the earlier transcripts are another recording's.
		if err := scratch.PurgeTranscripts(h.o.WorkRoot, old.WorkDir, state.ParseKind(old.Kind)); err != nil {
			return false, fmt.Errorf("could not clear the earlier run's transcripts: %w", err)
		}
	}
	if err := h.prepareContinue(ctx, old, to); err != nil {
		return false, fmt.Errorf("could not ready the book's earlier run: %w", err)
	}
	if err := h.writeReadalongRequest(r, old.WorkDir); err != nil {
		return false, err
	}
	err = h.o.DB.ContinueReadalong(ctx, old.ID, old.State, string(to), goal,
		store.GenerateBook{ServerJobID: r.job.ID, LibraryID: r.lib.ID, RelPath: r.rel})
	if errors.Is(err, store.ErrNotContinuable) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	r.bookID = old.ID
	h.o.Log.Info("readalong: continuing the book's earlier run", "job", r.job.ID, "book", old.ID, "from", old.State, "at", to)
	eng.Notify()
	return true, nil
}

// continueReadalongRun takes over a done read-along run of the same book for a new
// generate job reading the audio (decision 7): on at qa_sweep with goal "", the
// agent review then runs on the transcript the read-along already repaired, which
// is never made again. Only while the run's transcripts are kept and its audio is
// the book's, and never a run transcribed in windows (a chapterless file, or a
// marker table it couldn't use): the whole pipeline would write characters and
// recaps by window, its chapters never found; held throughout, as
// continueChapters does. It reports whether it continued it.
func (h *Handler) continueReadalongRun(ctx context.Context, eng Engine, r *run, old store.Book, ebookRun int64) (bool, error) {
	if r.source != SourceAudio || old.Goal != state.GoalReadalong || state.State(old.State) != state.Done {
		return false, nil
	}
	release, err := h.holdBook(ctx, old.ID)
	if errors.Is(err, errBookBusy) {
		return false, errEarlierRunBusy
	} else if err != nil {
		return false, err
	}
	defer release()
	if old, err = h.o.DB.GetBook(ctx, old.ID); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	} else if old.Goal != state.GoalReadalong || state.State(old.State) != state.Done {
		return false, nil
	}
	if m, err := audio.ReadManifest(old.WorkDir); err != nil || m.Style == audio.StyleWindows {
		return false, nil
	}
	if swept, err := h.o.DB.GenerateBookSwept(ctx, old.ID); err != nil || swept {
		return false, err
	}
	current, err := pipeline.AudioCurrent(old.WorkDir, r.place.full)
	if err != nil {
		if _, serr := os.Stat(r.place.full); serr != nil {
			return false, fmt.Errorf("%w; try again in a minute", errAudioUnseen)
		}
	}
	if !current {
		return false, nil
	}
	if err := h.clearEbookRun(ctx, eng, r, ebookRun); err != nil {
		return false, err
	}
	if err := h.prepareContinue(ctx, old, state.QASweep); err != nil {
		return false, fmt.Errorf("could not ready the book's read-along run: %w", err)
	}
	err = h.o.DB.ContinueChapters(ctx, old.ID, string(state.Done), string(state.QASweep), h.newBook(r, old.AudioSource(), ""),
		store.GenerateBook{ServerJobID: r.job.ID, LibraryID: r.lib.ID, RelPath: r.rel})
	if errors.Is(err, store.ErrNotContinuable) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	r.bookID = old.ID
	h.o.Log.Info("generate: continuing the book's read-along run, its transcription kept", "job", r.job.ID, "book", old.ID)
	eng.Notify()
	return true, nil
}

// finishReadalong writes a done read-along book's map: refused under the coverage
// floor (nothing written) or when the ebook changed since it was aligned (its
// sha256 now against the map's), else saved into .audiosilo/ (parking
// book_not_writable until it can be) and the server asked to read it.
func (h *Handler) finishReadalong(ctx context.Context, r *run, b store.Book) (any, error) {
	r.engineParked(false, "", "")
	raw, m, err := pipeline.ReadalongMap(b.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("the engine kept no read-along map for the book: %w", err)
	}
	if m.Coverage.Text < minReadalongCoverage {
		return nil, errNoMatch(m.Coverage.Text)
	}
	if len(raw) > readalong.MaxMapBytes {
		return nil, readalong.ErrMapTooBig
	}
	if err := checkEbookUnchanged(r, m.Ebook.SHA256); err != nil {
		return nil, err
	}
	extra := map[string]any{"stage": string(state.Done), "source": r.source}
	err = h.save(ctx, r, saveSpec{
		what:     "the read-along",
		wait:     true,
		record:   true,
		progress: jobs.Progress{Step: stepWriting, Message: "Saving the read-along with the book", Extra: extra},
		files: func(place) ([]attachFile, error) {
			return []attachFile{{readalongName, raw}}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	h.o.Log.Info("readalong: wrote the book's read-along", "job", r.job.ID, "path", r.rel, "coverage", m.Coverage.Text)
	r.rep.Progress(jobs.Progress{Step: stepRescanning, Message: "Asking the server to read it", Extra: extra})
	h.rescan(ctx, r)
	res := ReadalongResult{LibraryID: r.lib.ID, Path: r.rel, File: r.place.prefix + readalongName, Coverage: m.Coverage,
		Sections: len(m.Sections), Spans: m.Spans(), AudioGaps: len(m.AudioGaps), EbookSHA256: m.Ebook.SHA256,
		Transcribed: h.transcribedForJob(ctx, r, b.ID)}
	if rep, err := pipeline.ReadReadalongReport(b.WorkDir); err == nil {
		res.HealWindows = rep.HealWindows
	}
	h.doneJobs.Add(1)
	r.rep.Progress(jobs.Progress{Step: string(state.Done), Message: "Finished", Done: 1, Total: 1, Extra: extra})
	return res, nil
}

// checkEbookUnchanged fails the job when the book's ebook (where the server has
// it now: readalongAsk, just before, followed a move) no longer hashes to sum, the
// one the map was aligned from.
func checkEbookUnchanged(r *run, sum string) error {
	ebook, err := r.readalongEbook()
	if errors.Is(err, errNoReadalongEbook) {
		return errNoReadalongEbook
	} else if err != nil {
		return fmt.Errorf("%w (the read-along is kept for the next try)", err)
	}
	n, got, err := fsutil.FileSHA256(ebook)
	if err != nil {
		return fmt.Errorf("the book's ebook can't be read to check it (%s)", writeProblem(err))
	}
	if n > readalong.MaxEbookBytes {
		return readalong.ErrEbookTooBig
	}
	if got != sum {
		return errEbookChanged
	}
	return nil
}

// transcribedForJob reports whether the book's speech recognition ran for this
// job (an asr run that finished after the job took the book), rather than an
// earlier run's transcript being reused.
func (h *Handler) transcribedForJob(ctx context.Context, r *run, bookID int64) bool {
	m, err := h.o.DB.GetGenerateBook(ctx, r.job.ID)
	if err != nil {
		return false
	}
	runs, err := h.o.DB.ListStageRuns(ctx, bookID)
	if err != nil {
		return false
	}
	for _, sr := range runs {
		if sr.Stage == string(state.ASR) && sr.Ok != nil && *sr.Ok && sr.StartedAt >= m.CreatedAt {
			return true
		}
	}
	return false
}
