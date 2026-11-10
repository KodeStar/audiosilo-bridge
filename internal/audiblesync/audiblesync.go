// Package audiblesync keeps an Audible account's places in step with the
// server, both ways (BRIDGE-PLAN.md Phase 4c):
//
//   - audible.sync (a light job the server queues every 15 minutes per account
//     with sync on, its params the titles to sync): Audible's last-heard
//     places (lastpositions, 25 ASINs a request, split on a 400 or 414) become
//     Sync v2 events for the account's person, a listen of no length at the
//     place (a finish too at the book's end), posted furthest only: the server
//     never moves a place back for one.
//   - the push loop (push.go, not a job: no row a minute): every minute it
//     reads the person's places that changed (GET /bridge/progress, the
//     account's own rows left out) and writes back to Audible
//     (SetLastPosition, the content reference cached per ASIN) a place that
//     has been still for 3 minutes, is more than 5 seconds from the last one
//     written, and that Audible hasn't moved further on from since: furthest
//     wins both ways.
//
// Neither touches a title the server didn't list (Not this book, Don't sync
// this book, no match), nor a place past Audible's runtime (the match looks
// wrong), nor a refused account (it must sign in again first). A sync error
// never refuses the account: it backs off (honouring Audible's Retry-After)
// and shows in the setup item's detail. No credential, title or listening
// time leaves here; params, results and errors carry ids, ASINs, paths and
// counts only.
package audiblesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audibleacct"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Kind is the job kind.
const Kind = link.KindAudibleSync

// The failure codes a sync reports ({code, message} as its result and error).
// The server reads CodeRefused: it marks the account refused and queues no
// sync until the Bridge maps it again, so it is a device refusal's alone (only
// a sign-in again ends one, and that maps it again). A sign-in refusal ends by
// itself once Audible answers a call (the next poll): CodeSignIn, which the
// server leaves alone, so its syncs are queued as usual.
const (
	CodeRefused         = "audible_refused"  // Audible denied the account's device: it must sign in again on the Bridge
	CodeSignIn          = "audible_sign_in"  // Audible refused the sign-in for now
	CodeUnknownAccount  = "unknown_account"  // not this Bridge's account, or mapped on another server
	CodeUserUnavailable = "user_unavailable" // the server can't sync for the account's person now
	CodeAudible         = "audible_error"    // Audible didn't answer the places (backed off)
)

// Bounds and tolerances.
const (
	// finishPct and endRemainingS say a place is at the book's end (the
	// manager's atBookEnd): 98% in, or within 3 minutes of the end and 90% in.
	finishPct     = 0.98
	endRemainingS = 180.0
	// runtimeSlackPct and runtimeSlackS say a place is past the runtime (the
	// manager's serverPastRuntime): more than 5% plus 2 minutes past it
	// (Audible's runtime is minute-rounded), a sign of a wrong match.
	runtimeSlackPct = 1.05
	runtimeSlackS   = 120.0
	// maxEventAge: a place Audible heard longer ago than this isn't sent (the
	// server rejects events older than 90 days).
	maxEventAge = 85 * 24 * time.Hour
	// echoMs: a place within this of the one last written to Audible (or, the
	// push's way, of the one last heard from it) is the other side's own place
	// coming back. It is also the push's debounce: a place this close to the
	// last one written isn't written again.
	echoMs = 5000
	// maxTitles bounds a job's titles (the server's params are 1 MiB).
	maxTitles = 20000
)

// pastRuntime reports whether a place (seconds) is past a book's runtime by
// more than the slack; false when the runtime is unknown.
func pastRuntime(placeS float64, runtimeS int64) bool {
	return runtimeS > 0 && placeS > float64(runtimeS)*runtimeSlackPct+runtimeSlackS
}

// atBookEnd reports whether a place (seconds) is at the end of a book of
// runtimeS seconds; false when the runtime is unknown.
func atBookEnd(placeS float64, runtimeS int64) bool {
	if runtimeS <= 0 {
		return false
	}
	rt := float64(runtimeS)
	return placeS >= rt*finishPct || (rt-placeS <= endRemainingS && placeS >= rt*0.9)
}

// EventID is a place's event id: "aud-" and the first 32 hex characters of
// sha256(account|asin|position_ms|last_updated|kind|library_id|path), so an
// unchanged Audible place is the same event (duplicate on the server) at every
// sync, and the same place on another book (the title matched again) is a new
// one: the server keeps ids per person, not per book. kind "" is the listen,
// "finish" the finish; the path, the one free-form part, comes last.
func EventID(account string, t link.AudibleSyncTitle, pl audible.Position, kind string) string {
	updated := ""
	if !pl.LastUpdated.IsZero() {
		updated = pl.LastUpdated.UTC().Format(time.RFC3339Nano)
	}
	in := account + "|" + t.ASIN + "|" + strconv.FormatInt(pl.PositionMs, 10) + "|" + updated + "|" + kind + "|" +
		strconv.FormatInt(t.LibraryID, 10) + "|" + t.Path
	sum := sha256.Sum256([]byte(in))
	return "aud-" + hex.EncodeToString(sum[:])[:32]
}

// DeviceID is the server's device id for account's places.
func DeviceID(account string) string { return "audible:" + account }

// Accounts is the part of the account manager the sync uses
// (*audibleacct.Manager).
type Accounts interface {
	Account(ctx context.Context, id string) (store.AudibleAccount, error)
	Accounts(ctx context.Context) ([]store.AudibleAccount, error)
	Positions(ctx context.Context, id string, asins []string) (map[string]audible.Position, error)
	ContentACR(ctx context.Context, id, asin string) (string, error)
	SetPosition(ctx context.Context, id, asin, acr string, positionMs int64) error
}

// Store is the part of the store the sync uses (*store.DB).
type Store interface {
	ListAudibleSync(ctx context.Context, account string, titles []store.AudibleSyncTitle) error
	AudiblePushes(ctx context.Context, account string) ([]store.AudiblePush, error)
	AudibleWaiting(ctx context.Context, account string) ([]store.AudiblePush, error)
	WantAudiblePush(ctx context.Context, account, asin string, row store.AudibleRow) error
	SeenAudibleRow(ctx context.Context, account, asin string, row store.AudibleRow) error
	PendAudiblePush(ctx context.Context, account, asin string, libraryID int64, path string, want bool, row store.AudibleRow) error
	AudiblePushed(ctx context.Context, account, asin string, ms int64, at time.Time) error
	DropAudiblePush(ctx context.Context, account, asin string) error
	AudibleHeard(ctx context.Context, x store.AudiblePush, ms int64, applied bool, since time.Time) error
	RearmAudiblePush(ctx context.Context, x store.AudiblePush, now time.Time) (bool, error)
	SetAudibleACR(ctx context.Context, account, asin, acr string) error
	SetAudiblePushCursor(ctx context.Context, id, cursor string, from time.Time) error
	AudibleSyncOK(ctx context.Context, id string, at time.Time) error
	AudibleSyncFailed(ctx context.Context, id, why string, retryAt time.Time) error
	ClearAudibleSyncError(ctx context.Context, id string) (bool, error)
	// AudiblePreorders are the pending preorders a poll remembers (migration
	// 0012): neither direction syncs one (it isn't out, so Audible has no
	// place for it and denies the content reference a push needs).
	AudiblePreorders(ctx context.Context, account string) ([]store.AudiblePreorder, error)
}

// Server is the part of the server link the sync calls (*link.Client).
type Server interface {
	PostAudibleListening(ctx context.Context, account string, in link.AudibleListening) (*link.SyncAnswer, error)
	AudibleProgress(ctx context.Context, account, since string, limit int) (*link.AudibleProgress, error)
}

// Options wire the job and the push loop.
type Options struct {
	Accounts Accounts
	DB       Store
	// Server is the link (nil while unpaired); ServerID the pairing's server.
	Server   func() Server
	ServerID func() string
	Log      *slog.Logger
	// OnChange runs when an account's sync health changes (the heartbeat's
	// checklist moves).
	OnChange func()
	// Now is the clock (tests replace it).
	Now func() time.Time
}

func (o *Options) defaults() {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.ServerID == nil {
		o.ServerID = func() string { return "" }
	}
	if o.OnChange == nil {
		o.OnChange = func() {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// preorders are account's remembered pending preorders by ASIN (none, with a
// warning, when the store can't say: the sync then runs as it did before).
func (o *Options) preorders(ctx context.Context, account string) map[string]bool {
	rows, err := o.DB.AudiblePreorders(ctx, account)
	if err != nil {
		o.Log.Warn("audible: reading the remembered preorders failed", "account", account, "err", err)
		return nil
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.ASIN] = true
	}
	return out
}

func (o *Options) server() Server {
	if o.Server == nil {
		return nil
	}
	return o.Server()
}

// health records how account a's sync went (err nil: it went through),
// honouring a wait Audible asked for, and runs OnChange when that changes
// what the checklist says (an error appears or clears). It never refuses the
// account.
func (o *Options) health(ctx context.Context, a store.AudibleAccount, err error, retryAt time.Time) {
	ctx = context.WithoutCancel(ctx)
	before := a.SyncError
	if cur, cerr := o.Accounts.Account(ctx, a.ID); cerr == nil {
		before = cur.SyncError
	}
	if err == nil {
		if rerr := o.DB.AudibleSyncOK(ctx, a.ID, o.Now()); rerr != nil {
			o.Log.Warn("audible: recording a sync failed", "account", a.ID, "err", rerr)
		}
		if before != "" {
			o.OnChange()
		}
		return
	}
	why := audible.RedactedError(err).Error()
	if rerr := o.DB.AudibleSyncFailed(ctx, a.ID, why, retryAt); rerr != nil {
		o.Log.Warn("audible: recording a sync failure failed", "account", a.ID, "err", rerr)
	}
	if before == "" {
		o.OnChange()
	}
}

// syncOff clears a sync error that no longer applies, without recording a
// sync success (a wait Audible asked for stays), and runs OnChange when there
// was one to clear.
func (o *Options) syncOff(ctx context.Context, a store.AudibleAccount) {
	cleared, err := o.DB.ClearAudibleSyncError(context.WithoutCancel(ctx), a.ID)
	if err != nil {
		o.Log.Warn("audible: clearing sync health failed", "account", a.ID, "err", err)
		return
	}
	if cleared {
		o.OnChange()
	}
}

// retryAt is when the account may call Audible again after err, with now the
// moment the failure came back (after the client's own waits): Audible's
// Retry-After (at least minWait) for a 429/503, else zero (no wait). Only
// Audible's: the server asking the Bridge to wait is no reason to leave
// Audible alone.
func retryAt(err error, now time.Time, minWait time.Duration) time.Time {
	wait, ok := audible.RetryWait(err)
	if !ok {
		return time.Time{}
	}
	return now.Add(max(wait, minWait))
}

// waiting reports whether a asked to wait until after now.
func waiting(a store.AudibleAccount, now time.Time) (time.Time, bool) {
	if a.SyncRetryAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, a.SyncRetryAt)
	return t, err == nil && now.Before(t)
}

// Failure is a failed sync's result and error: a code and a sentence for the
// console, never a credential or an Amazon account identifier.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (f *Failure) Error() string { return f.Message }

func fail(code, format string, args ...any) *Failure {
	for i, a := range args {
		if err, ok := a.(error); ok {
			args[i] = audible.RedactedError(err)
		}
	}
	return &Failure{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Handler runs audible.sync.
type Handler struct{ o Options }

// New returns the job handler.
func New(o Options) *Handler {
	o.defaults()
	return &Handler{o: o}
}

// Kind is audible.sync.
func (h *Handler) Kind() string { return Kind }

// Lane is the light lane: a sync is a few API calls.
func (h *Handler) Lane() string { return jobs.LaneLight }

// ParseParams reads the params strictly (an unknown field is refused, as the
// server checks them): an account id and titles each with an ASIN, a library
// and a book path, once each ASIN.
func ParseParams(raw json.RawMessage) (link.AudibleSyncParams, error) {
	var p link.AudibleSyncParams
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return link.AudibleSyncParams{}, fmt.Errorf("bad params: %w", err)
	}
	if !audibleacct.IDPattern.MatchString(p.Account) {
		return link.AudibleSyncParams{}, errors.New("bad params: account must be an Audible account id")
	}
	if len(p.Titles) > maxTitles {
		return link.AudibleSyncParams{}, errors.New("bad params: too many titles")
	}
	seen := make(map[string]bool, len(p.Titles))
	for _, t := range p.Titles {
		switch {
		case !audibleacct.ASINPattern.MatchString(t.ASIN) || seen[t.ASIN]:
			return link.AudibleSyncParams{}, errors.New("bad params: each title's asin must be an ASIN, once")
		case t.LibraryID <= 0 || t.Path == "" || t.RuntimeS < 0:
			return link.AudibleSyncParams{}, errors.New("bad params: each title needs a library, a book path and a runtime of 0 or more")
		}
		seen[t.ASIN] = true
	}
	return p, nil
}

// Result is what a sync reports: counts, never a title or a credential.
type Result struct {
	Account string `json:"account"`
	Titles  int    `json:"titles"`
	// Heard: titles Audible has a place for. Sent: the places posted (a
	// finish counts beside its listen in Finished).
	Heard    int `json:"heard"`
	Sent     int `json:"sent"`
	Finished int `json:"finished"`
	// The server's answers: applied (the server's place moved), duplicate (the
	// same Audible place as before), behind (not ahead of the server's),
	// not_applied for another reason, rejected.
	Applied    int `json:"applied"`
	Duplicate  int `json:"duplicate"`
	Behind     int `json:"behind"`
	NotApplied int `json:"not_applied"`
	Rejected   int `json:"rejected"`
	// Mismatched: of the rejected, those on a book whose length disagrees
	// with the title's runtime (duration_mismatch: another edition matched).
	// Not sent again within the job; the next sync sends them again, so a
	// corrected match syncs.
	Mismatched int `json:"mismatched,omitempty"`
	// Reasserted: of the behind, places the Audible app uploaded over the one
	// the Bridge wrote while the server still holds that one: the server's
	// place waits to be written to Audible again (the push).
	Reasserted int `json:"reasserted,omitempty"`
	// Skipped places: Audible's echo of a place the Bridge wrote, a place past
	// the runtime, one too old for the server, one at the start.
	SkippedEcho        int `json:"skipped_echo,omitempty"`
	SkippedPastRuntime int `json:"skipped_past_runtime,omitempty"`
	SkippedOld         int `json:"skipped_old,omitempty"`
	SkippedStart       int `json:"skipped_start,omitempty"`
	// SkippedPreorder: titles the server listed that the Bridge remembers as
	// pending preorders (not synced until they are out).
	SkippedPreorder int `json:"skipped_preorder,omitempty"`
	// Waiting: Audible asked the Bridge to wait (until WaitUntil), so the sync
	// didn't call it; SyncOff: the server's sync was off by the time it ran.
	Waiting   bool   `json:"waiting,omitempty"`
	WaitUntil string `json:"wait_until,omitempty"`
	SyncOff   bool   `json:"sync_off,omitempty"`
}

// Run syncs the account's places on Audible to the server.
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

func (h *Handler) run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (*Result, error) {
	p, err := ParseParams(job.Params)
	if err != nil {
		return nil, err
	}
	acct, err := h.o.Accounts.Account(ctx, p.Account)
	if errors.Is(err, audibleacct.ErrNotFound) {
		return nil, fail(CodeUnknownAccount, "This Bridge has no Audible account %s: it was removed here, or signed in on another Bridge.", p.Account)
	}
	if err != nil {
		return nil, err
	}
	if acct.OnOtherServer(h.o.ServerID()) {
		return nil, fail(CodeUnknownAccount, "The Audible account is mapped on another server: map it again on the Bridge's Accounts tab.")
	}
	if audibleacct.Throttled(acct) {
		return nil, fail(CodeRefused, "Audible refused the account's device: sign in to Audible again on the Bridge (Accounts); its places sync again after that.")
	}
	if acct.RefusedAt != "" {
		return nil, fail(CodeSignIn, "Audible refused the account's sign-in: its places sync again once Audible accepts it, or after signing in again on the Bridge (Accounts).")
	}
	srv := h.o.server()
	if srv == nil {
		return nil, errors.New("the Bridge isn't paired with a server")
	}
	// A pending preorder the server matched to a book (another edition) is
	// left out: unlisted here, so neither read from Audible nor pushed.
	pre := h.o.preorders(ctx, acct.ID)
	before := len(p.Titles)
	p.Titles = slices.DeleteFunc(p.Titles, func(t link.AudibleSyncTitle) bool { return pre[t.ASIN] })
	skipped := before - len(p.Titles)
	res := &Result{Account: acct.ID, Titles: len(p.Titles), SkippedPreorder: skipped}
	now := h.o.Now()

	// The listing goes first, even while Audible asks the Bridge to wait: the
	// push reads it (a title the console stopped syncing is never pushed).
	titles := make([]store.AudibleSyncTitle, len(p.Titles))
	asins := make([]string, len(p.Titles))
	for i, t := range p.Titles {
		titles[i] = store.AudibleSyncTitle(t)
		asins[i] = t.ASIN
	}
	if err := h.o.DB.ListAudibleSync(ctx, acct.ID, titles); err != nil {
		return nil, err
	}
	if until, ok := waiting(acct, now); ok {
		res.Waiting, res.WaitUntil = true, until.UTC().Format(time.RFC3339)
		return res, nil
	}
	if len(asins) == 0 {
		h.o.health(ctx, acct, nil, time.Time{})
		return res, nil
	}

	// The push's rows before Audible's places: a place the push writes while
	// they are read is then compared with the write before it (its echo), never
	// taken for Audible's own.
	list, err := h.o.DB.AudiblePushes(ctx, acct.ID)
	if err != nil {
		return nil, err
	}
	pushes := make(map[string]store.AudiblePush, len(list))
	for _, x := range list {
		pushes[x.ASIN] = x
	}
	rep.Progress(jobs.Progress{Step: "audible", Message: "Reading the places on Audible", Total: len(asins)})
	places, err := h.o.Accounts.Positions(ctx, acct.ID, asins)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		h.o.health(ctx, acct, fmt.Errorf("reading the places on Audible: %w", err), retryAt(err, h.o.Now(), errorWait))
		return nil, fail(CodeAudible, "Audible didn't answer the places: %v", err)
	}

	var events []link.AudibleEvent
	type heardPlace struct {
		asin string
		ms   int64
	}
	heard := map[string]heardPlace{} // a listen's event id -> the place it carries
	for _, t := range p.Titles {
		pl, ok := places[t.ASIN]
		if !ok {
			continue
		}
		res.Heard++
		placeS := float64(pl.PositionMs) / 1000
		switch {
		case pl.PositionMs < 1000:
			res.SkippedStart++
			continue
		case pastRuntime(placeS, t.RuntimeS):
			res.SkippedPastRuntime++
			h.o.Log.Warn("audible: a place on Audible is past the title's runtime, so its match looks wrong; not synced",
				"account", acct.ID, "asin", t.ASIN)
			continue
		case !pl.LastUpdated.IsZero() && now.Sub(pl.LastUpdated) > maxEventAge:
			res.SkippedOld++
			continue
		}
		if x, ok := pushes[t.ASIN]; ok && near(pl.PositionMs, x.LastMs) {
			// The place the push wrote, coming back.
			res.SkippedEcho++
			continue
		}
		at := pl.LastUpdated
		if at.IsZero() {
			at = now
		}
		stamp := at.UTC().Format(time.RFC3339)
		id := EventID(acct.ID, t, pl, "")
		events = append(events, link.AudibleEvent{ID: id, Kind: link.EventListen, LibraryID: t.LibraryID, Path: t.Path,
			FromPos: &placeS, ToPos: &placeS, StartedAt: stamp, EndedAt: stamp, Start: "jump"})
		heard[id] = heardPlace{t.ASIN, pl.PositionMs}
		if atBookEnd(placeS, t.RuntimeS) {
			fid := EventID(acct.ID, t, pl, link.EventFinish)
			events = append(events, link.AudibleEvent{ID: fid, Kind: link.EventFinish, LibraryID: t.LibraryID, Path: t.Path, At: stamp})
			res.Finished++
		}
	}

	rep.Progress(jobs.Progress{Step: "server", Message: "Sending the places to the server", Total: len(events)})
	for start := 0; start < len(events); start += link.MaxAudibleEvents {
		batch := events[start:min(start+link.MaxAudibleEvents, len(events))]
		ans, err := srv.PostAudibleListening(ctx, acct.ID, link.AudibleListening{SentAt: h.o.Now().UTC().Format(time.RFC3339), Events: batch})
		switch {
		case errors.Is(err, link.ErrSyncOff):
			res.SyncOff = true
			h.o.syncOff(ctx, acct)
			return res, nil
		case errors.Is(err, link.ErrUserUnavailable):
			h.o.health(ctx, acct, err, time.Time{})
			return nil, fail(CodeUserUnavailable, "%v", err)
		case errors.Is(err, link.ErrUnknownAccount):
			return nil, fail(CodeUnknownAccount, "The server has no such Audible account: sign in or map the account again on the Bridge's Accounts tab.")
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			h.o.health(ctx, acct, fmt.Errorf("sending the places to the server: %w", err), time.Time{})
			return nil, fmt.Errorf("sending the places to the server: %w", err)
		}
		res.Sent += len(batch)
		for _, r := range ans.Results {
			applied := r.Status == link.SyncApplied || r.Status == link.SyncDiverged
			switch {
			case applied:
				res.Applied++
			case r.Status == link.SyncDuplicate:
				res.Duplicate++
			case r.Status == link.SyncNotApplied && r.Reason == link.SyncBehind:
				res.Behind++
			case r.Status == link.SyncNotApplied:
				res.NotApplied++
			default:
				res.Rejected++
				if r.Reason == link.SyncDurationMismatch {
					res.Mismatched++
				}
			}
			hp, ok := heard[r.ID]
			if !ok || r.Status == link.SyncRejected {
				continue
			}
			x, ok := pushes[hp.asin]
			if !ok || (!applied && x.HeardMs == hp.ms) {
				continue // not listed, or already recorded
			}
			// Applied: the server's place is Audible's now, so a place waiting
			// from an older server row would move Audible back: it goes.
			if err := h.o.DB.AudibleHeard(ctx, x, hp.ms, applied, now); err != nil {
				h.o.Log.Warn("audible: recording a place heard failed", "account", acct.ID, "err", err)
				continue
			}
			if r.Status == link.SyncNotApplied && r.Reason == link.SyncBehind && overwritten(x, hp.ms) {
				h.reassert(ctx, acct.ID, x, res)
			}
		}
		rep.Progress(jobs.Progress{Step: "server", Message: "Sending the places to the server", Done: res.Sent, Total: len(events)})
	}
	if res.Mismatched > 0 {
		h.o.Log.Warn("audible: the server's books for some titles are another length than on Audible, so their matches look wrong; not synced",
			"account", acct.ID, "events", res.Mismatched)
	}
	h.o.health(ctx, acct, nil, time.Time{})
	h.o.Log.Info("audible: places synced", "account", acct.ID, "titles", res.Titles, "heard", res.Heard, "sent", res.Sent,
		"applied", res.Applied, "behind", res.Behind, "duplicate", res.Duplicate, "reasserted", res.Reasserted)
	return res, nil
}

// overwritten reports whether Audible's place (heardMs, which the server found
// behind its own) is an older place over the one the Bridge last wrote for x's
// title: the Bridge wrote one (LastMs), the server row it last read is still
// that place (within echoMs: the server hasn't moved since the push), and
// Audible's place is behind it by more than echoMs. The Audible app does this
// when it uploads the place it still holds after the push (bridge#51).
func overwritten(x store.AudiblePush, heardMs int64) bool {
	return x.LastMs >= 0 && x.RowMs >= 0 && near(x.RowMs, x.LastMs) && heardMs < x.RowMs-echoMs
}

// reassert makes the server's place for x's title wait to be written to
// Audible again (store.RearmAudiblePush, guarded against anything the push
// recorded since the job read x). The push writes it once it has been still
// for StillFor from now (never the job's start: a long sync would leave no
// wait) and only after reading Audible's places again, so a place Audible
// moved further on meanwhile is never overwritten. At most once per place the
// Bridge wrote (reasserted_ms, migration 0014): the sync can't tell the app's
// stale upload from a listener who rewound in the Audible app and listens on
// there, so a second behind for the same written place is left alone until
// the push writes a new one.
func (h *Handler) reassert(ctx context.Context, account string, x store.AudiblePush, res *Result) {
	ok, err := h.o.DB.RearmAudiblePush(ctx, x, h.o.Now())
	switch {
	case err != nil:
		h.o.Log.Warn("audible: re-arming a place to write to Audible failed", "account", account, "err", err)
	case ok:
		res.Reasserted++
		h.o.Log.Info("audible: Audible holds an older place than the one written to it; writing the server's place again",
			"account", account, "asin", x.ASIN, "position_ms", x.RowMs)
	case x.ReassertedMs == x.RowMs:
		h.o.Log.Info("audible: Audible went behind a place already written again once; left alone",
			"account", account, "asin", x.ASIN, "position_ms", x.RowMs)
	}
}

// near reports whether v is within echoMs of ref (-1: none).
func near(v, ref int64) bool { return ref >= 0 && abs(v-ref) <= echoMs }

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// ms is a place in seconds as whole milliseconds.
func ms(s float64) int64 { return int64(math.Round(s * 1000)) }
