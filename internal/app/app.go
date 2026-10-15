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

	"github.com/kodestar/audiosilo-bridge/internal/agentbox"
	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/auth"
	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/generate"
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
	vault   secrets.Store
	conn    *Conn
	libs    *mapping.Registry
	runner  *jobs.Runner
	auth    *auth.Manager
	// box is the agent container runner (decision 15), gen the sidecars.generate
	// job, engine the sidecars engine once it has started.
	box    *agentbox.Runner
	gen    *generate.Handler
	engine engineState

	kick chan struct{}

	// saveMu serializes the page's config edits: each is a load, an edit and a
	// write of config.yaml, so two at once would lose one.
	saveMu sync.Mutex

	mu       sync.Mutex
	lastBeat time.Time
	beatErr  string
	// beatGen counts pairings, so a heartbeat answer from before an unpair or
	// a new pairing isn't recorded as the current one.
	beatGen int
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
		vault: vault, conn: conn, libs: mapping.NewRegistry(), auth: am, kick: make(chan struct{}, 1),
		box: newAgentBox(cfg, opts.DataDir, opts.Logger)}
	b.cfg.Store(&cfg)
	verifier := verify.New(b.libs, b.mount, func() mapping.BookSource {
		if cl := b.conn.Client(); cl != nil {
			return cl
		}
		return nil
	})
	b.gen = generate.New(generate.Options{DB: db, Engine: b.engine.generateEngine, Libraries: b.libs, Mount: b.mount,
		Server: func() generate.Server {
			if cl := b.conn.Client(); cl != nil {
				return cl
			}
			return nil
		},
		WorkRoot: filepath.Join(opts.DataDir, "work"), Version: opts.Version, RetentionDays: cfg.Transcripts.RetentionDays,
		Log: opts.Logger})
	b.runner = jobs.New(db, b.server, b.tokenRefused, jobs.Options{Log: opts.Logger,
		ServerID: func() string { return b.conn.Pairing().ServerID }}, verifier, b.gen)
	conn.onChange = b.linkChanged
	return b, oneTime, nil
}

// config is the current configuration.
func (b *Bridge) config() config.Config { return *b.cfg.Load() }

// mount says where the Bridge sees a library (the current config's mapping).
func (b *Bridge) mount(id int64, root string) (string, bool) { return b.config().MountFor(id, root) }

// server is the runner's view of the link: nil while unpaired or refused, and
// until a heartbeat on the current link has gone through, so no job (a resumed
// one included) runs before the server's libraries are known.
func (b *Bridge) server() jobs.Server {
	b.mu.Lock()
	ready := !b.lastBeat.IsZero()
	b.mu.Unlock()
	if !ready {
		return nil
	}
	if cl := b.conn.Client(); cl != nil {
		return cl
	}
	return nil
}

// refusedReason is what the page says while the server refuses the token.
const refusedReason = "the server refused the Bridge's token. If it was unpaired in the server's console or " +
	"another Bridge was paired, pair it again; if the admin who paired it lost the role for a while, it " +
	"comes back by itself (it tries again every 5 minutes)"

// tokenRefused handles a 401 the runner got on srv: only a refusal of the
// current link counts, never one of a link a re-pair has replaced (a job of
// the previous pairing still reporting with its revoked token).
func (b *Bridge) tokenRefused(srv jobs.Server) {
	if cl, ok := srv.(*link.Client); ok {
		b.conn.Refuse(context.Background(), cl, refusedReason)
	}
}

// linkChanged runs after a pair or an unpair, and when the server refuses the
// token or takes it again: a new link beats at once (so the console sees it and
// the mappings are checked), and the runner looks again once that heartbeat
// has listed the libraries (server).
func (b *Bridge) linkChanged() {
	b.mu.Lock()
	b.beatGen++
	b.lastBeat, b.beatErr = time.Time{}, ""
	b.libs.SetLibraries(nil)
	b.mu.Unlock()
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

// listen opens the page's listener (a test swaps in one that fails).
var listen = func(ctx context.Context, addr string) (net.Listener, error) {
	return (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
}

// Run serves the page and runs the heartbeat and the runner until ctx ends,
// or the page's server fails (its error is returned once everything stopped).
func (b *Bridge) Run(ctx context.Context) error {
	defer func() { _ = b.db.Close() }()
	// The heartbeat and the runner stop with ctx: a failing page server must
	// stop them too, or Run would wait for them for ever.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	cfg := b.config()
	// Listen before pairing: a start that can't serve must not use up the code.
	ln, err := listen(ctx, cfg.Listen)
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
		"key_source", string(b.keySrc), "paired", b.conn.Client() != nil)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); b.runEngine(ctx) }()
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
		b.log.Error("bridge: the page's server failed", "err", err)
	}
	stop()
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
		b.retryRefused(ctx)
		return
	}
	b.mu.Lock()
	gen := b.beatGen
	b.mu.Unlock()
	cfg := b.config()
	st := link.Status{Version: b.version, Kinds: b.runner.Kinds(), Libraries: b.libs.Statuses(), Workers: []link.Worker{},
		UIURL: cfg.PublicURL, Estimates: b.estimates(ctx)}
	reply, err := cl.Heartbeat(ctx, st)
	if ctx.Err() != nil {
		return
	}
	b.mu.Lock()
	if gen != b.beatGen {
		b.mu.Unlock()
		return
	}
	first, changed := false, false
	if err == nil {
		// The libraries are recorded before the link counts as ready (server),
		// and under the lock, so a reply from before a pair or an unpair can't
		// land after it.
		changed = b.libs.SetLibraries(reply.Libraries)
		first = b.lastBeat.IsZero()
		b.lastBeat, b.beatErr = time.Now(), ""
	} else {
		b.beatErr = err.Error()
	}
	b.mu.Unlock()
	switch {
	case errors.Is(err, link.ErrUnauthorized):
		b.conn.Refuse(ctx, cl, refusedReason)
		return
	case err != nil:
		b.log.Warn("bridge: heartbeat failed", "err", err)
		return
	}
	if first {
		b.runner.Wake()
	}
	interval := time.Duration(cfg.Mapping.CheckMinutes) * time.Minute
	if changed || b.libs.Due(time.Now(), interval) {
		b.CheckMappings(ctx)
	}
}

// retryRefused tries a refused token again when its retry is due: a heartbeat
// that goes through ends the refusal.
func (b *Bridge) retryRefused(ctx context.Context) {
	cl := b.conn.retryClient()
	if cl == nil {
		return
	}
	cfg := b.config()
	_, err := cl.Heartbeat(ctx, link.Status{Version: b.version, Kinds: b.runner.Kinds(), Libraries: []link.LibraryStatus{},
		Workers: []link.Worker{}, UIURL: cfg.PublicURL})
	switch {
	case err == nil:
		b.conn.accept(ctx, cl)
	case errors.Is(err, link.ErrUnauthorized):
		b.conn.Refuse(ctx, cl, refusedReason) // still refused: the next retry in RefusedRetry
	default:
		b.log.Warn("bridge: retrying the refused token failed", "err", err)
	}
}

// CheckMappings checks the libraries ids (every library with none) in the
// background and beats once the check is done. A check asked for while one runs
// follows it (mapping.Registry.Check).
func (b *Bridge) CheckMappings(ctx context.Context, ids ...int64) {
	cl := b.conn.Client()
	if cl == nil {
		return
	}
	go func() {
		// A check queued behind a running one is run (and beaten) by that one:
		// beating here too would ask for it again while it is still due.
		if b.libs.Check(ctx, cl, b.mount, time.Now, ids...) {
			b.Kick()
		}
	}()
}
