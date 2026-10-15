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
	now     func() time.Time

	mu      sync.Mutex
	client  *link.Client
	pairing Pairing
	// onChange is called after a pair or an unpair (the runner and the
	// heartbeat wake).
	onChange func()
}

// openConn loads the stored pairing.
func openConn(ctx context.Context, db *store.DB, vault secrets.Store, version string, log *slog.Logger) (*Conn, error) {
	c := &Conn{db: db, vault: vault, version: version, log: log, now: time.Now, onChange: func() {}}
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
		c.client, c.pairing.Paired = cl, true
	}
	return c, nil
}

// Client is the server link, nil while unpaired.
func (c *Conn) Client() *link.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
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
	pairedAt := c.now().UTC().Format(time.RFC3339)
	for k, v := range map[string]string{keyServerURL: cl.BaseURL(), keyServerID: p.ServerID, keyServerName: p.ServerName, keyPairedAt: pairedAt} {
		if err := c.db.SetSetting(ctx, k, v); err != nil {
			return err
		}
	}
	if err := c.db.DeleteSetting(ctx, keyUnpaired); err != nil {
		return err
	}
	c.mu.Lock()
	c.client = cl
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
	c.client = nil
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
