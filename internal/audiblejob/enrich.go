package audiblejob

import (
	"context"
	"errors"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The enrichment's pace. A first poll can backfill hundreds of ASINs, one PUT
// each, and the server's address limiter (shared with the house's players)
// answered most of a quick run 429. So the PUTs go out at most one per
// enrichGap, a 429 waits its Retry-After (else enrichBackoff, doubling) and
// slows the pace, and what the poll can't finish waits for the next one: those
// books still have no ASIN, so the next poll picks them up again.
const (
	// enrichGap is the shortest time between two enrichment PUTs (5 a second).
	enrichGap = 200 * time.Millisecond
	// enrichMaxGap is the slowest pace 429s push the gap to.
	enrichMaxGap = 2 * time.Second
	// enrichBackoff is the first wait after a 429 without a Retry-After; each
	// consecutive 429 doubles it, up to enrichMaxWait.
	enrichBackoff = 2 * time.Second
	// enrichMaxWait caps one wait: a Retry-After above it leaves the rest of
	// the backfill to the next poll rather than holding the job.
	enrichMaxWait = time.Minute
	// enrichMaxLimited is how many 429s in a row leave the rest to the next poll.
	enrichMaxLimited = 5
	// enrichBudget is the longest one poll spends enriching.
	enrichBudget = 10 * time.Minute
)

// hit is a matched book to give its title's ASIN.
type hit struct {
	path, asin string
}

// enrich backfills each hit's ASIN, paced (see enrichGap), counting into res:
// Enriched, EnrichFailed, and EnrichDeferred for the books left to the next
// poll. Only a cancelled job is an error.
func (h *Handler) enrich(ctx context.Context, srv Server, acct store.AudibleAccount, hits []hit, rep *jobs.Reporter, res *Result) error {
	if len(hits) == 0 {
		return nil
	}
	msg := "Adding the ASINs of the matched books"
	rep.Progress(jobs.Progress{Step: "enriching", Message: msg, Total: len(hits)})
	deadline := h.now().Add(enrichBudget)
	gap := enrichGap
	for i, book := range hits {
		if i > 0 {
			if err := h.sleep(ctx, gap); err != nil {
				return err
			}
		}
		var waitErr error
		err := enrichLimit.Do(ctx, func(ctx context.Context, wait time.Duration) error {
			gap = min(gap*2, enrichMaxGap)
			rep.Progress(jobs.Progress{Step: "enriching", Message: "The server asked the Bridge to slow down: waiting", Done: i, Total: len(hits)})
			waitErr = h.sleep(ctx, wait)
			return waitErr
		}, func(ctx context.Context) error {
			if !h.now().Before(deadline) {
				return errEnrichBudget
			}
			return srv.Enrich(ctx, acct.LibraryID, book.path, book.asin)
		})
		if waitErr != nil {
			return waitErr
		}
		if _, limited := link.RateLimited(err); limited || errors.Is(err, errEnrichBudget) {
			res.EnrichDeferred = len(hits) - i
			break
		}
		switch {
		case err == nil:
			res.Enriched++
		case errors.Is(err, link.ErrASINSet), errors.Is(err, link.ErrNoBook):
			// Another ASIN arrived meanwhile, or the book moved: the next poll sees it.
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			res.EnrichFailed++
			h.o.Log.Warn("audible: adding a book's ASIN failed", "account", acct.ID, "library", acct.LibraryID, "err", err)
		}
		rep.Progress(jobs.Progress{Step: "enriching", Message: msg, Done: i + 1, Total: len(hits)})
	}
	if res.EnrichDeferred > 0 {
		h.o.Log.Info("audible: the rest of the ASIN backfill waits for the next poll", "account", acct.ID,
			"enriched", res.Enriched, "deferred", res.EnrichDeferred)
	}
	return nil
}

var errEnrichBudget = errors.New("the poll has spent its time adding ASINs")

// enrichLimit is how an enrichment PUT waits out the server's 429s: its
// Retry-After, else enrichBackoff doubling; enrichMaxLimited 429s in a row,
// or a wait over enrichMaxWait, leaves the PUT.
var enrichLimit = link.Backoff{Tries: enrichMaxLimited, First: enrichBackoff, Max: enrichMaxWait}

// sleepCtx waits d, or until ctx ends. An ended ctx always wins, so a zero
// wait (an explicit Retry-After: 0) never lets a cancelled job go on.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil || d <= 0 {
		return err
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
