package audiblesync

import (
	"context"
	"errors"
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
	// errorWait is the least wait after a failure (the server's or Audible's).
	errorWait = 2 * time.Minute
	// baselineWindow: the first read of an account's feed to its end (the
	// baseline, push_from still unset) takes the places changed this recently
	// (by the row's updated_at) as waiting to be pushed; older ones are
	// history and stay where they are. After it, every row the feed gives is
	// new: it pages in the order the server wrote them, not by updated_at.
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
}

// NewPusher returns the push loop.
func NewPusher(o Options) *Pusher {
	o.defaults()
	return &Pusher{o: o, paused: map[string]time.Time{}}
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

// account reads a's feed from its cursor, records the places waiting, then
// writes the ones due.
func (p *Pusher) account(ctx context.Context, srv Server, a store.AudibleAccount, now time.Time) {
	// cutoff: while the baseline reads the feed (push_from unset, perhaps over
	// several passes), a row changed before it is history; after, none is.
	var cutoff time.Time
	if a.PushFrom == "" {
		cutoff = now.Add(-baselineWindow)
	}
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
	for range maxPages {
		page, err := srv.AudibleProgress(ctx, a.ID, cursor, link.MaxAudibleProgress)
		switch {
		case errors.Is(err, link.ErrSyncOff):
			p.pause(a.ID, now.Add(offWait))
			p.o.syncOff(ctx, a)
			return
		case errors.Is(err, link.ErrUserUnavailable), errors.Is(err, link.ErrUnknownAccount):
			p.pause(a.ID, now.Add(refusedWait))
			p.o.health(ctx, a, err, time.Time{})
			return
		case err != nil:
			if ctx.Err() == nil {
				p.o.Log.Warn("audible: reading the server's places failed", "account", a.ID, "err", err)
				p.o.health(ctx, a, errors.New("reading the server's places failed: "+err.Error()), time.Time{})
				// The server asking to wait pauses this loop only: it is no
				// reason to leave Audible alone (the sync's wait).
				var e *link.APIError
				if errors.As(err, &e) && (e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable) {
					p.pause(a.ID, p.o.Now().Add(max(e.RetryAfter, errorWait)))
				}
			}
			return
		}
		for _, row := range page.Rows {
			if !p.row(ctx, a, titles, row, cutoff, now) {
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
	p.due(ctx, a, now)
}

// row records one server place: a place on a listed title's book that moved
// since the row last read becomes the title's waiting place, first read now
// (the Bridge's clock: the stillness is counted from it). The feed pages in
// the order the server wrote rows, and may give a row again with its
// updated_at unchanged (an edit, a diverged place applied), so a row is new by
// its place, never its time: one read again at the same place (whatever else
// changed: its version, finish, length or device) is no new place and doesn't
// restart the wait. Audible's own rows (the server leaves them out too) are left,
// and history (changed before cutoff, the baseline's only) is recorded with no
// waiting place. A row on a title the last sync didn't list for that book (the
// server's feed already syncs it: a new match, a re-match, a title synced
// again) is kept pending until the next sync lists it, never skipped past.
// titles is account a's rows by ASIN, kept up to date with what row records.
// ok is false when recording the row failed.
func (p *Pusher) row(ctx context.Context, a store.AudibleAccount, titles map[string]store.AudiblePush, row link.AudibleProgressRow,
	cutoff, now time.Time) (ok bool) {
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
	if !cutoff.IsZero() {
		updated, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
		history = err != nil || updated.Before(cutoff)
	}
	rec := store.AudibleRow{Ms: place, Seen: now}
	var err error
	switch {
	case !sameBook || !x.Listed:
		err = p.o.DB.PendAudiblePush(ctx, a.ID, row.ASIN, row.LibraryID, row.Path, !history, rec)
		x = store.AudiblePush{Account: a.ID, ASIN: row.ASIN, LibraryID: row.LibraryID, Path: row.Path, LastMs: x.LastMs, HeardMs: x.HeardMs}
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
// Audible's.
func (p *Pusher) due(ctx context.Context, a store.AudibleAccount, now time.Time) {
	list, err := p.o.DB.AudibleWaiting(ctx, a.ID)
	if err != nil {
		p.o.Log.Warn("audible: listing the places to push failed", "account", a.ID, "err", err)
		return
	}
	// A remembered pending preorder is never pushed (Audible denies its
	// content reference: a false sync error every pass). Its place waits.
	var pre map[string]bool
	if len(list) > 0 {
		pre = p.o.preorders(ctx, a.ID)
	}
	var ready []store.AudiblePush
	for _, x := range list {
		seen, err := time.Parse(time.RFC3339Nano, x.RowSeen)
		if err != nil || now.Sub(seen) < StillFor || pre[x.ASIN] || p.dropped(ctx, a, x) {
			continue
		}
		ready = append(ready, x)
	}
	if len(ready) == 0 || ctx.Err() != nil {
		return
	}
	asins := make([]string, len(ready))
	for i, x := range ready {
		asins[i] = x.ASIN
	}
	places, err := p.o.Accounts.Positions(ctx, a.ID, asins)
	if err != nil {
		if !titleAnswer(err) {
			p.failed(ctx, a, nil, "reading the places on Audible", err)
			return
		}
		// Audible refused the read itself (a 4xx): the places are written as
		// they were before this check, as when Audible has none.
		p.o.Log.Warn("audible: reading the places on Audible before writing failed", "account", a.ID, "err", audible.RedactedError(err))
		places = nil
	}
	for _, x := range ready {
		if ctx.Err() != nil {
			return
		}
		if pl, ok := places[x.ASIN]; ok && movedOn(pl.PositionMs, x) {
			p.o.Log.Info("audible: the place on Audible is further on and new; left for the sync, not overwritten",
				"account", a.ID, "asin", x.ASIN)
			p.drop(ctx, a, x)
			continue
		}
		if stop := p.push(ctx, a, x); stop {
			return
		}
	}
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
// wait too.
func (p *Pusher) push(ctx context.Context, a store.AudibleAccount, x store.AudiblePush) (stop bool) {
	want := *x.WantMs
	acr, cached := x.ACR, x.ACR != ""
	if !cached {
		var stop, ok bool
		if acr, stop, ok = p.contentACR(ctx, a, x); !ok {
			return stop
		}
	}
	err := p.o.Accounts.SetPosition(ctx, a.ID, x.ASIN, acr, want)
	if s := audible.HTTPStatus(err); cached && (s == http.StatusBadRequest || s == http.StatusNotFound) {
		// A cached content reference Audible no longer takes: read it again
		// and write once more.
		_ = p.o.DB.SetAudibleACR(ctx, a.ID, x.ASIN, "")
		var stop, ok bool
		if acr, stop, ok = p.contentACR(ctx, a, x); !ok {
			return stop
		}
		err = p.o.Accounts.SetPosition(ctx, a.ID, x.ASIN, acr, want)
	}
	if err != nil {
		return p.failed(ctx, a, &x, "writing the place to Audible", err)
	}
	if err := p.o.DB.AudiblePushed(ctx, a.ID, x.ASIN, want, p.o.Now()); err != nil {
		p.o.Log.Warn("audible: recording a push failed", "account", a.ID, "err", err)
	}
	p.o.Log.Info("audible: place written to Audible", "account", a.ID, "asin", x.ASIN, "position_ms", want)
	p.o.health(ctx, a, nil, time.Time{})
	return false
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
		p.o.health(ctx, a, errors.New(what+": Audible asked the Bridge to wait"), retryAt(err, now, errorWait))
		return true
	}
	p.o.health(ctx, a, errors.New(what+": "+err.Error()), time.Time{})
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
