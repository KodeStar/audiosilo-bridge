package audiblejob

import (
	"context"
	"errors"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/importjob"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/transfer"
)

// The queue has no batch ID. Nearby imports on one pairing and library share
// a planning snapshot. A later batch reads the listing again.
const booksMaxAge = 15 * time.Minute

type importLibrary struct {
	server, pairing string
	id              int64
}

type bookSnapshot struct {
	library importLibrary
	readAt  time.Time
	importjob.Listing
	changed []string
}

func (h *Importer) booksFor(ctx context.Context, srv ImportServer, library importLibrary) ([]link.LibraryBook, bool, error) {
	h.mu.Lock()
	last := h.listing
	h.mu.Unlock()
	// A rescan can finish while a listing is in flight. Only publish the
	// snapshot if no other reader or rescan has replaced it in the meantime.
	next := &bookSnapshot{library: library, readAt: time.Now()}
	reused := last != nil && library.server != "" && last.library == library && time.Since(last.readAt) < booksMaxAge
	var err error
	if reused {
		next.Listing, next.readAt = last.Listing, last.readAt
		for _, at := range last.changed {
			err = next.refreshBook(ctx, srv, at)
			if errors.Is(err, link.ErrNoBook) || errors.Is(err, link.ErrBadCursor) {
				reused = false // removed book or invalidated listing
				break
			}
			if err != nil {
				return nil, false, err
			}
		}
	}
	if !reused {
		next.Listing, err = importjob.ReadListing(ctx, srv, library.id)
		if err != nil {
			return nil, false, err
		}
	}
	h.mu.Lock()
	if h.listing == last {
		h.listing = next
	}
	h.mu.Unlock()
	return next.Books, reused, nil
}

// On a reused plan, ask about the destination and its possible book folders
// by path. A listed book needs the full, current matching and twin checks.
// A no-book answer for each path leaves only the ordinary on-disk checks.
func booksAt(ctx context.Context, srv ImportServer, libraryID int64, mount, rel string) ([]link.LibraryBook, error) {
	root, err := os.OpenRoot(mount)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	dir := path.Dir(rel)
	paths := []string{rel, dir, path.Dir(dir)}
	for _, former := range formerDirs(transfer.RootFS(root), dir) {
		paths = append(paths, former, path.Join(former, path.Base(rel)))
	}
	for _, at := range paths {
		if at == "." {
			continue
		}
		_, err := srv.Book(ctx, libraryID, at)
		if errors.Is(err, link.ErrNoBook) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return allBooks(ctx, srv, libraryID)
	}
	return nil, nil
}

// Mark the rescan for the next planning pass. The library's new book can
// supply the author folder or the series siblings for the next import. A
// listing past booksMaxAge is dropped rather than kept for the process's life.
func (h *Importer) recordBook(library importLibrary, at string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var next bookSnapshot
	if h.listing != nil && time.Since(h.listing.readAt) < booksMaxAge {
		next = *h.listing
	}
	if next.library == library {
		next.changed = append(slices.Clip(next.changed), at)
	}
	h.listing = &next
}

func (s *bookSnapshot) refreshBook(ctx context.Context, srv ImportServer, at string) error {
	book, err := s.BookAt(ctx, srv, s.library.id, at)
	if err != nil {
		return err
	}
	i, found := slices.BinarySearchFunc(s.Books, at, func(b link.LibraryBook, key string) int { return strings.Compare(b.Path, key) })
	if found {
		s.Books = slices.Clone(s.Books)
		s.Books[i] = book
	} else {
		s.Books = slices.Insert(slices.Clip(s.Books), i, book)
	}
	return nil
}
