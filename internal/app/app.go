// Package app wires the Bridge (BRIDGE-PLAN.md Phase 0): config, store, the
// sealed secrets, the pairing and server link, the heartbeat that carries the
// library mapping checks, the job runner with its handlers, and the Bridge's
// own page. It is the backend the page's API (internal/api) calls.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/auth"
	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/logring"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/verify"
	"github.com/kodestar/audiosilo-bridge/internal/web"
)

// HeartbeatEvery is how often the Bridge reports to the server; the server
// counts it online for two minutes after a report.
const HeartbeatEvery = 30 * time.Second

// Options start the Bridge.
type Options struct {
	DataDir string
	Version string
	// Listen overrides config's listen ("" keeps it).
	Listen string
	// Logger, wrapped so the page can show recent lines.
	Logger *slog.Logger
	// Ring keeps the recent lines (logring.Handler around Logger's handler).
	Ring *logring.Ring
}

// Bridge is the running Bridge.
type Bridge struct {
	dataDir string
	version string
	log     *slog.Logger
	ring    *logring.Ring
	cfg     atomic.Pointer[config.Config]
	db      *store.DB
	keySrc  secrets.KeySource
	conn    *Conn
	libs    *mapping.Registry
	runner  *jobs.Runner
	auth    *auth.Manager

	kick chan struct{}

	mu       sync.Mutex
	lastBeat time.Time
	beatErr  string
	// beatGen counts pairings, so a heartbeat answer from before an unpair or
	// a new pairing isn't recorded as the current one.
	beatGen   int
	sawFirst  bool
	checkLock sync.Mutex
}

// New opens the Bridge's state in opts.DataDir.
func New(ctx context.Context, opts Options) (*Bridge, string, error) {
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create data dir: %w", err)
	}
	cfg, err := config.Load(opts.DataDir)
	if err != nil {
		return nil, "", err
	}
	if opts.Listen != "" {
		cfg.Listen = opts.Listen
	}
	for _, d := range cfg.Deprecations() {
		opts.Logger.Warn("config: " + d)
	}
	db, err := store.Open(ctx, filepath.Join(opts.DataDir, store.FileName))
	if err != nil {
		return nil, "", err
	}
	vault, keySrc, err := secrets.Open(opts.DataDir, os.Getenv(secrets.KeyEnv))
	if err != nil {
		_ = db.Close()
		return nil, "", err
	}
	conn, err := openConn(ctx, db, vault, opts.Version, opts.Logger)
	if err != nil {
		_ = db.Close()
		if errors.Is(err, secrets.ErrWrongKey) {
			return nil, "", fmt.Errorf("the stored server token can't be opened: %s changed or %s was replaced (%w)",
				secrets.KeyEnv, secrets.KeyFileName, err)
		}
		return nil, "", err
	}
	am := auth.New(db.AuthStore())
	oneTime, err := am.EnsureAdmin()
	if err != nil {
		_ = db.Close()
		return nil, "", err
	}
	b := &Bridge{dataDir: opts.DataDir, version: opts.Version, log: opts.Logger, ring: opts.Ring, db: db, keySrc: keySrc,
		conn: conn, libs: mapping.NewRegistry(), auth: am, kick: make(chan struct{}, 1)}
	b.cfg.Store(&cfg)
	verifier := verify.New(b.libs, b.mount, func() mapping.BookSource {
		if cl := b.conn.Client(); cl != nil {
			return cl
		}
		return nil
	})
	b.runner = jobs.New(db, b.server, b.tokenRefused, jobs.Options{Log: opts.Logger}, verifier)
	conn.onChange = b.linkChanged
	return b, oneTime, nil
}

// config is the current configuration.
func (b *Bridge) config() config.Config { return *b.cfg.Load() }

// mount says where the Bridge sees a library (the current config's mapping).
func (b *Bridge) mount(id int64, root string) (string, bool) { return b.config().MountFor(id, root) }

// server is the runner's view of the link (nil while unpaired).
func (b *Bridge) server() jobs.Server {
	if cl := b.conn.Client(); cl != nil {
		return cl
	}
	return nil
}

// tokenRefused handles a 401 from the server: the token was revoked.
func (b *Bridge) tokenRefused() {
	if b.conn.Client() == nil {
		return
	}
	if err := b.conn.Unpair(context.Background(), "the server refused the Bridge's token: it was unpaired in the server's console, another Bridge was paired, or the admin who paired it is no longer an admin"); err != nil {
		b.log.Warn("bridge: forgetting the token failed", "err", err)
	}
}

// linkChanged runs after a pair or an unpair: a new pairing beats at once (so
// the console sees it and the mappings are checked), and the runner looks
// again.
func (b *Bridge) linkChanged() {
	b.mu.Lock()
	b.beatGen++
	b.sawFirst = false
	b.lastBeat, b.beatErr = time.Time{}, ""
	b.mu.Unlock()
	b.libs.SetLibraries(nil)
	b.Kick()
	b.runner.Wake()
}

// Kick asks for a heartbeat now.
func (b *Bridge) Kick() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Run serves the page and runs the heartbeat and the runner until ctx ends.
func (b *Bridge) Run(ctx context.Context) error {
	defer func() { _ = b.db.Close() }()
	cfg := b.config()
	// Listen before pairing: a start that can't serve must not use up the code.
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	if code := strings.TrimSpace(os.Getenv("AUDIOSILO_BRIDGE_PAIR_CODE")); code != "" && b.conn.Client() == nil {
		if cfg.Server.URL == "" {
			b.log.Warn("bridge: AUDIOSILO_BRIDGE_PAIR_CODE is set without AUDIOSILO_BRIDGE_SERVER_URL; pair from the Bridge's page")
		} else if err := b.conn.Pair(ctx, cfg.Server.URL, code); err != nil {
			b.log.Warn("bridge: pairing with the code from the environment failed", "server", cfg.Server.URL, "err", err)
		}
	}
	srv := &http.Server{
		Handler:           api.New(b, b.auth, web.New(), b.log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	b.log.Info("bridge: started", "version", b.version, "page", "http://"+ln.Addr().String(), "data", b.dataDir,
		"secret_key", string(b.keySrc), "paired", b.conn.Client() != nil)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); b.heartbeatLoop(ctx) }()
	go func() { defer wg.Done(); b.runner.Run(ctx) }()
	serveErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
	wg.Wait()
	b.log.Info("bridge: stopped")
	return err
}

// heartbeatLoop reports every HeartbeatEvery, and at once when kicked.
func (b *Bridge) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	for {
		b.beat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.kick:
		}
	}
}

// beat sends one heartbeat with the last mapping checks, records the server's
// libraries, and starts a mapping check when one is due or the libraries
// changed.
func (b *Bridge) beat(ctx context.Context) {
	cl := b.conn.Client()
	if cl == nil {
		return
	}
	b.mu.Lock()
	gen := b.beatGen
	b.mu.Unlock()
	cfg := b.config()
	st := link.Status{Version: b.version, Kinds: b.runner.Kinds(), Libraries: b.libs.Statuses(), Workers: []link.Worker{}, UIURL: cfg.PublicURL}
	reply, err := cl.Heartbeat(ctx, st)
	if ctx.Err() != nil {
		return
	}
	b.mu.Lock()
	if gen != b.beatGen {
		b.mu.Unlock()
		return
	}
	first := false
	if err == nil {
		b.lastBeat, b.beatErr = time.Now(), ""
		first, b.sawFirst = !b.sawFirst, true
	} else {
		b.beatErr = err.Error()
	}
	b.mu.Unlock()
	switch {
	case errors.Is(err, link.ErrUnauthorized):
		b.tokenRefused()
		return
	case err != nil:
		b.log.Warn("bridge: heartbeat failed", "err", err)
		return
	}
	if first {
		b.runner.Wake()
	}
	changed := b.libs.SetLibraries(reply.Libraries)
	interval := time.Duration(cfg.Mapping.CheckMinutes) * time.Minute
	if changed || b.libs.Due(time.Now(), interval) {
		b.CheckMappings(ctx)
	}
}

// CheckMappings checks every library's mapping in the background (one check at
// a time; a check already running is enough) and beats once it is done.
func (b *Bridge) CheckMappings(ctx context.Context) {
	cl := b.conn.Client()
	if cl == nil || !b.checkLock.TryLock() {
		return
	}
	go func() {
		defer b.checkLock.Unlock()
		b.libs.CheckAll(ctx, cl, b.mount, time.Now)
		b.Kick()
	}()
}
