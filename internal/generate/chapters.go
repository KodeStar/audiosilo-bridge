package generate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Chapters found in the narration (BRIDGE-PLAN.md Phase 1d). A book with no
// chapter markers gets its chapters from the engine's chapter_detecting stage
// (pipeline.DetectedChapters); the jobs write them into the library as the book's
// .audiosilo/chapters.json, which the server reads as a chapter source of its own:
//
//	{"version": 1, "generator": "audiosilo-bridge", "generated_at": "RFC3339",
//	 "from": "narration",
//	 "files": [{"path": "<relative to the book folder>", "size": 123, "duration_ms": 456}],
//	 "chapters": [{"title": "Chapter 1", "start_ms": 0, "length_ms": 1076602}]}
//
// on the whole-book timeline, contiguous from 0 to the end, 2..2000 entries (a
// one-chapter book gets no file).

// ChaptersFile is .audiosilo/chapters.json.
type ChaptersFile struct {
	Version     int                `json:"version"`
	Generator   string             `json:"generator"`
	GeneratedAt string             `json:"generated_at"`
	From        string             `json:"from"`
	Files       []ChaptersFileItem `json:"files"`
	Chapters    []ChaptersItem     `json:"chapters"`
}

// ChaptersFileItem is one of the book's audio files the chapters lie over.
type ChaptersFileItem struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	DurationMs int64  `json:"duration_ms"`
}

// ChaptersItem is one chapter of chapters.json.
type ChaptersItem struct {
	Title    string `json:"title"`
	StartMs  int64  `json:"start_ms"`
	LengthMs int64  `json:"length_ms"`
}

// chaptersFrom is the only source the Bridge writes chapters from.
const chaptersFrom = "narration"

// The server's bounds on chapters.json (more would make it unusable there).
const (
	minFileChapters  = 2
	maxFileChapters  = 2000
	maxChaptersTitle = 200
)

// composeChapters renders chapters.json for the detected list d over the book's
// files as the server lists them (paths made relative to the book's folder rel; a
// single-file book's is the file's name). nil for a list a file isn't written for:
// fewer than two chapters, or more than the server reads.
func composeChapters(d pipeline.DetectedChapters, files []link.BookFile, rel string, single bool, now time.Time) ([]byte, error) {
	if len(d.Chapters) < minFileChapters || len(d.Chapters) > maxFileChapters || len(files) == 0 {
		return nil, nil
	}
	f := ChaptersFile{Version: 1, Generator: "audiosilo-bridge", GeneratedAt: now.UTC().Format(time.RFC3339), From: chaptersFrom}
	for _, bf := range files {
		p := strings.TrimPrefix(bf.Path, strings.TrimSuffix(rel, "/")+"/")
		if single {
			p = path.Base(bf.Path)
		}
		item := ChaptersFileItem{Path: p, Size: bf.Size}
		if len(files) == 1 {
			item.DurationMs = d.DurationMs
		}
		f.Files = append(f.Files, item)
	}
	for i, c := range d.Chapters {
		f.Chapters = append(f.Chapters, ChaptersItem{Title: chapterTitle(c.Title, i+1), StartMs: c.StartMs, LengthMs: c.LengthMs})
	}
	return indentJSON(f)
}

// chapterTitle is a title as the server takes it: control characters out, at most
// maxChaptersTitle bytes (at a character), "Chapter N" for one with none.
func chapterTitle(t string, n int) string {
	t = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, t))
	if len(t) > maxChaptersTitle {
		cut := maxChaptersTitle
		for cut > 0 && !utf8.RuneStart(t[cut]) {
			cut--
		}
		t = strings.TrimSpace(t[:cut])
	}
	if t == "" {
		return "Chapter " + strconv.Itoa(n)
	}
	return t
}

// detectedSum identifies a detected list's content.
func detectedSum(d pipeline.DetectedChapters) string {
	raw, _ := json.Marshal(d)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// refreshChapters composes the run's chapters.json from the engine book's detected
// list, when it has one for its current manifest that wasn't composed yet. It
// reports whether there are chapters to write.
func (h *Handler) refreshChapters(r *run, b store.Book) bool {
	d, err := pipeline.ReadDetectedChapters(b.WorkDir)
	if err != nil || !d.CurrentFor(b.WorkDir) {
		return r.chapters != nil
	}
	if sum := detectedSum(d); sum != r.chaptersSum {
		raw, err := composeChapters(d, r.files, r.rel, r.place.single, h.now())
		if err != nil {
			h.o.Log.Warn("generate: composing chapters.json failed", "job", r.job.ID, "err", err)
			return r.chapters != nil
		}
		r.chapters, r.chaptersSum = raw, sum
		r.chaptersProv = nil
		if raw != nil {
			r.chaptersProv = &ChaptersProvenance{JobID: r.job.ID, GeneratedAt: h.now().UTC().Format(time.RFC3339),
				From: chaptersFrom, Chapters: len(d.Chapters)}
		}
	}
	return r.chapters != nil
}

// saveChapters writes the run's chapters.json and bridge.json (its chapters object
// set, the rest kept) into the book's .audiosilo/, then asks the server to read
// them. wait parks book_not_writable until the save goes through.
func (h *Handler) saveChapters(ctx context.Context, r *run, wait bool, extra map[string]any) error {
	err := h.save(ctx, r, saveSpec{
		what:     "the chapters",
		wait:     wait,
		record:   r.bookID != 0,
		progress: jobs.Progress{Step: stepWriting, Message: "Saving the chapters with the book", Extra: extra},
		files: func(p place) ([]attachFile, error) {
			fresh := provenance(h.o.Version, provKind(r.kind), r.job.ID, h.now(), SourceAudio, "", inputsOf(r.mount, r.files, ""))
			fresh.Description = keptDescription(p.dir, p.prefix)
			fresh.Chapters = r.chaptersProv
			prov, err := mergedProvenance(p.dir, p.prefix, fresh, "chapters", r.chaptersProv)
			if err != nil {
				return nil, err
			}
			return []attachFile{{chaptersName, r.chapters}, {provenanceName, prov}}, nil
		},
	})
	if err != nil {
		return err
	}
	r.chaptersSaved = r.chaptersSum
	h.o.Log.Info("generate: wrote the book's chapters", "job", r.job.ID, "path", r.rel)
	r.rep.Progress(jobs.Progress{Step: stepRescanning, Message: "Asking the server to read them", Extra: extra})
	h.rescan(ctx, r)
	return nil
}

// provKind is a run's job kind as bridge.json names it.
func provKind(kind string) string {
	if kind == "" {
		return Kind
	}
	return kind
}

// saveChaptersEarly writes a generate run's chapters as soon as the engine found
// them (once per detected list; a save that fails is logged and tried at the next
// look), long before the characters and recaps: the listener has chapters while the
// rest is still being written.
func (h *Handler) saveChaptersEarly(ctx context.Context, r *run, b store.Book) {
	if !h.refreshChapters(r, b) || r.chaptersSaved == r.chaptersSum {
		return
	}
	if err := h.saveChapters(ctx, r, false, r.bookExtra(b)); err != nil {
		if msg := err.Error(); msg != r.writeErr {
			r.writeErr = msg
			h.o.Log.Warn("generate: writing the chapters failed; trying again", "job", r.job.ID, "err", err)
		}
	}
}

// continueChapters takes over a finished chapters.detect run of the same book for
// a new generate job reading the audio: its transcription is the expensive part,
// so the book goes on from splitting (store.ContinueChapters) instead of being
// deleted and transcribed again. Only a done chapters book whose transcripts are
// still here (the retention didn't sweep them) and that no open job follows (the
// caller checked). It reports whether it continued it.
func (h *Handler) continueChapters(ctx context.Context, eng Engine, r *run, old store.Book) (bool, error) {
	if old.Goal != state.GoalChapters || state.State(old.State) != state.Done || r.source != SourceAudio {
		return false, nil
	}
	if swept, err := h.o.DB.GenerateBookSwept(ctx, old.ID); err != nil || swept {
		return false, err
	}
	nb := h.newBook(r, old.SourcePath, "")
	if err := h.o.DB.ContinueChapters(ctx, old.ID, nb.ContribMode, nb.WorkID); errors.Is(err, store.ErrNotContinuable) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := h.o.DB.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: r.job.ID, BookID: &old.ID, LibraryID: r.lib.ID, RelPath: r.rel}); err != nil {
		return false, err
	}
	r.bookID = old.ID
	h.o.Log.Info("generate: continuing the book's chapters run, its transcription kept", "job", r.job.ID, "book", old.ID)
	eng.Notify()
	return true, nil
}

// hasChaptersAlready reports an engine failure that says the book has chapters of
// its own (a chapters-only run's inspect).
func hasChaptersAlready(msg string) bool {
	return msg == pipeline.HasMarkerChaptersMsg || msg == pipeline.HasFileChaptersMsg
}

// --- chapters.detect ---

// ChaptersKind is the chapters-only job (Health's no_chapters fix): speech
// recognition and chapter detection, no agent, for a single file with no chapter
// markers; the chapters are written as the book's .audiosilo/chapters.json.
const ChaptersKind = "chapters.detect"

// The ways a chapters job fails that need an admin's words.
const errChaptersGenerating describeFailure = "A generate job is already working on this book; its chapters come with it."

// ChaptersJob runs chapters.detect. It shares the generate handler's parts: the
// checked mapping, the book's folder, the engine book it follows, its parks, its
// Retry, the retention.
type ChaptersJob struct{ h *Handler }

// ChaptersJob is the chapters.detect handler beside this one.
func (h *Handler) ChaptersJob() *ChaptersJob { return &ChaptersJob{h: h} }

// Kind is chapters.detect.
func (c *ChaptersJob) Kind() string { return ChaptersKind }

// Lane is the pipeline lane: the job follows an engine book for hours.
func (c *ChaptersJob) Lane() string { return jobs.LanePipeline }

// Abandon releases the engine book of a job the runner ended without running it,
// as a generate job's.
func (c *ChaptersJob) Abandon(ctx context.Context, job jobs.Job) { c.h.Abandon(ctx, job) }

// ChaptersJobResult is what a done chapters job reports.
type ChaptersJobResult struct {
	LibraryID int64    `json:"library_id"`
	Path      string   `json:"path"`
	Chapters  int      `json:"chapters"`
	Files     []string `json:"files"`
	Source    string   `json:"source"`
}

// ParseChaptersParams checks a chapters job's params: nothing is asked for (the
// server sends {}), and a field a later server adds is ignored.
func ParseChaptersParams(raw json.RawMessage) error { return parseObjectParams(raw) }

// Run checks the book as a generate job does, then writes the chapters the engine
// already found for it, or runs an engine book with goal chapters and follows it
// to done.
func (c *ChaptersJob) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	h := c.h
	if err := ParseChaptersParams(job.Params); err != nil {
		return nil, err
	}
	r, stop, err := h.startRun(job, rep, ChaptersKind)
	if err != nil {
		return nil, err
	}
	defer stop()
	r.kind, r.source = ChaptersKind, SourceAudio
	eng, err := h.waitEngine(ctx, r)
	if err != nil {
		return nil, err
	}
	mapped, merr := h.o.DB.GetGenerateBook(ctx, job.ID)
	if merr != nil && !errors.Is(merr, store.ErrNotFound) {
		return nil, merr
	}
	// As a generate job's: the mapping is this job's own only for the same library
	// and book (server ids repeat across servers).
	resumed := merr == nil && mapped.BookID != nil && mapped.LibraryID == *job.LibraryID
	ownBook := func() bool { return mapped.RelPath == r.rel || mapped.RelPath == *job.Path }
	r.resuming = resumed
	if err := h.prepare(ctx, r, !resumed); err != nil {
		if resumed && ownBook() {
			r.bookID = *mapped.BookID
		}
		return h.end(ctx, r, nil, err)
	}
	if resumed && !ownBook() {
		h.stopBook(context.WithoutCancel(ctx), job.ID, *mapped.BookID)
		resumed = false
	}
	if resumed {
		r.bookID = *mapped.BookID
		if err := h.attach(ctx, eng, r, mapped); err != nil {
			return h.end(ctx, r, nil, err)
		}
	} else {
		res, done, err := h.chaptersFromEarlier(ctx, r)
		if done || err != nil {
			return res, err
		}
		if err := h.create(ctx, eng, r); err != nil {
			return h.end(ctx, r, nil, err)
		}
	}
	res, err := h.follow(ctx, eng, r)
	return h.end(ctx, r, res, err)
}

// chaptersFromEarlier writes the chapters an earlier run here already found for the
// book (an engine book at its source path whose detected list belongs to its
// manifest now), with no new run; done says it did. A book an open generate job
// follows is refused: that run brings the chapters.
func (h *Handler) chaptersFromEarlier(ctx context.Context, r *run) (any, bool, error) {
	old, err := h.bookAtSource(ctx, r)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if err := h.openJobRefusal(ctx, r, old.ID); err != nil {
		return nil, true, err
	}
	d, err := pipeline.ReadDetectedChapters(old.WorkDir)
	if err != nil || !d.CurrentFor(old.WorkDir) {
		return nil, false, nil
	}
	h.o.Log.Info("chapters: an earlier run found the book's chapters; writing them", "job", r.job.ID, "book", old.ID)
	res, err := h.writeDetected(ctx, r, old)
	return res, true, err
}

// openJobRefusal refuses a chapters job for an engine book another open job
// follows: a generate job's run brings the chapters; another chapters job is
// already finding them.
func (h *Handler) openJobRefusal(ctx context.Context, r *run, bookID int64) error {
	open, err := h.o.DB.OpenGenerateJobsForBook(ctx, bookID, r.job.ID)
	if err != nil || len(open) == 0 {
		return err
	}
	if sj, err := h.o.DB.GetServerJob(ctx, open[0]); err == nil && sj.Kind == Kind {
		return errChaptersGenerating
	}
	return fmt.Errorf("job %d is already finding this book's chapters; cancel it first", open[0])
}

// createChapters makes the engine book of a chapters job: goal chapters, the
// audio, no contribution. An earlier engine book at the same source path with
// nothing to reuse is deleted first (as a generate job does), unless an open job
// follows it.
func (h *Handler) createChapters(ctx context.Context, eng Engine, r *run) error {
	sp, ok, err := metaops.AllowedPath(r.place.full, []string{r.mount})
	if err != nil || !ok {
		return errors.New("the book's path leaves the library's folder")
	}
	if old, err := h.o.DB.GetBookBySourcePath(ctx, sp); err == nil {
		if err := h.openJobRefusal(ctx, r, old.ID); err != nil {
			return err
		}
		if err := h.deleteEarlier(ctx, eng, r, old.ID); errors.Is(err, scheduler.ErrBusy) {
			return errors.New("the engine is still busy with this book's earlier run; try again in a minute")
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("could not clear the book's earlier run: %w", err)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	nb := h.newBook(r, sp, "")
	nb.Goal = state.GoalChapters
	batch, err := h.o.DB.CreateBatch(ctx)
	if err != nil {
		return err
	}
	nb.BatchID = batch.ID
	b, err := h.o.DB.CreateBook(ctx, nb)
	if err != nil {
		return fmt.Errorf("could not queue the book in the engine: %w", err)
	}
	if err := h.o.DB.PutGenerateBook(ctx, store.GenerateBook{ServerJobID: r.job.ID, BookID: &b.ID, LibraryID: r.lib.ID, RelPath: r.rel}); err != nil {
		_ = h.o.DB.DeleteBook(context.WithoutCancel(ctx), b.ID)
		return err
	}
	r.bookID = b.ID
	h.o.Log.Info("chapters: queued in the engine", "job", r.job.ID, "book", b.ID, "path", r.rel)
	eng.Notify()
	return nil
}

// finishChapters writes a done chapters book's chapters.
func (h *Handler) finishChapters(ctx context.Context, r *run, b store.Book) (any, error) {
	r.engineParked(false, "", "")
	return h.writeDetected(ctx, r, b)
}

// writeDetected writes the engine book b's detected chapters into the library
// (parking book_not_writable until it can) and composes the result.
func (h *Handler) writeDetected(ctx context.Context, r *run, b store.Book) (any, error) {
	d, err := pipeline.ReadDetectedChapters(b.WorkDir)
	if err != nil || !d.CurrentFor(b.WorkDir) {
		return nil, errors.New("the engine kept no chapters for the book")
	}
	h.refreshChapters(r, b)
	extra := map[string]any{"stage": string(state.Done), "source": r.source}
	files := []string{}
	if r.chapters != nil {
		if err := h.saveChapters(ctx, r, true, extra); err != nil {
			return nil, err
		}
		files = attachmentNames(r.place.prefix, sidecars{chapters: r.chapters})
	}
	r.rep.Progress(jobs.Progress{Step: string(state.Done), Message: "Finished", Done: 1, Total: 1, Extra: extra})
	return ChaptersJobResult{LibraryID: r.lib.ID, Path: r.rel, Chapters: len(d.Chapters), Files: files, Source: SourceAudio}, nil
}
