package importjob

import (
	"context"
	"errors"
	"fmt"

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
// back to the start once; a second refusal is the error.
func AllBooks(ctx context.Context, srv BookLister, libraryID int64) ([]link.LibraryBook, error) {
	var books []link.LibraryBook
	after := ""
	restarted := false
	for range maxBookPages {
		page, err := srv.LibraryBooks(ctx, libraryID, after, link.MaxLibraryBooks)
		if errors.Is(err, link.ErrBadCursor) && !restarted {
			books, after, restarted = nil, "", true
			continue
		}
		if err != nil {
			return nil, err
		}
		books = append(books, page.Books...)
		if page.Next == "" || page.Next == after {
			return books, nil
		}
		after = page.Next
	}
	return nil, fmt.Errorf("the library has more than %d pages of books", maxBookPages)
}
