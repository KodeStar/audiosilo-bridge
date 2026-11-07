package audible

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// flexMS unmarshals a millisecond value that Audible may encode as either a JSON
// number or a quoted string.
type flexMS int64

func (f *flexMS) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		*f = flexMS(n)
		return nil
	}
	fl, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("position_ms %q: %w", s, err)
	}
	*f = flexMS(int64(fl))
	return nil
}

// lastPositionsBatch is the starting number of ASINs per annotations request.
// Chunking is required because a whole (large) library in one query overflows the
// URL length limit (Audible returns HTTP 414, Request-URI Too Long) and the
// endpoint separately rejects an over-long ASIN list with an HTTP 400 validation
// error ("1 validation error detected ... Member must have length less than or
// equal to N"). Audible doesn't document N and it varies, so lastPositionsChunk
// self-tunes below this on a 400; this is just a low-request-count starting point.
const lastPositionsBatch = 25

// Position is a book's last-heard place on Audible: milliseconds into the book,
// and when Audible recorded it (zero when it didn't say, or said it in a shape
// that doesn't parse). Sync compares LastUpdated with the server's place.
type Position struct {
	PositionMs  int64
	LastUpdated time.Time
}

// LastPositions fetches the last-heard position for each ASIN from Audible's
// annotations service. ASINs with no recorded position are omitted.
// The ASIN list is chunked (lastPositionsBatch per request) so a large library
// doesn't overflow the query-string length limit; results are merged. onProgress
// (may be nil) is called with the cumulative count of ASINs fetched after each
// batch, so a caller can show a progress bar over a large library.
func (c *Client) LastPositions(ctx context.Context, asins []string, onProgress func(done int)) (map[string]Position, error) {
	out := make(map[string]Position, len(asins))
	for start := 0; start < len(asins); start += lastPositionsBatch {
		end := min(start+lastPositionsBatch, len(asins))
		if err := c.lastPositionsChunk(ctx, asins[start:end], out); err != nil {
			return nil, err
		}
		if onProgress != nil {
			onProgress(end)
		}
	}
	return out, nil
}

// lastPositionsChunk fetches one batch of ASINs and merges the "Exists" positions
// into out, split by splitOnOverflow.
func (c *Client) lastPositionsChunk(ctx context.Context, asins []string, out map[string]Position) error {
	return splitOnOverflow(asins, func(batch []string) error {
		path := "/1.0/annotations/lastpositions?" + url.Values{"asins": {strings.Join(batch, ",")}}.Encode()
		var resp struct {
			Annots []struct {
				ASIN string `json:"asin"`
				Last struct {
					Status      string `json:"status"`
					PositionMs  flexMS `json:"position_ms"`
					LastUpdated string `json:"last_updated"`
				} `json:"last_position_heard"`
			} `json:"asin_last_position_heard_annots"`
		}
		if err := c.doSigned(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return err
		}
		for _, a := range resp.Annots {
			if a.Last.Status != "Exists" {
				continue
			}
			out[a.ASIN] = Position{PositionMs: int64(a.Last.PositionMs), LastUpdated: parseLastUpdated(a.Last.LastUpdated)}
		}
		return nil
	})
}

// splitOnOverflow calls fetch with asins, and when Audible refuses a list of
// more than one for its length, calls it again with each half. Audible caps
// an ASIN list's length per request but doesn't document the limit and 400s
// the whole batch when exceeded (a list-length constraint, not a per-ASIN
// one); a 414 (the URL too long for Amazon's front end) is the same overflow
// met before the list-length check. So it converges on whatever the real cap
// is without a magic number, and a single ASIN can't overflow a list-length
// cap, so the recursion always terminates (fetch's error for one is returned).
func splitOnOverflow(asins []string, fetch func([]string) error) error {
	if len(asins) == 0 {
		return nil
	}
	err := fetch(asins)
	if len(asins) > 1 && listOverflow(err) {
		mid := len(asins) / 2
		if err := splitOnOverflow(asins[:mid], fetch); err != nil {
			return err
		}
		return splitOnOverflow(asins[mid:], fetch)
	}
	return err
}

// listOverflow reports whether err is Audible refusing an ASIN list for its
// length (a 400 or a 414).
func listOverflow(err error) bool {
	s := HTTPStatus(err)
	return s == http.StatusBadRequest || s == http.StatusRequestURITooLong
}

// lastUpdatedLayouts are the shapes Audible's last_updated has been seen in
// ("2022-03-10 12:10:13.093", UTC), plus RFC 3339 in case it moves to that.
var lastUpdatedLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	time.RFC3339Nano,
}

// parseLastUpdated reads a last_updated value as UTC; zero when it doesn't parse.
func parseLastUpdated(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range lastUpdatedLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// SetLastPosition writes a last-heard position (milliseconds) back to Audible so
// other devices reflect server-side progress, via PUT /1.0/lastpositions/{asin}
// with {acr, asin, position_ms} (per the mkb79/Audible API reference). The acr is
// the Audible Content Reference from a license request (see ContentACR). Gated
// behind an explicit confirmation in the UI; a non-2xx is a per-book failure.
func (c *Client) SetLastPosition(ctx context.Context, asin, acr string, positionMs int64) error {
	if asin == "" || acr == "" {
		return fmt.Errorf("asin and acr are required to set a position")
	}
	body, err := json.Marshal(map[string]any{
		"acr":         acr,
		"asin":        asin,
		"position_ms": positionMs, // numeric milliseconds
	})
	if err != nil {
		return err
	}
	return c.doSigned(ctx, http.MethodPut, "/1.0/lastpositions/"+url.PathEscape(asin), body, nil)
}
