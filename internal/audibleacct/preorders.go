package audibleacct

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// PreorderDropAfter is how long after its release date a remembered preorder
// the library still doesn't list is dropped: by then it was cancelled, or is
// a title Audible never delivered, and the server has stopped waiting for it
// (its own wait is 14 days).
const PreorderDropAfter = 30 * 24 * time.Hour

// DeliveredDropAfter is how long a remembered preorder whose order was seen
// delivered is kept while the library still doesn't list it, counted from
// when the order closed (bridge#60): the library lists a delivered title soon
// after, so one it never does was refunded or released under another ASIN,
// and holding it longer only keeps it out of the sync.
const DeliveredDropAfter = 14 * 24 * time.Hour

// undatedPreorderYears: a remembered preorder the catalog never dated (no
// date, or a placeholder one) counts as released this many years after its
// order was last seen pending, and is dropped PreorderDropAfter later: Audible
// dates a preorder at most two years ahead (audible's plausibleDate), so by
// then the library lists it or it was cancelled.
const undatedPreorderYears = 2

// Preorders are account id's pending preorders, for a poll that listed
// library (the account's Audible library): every title a pending preorder
// order holds that the library doesn't list (the library wins), remembered
// in the store (store.AudiblePreorder) so one whose order has left the orders
// listing's window (about six months) is still reported, each described by
// the catalog (read again every poll, so a moved release date is picked up),
// as a record with Preorder set and audible.PreorderReason.
//
// A remembered preorder is dropped when the library lists it, when its order
// is seen cancelled (none pending; a delivered order keeps it for
// DeliveredDropAfter, see reconcilePreorders), when an orders listing that reaches back past its
// order date no longer holds it (cancelled), or when its release date is
// PreorderDropAfter past (one the catalog never dated: undatedPreorderYears
// after its order was last seen pending, then PreorderDropAfter).
//
// It never fails the poll: the orders or the catalog failing logs one warning
// each (Audible's answer, redacted, never a credential) and the remembered
// preorders are still reported. A failed orders listing still has the catalog
// describe them, unless Audible asked the Bridge to wait (429/503) or the
// sign-in can't be opened; a failed catalog leaves each with its last
// description (one that fails part way still describes the titles it read).
// A preorder
// the catalog never described is reported by its ASIN alone (the server takes
// a title without a title). The store failing is a warning too (nothing is
// then written back, so nothing remembered is lost).
func (m *Manager) Preorders(ctx context.Context, id string, library []source.Record) []source.Record {
	now := m.o.Now()
	inLibrary := make(map[string]bool, len(library))
	for _, r := range library {
		inLibrary[strings.ToUpper(strings.TrimSpace(r.ASIN))] = true
	}
	rows, err := m.o.DB.AudiblePreorders(ctx, id)
	stored := err == nil
	if err != nil {
		m.o.Log.Warn("audible: reading the remembered preorders failed", "account", id, "err", err)
	}
	creds, err := m.open(id)
	if err != nil {
		m.o.Log.Warn("audible: the account's sign-in can't be opened for its preorders", "account", id, "err", err)
		return preorderRecords(dropReleased(reconcilePreorders(rows, inLibrary, nil, now), now))
	}

	orders, err := m.o.Service.Orders(ctx, creds)
	_, wait := audible.RetryWait(err)
	if err != nil {
		orders = nil // a failed listing drops and adds nothing
		if ctx.Err() == nil {
			m.o.Log.Warn("audible: listing the orders failed: reporting the preorders remembered", "account", id, "err", err)
		}
	}
	rows = reconcilePreorders(rows, inLibrary, orders, now)

	if len(rows) > 0 && !wait {
		asins := make([]string, len(rows))
		for i, r := range rows {
			asins[i] = r.ASIN
		}
		recs, err := m.o.Service.Products(ctx, creds, asins)
		if err != nil && ctx.Err() == nil {
			m.o.Log.Warn("audible: reading the preorders from the catalog failed: reporting what it said before", "account", id, "err", err)
		}
		fillPreorders(rows, recs)
	}
	rows = dropReleased(rows, now)

	if stored && ctx.Err() == nil {
		if err := m.o.DB.SetAudiblePreorders(context.WithoutCancel(ctx), id, rows); err != nil {
			m.o.Log.Warn("audible: remembering the preorders failed", "account", id, "err", err)
		}
	}
	return preorderRecords(rows)
}

// rememberedPreorder is account id's remembered pending preorder asin as a
// record (false when none is remembered, or the store can't say).
func (m *Manager) rememberedPreorder(ctx context.Context, id, asin string) (source.Record, bool) {
	rows, err := m.o.DB.AudiblePreorders(ctx, id)
	if err != nil {
		return source.Record{}, false
	}
	for _, r := range rows {
		if strings.EqualFold(r.ASIN, asin) {
			return preorderRecord(r), true
		}
	}
	return source.Record{}, false
}

// reconcilePreorders is the remembered preorders after an orders listing
// (none when it failed: then only the library rule applies), at now. A
// title a pending preorder order holds is added (ordered at its order's date,
// else now) or seen again (last seen now); a remembered one is dropped when
// the library lists it, when an order holds it cancelled (neither pending nor
// delivered) and none pending or delivered, when it is missing from every
// order of a listing whose oldest order is older than its order (it was in the
// window, so it was cancelled), or when an order holds it delivered, none
// pending, and it has waited DeliveredDropAfter for the library since
// (deliveredLongAgo). By ASIN.
func reconcilePreorders(rows []store.AudiblePreorder, inLibrary map[string]bool, orders []audible.Order, now time.Time) []store.AudiblePreorder {
	pending := map[string]time.Time{}
	cancelled, delivered := map[string]bool{}, map[string]bool{}
	listed := map[string]bool{} // in any order of the listing
	var oldest time.Time
	for _, o := range orders {
		if !o.Date.IsZero() && (oldest.IsZero() || o.Date.Before(oldest)) {
			oldest = o.Date
		}
		for _, it := range o.Items {
			a := strings.ToUpper(strings.TrimSpace(it.ASIN))
			if !ASINPattern.MatchString(a) {
				continue
			}
			listed[a] = true
			if !o.PendingPreorder() {
				// A delivered order keeps a remembered preorder until the
				// library lists it (it may close before the library does),
				// even beside an earlier cancelled one, for at most
				// DeliveredDropAfter; any other state (cancelled) drops it.
				if o.Delivered() {
					delivered[a] = true
				} else {
					cancelled[a] = true
				}
				continue
			}
			// The earliest pending order's date (an unknown one gives way).
			if d, ok := pending[a]; !ok || d.IsZero() || (!o.Date.IsZero() && o.Date.Before(d)) {
				pending[a] = o.Date
			}
		}
	}
	out := make([]store.AudiblePreorder, 0, len(rows)+len(pending))
	have := map[string]bool{}
	for _, r := range rows {
		a := r.ASIN
		if inLibrary[a] || have[a] {
			continue
		}
		if d, ok := pending[a]; ok {
			r.LastSeenAt = now
			if r.OrderedAt.IsZero() {
				r.OrderedAt = orderedAt(d, now)
			}
		} else if (cancelled[a] && !delivered[a]) || (!listed[a] && !oldest.IsZero() && !r.OrderedAt.IsZero() && oldest.Before(r.OrderedAt)) {
			continue
		} else if delivered[a] && deliveredLongAgo(r, now) {
			continue
		}
		have[a] = true
		out = append(out, r)
	}
	for a, d := range pending {
		if inLibrary[a] || have[a] {
			continue
		}
		have[a] = true
		out = append(out, store.AudiblePreorder{ASIN: a, OrderedAt: orderedAt(d, now), LastSeenAt: now})
	}
	slices.SortFunc(out, func(x, y store.AudiblePreorder) int { return strings.Compare(x.ASIN, y.ASIN) })
	return out
}

// deliveredLongAgo reports whether r, a remembered preorder whose order is
// seen delivered, has waited DeliveredDropAfter for the library since its
// order closed. The listing gives no closing date, so it counts from the
// later of when a poll last saw the order pending and the catalog's release
// date: both come before the close. Never, when neither is known.
func deliveredLongAgo(r store.AudiblePreorder, now time.Time) bool {
	closed := r.LastSeenAt
	if d, ok := releaseDate(r); ok && d.After(closed) {
		closed = d
	}
	return !closed.IsZero() && !now.Before(closed.Add(DeliveredDropAfter))
}

// orderedAt is an order's date, or now when it had none.
func orderedAt(d, now time.Time) time.Time {
	if d.IsZero() {
		return now
	}
	return d
}

// fillPreorders records the catalog's description of each row recs answers
// for (by ASIN). A row the catalog didn't answer keeps what it had.
func fillPreorders(rows []store.AudiblePreorder, recs []source.Record) {
	by := make(map[string]source.Record, len(recs))
	for _, r := range recs {
		by[strings.ToUpper(strings.TrimSpace(r.ASIN))] = r
	}
	for i := range rows {
		r, ok := by[rows[i].ASIN]
		if !ok {
			continue
		}
		raw, err := json.Marshal(r)
		if err != nil {
			continue
		}
		rows[i].Record = string(raw)
	}
}

// dropReleased drops the rows whose release date (the catalog's) is
// PreorderDropAfter or more before now, a row with none counting as released
// undatedPreorderYears after its order was last seen pending (never, when
// that is unknown).
func dropReleased(rows []store.AudiblePreorder, now time.Time) []store.AudiblePreorder {
	return slices.DeleteFunc(rows, func(r store.AudiblePreorder) bool {
		d, ok := releaseDate(r)
		if !ok {
			if r.LastSeenAt.IsZero() {
				return false
			}
			d = r.LastSeenAt.AddDate(undatedPreorderYears, 0, 0)
		}
		return !now.Before(d.Add(PreorderDropAfter))
	})
}

// releaseDate is r's release date as the catalog described it, if it did.
func releaseDate(r store.AudiblePreorder) (time.Time, bool) {
	d, err := time.Parse(time.DateOnly, preorderRecord(r).ReleaseDate)
	return d, err == nil
}

// preorderRecord is a row as a record: the catalog's description when it has
// one (a cache the next fill replaces, so a field a later build renames only
// waits for that), its ASIN, Preorder and audible.PreorderReason always.
func preorderRecord(r store.AudiblePreorder) source.Record {
	var rec source.Record
	if r.Record != "" {
		_ = json.Unmarshal([]byte(r.Record), &rec)
	}
	rec.ASIN, rec.Preorder, rec.UnavailableReason = r.ASIN, true, audible.PreorderReason
	return rec
}

func preorderRecords(rows []store.AudiblePreorder) []source.Record {
	out := make([]source.Record, len(rows))
	for i, r := range rows {
		out[i] = preorderRecord(r)
	}
	return out
}
