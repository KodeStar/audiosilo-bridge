package generate

import (
	"fmt"
	"math"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The steps the job reports beside the engine's stage names.
const (
	stepWaiting    = "waiting"
	stepWriting    = "writing"
	stepRescanning = "rescanning"
)

// Park codes the Bridge itself parks a job with, before the engine has the book
// or while it writes into the library (the engine's own are state.ParkCode).
const (
	parkBookNotWritable    = "book_not_writable"
	parkLibraryNotWritable = "library_not_writable"
)

// stageLabels word each engine stage for the server's console and the player.
var stageLabels = map[state.State]string{
	state.Queued:             "Waiting for its turn",
	state.Inspecting:         "Reading the audio files",
	state.MarkersNormalizing: "Working out the chapters",
	state.Splitting:          "Splitting the audio into chapters",
	state.ASR:                "Transcribing",
	state.Sanitizing:         "Tidying the transcripts",
	state.QASweep:            "Checking the transcripts",
	state.QAAdjudicating:     "Reviewing transcript problems",
	state.Retranscribing:     "Transcribing chapters again",
	state.SpellingResearch:   "Researching the names",
	state.Correcting:         "Correcting the names",
	state.Extracting:         "Reading the ebook",
	state.ChapterMapping:     "Working out the ebook's chapters",
	state.FactPass:           "Reading for the facts",
	state.Synthesizing:       "Writing the characters and recaps",
	state.Validating:         "Checking the characters and recaps",
	state.Auditing:           "Checking for spoilers",
	state.Fixing:             "Fixing what the check found",
	state.Ready:              "Ready to save",
	state.Contributing:       "Saving the characters and recaps",
	state.Done:               "Finished",
}

// parkHints tell an admin what a park needs. A code with no entry gets the
// engine's own message only.
var parkHints = map[string]string{
	string(state.ParkAgentUnavailable): "The agent container isn't answering, or its CLI has no credential. The book " +
		"resumes by itself once it does: check the agent container is running and logged in.",
	string(state.ParkAgentRateLimited): "The agent account hit its usage limit. The book resumes by itself when the limit resets.",
	string(state.ParkBudgetExceeded): "The agent budget is spent. A monthly cap resumes on the 1st; for the per-book cap, " +
		"raise agent.book_budget_usd in the Bridge's config.yaml, restart it and press Retry.",
	string(state.ParkAgentValidationExhausted): "The agent's answer kept failing the checks. Press Retry to give it another go.",
	string(state.ParkMarkersNotConfident):      "The chapter markers couldn't be read with confidence. Press Retry, or check the book's chapters.",
	string(state.ParkQANoConverge):             "The transcript checks didn't settle. Press Retry for another round.",
	string(state.ParkSpellingGateFailure):      "The name corrections failed their checks. Press Retry for another go.",
	string(state.ParkMediaToolsUnavailable):    "ffmpeg or ffprobe is missing on the Bridge. Install them (or allow the download), then press Retry.",
	string(state.ParkASRUnavailable):           "Speech recognition isn't available on the Bridge. Fix it (see the Bridge's log), then press Retry.",
	string(state.ParkManifestChanged): "The book's audio changed since it was transcribed. Generate it again to start over " +
		"from the new files.",
	string(state.ParkFixLoopExhausted): "The spoiler check kept finding problems. Press Retry for another round.",
	string(state.ParkASRDecodeTimeout): "A chapter took too long to transcribe. Press Retry to try it again.",
	string(state.ParkContribUnavailable): "The contribution couldn't reach GitHub or has no token. Fix that, then press Retry. " +
		"The characters and recaps are already saved with the book.",
	string(state.ParkCoreNeeded): "The book isn't in the community database yet, so its recaps can't be contributed. " +
		"They are saved with the book; propose the book to the database, or generate it with contribute set to local.",
	string(state.ParkCorePending):     "The book's community record is waiting for the next data release. It resumes by itself.",
	string(state.ParkEbookUnreadable): "The .epub couldn't be read (it may be DRM-protected). Generate it from the audio instead.",
	string(state.ParkEbookNoChapters): "No chapters were found in the .epub. Generate it from the audio instead.",
	string(state.ParkEbookChaptersNotConfident): "The ebook's chapters couldn't be matched with confidence. Press Retry, or " +
		"generate it from the audio.",
	string(state.ParkSupervisorEscalated): "The engine's supervisor stopped the book for a person to look at. Check the " +
		"Bridge's log, then press Retry.",
	string(state.ParkSupervisorBudget): "The supervisor's budget for this book is spent. Press Retry to let it go on.",
	parkBookNotWritable: "The Bridge can't write in the book's folder. Give the Bridge's user write access to it " +
		"(the job goes on by itself within a minute, or press Retry).",
	parkLibraryNotWritable: "The library isn't mapped, or isn't writable, on the Bridge any more. Fix the mapping on the " +
		"Bridge's page; the job goes on once the next check passes.",
}

// ParkHint is the advice for a park code ("" for one with none).
func ParkHint(code string) string { return parkHints[code] }

// view is what a poll of the engine book found, for describe.
type view struct {
	book     store.Book
	progress []store.Progress
	eta      int64
	hasETA   bool
	costUSD  float64
	source   string
}

// describe turns the engine book into the job's progress (contract section 1):
// the step and a sentence, the stage's done/total, and in Extra the stage, the
// whole-book ETA, the spend so far, the source and, while parked, the park.
func describe(v view) jobs.Progress {
	b := v.book
	st := state.State(b.State)
	pr := jobs.Progress{Step: b.State, Message: stageLabels[st]}
	if pr.Message == "" {
		pr.Message = b.State
	}
	if st == state.Queued {
		pr.Step = stepWaiting
	}
	for _, p := range v.progress {
		if p.Stage == b.State && p.Total > 0 {
			pr.Done, pr.Total = p.Done, p.Total
			pr.Message = fmt.Sprintf("%s (%d of %d)", pr.Message, p.Done, p.Total)
		}
	}
	extra := map[string]any{"stage": b.State, "source": v.source, "cost_usd": Cents(v.costUSD)}
	switch state.Status(b.Status) {
	case state.StatusNeedsAttention:
		park := map[string]any{"code": b.ParkCode, "hint": ParkHint(b.ParkCode)}
		if b.RetryAt != "" {
			park["retry_at"] = b.RetryAt
		}
		extra["park"] = park
		pr.Message = "Needs attention: " + pr.Message
	case state.StatusPaused:
		pr.Message = "Paused: " + pr.Message
	default:
		if v.hasETA {
			extra["eta_seconds"] = v.eta
		}
	}
	pr.Extra = extra
	return pr
}

// Cents rounds a dollar figure to the cent, as every figure the Bridge reports is.
func Cents(usd float64) float64 { return math.Round(usd*100) / 100 }
