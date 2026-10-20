package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// filingSkew is how far before a marker's time the lookup reaches (clocks).
const filingSkew = 2 * time.Minute

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

func payloadSum(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
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

// filedGist looks for the gist an interrupted submit made: one of the user's
// gists made since the marker holding only the sidecar's file, at its size.
// found false means none was (the list is complete back to then).
func (e *Executor) filedGist(ctx context.Context, cli *contrib.Client, since time.Time, kind, file string, size int) (string, bool, error) {
	gists, complete, err := cli.GistsSince(ctx, since.Add(-filingSkew))
	if err != nil {
		return "", false, fmt.Errorf("%w (the %s gist): %w", ErrFilingUnknown, kind, err)
	}
	for _, g := range gists {
		if f, ok := g.Files[file]; ok && len(g.Files) == 1 && f.Size == size && f.RawURL != "" {
			return f.RawURL, true, nil
		}
	}
	if !complete {
		return "", false, fmt.Errorf("%w (the %s gist): more gists were made since than one page lists", ErrFilingUnknown, kind)
	}
	return "", false, nil
}

// filedIssue looks for the issue an interrupted submit filed: one the same user
// opened on the community repo since the marker, with this title and body.
// found false means none was (the list is complete back to then).
func (e *Executor) filedIssue(ctx context.Context, cli *contrib.Client, since time.Time, kind, title, body string) (contrib.Issue, bool, error) {
	login, err := cli.Login(ctx)
	if err != nil {
		return contrib.Issue{}, false, fmt.Errorf("%w (the %s issue): %w", ErrFilingUnknown, kind, err)
	}
	issues, complete, err := cli.IssuesSinceBy(ctx, e.contribCommunityRepo, since.Add(-filingSkew), login)
	if err != nil {
		return contrib.Issue{}, false, fmt.Errorf("%w (the %s issue): %w", ErrFilingUnknown, kind, err)
	}
	want := strings.TrimSpace(body)
	for _, is := range issues {
		if is.Title == title && strings.TrimSpace(strings.ReplaceAll(is.Body, "\r\n", "\n")) == want {
			return contrib.Issue{Number: is.Number, URL: is.URL}, true, nil
		}
	}
	if !complete {
		return contrib.Issue{}, false, fmt.Errorf("%w (the %s issue): more issues were opened since than one page lists", ErrFilingUnknown, kind)
	}
	return contrib.Issue{}, false, nil
}

// fileIssue files one dimension's intake issue at most once across submits: an
// oversized body links a secret gist (made once too), and an interrupted
// earlier submit's gist or issue is adopted rather than made again.
func (e *Executor) fileIssue(ctx context.Context, cli *contrib.Client, bookID int64, kind, slug string, payload []byte) (contrib.Issue, error) {
	f, err := e.readFiling(ctx, bookID, kind, payloadSum(payload))
	if err != nil {
		return contrib.Issue{}, err
	}
	title, body, labels := composeIssue(kind, slug, payload, "")
	if contrib.ExceedsBodyLimit(body) {
		file := fileNameFor(kind)
		if f.Gist == "" && !f.GistSince.IsZero() {
			raw, found, err := e.filedGist(ctx, cli, f.GistSince, kind, file, len(payload))
			if err != nil {
				return contrib.Issue{}, err
			}
			if found {
				e.log.Info("contributing: an interrupted submit had made the gist", "book", bookID, "kind", kind)
				f.Gist = raw
			}
			f.GistSince = time.Time{}
			if err := e.writeFiling(ctx, bookID, kind, f); err != nil {
				return contrib.Issue{}, err
			}
		}
		if f.Gist == "" {
			f.GistSince = e.now().UTC()
			if err := e.writeFiling(ctx, bookID, kind, f); err != nil {
				return contrib.Issue{}, err
			}
			raws, gerr := cli.CreateGist(ctx, map[string]string{file: string(payload)}, true)
			if gerr != nil {
				if contrib.NothingFiled(gerr) {
					f.GistSince = time.Time{}
					_ = e.writeFiling(ctx, bookID, kind, f)
				}
				return contrib.Issue{}, gerr
			}
			f.Gist, f.GistSince = raws[file], time.Time{}
			if err := e.writeFiling(ctx, bookID, kind, f); err != nil {
				return contrib.Issue{}, err
			}
		}
		title, body, labels = composeIssue(kind, slug, payload, f.Gist)
	}
	if !f.IssueSince.IsZero() {
		issue, found, err := e.filedIssue(ctx, cli, f.IssueSince, kind, title, body)
		if err != nil {
			return contrib.Issue{}, err
		}
		if found {
			e.log.Info("contributing: an interrupted submit had filed the issue", "book", bookID, "kind", kind,
				"issue", issue.Number)
			return issue, nil
		}
	}
	f.IssueSince = e.now().UTC()
	if err := e.writeFiling(ctx, bookID, kind, f); err != nil {
		return contrib.Issue{}, err
	}
	issue, err := cli.CreateIssue(ctx, e.contribCommunityRepo, title, body, labels)
	if err != nil {
		if contrib.NothingFiled(err) {
			// GitHub filed nothing; a gist made for it is kept for the next try.
			f.IssueSince = time.Time{}
			_ = e.writeFiling(ctx, bookID, kind, f)
		}
		return contrib.Issue{}, err
	}
	return issue, nil
}
