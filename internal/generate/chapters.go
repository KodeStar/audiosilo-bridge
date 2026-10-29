package generate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/audio"
	"github.com/kodestar/audiosilo-bridge/internal/chapterdetect"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
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
	minFileChapters = 2
	maxFileChapters = 2000
)

// writesFile reports whether a file is written for the detected list d over the
// book's files: two chapters at least, no more than the server reads, and files to
// lay them over.
func writesFile(d pipeline.DetectedChapters, files []link.BookFile) bool {
	return len(d.Chapters) >= minFileChapters && len(d.Chapters) <= maxFileChapters && len(files) > 0
}

// bookRelPath is a server file path p as the book's sidecars name it: relative to
// the book's folder rel, or a single-file book's file name.
func bookRelPath(p, rel string, single bool) string {
	if single {
		return path.Base(p)
	}
	return strings.TrimPrefix(p, strings.TrimSuffix(rel, "/")+"/")
}

// composeChapters renders chapters.json for the detected list d over the book's
// files as the server lists them (paths made relative to the book's folder rel; a
// single-file book's is the file's name). nil for a list a file isn't written for
// (writesFile).
func composeChapters(d pipeline.DetectedChapters, files []link.BookFile, rel string, single bool, now time.Time) ([]byte, error) {
	if !writesFile(d, files) {
		return nil, nil
	}
	f := ChaptersFile{Version: 1, Generator: "audiosilo-bridge", GeneratedAt: now.UTC().Format(time.RFC3339), From: chaptersFrom}
	for _, bf := range files {
		item := ChaptersFileItem{Path: bookRelPath(bf.Path, rel, single), Size: bf.Size}
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
// chapterdetect.MaxTitleBytes (at a character), "Chapter N" for one with none.
func chapterTitle(t string, n int) string {
	t = chapterdetect.ClipTitle(strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, t)))
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

// markersFile is the one audio file of the book at path (a file, or a folder
// holding one), "" when it isn't a one-file book or can't be looked at.
func markersFile(path string) string {
	if src, err := audio.ResolveSource(path); err == nil && src.Style == audio.StyleMarkers {
		return src.BookFile
	}
	return ""
}

// bookFileNow is the book's one audio file where the job has it now: at the path
// the server's last answer moved the job to (adoptMove takes it up at the next
// save, after the chapters were checked), else at the run's place; "" when that
// can't be told.
func (r *run) bookFileNow() string {
	full := r.place.full
	if r.rep != nil {
		if cur := r.rep.Path(); cur != "" && cur != r.jobPath {
			if p, err := locate(r.mount, cur); err == nil {
				full = p.full
			}
		}
	}
	return markersFile(full)
}

// errAudioUnseen: the book's audio can't be looked at to tell whether the
// chapters found are still its own.
var errAudioUnseen = errors.New("the Bridge can't look at the book's audio file now to check the chapters found against it")

// awaitChapters is refreshChapters for a write that must not go without the run's
// chapters only because their audio can't be looked at now (a mount gone for a
// moment): with wait (the book is done) it parks book_not_writable and looks
// again every Recheck (or on Retry) until it can, as a save that can't write
// does, lifting its own park once it can; without, it returns errAudioUnseen for
// the caller to try again later.
func (h *Handler) awaitChapters(ctx context.Context, r *run, b store.Book, wait bool) (pipeline.DetectedChapters, bool, error) {
	parked := false
	for {
		d, ok := h.refreshChapters(r, b)
		if !r.detectedUnseen {
			if parked && r.ownPark {
				r.rep.Unpark()
				r.ownPark = false
			}
			return d, ok, nil
		}
		if !wait {
			return d, false, errAudioUnseen
		}
		h.park(r, parkBookNotWritable, errAudioUnseen.Error())
		parked = true
		if err := h.recheckWait(ctx, r, time.Now().Add(h.o.Recheck)); err != nil {
			return d, false, err
		}
	}
}

// refreshChapters reads the engine book's detected list when it has one for its
// current manifest, notes it for the run's chapters.json (chaptersJSON composes the
// file at each write) and returns the list and whether it is current. A generate
// job looks at every poll, so the list is read and checked again only when its
// files changed since the last look (pipeline.DetectedStamp: local stats; no list
// yet is one stat; a list whose audio can't be looked at is not current,
// r.detectedUnseen says so, and it is checked again at the next look).
func (h *Handler) refreshChapters(r *run, b store.Book) (pipeline.DetectedChapters, bool) {
	stamp, ok := pipeline.DetectedStamp(b.WorkDir)
	if !ok {
		r.detected, r.detectedOK, r.detectedUnseen, r.detectedStamp = pipeline.DetectedChapters{}, false, false, ""
		return pipeline.DetectedChapters{}, false
	}
	if stamp != r.detectedStamp {
		d, err := pipeline.ReadDetectedChapters(b.WorkDir)
		if err != nil {
			// A list that can't be read is none: an earlier one noted is never
			// written in its place.
			r.detectedStamp, r.detected, r.detectedOK, r.detectedUnseen = stamp, pipeline.DetectedChapters{}, false, false
			return pipeline.DetectedChapters{}, false
		}
		// The audio where the job finds it now (the server moves a job with its
		// book); the manifest's path when that can't be told.
		current, err := d.CheckCurrent(b.WorkDir, r.bookFileNow())
		if err != nil {
			// The audio can't be looked at now (a mount gone for a moment): not
			// current, and asked again at the next look rather than kept.
			stamp = ""
		}
		r.detectedStamp, r.detected, r.detectedOK, r.detectedUnseen = stamp, d, current, err != nil
	}
	if !r.detectedOK {
		return r.detected, false
	}
	if sum := detectedSum(r.detected); sum != r.chaptersSum {
		r.chaptersSum, r.chaptersAt, r.chaptersProv = sum, h.now(), nil
		if writesFile(r.detected, r.files) {
			r.chaptersProv = &ChaptersProvenance{JobID: r.job.ID, GeneratedAt: r.chaptersAt.UTC().Format(time.RFC3339),
				From: chaptersFrom, Chapters: len(r.detected.Chapters)}
		}
	}
	return r.detected, true
}

// chaptersJSON is the run's chapters.json for the book as it is now (its files and
// path: the server may have moved the job with its book since the list was found),
// composed from the detected list refreshChapters last found current, at the time
// the run first noted it, so an unchanged book composes the same bytes; nil for none
// to write (no current list, or one a file isn't written for). single is the book's
// place's (a single-file book names its file).
func (r *run) chaptersJSON(single bool) ([]byte, error) {
	if !r.detectedOK || r.chaptersProv == nil {
		return nil, nil
	}
	return composeChapters(r.detected, r.files, r.rel, single, r.chaptersAt)
}

// saveChapters writes the run's chapters.json and bridge.json (its chapters object
// set, the rest kept) into the book's .audiosilo/, then asks the server to read
// them. wait parks book_not_writable until the save goes through; record records a
// move in the job's mapping (a job writing an earlier run's chapters has none).
func (h *Handler) saveChapters(ctx context.Context, r *run, wait, record bool, extra map[string]any) error {
	err := h.save(ctx, r, saveSpec{
		what:     "the chapters",
		wait:     wait,
		record:   record,
		progress: jobs.Progress{Step: stepWriting, Message: "Saving the chapters with the book", Extra: extra},
		files: func(p place) ([]attachFile, error) {
			raw, err := r.chaptersJSON(p.single)
			if err != nil {
				return nil, err
			}
			fresh := provenance(h.o.Version, r.kind, r.job.ID, h.now(), SourceAudio, "", inputsOf(r.mount, r.files, ""))
			fresh.Chapters = r.chaptersProv
			prov, err := mergedProvenance(p.dir, p.prefix, fresh, "chapters")
			if err != nil {
				return nil, err
			}
			return []attachFile{{chaptersName, raw}, {provenanceName, prov}}, nil
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

// saveChaptersEarly writes a generate run's chapters as soon as the engine found
// them (once per detected list), long before the characters and recaps: the
// listener has chapters while the rest is still being written. It never waits, as
// it runs on every look at the engine book: not while the library isn't checked
// mapped and writable (the finish writes them with the rest), and a save that fails
// is logged and tried again a Recheck later. Never beside an earlier run's
// characters and recaps made without chapters found in the narration
// (sidecarsWithoutChapters): the server would gate their positions by this list
// until the finish replaces them. It reports whether it tried a save (the stage is
// then reported again).
func (h *Handler) saveChaptersEarly(ctx context.Context, r *run, b store.Book) bool {
	// Only a list current now: a noted one that went stale, or whose audio can't be
	// looked at, would save bridge.json's chapters object without its file.
	if _, ok := h.refreshChapters(r, b); !ok || r.chaptersProv == nil || r.chaptersSaved == r.chaptersSum ||
		h.now().Before(r.chaptersRetryAt) || !h.mappedNow(r) {
		return false
	}
	if p, err := locate(r.mount, r.rel); err != nil || sidecarsWithoutChapters(p) {
		r.chaptersRetryAt = h.now().Add(h.o.Recheck)
		return false
	}
	err := h.saveChapters(ctx, r, false, true, r.bookExtra(b))
	if err != nil {
		r.chaptersRetryAt = h.now().Add(h.o.Recheck)
		if msg := err.Error(); msg != r.writeErr {
			r.writeErr = msg
			h.o.Log.Warn("generate: writing the chapters failed; trying again later", "job", r.job.ID, "err", err)
		}
	}
	return true
}

// mappedNow reports whether the job's library is checked mapped and writable at the
// mount the Bridge uses now (waitMapped's condition, without its wait).
func (h *Handler) mappedNow(r *run) bool {
	mount, _ := h.o.Mount(r.lib.ID, r.lib.Root)
	st, checked := h.o.Libraries.Status(r.lib.ID)
	return checked && st.Mapped && st.Writable && st.MountPath == mount
}

// sidecarsWithoutChapters reports whether the book's .audiosilo/ (at p) holds
// characters or recaps that were made without chapters found in the narration: its
// bridge.json carries no chapters object (or there is none). Their reveal and recap
// positions belong to another chapter list (a run of the file as one chapter, the
// ebook's chapters), so a chapters.json written beside them alone would make the
// server gate them by this one: a whole-book recap of a one-chapter run opens with
// the second chapter.
func sidecarsWithoutChapters(p place) bool {
	adir := filepath.Join(p.dir, AttachDir)
	has := false
	for _, name := range []string{charactersName, recapsName} {
		if info, err := os.Lstat(filepath.Join(adir, p.prefix+name)); err == nil && info.Mode().IsRegular() {
			has = true
		}
	}
	if !has {
		return false
	}
	raw, ok := readProvenance(adir, p.prefix+provenanceName)["chapters"]
	return !ok || string(raw) == "null"
}

// continueChapters takes over a chapters.detect run of the same book for a new
// generate job reading the audio, so its transcription (the expensive part) is
// kept instead of deleted and done again (store.ContinueChapters). Two runs
// continue (continuable):
//   - a done one whose detected list is current for the audio on disk (a file
//     replaced since is transcribed again) goes on from splitting, which cuts the
//     chapter FLACs the done-time purge removed; asr then finds every transcript
//     complete, so the book goes on to sanitizing and the rest;
//   - one cancelled or failed at chapter_detecting (the server cancels a parked
//     chapters job as it queues a generate job for the book) whose kept window
//     layer can run the stage again (pipeline.DetectionRerunnable) goes on from
//     there, the stage running again over the kept windows;
//   - one cancelled or failed at asr (a chapters job parked asr_worker_waiting,
//     cancelled the same way) whose windows manifest is in place for the file
//     inspected (pipeline.TranscriptionContinuable) goes on from splitting (its
//     kept sentinel passes it straight on; a purge that took the window FLACs
//     took that too, so they are cut again), and asr transcribes only the
//     windows it hadn't finished.
//
// Either only while the book is still the one file it was (a folder that has
// gained an audio file is a multi-file book now), the retention hasn't swept its
// transcripts and no open job follows it (the caller checked). It is held
// throughout, waiting a few polls for the hold (a stage the cancel just stopped
// may still be finishing; one that stays busy fails the job to be tried again,
// rather than the run being deleted unseen) and looked at again once held, so no
// purge or stage starts on it in between; the identity swap, the cleared status
// and the job's mapping are one transaction, on the job's context (a job
// cancelled meanwhile leaves the run as it was, for the next one): a continued
// book always has its job to follow it and is never left stopped. It reports
// whether it continued it.
func (h *Handler) continueChapters(ctx context.Context, eng Engine, r *run, old store.Book) (bool, error) {
	if r.source != SourceAudio || !continuable(old) {
		return false, nil
	}
	bookFile := markersFile(old.AudioSource())
	if bookFile == "" {
		return false, nil
	}
	release, err := h.holdBook(ctx, old.ID)
	if errors.Is(err, errBookBusy) {
		return false, errEarlierRunBusy
	} else if err != nil {
		return false, err
	}
	defer release()
	// The stage the cancel stopped may have finished first (done now).
	if old, err = h.o.DB.GetBook(ctx, old.ID); errors.Is(err, store.ErrNotFound) || (err == nil && !continuable(old)) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	to := state.Splitting
	switch from := state.State(old.State); from {
	case state.ChapterDetecting:
		if !pipeline.DetectionRerunnable(old.WorkDir) {
			return false, nil
		}
		to = from
	case state.ASR:
		if !pipeline.TranscriptionContinuable(old.WorkDir) {
			return false, nil
		}
	default:
		// The run's own file, the one the continued run cuts the chapters from (a
		// file renamed in the book's folder since is a new run's to read).
		if m, err := audio.ReadManifest(old.WorkDir); err != nil || m.Source != bookFile {
			return false, nil
		}
		d, err := pipeline.ReadDetectedChapters(old.WorkDir)
		if err != nil {
			return false, nil
		}
		if current, err := d.CheckCurrent(old.WorkDir, bookFile); err != nil {
			// The audio out of sight for a moment: the run is kept for the next
			// job, never cleared unseen.
			return false, fmt.Errorf("%w; try again in a minute", errAudioUnseen)
		} else if !current {
			return false, nil
		}
	}
	err = h.o.DB.ContinueChapters(ctx, old.ID, old.State, string(to), h.newBook(r, old.AudioSource(), ""),
		store.GenerateBook{ServerJobID: r.job.ID, LibraryID: r.lib.ID, RelPath: r.rel})
	if errors.Is(err, store.ErrNotContinuable) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	r.bookID = old.ID
	h.o.Log.Info("generate: continuing the book's chapters run, its transcription kept", "job", r.job.ID, "book", old.ID,
		"from", old.State)
	eng.Notify()
	return true, nil
}

// continuable reports whether engine book b is a chapters run a generate job can
// go on with: goal chapters, and done, or failed (a cancel included) at asr or
// chapter_detecting.
func continuable(b store.Book) bool {
	switch state.State(b.State) {
	case state.Done:
		return b.Goal == state.GoalChapters
	case state.ASR, state.ChapterDetecting:
		return b.Goal == state.GoalChapters && state.Status(b.Status) == state.StatusFailed
	default:
		return false
	}
}

// awaitChaptersGivingWay waits up to holdTries polls for the book's chapters run
// to give way to this generate job (chaptersGivingWay): the server queues a
// generate job beside a chapters.detect job only once that one is cancelled (a
// parked one it cancels as it queues it; a working one an admin cancelled), the
// Bridge hears it at that job's next report, and the job's end stops the book
// (continueChapters takes it on from there). A run no job follows that still
// isn't stopped when the wait runs out (its job ended without stopping it) is
// stopped here, as that end would have. It returns the book as it is then and
// the first other open job following it (openID 0 when none, kind "" when
// unknown), which the caller refuses.
func (h *Handler) awaitChaptersGivingWay(ctx context.Context, r *run, b store.Book) (_ store.Book, openID int64, kind string, err error) {
	for i := 1; ; i++ {
		if openID, kind, err = h.firstOpenJob(ctx, b.ID, r.job.ID); err != nil {
			return b, 0, "", err
		}
		if !chaptersGivingWay(b, openID, kind) {
			return b, openID, kind, nil
		}
		if i == holdTries {
			if openID == 0 {
				h.stopBook(context.WithoutCancel(ctx), r.job.ID, b.ID)
				b, err = h.o.DB.GetBook(ctx, b.ID)
			}
			return b, openID, kind, err
		}
		r.rep.Progress(jobs.Progress{Step: stepWaiting, Message: "Waiting for the book's chapters job to give way"})
		if err := agent.SleepCtx(ctx, h.o.Poll); err != nil {
			return b, 0, "", err
		}
		if b, err = h.o.DB.GetBook(ctx, b.ID); err != nil {
			return b, 0, "", err
		}
	}
}

// chaptersGivingWay reports whether book b is a chapters run on its way to giving
// way: its chapters job still open (whatever the book is doing: working, parked,
// or done and saving its list, the job is cancelled and ends at its next report),
// or with no job following it and not stopped yet (that job's end is stopping it).
func chaptersGivingWay(b store.Book, openID int64, kind string) bool {
	switch {
	case b.Goal != state.GoalChapters:
		return false
	case openID != 0:
		return kind == ChaptersKind
	default:
		return !finished(b.State, b.Status)
	}
}

// firstOpenJob is the first open job other than exceptJob following engine book
// bookID and its kind: id 0 when there is none, kind "" when the job can't be read.
func (h *Handler) firstOpenJob(ctx context.Context, bookID, exceptJob int64) (id int64, kind string, err error) {
	open, err := h.o.DB.OpenGenerateJobsForBook(ctx, bookID, exceptJob)
	if err != nil || len(open) == 0 {
		return 0, "", err
	}
	if sj, err := h.o.DB.GetServerJob(ctx, open[0]); err == nil {
		kind = sj.Kind
	}
	return open[0], kind, nil
}

// hasChaptersFailure reports, with its sentence, that the engine book's last
// inspecting run refused the chapters-only run (pipeline.ChaptersRefusals: the book
// has chapters of its own, or a file whose chapters can't be found in the
// narration). It reads the run's own error, which the supervisor never rewrites (an
// escalation parks the book with a message of its own); the engine keeps no failure
// code beside it.
func (h *Handler) hasChaptersFailure(ctx context.Context, bookID int64) (string, bool) {
	runs, err := h.o.DB.ListStageRuns(ctx, bookID)
	if err != nil {
		return "", false
	}
	for i := len(runs) - 1; i >= 0; i-- {
		sr := runs[i]
		if sr.Stage != string(state.Inspecting) || sr.Ok == nil {
			continue
		}
		var m struct {
			Error string `json:"error"`
		}
		if *sr.Ok || json.Unmarshal(sr.Metrics, &m) != nil {
			return "", false
		}
		for _, e := range pipeline.ChaptersRefusals {
			if m.Error == e.Error() {
				return m.Error, true
			}
		}
		return "", false
	}
	return "", false
}

// --- chapters.detect ---

// ChaptersKind is the chapters-only job (Health's no_chapters fix): speech
// recognition and chapter detection, no agent, for a single file with no chapter
// markers; the chapters are written as the book's .audiosilo/chapters.json.
const ChaptersKind = "chapters.detect"

// The ways a chapters job fails that need an admin's words.
const (
	errChaptersGenerating describeFailure = "A generate job is already working on this book; its chapters come with it."
	// errChaptersBesideSidecars: the book's characters and recaps were made without
	// these chapters (sidecarsWithoutChapters), and a chapters.json beside them would
	// move their spoiler positions.
	errChaptersBesideSidecars describeFailure = "This book's characters and recaps were made without chapters found in the narration, " +
		"and new chapters would move their spoiler positions. Generate the book again to find its chapters with them."
	// errChaptersKeepsRun: a finished whole run of the book is the engine's (its
	// notes carry a series on and describe the book), never cleared for chapters.
	errChaptersKeepsRun describeFailure = "The Bridge already generated this book, and a chapters run would replace that run. " +
		"Generate the book again to find its chapters with its characters and recaps."
)

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

// Run checks the book as a generate job does, then writes the chapters the engine
// already found for it, or runs an engine book with goal chapters and follows it
// to done. Its params ask for nothing (the server sends {}).
func (c *ChaptersJob) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	h := c.h
	if err := parseObjectParams(job.Params); err != nil {
		return nil, err
	}
	r, stop, err := h.startRun(job, rep, ChaptersKind)
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
	return h.runEngineBook(ctx, eng, r, mapped, resumed, func() (any, bool, error) { return h.chaptersFromEarlier(ctx, r) })
}

// chaptersFromEarlier writes the chapters an earlier run here already found for the
// book (an engine book at its source path whose detected list belongs to its
// manifest now), with no new run; done says it did. A book an open generate job
// follows is refused: that run brings the chapters; so is one whose characters and
// recaps were made without chapters found in the narration (before any run).
func (h *Handler) chaptersFromEarlier(ctx context.Context, r *run) (any, bool, error) {
	if sidecarsWithoutChapters(r.place) {
		return nil, true, errChaptersBesideSidecars
	}
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
	// A list whose audio can't be looked at now fails the job rather than going on
	// to a new run, which would clear this one unseen.
	if _, ok, err := h.awaitChapters(ctx, r, old, false); err != nil {
		return nil, true, fmt.Errorf("%w; try again in a minute", err)
	} else if !ok {
		return nil, false, nil
	}
	h.o.Log.Info("chapters: an earlier run found the book's chapters; writing them", "job", r.job.ID, "book", old.ID)
	// The earlier run's book is the job's to report on from here: a park of the
	// job's own (the book's folder not writable) is lifted by the save that goes
	// through, never between two tries (unparkOwn), which the server would hear as a
	// new park each time. It records no move: the job has no mapping of its own.
	r.bookID = old.ID
	res, err := h.writeDetected(ctx, r, old, false)
	return res, true, err
}

// openJobRefusal refuses a chapters job for an engine book another open job
// follows: a generate job's run brings the chapters; another chapters job is
// already finding them.
func (h *Handler) openJobRefusal(ctx context.Context, r *run, bookID int64) error {
	id, kind, err := h.firstOpenJob(ctx, bookID, r.job.ID)
	if err != nil || id == 0 {
		return err
	}
	switch kind {
	case Kind:
		return errChaptersGenerating
	case ReadalongKind:
		return fmt.Errorf("job %d is making this book's read-along; let it finish or cancel it first", id)
	}
	return fmt.Errorf("job %d is already finding this book's chapters; cancel it first", id)
}

// createChapters makes the engine book of a chapters job: goal chapters, the
// audio, no contribution, and no series (a chapters-only run makes nothing a later
// volume carries on from, so it must not hold the series' lock; a generate job that
// continues it gives it the book's series). An earlier engine book at the same source
// path with nothing to reuse is deleted first (as a generate job does), unless an
// open job follows it or it is a finished whole run (errChaptersKeepsRun).
func (h *Handler) createChapters(ctx context.Context, eng Engine, r *run) error {
	sp, ok, err := metaops.AllowedPath(r.place.full, []string{r.mount})
	if err != nil || !ok {
		return errors.New("the book's path leaves the library's folder")
	}
	old, key, err := h.audioRun(ctx, sp)
	if err == nil {
		if err := h.openJobRefusal(ctx, r, old.ID); err != nil {
			return err
		}
		if keeps, err := h.keepsWholeRun(ctx, old); err != nil {
			return err
		} else if keeps {
			return errChaptersKeepsRun
		}
		if err := h.clearEarlier(ctx, eng, r, old.ID); err != nil {
			return err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	nb := h.newBook(r, key, "")
	nb.Goal = state.GoalChapters
	nb.Series, nb.SeriesPos = "", ""
	return h.queueBook(ctx, eng, r, nb)
}

// finishChapters writes a done chapters book's chapters.
func (h *Handler) finishChapters(ctx context.Context, r *run, b store.Book) (any, error) {
	r.engineParked(false, "", "")
	return h.writeDetected(ctx, r, b, true)
}

// writeDetected writes the engine book b's detected chapters into the library
// (parking book_not_writable until it can) and composes the result; record records
// a move in the job's mapping. Never beside characters and recaps made without
// chapters found in the narration (errChaptersBesideSidecars).
func (h *Handler) writeDetected(ctx context.Context, r *run, b store.Book, record bool) (any, error) {
	d, ok, err := h.awaitChapters(ctx, r, b, true)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("the engine kept no chapters for the book")
	}
	extra := map[string]any{"stage": string(state.Done), "source": r.source}
	files := []string{}
	if r.chaptersProv != nil {
		if p, err := locate(r.mount, r.rel); err == nil && sidecarsWithoutChapters(p) {
			return nil, errChaptersBesideSidecars
		}
		if err := h.saveChapters(ctx, r, true, record, extra); err != nil {
			return nil, err
		}
		files = []string{r.place.prefix + chaptersName, r.place.prefix + provenanceName}
	}
	r.rep.Progress(jobs.Progress{Step: string(state.Done), Message: "Finished", Done: 1, Total: 1, Extra: extra})
	return ChaptersJobResult{LibraryID: r.lib.ID, Path: r.rel, Chapters: len(d.Chapters), Files: files, Source: SourceAudio}, nil
}
