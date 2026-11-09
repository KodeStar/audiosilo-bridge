// Package audibleacct holds the Bridge's Audible accounts (BRIDGE-PLAN.md
// Phase 4a, decision 19): the paste-back sign-in (a PKCE pair and a device
// serial held in memory for LoginTTL while the admin signs in at Amazon), the
// device registration it ends in, the mapping of each account to a server user
// and a books library (recorded on the server first), and the removal that
// forgets an account locally and then deregisters its device.
//
// The sign-in (audible.Credentials: tokens and the device's RSA key, full
// access to the Amazon account) is sealed in the vault as audible_<id> and
// handed only to the audible client. The store keeps what the page shows (the
// label, marketplace, the customer id's hash, the mapping, how polls went);
// nothing here logs, returns or reports a credential, and errors carry
// Audible's or the server's answer, never a token.
package audibleacct

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// LoginTTL is how long a started sign-in waits for its redirect URL.
const LoginTTL = 15 * time.Minute

// maxLogins bounds the sign-ins held at once (the oldest goes first).
const maxLogins = 8

// maxLabel is the longest label (the server keeps 100 characters).
const maxLabel = 100

// Policies are what an account backs up (Phase 4b acts on them): every new
// title, titles in a series the user already has, or only what an admin picks.
var Policies = []string{"all", "series", "manual"}

// IDPattern is an account's id: the Bridge's own, opaque to the server.
var IDPattern = regexp.MustCompile(`^[a-z0-9]{8,32}$`)

// ASINPattern is an ASIN as the server takes one.
var ASINPattern = regexp.MustCompile(`^[A-Z0-9]{10}$`)

// Errors.
var (
	// ErrInvalid: a request the page can correct (an unknown marketplace or
	// policy, no user or library, a redirect URL with no code); the message
	// says which.
	ErrInvalid = errors.New("invalid")
	// ErrLoginExpired: no sign-in waits under that id (it expired, was used,
	// or the Bridge restarted).
	ErrLoginExpired = errors.New("the sign-in expired: open the Amazon sign-in again")
	// ErrNotFound: no such account on this Bridge.
	ErrNotFound = errors.New("no such Audible account on this Bridge")
	// ErrRefused: Audible refused the account's sign-in (401/403 on a signed
	// call): signing in again ends it.
	ErrRefused = errors.New("Audible refused the sign-in: sign in again on the Accounts tab")
	// ErrRegister: Amazon didn't take the sign-in (the code was used or
	// expired, or the registration failed); the wrapped error says how.
	ErrRegister = errors.New("the Amazon sign-in wasn't accepted")
	// ErrNotPaired: signing in or mapping needs the server, and the Bridge
	// isn't paired with one.
	ErrNotPaired = errors.New("pair the Bridge with a server first")
)

// RefusedText is an account's last_error after a refusal.
const RefusedText = "Audible refused the sign-in (the device was removed from the Amazon account, or it expired): sign in again"

// ThrottledText is an account's last_error after Audible denied its device a
// license (audible.ErrThrottled): the account is refused like a sign-in
// Audible refused, as a device refusal (store.RefusalDevice), and only a new
// sign-in ends it, since its library still lists (a poll keeps it,
// store.AudiblePolled). The kind is its own column (bridge#46): this is copy
// only (migration 0011 read 4b's rows by it).
const ThrottledText = "Audible refused this device its downloads (customer throttled): sign in again"

// Throttled reports whether a is refused for its device's licenses
// (store.RefusalDevice) rather than its sign-in.
func Throttled(a store.AudibleAccount) bool {
	return a.RefusedAt != "" && a.RefusalKind == store.RefusalDevice
}

// OldProfile reports whether a's device registered with a profile other than
// the one this build registers with (audible.ProfileVersion), or before the
// profile was recorded: Amazon denies its licenses (audible.ErrThrottled)
// until the account signs in again, which registers a new device and removes
// the old one.
func OldProfile(a store.AudibleAccount) bool { return !audible.IsCurrentProfile(a.DeviceProfile) }

// Service is Audible as the accounts use it: the audible package's calls
// (Audible returns them), a stub in tests.
type Service interface {
	AuthorizeURL(mp audible.Marketplace, pkce audible.PKCE, serial string) string
	Register(ctx context.Context, mp audible.Marketplace, code string, pkce audible.PKCE, serial string) (audible.Credentials, error)
	Deregister(ctx context.Context, creds audible.Credentials) error
	// Library answers ErrRefused (wrapped) when Audible refuses the device's
	// credentials (401/403 on the signed call).
	Library(ctx context.Context, creds audible.Credentials) ([]source.Record, error)
	// Title, License and Download are an import's (Phase 4b): one title of the
	// library, a book's license and its download. Title and License answer
	// ErrRefused (wrapped) as Library does; Download's 403 is an expired URL,
	// never a refusal.
	Title(ctx context.Context, creds audible.Credentials, asin string) (source.Record, error)
	License(ctx context.Context, creds audible.Credentials, asin string) (audible.License, error)
	Download(ctx context.Context, creds audible.Credentials, url, dest string, opts audible.DownloadOptions) error
	// LastPositions, ContentACR and SetLastPosition are the position sync's
	// (Phase 4c): the places Audible heard, a title's content reference (a
	// license request asking only for it) and a place written back. Their
	// 401/403 is answered as it came, never as ErrRefused: a sync error never
	// refuses the account.
	LastPositions(ctx context.Context, creds audible.Credentials, asins []string) (map[string]audible.Position, error)
	ContentACR(ctx context.Context, creds audible.Credentials, asin string) (string, error)
	SetLastPosition(ctx context.Context, creds audible.Credentials, asin, acr string, positionMs int64) error
	// Orders and Products are a poll's preorders: the account's orders (a
	// pending preorder shows nowhere else) and the catalog's description of
	// titles not in the library. Their 401/403 is answered as it came, never
	// as ErrRefused: a poll's preorders never refuse the account.
	Orders(ctx context.Context, creds audible.Credentials) ([]audible.Order, error)
	Products(ctx context.Context, creds audible.Credentials, asins []string) ([]source.Record, error)
}

// Audible is the real Service, its calls made with opts (endpoints, an HTTP
// client; a build with the audiblefake tag points every endpoint at the fake
// through audible.EndpointsFor). Every error it answers is redacted of the
// account's own customer id and device serial by value
// (audible.Credentials.Redact), beside the shapes the audible package redacts.
func Audible(opts ...audible.Option) Service { return client{opts: opts} }

type client struct{ opts []audible.Option }

func (c client) AuthorizeURL(mp audible.Marketplace, pkce audible.PKCE, serial string) string {
	return audible.BuildAuthorizeURL(mp, pkce, serial, c.opts...)
}

func (c client) Register(ctx context.Context, mp audible.Marketplace, code string, pkce audible.PKCE, serial string) (audible.Credentials, error) {
	creds, err := audible.Register(ctx, mp, code, pkce, serial, c.opts...)
	return creds, audible.Credentials{DeviceSerial: serial}.Redact(err)
}

func (c client) Deregister(ctx context.Context, creds audible.Credentials) error {
	return creds.Redact(audible.Deregister(ctx, creds, c.opts...))
}

func (c client) Library(ctx context.Context, creds audible.Credentials) ([]source.Record, error) {
	cl, err := audible.NewClient(creds, c.opts...)
	if err != nil {
		return nil, err
	}
	recs, err := cl.Library(ctx)
	return recs, refused(creds.Redact(err))
}

func (c client) Title(ctx context.Context, creds audible.Credentials, asin string) (source.Record, error) {
	cl, err := audible.NewClient(creds, c.opts...)
	if err != nil {
		return source.Record{}, err
	}
	rec, err := cl.Title(ctx, asin)
	return rec, refused(creds.Redact(err))
}

func (c client) License(ctx context.Context, creds audible.Credentials, asin string) (audible.License, error) {
	cl, err := audible.NewClient(creds, c.opts...)
	if err != nil {
		return audible.License{}, err
	}
	lic, err := cl.License(ctx, asin)
	return lic, refused(creds.Redact(err))
}

func (c client) Download(ctx context.Context, creds audible.Credentials, url, dest string, opts audible.DownloadOptions) error {
	cl, err := audible.NewClient(creds, c.opts...)
	if err != nil {
		return err
	}
	return creds.Redact(cl.Download(ctx, url, dest, opts))
}

func (c client) LastPositions(ctx context.Context, creds audible.Credentials, asins []string) (map[string]audible.Position, error) {
	cl, err := audible.NewClient(creds, c.opts...)
	if err != nil {
		return nil, err
	}
	out, err := cl.LastPositions(ctx, asins, nil)
	return out, creds.Redact(err)
}

func (c client) ContentACR(ctx context.Context, creds audible.Credentials, asin string) (string, error) {
	cl, err := audible.NewClient(creds, c.opts...)
	if err != nil {
		return "", err
	}
	acr, err := cl.ContentACR(ctx, asin)
	return acr, creds.Redact(err)
}

func (c client) SetLastPosition(ctx context.Context, creds audible.Credentials, asin, acr string, positionMs int64) error {
	cl, err := audible.NewClient(creds, c.opts...)
	if err != nil {
		return err
	}
	return creds.Redact(cl.SetLastPosition(ctx, asin, acr, positionMs))
}

func (c client) Orders(ctx context.Context, creds audible.Credentials) ([]audible.Order, error) {
	cl, err := audible.NewClient(creds, c.opts...)
	if err != nil {
		return nil, err
	}
	out, err := cl.Orders(ctx)
	return out, creds.Redact(err)
}

func (c client) Products(ctx context.Context, creds audible.Credentials, asins []string) ([]source.Record, error) {
	cl, err := audible.NewClient(creds, c.opts...)
	if err != nil {
		return nil, err
	}
	out, err := cl.Products(ctx, asins)
	return out, creds.Redact(err)
}

// refused wraps a 401/403 on a signed API call as ErrRefused.
func refused(err error) error {
	if audible.IsRefused(err) {
		return fmt.Errorf("%w (HTTP %d)", ErrRefused, audible.HTTPStatus(err))
	}
	return err
}

// Store is the part of the store the accounts use (*store.DB).
type Store interface {
	AudibleAccounts(ctx context.Context) ([]store.AudibleAccount, error)
	GetAudibleAccount(ctx context.Context, id string) (store.AudibleAccount, error)
	AudibleAccountByCustomer(ctx context.Context, hash, marketplace string) (store.AudibleAccount, error)
	SignInAudibleAccount(ctx context.Context, a store.AudibleAccount, at time.Time) error
	MapAudibleAccount(ctx context.Context, a store.AudibleAccount) error
	AudiblePolled(ctx context.Context, id string, titles, matched int, at time.Time) error
	RefuseAudibleAccount(ctx context.Context, id, kind, why string, at time.Time) error
	AudibleAccepted(ctx context.Context, id, kind string) (bool, error)
	AudibleAccountError(ctx context.Context, id, why string) error
	DeleteAudibleAccount(ctx context.Context, id string) error
	AudiblePreorders(ctx context.Context, account string) ([]store.AudiblePreorder, error)
	SetAudiblePreorders(ctx context.Context, account string, ps []store.AudiblePreorder) error
}

// Server is the part of the server link the accounts use (*link.Client).
type Server interface {
	PutAudibleAccount(ctx context.Context, account string, in link.AudibleAccountInput) (*link.AudibleAccount, error)
	DeleteAudibleAccount(ctx context.Context, account string) error
}

// Options wire a Manager.
type Options struct {
	Vault   secrets.Store
	DB      Store
	Service Service
	Log     *slog.Logger
	// OnChange runs after a sign-in, a removal or a refusal (the heartbeat's
	// checklist moves).
	OnChange func()
	// Now is the clock (tests replace it).
	Now func() time.Time
}

// Manager is the Bridge's Audible accounts.
type Manager struct {
	o Options

	mu     sync.Mutex
	logins map[string]login
	// finishMu serialises Finish from the account lookup until the row is
	// stored: two sign-ins of the same Amazon account finishing at once would
	// otherwise both find no account and make two (the later now replaces the
	// earlier, as any sign-in again does; the store's unique index on
	// (customer_hash, marketplace) backs it). Held across the server call;
	// sign-ins are rare. Remove takes it while it forgets locally, so a sign-in
	// again finishing meanwhile never stores the account back after it.
	finishMu sync.Mutex
}

// login is a sign-in in progress: what Register needs once the admin pastes
// the redirect URL back. In memory only.
type login struct {
	mp      audible.Marketplace
	pkce    audible.PKCE
	serial  string
	expires time.Time
}

// New returns the accounts.
func New(o Options) *Manager {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.OnChange == nil {
		o.OnChange = func() {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Service == nil {
		o.Service = Audible(audible.WithLogger(o.Log))
	}
	return &Manager{o: o, logins: map[string]login{}}
}

// Started is a sign-in waiting for the admin: open URL at Amazon, then paste
// the page it lands on back (Finish) before ExpiresAt.
type Started struct {
	Login       string
	URL         string
	Marketplace string
	ExpiresAt   time.Time
}

// Start begins a sign-in in marketplace (an audible marketplace code).
func (m *Manager) Start(marketplace string) (Started, error) {
	mp, ok := audible.MarketplaceFor(strings.ToLower(strings.TrimSpace(marketplace)))
	if !ok {
		return Started{}, fmt.Errorf("%w: unknown marketplace %q", ErrInvalid, marketplace)
	}
	pkce, err := audible.NewPKCE()
	if err != nil {
		return Started{}, err
	}
	serial, err := audible.NewDeviceSerial()
	if err != nil {
		return Started{}, err
	}
	id, err := randomID()
	if err != nil {
		return Started{}, err
	}
	now := m.o.Now()
	l := login{mp: mp, pkce: pkce, serial: serial, expires: now.Add(LoginTTL)}
	m.mu.Lock()
	m.pruneLocked(now)
	for len(m.logins) >= maxLogins {
		oldest := ""
		for k, v := range m.logins {
			if oldest == "" || v.expires.Before(m.logins[oldest].expires) {
				oldest = k
			}
		}
		delete(m.logins, oldest)
	}
	m.logins[id] = l
	m.mu.Unlock()
	return Started{Login: id, URL: m.o.Service.AuthorizeURL(mp, pkce, serial), Marketplace: mp.Code, ExpiresAt: l.expires.UTC()}, nil
}

// pruneLocked drops the expired sign-ins.
func (m *Manager) pruneLocked(now time.Time) {
	for k, v := range m.logins {
		if !now.Before(v.expires) {
			delete(m.logins, k)
		}
	}
}

// Mapping is an account's mapping: its label, the server user (UserName is
// what the page shows), the books library its titles go to, the policy and
// whether its places sync.
type Mapping struct {
	Label     string
	UserID    int64
	UserName  string
	LibraryID int64
	Policy    string
	Sync      bool
}

// check normalises and checks m.
func (m *Mapping) check() error {
	m.Label = strings.Join(strings.Fields(m.Label), " ")
	if len(m.Label) > maxLabel {
		m.Label = strings.ToValidUTF8(m.Label[:maxLabel], "")
	}
	m.Policy = strings.ToLower(strings.TrimSpace(m.Policy))
	switch {
	case !slices.Contains(Policies, m.Policy):
		return fmt.Errorf("%w: the policy must be all, series or manual", ErrInvalid)
	case m.UserID <= 0:
		return fmt.Errorf("%w: pick the person the account belongs to", ErrInvalid)
	case m.LibraryID <= 0:
		return fmt.Errorf("%w: pick the books library its titles go to", ErrInvalid)
	}
	return nil
}

// label is m's label, else "<user> (<MARKETPLACE>)".
func (m Mapping) label(marketplace string) string {
	if m.Label != "" {
		return m.Label
	}
	name := m.UserName
	if name == "" {
		name = "Audible"
	}
	return name + " (" + strings.ToUpper(marketplace) + ")"
}

// madeLabel reports whether label is the one the Bridge made for a (its person
// and store, the forms send it back as it is): mapped again, such a name
// follows the new person rather than keep the old one's.
func madeLabel(label string, a store.AudibleAccount) bool {
	return label != "" && label == (Mapping{UserName: a.UserName}).label(a.Marketplace)
}

// Finish is the end of a sign-in: the login Start answered, the redirect URL
// the admin pasted back, and the account's mapping.
type Finish struct {
	Login       string
	RedirectURL string
	Mapping
}

// Finish ends a sign-in: registers the device with the code in the pasted URL,
// records the mapping on the server (srv, paired with serverID), seals the
// sign-in and records the account. A second sign-in of the same Amazon account
// in the same marketplace replaces the first (its id kept, its old device
// deregistered after). Anything that fails after the device registered
// deregisters it again, so a failed sign-in leaves no device behind, and puts
// back what it changed: the account's earlier sign-in in the vault and the
// server's record of it (a new account removed, an existing one's earlier
// mapping recorded again).
func (m *Manager) Finish(ctx context.Context, srv Server, serverID string, f Finish) (store.AudibleAccount, error) {
	if err := f.check(); err != nil {
		return store.AudibleAccount{}, err
	}
	if srv == nil {
		return store.AudibleAccount{}, ErrNotPaired
	}
	code, err := audible.ParseAuthCode(f.RedirectURL)
	if err != nil {
		// The login stays: the admin can paste the right page.
		return store.AudibleAccount{}, fmt.Errorf("%w: paste the address of the page Amazon showed after the sign-in (it holds an authorization code)", ErrInvalid)
	}
	m.mu.Lock()
	m.pruneLocked(m.o.Now())
	l, ok := m.logins[f.Login]
	delete(m.logins, f.Login) // the code is spent whatever happens next
	m.mu.Unlock()
	if !ok {
		return store.AudibleAccount{}, ErrLoginExpired
	}

	creds, err := m.o.Service.Register(ctx, l.mp, code, l.pkce, l.serial)
	if err != nil {
		return store.AudibleAccount{}, fmt.Errorf("%w: %w", ErrRegister, err)
	}
	// From here a failure deregisters the new device again.
	undo := func(why string) {
		if derr := m.o.Service.Deregister(context.WithoutCancel(ctx), creds); derr != nil {
			m.o.Log.Warn("audible: a sign-in failed and its device could not be removed again; remove it in the Amazon account's devices",
				"step", why, "err", derr)
		}
	}
	// See finishMu: held from the lookup until the row is stored.
	m.finishMu.Lock()
	unlock := sync.OnceFunc(m.finishMu.Unlock)
	defer unlock()
	hash := customerHash(creds.CustomerID)
	prev, perr := m.o.DB.AudibleAccountByCustomer(ctx, hash, l.mp.Code)
	switch {
	case perr == nil:
	case errors.Is(perr, store.ErrNotFound):
		prev = store.AudibleAccount{}
	default:
		undo("lookup")
		return store.AudibleAccount{}, perr
	}
	if prev.ID != "" && madeLabel(f.Label, prev) {
		f.Label = ""
	}
	acct := store.AudibleAccount{ID: prev.ID, Marketplace: l.mp.Code, CustomerHash: hash, ServerID: serverID,
		Label: f.label(l.mp.Code), UserID: f.UserID, UserName: f.UserName, LibraryID: f.LibraryID, Policy: f.Policy, Sync: f.Sync,
		DeviceProfile: creds.Profile}
	if acct.ID == "" {
		if acct.ID, err = randomID(); err != nil {
			undo("id")
			return store.AudibleAccount{}, err
		}
	}
	name := secrets.AudibleName(acct.ID)
	// The account's earlier sign-in, kept to put back ("" when there was none,
	// or it can't be read) and to deregister its device once this one is kept.
	var oldRaw string
	var old *audible.Credentials
	if prev.ID != "" {
		if raw, gerr := m.o.Vault.Get(name); gerr == nil && raw != "" {
			if c, cerr := audible.ParseCredentials(raw); cerr == nil {
				oldRaw, old = raw, &c
			}
		}
	}
	sealedNew := false
	// rollback undoes a sign-in that failed once the server was asked to record
	// it, each step best effort: a failed call may have been applied before its
	// answer was lost.
	rollback := func(why string) {
		undo(why)
		bg := context.WithoutCancel(ctx)
		if sealedNew {
			verr := m.o.Vault.Delete(name)
			if oldRaw != "" {
				verr = m.o.Vault.Set(name, oldRaw)
			}
			if verr != nil {
				m.o.Log.Warn("audible: putting the account's earlier sign-in back failed", "account", acct.ID, "err", verr)
			}
		}
		switch {
		case prev.ID == "":
			if derr := srv.DeleteAudibleAccount(bg, acct.ID); derr != nil {
				m.o.Log.Warn("audible: removing a failed sign-in's account from the server failed", "account", acct.ID, "err", derr)
			}
		case prev.ServerID == serverID:
			if _, perr := srv.PutAudibleAccount(bg, prev.ID, input(prev)); perr != nil {
				m.o.Log.Warn("audible: recording the account's earlier mapping on the server again failed", "account", acct.ID, "err", perr)
			}
		}
	}
	if _, err := srv.PutAudibleAccount(ctx, acct.ID, input(acct)); err != nil {
		rollback("server")
		return store.AudibleAccount{}, err
	}
	sealed, err := creds.Marshal()
	if err == nil {
		if err = m.o.Vault.Set(name, sealed); err == nil {
			sealedNew = true
		}
	}
	if err == nil {
		err = m.o.DB.SignInAudibleAccount(ctx, acct, m.o.Now())
	}
	if err != nil {
		rollback("store")
		return store.AudibleAccount{}, fmt.Errorf("could not keep the sign-in: %w", err)
	}
	// Stored: a later sign-in of this Amazon account now finds this one (and
	// its new device in the vault), so the old device's removal needs no lock.
	unlock()
	switch {
	case old != nil && old.DeviceSerial != creds.DeviceSerial:
		if derr := m.o.Service.Deregister(context.WithoutCancel(ctx), *old); derr != nil {
			m.o.Log.Warn("audible: the account's earlier device could not be removed; remove it in the Amazon account's devices",
				"account", acct.ID, "err", derr)
		}
	case old == nil && prev.ID != "":
		m.o.Log.Warn("audible: the account's earlier sign-in couldn't be opened, so its device could not be removed; "+
			"remove it in the Amazon account's devices", "account", acct.ID)
	}
	m.o.Log.Info("audible: account signed in", "account", acct.ID, "marketplace", acct.Marketplace, "again", prev.ID != "")
	m.o.OnChange()
	return m.o.DB.GetAudibleAccount(ctx, acct.ID)
}

// input is the server's mapping of a.
func input(a store.AudibleAccount) link.AudibleAccountInput {
	return link.AudibleAccountInput{Label: a.Label, Marketplace: a.Marketplace, UserID: a.UserID, LibraryID: a.LibraryID,
		Policy: a.Policy, Sync: a.Sync}
}

// Map records a new mapping of account id: on the server (srv, paired with
// serverID) first, then here. ErrNotFound: no such account.
func (m *Manager) Map(ctx context.Context, srv Server, serverID, id string, mp Mapping) (store.AudibleAccount, error) {
	if err := mp.check(); err != nil {
		return store.AudibleAccount{}, err
	}
	if srv == nil {
		return store.AudibleAccount{}, ErrNotPaired
	}
	a, err := m.Account(ctx, id)
	if err != nil {
		return store.AudibleAccount{}, err
	}
	if madeLabel(mp.Label, a) {
		mp.Label = ""
	}
	a.Label, a.UserID, a.UserName, a.LibraryID, a.Policy, a.Sync, a.ServerID =
		mp.label(a.Marketplace), mp.UserID, mp.UserName, mp.LibraryID, mp.Policy, mp.Sync, serverID
	if _, err := srv.PutAudibleAccount(ctx, a.ID, input(a)); err != nil {
		return store.AudibleAccount{}, err
	}
	if err := m.o.DB.MapAudibleAccount(ctx, a); err != nil {
		return store.AudibleAccount{}, err
	}
	m.o.OnChange()
	return m.o.DB.GetAudibleAccount(ctx, id)
}

// Remove forgets account id here first (the sealed sign-in and the row, so the
// Bridge stops using it whatever happens next), then removes it on the server
// (srv; nil while unpaired) and deregisters its device at Amazon. warning
// says what didn't happen ("" when all did); err is a local failure. Once it
// starts forgetting it runs to its end even if the page goes away: the sealed
// sign-in is the only way to deregister the device.
func (m *Manager) Remove(ctx context.Context, srv Server, id string) (warning string, err error) {
	// See finishMu: held until the account is forgotten here.
	m.finishMu.Lock()
	unlock := sync.OnceFunc(m.finishMu.Unlock)
	defer unlock()
	if _, err := m.Account(ctx, id); err != nil {
		return "", err
	}
	ctx = context.WithoutCancel(ctx)
	creds, credErr := m.credentials(id)
	if err := m.o.Vault.Delete(secrets.AudibleName(id)); err != nil {
		return "", fmt.Errorf("could not forget the sign-in: %w", err)
	}
	if err := m.o.DB.DeleteAudibleAccount(ctx, id); err != nil {
		return "", err
	}
	unlock()
	m.o.OnChange()
	var warnings []string
	if srv == nil {
		warnings = append(warnings, "the Bridge isn't paired, so the server may still list the account")
	} else if err := srv.DeleteAudibleAccount(ctx, id); err != nil {
		m.o.Log.Warn("audible: removing the account on the server failed", "account", id, "err", err)
		warnings = append(warnings, "the server didn't confirm the removal, so it may still list the account")
	}
	if credErr != nil {
		warnings = append(warnings, "the sign-in couldn't be read, so its AudioSilo device is still on the Amazon account: remove it in Amazon's device list")
	} else if err := m.o.Service.Deregister(ctx, creds); err != nil {
		m.o.Log.Warn("audible: deregistering the account's device failed", "account", id, "err", err)
		warnings = append(warnings, "Amazon didn't confirm removing the AudioSilo device: remove it in Amazon's device list")
	}
	m.o.Log.Info("audible: account removed", "account", id)
	return strings.Join(warnings, "; "), nil
}

// Accounts lists the accounts.
func (m *Manager) Accounts(ctx context.Context) ([]store.AudibleAccount, error) {
	return m.o.DB.AudibleAccounts(ctx)
}

// Account returns one account (ErrNotFound).
func (m *Manager) Account(ctx context.Context, id string) (store.AudibleAccount, error) {
	if !IDPattern.MatchString(id) {
		return store.AudibleAccount{}, ErrNotFound
	}
	a, err := m.o.DB.GetAudibleAccount(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.AudibleAccount{}, ErrNotFound
	}
	return a, err
}

// credentials opens account id's sealed sign-in.
func (m *Manager) credentials(id string) (audible.Credentials, error) {
	raw, err := m.o.Vault.Get(secrets.AudibleName(id))
	if err != nil {
		return audible.Credentials{}, err
	}
	if raw == "" {
		return audible.Credentials{}, ErrNotFound
	}
	return audible.ParseCredentials(raw)
}

// Library lists account id's Audible library. A refusal (401/403 on the signed
// call) marks the account refused and answers ErrRefused.
func (m *Manager) Library(ctx context.Context, id string) ([]source.Record, error) {
	var recs []source.Record
	err := m.signed(ctx, id, func(creds audible.Credentials) (err error) {
		recs, err = m.o.Service.Library(ctx, creds)
		return err
	})
	return recs, err
}

// Title reads one title of account id's library (an import's metadata);
// audible.ErrNotInLibrary when the library doesn't hold it, unless it is a
// pending preorder a poll remembers (Preorders): then its preorder record, so
// an import says it isn't released yet rather than not owned. A refusal is
// Library's.
func (m *Manager) Title(ctx context.Context, id, asin string) (source.Record, error) {
	var rec source.Record
	err := m.signed(ctx, id, func(creds audible.Credentials) (err error) {
		rec, err = m.o.Service.Title(ctx, creds, asin)
		return err
	})
	if errors.Is(err, audible.ErrNotInLibrary) {
		if pre, ok := m.rememberedPreorder(ctx, id, asin); ok {
			return pre, nil
		}
	}
	return rec, err
}

// License asks for a book's download license with account id's sign-in. The
// answer holds the book's key and iv: the caller keeps it in memory and hands
// it to audible.Decrypt only. A refusal is Library's; a license denied to the
// device (audible.ErrThrottled) marks the account refused the same way, with
// ThrottledText, and answers the denial.
func (m *Manager) License(ctx context.Context, id, asin string) (audible.License, error) {
	var lic audible.License
	err := m.signed(ctx, id, func(creds audible.Credentials) (err error) {
		lic, err = m.o.Service.License(ctx, creds, asin)
		return err
	})
	return lic, err
}

// Download fetches a license's URL to dest with account id's signature
// (audible.Client.Download: resumable, capped).
func (m *Manager) Download(ctx context.Context, id, url, dest string, opts audible.DownloadOptions) error {
	creds, err := m.open(id)
	if err != nil {
		return err
	}
	return m.o.Service.Download(ctx, creds, url, dest, opts)
}

// Positions reads the places Audible heard for asins with account id's
// sign-in (Phase 4c). A refusal is answered as it came and never recorded: the
// position sync's errors don't refuse the account (its caller skips a refused
// one).
func (m *Manager) Positions(ctx context.Context, id string, asins []string) (map[string]audible.Position, error) {
	creds, err := m.open(id)
	if err != nil {
		return nil, err
	}
	return m.o.Service.LastPositions(ctx, creds, asins)
}

// ContentACR asks Audible for a title's content reference (what a place
// written back needs) with account id's sign-in. Never a refusal, as
// Positions.
func (m *Manager) ContentACR(ctx context.Context, id, asin string) (string, error) {
	creds, err := m.open(id)
	if err != nil {
		return "", err
	}
	return m.o.Service.ContentACR(ctx, creds, asin)
}

// SetPosition writes a place (milliseconds) back to Audible with account id's
// sign-in. Never a refusal, as Positions.
func (m *Manager) SetPosition(ctx context.Context, id, asin, acr string, positionMs int64) error {
	creds, err := m.open(id)
	if err != nil {
		return err
	}
	return m.o.Service.SetLastPosition(ctx, creds, asin, acr, positionMs)
}

// open opens account id's sealed sign-in, saying what is wrong in words.
func (m *Manager) open(id string) (audible.Credentials, error) {
	creds, err := m.credentials(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return audible.Credentials{}, fmt.Errorf("the account's sign-in is missing on this Bridge: sign in again")
		}
		return audible.Credentials{}, fmt.Errorf("the account's sign-in can't be opened: %w", err)
	}
	return creds, nil
}

// signed runs one signed call with account id's sign-in. A refusal (ErrRefused
// from the Service, or a license denied to the device: audible.ErrThrottled)
// is tried once more with a newer sign-in when one replaced the device
// meanwhile, else it marks the account refused (a sign-in refusal with
// RefusedText, or a device one with ThrottledText), runs OnChange and answers
// ErrRefused, or the throttled denial as it came. A call Audible answers ends
// a sign-in refusal recorded before (a transient 401/403), never a device's
// (only a sign-in again ends it).
func (m *Manager) signed(ctx context.Context, id string, call func(audible.Credentials) error) error {
	creds, err := m.open(id)
	if err != nil {
		return err
	}
	err = call(creds)
	if refusal(err) {
		// A sign-in that finished meanwhile deregistered the device this call
		// used: call again with the new one rather than mark it refused.
		if fresh, ferr := m.credentials(id); ferr == nil && fresh.DeviceSerial != creds.DeviceSerial {
			creds = fresh
			err = call(fresh)
		}
	}
	if !refusal(err) {
		if err == nil {
			m.accepted(ctx, id)
		}
		return err
	}
	throttled := errors.Is(err, audible.ErrThrottled)
	kind, why, msg := store.RefusalSignIn, RefusedText, "audible: Audible refused the account's sign-in"
	if throttled {
		kind, why, msg = store.RefusalDevice, ThrottledText, "audible: Audible refused the account's device a license: it must sign in again"
	}
	// Under finishMu (held by a sign-in from its lookup until its row is
	// stored), and only while the refused device is still the account's: a
	// sign-in again that stored a new device after the check above must not
	// be marked refused for the old one (a ThrottledText refusal outlives polls).
	m.finishMu.Lock()
	cur, cerr := m.credentials(id)
	stale := cerr == nil && cur.DeviceSerial != creds.DeviceSerial
	if !stale {
		if rerr := m.o.DB.RefuseAudibleAccount(context.WithoutCancel(ctx), id, kind, why, m.o.Now()); rerr != nil {
			m.o.Log.Warn("audible: recording a refusal failed", "account", id, "err", rerr)
		}
	}
	m.finishMu.Unlock()
	m.o.Log.Warn(msg, "account", id, "err", err, "replaced", stale)
	if !stale {
		m.o.OnChange()
	}
	if throttled {
		return err
	}
	return ErrRefused
}

// accepted ends account id's sign-in refusal (store.RefusalSignIn) after Audible
// answered a signed call, running OnChange when it ended one.
func (m *Manager) accepted(ctx context.Context, id string) {
	ended, err := m.o.DB.AudibleAccepted(context.WithoutCancel(ctx), id, store.RefusalSignIn)
	if err != nil {
		m.o.Log.Warn("audible: recording an accepted sign-in failed", "account", id, "err", err)
		return
	}
	if ended {
		m.o.Log.Info("audible: Audible accepts the account's sign-in again", "account", id)
		m.o.OnChange()
	}
}

// refusal reports whether err from a signed call refuses the account's device:
// its sign-in (ErrRefused) or its licenses (audible.ErrThrottled).
func refusal(err error) bool {
	return errors.Is(err, ErrRefused) || errors.Is(err, audible.ErrThrottled)
}

// Polled records a complete poll of account id.
func (m *Manager) Polled(ctx context.Context, id string, titles, matched int) error {
	return m.o.DB.AudiblePolled(ctx, id, titles, matched, m.o.Now())
}

// Failed records why account id's poll failed (a refusal is recorded by
// Library already).
func (m *Manager) Failed(ctx context.Context, id, why string) error {
	return m.o.DB.AudibleAccountError(ctx, id, why)
}

// customerHash is the sha256 of an Amazon customer id ("" for none).
func customerHash(customerID string) string {
	if customerID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("audiosilo-bridge/audible-customer/" + customerID))
	return hex.EncodeToString(sum[:])
}

// randomID is a new opaque id: 16 lower-case hex characters.
func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
