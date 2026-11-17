package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Phase 4c: an Audible account's places, both ways (the server's
// handlers_audible_sync.go and catalog/audible_sync.go, mirrored BY HAND). The
// audible.sync job posts the account's places on Audible for its person
// (furthest only, as device "audible:<account>"); the push loop reads the
// person's places to write them back to Audible. Both act for the account's
// person, never the paired admin; the bridge token goes in the header only.

// The audible.sync job's kind and its params, as the server builds them
// (bridge.AudibleSyncParams): the account and every title to sync (matched to
// a book on the server, not "Not this book" nor "Don't sync this book", not
// gone), each with the book its places go to and Audible's runtime.
const KindAudibleSync = "audible.sync"

// AudibleSyncParams are audible.sync's params.
type AudibleSyncParams struct {
	Account string             `json:"account"`
	Titles  []AudibleSyncTitle `json:"titles"`
}

// AudibleSyncTitle is one title an audible.sync reads: its ASIN, the book on
// the server it is matched to (library and book path, the progress key the
// events name) and Audible's runtime in seconds (0 unknown).
type AudibleSyncTitle struct {
	ASIN      string `json:"asin"`
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	RuntimeS  int64  `json:"runtime_s"`
}

// The Sync v2 event kinds an account's places use.
const (
	EventListen = "listen"
	EventFinish = "finish"
)

// MaxAudibleEvents is the most events the Bridge sends in one listening call
// (the server takes 500).
const MaxAudibleEvents = 200

// AudibleEvent is one Sync v2 event of an account's places: a listen span of
// no length at Audible's place (FromPos = ToPos, StartedAt = EndedAt = when
// Audible heard it, Start "jump"), or a finish (At). The server decodes each
// event strictly: only these fields.
type AudibleEvent struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	LibraryID int64    `json:"library_id"`
	Path      string   `json:"path"`
	FromPos   *float64 `json:"from_pos,omitempty"`
	ToPos     *float64 `json:"to_pos,omitempty"`
	StartedAt string   `json:"started_at,omitempty"`
	EndedAt   string   `json:"ended_at,omitempty"`
	Start     string   `json:"start,omitempty"`
	At        string   `json:"at,omitempty"`
}

// AudibleListening is one POST .../listening: the Bridge's clock when it sent
// the events (RFC 3339), and at most MaxAudibleEvents events.
type AudibleListening struct {
	SentAt string         `json:"sent_at"`
	Events []AudibleEvent `json:"events"`
}

// The Sync v2 event statuses and the reasons the Bridge reads.
const (
	SyncApplied    = "applied"
	SyncDuplicate  = "duplicate"
	SyncDiverged   = "diverged"
	SyncNotApplied = "not_applied"
	SyncRejected   = "rejected"
	// SyncBehind: the place isn't ahead of the stored one (furthest only).
	SyncBehind = "behind"
	// SyncDurationMismatch: rejected, the book's length disagrees with the
	// title's runtime on Audible (another edition or volume is matched).
	SyncDurationMismatch = "duration_mismatch"
)

// SyncResult is one event's result.
type SyncResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Kept   string `json:"kept,omitempty"`
}

// SyncAnswer is the listening call's answer (the §25 answer): the server's
// time, a result per event in request order, and the progress of the books
// the events touched (not read here).
type SyncAnswer struct {
	ServerTime string          `json:"server_time"`
	Results    []SyncResult    `json:"results"`
	Progress   json.RawMessage `json:"progress"`
}

// AudibleProgressRow is one of the person's places the Bridge may push to the
// account on Audible: the book, its title's ASIN, the place and the book's
// length (seconds), whether it is finished, when the row changed (RFC 3339)
// and on which device, and its version.
type AudibleProgressRow struct {
	LibraryID int64   `json:"library_id"`
	Path      string  `json:"path"`
	ASIN      string  `json:"asin"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Finished  bool    `json:"finished"`
	UpdatedAt string  `json:"updated_at"`
	DeviceID  string  `json:"device_id"`
	Version   int64   `json:"version"`
}

// AudibleProgress is one page of GET /bridge/progress: the rows changed after
// the cursor sent as since (the books of the account's synced titles the
// person can reach, rows the account itself wrote left out, oldest change
// first), Next the cursor to send next time (since again when nothing
// changed: kept between polls), More whether to ask again at once.
type AudibleProgress struct {
	Rows []AudibleProgressRow `json:"rows"`
	Next string               `json:"next"`
	More bool                 `json:"more"`
}

// MaxAudibleProgress is the largest page the Bridge asks for.
const MaxAudibleProgress = 200

// Errors of the sync routes (ErrUnknownAccount too).
var (
	// ErrSyncOff is a 403 sync_off: the audible module or the account's sync
	// is off on the server.
	ErrSyncOff = errors.New("sync with Audible is off for this account on the server")
	// ErrUserUnavailable is a 403 user_unavailable: the account's person is
	// disabled, a kid or a demo account since it was mapped.
	ErrUserUnavailable = errors.New("the account's person can't be synced for (disabled, a kid or a demo account): map the account to someone else")
	// ErrBadCursor is AudibleProgress's 400 for a since the server can't read
	// (a server restored from a backup, its feed rebuilt): read from the start.
	ErrBadCursor = errors.New("the server refused the progress cursor")
	// ErrNoAudibleSync is a 404 that names no account: the route itself is
	// missing, a server older than 2.6.0 (before Audible sync).
	ErrNoAudibleSync = errors.New("the server doesn't sync places with Audible: update it to 2.6.0 or later")
)

// syncError maps the sync routes' refusals.
func syncError(err error) error {
	var e *APIError
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case e.Status == http.StatusForbidden && e.Code == "sync_off":
		return ErrSyncOff
	case e.Status == http.StatusForbidden && e.Code == "user_unavailable":
		return ErrUserUnavailable
	case e.Status == http.StatusNotFound && e.Code == "unknown_account":
		return ErrUnknownAccount
	case e.Status == http.StatusNotFound:
		return ErrNoAudibleSync
	}
	return err
}

// PostAudibleListening posts account's places on Audible for its person
// (POST /bridge/audible/accounts/{account}/listening). ErrSyncOff,
// ErrUserUnavailable, ErrUnknownAccount, ErrNoAudibleSync.
func (c *Client) PostAudibleListening(ctx context.Context, account string, in AudibleListening) (*SyncAnswer, error) {
	if in.Events == nil {
		in.Events = []AudibleEvent{}
	}
	var out SyncAnswer
	if err := c.do(ctx, http.MethodPost, audiblePath(account)+"/listening", nil, in, &out); err != nil {
		return nil, syncError(err)
	}
	return &out, nil
}

// AudibleProgress reads one page of account's person's places changed after
// since ("" from the start), at most limit (0 for MaxAudibleProgress).
// ErrSyncOff, ErrUserUnavailable, ErrUnknownAccount, ErrNoAudibleSync,
// ErrBadCursor (a 400 when since was sent).
func (c *Client) AudibleProgress(ctx context.Context, account, since string, limit int) (*AudibleProgress, error) {
	if limit <= 0 {
		limit = MaxAudibleProgress
	}
	q := url.Values{"account": {account}, "limit": {strconv.Itoa(limit)}}
	if since != "" {
		q.Set("since", since)
	}
	var out AudibleProgress
	if err := c.do(ctx, http.MethodGet, "/api/v1/bridge/progress", q, nil, &out); err != nil {
		var e *APIError
		if since != "" && errors.As(err, &e) && e.Status == http.StatusBadRequest {
			return nil, fmt.Errorf("%w: %w", ErrBadCursor, err)
		}
		return nil, syncError(err)
	}
	if out.Rows == nil {
		out.Rows = []AudibleProgressRow{}
	}
	return &out, nil
}
