package importjob

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// maxBookPages bounds the library listing (500 books a page: a million books).
const maxBookPages = 2000

// BookLister reads a library's books page by page (*link.Client).
type BookLister interface {
	LibraryBooks(ctx context.Context, libraryID int64, after string, limit int) (*link.LibraryBooks, error)
}

// AllBooks reads every page of the library's books (the server's paged
// listing, read whole), as Plan and the matcher take them. A cursor the server
// refuses (link.ErrBadCursor: one it gave, refused mid-read) sends the read
// back to the start once, with the whole maxBookPages again; a second refusal
// is the error.
func AllBooks(ctx context.Context, srv BookLister, libraryID int64) ([]link.LibraryBook, error) {
	list, err := ReadListing(ctx, srv, libraryID)
	return list.Books, err
}

// Listing retains the server-issued cursors into its path-ordered listing.
// They allow an import to refresh an indexed book with all of its series.
type Listing struct {
	Books []link.LibraryBook
	pages []bookPage
}

type bookPage struct {
	lastPath string
	next     string
}

// ReadListing reads all book pages and keeps their cursors for path lookups.
func ReadListing(ctx context.Context, srv BookLister, libraryID int64) (Listing, error) {
	var list Listing
	after := ""
	restarted := false
	for pages := 0; pages < maxBookPages; pages++ {
		page, err := srv.LibraryBooks(ctx, libraryID, after, link.MaxLibraryBooks)
		if errors.Is(err, link.ErrBadCursor) && !restarted {
			list, after, restarted, pages = Listing{}, "", true, -1
			continue
		}
		if err != nil {
			return Listing{}, err
		}
		list.Books = append(list.Books, page.Books...)
		if page.Next == "" || page.Next == after {
			return list, nil
		}
		if n := len(page.Books); n > 0 {
			list.pages = append(list.pages, bookPage{page.Books[n-1].Path, page.Next})
		}
		after = page.Next
	}
	return Listing{}, fmt.Errorf("the library has more than %d pages of books", maxBookPages)
}

// BookAt reads the page containing at using the nearest earlier cursor. An
// insertion can shift a book onto a later page, so follow Next until the path
// is found or passed. A refused cursor tells the caller to renew its listing.
func (l Listing) BookAt(ctx context.Context, srv BookLister, libraryID int64, at string) (link.LibraryBook, error) {
	i, _ := slices.BinarySearchFunc(l.pages, at, func(p bookPage, s string) int { return strings.Compare(p.lastPath, s) })
	after := ""
	if i > 0 {
		after = l.pages[i-1].next
	}
	for range maxBookPages {
		page, err := srv.LibraryBooks(ctx, libraryID, after, link.MaxLibraryBooks)
		if err != nil {
			return link.LibraryBook{}, err
		}
		i, found := slices.BinarySearchFunc(page.Books, at, func(b link.LibraryBook, s string) int { return strings.Compare(b.Path, s) })
		if found {
			return page.Books[i], nil
		}
		if i < len(page.Books) || page.Next == "" || page.Next == after {
			return link.LibraryBook{}, link.ErrNoBook
		}
		after = page.Next
	}
	return link.LibraryBook{}, fmt.Errorf("the library has more than %d pages of books", maxBookPages)
}
