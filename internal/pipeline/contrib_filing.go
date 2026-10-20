package pipeline

import (
	"cmp"
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
// one looks among what the same user made since then and adopts a match, and makes
// another only when that list is complete and holds none.

// ErrFilingUnknown is a dimension an earlier submit may have filed that the
// Bridge can't find or rule out (GitHub's list didn't reach back far enough, or
// couldn't be read, or the earlier try was moments ago): filing again could make
// a duplicate.
var ErrFilingUnknown = errors.New("an earlier try may have filed it, and the Bridge can't tell")

// errFilingSettling is a lookup too soon after the create it looks for: the
// client may have given up while GitHub was still making it.
var errFilingSettling = errors.New("the earlier try was moments ago, and GitHub may still be making it")

// filingSettle is how old an issue's marker must be before a lookup that finds
// nothing rules the issue out.
const filingSettle = time.Minute

// filing is a dimension's marker (the settings KV, filingKey): what its last
// submit was making when it stopped. Times are GitHub's (contrib.Client.Now).
type filing struct {
	// Payload is the sha256 of the sidecar the calls carried: a gist made for
	// other content isn't reused.
	Payload string `json:"payload"`
	// Login is the GitHub user the calls were made as (the issue is looked for
	// among that user's).
	Login string `json:"login,omitempty"`
	// GistSince is set while a gist POST may have reached GitHub (since then).
	GistSince time.Time `json:"gist_since,omitzero"`
	// Gist is the raw URL of the gist made for Payload (an oversized body links it).
	Gist string `json:"gist,omitempty"`
	// IssueSince is set while an issue POST may have reached GitHub; Issue is
	// issueSum of the title and body it sent (looked for whatever the sidecar
	// is now).
	IssueSince time.Time `json:"issue_since,omitzero"`
	Issue      string    `json:"issue,omitempty"`
}

func filingKey(bookID int64, kind string) string {
	return "contrib.filing/" + strconv.FormatInt(bookID, 10) + "/" + kind
}

// issueSum identifies an issue by its title and body (as sent or as GitHub
// lists it).
func issueSum(title, body string) string {
	return hexSHA256([]byte(title), []byte{0}, []byte(contrib.NormalizeBody(body)))
}

// gistDescription names a sidecar's gist by its content, so a lookup matches the
// gist made for exactly this payload.
func gistDescription(kind, slug, sum string) string {
	return "AudioSilo " + kind + " for " + slug + " (sha256 " + sum + ")"
}

// readFiling is the dimension's marker (a zero one when there is none).
func (e *Executor) readFiling(ctx context.Context, bookID int64, kind string) (filing, error) {
	if e.db == nil {
		return filing{}, nil
	}
	v, ok, err := e.db.GetSetting(ctx, filingKey(bookID, kind))
	if err != nil {
		return filing{}, fmt.Errorf("contributing: read the %s filing marker: %w", kind, err)
	}
	var f filing
	if !ok || v == "" || json.Unmarshal([]byte(v), &f) != nil {
		return filing{}, nil
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
// set to GitHub's time and f recorded first; a refusal (contrib.NothingFiled)
// made nothing, so *since is cleared again.
func (e *Executor) guardedCreate(ctx context.Context, cli *contrib.Client, bookID int64, kind string, f *filing, since *time.Time,
	create func() error) error {
	*since = cli.Now(ctx)
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
// gists made since the marker holding only the sidecar's file, with the
// description naming its content. found false means none was (the list is
// complete back to then; a gist made as another user isn't listed, and is left).
func filedGist(ctx context.Context, cli *contrib.Client, since time.Time, file, desc string) (string, bool, error) {
	gists, complete, err := cli.GistsSince(ctx, since.Add(-contrib.FilingSkew))
	if err != nil {
		return "", false, err
	}
	for _, g := range gists {
		if raw := g.Files[file]; raw != "" && len(g.Files) == 1 && g.Description == desc {
			return raw, true, nil
		}
	}
	if !complete {
		return "", false, contrib.ErrListIncomplete
	}
	return "", false, nil
}

// filedIssue looks for the issue an interrupted submit filed: one its user (the
// marker's, else the one signed in now) opened on the community repo since the
// marker with the title and body it sent, other than recorded (the closed issue
// a share files again). found false means none was: the list is complete back
// to then, and the marker old enough for GitHub to list it. An issue edited
// since doesn't match and is filed again: a duplicate a maintainer can close,
// rather than a dimension never filed.
func (e *Executor) filedIssue(ctx context.Context, cli *contrib.Client, f filing, login string, recorded int) (contrib.Issue, bool, error) {
	creator := cmp.Or(f.Login, login)
	if creator == "" {
		return contrib.Issue{}, false, errors.New("the GitHub login is unknown")
	}
	issues, complete, err := cli.IssuesSinceBy(ctx, e.contribCommunityRepo, f.IssueSince.Add(-contrib.FilingSkew), creator)
	if err != nil {
		return contrib.Issue{}, false, err
	}
	for _, is := range issues {
		if is.Number != recorded && issueSum(is.Title, is.Body) == f.Issue {
			return contrib.Issue{Number: is.Number, URL: is.URL}, true, nil
		}
	}
	switch {
	case !complete:
		return contrib.Issue{}, false, contrib.ErrListIncomplete
	case cli.Now(ctx).Sub(f.IssueSince) < filingSettle:
		return contrib.Issue{}, false, errFilingSettling
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
// earlier submit's gist or issue is adopted rather than made again. recorded is
// the issue the dimension's row names (0 for none), never the one adopted.
func (e *Executor) fileIssue(ctx context.Context, cli *contrib.Client, bookID int64, kind, slug string, payload []byte,
	recorded int) (contrib.Issue, error) {
	f, err := e.readFiling(ctx, bookID, kind)
	if err != nil {
		return contrib.Issue{}, err
	}
	login, err := cli.Login(ctx)
	if err != nil {
		// The calls below fail too when GitHub can't be reached; an issue filed
		// without a login is looked for as whoever is signed in then.
		e.log.Warn("contributing: read the GitHub login", "book", bookID, "kind", kind, "err", err)
	}
	// An issue POST that may have reached GitHub is looked for first, by what it
	// sent: whatever the sidecar is now, an issue it made is the dimension's.
	if !f.IssueSince.IsZero() {
		issue, found, err := e.filedIssue(ctx, cli, f, login, recorded)
		if err != nil {
			return contrib.Issue{}, filingUnknown(kind, "issue", err)
		}
		if found {
			e.log.Info("contributing: an interrupted submit had filed the issue", "book", bookID, "kind", kind,
				"issue", issue.Number)
			return issue, nil
		}
		f.IssueSince = time.Time{}
	}
	if sum := hexSHA256(payload); f.Payload != sum {
		f = filing{Payload: sum} // a gist made for other content isn't this one's
	}
	f.Login = login
	title, body, labels := composeIssue(kind, slug, payload, "")
	if contrib.ExceedsBodyLimit(body) {
		file, desc := fileNameFor(kind), gistDescription(kind, slug, f.Payload)
		if f.Gist == "" && !f.GistSince.IsZero() {
			raw, found, err := filedGist(ctx, cli, f.GistSince, file, desc)
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
			if err := e.guardedCreate(ctx, cli, bookID, kind, &f, &f.GistSince, func() (err error) {
				raws, err = cli.CreateGist(ctx, desc, map[string]string{file: string(payload)}, true)
				return err
			}); err != nil {
				return contrib.Issue{}, err
			}
			// Recorded with the issue's marker, before the issue POST.
			f.Gist, f.GistSince = raws[file], time.Time{}
		}
		title, body, labels = composeIssue(kind, slug, payload, f.Gist)
	}
	// A refusal leaves a made gist in the marker for the next try.
	f.Issue = issueSum(title, body)
	var issue contrib.Issue
	err = e.guardedCreate(ctx, cli, bookID, kind, &f, &f.IssueSince, func() (err error) {
		issue, err = cli.CreateIssue(ctx, e.contribCommunityRepo, title, body, labels)
		return err
	})
	return issue, err
}
