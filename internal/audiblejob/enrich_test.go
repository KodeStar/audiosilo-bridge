package audiblejob

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/source"
)

// backfill is a library of n books without an ASIN and the n titles that match
// them, one each.
func backfill(n int) (*fakeServer, *stubAccounts) {
	fs := &fakeServer{pageSize: 500}
	accts := &stubAccounts{acct: account()}
	for i := range n {
		title := fmt.Sprintf("Quiet Harbour Chronicle %c", 'A'+i)
		fs.books = append(fs.books, link.LibraryBook{Path: "Brook/" + strconv.Itoa(i), Title: title, Authors: []string{"Wren Brook"}})
		accts.recs = append(accts.recs, source.Record{ASIN: fmt.Sprintf("B0PACE%04d", i), Title: title, Author: "Wren Brook"})
	}
	return fs, accts
}

// clock is a fake time the stub sleep advances, recording each wait.
type clock struct {
	t     time.Time
	waits []time.Duration
}

func (c *clock) install(h *Handler) {
	c.t = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return c.t }
	h.sleep = func(ctx context.Context, d time.Duration) error {
		c.waits = append(c.waits, d)
		c.t = c.t.Add(d)
		return ctx.Err()
	}
}

// TestEnrichPaced: the PUTs go out enrichGap apart, never back to back.
func TestEnrichPaced(t *testing.T) {
	t.Parallel()
	fs, accts := backfill(4)
	h := setup(t, fs, accts)
	var c clock
	c.install(h)
	res, err := run(t, h, "acct0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Enriched != 4 || res.EnrichFailed != 0 || res.EnrichDeferred != 0 || len(fs.enrichedNow()) != 4 {
		t.Fatalf("result = %+v, enriched %v", res, fs.enrichedNow())
	}
	if len(c.waits) != 3 {
		t.Fatalf("waits = %v, want 3 gaps", c.waits)
	}
	for _, w := range c.waits {
		if w != enrichGap {
			t.Fatalf("waits = %v, want %v each", c.waits, enrichGap)
		}
	}
}

// TestEnrichHonoursRetryAfter: a 429 waits its Retry-After, slows the pace and
// tries the same book again; none is lost or failed.
func TestEnrichHonoursRetryAfter(t *testing.T) {
	t.Parallel()
	fs, accts := backfill(4)
	fs.limit = func(n int) (string, bool) {
		switch n {
		case 2:
			return "3", true
		case 3:
			return "-", true // no Retry-After: the backoff
		}
		return "", false
	}
	h := setup(t, fs, accts)
	var c clock
	c.install(h)
	res, err := run(t, h, "acct0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Enriched != 4 || res.EnrichFailed != 0 || res.EnrichDeferred != 0 || len(fs.enrichedNow()) != 4 {
		t.Fatalf("result = %+v, enriched %v", res, fs.enrichedNow())
	}
	// The second book: gap, 429 -> its Retry-After (3 s), 429 again -> the
	// backoff (the second in a row: 4 s), then through; the last two at the
	// slower pace (two 429s doubled the gap twice).
	want := []time.Duration{enrichGap, 3 * time.Second, 2 * enrichBackoff, 4 * enrichGap, 4 * enrichGap}
	if len(c.waits) != len(want) || c.waits[2] < enrichBackoff || c.waits[2] > 2*enrichBackoff {
		t.Fatalf("waits = %v, want a jittered second backoff", c.waits)
	}
	want[2] = c.waits[2]
	if fmt.Sprint(c.waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", c.waits, want)
	}
}

func TestEnrichZeroRetryAfter(t *testing.T) {
	t.Parallel()
	for _, header := range []string{"0", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)} {
		t.Run(header, func(t *testing.T) {
			t.Parallel()
			fs, accts := backfill(1)
			fs.limit = func(int) (string, bool) { return header, true }
			h := setup(t, fs, accts)
			var c clock
			c.install(h)
			res, err := run(t, h, "acct0001")
			if err != nil {
				t.Fatal(err)
			}
			if fs.enrichCalls() != enrichMaxLimited || res.EnrichDeferred != 1 || len(c.waits) != enrichMaxLimited-1 {
				t.Fatalf("result = %+v after %d attempts, waits %v", res, fs.enrichCalls(), c.waits)
			}
			for _, wait := range c.waits {
				if wait != 0 {
					t.Fatalf("Retry-After %q waited %v", header, wait)
				}
			}
		})
	}
}

func TestEnrichCancelledDuringZeroRetry(t *testing.T) {
	t.Parallel()
	fs, accts := backfill(1)
	fs.limit = func(int) (string, bool) { return "0", true }
	h := setup(t, fs, accts)
	h.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if _, err := run(t, h, "acct0001"); err == nil || fs.enrichCalls() != 1 {
		t.Fatalf("cancelled retry: err %v, attempts %d", err, fs.enrichCalls())
	}
}

// TestEnrichDefersTheRest: a Retry-After past enrichMaxWait, too many 429s in
// a row, or the poll's time running out leave the rest to the next poll
// (counted, not failed), and a next poll finishes the backfill.
func TestEnrichDefersTheRest(t *testing.T) {
	t.Parallel()
	t.Run("long Retry-After", func(t *testing.T) {
		t.Parallel()
		fs, accts := backfill(5)
		fs.limit = func(n int) (string, bool) { return "600", n == 3 }
		h := setup(t, fs, accts)
		var c clock
		c.install(h)
		res, err := run(t, h, "acct0001")
		if err != nil {
			t.Fatal(err)
		}
		if res.Enriched != 2 || res.EnrichDeferred != 3 || res.EnrichFailed != 0 {
			t.Fatalf("result = %+v", res)
		}
		for _, w := range c.waits {
			if w > enrichMaxWait {
				t.Fatalf("waited %v", w)
			}
		}
		// The next poll: the enriched books carry their ASIN now (an ASIN
		// match, no PUT), the rest get theirs.
		fs.mu.Lock()
		for i := range fs.books {
			if a, ok := fs.enriched[fs.books[i].Path]; ok {
				fs.books[i].ASIN = a
			}
		}
		fs.limit = nil
		fs.mu.Unlock()
		res, err = run(t, h, "acct0001")
		if err != nil {
			t.Fatal(err)
		}
		if res.Enriched != 3 || res.EnrichDeferred != 0 || res.MatchedASIN != 2 || len(fs.enrichedNow()) != 5 {
			t.Fatalf("the next poll = %+v, enriched %v", res, fs.enrichedNow())
		}
	})
	t.Run("429s in a row", func(t *testing.T) {
		t.Parallel()
		fs, accts := backfill(3)
		fs.limit = func(n int) (string, bool) { return "1", n > 1 }
		h := setup(t, fs, accts)
		var c clock
		c.install(h)
		res, err := run(t, h, "acct0001")
		if err != nil {
			t.Fatal(err)
		}
		// One through, then enrichMaxLimited 429s in a row: the last of them
		// leaves the rest, without a wait.
		if res.Enriched != 1 || res.EnrichDeferred != 2 || res.EnrichFailed != 0 || fs.enrichCalls() != 1+enrichMaxLimited {
			t.Fatalf("result = %+v after %d calls", res, fs.enrichCalls())
		}
	})
	t.Run("the poll's time runs out", func(t *testing.T) {
		t.Parallel()
		fs, accts := backfill(4)
		h := setup(t, fs, accts)
		var c clock
		c.install(h)
		h.sleep = func(ctx context.Context, d time.Duration) error {
			c.t = c.t.Add(enrichBudget / 2) // each wait is half the budget
			return ctx.Err()
		}
		res, err := run(t, h, "acct0001")
		if err != nil {
			t.Fatal(err)
		}
		if res.Enriched != 2 || res.EnrichDeferred != 2 {
			t.Fatalf("result = %+v", res)
		}
	})
}

// TestEnrichCancelled: a job cancelled while it waits ends with its context's
// error and records no failure.
func TestEnrichCancelled(t *testing.T) {
	t.Parallel()
	fs, accts := backfill(3)
	h := setup(t, fs, accts)
	h.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if _, err := run(t, h, "acct0001"); err == nil {
		t.Fatal("a cancelled wait finished the poll")
	}
	if len(accts.failed) != 0 || len(fs.enrichedNow()) != 1 {
		t.Fatalf("failed %v, enriched %v", accts.failed, fs.enrichedNow())
	}
}

// TestPollContestedKeepsTheBest: titles claiming one book keep the match on
// the one whose series number is the book's, else the best score; a tie
// between two works nothing tells apart keeps none; the same work owned twice
// keeps both; the losers go out with the reason contested and the book's path,
// unless their number says they are another volume (no reason: plainly not
// here); a contested book is never enriched. The Lanterns book has no number
// ("") or its folder code's ("2", on a main series Audible doesn't name).
func TestPollContestedKeepsTheBest(t *testing.T) {
	t.Parallel()
	for _, lt := range []string{"", "2"} {
		t.Run("lanterns "+lt, func(t *testing.T) {
			t.Parallel()
			const ls02 = "Aar/Lantern Street/LS02 - Moon over Tidewater"
			fs := &fakeServer{pageSize: 500, books: []link.LibraryBook{
				{Path: "Val/Road to Craft/RC03", Title: "Road to Craft 3", Authors: []string{"Val Erio"},
					Series: []link.SeriesRef{{Name: "Road to Craft", Position: "3"}}},
				{Path: ls02, Title: "Moon over Tidewater: Lanterns of London, Book 2",
					Authors: []string{"Ben Aar"}, Series: []link.SeriesRef{{Name: "Lantern Street", Position: lt}}},
				{Path: "Prat/Discs/D03 - Equal Measures", Title: "Equal Measures", Authors: []string{"Tess Prat"},
					Series: []link.SeriesRef{{Name: "Discs", Position: "3"}}},
			}}
			accts := &stubAccounts{acct: account(), recs: []source.Record{
				{ASIN: "B0CONT0001", Title: "Road to Craft", Author: "Val Erio", Series: "Road to Craft", SeriesPosition: "1", Sequence: 1, HasSequence: true},
				{ASIN: "B0CONT0003", Title: "Road to Craft 3", Author: "Val Erio", Series: "Road to Craft", SeriesPosition: "3", Sequence: 3, HasSequence: true},
				{ASIN: "B0CONT0011", Title: "Lanterns of London", Author: "Ben Aar", Series: "Lanterns of London", SeriesPosition: "1", Sequence: 1, HasSequence: true},
				{ASIN: "B0CONT0012", Title: "Moon over Tidewater", Author: "Ben Aar", Series: "Lanterns of London", SeriesPosition: "2", Sequence: 2, HasSequence: true},
				{ASIN: "B0CONT0021", Title: "Equal Measures", Author: "Tess Prat", Series: "Discs", SeriesPosition: "3", Sequence: 3, HasSequence: true},
				{ASIN: "B0CONT0022", Title: "Equal Measures", Author: "Tess Prat", Series: "Discs", SeriesPosition: "3", Sequence: 3, HasSequence: true},
				{ASIN: "B0CONT0031", Title: "A Title Nowhere", Author: "Tess Prat"},
			}}
			h := setup(t, fs, accts)
			res, err := run(t, h, "acct0001")
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, tt := range fs.calls[0].Titles {
				if tt.Match != nil {
					got[tt.ASIN] = tt.Match.Path
				}
			}
			want := map[string]string{
				"B0CONT0003": "Val/Road to Craft/RC03",
				"B0CONT0021": "Prat/Discs/D03 - Equal Measures",
				"B0CONT0022": "Prat/Discs/D03 - Equal Measures",
			}
			contested := map[string]string{"B0CONT0011": ls02, "B0CONT0012": ls02} // nothing tells them apart
			if lt == "2" {
				want["B0CONT0012"], contested = ls02, map[string]string{} // book 2 by its number; book 1 is another volume
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("matches = %v, want %v", got, want)
			}
			// The titles that lost a book they may be say so and name it; a
			// title of another volume, one that matched nothing, or one that
			// kept its match carries no reason.
			for _, tt := range fs.calls[0].Titles {
				wantReason, wantPath := "", contested[tt.ASIN]
				if wantPath != "" {
					wantReason = link.ReasonContested
				}
				if tt.Reason != wantReason || tt.ContestedPath != wantPath || tt.State != link.AudibleAvailable {
					t.Errorf("%s: state %q reason %q contested_path %q, want available %q %q", tt.ASIN, tt.State, tt.Reason, tt.ContestedPath, wantReason, wantPath)
				}
			}
			if res.Matched != len(want) || res.Contested != 3 || res.Enriched != 0 || len(fs.enrichedNow()) != 0 {
				t.Fatalf("result = %+v, enriched %v", res, fs.enrichedNow())
			}
		})
	}
}

// TestPollOtherVolumeNotEnriched: the opening volume's bare title matches a
// numbered sequel's folder (pkg/match lets volume 1 claim a bare-titled book).
// With the sequel not owned that is the folder's only claim, but the numbers
// disagree, so the opening volume's ASIN never goes onto the sequel's folder.
// The same title on its own folder is enriched.
func TestPollOtherVolumeNotEnriched(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pos  string
		want map[string]string
	}{
		{"3", map[string]string{}},
		{"1", map[string]string{"Val/Road to Craft/RC03": "B0CONT0001"}},
	} {
		fs := &fakeServer{pageSize: 500, books: []link.LibraryBook{
			{Path: "Val/Road to Craft/RC03", Title: "Road to Craft " + tc.pos, Authors: []string{"Val Erio"},
				Series: []link.SeriesRef{{Name: "Road to Craft", Position: tc.pos}}},
		}}
		accts := &stubAccounts{acct: account(), recs: []source.Record{
			{ASIN: "B0CONT0001", Title: "Road to Craft", Author: "Val Erio", Series: "Road to Craft", SeriesPosition: "1", Sequence: 1, HasSequence: true},
		}}
		h := setup(t, fs, accts)
		res, err := run(t, h, "acct0001")
		if err != nil {
			t.Fatal(err)
		}
		if res.Matched != 1 || fmt.Sprint(fs.enrichedNow()) != fmt.Sprint(tc.want) {
			t.Fatalf("book #%s: result = %+v, enriched %v, want %v", tc.pos, res, fs.enrichedNow(), tc.want)
		}
	}
}

// TestPollPackOnVolumeNotEnriched: a pack whose title fell back to the folder
// at its first volume's position matches it, but that folder may hold one
// volume, so the pack's ASIN never goes onto it; a pack on its own range
// folder is enriched.
func TestPollPackOnVolumeNotEnriched(t *testing.T) {
	t.Parallel()
	const boxPath = "Quill/Orchard Cycle/OC01-03"
	fs := &fakeServer{pageSize: 500, books: []link.LibraryBook{
		{Path: boxPath, Title: "The Orchard Cycle", Authors: []string{"Mara Quill"},
			Series: []link.SeriesRef{{Name: "Orchard Cycle", Position: "1-3"}}},
		{Path: "Fell/Iron Vale/IV05 - Ash Road", Title: "Ash Road", Authors: []string{"Ona Fell"},
			Series: []link.SeriesRef{{Name: "Iron Vale", Position: "5"}}},
		{Path: "Fell/Iron Vale/IV07-08", Title: "Iron Vale", Authors: []string{"Ona Fell"},
			Series: []link.SeriesRef{{Name: "Iron Vale", Position: "7-8"}}},
	}}
	accts := &stubAccounts{acct: account(), recs: []source.Record{
		{ASIN: "B0PACK0001", Title: "The Orchard Cycle Box Set", Author: "Mara Quill", Series: "Orchard Cycle"},
		{ASIN: "B0SOLO0001", Title: "The Orchard Cycle", Author: "Mara Quill", Series: "Orchard Cycle"},
		{ASIN: "B0PACK0056", Title: "Iron Vale: Books 5-6", Author: "Ona Fell", Series: "Iron Vale", SeriesPosition: "5-6"},
		{ASIN: "B0PACK0078", Title: "Iron Vale: Books 7-8", Author: "Ona Fell", Series: "Iron Vale", SeriesPosition: "7-8"},
	}}
	h := setup(t, fs, accts)
	res, err := run(t, h, "acct0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 3 || res.Enriched != 2 {
		t.Fatalf("result = %+v, enriched %v", res, fs.enrichedNow())
	}
	if want := map[string]string{boxPath: "B0PACK0001", "Fell/Iron Vale/IV07-08": "B0PACK0078"}; fmt.Sprint(fs.enrichedNow()) != fmt.Sprint(want) {
		t.Fatalf("enriched %v, want %v", fs.enrichedNow(), want)
	}
}
