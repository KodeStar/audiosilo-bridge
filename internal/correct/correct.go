// Package correct is the meta.correct job (BRIDGE-PLAN.md Phase 2b, plan D5): an
// admin's edits of a matched book's details, filed to the community metadata
// database's core repo as ONE correction-batch issue with the Bridge's GitHub
// sign-in. It writes nothing into the library and runs no agent: it maps the
// server's fields onto meta's records (contrib.CorrectionBatchIssue), files the
// issue and reports where it is. A filing is guarded per REQUEST (what the admin
// asked to file), not per job: a marker written before the issue is opened
// (meta.correct.filing/<server>/<request>) makes the next job asking for the same
// corrections, or this one resumed, look for what the interrupted filing made
// first: its issue is adopted (as sent, whatever the community record says now),
// an issue for the same record it can't account for stops it once for a person to
// decide, and it files again only when the list holds neither (or can't reach
// back to a marker over a day old).
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
	errFiledBefore failure = "An earlier suggestion of these corrections stopped while filing them, and the Bridge " +
		"can't tell whether they were filed: look for a [corrections] issue on the community database before " +
		"suggesting them again."
	errSettling failure = "These corrections were being filed moments ago, and GitHub may still be making the " +
		"issue: suggest them again in a minute."
	errNoAnswer failure = "GitHub didn't confirm the issue (no answer in time, or a server error). The Bridge will " +
		"check whether the corrections were filed before filing them again: suggest them again in a minute."
	errRateLimited failure = "GitHub's rate limit for the Bridge's sign-in is reached: suggest the corrections " +
		"again later."
)

// otherBatch is a correction batch for the same record among the issues made
// since an interrupted filing, with neither the request's sum nor the marker's:
// another admin's, or the filing's own issue edited since. The Bridge can't tell
// which, so a person decides. whole says the list was read back to the marker
// and the marker has settled, so that batch (or nothing) is all the filing can
// have made: once the person is told, the marker goes. err is why the list
// stopped short (a refusal or a rate limit is reported as itself).
type otherBatch struct {
	number int
	whole  bool
	err    error
}

func (e otherBatch) Error() string {
	return fmt.Sprintf("issue #%d corrects the same record with another body", e.number)
}

func (e otherBatch) Unwrap() error { return e.err }

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

// KV keeps each job's result and each request's filing marker (store.DB's
// settings).
type KV interface {
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
	DeleteSetting(ctx context.Context, key string) error
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
// it); a filing in progress under its request's (filingKey), JSON filing: since
// (GitHub's time, contrib.Client.Now, when the issue was about to be opened),
// sum (contrib.IssueSum of the title and body sent) and the job. Absent (or "")
// is none.
type filing struct {
	Since time.Time `json:"since"`
	Sum   string    `json:"sum"`
	Job   int64     `json:"job"`
}

// same reports whether two markers are one (a JSON round trip keeps the time to
// the nanosecond).
func (f filing) same(g filing) bool {
	return f.Since.Equal(g.Since) && f.Sum == g.Sum && f.Job == g.Job
}

func (h *Handler) resultKey(job jobs.Job) string {
	return "meta.correct/" + h.o.ServerID() + "/" + strconv.FormatInt(job.ID, 10)
}

// filingKey is the request's: the sha256 of the params less the server's source
// stamp. What the admin asked to file, not the book (which can move) or the
// composed issue (whose title is the work's live title, which a filed title
// correction changes), so the same corrections suggested again, or this job
// resumed, find the marker, and other corrections never share or overwrite it.
func (h *Handler) filingKey(p Params) string {
	p.Source = ""
	raw, _ := json.Marshal(p) // strings only: it can't fail
	sum := sha256.Sum256(raw)
	return "meta.correct.filing/" + h.o.ServerID() + "/" + hex.EncodeToString(sum[:])
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

// clearFiling deletes the request's marker once what it guards is settled, but
// only while it is still f: another run for the request may have written its own
// since. A marker kept by mistake costs a lookup, one deleted by mistake a
// duplicate, so a marker that can't be read back is kept.
func (h *Handler) clearFiling(ctx context.Context, key string, f filing) {
	ctx = context.WithoutCancel(ctx)
	cur, ok, err := h.readFiling(ctx, key)
	if err != nil || !ok || !cur.same(f) {
		return
	}
	// Deleted, not emptied: the keys are per request, and would pile up.
	if err := h.o.KV.DeleteSetting(ctx, key); err != nil {
		h.o.Log.Warn("correct: clearing the filing marker failed", "key", key, "err", err)
	}
}

// Run files the corrections, or reports the issue an earlier run filed.
func (h *Handler) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	p, err := ParseParams(job.Params)
	if err != nil {
		return nil, err
	}
	prev, ok, err := h.o.KV.GetSetting(ctx, h.resultKey(job))
	if err != nil {
		return nil, err
	}
	if ok {
		var r Result
		if err := json.Unmarshal([]byte(prev), &r); err == nil && r.IssueNumber > 0 {
			return r, nil
		}
	}
	fkey := h.filingKey(p)
	marker, interrupted, err := h.readFiling(ctx, fkey)
	if err != nil {
		return nil, err
	}
	if interrupted && marker.Since.IsZero() {
		// No time to look from: a person decides, once (the next suggestion files).
		h.clearFiling(ctx, fkey, marker)
		return nil, errFiledBefore
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
	sum := contrib.IssueSum(title, body)
	if interrupted {
		filed, err := h.filedSince(ctx, cli, marker, sum, body)
		var other otherBatch
		var rateErr *contrib.RateLimitError
		switch {
		case filed.Number != 0:
			h.o.Log.Info("correct: an interrupted filing had filed these corrections", "job", job.ID,
				"issue", filed.Number, "by_job", marker.Job)
			return h.done(ctx, job, fkey, marker, contrib.Issue{Number: filed.Number, URL: filed.URL}, mapped, rep), nil
		case contrib.Unauthorized(err):
			return nil, errRefused
		case errors.As(err, &rateErr):
			return nil, errRateLimited
		case errors.Is(err, contrib.ErrFilingSettling):
			return nil, errSettling
		case errors.As(err, &other):
			h.o.Log.Warn("correct: an interrupted filing's issue can't be told from another batch", "job", job.ID,
				"err", err, "whole", other.whole)
			if other.whole {
				h.clearFiling(ctx, fkey, marker)
			}
			return nil, errFiledBefore
		case err != nil && !cli.StaleFiling(marker.Since):
			h.o.Log.Warn("correct: looking for an interrupted filing's issue failed", "job", job.ID, "err", err)
			return nil, errFiledBefore
		case err != nil:
			// The list can't be read, or can't reach back to the marker, and the
			// marker is over a day old (contrib.FilingStale): file again rather than
			// fail for good; meta's intake catches a batch it has applied.
			h.o.Log.Warn("correct: an interrupted filing's issue can't be found or ruled out, and its marker is "+
				"over a day old: filing again", "job", job.ID, "err", err)
		}
	}
	// GitHub's time, which the issues' created_at is compared with later.
	mine := filing{Since: cli.Now(ctx), Sum: sum, Job: job.ID}
	raw, err := json.Marshal(mine)
	if err != nil {
		return nil, err
	}
	if err := h.o.KV.SetSetting(ctx, fkey, string(raw)); err != nil {
		return nil, err
	}
	issue, err := cli.CreateIssue(ctx, h.o.CoreRepo, title, body, labels)
	if err != nil {
		if !contrib.NothingFiled(err) {
			// GitHub may have filed it (a server error can come after the issue is
			// made, and an answer can be lost): the marker stays, and the request's
			// next filing looks for it first.
			h.o.Log.Warn("correct: GitHub's answer to the issue is lost", "job", job.ID, "err", err)
			return nil, errNoAnswer
		}
		// GitHub refused the issue and filed nothing: a later run may file them.
		h.clearFiling(ctx, fkey, mine)
		var rateErr *contrib.RateLimitError
		switch {
		case contrib.Unauthorized(err):
			return nil, errRefused
		case errors.As(err, &rateErr):
			return nil, errRateLimited
		}
		return nil, fmt.Errorf("could not file the corrections: %w", err)
	}
	h.o.Log.Info("correct: filed the corrections", "job", job.ID, "work", cov.WorkID, "issue", issue.Number,
		"changes", len(mapped))
	return h.done(ctx, job, fkey, mine, issue, mapped, rep), nil
}

// recordSection is a batch body's Record section (what it corrects).
func recordSection(body string) string {
	record, _, _ := strings.Cut(contrib.NormalizeBody(body), "\n\n### ")
	return record
}

// filedSince looks for what an interrupted filing of the request made: a
// correction batch made since the marker (less contrib.FilingSkew) with the
// job's own sum, or the marker's (the request as it was composed then: the title
// is the work's live title, and the recording comes from the live record), first
// among the issues read even when a later page failed. With neither, a batch for
// the same record with another sum is an otherBatch, a list that failed or can't
// reach back is that error (contrib.ErrListIncomplete), and a marker under
// contrib.FilingSettle old is contrib.ErrFilingSettling.
func (h *Handler) filedSince(ctx context.Context, cli *contrib.Client, m filing, sum, body string) (contrib.FiledIssue, error) {
	issues, complete, err := cli.IssuesSince(ctx, h.o.CoreRepo, m.Since.Add(-contrib.FilingSkew))
	record := recordSection(body)
	other := 0 // the newest same-record batch with another sum
	for _, is := range issues {
		if !strings.HasPrefix(is.Title, contrib.CorrectionsTitleTag) {
			continue
		}
		if got := contrib.IssueSum(is.Title, is.Body); got == sum || got == m.Sum {
			return is, nil
		}
		if other == 0 && recordSection(is.Body) == record {
			other = is.Number
		}
	}
	switch {
	case other != 0:
		whole := complete && !cli.Settling(ctx, m.Since) // a failed list is never complete
		return contrib.FiledIssue{}, otherBatch{number: other, whole: whole, err: err}
	case err != nil:
		return contrib.FiledIssue{}, err
	case !complete:
		return contrib.FiledIssue{}, contrib.ErrListIncomplete
	case cli.Settling(ctx, m.Since):
		return contrib.FiledIssue{}, contrib.ErrFilingSettling
	}
	return contrib.FiledIssue{}, nil
}

// done records the filed issue as the job's result and reports it. The
// request's marker f goes only once the result is recorded: a run that resumes
// after a failed write still looks, and adopts the issue.
func (h *Handler) done(ctx context.Context, job jobs.Job, fkey string, f filing, issue contrib.Issue,
	mapped []contrib.MappedChange, rep *jobs.Reporter) Result {
	out := Result{IssueURL: issue.URL, IssueNumber: issue.Number, Changes: len(mapped), Auto: []string{}, Maintainer: []string{}}
	for _, m := range mapped {
		if m.Auto {
			out.Auto = append(out.Auto, m.ServerField)
		} else {
			out.Maintainer = append(out.Maintainer, m.ServerField)
		}
	}
	raw, _ := json.Marshal(out) // strings and ints: it can't fail
	if err := h.o.KV.SetSetting(context.WithoutCancel(ctx), h.resultKey(job), string(raw)); err != nil {
		h.o.Log.Warn("correct: recording the filed issue failed", "job", job.ID, "err", err)
	} else {
		h.clearFiling(ctx, fkey, f) // the filing is confirmed
	}
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
