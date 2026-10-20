package contrib

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// requestTimeout bounds a single REST request (a poll of many small requests
// wraps this in its own overall context).
const requestTimeout = 15 * time.Second

// maxBodyBytes caps how much of a response body is read - GitHub JSON for the
// endpoints we use is small, and this bounds memory against a pathological body.
const maxBodyBytes = 8 << 20 // 8 MiB

// maxErrorBody caps the response snippet embedded in an APIError/RateLimitError.
const maxErrorBody = 300

// Issue is the subset of a GitHub issue the contribution flow needs. Labels is
// the labels that actually stuck (GitHub silently drops labels set by a
// non-collaborator, so the caller re-reads via GetIssue to verify).
type Issue struct {
	Number    int
	URL       string
	State     string // "open" | "closed" (the poller closes an issue with no merged intake PR)
	Labels    []string
	UpdatedAt string // GitHub's updated_at: moves on any edit, comment or label change
}

// PR is the subset of a GitHub pull request the contribution flow needs. BaseSHA
// and HeadSHA bound the PR's own change (the poller diffs them to learn which work
// a merged add-work PR created, whatever the merge style).
type PR struct {
	Number  int
	URL     string
	State   string
	Merged  bool
	BaseSHA string
	HeadSHA string
}

// APIError is an unexpected (non-success) GitHub HTTP response. It carries the
// status and a trimmed response body; it NEVER carries the request's bearer
// token (that lives only in the request header, and GitHub does not echo it).
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github api: status %d: %s", e.Status, e.Body)
}

// RateLimitError is a 403/429 response whose x-ratelimit-remaining is 0. Callers
// treat it as transient (retry later), distinct from a permanent APIError.
type RateLimitError struct {
	Status int
	Body   string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github api rate limited: status %d: %s", e.Status, e.Body)
}

// Client is a minimal GitHub REST client over the stdlib HTTP client. baseURL is
// injectable so tests point it at an httptest server. token may be empty (public
// reads work unauthenticated).
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	// onUnauthorized runs on a 401 (NewClientFor: the token is recorded as
	// refused, so the Bridge stops offering to contribute with it), onOK on a
	// call answered as wanted (the token is recorded as working).
	onUnauthorized func(ctx context.Context)
	onOK           func(ctx context.Context)
	// from is where the token came from (FromEnv, FromDevice, FromGH; "" when
	// NewClient made it).
	from string
	// login caches Login's answer (the token's user doesn't change).
	loginMu sync.Mutex
	login   string
	// ahead is how far the Bridge's clock ran ahead of the Date of GitHub's
	// last answer (dated: there was one), for Now.
	clockMu sync.Mutex
	ahead   time.Duration
	dated   bool
}

// AppFiled reports whether the Client's issues are filed through the GitHub App
// (the Accounts tab's sign-in): the meta repo's route workflow labels those
// itself, after the issue is opened.
func (c *Client) AppFiled() bool { return c.from == FromDevice }

// NewClient returns a Client for baseURL (e.g. https://api.github.com) carrying
// token as a Bearer credential (empty token = unauthenticated).
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{},
	}
}

// request performs one REST call with a per-request timeout and a single retry
// on a 5xx for a GET (a POST is never sent twice: GitHub can answer a server
// error after it made the issue or gist, and a resend would make a second). A 403/429 with x-ratelimit-remaining:0 becomes a *RateLimitError; a
// non-want status becomes an *APIError. want lists the acceptable status codes
// (empty = any 2xx).
func (c *Client) request(ctx context.Context, method, path string, body any, want ...int) ([]byte, http.Header, error) {
	return c.requestAccept(ctx, "", method, path, body, want...)
}

// requestAccept is request with an explicit Accept media type ("" = the default
// application/vnd.github+json), for the endpoints that answer raw bytes.
func (c *Client) requestAccept(ctx context.Context, accept, method, path string, body any, want ...int) ([]byte, http.Header, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("contrib: marshal request body: %w", err)
		}
	}

	var lastErr error
	for attempt := range 2 {
		respBody, header, status, err := c.doOnce(ctx, accept, method, path, payload)
		if err != nil {
			return nil, nil, err
		}
		if status == http.StatusUnauthorized && c.onUnauthorized != nil {
			c.onUnauthorized(ctx)
		}
		if rl := rateLimitError(status, header, respBody); rl != nil {
			return nil, header, rl
		}
		if status >= 500 && attempt == 0 && method == http.MethodGet {
			lastErr = newAPIError(status, respBody)
			continue // retry once on a server error
		}
		if !statusWanted(status, want) {
			return nil, header, newAPIError(status, respBody)
		}
		if c.onOK != nil {
			c.onOK(ctx)
		}
		return respBody, header, nil
	}
	return nil, nil, lastErr
}

// doOnce issues a single HTTP request and reads the (bounded) response body.
func (c *Client) doOnce(ctx context.Context, accept, method, path string, payload []byte) ([]byte, http.Header, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("contrib: new request: %w", err)
	}
	c.setHeaders(req, payload != nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("contrib: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	c.noteDate(resp.Header.Get("Date"))
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("contrib: read response: %w", err)
	}
	return respBody, resp.Header, resp.StatusCode, nil
}

// noteDate records how far the Bridge's clock runs ahead of GitHub's from an
// answer's Date (whole seconds, so it reads up to a second more: a time taken
// from Now is that much early, never late).
func (c *Client) noteDate(date string) {
	at, err := http.ParseTime(date)
	if err != nil {
		return
	}
	ahead := time.Since(at)
	c.clockMu.Lock()
	c.ahead, c.dated = ahead, true
	c.clockMu.Unlock()
}

// Now is GitHub's clock as near as the Client can tell, for a time compared
// with GitHub's created_at later: the Bridge's clock less how far it ran ahead
// of the Date of GitHub's last answer. A Client that has had none asks first
// (GET /rate_limit, which costs no rate limit); the Bridge's own clock when that
// fails too.
func (c *Client) Now(ctx context.Context) time.Time {
	if now, ok := c.githubNow(); ok {
		return now
	}
	_, _, _ = c.request(ctx, http.MethodGet, "/rate_limit", nil, http.StatusOK)
	now, _ := c.githubNow()
	return now
}

// githubNow is the Bridge's clock less how far it ran ahead of the Date of
// GitHub's last answer; ok is false before any answer (the Bridge's own clock).
func (c *Client) githubNow() (now time.Time, ok bool) {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	return time.Now().Add(-c.ahead).UTC(), c.dated
}

// setHeaders applies the GitHub API headers and (when present) the bearer token.
func (c *Client) setHeaders(req *http.Request, hasBody bool) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "audiosilo-bridge")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
}

// statusWanted reports whether status is acceptable: any 2xx when want is empty,
// otherwise membership in want.
func statusWanted(status int, want []int) bool {
	if len(want) == 0 {
		return status >= 200 && status < 300
	}
	return slices.Contains(want, status)
}

// rateLimitError returns a *RateLimitError when status is 403/429 and the
// x-ratelimit-remaining header is 0, else nil.
func rateLimitError(status int, header http.Header, body []byte) *RateLimitError {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return nil
	}
	if header.Get("X-RateLimit-Remaining") != "0" {
		return nil
	}
	return &RateLimitError{Status: status, Body: trimBody(body)}
}

func newAPIError(status int, body []byte) *APIError {
	return &APIError{Status: status, Body: trimBody(body)}
}

// trimBody trims whitespace and caps the body snippet used in errors.
func trimBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody]
	}
	return s
}

// OwnerOf returns the owner segment of an "owner/name" repo (or fork full name),
// or the whole string when it has no slash. Exported so the contributing stage
// shares this one parser rather than duplicating it.
func OwnerOf(repo string) string {
	if i := strings.IndexByte(repo, '/'); i >= 0 {
		return repo[:i]
	}
	return repo
}

// --- response shapes ---

type issueResp struct {
	Number    int    `json:"number"`
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	UpdatedAt string `json:"updated_at"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (r issueResp) toIssue() Issue {
	labels := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		labels = append(labels, l.Name)
	}
	return Issue{Number: r.Number, URL: r.HTMLURL, State: r.State, Labels: labels, UpdatedAt: r.UpdatedAt}
}

type pullResp struct {
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	State    string  `json:"state"`
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
	Base     struct {
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (r pullResp) toPR() PR {
	return PR{
		Number: r.Number,
		URL:    r.HTMLURL,
		State:  r.State,
		// The list endpoint omits `merged`, so fall back to merged_at != null.
		Merged:  r.Merged || r.MergedAt != nil,
		BaseSHA: r.Base.SHA,
		HeadSHA: r.Head.SHA,
	}
}

// --- methods ---

// CreateIssue opens an issue on repo with the given labels, returning the issue
// as GitHub echoed it. GitHub may silently drop labels for a non-collaborator,
// so the caller re-reads via GetIssue to see which labels actually stuck.
func (c *Client) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (Issue, error) {
	reqBody := map[string]any{"title": title, "body": body, "labels": labels}
	respBody, _, err := c.request(ctx, http.MethodPost, "/repos/"+repo+"/issues", reqBody, http.StatusCreated)
	if err != nil {
		return Issue{}, err
	}
	var r issueResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return Issue{}, fmt.Errorf("contrib: decode issue: %w", err)
	}
	return r.toIssue(), nil
}

// FiledIssue is an issue as the repo's issue list shows it.
type FiledIssue struct {
	Number    int
	URL       string
	Title     string
	Body      string
	CreatedAt time.Time
}

// maxListPages bounds how many pages a lookup reads (one GET each).
const maxListPages = 20

// ErrListIncomplete is a lookup whose list holds more since its time than the
// Bridge reads (maxListPages pages): what it looks for can't be ruled out.
var ErrListIncomplete = errors.New("more were made since than the Bridge reads back")

// eachRow reads a list endpoint (path?q) page by page, perPage rows a page, and
// hands each row to row until a short page (the list's end) or a page on which
// row reported one past the window (a list sorted newest first reaches back no
// further). complete is false when maxListPages pages ran out first.
func eachRow[T any](ctx context.Context, c *Client, path string, q url.Values, perPage int, row func(T) (past bool)) (complete bool, err error) {
	q.Set("per_page", strconv.Itoa(perPage))
	for page := 1; page <= maxListPages; page++ {
		q.Set("page", strconv.Itoa(page))
		respBody, _, err := c.request(ctx, http.MethodGet, path+"?"+q.Encode(), nil, http.StatusOK)
		if err != nil {
			return false, err
		}
		var rows []T
		if err := json.Unmarshal(respBody, &rows); err != nil {
			return false, fmt.Errorf("contrib: decode %s: %w", path, err)
		}
		past := false
		for _, r := range rows {
			past = row(r) || past
		}
		if past || len(rows) < perPage {
			return true, nil
		}
	}
	return false, nil
}

// IssuesPage is how many issues a page of the issue list holds (a body runs to
// 64 KB, so fewer than the 100 GitHub serves).
const IssuesPage = 50

// IssuesSince lists repo's issues (pull requests left out) created at or after
// since, newest first, reading back page by page: complete is false when
// maxListPages pages don't reach back to since. The list reads GitHub's own
// store, so an issue made a moment ago is in it (search lags behind).
func (c *Client) IssuesSince(ctx context.Context, repo string, since time.Time) (issues []FiledIssue, complete bool, err error) {
	return c.IssuesSinceBy(ctx, repo, since, "")
}

// issueRow is one row of a repo's issue list (pull requests included).
type issueRow struct {
	Number      int             `json:"number"`
	HTMLURL     string          `json:"html_url"`
	Title       string          `json:"title"`
	Body        *string         `json:"body"`
	CreatedAt   time.Time       `json:"created_at"`
	PullRequest json.RawMessage `json:"pull_request"`
}

// IssuesSinceBy is IssuesSince narrowed to the issues creator opened ("" for
// anyone's), so the pages reach back much further.
func (c *Client) IssuesSinceBy(ctx context.Context, repo string, since time.Time, creator string) (issues []FiledIssue, complete bool, err error) {
	// since filters on updated_at; sorted by creation, the first row made before
	// it ends the window.
	q := url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"desc"},
		"since": {since.UTC().Format(time.RFC3339)}}
	if creator != "" {
		q.Set("creator", creator)
	}
	complete, err = eachRow(ctx, c, "/repos/"+repo+"/issues", q, IssuesPage, func(r issueRow) bool {
		if r.CreatedAt.Before(since) {
			return true
		}
		if len(r.PullRequest) == 0 {
			body := ""
			if r.Body != nil {
				body = *r.Body
			}
			issues = append(issues, FiledIssue{Number: r.Number, URL: r.HTMLURL, Title: r.Title, Body: body, CreatedAt: r.CreatedAt})
		}
		return false
	})
	if err != nil {
		return nil, false, err
	}
	return issues, complete, nil
}

// GetIssue reads a single issue back (used to verify labels stuck).
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	respBody, _, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, http.StatusOK)
	if err != nil {
		return Issue{}, err
	}
	var r issueResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return Issue{}, fmt.Errorf("contrib: decode issue: %w", err)
	}
	return r.toIssue(), nil
}

// Login is the login of the user the token belongs to (GET /user, read once
// per Client).
func (c *Client) Login(ctx context.Context) (string, error) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.login != "" {
		return c.login, nil
	}
	respBody, _, err := c.request(ctx, http.MethodGet, "/user", nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var r struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return "", fmt.Errorf("contrib: decode user: %w", err)
	}
	if r.Login == "" {
		return "", errors.New("contrib: the user has no login")
	}
	c.login = r.Login
	return r.Login, nil
}

// FilingSkew is how far before a filing marker's time (taken from Now) a lookup
// for what it may have made reaches back.
const FilingSkew = 2 * time.Minute

// FilingStale is the age (by GitHub's clock) past which a filing marker no
// longer stops a new filing when the lookup for what it may have made fails in a
// way no later try mends (each caller says which: ErrListIncomplete only grows):
// it files again rather than failing. meta's intake catches a duplicate it has
// already applied, and a marker nothing on the page can clear would block for
// good.
const FilingStale = 24 * time.Hour

// StaleFiling reports whether a marker set at since (GitHub's time) is past
// FilingStale by the GitHub clock the Client learned from GitHub's answers;
// false while GitHub hasn't answered it (the Bridge's own clock is no measure).
func (c *Client) StaleFiling(since time.Time) bool {
	now, ok := c.githubNow()
	return ok && !since.IsZero() && now.Sub(since) > FilingStale
}

// NormalizeBody is an issue body as sent or as GitHub lists it, comparable: the
// list can answer CRLF line ends and trims.
func NormalizeBody(body string) string {
	return strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
}

// NothingFiled reports whether err rules out that a create call made anything:
// GitHub refused it outright (a 4xx answer, a rate limit), or the request never
// left (no connection was made: the name didn't resolve, the dial failed). Any
// other error (a server error, which can come after the issue or gist was made,
// a timeout, a lost answer) may have made it.
func NothingFiled(err error) bool {
	var apiErr *APIError
	var rateErr *RateLimitError
	var opErr *net.OpError
	return (errors.As(err, &apiErr) && apiErr.Status < http.StatusInternalServerError) || errors.As(err, &rateErr) ||
		(errors.As(err, &opErr) && (opErr.Op == "dial" || opErr.Op == "proxyconnect"))
}

// Gist is one of the user's gists as the gist list shows it.
type Gist struct {
	ID          string
	Description string
	CreatedAt   time.Time
	Files       map[string]string // file name -> raw URL
}

// GistsPage is how many gists a page of the gist list holds (the most GitHub
// serves).
const GistsPage = 100

// gistRow is one row of the user's gist list.
type gistRow struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	Files       map[string]struct {
		RawURL string `json:"raw_url"`
	} `json:"files"`
}

// GistsSince lists the user's gists created at or after since. The list's order
// isn't documented, so it is read to its end (since narrows it to the gists
// updated since): complete is false when maxListPages pages don't reach it.
func (c *Client) GistsSince(ctx context.Context, since time.Time) (gists []Gist, complete bool, err error) {
	q := url.Values{"since": {since.UTC().Format(time.RFC3339)}}
	complete, err = eachRow(ctx, c, "/gists", q, GistsPage, func(r gistRow) bool {
		if !r.CreatedAt.Before(since) {
			g := Gist{ID: r.ID, Description: r.Description, CreatedAt: r.CreatedAt, Files: make(map[string]string, len(r.Files))}
			for name, f := range r.Files {
				g.Files[name] = f.RawURL
			}
			gists = append(gists, g)
		}
		return false
	})
	if err != nil {
		return nil, false, err
	}
	return gists, complete, nil
}

// CreateGist uploads files (filename -> content) as a gist with description and
// returns their raw URLs (filename -> raw_url). secret=true makes the gist
// unlisted.
func (c *Client) CreateGist(ctx context.Context, description string, files map[string]string, secret bool) (map[string]string, error) {
	fm := make(map[string]map[string]string, len(files))
	for name, content := range files {
		fm[name] = map[string]string{"content": content}
	}
	reqBody := map[string]any{"description": description, "public": !secret, "files": fm}
	respBody, _, err := c.request(ctx, http.MethodPost, "/gists", reqBody, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	var r struct {
		Files map[string]struct {
			RawURL string `json:"raw_url"`
		} `json:"files"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return nil, fmt.Errorf("contrib: decode gist: %w", err)
	}
	out := make(map[string]string, len(r.Files))
	for name, f := range r.Files {
		out[name] = f.RawURL
	}
	return out, nil
}

// FindIntakePR looks up the intake bot's PR for an issue: the branch
// intake/issue-<n> in the upstream repo. found is false when no such PR exists.
func (c *Client) FindIntakePR(ctx context.Context, repo string, issueNumber int) (PR, bool, error) {
	head := fmt.Sprintf("%s:intake/issue-%d", OwnerOf(repo), issueNumber)
	q := url.Values{"head": {head}, "state": {"all"}}
	respBody, _, err := c.request(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, http.StatusOK)
	if err != nil {
		return PR{}, false, err
	}
	var list []pullResp
	if err := json.Unmarshal(respBody, &list); err != nil {
		return PR{}, false, fmt.Errorf("contrib: decode pulls: %w", err)
	}
	if len(list) == 0 {
		return PR{}, false, nil
	}
	return list[0].toPR(), true, nil
}

// GetPull reads a single pull request (its state and merged flag).
func (c *Client) GetPull(ctx context.Context, repo string, number int) (PR, error) {
	respBody, _, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, http.StatusOK)
	if err != nil {
		return PR{}, err
	}
	var r pullResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return PR{}, fmt.Errorf("contrib: decode pull: %w", err)
	}
	return r.toPR(), nil
}

// ChangedFile is one file a comparison touches. PreviousFilename is set for a
// rename (a pack REBIND renames the file, since a pack's bound is its name).
type ChangedFile struct {
	Filename         string
	PreviousFilename string
}

// Comparison is GitHub's three-dot compare of base...head: the merge base and the
// files changed from it to head.
type Comparison struct {
	MergeBase string
	Files     []ChangedFile
}

// Compare compares base...head (GitHub lists at most 300 files per comparison).
func (c *Client) Compare(ctx context.Context, repo, base, head string) (Comparison, error) {
	respBody, _, err := c.request(ctx, http.MethodGet,
		"/repos/"+repo+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(head), nil, http.StatusOK)
	if err != nil {
		return Comparison{}, err
	}
	var r struct {
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(respBody, &r); err != nil {
		return Comparison{}, fmt.Errorf("contrib: decode compare: %w", err)
	}
	out := Comparison{MergeBase: r.MergeBase.SHA}
	for _, f := range r.Files {
		out.Files = append(out.Files, ChangedFile{Filename: f.Filename, PreviousFilename: f.PreviousFilename})
	}
	return out, nil
}

// FileAt reads a file's raw bytes at ref (a commit sha or branch). found is false
// for a 404 - the file does not exist at that revision - with a nil error. The raw
// media type is used so files over the contents API's 1 MB JSON limit still read.
func (c *Client) FileAt(ctx context.Context, repo, path, ref string) (content []byte, found bool, err error) {
	respBody, _, rerr := c.requestAccept(ctx, "application/vnd.github.raw+json", http.MethodGet,
		"/repos/"+repo+"/contents/"+escapePath(path)+"?ref="+url.QueryEscape(ref), nil, http.StatusOK)
	if rerr != nil {
		var apiErr *APIError
		if errors.As(rerr, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, rerr
	}
	return respBody, true, nil
}

// escapePath escapes each segment of a slash-separated repository path.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// IssueComment is one comment on an issue.
type IssueComment struct {
	Body   string
	Author string
}

// maxCommentPages bounds IssueComments' pagination.
const maxCommentPages = 10

// IssueComments lists an issue's comments, oldest first (the intake poller reads
// the bot's latest verdict comment from it).
func (c *Client) IssueComments(ctx context.Context, repo string, number int) ([]IssueComment, error) {
	var out []IssueComment
	for page := 1; page <= maxCommentPages; page++ {
		respBody, _, err := c.request(ctx, http.MethodGet,
			fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", repo, number, page), nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var list []struct {
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(respBody, &list); err != nil {
			return nil, fmt.Errorf("contrib: decode comments: %w", err)
		}
		for _, cm := range list {
			out = append(out, IssueComment{Body: cm.Body, Author: cm.User.Login})
		}
		if len(list) < 100 {
			break
		}
	}
	return out, nil
}
