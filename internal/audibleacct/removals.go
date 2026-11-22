package audibleacct

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// RetryRemoval is the heartbeat's turn at the server DELETEs a Remove still
// owes (bridge#40), on srv, the link a heartbeat just went through on. It
// drops the removals of an ended pairing (theirs to send, never another
// server's) and sends one that is due, unless its DELETE is already on its way
// (a Remove's own, or an earlier retry's: sending). A removed account's id is
// never used again (a sign-in again of the same Amazon account gets a fresh
// one), so a late DELETE can't reach an account added since, and this never
// waits for finishMu.
func (m *Manager) RetryRemoval(ctx context.Context, srv Server) {
	pairing := srv.PairingID()
	if pairing == "" {
		return
	}
	if n, err := m.o.DB.DropAudibleRemovals(ctx, pairing); err != nil {
		if ctx.Err() == nil {
			m.o.Log.Warn("audible: dropping an ended pairing's pending removals failed", "err", err)
		}
	} else if n > 0 {
		m.o.Log.Warn("audible: dropped pending server removals of an ended pairing; the console can remove those accounts", "removals", n)
	}
	r, err := m.o.DB.NextAudibleRemoval(ctx, pairing, m.o.Now())
	if errors.Is(err, store.ErrNotFound) || ctx.Err() != nil {
		return
	}
	if err != nil {
		m.o.Log.Warn("audible: reading pending removals failed", "err", err)
		return
	}
	if !m.setSending(r.Account, true) {
		return
	}
	defer m.setSending(r.Account, false)
	if pending, err := m.removeOnServer(ctx, srv, r); pending && err != nil && ctx.Err() == nil {
		m.o.Log.Warn("audible: retrying the pending removal failed", "account", r.Account, "err", err)
	}
}

// setSending marks account's server DELETE as on its way (on) or no longer;
// false when on finds one already on its way.
func (m *Manager) setSending(account string, on bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !on {
		delete(m.sending, account)
		return true
	}
	if m.sending[account] {
		return false
	}
	m.sending[account] = true
	return true
}

// removeOnServer sends r's DELETE once. An answer clears r: any 2xx or a 404
// (the server never knew it), or a refusal (an ended token, a 4xx), which is
// returned and logged. A failure with no such answer (the network, a 5xx, a
// 408, a 429) leaves r pending (true) and due again after a backoff: a minute,
// doubling to an hour, or the answer's longer Retry-After (at most a day). One
// cut short by ctx's end (the Bridge stopping) is left as it was: due again at
// once, at the next start's first heartbeat.
func (m *Manager) removeOnServer(ctx context.Context, srv Server, r store.AudibleRemoval) (pending bool, err error) {
	err = srv.DeleteAudibleAccount(link.SingleAttempt(ctx), r.Account)
	// An answer is recorded even when ctx ends just after it.
	dbCtx := context.WithoutCancel(ctx)
	var apiErr *link.APIError
	hasStatus := errors.As(err, &apiErr)
	switch {
	case err == nil, hasStatus && apiErr.Status == http.StatusNotFound:
		m.clearRemoval(dbCtx, r.Account)
		return false, nil
	case errors.Is(err, link.ErrUnauthorized), hasStatus && !apiErr.Transient():
		m.o.Log.Warn("audible: server refused account removal, dropping pending removal", "account", r.Account, "err", err)
		m.clearRemoval(dbCtx, r.Account)
		return false, err
	case ctx.Err() != nil:
		return true, err
	}
	delay := min(time.Minute<<min(r.Attempts, 6), time.Hour)
	if hasStatus {
		delay = max(delay, min(apiErr.RetryAfter, 24*time.Hour))
	}
	if derr := m.o.DB.DeferAudibleRemoval(dbCtx, r.Account, m.o.Now().Add(delay)); derr != nil {
		m.o.Log.Warn("audible: recording when to retry a pending removal failed", "account", r.Account, "err", derr)
	}
	return true, err
}

// clearRemoval forgets account's answered removal. A failure is logged: the
// row stays, so the DELETE goes again and the server answers it the same.
func (m *Manager) clearRemoval(ctx context.Context, account string) {
	if err := m.o.DB.ClearAudibleRemoval(ctx, account); err != nil {
		m.o.Log.Warn("audible: clearing an answered pending removal failed", "account", account, "err", err)
	}
}
