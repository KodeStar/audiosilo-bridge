package audibleacct

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// RetryRemoval gives the heartbeat one due DELETE, bounded by the link's
// request timeout. Skip a busy accounts manager until the next heartbeat.
func (m *Manager) RetryRemoval(ctx context.Context, current func() Server) {
	select {
	case m.finishMu <- struct{}{}:
		defer m.unlockFinish()
	default:
		return
	}
	srv := current()
	pairing := ""
	if srv != nil {
		pairing = srv.PairingID()
	}
	r, err := m.o.DB.NextAudibleRemoval(ctx, pairing, m.o.Now())
	if errors.Is(err, store.ErrNotFound) || ctx.Err() != nil {
		return
	}
	if err != nil {
		m.o.Log.Warn("audible: reading pending removals failed", "err", err)
		return
	}
	if srv == nil || r.Pairing != pairing {
		m.o.Log.Warn("audible: dropping pending removal because its pairing ended", "account", r.Account)
		err = m.o.DB.ClearAudibleRemoval(ctx, r)
	} else {
		_, err = m.removeOnServer(ctx, srv, r)
	}
	if err != nil && ctx.Err() == nil {
		m.o.Log.Warn("audible: retrying the pending removal failed", "account", r.Account, "err", err)
	}
}

// removeOnServer attempts the journaled DELETE under finishMu. A transient
// failure leaves the removal scheduled. A permanent refusal ends the retry
// and is returned to the page. Keep SQLite writes independent of an HTTP timeout.
func (m *Manager) removeOnServer(ctx context.Context, srv Server, r store.AudibleRemoval) (pending bool, err error) {
	if !r.Uncertain {
		if err := m.o.DB.MarkAudibleRemovalSent(ctx, r); err != nil {
			return true, err
		}
	}
	err = srv.DeleteAudibleAccount(link.SingleAttempt(ctx), r.Account)
	dbCtx := context.WithoutCancel(ctx)
	var apiErr *link.APIError
	hasStatus := errors.As(err, &apiErr)
	switch {
	case err == nil, hasStatus && apiErr.Status == http.StatusNotFound:
		return false, m.o.DB.ClearAudibleRemoval(dbCtx, r)
	case errors.Is(err, link.ErrUnauthorized), hasStatus && !apiErr.Transient():
		m.o.Log.Warn("audible: server refused account removal, dropping pending removal", "account", r.Account, "err", err)
		return false, errors.Join(err, m.o.DB.ClearAudibleRemoval(dbCtx, r))
	default:
		// One minute, doubling to one hour. A longer Retry-After wins.
		delay := min(time.Minute<<min(r.Attempts, 6), time.Hour)
		if hasStatus {
			delay = max(delay, apiErr.RetryAfter)
		}
		// A dial failure never sent the request, and the server's 429 limiter
		// ran before the route. Neither resolves an earlier missing answer.
		var op *net.OpError
		dialFailed := errors.As(err, &op) && op.Op == "dial"
		rateLimited := hasStatus && apiErr.Status == http.StatusTooManyRequests
		r.Uncertain = r.Uncertain || !dialFailed && !rateLimited
		return true, errors.Join(err, m.o.DB.DeferAudibleRemoval(dbCtx, r, m.o.Now().Add(delay)))
	}
}

// newAccountID lets a re-add cancel a pending DELETE under finishMu. If
// every attempt failed before the route ran, the PUT can reclaim the old id.
// Otherwise a delayed DELETE could still arrive. Confirm the old removal
// and use a fresh id in that case.
func (m *Manager) newAccountID(ctx context.Context, srv Server, hash, marketplace string) (string, error) {
	r, err := m.o.DB.RemovedAudibleCustomer(ctx, srv.PairingID(), hash, marketplace)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return randomID()
	case err != nil:
		return "", err
	case !r.Uncertain:
		// A failed sign-in rolls the PUT back with a DELETE. Until the sign-in
		// commits and clears the tombstone, a restart must allow for that call.
		return r.Account, m.o.DB.MarkAudibleRemovalSent(ctx, r)
	}
	if _, err := m.removeOnServer(ctx, srv, r); err != nil {
		return "", fmt.Errorf("could not finish the account's previous removal: %w", err)
	}
	return randomID()
}
