package audiblesync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The push loop's pace.
const (
	// PushEvery is how often the loop reads the server's places.
	PushEvery = time.Minute
	// StillFor is how long a place must have been still (its row unchanged)
	// before it is written to Audible: the listener stopped.
	StillFor = 3 * time.Minute
	// offWait is how long an account the server answers sync_off for is left
	// before the loop asks again; refusedWait the same for an account whose
	// person can't be synced for now, or one the server doesn't know.
	offWait     = 5 * time.Minute
	refusedWait = 15 * time.Minute
	// oldServerWait is how long an account is left when the server has no
	// progress feed at all (link.ErrNoAudibleSync: a server older than 2.6.0,
	// before Audible sync), rather than asking every minute. Only the second
	// such answer in a row counts (oldServerStrikes): a proxy in front of a
	// server that is restarting answers a bare 404 too.
	oldServerWait = time.Hour
	// oldServerStrikes is how many route-missing 404s in a row, errorWait
	// apart, make the server an old one.
	oldServerStrikes = 2
	// errorWait is the least wait after a failure (the server's or Audible's).
	errorWait = 2 * time.Minute
	// resetStrikes is how many passes in a row the server may refuse the
	// push's cursor (each a read from the start) before the account is left
	// for resetWait: a server refusing even the cursors it gave would
	// otherwise send the push back to the feed's start every pass.
	resetStrikes = 3
	resetWait    = time.Hour
	// baselineWindow: the first read of an account's feed to its end (the
	// baseline, push_from still unset) takes the places changed this recently
	// (by the row's updated_at) as waiting to be pushed; older ones are
	// history and stay where they are. After it, every row the feed gives is
	// new: it pages in the order the server wrote them, not by updated_at.
	// A read from the start after a refused cursor (push_reread) keeps the
	// same line: the window before the baseline's end, push_from.
	baselineWindow = 10 * time.Minute
	// maxPages bounds one read of an account's feed (200 rows a page).
	maxPages = 50
)

// Pusher is the push loop.
type Pusher struct {
	o Options

	mu sync.Mutex
	// paused: accounts left alone until a time (sync off on the server, a
	// person that can't be synced for), in memory: a restart asks again.
	paused map[string]time.Time
	// noFeed counts each account's route-missing 404s in a row
	// (oldServerStrikes); any other answer ends the run.
	noFeed map[string]int
	// resets counts each account's passes that ended on a refused cursor
	// (resetStrikes); a pass the server took a cursor it gave in ends the
	// run (a read from the start sends none, so it neither counts nor ends it).
	resets map[string]int
}

// NewPusher returns the push loop.
func NewPusher(o Options) *Pusher {
	o.defaults()
	return &Pusher{o: o, paused: map[string]time.Time{}, noFeed: map[string]int{}, resets: map[string]int{}}
}

// Run pushes every PushEvery until ctx ends.
func (p *Pusher) Run(ctx context.Context) {
	t := time.NewTicker(PushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Tick(ctx)
		}
	}
}

// Tick is one pass over the accounts: each account mapped on the paired
// server, not refused and not waiting reads its feed and writes the places
// that are due.
func (p *Pusher) Tick(ctx context.Context) {
	srv := p.o.server()
	serverID := p.o.ServerID()
	if srv == nil || serverID == "" {
		return
	}
	accts, err := p.o.Accounts.Accounts(ctx)
	if err != nil {
		p.o.Log.Warn("audible: listing the accounts for the push failed", "err", err)
		return
	}
	now := p.o.Now()
	for _, a := range accts {
		if ctx.Err() != nil {
			return
		}
		if a.OnOtherServer(serverID) || a.ServerID == "" || a.RefusedAt != "" || p.isPaused(a.ID, now) {
			continue
		}
		if _, ok := waiting(a, now); ok {
			continue
		}
		p.account(ctx, srv, a, now)
	}
}

func (p *Pusher) isPaused(id string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.paused[id]
	if ok && !now.Before(until) {
		delete(p.paused, id)
		return false
	}
	return ok
}

func (p *Pusher) pause(id string, until time.Time) {
	p.mu.Lock()
	p.paused[id] = until
	p.mu.Unlock()
}

// strike counts one more in a row for id in runs (noFeed, resets) when hit,
// else ends id's run, and returns the count.
func (p *Pusher) strike(runs map[string]int, id string, hit bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !hit {
		delete(runs, id)
		return 0
	}
	runs[id]++
	return runs[id]
}

// account reads a's feed from its cursor, records the places waiting, then
// writes the ones due.
func (p *Pusher) account(ctx context.Context, srv Server, a store.AudibleAccount, now time.Time) {
	list, err := p.o.DB.AudiblePushes(ctx, a.ID)
	if err != nil {
		p.o.Log.Warn("audible: listing the places to push failed", "account", a.ID, "err", err)
		return
	}
	titles := make(map[string]store.AudiblePush, len(list))
	for _, x := range list {
		titles[x.ASIN] = x
	}
	cursor := a.PushCursor
	end := false        // the feed was read to its end
	unrecorded := false // a row wasn't recorded: the cursor stays, so it is read again
	took := false       // the server took a cursor it gave (a read from the start sends none)
	for range maxPages {
		asked := cursor
		page, err := srv.AudibleProgress(ctx, a.ID, cursor, link.MaxAudibleProgress)
		strikes := p.strike(p.noFeed, a.ID, errors.Is(err, link.ErrNoAudibleSync))
		switch {
		case errors.Is(err, link.ErrSyncOff):
			p.pause(a.ID, now.Add(offWait))
			p.o.syncOff(ctx, a)
			return
		case errors.Is(err, link.ErrUserUnavailable), errors.Is(err, link.ErrUnknownAccount):
			p.pause(a.ID, now.Add(refusedWait))
			p.o.health(ctx, store.SyncPush, a, err, time.Time{})
			return
		case errors.Is(err, link.ErrBadCursor):
			// The server refused the cursor (a server restored from a backup,
			// its feed rebuilt, one past the feed's end): sent again it fails
			// the same way every pass, stopping the push for good.
			p.cursorRefused(ctx, a, err, now)
			return
		case strikes > 0 && strikes < oldServerStrikes:
			// A bare 404 may be a proxy's while the server restarts: asked
			// again after errorWait, and only a second one in a row is an old
			// server (nothing recorded until then).
			p.o.Log.Info("audible: the server answered 404 for its places feed; asking again shortly before taking it "+
				"for a server older than 2.6.0", "account", a.ID)
			p.pause(a.ID, now.Add(errorWait))
			return
		case strikes > 0:
			p.o.Log.Warn("audible: the server has no places feed (it is older than 2.6.0); asking again in an hour", "account", a.ID)
			p.pause(a.ID, now.Add(oldServerWait))
			p.o.health(ctx, store.SyncPush, a, err, time.Time{})
			return
		case err != nil:
			if ctx.Err() == nil {
				p.o.Log.Warn("audible: reading the server's places failed", "account", a.ID, "err", err)
				p.o.health(ctx, store.SyncPush, a, fmt.Errorf("reading the server's places failed: %w", err), time.Time{})
				// The server asking to wait pauses this loop only: it is no
				// reason to leave Audible alone (the sync's wait).
				var e *link.APIError
				if errors.As(err, &e) && (e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable) {
					p.pause(a.ID, p.o.Now().Add(max(e.RetryAfter, errorWait)))
				}
			}
			return
		}
		took = took || asked != ""
		// The skew is against the Bridge's clock when the page came back, not
		// the pass's start: a pass (and the accounts before it in the Tick)
		// can take a while, and that time would count as the server's lead.
		read := p.o.Now()
		skew := serverSkew(page.ServerTime, read)
		cutoff := historyCutoff(a, skew, read)
		for _, row := range page.Rows {
			if !p.row(ctx, a, titles, row, cutoff, skew, now) {
				unrecorded = true
			}
		}
		if page.Next != "" {
			cursor = page.Next
		}
		if !page.More || page.Next == "" {
			end = true
			break
		}
	}
	if took {
		// Only a cursor the server gave and then took ends the run of
		// refusals: a read from the start that fits on a page sends none, so
		// counting it would end the run every other pass and the wait would
		// never come.
		p.strike(p.resets, a.ID, false)
	}
	// A row that wasn't recorded keeps the cursor where it was: the next pass
	// reads the pages again (the rows recorded read again at the same place
	// change nothing), never past a place it would then never push.
	if !unrecorded {
		var from time.Time // the baseline is over once a read reached the end
		if end {
			from = now
		}
		if err := p.o.DB.SetAudiblePushCursor(ctx, a.ID, cursor, from); err != nil {
			p.o.Log.Warn("audible: recording the push's cursor failed", "account", a.ID, "err", err)
		}
	}
	// A pass that read the feed and wrote what was due without a failure ends
	// the push's last error (the feed back after a server's outage, the
	// person syncable again, a dropped title's): with nothing to write, no
	// push would clear it, and the job's success no longer does (SyncPush).
	if p.due(ctx, a, now) && a.PushError != "" {
		p.o.health(ctx, store.SyncPush, a, nil, time.Time{})
	}
}

// cursorRefused sends a's push back to the feed's start after the server
// refused its cursor (err): the rows recorded already read at the same place
// change nothing, and the ones changed before the account's baseline, or
// before the place last read for their title, are history again
// (historyCutoff, olderThanKnown), whatever place they have now. It leaves the account for resetWait once that
// happened resetStrikes times with no cursor taken between: the first refusal
// and the one that starts the waits are logged, the ones between and after
// are not. Each refusal from then on records the push's failure again (the
// clean read from the start between two ends it).
func (p *Pusher) cursorRefused(ctx context.Context, a store.AudibleAccount, err error, now time.Time) {
	n := p.strike(p.resets, a.ID, true)
	switch n {
	case 1:
		p.o.Log.Warn("audible: the server refused the push's cursor, reading its places from the start", "account", a.ID, "err", err)
	case resetStrikes:
		p.o.Log.Warn("audible: the server keeps refusing the push's cursor, even the ones it gave; reading its places "+
			"again later", "account", a.ID, "refusals", n, "wait", resetWait, "err", err)
	}
	if n >= resetStrikes {
		p.pause(a.ID, now.Add(resetWait))
		p.o.health(ctx, store.SyncPush, a, fmt.Errorf("the server keeps refusing the cursor of its places feed: %w", err), time.Time{})
	}
	if err := p.o.DB.ResetAudiblePushCursor(ctx, a.ID); err != nil {
		p.o.Log.Warn("audible: resetting the push's cursor failed", "account", a.ID, "err", err)
	}
}

// serverSkew is the server's clock less the Bridge's (now) by the page's
// server_time, 0 from a server before 2.7.0 (none).
func serverSkew(serverTime string, now time.Time) time.Duration {
	at, err := time.Parse(time.RFC3339Nano, serverTime)
	if err != nil {
		return 0
	}
	return at.Sub(now)
}

// olderThanKnown reports whether a row read again from the start after a
// refused cursor holds a place from no later than the last place this Bridge
// read for the title x (row_seen): a server restored from a backup answers the
// row as it was before then, changed after the baseline perhaps but older than
// the place already handled, and written again it would move Audible back.
// Never by when the place was written to Audible (at): that is after the read,
// and a place another device wrote between the two was never read, so it is
// new. skew moves the Bridge's clock onto the server's (serverSkew). A title
// never read counts as never handled (the baseline's line alone).
func olderThanKnown(updated time.Time, x store.AudiblePush, skew time.Duration) bool {
	seen, err := time.Parse(time.RFC3339Nano, x.RowSeen)
	return err == nil && !updated.After(seen.Add(skew))
}

// historyCutoff is when a row's place counts as history: while the baseline
// reads the feed (push_from unset, perhaps over several passes) a row changed
// before it is history, after it none is (zero). The rows' updated_at are the
// server's clock, so the window is counted back from the page's server_time
// (bridge#50: a Bridge clock minutes off misjudged recent places), else, from a
// server before 2.7.0, from the Bridge's own clock.
//
// A read from the start after a refused cursor (push_reread, perhaps over
// several passes too) is the baseline's again, at the line the baseline drew:
// the window before push_from. The feed then gives every row the account has,
// and one that differs from the place recorded here is not thereby new: a
// server restored from a backup answers older places than the ones this Bridge
// read and wrote to Audible, and written again they would move Audible back.
// A row changed since the baseline is still a new place. (push_from is the
// Bridge's clock, moved onto the server's by skew, serverSkew; one that can't
// be read draws the line again, as a baseline.)
func historyCutoff(a store.AudibleAccount, skew time.Duration, now time.Time) time.Time {
	if a.PushFrom != "" {
		if !a.PushReread {
			return time.Time{}
		}
		if from, err := time.Parse(time.RFC3339Nano, a.PushFrom); err == nil {
			return from.Add(skew - baselineWindow)
		}
	}
	return now.Add(skew - baselineWindow)
}

// row records one server place: a place on a listed title's book that moved
// since the row last read becomes the title's waiting place, first read now
// (the Bridge's clock: the stillness is counted from it). The feed pages in
// the order the server wrote rows, and may give a row again with its
// updated_at unchanged (an edit, a diverged place applied), so a row is new by
// its place, never its time: one read again at the same place (whatever else
// changed: its version, finish, length or device) is no new place and doesn't
// restart the wait. Audible's own rows (the server leaves them out too) are left,
// and history (changed before cutoff, the baseline's, and on a read from the
// start after a refused cursor, a place no later than the one last read for
// its title, olderThanKnown by skew) is recorded with no waiting place. A row on a title the last sync didn't list for that book (the
// server's feed already syncs it: a new match, a re-match, a title synced
// again) is kept pending until the next sync lists it, never skipped past.
// titles is account a's rows by ASIN, kept up to date with what row records.
// ok is false when recording the row failed.
func (p *Pusher) row(ctx context.Context, a store.AudibleAccount, titles map[string]store.AudiblePush, row link.AudibleProgressRow,
	cutoff time.Time, skew time.Duration, now time.Time) (ok bool) {
	if row.DeviceID == DeviceID(a.ID) {
		return true
	}
	place := ms(row.Position)
	x, known := titles[row.ASIN]
	sameBook := known && x.LibraryID == row.LibraryID && x.Path == row.Path
	if sameBook && x.RowMs == place {
		return true
	}
	history := false
	updated, bad := time.Parse(time.RFC3339Nano, row.UpdatedAt)
	if !cutoff.IsZero() {
		history = bad != nil || updated.Before(cutoff)
	}
	if !history && a.PushReread && sameBook {
		history = bad != nil || olderThanKnown(updated, x, skew)
	}
	rec := store.AudibleRow{Ms: place, Seen: now}
	var err error
	switch {
	case !sameBook || !x.Listed:
		err = p.o.DB.PendAudiblePush(ctx, a.ID, row.ASIN, row.LibraryID, row.Path, !history, rec)
		reasserted := x.ReassertedMs
		if !sameBook {
			reasserted = -1
		}
		x = store.AudiblePush{Account: a.ID, ASIN: row.ASIN, LibraryID: row.LibraryID, Path: row.Path, LastMs: x.LastMs, HeardMs: x.HeardMs,
			ReassertedMs: reasserted}
		if !known {
			x.LastMs, x.HeardMs = -1, -1
		}
	case history:
		err = p.o.DB.SeenAudibleRow(ctx, a.ID, row.ASIN, rec)
	default:
		err = p.o.DB.WantAudiblePush(ctx, a.ID, row.ASIN, rec)
	}
	if err != nil {
		p.o.Log.Warn("audible: recording a server place failed", "account", a.ID, "err", err)
		return false
	}
	x.RowMs, x.RowSeen = rec.Ms, store.Timestamp(now)
	titles[row.ASIN] = x
	return true
}

// due writes each waiting place still for StillFor (since this Bridge first
// read it, by its own clock: never the row's updated_at), after
// one read of Audible's places for them: a place Audible moved further on
// since the Bridge last wrote or heard it (listening in the Audible app that
// no sync has brought in yet, or during a pause) isn't overwritten. Furthest
// wins, as on the server: the waiting place goes, and the next sync sends
// Audible's. clean: nothing failed (nothing due counts), so the push's last
// error no longer holds.
func (p *Pusher) due(ctx context.Context, a store.AudibleAccount, now time.Time) (clean bool) {
	list, err := p.o.DB.AudibleWaiting(ctx, a.ID)
	if err != nil {
		p.o.Log.Warn("audible: listing the places to push failed", "account", a.ID, "err", err)
		return false
	}
	// A remembered pending preorder is never pushed (Audible denies its
	// content reference: a false sync error every pass). Its place waits
	// until the next sync, which leaves it unlisted and forgets the place.
	// Read only once a place is still long enough to be written.
	var pre map[string]bool
	var ready []store.AudiblePush
	for _, x := range list {
		seen, err := time.Parse(time.RFC3339Nano, x.RowSeen)
		if err != nil || now.Sub(seen) < StillFor {
			continue
		}
		if pre == nil {
			if pre = p.o.preorders(ctx, a.ID); pre == nil {
				pre = map[string]bool{}
			}
		}
		if pre[x.ASIN] || p.dropped(ctx, a, x) {
			continue
		}
		ready = append(ready, x)
	}
	if ctx.Err() != nil {
		return false
	}
	if len(ready) == 0 {
		return true
	}
	asins := make([]string, len(ready))
	for i, x := range ready {
		asins[i] = x.ASIN
	}
	places, err := p.o.Accounts.Positions(ctx, a.ID, asins)
	if err != nil {
		if !titleAnswer(err) {
			p.failed(ctx, a, nil, "reading the places on Audible", err)
			return false
		}
		// Audible refused the read itself (a 4xx): the places are written as
		// they were before this check, as when Audible has none, but for the
		// ones it answered before the refusal (a later batch's), whose
		// further-on places on Audible still win (bridge#50).
		p.o.Log.Warn("audible: reading the places on Audible before writing failed", "account", a.ID, "err", audible.RedactedError(err))
	}
	clean = true
	for _, x := range ready {
		if ctx.Err() != nil {
			return false
		}
		if pl, ok := places[x.ASIN]; ok && movedOn(pl.PositionMs, x) {
			p.o.Log.Info("audible: the place on Audible is further on and new; left for the sync, not overwritten",
				"account", a.ID, "asin", x.ASIN)
			p.drop(ctx, a, x)
			continue
		}
		stop, ok := p.push(ctx, a, x)
		if stop {
			return false
		}
		clean = clean && ok
	}
	return clean
}

// movedOn reports whether Audible's place (ms) is further on than x's waiting
// place and is neither the place last written nor the one last heard: Audible
// moved on since the Bridge last knew its place.
func movedOn(audibleMs int64, x store.AudiblePush) bool {
	return audibleMs > *x.WantMs+echoMs && !near(audibleMs, x.LastMs) && !near(audibleMs, x.HeardMs)
}

// dropped drops x's waiting place, and reports so, when it is never written: a
// place at the start, one within echoMs of the last one written (the debounce)
// or of the last one heard from Audible (Audible's own place coming back), or
// one past the title's runtime (a wrong match: never written to the real
// account). A book whose length disagrees with the title's runtime never
// reaches the push: the server's feed leaves it out (duration_mismatch).
func (p *Pusher) dropped(ctx context.Context, a store.AudibleAccount, x store.AudiblePush) bool {
	want := *x.WantMs
	switch {
	case want < 1000, near(want, x.LastMs), near(want, x.HeardMs):
	case pastRuntime(float64(want)/1000, x.RuntimeS):
		p.o.Log.Warn("audible: a place on the server is past the title's runtime on Audible, so its match looks wrong; not written",
			"account", a.ID, "asin", x.ASIN)
	default:
		return false
	}
	p.drop(ctx, a, x)
	return true
}

func (p *Pusher) drop(ctx context.Context, a store.AudibleAccount, x store.AudiblePush) {
	if err := p.o.DB.DropAudiblePush(ctx, a.ID, x.ASIN); err != nil {
		p.o.Log.Warn("audible: dropping a place failed", "account", a.ID, "err", err)
	}
}

// push writes x's waiting place to Audible: the content reference (cached; a
// cached one Audible no longer takes is read again once), then the place.
// stop: Audible asked to wait or didn't answer, so the account's other places
// wait too; ok: the place was written.
func (p *Pusher) push(ctx context.Context, a store.AudibleAccount, x store.AudiblePush) (stop, ok bool) {
	want := *x.WantMs
	acr, cached := x.ACR, x.ACR != ""
	if !cached {
		if acr, stop, ok = p.contentACR(ctx, a, x); !ok {
			return stop, false
		}
	}
	err := p.o.Accounts.SetPosition(ctx, a.ID, x.ASIN, acr, want)
	if s := audible.HTTPStatus(err); cached && (s == http.StatusBadRequest || s == http.StatusNotFound) {
		// A cached content reference Audible no longer takes: read it again
		// and write once more.
		_ = p.o.DB.SetAudibleACR(ctx, a.ID, x.ASIN, "")
		if acr, stop, ok = p.contentACR(ctx, a, x); !ok {
			return stop, false
		}
		err = p.o.Accounts.SetPosition(ctx, a.ID, x.ASIN, acr, want)
	}
	if err != nil {
		return p.failed(ctx, a, &x, "writing the place to Audible", err), false
	}
	if err := p.o.DB.AudiblePushed(ctx, a.ID, x.ASIN, want, p.o.Now()); err != nil {
		p.o.Log.Warn("audible: recording a push failed", "account", a.ID, "err", err)
	}
	p.o.Log.Info("audible: place written to Audible", "account", a.ID, "asin", x.ASIN, "position_ms", want)
	p.o.health(ctx, store.SyncPush, a, nil, time.Time{})
	return false, true
}

// contentACR reads and caches x's content reference; !ok: it failed (stop as
// failed says).
func (p *Pusher) contentACR(ctx context.Context, a store.AudibleAccount, x store.AudiblePush) (acr string, stop, ok bool) {
	acr, err := p.o.Accounts.ContentACR(ctx, a.ID, x.ASIN)
	if err != nil {
		return "", p.failed(ctx, a, &x, "reading the title's content reference on Audible", err), false
	}
	if err := p.o.DB.SetAudibleACR(ctx, a.ID, x.ASIN, acr); err != nil {
		p.o.Log.Warn("audible: caching a content reference failed", "account", a.ID, "err", err)
	}
	return acr, false, true
}

// titleAnswer reports whether err is Audible's answer about the call itself:
// an HTTP refusal that isn't a wait (a 4xx), or a license it denies (a 200
// with no content reference).
func titleAnswer(err error) bool {
	s := audible.HTTPStatus(err)
	return (s >= 400 && s < 500 && s != http.StatusTooManyRequests) || audible.IsDenial(err)
}

// failed records a failed call to Audible for x's place (nil: the read of the
// account's places). Audible asking to wait (429/503) keeps the place and stops
// the account until its Retry-After (at least errorWait), counted from when the
// answer came back. Audible failing (another 5xx) or no answer at all keeps it
// too and leaves the account's places for errorWait (this loop only: no wait
// for the sync). A title Audible won't take a place for (another HTTP answer, a
// license it denies) drops the place, so it isn't tried every minute; a device
// it denies every license leaves the account's other places for refusedWait.
// Never a refusal.
func (p *Pusher) failed(ctx context.Context, a store.AudibleAccount, x *store.AudiblePush, what string, err error) (stop bool) {
	if ctx.Err() != nil {
		return true
	}
	now := p.o.Now()
	asin := ""
	if x != nil {
		asin = x.ASIN
	}
	p.o.Log.Warn("audible: "+what+" failed", "account", a.ID, "asin", asin, "err", audible.RedactedError(err))
	if _, ok := audible.RetryWait(err); ok {
		p.o.health(ctx, store.SyncPush, a, fmt.Errorf("%s: Audible asked the Bridge to wait", what), retryAt(err, now, errorWait))
		return true
	}
	p.o.health(ctx, store.SyncPush, a, fmt.Errorf("%s: %w", what, err), time.Time{})
	if !titleAnswer(err) {
		p.pause(a.ID, now.Add(errorWait))
		return true
	}
	if x != nil {
		p.drop(ctx, a, *x)
	}
	if errors.Is(err, audible.ErrThrottled) {
		p.pause(a.ID, now.Add(refusedWait))
		return true
	}
	return false
}
