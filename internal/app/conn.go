package app

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
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
	// keyPairingID is the pairing's identity (jobs.Pairing.ID), recorded with
	// every job claimed under it: made by a pairing (kept by a pairing with
	// the same server while the token is refused), deleted by an unpair.
	keyPairingID = "pairing.id"
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

	// pairMu orders a pairing's save (Pair, once the server accepted the code)
	// and an unpair: a pairing that read the identity it keeps (a refused one's)
	// never writes it back after an unpair deleted it meanwhile.
	pairMu  sync.Mutex
	mu      sync.Mutex
	client  *link.Client
	pairing Pairing
	// id is the pairing's identity (keyPairingID), "" while unpaired; the
	// link carries it too (link.Client.WithPairing).
	id string
	// refusedAt is set while the server refuses the token (a 401). The token is
	// kept: an unpair or a new pairing revoked it for good, but a demoted
	// admin's Bridge works again once they are an admin again, so the heartbeat
	// retries it every RefusedRetry.
	refusedAt time.Time
	// onChange is called after a pair or an unpair, and when a refusal starts
	// or ends (the runner and the heartbeat wake).
	onChange func()
	// onUnpair is called by Unpair with the link it ended, before onChange:
	// the runner stops that pairing's jobs (jobs.Runner.Unpaired).
	onUnpair func(*link.Client)
}

// openConn loads the stored pairing. It gives the jobs recorded before the
// Bridge kept their pairing (an upgrade) to the pairing in place when they are
// its server's, so a job running then resumes; another server's, or all while
// unpaired, to none (they are dropped).
func openConn(ctx context.Context, db *store.DB, vault secrets.Store, version string, log *slog.Logger) (*Conn, error) {
	c := &Conn{db: db, vault: vault, version: version, log: log, onChange: func() {}, onUnpair: func(*link.Client) {}}
	get := func(k string) string { v, _, _ := db.GetSetting(ctx, k); return v }
	c.pairing = Pairing{ServerURL: get(keyServerURL), ServerID: get(keyServerID), ServerName: get(keyServerName),
		PairedAt: get(keyPairedAt), UnpairedReason: get(keyUnpaired)}
	token, err := vault.Get(secrets.ServerToken)
	if err != nil {
		return nil, err
	}
	stored, _, err := db.GetSetting(ctx, keyPairingID)
	if err != nil {
		return nil, err
	}
	if token == "" || c.pairing.ServerURL == "" {
		if stored != "" {
			// An unpair that forgot the token but not the id: unpaired all the same.
			if err := db.DeleteSetting(ctx, keyPairingID); err != nil {
				return nil, err
			}
		}
		// Rows from before the Bridge kept a job's pairing belong to no pairing
		// now: an id no pairing has drops them at the runner's first look.
		return c, db.AdoptServerJobs(ctx, "", "", newPairingID())
	}
	cl, err := link.New(c.pairing.ServerURL, token, version)
	if err != nil {
		return nil, err
	}
	if stored == "" {
		// Paired before the Bridge kept the pairing's id (an upgrade): the
		// pairing in place gets one, and the jobs recorded under it.
		stored = newPairingID()
		if err := db.SetSetting(ctx, keyPairingID, stored); err != nil {
			return nil, err
		}
	}
	// Only the paired server's rows join the pairing; another server's (an
	// earlier pairing's, its end maybe not yet dropped) get an id no pairing
	// has, so the runner's first look drops them.
	if err := db.AdoptServerJobs(ctx, c.pairing.ServerID, stored, newPairingID()); err != nil {
		return nil, err
	}
	// A refusal before the restart is found out again by the first heartbeat.
	c.client, c.pairing.Paired, c.pairing.UnpairedReason = cl.WithServerID(c.pairing.ServerID).WithPairing(stored), true, ""
	c.id = stored
	return c, nil
}

// newPairingID is a new pairing identity (jobs.Pairing.ID).
func newPairingID() string { return rand.Text() }

// jobsPairing is the pairing as the job runner sees it (jobs.Options.Pairing):
// its identity, "" while unpaired, and its server's id.
func (c *Conn) jobsPairing() jobs.Pairing {
	c.mu.Lock()
	defer c.mu.Unlock()
	return jobs.Pairing{ID: c.id, ServerID: c.pairing.ServerID}
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

// learnServerID asks cl's server for its id when the stored pairing has none
// (link.FetchServerID) and keeps it, so cl's link counts for jobs from then on
// (Bridge.server): a job of a server whose id isn't known runs nowhere. It
// returns the link to use now: cl with the id, or cl as it was when the
// server named none or the pairing changed meanwhile. The id is recorded only
// while cl is still the pairing's link, under the lock an unpair takes: an
// answer that arrives after an unpair and a pairing with another server must
// not record this server's id as the new pairing's (a restart would then read
// the new server's link as this one's, and report this server's jobs there).
func (c *Conn) learnServerID(ctx context.Context, cl *link.Client) *link.Client {
	id, err := cl.FetchServerID(ctx)
	if err != nil || id == "" {
		c.log.Warn("bridge: the paired server's id isn't known yet; no job runs until it is", "err", err)
		return cl
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != cl {
		return cl
	}
	if err := c.db.SetSetting(ctx, keyServerID, id); err != nil {
		c.log.Warn("bridge: recording the paired server's id failed", "err", err)
		return cl
	}
	c.client = cl.WithServerID(id)
	c.pairing.ServerID = id
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

// pairNotSaved is a failure after the server accepted (and spent) the pairing
// code: keeping the token or the pairing on this Bridge failed. Unwrap is the
// cause.
type pairNotSaved struct{ cause error }

func (e *pairNotSaved) Error() string {
	return "the server accepted the pairing, but this Bridge couldn't save it: " + e.cause.Error()
}
func (e *pairNotSaved) Unwrap() error { return e.cause }

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
	// this Bridge's, tagged pairNotSaved so the page says so. The save no
	// longer follows the caller's cancellation (a page closed mid-pairing must
	// not lose a spent code), and a failed one forgets the token again so a
	// restart can't come back paired from a half-saved pairing.
	ctx = context.WithoutCancel(ctx)
	c.pairMu.Lock()
	defer c.pairMu.Unlock()
	cl, err := link.New(serverURL, p.Token, c.version)
	if err != nil {
		return notSaved(err)
	}
	// The pairing's identity: kept by a pairing with the same server while
	// its token is refused (no unpair came between: the jobs it stopped
	// resume on the new link), new otherwise (an unpair cleared it, or another
	// server): the jobs of an earlier pairing are dropped, never resumed.
	c.mu.Lock()
	id := c.id
	if id == "" || p.ServerID == "" || c.pairing.ServerID != p.ServerID {
		id = newPairingID()
	}
	c.mu.Unlock()
	cl = cl.WithServerID(p.ServerID).WithPairing(id)
	if err := c.vault.Set(secrets.ServerToken, p.Token); err != nil {
		return notSaved(err)
	}
	pairedAt := time.Now().UTC().Format(time.RFC3339)
	if err := c.savePairing(ctx, cl.BaseURL(), p, id, pairedAt); err != nil {
		if derr := c.vault.Delete(secrets.ServerToken); derr != nil {
			c.log.Warn("bridge: forgetting the unsaved pairing's token failed", "err", derr)
		}
		return notSaved(err)
	}
	c.mu.Lock()
	if c.client != nil {
		c.client.WithdrawClaimMinPriority()
	}
	c.client, c.refusedAt, c.id = cl, time.Time{}, id
	c.pairing = Pairing{Paired: true, ServerURL: cl.BaseURL(), ServerID: p.ServerID, ServerName: p.ServerName, PairedAt: pairedAt}
	onChange := c.onChange
	c.mu.Unlock()
	c.log.Info("bridge: paired", "server", cl.BaseURL(), "server_name", p.ServerName)
	onChange()
	return nil
}

// savePairing keeps the pairing's settings (id its identity) and clears an
// unpaired reason.
func (c *Conn) savePairing(ctx context.Context, serverURL string, p *link.Paired, id, pairedAt string) error {
	for k, v := range map[string]string{keyServerURL: serverURL, keyServerID: p.ServerID, keyServerName: p.ServerName,
		keyPairedAt: pairedAt, keyPairingID: id} {
		if err := c.db.SetSetting(ctx, k, v); err != nil {
			return err
		}
	}
	return c.db.DeleteSetting(ctx, keyUnpaired)
}

// Unpair forgets the token (the server keeps the bridge token until an admin
// unpairs it in the console or another Bridge pairs) and the pairing's
// identity. reason is shown on the page. The pairing's jobs stop with it
// (onUnpair), a refused pairing's too, as soon as the link is gone here, even
// when forgetting the token then fails: nothing more is sent to the server on
// a link the page shows as unpaired. The jobs are the store's another
// pairing's once the identity is gone, so none resumes after a restart, a new
// pairing with the same server included; onUnpair runs after that and doesn't
// wait for them (their work is released in the background), so the page's
// request is answered at once.
func (c *Conn) Unpair(ctx context.Context, reason string) error {
	c.pairMu.Lock()
	defer c.pairMu.Unlock()
	c.mu.Lock()
	old := c.client
	was := old != nil
	if was {
		old.WithdrawClaimMinPriority()
	}
	c.client, c.refusedAt, c.id = nil, time.Time{}, ""
	c.pairing.Paired = false
	c.pairing.UnpairedReason = reason
	onChange, onUnpair := c.onChange, c.onUnpair
	c.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	err := c.vault.Delete(secrets.ServerToken)
	if derr := c.db.DeleteSetting(ctx, keyPairingID); derr != nil {
		err = errors.Join(err, derr)
	}
	if serr := c.db.SetSetting(ctx, keyUnpaired, reason); serr != nil {
		err = errors.Join(err, serr)
	}
	if was {
		onUnpair(old)
		c.log.Warn("bridge: unpaired", "reason", reason)
		onChange()
	}
	return err
}
