package generate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/scratch"
	"github.com/kodestar/audiosilo-bridge/internal/spelling"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
)

// The transcript retention (decision 5): a finished book keeps its work dir,
// because series carryover reads a predecessor's knowledge files, but everything
// holding the book's transcript text goes retention_days after its job ended.

// transcriptDirs hold the transcript text, every layer of it.
var transcriptDirs = []string{
	transcript.RawDir, transcript.JSONDir, transcript.TextDir, transcript.RepairedDir, spelling.CorrectedDir,
}

// transcriptFiles quote the transcript at the work dir's root: the spelling
// candidates' snippets, the reference matches, the QA reports' looped phrases,
// and the correction and repair logs.
var transcriptFiles = []string{
	"spelling_candidates.json", "spelling_reference_matches.json", "qa_report.json", "qa_report.md",
	"corrections.log", "repairs.log",
}

// spellingRefsDir holds a predecessor's carryover; its *.txt there are copies of
// the predecessor's corrected transcript (the prior-* ledger, rules and marker
// titles, and the series glossary, are kept).
const spellingRefsDir = "spelling-refs"

// PurgeTranscripts deletes a book's transcript text from its work dir: the split
// audio and agent scratch (scratch.Purge, the ebook's text included), every
// transcripts-* layer, the predecessor transcript copies under spelling-refs/
// and the root files that quote it. It keeps the facts (knowledge-final.md, which
// a sequel's fact pass inherits), the spelling ledger and rules, the manifest and
// the sidecars. Confined to workRoot; a missing work dir is nothing to do.
func PurgeTranscripts(workRoot, workDir string, kind state.Kind) error {
	wd, ok := scratch.Confined(workRoot, workDir)
	if !ok {
		return nil
	}
	if err := scratch.Purge(workRoot, workDir, kind); err != nil {
		return err
	}
	var errs []error
	for _, d := range transcriptDirs {
		errs = append(errs, os.RemoveAll(filepath.Join(wd, d)))
	}
	for _, f := range transcriptFiles {
		if err := os.Remove(filepath.Join(wd, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	refs := filepath.Join(wd, spellingRefsDir)
	entries, err := os.ReadDir(refs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") || strings.HasPrefix(e.Name(), "prior-") || e.Name() == "series-glossary.txt" {
			continue
		}
		errs = append(errs, os.Remove(filepath.Join(refs, e.Name())))
	}
	return errors.Join(errs...)
}

// hasTranscripts reports whether any transcript layer is still on disk (so the
// sweep skips a book it already swept).
func hasTranscripts(workDir string) bool {
	for _, d := range transcriptDirs {
		if _, err := os.Stat(filepath.Join(workDir, d)); err == nil {
			return true
		}
	}
	return false
}

// finished reports whether an engine book is out of the scheduler's hands: done,
// or failed (cancelled included). Only such a book's transcripts are swept.
func finished(b store.Book) bool {
	return state.IsTerminal(state.State(b.State)) || state.Status(b.Status) == state.StatusFailed
}

// SweepTranscripts deletes the transcripts of every finished engine book whose
// job ended at least retention ago, and returns how many it swept.
func (h *Handler) SweepTranscripts(ctx context.Context, retention time.Duration) (int, error) {
	books, err := h.o.DB.EndedGenerateBooks(ctx, h.now().Add(-retention))
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, b := range books {
		if ctx.Err() != nil {
			break
		}
		if !finished(b) || !hasTranscripts(b.WorkDir) {
			continue
		}
		if err := PurgeTranscripts(h.o.WorkRoot, b.WorkDir, state.ParseKind(b.Kind)); err != nil {
			errs = append(errs, err)
			continue
		}
		// The disk gauge follows what is left (the durables).
		if size, err := scratch.DirSize(b.WorkDir); err == nil {
			_ = h.o.DB.UpdateScratchBytes(ctx, b.ID, size)
		}
		n++
	}
	return n, errors.Join(errs...)
}
