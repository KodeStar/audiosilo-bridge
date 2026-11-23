package link

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Phase 4a: Audible. The server's routes for the accounts this Bridge signs in
// to (its handlers_audible.go, mirrored BY HAND): who an account may be mapped
// to, an account's mapping, the titles a poll found, a library's books for the
// matching and an ASIN backfill. Every route takes the bridge token in the
// header only.

// User is one person an Audible account may be mapped to (GET /bridge/users:
// enabled adults and admins; never a kid, demo or disabled account, never an
// email).
type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Admin       bool   `json:"admin"`
}

// AudibleAccountInput is an account's mapping (PUT
// /bridge/audible/accounts/{account}): the server user it belongs to, the books
// library its titles go to and the policy (all | series | manual). Whether its
// places sync is the console's alone (Phase 4c): the server reads and ignores
// a sync sent here, so none is (bridge#50).
type AudibleAccountInput struct {
	Label       string `json:"label"`
	Marketplace string `json:"marketplace"`
	UserID      int64  `json:"user_id"`
	LibraryID   int64  `json:"library_id"`
	Policy      string `json:"policy"`
}

// AudibleAccount is the server's record of an account (the part the Bridge
// reads back). LibraryID is nil while the account is paused (its library was
// deleted on the server); PollHours is how often the server queues its poll.
type AudibleAccount struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Marketplace string `json:"marketplace"`
	UserID      int64  `json:"user_id"`
	User        struct {
		ID          int64  `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"user"`
	LibraryID *int64 `json:"library_id"`
	Paused    bool   `json:"paused"`
	Policy    string `json:"policy"`
	Sync      bool   `json:"sync"`
	PollHours int    `json:"poll_hours"`
}

// The Audible title states a poll reports.
const (
	AudibleAvailable   = "available"
	AudiblePreorder    = "preorder"
	AudibleUnavailable = "unavailable"
)

// The ways a title matched a book.
const (
	MatchASIN  = "asin"
	MatchFuzzy = "fuzzy"
)

// MaxAudibleTitles is the most titles one PUT .../titles call carries (the
// server's MaxAudibleTitles); MaxLibraryBooks the largest page of a library's
// books (its MaxBridgeBooks).
const (
	MaxAudibleTitles = 500
	MaxLibraryBooks  = 500
)

// AudibleMatch is the book a title matched: the library, the book's path and
// how (MatchASIN, MatchFuzzy).
type AudibleMatch struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	How       string `json:"how"`
}

// ReasonContested is an available title's Reason when it matched a book
// another title claimed with a better (or the same) score, and its series
// number doesn't say it is another volume: the book is probably here under the
// other title, so a backup of everything skips it. ContestedPath names that
// book.
const ReasonContested = "contested"

// The kinds of a planned destination (AudiblePlan.Kind): beside the series'
// books the library has, as the first of its series there, or on its own.
const (
	PlanSeriesExisting = "series_existing"
	PlanSeriesNew      = "series_new"
	PlanStandalone     = "standalone"
)

// The states of a planned destination (AudiblePlan.State): a new folder, one
// that joins folders the library has (the series' or the author's), one taken
// already (Conflict says by what), or no destination (Conflict says why).
const (
	PlanNew    = "new"
	PlanJoins  = "joins"
	PlanExists = "exists"
	PlanError  = "error"
)

// The conflicts of a PlanExists destination: a file is at the path, its folder
// is a book the library lists, or its folder already holds audio.
const (
	ConflictFile  = "file"
	ConflictBook  = "book"
	ConflictAudio = "audio"
)

// AudiblePlan is where a backup of an unmatched title would go, worked out at
// the poll: the library, the file's path in it and its name, the placement's
// kind and how many of the series' books the library has, the state and its
// conflict, and the account's template the plan used ("" for Auto). Path and
// File are "" when State is PlanError.
type AudiblePlan struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	File      string `json:"file"`
	Kind      string `json:"kind"`
	Siblings  int    `json:"siblings"`
	State     string `json:"state"`
	Conflict  string `json:"conflict"`
	Template  string `json:"template"`
}

// AudibleTitle is one title of an account's library as a poll reports it.
// ReleaseDate is YYYY-MM-DD or ""; Reason says why an unavailable title can't
// be downloaded, or ReasonContested for an available one (ContestedPath the
// book it lost); Match is nil when no book matched, and Planned then says
// where a backup would go (nil for a matched title).
type AudibleTitle struct {
	ASIN          string        `json:"asin"`
	Title         string        `json:"title"`
	Authors       []string      `json:"authors"`
	Series        []SeriesRef   `json:"series"`
	ReleaseDate   string        `json:"release_date"`
	RuntimeS      int64         `json:"runtime_s"`
	State         string        `json:"state"`
	Reason        string        `json:"reason"`
	Match         *AudibleMatch `json:"match"`
	ContestedPath string        `json:"contested_path,omitempty"`
	Planned       *AudiblePlan  `json:"planned"`
}

// AudibleTitles is one PUT .../titles call: a page of one poll's titles (at
// most MaxAudibleTitles). On the poll's Final call the server marks every
// title of the account the poll didn't report as gone.
type AudibleTitles struct {
	PollID string         `json:"poll_id"`
	Final  bool           `json:"final"`
	Titles []AudibleTitle `json:"titles"`
}

// Errors of the Audible routes.
var (
	// ErrASINSet is an enrichment's 409 asin_set: the book has another ASIN.
	ErrASINSet = errors.New("the book already has another ASIN")
	// ErrUnknownAccount is a 404 unknown_account: the server has no such
	// Audible account (it was removed there, or this is another server).
	ErrUnknownAccount = errors.New("the server has no such Audible account")
)

// audiblePath is an account's route.
func audiblePath(account string) string {
	return "/api/v1/bridge/audible/accounts/" + url.PathEscape(account)
}

// Users lists who an Audible account may be mapped to.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var out struct {
		Users []User `json:"users"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/bridge/users", nil, nil, &out); err != nil {
		return nil, err
	}
	if out.Users == nil {
		out.Users = []User{}
	}
	return out.Users, nil
}

// PutAudibleAccount records account's mapping on the server (a new account,
// or one mapped again) and returns the server's record of it.
func (c *Client) PutAudibleAccount(ctx context.Context, account string, in AudibleAccountInput) (*AudibleAccount, error) {
	var out struct {
		Account AudibleAccount `json:"account"`
	}
	if err := c.do(ctx, http.MethodPut, audiblePath(account), nil, in, &out); err != nil {
		return nil, err
	}
	return &out.Account, nil
}

// DeleteAudibleAccount removes account on the server (its titles go with it).
// Any 2xx or 404 acknowledges the removal, even if the body was cut short.
func (c *Client) DeleteAudibleAccount(ctx context.Context, account string) error {
	status, err := c.doStatus(ctx, http.MethodDelete, audiblePath(account), nil, nil, nil)
	switch {
	case status >= 200 && status < 300, status == http.StatusNotFound:
		return nil
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	}
	// A proxy may truncate its error body or return a code normally used on
	// another route. Keep the HTTP status for the removal retry decision.
	var ae *APIError
	if status != 0 && err != nil && !errors.As(err, &ae) {
		return &APIError{Status: status, Message: err.Error()}
	}
	return err
}

// PutAudibleTitles stores one call of a poll's titles and returns how many the
// server stored. ErrUnknownAccount: the server has no such account.
func (c *Client) PutAudibleTitles(ctx context.Context, account string, t AudibleTitles) (int, error) {
	if t.Titles == nil {
		t.Titles = []AudibleTitle{}
	}
	var out struct {
		Stored int `json:"stored"`
	}
	err := c.do(ctx, http.MethodPut, audiblePath(account)+"/titles", nil, t, &out)
	if e := (*APIError)(nil); errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code == "unknown_account" {
		return 0, ErrUnknownAccount
	}
	if err != nil {
		return 0, err
	}
	return out.Stored, nil
}

// RejectedAudibleTitles reads the account's "Not this book" decisions before matching.
// Older servers have only the PUT on this path. A missing decision defaults to false.
func (c *Client) RejectedAudibleTitles(ctx context.Context, account string) (map[string]bool, error) {
	rejected := map[string]bool{}
	q := url.Values{}
	for {
		var page struct {
			Titles []struct {
				ASIN     string `json:"asin"`
				Rejected bool   `json:"rejected"`
			} `json:"titles"`
			Next string `json:"next"`
		}
		err := c.do(ctx, http.MethodGet, audiblePath(account)+"/titles", q, nil, &page)
		if err != nil {
			var e *APIError
			if errors.As(err, &e) {
				switch {
				case e.Status == http.StatusNotFound && e.Code == "unknown_account":
					return nil, fmt.Errorf("%w: %w", ErrUnknownAccount, err)
				case q.Get("after") == "" && (e.Status == http.StatusMethodNotAllowed ||
					(e.Status == http.StatusNotFound && (e.Code == "not_found" || e.Code == ""))):
					return nil, nil
				}
			}
			return nil, err
		}
		for _, t := range page.Titles {
			if t.Rejected {
				rejected[t.ASIN] = true
			}
		}
		if page.Next == "" {
			return rejected, nil
		}
		if page.Next <= q.Get("after") {
			return nil, errors.New("the server repeated an Audible title decisions page")
		}
		q.Set("after", page.Next)
	}
}

// LibraryBooks reads one page of a books library's indexed books, by path:
// after is the previous page's Next ("" for the first), limit at most
// MaxLibraryBooks (0 for the server's default). Next is "" on the last page.
// ErrBadCursor when the server refused after (badCursor): read from the start.
func (c *Client) LibraryBooks(ctx context.Context, libraryID int64, after string, limit int) (*LibraryBooks, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out LibraryBooks
	if err := c.do(ctx, http.MethodGet, "/api/v1/bridge/libraries/"+strconv.FormatInt(libraryID, 10)+"/books", q, nil, &out); err != nil {
		if after != "" && badCursor(err) {
			return nil, fmt.Errorf("%w: %w", ErrBadCursor, err)
		}
		return nil, err
	}
	if out.Books == nil {
		out.Books = []LibraryBook{}
	}
	return &out, nil
}

// Enrich backfills asin onto the book at path, which has none (an Audible
// title matched to it without doubt). ErrASINSet: it has another; ErrNoBook:
// no book is indexed there now.
func (c *Client) Enrich(ctx context.Context, libraryID int64, path, asin string) error {
	err := c.do(ctx, http.MethodPut, "/api/v1/bridge/libraries/"+strconv.FormatInt(libraryID, 10)+"/enrichment",
		url.Values{"path": {path}}, map[string]string{"asin": asin}, nil)
	var e *APIError
	switch {
	case errors.As(err, &e) && e.Status == http.StatusConflict && e.Code == "asin_set":
		return ErrASINSet
	case errors.As(err, &e) && e.Status == http.StatusNotFound:
		return ErrNoBook
	}
	return err
}
