package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-meta/pkg/model"

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Share is sidecars.contribute's step (BRIDGE-PLAN.md Phase 2b, plan D2): a
// finished book's characters and recaps, kept in the engine's work dir, filed to
// the community repository as intake issues, the way the contributing stage files
// them in issue mode. It runs outside the engine's stages (the caller holds the
// book): no agent, no spend. A local or covered row from an earlier run is filed
// again; an issue already filed for a dimension is kept, and reported.

// The dimensions a share files (the server's names).
const (
	ShareCharacters = "characters"
	ShareRecaps     = "recaps"
)

// ErrNotMatched is a book with no community work to file for.
var ErrNotMatched = errors.New("the book isn't matched to a community work")

// ShareResult is what a share filed: the work, each filed dimension's issue (one
// filed earlier included), and the dimensions the work already carries.
type ShareResult struct {
	WorkID      string
	Contributed map[string]string
	Covered     []string
}

// Share files the book's dims (both when empty) for workID (else the book's own
// match, else its exact identifiers). A missing GitHub credential is the
// contributing stage's park (ParkContribUnavailable).
func (e *Executor) Share(ctx context.Context, bookID int64, workID string, dims []string) (ShareResult, error) {
	if e.db == nil {
		return ShareResult{}, errors.New("share: no store")
	}
	book, err := e.db.GetBook(ctx, bookID)
	if err != nil {
		return ShareResult{}, err
	}
	if len(dims) == 0 {
		dims = []string{ShareCharacters, ShareRecaps}
	}
	type file struct{ dim, kind, path string }
	var files []file
	for _, d := range dims {
		f := file{dim: d, kind: store.ContribKindCharacters, path: filepath.Join(book.WorkDir, sidecarsDir, charactersFileName)}
		if d == ShareRecaps {
			f.kind, f.path = store.ContribKindRecaps, filepath.Join(book.WorkDir, sidecarsDir, recapsFileName)
		} else if d != ShareCharacters {
			return ShareResult{}, fmt.Errorf("share: unknown dimension %q", d)
		}
		if fsutil.IsFile(f.path) {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return ShareResult{}, ErrNoSidecars // the work dir keeps none of them
	}
	slug, err := e.shareWork(ctx, book, strings.TrimSpace(workID))
	if err != nil {
		return ShareResult{}, err
	}
	sourceRef := e.contributionSourceRef(ctx, book, slug)
	var artifacts []contribArtifact
	for _, f := range files {
		if err := reconcileSidecar(f.path, slug, sourceRef); err != nil {
			return ShareResult{}, fmt.Errorf("share: reconcile %s: %w", filepath.Base(f.path), err)
		}
		artifacts = append(artifacts, contribArtifact{kind: f.kind, path: f.path})
	}
	hasChars, hasRecaps := e.coveredDimensions(ctx, slug)
	out := ShareResult{WorkID: slug, Contributed: map[string]string{}, Covered: []string{}}
	for i, f := range files {
		artifacts[i].covered = (f.kind == store.ContribKindCharacters && hasChars) || (f.kind == store.ContribKindRecaps && hasRecaps)
		if artifacts[i].covered {
			out.Covered = append(out.Covered, f.dim)
		}
	}
	if err := e.submitIssueWith(ctx, book, slug, artifacts, auditAcceptanceNote(book.WorkDir), shareSettled); err != nil {
		return ShareResult{}, err
	}
	for _, r := range e.bookContributions(ctx, book.ID) {
		for _, f := range files {
			if r.Kind == f.kind && r.URL != "" && !slices.Contains(out.Covered, f.dim) {
				out.Contributed[f.dim] = r.URL
			}
		}
	}
	return out, nil
}

// shareSettled is a share's settled rule: only an issue that is filed and still
// in play (submitted, its pull request open, merged) is; a local export, a
// covered verdict or a closed issue is filed again.
func shareSettled(rows []store.Contribution, kind string) bool {
	for _, r := range rows {
		if r.Kind == kind {
			return r.URL != "" && (r.Status == store.ContribStatusSubmitted || r.Status == store.ContribStatusPROpen ||
				r.Status == store.ContribStatusMerged)
		}
	}
	return false
}

// shareWork is the work a share files for: the job's (verified, a retired slug
// followed to its survivor, and recorded on the book), else the book's match,
// else its exact identifiers. Unmatched is ErrNotMatched: a share never proposes
// a new work.
func (e *Executor) shareWork(ctx context.Context, book store.Book, workID string) (string, error) {
	id := workID
	if id == "" {
		id = strings.TrimSpace(book.WorkID)
	}
	if id != "" {
		cov, err := e.metaCoverageForWork(ctx, id, true)
		switch {
		case err == nil:
			id = cov.WorkID
		case errors.Is(err, metaops.ErrDisabled):
		case errors.Is(err, metaops.ErrWorkNotFound):
			return "", fmt.Errorf("%w: the community database has no work %q", ErrNotMatched, id)
		default:
			return "", fmt.Errorf("share: verify work %q: %w", id, err)
		}
	} else if wid, ok := e.lookupByIdentifier(ctx, book); ok {
		id = wid
	}
	if id == "" {
		return "", ErrNotMatched
	}
	if !model.ValidSlug(id) {
		return "", fmt.Errorf("share: work %q is not a valid slug", id)
	}
	if id != book.WorkID {
		if _, err := contrib.AdoptLiveWork(ctx, e.db, book.ID, book.WorkID, id); err != nil {
			e.log.Warn("share: record the work on the book", "book", book.ID, "work", id, "err", err)
		}
	}
	return id, nil
}
