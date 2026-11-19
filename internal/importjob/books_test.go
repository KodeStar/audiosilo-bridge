package importjob

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// pagedBooks serves a library one book a page; refuse is how many times it
// refuses a cursor it gave (link.ErrBadCursor) before taking it.
type pagedBooks struct {
	paths  []string
	refuse int
	asks   []string
}

func (s *pagedBooks) LibraryBooks(_ context.Context, _ int64, after string, _ int) (*link.LibraryBooks, error) {
	s.asks = append(s.asks, after)
	if after != "" && s.refuse > 0 {
		s.refuse--
		return nil, fmt.Errorf("%w: refused", link.ErrBadCursor)
	}
	i := 0
	if after != "" {
		_, _ = fmt.Sscanf(after, "p%d", &i)
	}
	page := &link.LibraryBooks{Books: []link.LibraryBook{{Path: s.paths[i]}}}
	if i+1 < len(s.paths) {
		page.Next = fmt.Sprintf("p%d", i+1)
	}
	return page, nil
}

// A cursor the server refuses mid-read sends the read back to the start once,
// so the books read whole are each listed once; a second refusal is the error
// (bridge#50).
func TestAllBooksRestartsOnARefusedCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := &pagedBooks{paths: []string{"A", "B", "C"}, refuse: 1}
	books, err := AllBooks(ctx, srv, 1)
	if err != nil || len(books) != 3 || books[0].Path != "A" || books[2].Path != "C" {
		t.Fatalf("books = %+v, %v", books, err)
	}
	if want := []string{"", "p1", "", "p1", "p2"}; fmt.Sprint(srv.asks) != fmt.Sprint(want) {
		t.Fatalf("asks = %q, want %q", srv.asks, want)
	}

	again := &pagedBooks{paths: []string{"A", "B"}, refuse: 2}
	if _, err := AllBooks(ctx, again, 1); !errors.Is(err, link.ErrBadCursor) {
		t.Fatalf("refused twice = %v, want ErrBadCursor", err)
	}
}
