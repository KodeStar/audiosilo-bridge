// Package correct is the meta.correct job (BRIDGE-PLAN.md Phase 2b, plan D5): an
// admin's edits of a matched book's details, filed to the community metadata
// database's core repo as ONE correction-batch issue with the Bridge's GitHub
// sign-in. It writes nothing into the library and runs no agent: it maps the
// server's fields onto meta's records (contrib.CorrectionBatchIssue), files the
// issue and reports where it is, once per job (a resumed job never files twice).
package correct

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

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
	errFiledBefore failure = "An earlier run of this job stopped while filing the corrections, so they may be filed " +
		"already: look for a [corrections] issue on the community database before suggesting them again."
)

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

// Result is what a done job reports: the issue, how many changes it carries,
// and which server fields meta's bot applies (auto) or a maintainer does.
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

// The kv values of a job's filing: "filing" before the issue is opened, the
// result's JSON after.
const filingMarker = "filing"

func (h *Handler) key(job jobs.Job) string {
	return "meta.correct/" + h.o.ServerID() + "/" + strconv.FormatInt(job.ID, 10)
}

// Run files the corrections, or reports the issue an earlier run filed.
func (h *Handler) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	p, err := ParseParams(job.Params)
	if err != nil {
		return nil, err
	}
	key := h.key(job)
	switch prev, ok, err := h.o.KV.GetSetting(ctx, key); {
	case err != nil:
		return nil, err
	case ok && prev == filingMarker:
		return nil, errFiledBefore
	case ok:
		var r Result
		if err := json.Unmarshal([]byte(prev), &r); err == nil && r.IssueNumber > 0 {
			return r, nil
		}
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
	if err := h.o.KV.SetSetting(ctx, key, filingMarker); err != nil {
		return nil, err
	}
	issue, err := cli.CreateIssue(ctx, h.o.CoreRepo, title, body, labels)
	if err != nil {
		var apiErr *contrib.APIError
		var rateErr *contrib.RateLimitError
		if errors.As(err, &apiErr) || errors.As(err, &rateErr) {
			// GitHub answered and filed nothing: a later run may file them.
			_ = h.o.KV.SetSetting(context.WithoutCancel(ctx), key, "")
		}
		// Otherwise GitHub may have filed it before the answer was lost: the
		// marker stays, and a later run says so rather than filing twice.
		if apiErr != nil && apiErr.Status == http.StatusUnauthorized {
			return nil, errRefused
		}
		return nil, fmt.Errorf("could not file the corrections: %w", err)
	}
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
	h.o.Log.Info("correct: filed the corrections", "job", job.ID, "work", cov.WorkID, "issue", issue.Number,
		"changes", len(mapped))
	rep.Progress(jobs.Progress{Step: "done", Message: "Filed", Done: 2, Total: 2})
	return out, nil
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
