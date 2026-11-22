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
// server's) and sends one that is due. A removed account's id is never used
// again (a sign-in again of the same Amazon account gets a fresh one), so a
// late DELETE can't reach an account added since, and this takes no lock.
func (m *Manager) RetryRemoval(ctx context.Context, srv Server) {
	pairing := srv.PairingID()
	if pairing == "" {
		return
	}
	if n, err := m.o.DB.DropAudibleRemovals(ctx, pairing); err != nil {
		m.o.Log.Warn("audible: dropping an ended pairing's pending removals failed", "err", err)
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
	if _, err := m.removeOnServer(ctx, srv, r); err != nil && ctx.Err() == nil {
		m.o.Log.Warn("audible: retrying the pending removal failed", "account", r.Account, "err", err)
	}
}

// removeOnServer sends r's DELETE once. An answer clears r: any 2xx or a 404
// (the server never knew it), or a refusal (an ended token, a 4xx), which is
// returned and logged. A failure with no such answer (the network, a 5xx, a
// 408, a 429) leaves r pending (true) and due again after a backoff: a minute,
// doubling to an hour, or the answer's longer Retry-After (at most a day).
func (m *Manager) removeOnServer(ctx context.Context, srv Server, r store.AudibleRemoval) (pending bool, err error) {
	err = srv.DeleteAudibleAccount(link.SingleAttempt(ctx), r.Account)
	// The answer is recorded even when ctx ends just after it.
	dbCtx := context.WithoutCancel(ctx)
	var apiErr *link.APIError
	hasStatus := errors.As(err, &apiErr)
	switch {
	case err == nil, hasStatus && apiErr.Status == http.StatusNotFound:
		return false, m.o.DB.ClearAudibleRemoval(dbCtx, r.Account)
	case errors.Is(err, link.ErrUnauthorized), hasStatus && !apiErr.Transient():
		m.o.Log.Warn("audible: server refused account removal, dropping pending removal", "account", r.Account, "err", err)
		return false, errors.Join(err, m.o.DB.ClearAudibleRemoval(dbCtx, r.Account))
	}
	delay := min(time.Minute<<min(r.Attempts, 6), time.Hour)
	if hasStatus {
		delay = max(delay, min(apiErr.RetryAfter, 24*time.Hour))
	}
	return true, errors.Join(err, m.o.DB.DeferAudibleRemoval(dbCtx, r.Account, m.o.Now().Add(delay)))
}
