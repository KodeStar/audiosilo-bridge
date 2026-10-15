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
	// onChange is called after a pair or an unpair (the runner and the
	// heartbeat wake).
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

// Refuse records that the server refused the token (a 401), keeping it for the
// retries; reason is shown on the page.
func (c *Conn) Refuse(ctx context.Context, reason string) {
	c.mu.Lock()
	was := c.client != nil && c.refusedAt.IsZero()
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

// accept ends a refusal: the server took the token again.
func (c *Conn) accept(ctx context.Context) {
	c.mu.Lock()
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
	cl, err := link.New(serverURL, p.Token, c.version)
	if err != nil {
		return err
	}
	if err := c.vault.Set(secrets.ServerToken, p.Token); err != nil {
		return err
	}
	pairedAt := time.Now().UTC().Format(time.RFC3339)
	for k, v := range map[string]string{keyServerURL: cl.BaseURL(), keyServerID: p.ServerID, keyServerName: p.ServerName, keyPairedAt: pairedAt} {
		if err := c.db.SetSetting(ctx, k, v); err != nil {
			return err
		}
	}
	if err := c.db.DeleteSetting(ctx, keyUnpaired); err != nil {
		return err
	}
	c.mu.Lock()
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
