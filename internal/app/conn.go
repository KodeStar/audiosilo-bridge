package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Settings keys of the pairing (the store's settings table); the token itself
// is a secret (secrets.ServerToken).
const (
	keyServerURL  = "pairing.server_url"
	keyServerID   = "pairing.server_id"
	keyServerName = "pairing.server_name"
	keyPairedAt   = "pairing.paired_at"
	keyUnpaired   = "pairing.unpaired_reason"
)

// Pairing is the Bridge's pairing as its page shows it.
type Pairing struct {
	Paired     bool   `json:"paired"`
	ServerURL  string `json:"server_url"`
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	PairedAt   string `json:"paired_at"`
	// UnpairedReason says why a Bridge that was paired no longer is.
	UnpairedReason string `json:"unpaired_reason,omitempty"`
}

// Conn holds the pairing and the server link built from it.
type Conn struct {
	db      *store.DB
	vault   secrets.Store
	version string
	log     *slog.Logger

	mu      sync.Mutex
	client  *link.Client
	pairing Pairing
	// refusedAt is set while the server refuses the token (a 401). The token is
	// kept: an unpair or a new pairing revoked it for good, but a demoted
	// admin's Bridge works again once they are an admin again, so the heartbeat
	// retries it every RefusedRetry.
	refusedAt time.Time
	// onChange is called after a pair or an unpair, and when a refusal starts
	// or ends (the runner and the heartbeat wake).
	onChange func()
}

// openConn loads the stored pairing.
func openConn(ctx context.Context, db *store.DB, vault secrets.Store, version string, log *slog.Logger) (*Conn, error) {
	c := &Conn{db: db, vault: vault, version: version, log: log, onChange: func() {}}
	get := func(k string) string { v, _, _ := db.GetSetting(ctx, k); return v }
	c.pairing = Pairing{ServerURL: get(keyServerURL), ServerID: get(keyServerID), ServerName: get(keyServerName),
		PairedAt: get(keyPairedAt), UnpairedReason: get(keyUnpaired)}
	token, err := vault.Get(secrets.ServerToken)
	if err != nil {
		return nil, err
	}
	if token != "" && c.pairing.ServerURL != "" {
		cl, err := link.New(c.pairing.ServerURL, token, version)
		if err != nil {
			return nil, err
		}
		// A refusal before the restart is found out again by the first heartbeat.
		c.client, c.pairing.Paired, c.pairing.UnpairedReason = cl, true, ""
	}
	return c, nil
}

// Client is the server link, nil while unpaired or refused.
func (c *Conn) Client() *link.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.refusedAt.IsZero() {
		return nil
	}
	return c.client
}

// RefusedRetry is how often a refused token is tried again.
const RefusedRetry = 5 * time.Minute

// retryClient is the link of a refused token when its retry is due (nil
// otherwise).
func (c *Conn) retryClient() *link.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == nil || c.refusedAt.IsZero() || time.Since(c.refusedAt) < RefusedRetry {
		return nil
	}
	return c.client
}

// Refuse records that the server refused cl's token (a 401), keeping it for
// the retries; reason is shown on the page. A 401 on any other link (one a
// re-pair or an unpair has replaced, still in use by a job or a call that
// started before) says nothing about the current pairing and is ignored.
func (c *Conn) Refuse(ctx context.Context, cl *link.Client, reason string) {
	c.mu.Lock()
	if cl == nil || cl != c.client {
		c.mu.Unlock()
		return
	}
	was := c.refusedAt.IsZero()
	cl.WithdrawClaimMinPriority()
	c.refusedAt = time.Now()
	c.pairing.Paired, c.pairing.UnpairedReason = false, reason
	onChange := c.onChange
	c.mu.Unlock()
	if err := c.db.SetSetting(ctx, keyUnpaired, reason); err != nil {
		c.log.Warn("bridge: recording the refusal failed", "err", err)
	}
	if was {
		c.log.Warn("bridge: the server refused the token", "retry_every", RefusedRetry)
		onChange()
	}
}

// accept ends a refusal: the server took cl's token again. A retry that
// answers after an unpair or a new pairing ends nothing.
func (c *Conn) accept(ctx context.Context, cl *link.Client) {
	c.mu.Lock()
	if cl != c.client || c.refusedAt.IsZero() {
		c.mu.Unlock()
		return
	}
	c.refusedAt = time.Time{}
	c.pairing.Paired, c.pairing.UnpairedReason = true, ""
	onChange := c.onChange
	c.mu.Unlock()
	if err := c.db.DeleteSetting(ctx, keyUnpaired); err != nil {
		c.log.Warn("bridge: clearing the refusal failed", "err", err)
	}
	c.log.Info("bridge: the server takes the token again")
	onChange()
}

// Pairing returns the pairing.
func (c *Conn) Pairing() Pairing {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pairing
}

// ErrAlreadyPaired is a pairing while one is live: unpair first.
var ErrAlreadyPaired = errors.New("the Bridge is already paired: unpair it first")

// ErrPairNotSaved wraps a failure after the server accepted the pairing code
// (the code is spent): keeping the token or the pairing on this Bridge failed.
var ErrPairNotSaved = errors.New("the server accepted the pairing, but this Bridge couldn't save it")

// pairNotSaved is a failure after the server accepted the code: errors.Is
// matches ErrPairNotSaved, and Unwrap is the cause.
type pairNotSaved struct{ cause error }

func (e *pairNotSaved) Error() string        { return ErrPairNotSaved.Error() + ": " + e.cause.Error() }
func (e *pairNotSaved) Unwrap() error        { return e.cause }
func (e *pairNotSaved) Is(target error) bool { return target == ErrPairNotSaved }

// notSaved tags err as a failure after the server accepted the code.
func notSaved(err error) error { return &pairNotSaved{cause: err} }

// Pair trades a pairing code from the server's console for the Bridge's token
// and keeps it (sealed). A wrong or expired code is link.ErrUnauthorized.
func (c *Conn) Pair(ctx context.Context, serverURL, code string) error {
	if c.Client() != nil {
		return ErrAlreadyPaired
	}
	p, err := link.Pair(ctx, serverURL, code, c.version)
	if err != nil {
		return err
	}
	// From here on the server has accepted (and spent) the code: a failure is
	// this Bridge's, tagged ErrPairNotSaved so the page says so.
	cl, err := link.New(serverURL, p.Token, c.version)
	if err != nil {
		return notSaved(err)
	}
	if err := c.vault.Set(secrets.ServerToken, p.Token); err != nil {
		return notSaved(err)
	}
	pairedAt := time.Now().UTC().Format(time.RFC3339)
	for k, v := range map[string]string{keyServerURL: cl.BaseURL(), keyServerID: p.ServerID, keyServerName: p.ServerName, keyPairedAt: pairedAt} {
		if err := c.db.SetSetting(ctx, k, v); err != nil {
			return notSaved(err)
		}
	}
	if err := c.db.DeleteSetting(ctx, keyUnpaired); err != nil {
		return notSaved(err)
	}
	c.mu.Lock()
	if c.client != nil {
		c.client.WithdrawClaimMinPriority()
	}
	c.client, c.refusedAt = cl, time.Time{}
	c.pairing = Pairing{Paired: true, ServerURL: cl.BaseURL(), ServerID: p.ServerID, ServerName: p.ServerName, PairedAt: pairedAt}
	onChange := c.onChange
	c.mu.Unlock()
	c.log.Info("bridge: paired", "server", cl.BaseURL(), "server_name", p.ServerName)
	onChange()
	return nil
}

// Unpair forgets the token (the server keeps the bridge token until an admin
// unpairs it in the console or another Bridge pairs). reason is shown on the
// page.
func (c *Conn) Unpair(ctx context.Context, reason string) error {
	c.mu.Lock()
	was := c.client != nil
	if was {
		c.client.WithdrawClaimMinPriority()
	}
	c.client, c.refusedAt = nil, time.Time{}
	c.pairing.Paired = false
	c.pairing.UnpairedReason = reason
	onChange := c.onChange
	c.mu.Unlock()
	if err := c.vault.Delete(secrets.ServerToken); err != nil {
		return err
	}
	if err := c.db.SetSetting(ctx, keyUnpaired, reason); err != nil {
		return err
	}
	if was {
		c.log.Warn("bridge: unpaired", "reason", reason)
		onChange()
	}
	return nil
}
