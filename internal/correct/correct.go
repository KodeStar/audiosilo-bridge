// Package correct is the meta.correct job (BRIDGE-PLAN.md Phase 2b, plan D5): an
// admin's edits of a matched book's details, filed to the community metadata
// database's core repo as ONE correction-batch issue with the Bridge's GitHub
// sign-in. It writes nothing into the library and runs no agent: it maps the
// server's fields onto meta's records (contrib.CorrectionBatchIssue), files the
// issue and reports where it is. A filing is guarded per REQUEST (the changes the
// admin asked for, whatever the evidence says), not per job: a marker written
// before the issue is opened (meta.correct.filing/<server>/<request>) makes the
// next job asking for the same corrections, or this one resumed, look for what
// the interrupted filing made first: its issue is adopted (as sent, whatever the
// community record or the evidence says now; by another job only while the
// marker is under a day old), an issue for the same record it can't account for
// stops it once for a person to decide, and it files again only when the list
// holds neither (or can't reach back to a marker over a day old).
package correct

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/store"
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
	errUnrefreshed failure = "The Bridge's GitHub sign-in couldn't be refreshed (" + contrib.UnrefreshedWhy +
		"): suggest the corrections again once GitHub answers."
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
// since an interrupted filing, with neither the request's sum nor the marker's
// (filedSince): another admin's, the filing's own issue edited since, or, for
// another job past a day, the filing's own as it was composed. The Bridge can't
// tell which, so a person decides. whole says the list was read back to the marker
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
	// The record's ids in the one form the filing key and recordingFor read.
	p.RecordingID = strings.TrimSpace(p.RecordingID)
	p.ASIN = strings.ToUpper(strings.TrimSpace(p.ASIN))
	p.ISBN = normISBN(p.ISBN)
	p.Region = strings.ToLower(p.Region)
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
	IntakeNote  string   `json:"intake_note,omitempty"`
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
	EarlierBookJobs(ctx context.Context, server, kind string, library int64, path string, before int64) ([]store.ServerJob, error)
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
	Since     time.Time `json:"since"`
	Sum       string    `json:"sum"`
	Job       int64     `json:"job"`
	FiledWith string    `json:"filed_with,omitempty"` // the credential source (contrib.From*)
}

// same reports whether two markers are one (a JSON round trip keeps the time to
// the nanosecond).
func (f filing) same(g filing) bool {
	return f.Since.Equal(g.Since) && f.Sum == g.Sum && f.Job == g.Job
}

// resultKey is the job's, under the server that handed it out (job ids repeat
// across servers): never the pairing's now, which may be another server's.
func (h *Handler) resultKey(job jobs.Job) string {
	return "meta.correct/" + job.KeyServer(h.o.ServerID()) + "/" + strconv.FormatInt(job.ID, 10)
}

// filingKey is the request's: what the admin asked to change, nothing more.
// The sha256 of the record the changes address (the work, and the recording id,
// or else the ASIN, ISBN and region that pick one: recordingFor) and the changes
// as a set (each field and its corrected value, sorted). Not the evidence (the
// console fills it with a default line naming the server's version, in the
// admin's language, each time its dialog opens) or the source stamp; not each
// change's current value (it moves when the community applies a fix); not the
// book (which can move) nor the composed issue (whose title is the work's live
// title). So the same corrections suggested again, reworded or reordered, or
// this job resumed, find the marker, and other corrections never share or
// overwrite it. Under the job's own server (resultKey). p is as ParseParams
// gives it, its ids in one form.
func (h *Handler) filingKey(job jobs.Job, p Params) string {
	// Its own type, not contrib.Correction: a field the wire gains later must not
	// change every request's key.
	type change struct {
		Field     string `json:"field"`
		Corrected string `json:"corrected"`
	}
	changes := make([]change, len(p.Changes))
	for i, c := range p.Changes {
		changes[i] = change{c.Field, strings.TrimSpace(c.Corrected)}
	}
	slices.SortFunc(changes, func(a, b change) int {
		return cmp.Or(strings.Compare(a.Field, b.Field), strings.Compare(a.Corrected, b.Corrected))
	})
	raw, _ := json.Marshal(struct {
		correctionRecord
		Changes []change `json:"changes"`
	}{p.record(), changes})
	sum := sha256.Sum256(raw)
	return "meta.correct.filing/" + job.KeyServer(h.o.ServerID()) + "/" + hex.EncodeToString(sum[:])
}

type correctionRecord struct {
	WorkID      string `json:"work_id"`
	RecordingID string `json:"recording_id"`
	ASIN        string `json:"asin"`
	ISBN        string `json:"isbn"`
	Region      string `json:"region"`
}

func (p Params) record() correctionRecord {
	if p.RecordingID != "" {
		return correctionRecord{WorkID: p.WorkID, RecordingID: p.RecordingID}
	}
	return correctionRecord{WorkID: p.WorkID, ASIN: p.ASIN, ISBN: p.ISBN, Region: p.Region}
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

// Keep the last submitted batch for this book beyond the jobs list's pruning.
// The issue result and any interrupted filing stay in their existing keys.
func (h *Handler) bookKey(job jobs.Job) string {
	if job.LibraryID == nil || job.Path == nil {
		return ""
	}
	return fmt.Sprintf("meta.correct.book/%s/%d/%s", job.KeyServer(h.o.ServerID()), *job.LibraryID, *job.Path)
}

type bookBatch struct {
	ID     int64           `json:"job_id"`
	Params json.RawMessage `json:"params"`
}

type priorRequest struct {
	result  Result
	key     string
	marker  filing
	changes []contrib.Correction
}

// smallerRetry looks for a strict subset of an earlier batch for this book.
// The earlier claim carries its full request, including in older databases.
// A confirmed issue has a result. An interrupted POST has its exact marker.
func (h *Handler) smallerRetry(ctx context.Context, job jobs.Job, p Params) (priorRequest, error) {
	key := h.bookKey(job)
	if key == "" || len(p.Changes) == 0 {
		return priorRequest{}, nil
	}
	rows, err := h.o.KV.EarlierBookJobs(ctx, job.KeyServer(h.o.ServerID()), Kind, *job.LibraryID, *job.Path, job.ID)
	if err != nil {
		return priorRequest{}, err
	}
	saved, ok, err := h.o.KV.GetSetting(ctx, key)
	if err != nil {
		return priorRequest{}, err
	}
	var batch bookBatch
	if ok && json.Unmarshal([]byte(saved), &batch) == nil && batch.ID <= job.ID && batch.ID > 0 {
		rows = append([]store.ServerJob{{ID: batch.ID, Params: batch.Params}}, rows...)
	}
	for _, row := range rows {
		old, err := ParseParams(row.Params)
		if err != nil || old.record() != p.record() || len(old.Changes) <= len(p.Changes) || !containsChanges(old.Changes, p.Changes) {
			continue
		}
		var out Result
		if json.Unmarshal(row.Result, &out) == nil && out.IssueNumber > 0 {
			return priorRequest{result: out}, nil
		}
		previous := job
		previous.ID = row.ID
		if out, ok, err := h.savedResult(ctx, previous); err != nil || ok {
			return priorRequest{result: out}, err
		}
		key := h.filingKey(job, old)
		m, found, err := h.readFiling(ctx, key)
		if err != nil {
			return priorRequest{}, err
		}
		if found {
			return priorRequest{key: key, marker: m, changes: old.Changes}, nil
		}
	}
	return priorRequest{}, nil
}

// credential resolves the GitHub credential, a failure worded for the console.
func (h *Handler) credential(ctx context.Context) (token, from string, err error) {
	token, from, err = h.o.Tokens.Resolve(ctx)
	switch {
	case errors.Is(err, contrib.ErrSignInUnrefreshed):
		return "", "", errUnrefreshed
	case errors.Is(err, contrib.ErrNoCredential):
		return "", "", errNoCredential
	case err != nil:
		return "", "", fmt.Errorf("resolve the GitHub credential: %w", err)
	}
	return token, from, nil
}

// issueOpen reports whether issue number on the core repo is still open: an
// earlier batch's confirmed issue answers a smaller retry only while it is. One
// GitHub no longer has (deleted, or moved away) is not.
func (h *Handler) issueOpen(ctx context.Context, number int) (bool, error) {
	token, from, err := h.credential(ctx)
	if err != nil {
		return false, err
	}
	is, err := contrib.NewClientFor(h.o.BaseURL, token, from, h.o.Tokens).GetIssue(ctx, h.o.CoreRepo, number)
	var apiErr *contrib.APIError
	var rateErr *contrib.RateLimitError
	switch {
	case err == nil:
		return is.State != "closed", nil
	case contrib.Unauthorized(err):
		return false, errRefused
	case errors.As(err, &rateErr):
		return false, errRateLimited
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusGone):
		return false, nil
	}
	return false, fmt.Errorf("could not read the earlier batch's issue #%d: %w", number, err)
}

func containsChanges(all, remaining []contrib.Correction) bool {
	for _, c := range remaining {
		if !slices.ContainsFunc(all, func(old contrib.Correction) bool {
			return old.Field == c.Field && strings.TrimSpace(old.Corrected) == strings.TrimSpace(c.Corrected)
		}) {
			return false
		}
	}
	return true
}

// savedResult is the issue a run of job confirmed, kept under its resultKey.
func (h *Handler) savedResult(ctx context.Context, job jobs.Job) (Result, bool, error) {
	raw, ok, err := h.o.KV.GetSetting(ctx, h.resultKey(job))
	if err != nil || !ok {
		return Result{}, false, err
	}
	var r Result
	if json.Unmarshal([]byte(raw), &r) != nil || r.IssueNumber <= 0 {
		return Result{}, false, nil
	}
	return r, true, nil
}

// Run files the corrections, or reports the issue an earlier run filed.
func (h *Handler) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	p, err := ParseParams(job.Params)
	if err != nil {
		return nil, err
	}
	if r, ok, err := h.savedResult(ctx, job); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return r, nil
	}
	fkey := h.filingKey(job, p)
	marker, interrupted, err := h.readFiling(ctx, fkey)
	if err != nil {
		return nil, err
	}
	lookupKey := fkey
	var prior priorRequest
	if !interrupted {
		prior, err = h.smallerRetry(ctx, job, p)
		if err != nil {
			return nil, err
		}
		if prior.result.IssueNumber > 0 {
			open, err := h.issueOpen(ctx, prior.result.IssueNumber)
			if err != nil {
				return nil, err
			}
			if open {
				h.saveResult(ctx, job, prior.result)
				return prior.result, nil
			}
			// Closed: a maintainer has dealt with that batch, so the changes
			// suggested again are a new request, filed afresh.
			h.o.Log.Info("correct: the earlier batch's issue is closed: filing these corrections again", "job", job.ID,
				"issue", prior.result.IssueNumber)
		}
		if prior.key != "" {
			lookupKey, marker, interrupted = prior.key, prior.marker, true
		}
	}
	if interrupted && marker.Since.IsZero() {
		// No time to look from: a person decides, once (the next suggestion files).
		h.clearFiling(ctx, lookupKey, marker)
		return nil, errFiledBefore
	}
	rep.Progress(jobs.Progress{Step: "checking", Message: "Reading the book's community record", Total: 2})
	if h.o.Meta == nil {
		return nil, errMetaOff
	}
	// The credential first: without one there is nothing to fetch the record for.
	token, from, err := h.credential(ctx)
	if err != nil {
		return nil, err
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
		// The request's own sum: filedSince adopts the marker's (an earlier, larger
		// batch's too) only while that marker is this job's or under a day old.
		filed, err := h.filedSince(ctx, cli, marker, job.ID, sum, body)
		var other otherBatch
		var rateErr *contrib.RateLimitError
		switch {
		case filed.Number != 0:
			filedChanges := mapped
			if prior.key != "" && contrib.IssueSum(filed.Title, filed.Body) == marker.Sum {
				// Only the earlier batch can confirm its original number of changes.
				original := batch
				original.Changes = prior.changes
				if filedChanges, err = contrib.MapCorrections(original); err != nil {
					return nil, err
				}
			}
			h.o.Log.Info("correct: an interrupted filing had filed these corrections", "job", job.ID,
				"issue", filed.Number, "by_job", marker.Job)
			return h.done(ctx, job, lookupKey, marker, contrib.Issue{Number: filed.Number, URL: filed.URL}, filedChanges, rep), nil
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
				h.clearFiling(ctx, lookupKey, marker)
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
		case lookupKey != fkey:
			// The list reached back to the earlier batch's marker and holds nothing
			// it made: that marker goes, so a later retry of that batch files it
			// rather than being told this one's issue was filed before.
			h.clearFiling(ctx, lookupKey, marker)
		}
	}
	// GitHub's time, which the issues' created_at is compared with later.
	mine := filing{Since: cli.Now(ctx), Sum: sum, Job: job.ID, FiledWith: from}
	raw, err := json.Marshal(mine)
	if err != nil {
		return nil, err
	}
	if key := h.bookKey(job); key != "" {
		snapshot, err := json.Marshal(bookBatch{ID: job.ID, Params: job.Params})
		if err != nil {
			return nil, err
		}
		if err := h.o.KV.SetSetting(ctx, key, string(snapshot)); err != nil {
			return nil, err
		}
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
// is the work's live title, the recording comes from the live record, and the
// evidence and source stamp may have been reworded since), first
// among the issues read even when a later page failed. The marker's sum is
// another job's to adopt only while the marker is under contrib.FilingStale old:
// later, the same changes suggested again (with a source asked for, say) are no
// retry of the lost answer, whose issue may be closed by then. With neither, a
// batch for the same record with another sum is an otherBatch, a list that
// failed or can't reach back is that error (contrib.ErrListIncomplete), and a
// marker under contrib.FilingSettle old is contrib.ErrFilingSettling.
func (h *Handler) filedSince(ctx context.Context, cli *contrib.Client, m filing, job int64, sum, body string) (contrib.FiledIssue, error) {
	issues, complete, err := cli.IssuesSince(ctx, h.o.CoreRepo, m.Since.Add(-contrib.FilingSkew))
	adopt := m.Sum
	if m.Job != job && cli.StaleFiling(m.Since) {
		adopt = sum
	}
	record := recordSection(body)
	other := 0 // the newest same-record batch with another sum
	for _, is := range issues {
		if !strings.HasPrefix(is.Title, contrib.CorrectionsTitleTag) {
			continue
		}
		if got := contrib.IssueSum(is.Title, is.Body); got == sum || got == adopt {
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
	out := Result{IssueURL: issue.URL, IntakeNote: contrib.IntakeNote(f.FiledWith), IssueNumber: issue.Number, Changes: len(mapped), Auto: []string{}, Maintainer: []string{}}
	for _, m := range mapped {
		if m.Auto {
			out.Auto = append(out.Auto, m.ServerField)
		} else {
			out.Maintainer = append(out.Maintainer, m.ServerField)
		}
	}
	saved := h.saveResult(ctx, job, out)
	if f.Job != 0 && f.Job != job.ID {
		original := job
		original.ID = f.Job
		saved = h.saveResult(ctx, original, out) && saved
	}
	if saved {
		h.clearFiling(ctx, fkey, f)
	}
	rep.Progress(jobs.Progress{Step: "done", Message: "Filed", Done: 2, Total: 2})
	return out
}

func (h *Handler) saveResult(ctx context.Context, job jobs.Job, out Result) bool {
	raw, _ := json.Marshal(out)
	if err := h.o.KV.SetSetting(context.WithoutCancel(ctx), h.resultKey(job), string(raw)); err != nil {
		h.o.Log.Warn("correct: recording the filed issue failed", "job", job.ID, "err", err)
		return false
	}
	return true
}

// recordingFor is the recording the changes address: the job's own, else the
// one the job's ASIN (in its region, when it names one) or ISBN picks out of the
// work's (none, or more than one, is "": meta then hands the recording's fields
// to a maintainer). p is as ParseParams gives it, its ids in one form.
func recordingFor(p Params, cov metaops.Coverage) string {
	if p.RecordingID != "" {
		return p.RecordingID
	}
	asin, isbn := p.ASIN, p.ISBN
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
