// Package correct is the meta.correct job (BRIDGE-PLAN.md Phase 2b, plan D5): an
// admin's edits of a matched book's details, filed to the community metadata
// database's core repo as ONE correction-batch issue with the Bridge's GitHub
// sign-in. It writes nothing into the library and runs no agent: it maps the
// server's fields onto meta's records (contrib.CorrectionBatchIssue), files the
// issue and reports where it is. A filing is guarded per BOOK, not per job: a
// marker written before the issue is opened (meta.correct.filing/<server>/<book>)
// makes the next job for the book, or this one resumed, look for what the
// interrupted filing made first: its own issue is adopted, an issue for the
// same record it can't account for stops it, and it files again only when the
// list holds neither (or can't reach back to a marker over a day old).
package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
)

// Kind is the job kind as the server knows it.
const Kind = "meta.correct"

// failure is a job failure worded for the console as it is.
type failure string

func (f failure) Error() string { return string(f) }

// The ways the job fails that need an admin's words.
const (
	errNoCredential failure = "The Bridge isn't signed in to GitHub: sign in on the Bridge's Accounts tab, then " +
		"suggest the corrections again."
	errRefused failure = "GitHub refused the Bridge's sign-in: sign in again on the Bridge's Accounts tab, then " +
		"suggest the corrections again."
	errMetaOff failure = "The Bridge's community metadata service is off (metadata.base_url), so it can't " +
		"check the book's community record."
	errFiledBefore failure = "An earlier suggestion of corrections for this book stopped while filing them, and " +
		"the Bridge can't tell whether they were filed: look for a [corrections] issue on the community database " +
		"before suggesting them again."
	errSettling failure = "Corrections for this book were being filed moments ago, and GitHub may still be making " +
		"the issue: suggest them again in a minute."
	errNoAnswer failure = "GitHub didn't confirm the issue (no answer in time, or a server error). The Bridge will " +
		"check whether the corrections were filed before filing them again: suggest them again in a minute."
	errRateLimited failure = "GitHub's rate limit for the Bridge's sign-in is reached: suggest the corrections " +
		"again later."
)

// errOtherBatch is a correction batch for the same record with another body
// among the issues made since an interrupted run: the record moved since (the
// batch may be that run's own, composed from the record as it was) or another
// admin's. The Bridge can't tell which, so a person decides, whatever its age.
var errOtherBatch = errors.New("corrects the same record with another body")

// Params are meta.correct's params (the server's bridge.CorrectParams).
type Params struct {
	WorkID      string               `json:"work_id"`
	RecordingID string               `json:"recording_id,omitempty"`
	ASIN        string               `json:"asin,omitempty"`
	ISBN        string               `json:"isbn,omitempty"`
	Region      string               `json:"region,omitempty"`
	Changes     []contrib.Correction `json:"changes"`
	Evidence    string               `json:"evidence"`
	Source      string               `json:"source,omitempty"`
}

// maxParamsBytes bounds the params (25 changes of 500 characters, the evidence).
const maxParamsBytes = 64 << 10

// ParseParams reads the params strictly: an unknown field, in them or in a
// change, is refused, since a field this Bridge doesn't read could change what
// it would file. The values are checked when the issue is composed.
func ParseParams(raw json.RawMessage) (Params, error) {
	var p Params
	if len(raw) > maxParamsBytes {
		return p, errors.New("bad params: too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("bad params: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return p, errors.New("bad params: more than one JSON value")
	}
	p.WorkID = strings.TrimSpace(p.WorkID)
	if p.WorkID == "" {
		return p, errors.New("bad params: work_id is required")
	}
	return p, nil
}

// Result is what a done job reports: the issue it filed, how many changes it
// carries, and which server fields are eligible for meta's bot to apply (auto)
// or go to a maintainer, as known at filing. The job ends at filing: the
// Bridge doesn't follow the issue, so a batch meta merges only in part (it
// comments the rest and labels the issue data:needs-human) is not reported
// back here (plan D8, deferred).
type Result struct {
	IssueURL    string   `json:"issue_url"`
	IssueNumber int      `json:"issue_number"`
	Changes     int      `json:"changes"`
	Auto        []string `json:"auto"`
	Maintainer  []string `json:"maintainer"`
}

// Meta is the community metadata service (metaops.Client).
type Meta interface {
	FreshCoverageForWork(ctx context.Context, workID string) (metaops.Coverage, error)
}

// KV keeps each job's filing (store.DB's settings).
type KV interface {
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Options wire the job.
type Options struct {
	Meta Meta
	// Tokens resolves the GitHub token (contrib.TokenSource); BaseURL is the
	// GitHub API root and CoreRepo the repo the corrections are filed to.
	Tokens   contrib.TokenResolver
	BaseURL  string
	CoreRepo string
	KV       KV
	// ServerID is the pairing's server (job ids repeat across servers).
	ServerID func() string
	Log      *slog.Logger
}

// Handler runs meta.correct.
type Handler struct{ o Options }

// New returns the handler.
func New(o Options) *Handler {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.ServerID == nil {
		o.ServerID = func() string { return "" }
	}
	return &Handler{o: o}
}

// Kind is meta.correct.
func (h *Handler) Kind() string { return Kind }

// Lane is the light lane: a few HTTP calls.
func (h *Handler) Lane() string { return jobs.LaneLight }

// A job's result is kept under its own key (resultKey: a resumed job reports
// it); a filing in progress under its book's (filingKey), JSON filing: since
// (GitHub's time, contrib.Client.Now, when the issue was about to be opened),
// sum (issueSum of the title and body sent) and the job. "" is none.
type filing struct {
	Since time.Time `json:"since"`
	Sum   string    `json:"sum"`
	Job   int64     `json:"job"`
}

// issueSum identifies a batch by its title and body (as sent or as listed).
func issueSum(title, body string) string {
	h := sha256.Sum256([]byte(title + "\x00" + contrib.NormalizeBody(body)))
	return hex.EncodeToString(h[:])
}

func (h *Handler) resultKey(job jobs.Job) string {
	return "meta.correct/" + h.o.ServerID() + "/" + strconv.FormatInt(job.ID, 10)
}

// filingKey is the book's: its library and path when the job names them, else
// the work it corrects.
func (h *Handler) filingKey(job jobs.Job, p Params) string {
	if job.LibraryID != nil && job.Path != nil {
		return "meta.correct.filing/" + h.o.ServerID() + "/" + strconv.FormatInt(*job.LibraryID, 10) + "/" + *job.Path
	}
	return "meta.correct.filing/" + h.o.ServerID() + "/work/" + p.WorkID
}

func (h *Handler) readFiling(ctx context.Context, key string) (f filing, ok bool, err error) {
	v, found, err := h.o.KV.GetSetting(ctx, key)
	if err != nil || !found || v == "" {
		return filing{}, false, err
	}
	if json.Unmarshal([]byte(v), &f) != nil {
		return filing{}, true, nil // unreadable: no time to look from
	}
	return f, true, nil
}

func (h *Handler) clearFiling(ctx context.Context, key string) {
	if err := h.o.KV.SetSetting(context.WithoutCancel(ctx), key, ""); err != nil {
		h.o.Log.Warn("correct: clearing the filing marker failed", "key", key, "err", err)
	}
}

// Run files the corrections, or reports the issue an earlier run filed.
func (h *Handler) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	p, err := ParseParams(job.Params)
	if err != nil {
		return nil, err
	}
	key := h.resultKey(job)
	prev, ok, err := h.o.KV.GetSetting(ctx, key)
	if err != nil {
		return nil, err
	}
	if ok {
		var r Result
		if err := json.Unmarshal([]byte(prev), &r); err == nil && r.IssueNumber > 0 {
			return r, nil
		}
	}
	fkey := h.filingKey(job, p)
	marker, interrupted, err := h.readFiling(ctx, fkey)
	if err != nil {
		return nil, err
	}
	if interrupted && marker.Since.IsZero() {
		return nil, errFiledBefore // no time to look from
	}
	rep.Progress(jobs.Progress{Step: "checking", Message: "Reading the book's community record", Total: 2})
	if h.o.Meta == nil {
		return nil, errMetaOff
	}
	// The credential first: without one there is nothing to fetch the record for.
	token, from, err := h.o.Tokens.Resolve(ctx)
	if errors.Is(err, contrib.ErrNoCredential) {
		return nil, errNoCredential
	}
	if err != nil {
		return nil, fmt.Errorf("resolve the GitHub credential: %w", err)
	}
	cov, err := h.o.Meta.FreshCoverageForWork(ctx, p.WorkID)
	switch {
	case errors.Is(err, metaops.ErrDisabled):
		return nil, errMetaOff
	case errors.Is(err, metaops.ErrWorkNotFound):
		return nil, failure(fmt.Sprintf("The community database has no work %q: match the book again, then suggest the corrections.", p.WorkID))
	case err != nil:
		return nil, fmt.Errorf("could not read the community record: %w", err)
	}
	batch := contrib.CorrectionBatch{WorkID: cov.WorkID, WorkTitle: cov.WorkTitle, RecordingID: recordingFor(p, cov),
		Changes: p.Changes, Evidence: p.Evidence, Source: p.Source}
	title, body, labels, mapped, err := contrib.CorrectionBatchIssue(batch)
	if err != nil {
		return nil, failure("The corrections can't be filed: " + err.Error() + ".")
	}
	cli := contrib.NewClientFor(h.o.BaseURL, token, from, h.o.Tokens)
	rep.Progress(jobs.Progress{Step: "filing", Message: "Filing the corrections", Done: 1, Total: 2})
	sum := issueSum(title, body)
	if interrupted {
		filed, look, err := h.filedSince(ctx, cli, marker, sum, body)
		var apiErr *contrib.APIError
		switch {
		case look == lookOurs:
			h.o.Log.Info("correct: an interrupted filing had filed these corrections", "job", job.ID,
				"issue", filed.Number, "by_job", marker.Job)
			return h.done(ctx, job, key, fkey, contrib.Issue{Number: filed.Number, URL: filed.URL}, mapped, rep), nil
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized:
			return nil, errRefused
		case errors.Is(err, errFilingSettling):
			return nil, errSettling
		case err != nil && (errors.Is(err, errOtherBatch) || !cli.StaleFiling(marker.Since)):
			h.o.Log.Warn("correct: looking for an interrupted filing's issue failed", "job", job.ID, "err", err)
			return nil, errFiledBefore
		case err != nil:
			// The list can't be read, or can't reach back to the marker, and the
			// marker is over a day old (contrib.FilingStale): file again rather than
			// fail for good; meta's intake catches a batch it has applied.
			h.o.Log.Warn("correct: an interrupted filing's issue can't be found or ruled out, and its marker is "+
				"over a day old: filing again", "job", job.ID, "err", err)
		case look == lookEarlier:
			// The interrupted filing was another batch, and it was filed: done.
			h.o.Log.Info("correct: an interrupted filing of other corrections had filed them", "job", job.ID,
				"issue", filed.Number, "by_job", marker.Job)
		}
	}
	// GitHub's time, which the issues' created_at is compared with later.
	raw, err := json.Marshal(filing{Since: cli.Now(ctx), Sum: sum, Job: job.ID})
	if err != nil {
		return nil, err
	}
	if err := h.o.KV.SetSetting(ctx, fkey, string(raw)); err != nil {
		return nil, err
	}
	issue, err := cli.CreateIssue(ctx, h.o.CoreRepo, title, body, labels)
	if err != nil {
		if contrib.NothingFiled(err) {
			// GitHub refused the issue and filed nothing: a later run may file them.
			h.clearFiling(ctx, fkey)
		}
		// Otherwise GitHub may have filed it (a server error can come after the
		// issue is made, and an answer can be lost): the marker stays, and the
		// book's next filing looks for it first.
		var apiErr *contrib.APIError
		var rateErr *contrib.RateLimitError
		switch {
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized:
			return nil, errRefused
		case errors.As(err, &rateErr):
			return nil, errRateLimited
		case !contrib.NothingFiled(err):
			h.o.Log.Warn("correct: GitHub's answer to the issue is lost", "job", job.ID, "err", err)
			return nil, errNoAnswer
		}
		return nil, fmt.Errorf("could not file the corrections: %w", err)
	}
	h.o.Log.Info("correct: filed the corrections", "job", job.ID, "work", cov.WorkID, "issue", issue.Number,
		"changes", len(mapped))
	return h.done(ctx, job, key, fkey, issue, mapped, rep), nil
}

// What a lookup found: none, the job's own batch (lookOurs), or the
// interrupted filing's other batch (lookEarlier: it was filed, so it is done).
type lookup int

const (
	lookNone lookup = iota
	lookOurs
	lookEarlier
)

// errFilingSettling is a lookup too soon after the marker (contrib.FilingSettle).
var errFilingSettling = errors.New("the interrupted filing was moments ago")

// filedSince looks for what an interrupted filing made: a correction batch
// made since the marker (less contrib.FilingSkew), first among the issues read
// even when a later page failed. The job's own body is lookOurs; the marker's
// (another job's other batch, which accounts for that filing whatever else is
// listed) lookEarlier. With neither, a batch for the same
// record with another body is errOtherBatch, a list that failed or can't reach
// back is that error (contrib.ErrListIncomplete), and a marker under
// contrib.FilingSettle old is errFilingSettling.
func (h *Handler) filedSince(ctx context.Context, cli *contrib.Client, m filing, sum, body string) (contrib.FiledIssue, lookup, error) {
	issues, complete, err := cli.IssuesSince(ctx, h.o.CoreRepo, m.Since.Add(-contrib.FilingSkew))
	body = contrib.NormalizeBody(body)
	record, _, _ := strings.Cut(body, "\n\n### ") // the Record section
	other, earlier := 0, contrib.FiledIssue{}     // the newest same-record batch with another body
	for _, is := range issues {
		if !strings.HasPrefix(is.Title, contrib.CorrectionsTitleTag) {
			continue
		}
		got := issueSum(is.Title, is.Body)
		switch {
		case got == sum:
			return is, lookOurs, nil
		case got == m.Sum && earlier.Number == 0:
			earlier = is
			continue
		}
		if r, _, _ := strings.Cut(contrib.NormalizeBody(is.Body), "\n\n### "); r == record && other == 0 {
			other = is.Number
		}
	}
	switch {
	case earlier.Number != 0:
		// The interrupted filing's own batch: what it made is accounted for,
		// whatever other batches for the record are listed.
		return earlier, lookEarlier, nil
	case other != 0:
		return contrib.FiledIssue{}, lookNone, fmt.Errorf("issue #%d %w", other, errOtherBatch)
	case err != nil:
		return contrib.FiledIssue{}, lookNone, err
	case !complete:
		return contrib.FiledIssue{}, lookNone, contrib.ErrListIncomplete
	case cli.Now(ctx).Sub(m.Since) < contrib.FilingSettle:
		return contrib.FiledIssue{}, lookNone, errFilingSettling
	}
	return contrib.FiledIssue{}, lookNone, nil
}

// done records the filed issue as the job's result and reports it.
func (h *Handler) done(ctx context.Context, job jobs.Job, key, fkey string, issue contrib.Issue, mapped []contrib.MappedChange, rep *jobs.Reporter) Result {
	out := Result{IssueURL: issue.URL, IssueNumber: issue.Number, Changes: len(mapped), Auto: []string{}, Maintainer: []string{}}
	for _, m := range mapped {
		if m.Auto {
			out.Auto = append(out.Auto, m.ServerField)
		} else {
			out.Maintainer = append(out.Maintainer, m.ServerField)
		}
	}
	if raw, err := json.Marshal(out); err == nil {
		if err := h.o.KV.SetSetting(context.WithoutCancel(ctx), key, string(raw)); err != nil {
			h.o.Log.Warn("correct: recording the filed issue failed", "job", job.ID, "err", err)
		}
	}
	h.clearFiling(ctx, fkey) // the filing is confirmed

	rep.Progress(jobs.Progress{Step: "done", Message: "Filed", Done: 2, Total: 2})
	return out
}

// recordingFor is the recording the changes address: the job's own, else the
// one the job's ASIN or ISBN picks out of the work's (none, or more than one,
// is "": meta then hands the recording's fields to a maintainer).
func recordingFor(p Params, cov metaops.Coverage) string {
	if id := strings.TrimSpace(p.RecordingID); id != "" {
		return id
	}
	asin, isbn := strings.TrimSpace(p.ASIN), normISBN(p.ISBN)
	if asin == "" && isbn == "" {
		return ""
	}
	found := ""
	for _, r := range cov.Recordings {
		hit := false
		for _, a := range r.ASINs {
			if asin != "" && strings.EqualFold(a.ASIN, asin) && (p.Region == "" || strings.EqualFold(a.Region, p.Region)) {
				hit = true
			}
		}
		for _, i := range r.ISBNs {
			if isbn != "" && normISBN(i) == isbn {
				hit = true
			}
		}
		if hit {
			if found != "" && found != r.ID {
				return ""
			}
			found = r.ID
		}
	}
	return found
}

func normISBN(s string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.TrimSpace(s)))
}
