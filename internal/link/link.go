// Package link is the Bridge's client of the AudioSilo server: the pairing
// exchange, the heartbeat, the long-poll job claim, job updates, a book's files
// for the mapping check and a targeted rescan (BRIDGE-PLAN.md Phase 0). The
// Bridge always calls the server; the server never calls the Bridge.
//
// The wire types here mirror audiosilo-server's internal/bridge (Job, Update,
// Status, LibraryStatus, Worker) and internal/api/handlers_bridge.go by hand,
// as every client of the server does (no codegen): a change there is a change
// here, with a test on both sides (CROSS-REPO.md, the Bridge section).
package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ClientHeader names the Bridge on every request (CROSS-REPO.md §20).
const ClientHeader = "X-AudioSilo-Client"

// ClientName is the app part of ClientHeader.
const ClientName = "AudioSilo Bridge"

// The server's job states the Bridge reports (its migration 0047); the
// finished ones are the store's (store.JobDone, ...), the same words.
const (
	StateRunning        = "running"
	StateDone           = "done"
	StateFailed         = "failed"
	StateNeedsAttention = "needs_attention"
	StateCancelled      = "cancelled"
)

// Job is a server job (bridge_jobs) as the Bridge routes answer it.
type Job struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	LibraryID *int64          `json:"library_id"`
	Path      *string         `json:"path"`
	Params    json.RawMessage `json:"params"`
	State     string          `json:"state"`
	Progress  json.RawMessage `json:"progress"`
	Result    json.RawMessage `json:"result"`
	Error     string          `json:"error"`
	// RequestedBy is the admin who queued the job, nil once they are deleted.
	RequestedBy     *Requester `json:"requested_by"`
	Priority        int        `json:"priority"`
	Attempts        int        `json:"attempts"`
	LeaseUntil      *string    `json:"lease_until"`
	CancelRequested bool       `json:"cancel_requested"`
	CreatedAt       string     `json:"created_at"`
	UpdatedAt       string     `json:"updated_at"`
	ClaimedAt       *string    `json:"claimed_at"`
	FinishedAt      *string    `json:"finished_at"`
}

// Requester names the admin who queued a job.
type Requester struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// Update is the Bridge's report on a job it holds (PATCH /bridge/jobs/{id}).
// Attempt is the attempts value of its claim; nil fields keep their value.
type Update struct {
	Attempt  int             `json:"attempt"`
	State    *string         `json:"state,omitempty"`
	Progress json.RawMessage `json:"progress,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *string         `json:"error,omitempty"`
}

// Status is the heartbeat (PUT /bridge/status).
type Status struct {
	Version   string          `json:"version"`
	Kinds     []string        `json:"kinds"`
	Libraries []LibraryStatus `json:"libraries"`
	Workers   []Worker        `json:"workers"`
	UIURL     string          `json:"ui_url"`
}

// LibraryStatus is one library's mapping as the Bridge checked it.
type LibraryStatus struct {
	LibraryID   int64  `json:"library_id"`
	MountPath   string `json:"mount_path"`
	Mapped      bool   `json:"mapped"`
	Writable    bool   `json:"writable"`
	CheckedPath string `json:"checked_path"`
	Error       string `json:"error"`
}

// Worker is an ASR worker connected to the Bridge (Phase 1b).
type Worker struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	LastSeen string `json:"last_seen"`
}

// Library is a server library as the heartbeat's answer lists it.
type Library struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"`
}

// StatusReply is the heartbeat's answer.
type StatusReply struct {
	ServerTime string    `json:"server_time"`
	Libraries  []Library `json:"libraries"`
}

// Book is an indexed book's files with the size and modification time the
// server sees now (GET /bridge/libraries/{id}/book).
type Book struct {
	LibraryID int64      `json:"library_id"`
	Path      string     `json:"path"`
	Files     []BookFile `json:"files"`
}

// BookFile is one of a Book's files, its path relative to the library root.
type BookFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

// Paired is what the pairing exchange answers.
type Paired struct {
	Token      string `json:"token"`
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
}

// Errors the server's answers map to.
var (
	// ErrUnauthorized is a 401: the token was revoked (the Bridge was unpaired,
	// another Bridge paired, its admin lost the role) or a pairing code was wrong.
	ErrUnauthorized = errors.New("the server refused the Bridge's credentials")
	// ErrNotHeld is a job update's 409: the claim no longer holds the job (it
	// finished, was cancelled, or its lease ran out and it was claimed again).
	ErrNotHeld = errors.New("the job is no longer held by this claim")
	// ErrNoBook is the book lookup's 404 no_book.
	ErrNoBook = errors.New("no indexed book there")
	// ErrNoHeartbeat is a claim before any heartbeat (409 no_heartbeat).
	ErrNoHeartbeat = errors.New("the server has no heartbeat from this Bridge yet")
	// ErrBadAddress is a server address that isn't an absolute http(s) URL.
	ErrBadAddress = errors.New("the server address must be an absolute http(s) URL")
)

// APIError is any other answer that isn't a success.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("server answered %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("server answered %d: %s", e.Status, e.Message)
}

// maxBody bounds what the Bridge reads of any answer.
const maxBody = 4 << 20

// requestTimeout bounds every call but the claim, whose bound is its wait plus
// claimSlack.
const (
	requestTimeout = 30 * time.Second
	claimSlack     = 15 * time.Second
	// MaxWait is the longest claim wait the server holds (its bridge.MaxWait).
	MaxWait = 25 * time.Second
)

// Client calls one server with the Bridge's token.
type Client struct {
	base    *url.URL
	token   string
	client  string
	httpc   *http.Client
	timeout time.Duration
}

// New returns a Client of the server at serverURL (an absolute http(s) URL)
// authenticating with token. version is the Bridge's, for ClientHeader.
func New(serverURL, token, version string) (*Client, error) {
	u, err := parseBase(serverURL)
	if err != nil {
		return nil, err
	}
	return &Client{base: u, token: token, client: clientValue(version), httpc: &http.Client{}, timeout: requestTimeout}, nil
}

// clientValue is the ClientHeader value: "AudioSilo Bridge/<version> (<os>)",
// within the server's grammar (auth.ParseClient).
func clientValue(version string) string {
	v := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".+_-", r) {
			return r
		}
		return -1
	}, version)
	if v == "" {
		v = "dev"
	}
	if len(v) > 32 {
		v = v[:32]
	}
	return ClientName + "/" + v + " (" + runtime.GOOS + ")"
}

// parseBase checks a server address: absolute http(s), a host, no credentials,
// query or fragment.
func parseBase(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("%w (with no credentials, query or fragment): %q", ErrBadAddress, raw)
	}
	return u, nil
}

// Pair trades a pairing code for the Bridge's token (POST /bridge/pair, no
// credentials). A wrong, used or expired code is ErrUnauthorized.
func Pair(ctx context.Context, serverURL, code, version string) (*Paired, error) {
	c, err := New(serverURL, "", version)
	if err != nil {
		return nil, err
	}
	var out Paired
	if err := c.do(ctx, http.MethodPost, "/api/v1/bridge/pair", nil, map[string]string{"code": strings.TrimSpace(code)}, &out); err != nil {
		return nil, err
	}
	if out.Token == "" {
		return nil, errors.New("the server's pairing answer carried no token")
	}
	return &out, nil
}

// Heartbeat sends the Bridge's status (PUT /bridge/status) and returns the
// server's libraries.
func (c *Client) Heartbeat(ctx context.Context, st Status) (*StatusReply, error) {
	var out StatusReply
	if err := c.do(ctx, http.MethodPut, "/api/v1/bridge/status", nil, st, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Claim asks for the next queued job of one of kinds, waiting up to wait for
// one (at most MaxWait). It returns nil when none came.
func (c *Client) Claim(ctx context.Context, kinds []string, wait time.Duration) (*Job, error) {
	wait = min(max(wait, 0), MaxWait)
	q := url.Values{"wait": {strconv.Itoa(int(wait / time.Second))}}
	for _, k := range kinds {
		q.Add("kind", k)
	}
	var out Job
	cc := *c
	cc.timeout = wait + claimSlack
	found, err := cc.doFound(ctx, http.MethodGet, "/api/v1/bridge/jobs/next", q, nil, &out)
	if err != nil || !found {
		return nil, err
	}
	return &out, nil
}

// UpdateJob reports on a job the Bridge holds and returns it as stored (its
// CancelRequested asks the Bridge to stop). A claim that no longer holds it is
// ErrNotHeld.
func (c *Client) UpdateJob(ctx context.Context, id int64, u Update) (*Job, error) {
	var out Job
	if err := c.do(ctx, http.MethodPatch, "/api/v1/bridge/jobs/"+strconv.FormatInt(id, 10), nil, u, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Book returns an indexed book's files (path "" for the library's first book,
// the mapping check's sample). No indexed book there is ErrNoBook.
func (c *Client) Book(ctx context.Context, libraryID int64, path string) (*Book, error) {
	var q url.Values
	if path != "" {
		q = url.Values{"path": {path}}
	}
	var out Book
	if err := c.do(ctx, http.MethodGet, "/api/v1/bridge/libraries/"+strconv.FormatInt(libraryID, 10)+"/book", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Rescan asks the server to read one path again (POST /bridge/rescan) and
// returns the indexed book's path.
func (c *Client) Rescan(ctx context.Context, libraryID int64, path string) (string, error) {
	var out struct {
		Path string `json:"path"`
	}
	body := map[string]any{"library_id": libraryID, "path": path}
	if err := c.do(ctx, http.MethodPost, "/api/v1/bridge/rescan", nil, body, &out); err != nil {
		return "", err
	}
	return out.Path, nil
}

// do is doFound for calls where 204 isn't an answer.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	_, err := c.doFound(ctx, method, path, q, body, out)
	return err
}

// doFound sends one request and decodes a 2xx answer into out; found is false
// for a 204.
func (c *Client) doFound(ctx context.Context, method, path string, q url.Values, body, out any) (found bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = q.Encode()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return false, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return false, err
	}
	req.Header.Set(ClientHeader, c.client)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return false, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return true, nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return false, fmt.Errorf("decode the server's answer to %s %s: %w", method, path, err)
		}
		return true, nil
	}
	return false, answerError(resp.StatusCode, raw)
}

// answerError maps a failed answer to its sentinel or an APIError.
func answerError(status int, raw []byte) error {
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(raw, &e)
	switch {
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusConflict && (e.Code == "not_held" || e.Code == "finished"):
		return ErrNotHeld
	case status == http.StatusConflict && e.Code == "no_heartbeat":
		return ErrNoHeartbeat
	case status == http.StatusNotFound && e.Code == "no_book":
		return ErrNoBook
	}
	msg := e.Error
	if msg == "" {
		msg = http.StatusText(status)
	}
	if len(msg) > 300 {
		msg = strings.ToValidUTF8(msg[:300], "") // cut at a character's boundary
	}
	return &APIError{Status: status, Code: e.Code, Message: msg}
}

// BaseURL is the server address the client calls (normalised: no trailing
// slash).
func (c *Client) BaseURL() string { return c.base.String() }
