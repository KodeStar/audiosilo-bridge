package audible

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/source"
)

type libraryResp struct {
	Items []libItem `json:"items"`
}

type libItem struct {
	ASIN                string      `json:"asin"`
	Title               string      `json:"title"`
	Subtitle            string      `json:"subtitle"`
	Authors             []nameRef   `json:"authors"`
	Narrators           []nameRef   `json:"narrators"`
	Series              []seriesRef `json:"series"`
	ReleaseDate         string      `json:"release_date"`
	IssueDate           string      `json:"issue_date"`
	ContentType         string      `json:"content_type"`
	ContentDeliveryType string      `json:"content_delivery_type"`
	// RuntimeLengthMin comes from the product_attrs response group (minute-rounded,
	// 0/absent for shows and some titles).
	RuntimeLengthMin int `json:"runtime_length_min"`
	// CustomerRights needs the customer_rights response group. nil when Audible
	// omits it - treat that as downloadable (defensive default).
	CustomerRights *customerRights `json:"customer_rights"`
}

type customerRights struct {
	IsConsumableOffline bool `json:"is_consumable_offline"`
}

type nameRef struct {
	Name string `json:"name"`
}

type seriesRef struct {
	Title    string `json:"title"`
	Sequence string `json:"sequence"`
}

const (
	// libraryPageSize is the per-request item count; a short page means the last one.
	libraryPageSize = 1000
	// maxLibraryPages bounds the pager. At libraryPageSize that allows a 250,000-title
	// account - far beyond any real library - while keeping a misbehaving endpoint
	// finite: the only exit used to be a short page, so an endpoint that clamped `page`
	// or replayed the last one for an out-of-range value appended 1000 records per
	// request until the caller's deadline, with nothing bounding the memory.
	maxLibraryPages = 250
)

// dedupeKey identifies a library item for the pager's dedupe. The ASIN is the real
// identity, but it MUST fall back to something non-empty: an item keyed on "" is
// never recorded as seen, so a replayed page re-appends it every time and the
// "contributed nothing new" stop never fires - one blank-ASIN item was enough to
// spin the pager to its page cap. The API always sends an ASIN; this is the
// defensive path, and collapsing two same-titled ASIN-less items loses nothing
// (the record's identity is the ASIN, so downstream can't tell them apart anyway).
func (it libItem) dedupeKey() string {
	if it.ASIN != "" {
		return it.ASIN
	}
	return "title:" + it.Title
}

// Library lists the account's owned audiobooks as import records (metadata
// only: audible.import fetches the file). Items are deduped
// by dedupeKey, so a repeated page can neither duplicate a book nor spin the pager.
func (c *Client) Library(ctx context.Context) ([]source.Record, error) {
	var recs []source.Record
	seen := make(map[string]bool, libraryPageSize) // one page is the common case
	for page := 1; page <= maxLibraryPages; page++ {
		q := url.Values{
			"response_groups": {libraryGroups},
			"num_results":     {strconv.Itoa(libraryPageSize)},
			"page":            {strconv.Itoa(page)},
		}
		var resp libraryResp
		if err := c.doSigned(ctx, http.MethodGet, "/1.0/library?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		now := time.Now()
		before := len(recs)
		for _, it := range resp.Items {
			k := it.dedupeKey()
			if seen[k] {
				continue
			}
			seen[k] = true
			recs = append(recs, it.toRecord(now))
			it.logOddDate(c.set.log, now)
		}
		// A short page is the last one. A full page that contributed nothing new means
		// the endpoint is replaying a page instead of advancing - stop either way.
		if len(resp.Items) < libraryPageSize || len(recs) == before {
			break
		}
	}
	return recs, nil
}

// libraryGroups are the response groups a library listing and a title read ask for.
const libraryGroups = "contributors,series,product_desc,product_attrs,customer_rights"

// ErrNotInLibrary is a title the account's library doesn't hold.
var ErrNotInLibrary = errors.New("the title isn't in the account's Audible library")

// Title reads one title of the account's library (GET /1.0/library/{asin}) as
// an import record, as Library would list it. A title the library doesn't hold
// is ErrNotInLibrary.
func (c *Client) Title(ctx context.Context, asin string) (source.Record, error) {
	var resp struct {
		Item *libItem `json:"item"`
	}
	q := url.Values{"response_groups": {libraryGroups}}
	err := c.doSigned(ctx, http.MethodGet, "/1.0/library/"+url.PathEscape(asin)+"?"+q.Encode(), nil, &resp)
	if HTTPStatus(err) == http.StatusNotFound || (err == nil && (resp.Item == nil || !strings.EqualFold(resp.Item.ASIN, asin))) {
		return source.Record{}, ErrNotInLibrary
	}
	if err != nil {
		return source.Record{}, err
	}
	return resp.Item.toRecord(time.Now()), nil
}

func (it libItem) toRecord(now time.Time) source.Record {
	reason, preorder := it.unavailableReason(now)
	rec := source.Record{
		Title: it.Title, TitleShort: it.Title, ASIN: it.ASIN,
		FileName:          safeFileName(it.Title) + ".m4b",
		RuntimeSec:        float64(it.RuntimeLengthMin) * 60,
		UnavailableReason: reason,
		Preorder:          preorder,
	}
	if len(it.Authors) > 0 {
		rec.Author = it.Authors[0].Name
		for _, a := range it.Authors {
			if a.Name != "" {
				rec.Authors = append(rec.Authors, a.Name)
			}
		}
	}
	if len(it.Narrators) > 0 {
		rec.Narrator = it.Narrators[0].Name
	}
	if len(it.Series) > 0 {
		rec.Series = it.Series[0].Title
		rec.SeriesPosition = strings.TrimSpace(it.Series[0].Sequence)
		if seq, ok := parseSeq(it.Series[0].Sequence); ok {
			rec.Sequence, rec.HasSequence = seq, true
		}
	}
	date, _ := it.dates(now)
	rec.Year = yearFromDate(date)
	rec.ReleaseDate = releaseDay(date)
	return rec
}

// maxAheadYears is how far ahead a release date may plausibly be: Audible
// lists preorders months ahead, never years. A date further out, or in the
// year maxPlausibleYear or later, is a placeholder (Audible sends 2200-01-01
// for some titles it has had for years) and reads as no date.
const (
	maxAheadYears    = 2
	maxPlausibleYear = 2100
)

// plausibleDate reports whether an Audible date can be a real release date:
// parseable, before maxPlausibleYear and at most maxAheadYears after now (a
// zero now checks the year only).
func plausibleDate(s string, now time.Time) bool {
	d := parseAudibleDate(s)
	if d.IsZero() || d.Year() >= maxPlausibleYear {
		return false
	}
	return now.IsZero() || !d.After(now.AddDate(maxAheadYears, 0, 0))
}

// dates is the item's release date and whether a date Audible sent was set
// aside: release_date when it is plausible, else issue_date when that is, else
// "" (unknown, which reads as released: never withhold a real title on a
// placeholder date). odd is true when either non-blank field is implausible.
func (it libItem) dates(now time.Time) (date string, odd bool) {
	for _, d := range []string{it.ReleaseDate, it.IssueDate} {
		switch {
		case strings.TrimSpace(d) == "":
		case !plausibleDate(d, now):
			odd = true
		case date == "":
			date = d
		}
	}
	return date, odd
}

// logOddDate logs, once per title a listing returns, the raw fields behind a
// date it set aside, so a title stored oddly can be explained from the log.
// Only library fields: never a token, the customer id or a request.
func (it libItem) logOddDate(log *slog.Logger, now time.Time) {
	if _, odd := it.dates(now); log == nil || !odd {
		return
	}
	rights := "absent"
	if it.CustomerRights != nil {
		rights = strconv.FormatBool(it.CustomerRights.IsConsumableOffline)
	}
	log.Info("audible: a title's release or issue date is implausible, set aside",
		"asin", it.ASIN, "title", it.Title, "release_date", it.ReleaseDate, "issue_date", it.IssueDate,
		"content_delivery_type", it.ContentDeliveryType, "content_type", it.ContentType,
		"is_consumable_offline", rights)
}

// PreorderReason is a preorder's UnavailableReason.
const PreorderReason = "Not released yet (pre-order)"

// unavailableReason reports why a library item can't be downloaded ("" = it
// can) and whether that is because it is a pre-order. The license endpoint
// would fail these with an opaque error (HTTP 400 for periodicals, an empty
// voucher for titles without offline rights), so classifying them here lets the
// UI say why up front. A show or periodical is checked first (never a
// pre-order), then a plausible release date still ahead (a pre-order, whatever
// its rights say: Audible grants none to a title not out yet), then the
// rights. All checks are defensive: a missing field never marks a book
// unavailable.
func (it libItem) unavailableReason(now time.Time) (reason string, preorder bool) {
	switch it.ContentDeliveryType {
	case "Periodical", "PodcastParent":
		// Audible shows/podcasts are containers of episodes, not a single
		// downloadable audiobook file; a license request is refused outright.
		return "An Audible show/periodical - it has no downloadable audiobook file", false
	}
	if date, _ := it.dates(now); ReleaseIsUpcoming(date, now) {
		return PreorderReason, true
	}
	if it.CustomerRights != nil && !it.CustomerRights.IsConsumableOffline {
		return "Audible reports no download rights for this title (often a Plus/included title that left the catalog)", false
	}
	return "", false
}

// ReleaseIsUpcoming reports whether an Audible release/issue date falls after now -
// a pre-order / not-yet-released title. A zero now disables the check; an empty or
// unparseable date reads as already released (never withhold a real title). It is
// the one definition of "upcoming".
func ReleaseIsUpcoming(date string, now time.Time) bool {
	if now.IsZero() {
		return false
	}
	d := parseAudibleDate(date)
	return !d.IsZero() && d.After(now)
}

// parseAudibleDate parses Audible's release/issue dates, which arrive as
// "2016-08-01" (sometimes with a time suffix), "2016-08", or "2016" depending on
// the endpoint. Zero time when unparseable.
func parseAudibleDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if len(s) > 10 {
		s = s[:10] // a full timestamp leads with the date
	}
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// releaseDay is an Audible date as YYYY-MM-DD when it names a whole day ("" for
// "2016-08", "2016" or anything unparseable).
func releaseDay(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 10 {
		s = s[:10]
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t.Format(time.DateOnly)
	}
	return ""
}

// parseSeq parses the free-form Audible series sequence ("1", "0.5", "1-3", "").
// Only a clean single number counts; anything else leaves the book unsequenced.
func parseSeq(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func yearFromDate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 4 && isDigit4(s[:4]) {
		return s[:4]
	}
	return ""
}

func isDigit4(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) == 4
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func safeFileName(s string) string {
	s = strings.TrimSpace(s)
	r := strings.NewReplacer("/", "-", "\\", "-", ":", " -", "\x00", "")
	out := strings.Trim(r.Replace(s), " .") // no hidden (dot) name, which the server never indexes
	if out == "" {
		out = "audiobook"
	}
	return out
}
