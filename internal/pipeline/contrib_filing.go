package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
)

// A create call to GitHub (an issue, a gist) that outlives the client's timeout,
// or whose answer is lost, may still have made what it asked for. The client
// never sends a POST twice, so the duplicate would come from the next submit
// (the stage's retry, a second "Share with the community"). Before each create a
// submit records a filing marker for the dimension; the next submit that finds
// one looks among what the user made since then and adopts a match, and makes
// another only when that list is complete and holds none.

// ErrFilingUnknown is a dimension an earlier submit may have filed that the
// Bridge can't find or rule out (GitHub's list didn't reach back far enough, or
// couldn't be read): filing again could make a duplicate.
var ErrFilingUnknown = errors.New("an earlier try may have filed it, and the Bridge can't tell")

// errListIncomplete is a lookup whose one page doesn't reach back to the marker.
var errListIncomplete = errors.New("more were made since than one page lists")

// filing is a dimension's marker (the settings KV, filingKey): what its last
// submit was making when it stopped.
type filing struct {
	// Payload is the sha256 of the sidecar the calls carried: a marker for other
	// content is dropped (that content is filed afresh).
	Payload string `json:"payload"`
	// GistSince is set while a gist POST may have reached GitHub (since then).
	GistSince time.Time `json:"gist_since,omitzero"`
	// Gist is the raw URL of the gist made for Payload (an oversized body links it).
	Gist string `json:"gist,omitempty"`
	// IssueSince is set while an issue POST may have reached GitHub.
	IssueSince time.Time `json:"issue_since,omitzero"`
}

func filingKey(bookID int64, kind string) string {
	return "contrib.filing/" + strconv.FormatInt(bookID, 10) + "/" + kind
}

// readFiling is the dimension's marker for payload sum (a zero one, carrying the
// sum, when there is none or it was for other content).
func (e *Executor) readFiling(ctx context.Context, bookID int64, kind, sum string) (filing, error) {
	none := filing{Payload: sum}
	if e.db == nil {
		return none, nil
	}
	v, ok, err := e.db.GetSetting(ctx, filingKey(bookID, kind))
	if err != nil {
		return none, fmt.Errorf("contributing: read the %s filing marker: %w", kind, err)
	}
	if !ok || v == "" {
		return none, nil
	}
	var f filing
	if err := json.Unmarshal([]byte(v), &f); err != nil || f.Payload != sum {
		return none, nil
	}
	return f, nil
}

// writeFiling records f before a create call: it must be on disk first, or a
// lost answer would leave nothing to look from.
func (e *Executor) writeFiling(ctx context.Context, bookID int64, kind string, f filing) error {
	if e.db == nil {
		return nil
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := e.db.SetSetting(context.WithoutCancel(ctx), filingKey(bookID, kind), string(b)); err != nil {
		return fmt.Errorf("contributing: record the %s filing marker: %w", kind, err)
	}
	return nil
}

// clearFiling drops the marker once the dimension's issue is recorded.
func (e *Executor) clearFiling(ctx context.Context, bookID int64, kind string) {
	if e.db == nil {
		return
	}
	if err := e.db.DeleteSetting(context.WithoutCancel(ctx), filingKey(bookID, kind)); err != nil {
		e.log.Warn("contributing: clear the filing marker", "book", bookID, "kind", kind, "err", err)
	}
}

// guardedCreate runs one create call under the marker: *since (a field of f) is
// set to now and f recorded first; a refusal (contrib.NothingFiled) made
// nothing, so *since is cleared again.
func (e *Executor) guardedCreate(ctx context.Context, bookID int64, kind string, f *filing, since *time.Time, create func() error) error {
	*since = time.Now().UTC()
	if err := e.writeFiling(ctx, bookID, kind, *f); err != nil {
		return err
	}
	err := create()
	if err != nil && contrib.NothingFiled(err) {
		*since = time.Time{}
		_ = e.writeFiling(ctx, bookID, kind, *f)
	}
	return err
}

// filedGist looks for the gist an interrupted submit made: one of the user's
// gists made since the marker holding only the sidecar's file, at its size.
// found false means none was (the list is complete back to then).
func filedGist(ctx context.Context, cli *contrib.Client, since time.Time, file string, size int) (string, bool, error) {
	gists, complete, err := cli.GistsSince(ctx, since.Add(-contrib.FilingSkew))
	if err != nil {
		return "", false, err
	}
	for _, g := range gists {
		if f, ok := g.Files[file]; ok && len(g.Files) == 1 && f.Size == size && f.RawURL != "" {
			return f.RawURL, true, nil
		}
	}
	if !complete {
		return "", false, errListIncomplete
	}
	return "", false, nil
}

// filedIssue looks for the issue an interrupted submit filed: one the same user
// opened on the community repo since the marker, with this title and body.
// found false means none was (the list is complete back to then).
func (e *Executor) filedIssue(ctx context.Context, cli *contrib.Client, since time.Time, title, body string) (contrib.Issue, bool, error) {
	login, err := cli.Login(ctx)
	if err != nil {
		return contrib.Issue{}, false, err
	}
	issues, complete, err := cli.IssuesSinceBy(ctx, e.contribCommunityRepo, since.Add(-contrib.FilingSkew), login)
	if err != nil {
		return contrib.Issue{}, false, err
	}
	want := contrib.NormalizeBody(body)
	for _, is := range issues {
		if is.Title == title && contrib.NormalizeBody(is.Body) == want {
			return contrib.Issue{Number: is.Number, URL: is.URL}, true, nil
		}
	}
	if !complete {
		return contrib.Issue{}, false, errListIncomplete
	}
	return contrib.Issue{}, false, nil
}

// filingUnknown words a lookup that failed: what may have been filed can't be
// found or ruled out.
func filingUnknown(kind, what string, err error) error {
	return fmt.Errorf("%w (the %s %s): %w", ErrFilingUnknown, kind, what, err)
}

// fileIssue files one dimension's intake issue at most once across submits: an
// oversized body links a secret gist (made once too), and an interrupted
// earlier submit's gist or issue is adopted rather than made again.
func (e *Executor) fileIssue(ctx context.Context, cli *contrib.Client, bookID int64, kind, slug string, payload []byte) (contrib.Issue, error) {
	f, err := e.readFiling(ctx, bookID, kind, hexSHA256(payload))
	if err != nil {
		return contrib.Issue{}, err
	}
	title, body, labels := composeIssue(kind, slug, payload, "")
	if contrib.ExceedsBodyLimit(body) {
		file := fileNameFor(kind)
		if f.Gist == "" && !f.GistSince.IsZero() {
			raw, found, err := filedGist(ctx, cli, f.GistSince, file, len(payload))
			if err != nil {
				return contrib.Issue{}, filingUnknown(kind, "gist", err)
			}
			if found {
				e.log.Info("contributing: an interrupted submit had made the gist", "book", bookID, "kind", kind)
				f.Gist = raw
			}
			// The next marker write records it (the gist's create, or the issue's).
			f.GistSince = time.Time{}
		}
		if f.Gist == "" {
			var raws map[string]string
			if err := e.guardedCreate(ctx, bookID, kind, &f, &f.GistSince, func() (err error) {
				raws, err = cli.CreateGist(ctx, map[string]string{file: string(payload)}, true)
				return err
			}); err != nil {
				return contrib.Issue{}, err
			}
			// Recorded with the issue's marker, before the issue POST.
			f.Gist, f.GistSince = raws[file], time.Time{}
		}
		title, body, labels = composeIssue(kind, slug, payload, f.Gist)
	}
	if !f.IssueSince.IsZero() {
		issue, found, err := e.filedIssue(ctx, cli, f.IssueSince, title, body)
		if err != nil {
			return contrib.Issue{}, filingUnknown(kind, "issue", err)
		}
		if found {
			e.log.Info("contributing: an interrupted submit had filed the issue", "book", bookID, "kind", kind,
				"issue", issue.Number)
			return issue, nil
		}
	}
	// A refusal leaves a made gist in the marker for the next try.
	var issue contrib.Issue
	err = e.guardedCreate(ctx, bookID, kind, &f, &f.IssueSince, func() (err error) {
		issue, err = cli.CreateIssue(ctx, e.contribCommunityRepo, title, body, labels)
		return err
	})
	return issue, err
}
