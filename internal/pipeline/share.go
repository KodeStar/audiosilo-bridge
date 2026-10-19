package pipeline

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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

// The dimensions a share files (the server's names, which are the contribution
// kinds).
const (
	ShareCharacters = store.ContribKindCharacters
	ShareRecaps     = store.ContribKindRecaps
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
	var artifacts []contribArtifact
	for _, d := range dims {
		if d != ShareCharacters && d != ShareRecaps {
			return ShareResult{}, fmt.Errorf("share: unknown dimension %q", d)
		}
		// A dimension is its contribution kind (store.ContribKind*).
		if path := filepath.Join(book.WorkDir, sidecarsDir, fileNameFor(d)); fsutil.IsFile(path) {
			artifacts = append(artifacts, contribArtifact{kind: d, path: path})
		}
	}
	if len(artifacts) == 0 {
		return ShareResult{}, ErrNoSidecars // the work dir keeps none of them
	}
	slug, err := e.shareWork(ctx, book, strings.TrimSpace(workID))
	if err != nil {
		return ShareResult{}, err
	}
	if err := e.prepareArtifacts(ctx, book, slug, artifacts); err != nil {
		return ShareResult{}, fmt.Errorf("share: %w", err)
	}
	out := ShareResult{WorkID: slug, Contributed: map[string]string{}, Covered: []string{}}
	filed := map[string]bool{}
	for _, a := range artifacts {
		if a.covered {
			out.Covered = append(out.Covered, a.kind)
		} else {
			filed[a.kind] = true
		}
	}
	if err := e.submitIssueWith(ctx, book, slug, artifacts, auditAcceptanceNote(book.WorkDir), shareSettled); err != nil {
		return ShareResult{}, err
	}
	for _, r := range e.bookContributions(ctx, book.ID) {
		if filed[r.Kind] && r.URL != "" {
			out.Contributed[r.Kind] = r.URL
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
