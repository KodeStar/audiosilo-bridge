// Package audiblejob runs the server's audible.poll job (BRIDGE-PLAN.md Phase
// 4a): list an Audible account's library, read the account's books library on
// the server page by page, match each title (importjob.BookMatcher: ASIN exact
// first, then author, series and title), report the titles to the server in
// calls of at most link.MaxAudibleTitles (the last one final, so the titles
// the poll didn't see become gone there), then backfill the ASIN of each book
// a title matched without doubt (a book two titles claimed is never enriched,
// whichever kept it: importjob.KeepBest), paced (enrich.go).
//
// It writes nothing into a library and reads no credential itself: the account
// manager (audibleacct) holds the sign-in and lists the library. Params,
// progress and the result carry the account's id and counts only.
package audiblejob

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audibleacct"
	"github.com/kodestar/audiosilo-bridge/internal/importjob"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Kind is the job kind.
const Kind = "audible.poll"

// maxBookPages bounds the library listing (500 books a page: a million books).
const maxBookPages = 2000

// maxRuntime is the longest runtime the server takes (seconds).
const maxRuntime = 1_000_000

// maxAuthors is the most authors a title carries: the server refuses a whole
// titles call over one title with more, so an anthology's long list of credits
// is cut to its first ones.
const maxAuthors = 20

// Accounts is the part of the account manager a poll uses (*audibleacct.Manager).
type Accounts interface {
	Account(ctx context.Context, id string) (store.AudibleAccount, error)
	// Library lists the account's Audible library (audibleacct.ErrRefused when
	// Audible refuses the sign-in, which it records).
	Library(ctx context.Context, id string) ([]source.Record, error)
	// Preorders are the account's pending preorders the library doesn't list
	// (audibleacct.Manager.Preorders: from the orders, remembered, described
	// by the catalog). It never fails: a failure is its own warning.
	Preorders(ctx context.Context, id string, library []source.Record) []source.Record
	Polled(ctx context.Context, id string, titles, matched int) error
	Failed(ctx context.Context, id, why string) error
}

// Server is the part of the server link a poll calls (*link.Client).
type Server interface {
	LibraryBooks(ctx context.Context, libraryID int64, after string, limit int) (*link.LibraryBooks, error)
	PutAudibleTitles(ctx context.Context, account string, t link.AudibleTitles) (int, error)
	Enrich(ctx context.Context, libraryID int64, path, asin string) error
}

// Options wire the handler.
type Options struct {
	Accounts Accounts
	// Server is the link (nil while unpaired); ServerID the pairing's server.
	Server   func() Server
	ServerID func() string
	// Libraries and Mount say whether the account's library is mapped at the
	// Bridge and where, for the unmatched titles' planned destinations (nil:
	// the books listing only).
	Libraries Libraries
	Mount     mapping.MountFunc
	Log       *slog.Logger
}

// Handler runs audible.poll.
type Handler struct {
	o     Options
	now   func() time.Time
	sleep func(context.Context, time.Duration) error // the enrichment's pace (tests stub both)
}

// New returns the handler.
func New(o Options) *Handler {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.ServerID == nil {
		o.ServerID = func() string { return "" }
	}
	return &Handler{o: o, now: time.Now, sleep: sleepCtx}
}

// Kind is audible.poll.
func (h *Handler) Kind() string { return Kind }

// Lane is the light lane: a poll is a few API calls, then the paced ASIN
// backfill (up to enrichBudget, the rest deferred to the next poll).
func (h *Handler) Lane() string { return jobs.LaneLight }

// Params are audible.poll's params: the account, and its path template ("" or
// absent for Auto), which the unmatched titles' planned destinations use.
type Params struct {
	Account  string `json:"account"`
	Template string `json:"template,omitempty"`
}

// ParseParams reads the params strictly (an unknown field is refused).
func ParseParams(raw json.RawMessage) (Params, error) {
	var p Params
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Params{}, fmt.Errorf("bad params: %w", err)
	}
	if !audibleacct.IDPattern.MatchString(p.Account) {
		return Params{}, errors.New("bad params: account must be an Audible account id")
	}
	if len(p.Template) > maxTemplate {
		return Params{}, errors.New("bad params: the template is too long")
	}
	return p, nil
}

// Result is what a poll reports: counts, never a title or a credential.
type Result struct {
	Account     string `json:"account"`
	PollID      string `json:"poll_id"`
	Titles      int    `json:"titles"`
	Available   int    `json:"available"`
	Preorder    int    `json:"preorder"`
	Unavailable int    `json:"unavailable"`
	// Skipped are titles Audible sent without a usable ASIN (never reported).
	Skipped int `json:"skipped,omitempty"`
	// Matched titles, by how; Contested the matched books two titles claimed.
	Matched      int `json:"matched"`
	MatchedASIN  int `json:"matched_asin"`
	MatchedFuzzy int `json:"matched_fuzzy"`
	Contested    int `json:"contested"`
	// Overlong titles matched a book whose path is longer than the server
	// takes (MaxBookPath): reported unmatched as contested, never as a title
	// to back up (bridge#56).
	Overlong int `json:"overlong,omitempty"`
	// Enriched books got the ASIN of their title; EnrichFailed ones the server
	// didn't answer for (tried again at the next poll).
	Enriched     int `json:"enriched"`
	EnrichFailed int `json:"enrich_failed,omitempty"`
	// EnrichDeferred books wait for the next poll: the server asked the
	// Bridge to slow down past what one poll waits, or the poll's time for
	// the backfill ran out (enrichBudget).
	EnrichDeferred int `json:"enrich_deferred,omitempty"`
	Books          int `json:"books"`
}

// Run polls the account. Its error passes through audible.RedactedError, so no
// Amazon account identifier reaches the job's error, whatever wrapped it.
//
// A refused account fails coded, as an import and a sync do (Failure, both its
// result and its error): CodeRefused when Audible refuses the device,
// CodeSignIn when it refuses the sign-in for now.
func (h *Handler) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	res, err := h.run(ctx, job, rep)
	var f *Failure
	if errors.As(err, &f) {
		return f, f
	}
	if err != nil {
		return nil, audible.RedactedError(err)
	}
	return res, nil
}

func (h *Handler) run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	p, err := ParseParams(job.Params)
	if err != nil {
		return nil, err
	}
	acct, err := h.o.Accounts.Account(ctx, p.Account)
	if errors.Is(err, audibleacct.ErrNotFound) {
		return nil, fmt.Errorf("this Bridge has no Audible account %s: it was removed here, or signed in on another Bridge", p.Account)
	}
	if err != nil {
		return nil, err
	}
	if acct.OnOtherServer(h.o.ServerID()) {
		return nil, h.fail(ctx, acct.ID, "the account is mapped on another server: map it again on the Bridge's Accounts tab")
	}
	var srv Server
	if h.o.Server != nil {
		srv = h.o.Server()
	}
	if srv == nil {
		return nil, errors.New("the Bridge isn't paired with a server")
	}

	rep.Progress(jobs.Progress{Step: "audible", Message: "Listing the Audible library"})
	recs, err := h.o.Accounts.Library(ctx, acct.ID)
	if errors.Is(err, audibleacct.ErrRefused) {
		return nil, h.refused(ctx, acct.ID) // recorded by the accounts
	}
	if err != nil {
		return nil, h.fail(ctx, acct.ID, "could not list the Audible library: "+err.Error())
	}
	// A pending preorder isn't in the library until it is released: the
	// orders show it, and the accounts remember it.
	rep.Progress(jobs.Progress{Step: "audible", Message: "Listing the Audible preorders"})
	recs = append(recs, h.o.Accounts.Preorders(ctx, acct.ID, recs)...)

	rep.Progress(jobs.Progress{Step: "books", Message: "Reading the library's books on the server"})
	books, err := allBooks(ctx, srv, acct.LibraryID)
	if err != nil {
		return nil, h.fail(ctx, acct.ID, "could not read the library's books on the server: "+err.Error())
	}

	rep.Progress(jobs.Progress{Step: "matching", Message: "Matching the titles to the library", Total: len(recs)})
	res := Result{Account: acct.ID, PollID: pollID(job), Books: len(books)}
	matcher := importjob.NewBookMatcher(books)
	titles := make([]link.AudibleTitle, 0, len(recs))
	used := make([]source.Record, 0, len(recs)) // used[i] is titles[i]'s record
	claims := make([]importjob.Claim, 0, len(recs))
	matchedBook := map[string]link.LibraryBook{}
	for _, rec := range recs {
		rec.ASIN = strings.ToUpper(strings.TrimSpace(rec.ASIN))
		if !audibleacct.ASINPattern.MatchString(rec.ASIN) {
			res.Skipped++
			continue
		}
		t := title(rec)
		switch t.State {
		case link.AudibleAvailable:
			res.Available++
		case link.AudiblePreorder:
			res.Preorder++
		default:
			res.Unavailable++
		}
		var c importjob.Claim
		// A preorder the catalog hasn't described yet matches by its ASIN only.
		if b, score, ok := matchTitled(matcher, rec); ok && matchable(b.Path) {
			agrees, disagrees := importjob.PositionAgrees(rec, b)
			c = importjob.Claim{Path: b.Path, Score: score, Work: importjob.WorkKey(rec), Agrees: agrees, Disagrees: disagrees}
			matchedBook[b.Path] = b
		} else if ok && fs.ValidPath(b.Path) {
			// A title matched to a book whose path the server won't take
			// can't be sent as a match; unmatched it would read as a title to
			// back up, and Back up everything would download a second copy.
			// It goes as contested (which that skips), with no book named:
			// the path can't be sent either.
			if t.Reason == "" {
				t.Reason = link.ReasonContested
				res.Overlong++
			}
			h.o.Log.Warn("audible: a title matched a book whose path is longer than the server takes; reported as contested, not as a title to back up",
				"account", acct.ID, "asin", rec.ASIN, "path_bytes", len(b.Path))
		}
		titles, used, claims = append(titles, t), append(used, rec), append(claims, c)
	}
	// A preorder isn't out yet, so a book a title of the library claims too is
	// that title's edition: the preorder gives way, rather than contest the
	// book or keep the title's ASIN off it.
	owned := map[string]bool{}
	for i, c := range claims {
		if c.Path != "" && titles[i].State != link.AudiblePreorder {
			owned[c.Path] = true
		}
	}
	for i, c := range claims {
		if titles[i].State == link.AudiblePreorder && owned[c.Path] {
			claims[i] = importjob.Claim{}
		}
	}
	// A book several titles claimed keeps the best one only (the one whose
	// series number is the book's, else the best score; a tie between two
	// works, none): the others are reported unmatched, with the reason
	// contested and the book's path unless their number says they are another
	// volume, so the console shows them as not here and a backup of everything
	// can skip them.
	keep, contested := importjob.KeepBest(claims)
	claimed := map[string]int{} // path -> how many titles claimed it
	for _, c := range claims {
		if c.Path == "" {
			continue
		}
		if claimed[c.Path]++; claimed[c.Path] == 2 {
			res.Contested++
		}
	}
	twins := importjob.Twins(books)
	var hits []hit
	for i, c := range claims {
		if c.Path == "" {
			continue
		}
		if !keep[i] {
			if contested[i] && titles[i].Reason == "" {
				titles[i].Reason, titles[i].ContestedPath = link.ReasonContested, c.Path
			}
			continue
		}
		b, rec := matchedBook[c.Path], used[i]
		how := link.MatchFuzzy
		if strings.EqualFold(b.ASIN, rec.ASIN) {
			how = link.MatchASIN
			res.MatchedASIN++
		} else {
			res.MatchedFuzzy++
		}
		titles[i].Match = &link.AudibleMatch{LibraryID: acct.LibraryID, Path: b.Path, How: how}
		// A book two titles claimed gets no ASIN: at least one of the claims
		// was wrong. A preorder isn't out yet, so the book on disk is another
		// edition: its ASIN would stay on that book for good. A pack matched
		// to a folder that holds no range may be one volume: not the pack's.
		// Nor does a book the library holds twice (a twin): the title's words
		// can't tell the copies apart.
		if claimed[c.Path] == 1 && b.ASIN == "" && titles[i].State != link.AudiblePreorder && !importjob.SpanMismatch(rec, b) &&
			!twins[b.Path] {
			hits = append(hits, hit{path: b.Path, asin: rec.ASIN})
		}
	}
	res.Titles, res.Matched = len(titles), res.MatchedASIN+res.MatchedFuzzy

	if err := h.planUnmatched(ctx, acct.LibraryID, p.Template, books, titles, used); err != nil {
		return nil, err
	}

	rep.Progress(jobs.Progress{Step: "reporting", Message: "Reporting the titles to the server", Total: len(titles)})
	for start := 0; ; start += link.MaxAudibleTitles {
		end := min(start+link.MaxAudibleTitles, len(titles))
		final := end == len(titles)
		if _, err := srv.PutAudibleTitles(ctx, acct.ID, link.AudibleTitles{PollID: res.PollID, Final: final, Titles: titles[start:end]}); err != nil {
			if errors.Is(err, link.ErrUnknownAccount) {
				return nil, h.fail(ctx, acct.ID, "the server has no such Audible account: sign in or map the account again on the Bridge's Accounts tab")
			}
			return nil, h.fail(ctx, acct.ID, "could not report the titles to the server: "+err.Error())
		}
		rep.Progress(jobs.Progress{Step: "reporting", Message: "Reporting the titles to the server", Done: end, Total: len(titles)})
		if final {
			break
		}
	}

	if err := h.enrich(ctx, srv, acct, hits, rep, &res); err != nil {
		return nil, err
	}

	if err := h.o.Accounts.Polled(ctx, acct.ID, res.Titles, res.Matched); err != nil {
		h.o.Log.Warn("audible: recording the poll failed", "account", acct.ID, "err", err)
	}
	h.o.Log.Info("audible: polled", "account", acct.ID, "titles", res.Titles, "matched", res.Matched, "contested", res.Contested,
		"enriched", res.Enriched, "enrich_failed", res.EnrichFailed, "enrich_deferred", res.EnrichDeferred)
	return res, nil
}

// matchTitled is the matcher's book for rec. A record with no title (a
// preorder the catalog hasn't described yet) matches by its ASIN only: the
// fuzzy pass would go on its author or series alone.
func matchTitled(m *importjob.BookMatcher, rec source.Record) (link.LibraryBook, float64, bool) {
	b, score, ok := m.MatchScored(rec)
	if ok && strings.TrimSpace(rec.Title) == "" && !strings.EqualFold(b.ASIN, rec.ASIN) {
		return link.LibraryBook{}, 0, false
	}
	return b, score, ok
}

// planUnmatched sets each unmatched title's Planned: where a backup of it
// would go, as the import would place it now (the console shows it on the
// title's row; used[i] is titles[i]'s record). The planner (and the library's
// mount) is opened only when a title needs a plan, and the disk reads stop
// with the poll.
func (h *Handler) planUnmatched(ctx context.Context, libraryID int64, template string, books []link.LibraryBook,
	titles []link.AudibleTitle, used []source.Record) error {
	var planner *titlePlanner
	defer func() {
		if planner != nil {
			planner.close()
		}
	}()
	for i := range titles {
		// A preorder the catalog hasn't described yet has no name to place.
		if titles[i].Match != nil || strings.TrimSpace(used[i].Title) == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if planner == nil {
			planner = newTitlePlanner(libraryID, template, books, h.mappedMount(libraryID))
		}
		titles[i].Planned = planner.plan(used[i])
	}
	return nil
}

// mappedMount is where the Bridge sees the books library id when it is mapped
// there (checked at the mount in use; writable or not, since a plan only
// reads), else "".
func (h *Handler) mappedMount(id int64) string {
	if h.o.Libraries == nil || h.o.Mount == nil {
		return ""
	}
	lib, ok := h.o.Libraries.Library(id)
	if !ok || lib.LibraryKind() != "books" {
		return ""
	}
	mount, _ := h.o.Mount(lib.ID, lib.Root)
	if st, checked := h.o.Libraries.Status(lib.ID); !checked || st.MountPath != mount || !st.Mapped || mount == "" {
		return ""
	}
	return mount
}

// fail records why the poll failed on the account and returns it as the job's
// error. A poll stopped, cancelled or lost meanwhile records nothing: that is
// the job's end, not the account's failure (the runner ends the job by its
// context's cause).
func (h *Handler) fail(ctx context.Context, account, why string) error {
	why = audible.RedactAccountIDs(why)
	if ctx.Err() != nil {
		return errors.New(why)
	}
	if err := h.o.Accounts.Failed(context.WithoutCancel(ctx), account, why); err != nil {
		h.o.Log.Warn("audible: recording a failed poll failed", "account", account, "err", err)
	}
	return errors.New(why)
}

// MaxBookPath is the longest book path the server takes, in bytes (its
// catalog.MaxPathBytes).
const MaxBookPath = 4096

// matchable reports whether p is a book path the server takes in a match and
// an enrichment: "." (a library that is one book, at its root) or a clean path
// below the root, within MaxBookPath.
func matchable(p string) bool {
	return len(p) <= MaxBookPath && fs.ValidPath(p)
}

// refused is a poll's failure when Audible refused account id: the device
// (Throttled, read again since the listing may have just recorded it) or the
// sign-in for now.
func (h *Handler) refused(ctx context.Context, id string) *Failure {
	if acct, err := h.o.Accounts.Account(context.WithoutCancel(ctx), id); err == nil && audibleacct.Throttled(acct) {
		return throttledDevice()
	}
	return fail(CodeSignIn, "Audible refused the account's sign-in: the next check tries again, or sign in again on the Bridge's Accounts tab.")
}

// bookLister reads a library's books page by page (Server and ImportServer).
type bookLister interface {
	LibraryBooks(ctx context.Context, libraryID int64, after string, limit int) (*link.LibraryBooks, error)
}

// allBooks reads every page of the library's books.
func allBooks(ctx context.Context, srv bookLister, libraryID int64) ([]link.LibraryBook, error) {
	if libraryID <= 0 {
		return nil, errors.New("the account has no library: map it again on the Bridge's Accounts tab")
	}
	var books []link.LibraryBook
	after := ""
	for range maxBookPages {
		page, err := srv.LibraryBooks(ctx, libraryID, after, link.MaxLibraryBooks)
		if err != nil {
			return nil, err
		}
		books = append(books, page.Books...)
		if page.Next == "" || page.Next == after {
			return books, nil
		}
		after = page.Next
	}
	return nil, fmt.Errorf("the library has more than %d pages of books", maxBookPages)
}

// title is a record as the poll reports it.
func title(rec source.Record) link.AudibleTitle {
	t := link.AudibleTitle{ASIN: rec.ASIN, Title: rec.Title, Authors: rec.Authors[:min(len(rec.Authors), maxAuthors)], Series: []link.SeriesRef{},
		ReleaseDate: rec.ReleaseDate, RuntimeS: min(max(int64(rec.RuntimeSec), 0), maxRuntime), State: link.AudibleAvailable}
	if len(t.Authors) == 0 {
		t.Authors = []string{}
		if rec.Author != "" {
			t.Authors = []string{rec.Author}
		}
	}
	if rec.Series != "" {
		pos := rec.SeriesPosition
		if pos == "" && rec.HasSequence {
			pos = strconv.FormatFloat(rec.Sequence, 'f', -1, 64)
		}
		t.Series = append(t.Series, link.SeriesRef{Name: rec.Series, Position: pos})
	}
	if reason := strings.TrimSpace(rec.UnavailableReason); reason != "" {
		t.Reason = reason
		t.State = link.AudibleUnavailable
		if rec.Preorder {
			t.State = link.AudiblePreorder
		}
	}
	return t
}

// pollID is the poll's id: the job and attempt, and a random suffix so a poll
// run again never shares one with an earlier run.
func pollID(job jobs.Job) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "j" + strconv.FormatInt(job.ID, 10) + "." + strconv.Itoa(job.Attempt) + "." + hex.EncodeToString(b)
}
