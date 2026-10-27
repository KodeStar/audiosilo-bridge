// Package api is the HTTP transport of the Bridge's own page (BRIDGE-PLAN.md
// Phase 0): login, pairing, the library mappings, settings, the jobs it ran
// and its recent log. It is transport only: every handler validates and calls
// the Backend (internal/app), or the injected auth.Manager for the login, the
// sessions and the password.
//
// The page answers loopback and private (LAN) addresses only, and always asks
// for its password, loopback included: an agent container sharing the Bridge's
// network could otherwise reach it (spike 2).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/auth"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/logring"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/workerdist"
)

// Status is the Bridge's state (GET /api/status).
type Status struct {
	Version string `json:"version"`
	Pairing any    `json:"pairing"`
	// Online: the last heartbeat went through less than two minutes ago.
	Online        bool        `json:"online"`
	LastHeartbeat *time.Time  `json:"last_heartbeat"`
	LastError     string      `json:"last_error"`
	Kinds         []string    `json:"kinds"`
	Lanes         []jobs.Lane `json:"lanes"`
	// SecretKey says where the master key came from (environment, key file).
	SecretKey string `json:"secret_key"`
	PublicURL string `json:"public_url"`
	// ServerURLDefault prefills the pairing form (config server.url).
	ServerURLDefault string `json:"server_url_default"`
	// Setup is the setup checklist (Phase 2b), the one the heartbeat carries;
	// Complete: every required item is done (the Status tab's "Get set up" card
	// shows until then).
	Setup                link.Setup `json:"setup"`
	SetupComplete        bool       `json:"setup_complete"`
	PodcastAllowNetworks []string   `json:"podcast_allow_networks"`
	// WorkerDownloads: this Bridge serves install.sh and the worker builds (the
	// image carries them; a native run doesn't). Set by the API itself.
	WorkerDownloads bool `json:"worker_downloads"`
}

// Library is a server library with the Bridge's mapping and its last check.
type Library struct {
	mapping.Entry
	// Locked: AUDIOSILO_BRIDGE_LIBRARIES sets the mappings.
	Locked bool `json:"locked"`
}

// Settings are the page's editable settings; Locked names the ones the
// environment sets.
type Settings struct {
	PublicURL           string `json:"public_url"`
	MappingCheckMinutes int    `json:"mapping_check_minutes"`
	// ASRWorkerFallback is asr.worker_fallback: local or wait ("" on a save
	// keeps it).
	ASRWorkerFallback string `json:"asr_worker_fallback"`
	// MonthlyBudgetUSD is agent.monthly_budget_usd (0 = no cap; nil on a save
	// keeps it). Saving it marks the budget reviewed (the setup checklist's
	// budget item); BudgetReviewed says whether it is.
	MonthlyBudgetUSD *float64 `json:"monthly_budget_usd"`
	BudgetReviewed   bool     `json:"budget_reviewed"`
	Locked           []string `json:"locked"`
}

// Worker is a paired ASR worker (GET /api/workers): what it last said, what it
// did, and whether it is connected now and on which segment.
type Worker struct {
	store.Worker
	Connected bool   `json:"connected"`
	Task      string `json:"task"`
}

// NewWorker is a worker just paired: its token is shown this once.
type NewWorker struct {
	Worker Worker `json:"worker"`
	Token  string `json:"token"`
}

// Engine is the sidecars engine's state for the page (GET /api/engine): the
// agent container, the speech recognition, the budgets and this month's spend,
// and the transcript retention.
type Engine struct {
	// Started: the engine is up (its first start may be downloading ffmpeg).
	Started bool        `json:"started"`
	Agent   EngineAgent `json:"agent"`
	ASR     EngineASR   `json:"asr"`
	// MonthlyBudgetUSD is the monthly agent cap (0 = none); MonthSpentUSD this
	// calendar month's spend (UTC).
	MonthlyBudgetUSD float64 `json:"monthly_budget_usd"`
	MonthSpentUSD    float64 `json:"month_spent_usd"`
	BookBudgetUSD    float64 `json:"book_budget_usd"`
	// TranscriptRetentionDays: a finished book's transcripts are deleted that
	// long after its job ended (0 = when it ends).
	TranscriptRetentionDays int `json:"transcript_retention_days"`
}

// EngineAgent is the agent container as the Bridge last saw it.
type EngineAgent struct {
	Backend string `json:"backend"`
	// Available: the container answers and the backend's CLI has a credential.
	Available    bool                 `json:"available"`
	Detail       string               `json:"detail"`
	Version      string               `json:"version"`
	Slots        int                  `json:"slots"`
	HeartbeatAt  *time.Time           `json:"heartbeat_at"`
	CLIs         map[string]EngineCLI `json:"clis"`
	Subscription bool                 `json:"subscription"`
	StagingDir   string               `json:"staging_dir"`
}

// EngineCLI is one agent CLI in the container: its version and the kind of
// credential it has ("" for none), never the credential.
type EngineCLI struct {
	Version    string `json:"version"`
	Credential string `json:"credential"`
}

// EngineASR is where speech recognition runs now: a connected worker
// (backend "worker", device "remote"), else the Bridge's own backend, with the
// fallback for when no worker is connected (local or wait).
type EngineASR struct {
	Backend        string `json:"backend"`
	Available      bool   `json:"available"`
	Device         string `json:"device"`
	Detail         string `json:"detail"`
	WorkerFallback string `json:"worker_fallback"`
	// Waiting: no segment can run now (no worker connected and the fallback is
	// wait, or nothing local to fall back on), so books wait for a worker.
	Waiting bool `json:"waiting"`
}

// Backend is what the page drives.
type Backend interface {
	Status() Status
	Pair(ctx context.Context, serverURL, code string) error
	Unpair(ctx context.Context) error
	Libraries() []Library
	SetMount(ctx context.Context, id int64, path string) error
	CheckNow(ctx context.Context)
	Settings() Settings
	SaveSettings(ctx context.Context, s Settings) error
	Jobs(ctx context.Context, before int64, limit int) ([]store.ServerJob, error)
	// Job is one job (ErrNotFound when the Bridge has no such job).
	Job(ctx context.Context, id int64) (*store.ServerJob, error)
	CancelJob(id int64) bool
	// RetryJob retries a parked job's work (ErrNotFound: no such job here;
	// ErrConflict: it isn't parked).
	RetryJob(ctx context.Context, id int64) error
	Engine(ctx context.Context) Engine
	Logs(after uint64, limit int, level slog.Level) []logring.Entry
	Workers(ctx context.Context) ([]Worker, error)
	// CreateWorker pairs a worker (ErrInvalid: no name); RevokeWorker
	// deletes one, which revokes its token (ErrNotFound: no such worker).
	CreateWorker(ctx context.Context, name string) (*NewWorker, error)
	RevokeWorker(ctx context.Context, id int64) error
	AccountsBackend
}

// The Backend's error kinds, mapped to statuses.
var (
	ErrInvalid     = errors.New("invalid")                     // 400
	ErrRefused     = errors.New("refused")                     // 422: the server said no
	ErrNotFound    = errors.New("not found")                   // 404
	ErrConflict    = errors.New("conflict")                    // 409
	ErrLocked      = errors.New("set by the environment")      // 409, code locked
	ErrUnreachable = errors.New("the server can't be reached") // 502
)

// kindError is a Backend error of one of the kinds above whose message is the
// sentence the page shows (no kind prefix).
type kindError struct {
	kind error
	msg  string
}

func (e *kindError) Error() string        { return e.msg }
func (e *kindError) Is(target error) bool { return target == e.kind }

// Errorf makes a Backend error of kind (ErrInvalid, ...) whose message is the
// formatted sentence alone.
func Errorf(kind error, format string, args ...any) error {
	return &kindError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// API serves the page and the worker routes.
type API struct {
	b       Backend
	auth    *auth.Manager
	workers WorkerHub
	web     http.Handler
	log     *slog.Logger
	limiter *auth.RateLimiter
	// dist is the worker builds' folder (workerdist.DefaultDir).
	dist workerdist.Dist
}

// New returns the API over b, with hub behind the worker routes (nil: none)
// and web serving the page's files.
func New(b Backend, am *auth.Manager, hub WorkerHub, web http.Handler, log *slog.Logger) *API {
	return &API{b: b, auth: am, workers: hub, web: web, log: log, limiter: auth.NewRateLimiter(10, 0.2),
		dist: workerdist.Dist{Dir: workerdist.DefaultDir}}
}

// Handler is the whole site: the API under /api, the page everywhere else,
// behind the LAN guard.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /api/login", a.handleLogin)
	mux.HandleFunc("POST /api/logout", a.requireAuth(a.handleLogout))
	mux.HandleFunc("POST /api/password", a.requireAuth(a.handlePassword))
	mux.HandleFunc("GET /api/status", a.requireAuth(a.handleStatus))
	mux.HandleFunc("POST /api/pair", a.requireAuth(a.handlePair))
	mux.HandleFunc("POST /api/unpair", a.requireAuth(a.handleUnpair))
	mux.HandleFunc("GET /api/libraries", a.requireAuth(a.handleLibraries))
	mux.HandleFunc("PUT /api/libraries/{id}", a.requireAuth(a.handleSetMount))
	mux.HandleFunc("POST /api/libraries/check", a.requireAuth(a.handleCheck))
	mux.HandleFunc("GET /api/settings", a.requireAuth(a.handleSettings))
	mux.HandleFunc("PUT /api/settings", a.requireAuth(a.handleSaveSettings))
	mux.HandleFunc("GET /api/jobs", a.requireAuth(a.handleJobs))
	mux.HandleFunc("GET /api/jobs/{id}", a.requireAuth(a.handleJob))
	mux.HandleFunc("POST /api/jobs/{id}/cancel", a.requireAuth(a.handleCancel))
	mux.HandleFunc("POST /api/jobs/{id}/retry", a.requireAuth(a.handleRetry))
	mux.HandleFunc("GET /api/engine", a.requireAuth(a.handleEngine))
	mux.HandleFunc("GET /api/logs", a.requireAuth(a.handleLogs))
	mux.HandleFunc("GET /api/workers", a.requireAuth(a.handleWorkers))
	mux.HandleFunc("POST /api/workers", a.requireAuth(a.handleCreateWorker))
	mux.HandleFunc("DELETE /api/workers/{id}", a.requireAuth(a.handleRevokeWorker))
	a.accountRoutes(mux)
	a.workerRoutes(mux)
	a.workerDistRoutes(mux)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeError(w, http.StatusNotFound, "not found") })
	mux.Handle("/", a.web)
	return securityHeaders(lanOnly(mux))
}

// --- middleware ---

// lanOnly refuses a request whose connection doesn't come from a loopback or
// private address. RemoteAddr only: the page isn't meant to sit behind a
// proxy, and a forwarded header would let any client claim to be local. A
// refused install.sh is a script that says why and exits 1 (a `curl -fsSL`
// drops a 403's body, and sh would exit 0 on nothing); everything else is a
// JSON 403.
func lanOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !LocalAddress(r.RemoteAddr) {
			if isWorkerScript(r) {
				writeScript(w, workerdist.ErrorScript(errScriptLANOnly))
				return
			}
			writeErrorCode(w, http.StatusForbidden, "lan_only", "the Bridge's page answers only the local network")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LocalAddress reports whether a connection's remote address (host:port) is
// loopback, private (RFC 1918, ULA) or link-local.
func LocalAddress(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth accepts a live session token (Authorization: Bearer) only.
func (a *API) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, err := a.auth.Resolve(bearerToken(r))
		if err != nil {
			a.log.Warn("bridge page: auth check failed", "err", err)
			writeError(w, http.StatusInternalServerError, "auth error")
			return
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "sign in first")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- JSON ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeErrorCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// decodeJSON decodes a small JSON body strictly; false (with a 400 written)
// when it doesn't.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// writeBackendError maps a Backend error; the message of a typed one is the
// Backend's sentence, anything else a 500 that is logged, never echoed.
func (a *API) writeBackendError(w http.ResponseWriter, err error, op string) {
	msg := err.Error()
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, msg)
	case errors.Is(err, ErrRefused):
		writeError(w, http.StatusUnprocessableEntity, msg)
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, msg)
	case errors.Is(err, ErrLocked):
		writeErrorCode(w, http.StatusConflict, "locked", msg)
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, msg)
	case errors.Is(err, ErrUnreachable):
		writeError(w, http.StatusBadGateway, msg)
	default:
		a.log.Warn("bridge page: "+op+" failed", "err", err)
		writeError(w, http.StatusInternalServerError, op+" failed")
	}
}

// --- handlers ---

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, slow down")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	token, err := a.auth.Login(req.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidCreds), errors.Is(err, auth.ErrNoAdmin):
		writeError(w, http.StatusUnauthorized, "wrong password")
	case err != nil:
		a.writeBackendError(w, err, "sign in")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"token": token})
	}
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := a.auth.Logout(bearerToken(r)); err != nil {
		a.writeBackendError(w, err, "sign out")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	err := a.auth.ChangePassword(req.Current, req.New, bearerToken(r))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, auth.ErrInvalidCreds):
		// 403, not 401: the session is fine, and a 401 signs the page out.
		writeError(w, http.StatusForbidden, "the current password is wrong")
	case errors.Is(err, auth.ErrPasswordReplaced):
		// The current password was right, but a reset (or another change)
		// replaced it while this change ran, and nothing was changed.
		writeError(w, http.StatusConflict, "the password was changed or reset meanwhile: sign in with the new one")
	case errors.Is(err, auth.ErrPasswordTooShort):
		writeError(w, http.StatusBadRequest, "the new password needs at least "+strconv.Itoa(auth.MinPasswordLen)+" characters")
	default:
		a.writeBackendError(w, err, "change the password")
	}
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.status())
}

// status is the Backend's status with what the API itself knows.
func (a *API) status() Status {
	st := a.b.Status()
	st.WorkerDownloads = a.dist.Available()
	return st
}

func (a *API) handlePair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServerURL string `json:"server_url"`
		Code      string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ServerURL) == "" || strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "the server address and the code are both needed")
		return
	}
	if err := a.b.Pair(r.Context(), strings.TrimSpace(req.ServerURL), req.Code); err != nil {
		a.writeBackendError(w, err, "pair")
		return
	}
	writeJSON(w, http.StatusOK, a.status())
}

func (a *API) handleUnpair(w http.ResponseWriter, r *http.Request) {
	if err := a.b.Unpair(r.Context()); err != nil {
		a.writeBackendError(w, err, "unpair")
		return
	}
	writeJSON(w, http.StatusOK, a.status())
}

func (a *API) handleLibraries(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"libraries": a.b.Libraries()})
}

func (a *API) handleSetMount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid library id")
		return
	}
	var req struct {
		MountPath string `json:"mount_path"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := a.b.SetMount(r.Context(), id, strings.TrimSpace(req.MountPath)); err != nil {
		a.writeBackendError(w, err, "save the mapping")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": a.b.Libraries()})
}

func (a *API) handleCheck(w http.ResponseWriter, r *http.Request) {
	a.b.CheckNow(r.Context())
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.b.Settings())
}

func (a *API) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicURL           string   `json:"public_url"`
		MappingCheckMinutes int      `json:"mapping_check_minutes"`
		ASRWorkerFallback   string   `json:"asr_worker_fallback"`
		MonthlyBudgetUSD    *float64 `json:"monthly_budget_usd"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	s := Settings{PublicURL: strings.TrimSpace(req.PublicURL), MappingCheckMinutes: req.MappingCheckMinutes,
		ASRWorkerFallback: strings.TrimSpace(req.ASRWorkerFallback), MonthlyBudgetUSD: req.MonthlyBudgetUSD}
	if err := a.b.SaveSettings(r.Context(), s); err != nil {
		a.writeBackendError(w, err, "save the settings")
		return
	}
	writeJSON(w, http.StatusOK, a.b.Settings())
}

func (a *API) handleJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	list, err := a.b.Jobs(r.Context(), before, limit)
	if err != nil {
		a.writeBackendError(w, err, "list the jobs")
		return
	}
	out := map[string]any{"jobs": list}
	if len(list) == limit {
		out["next_before"] = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

// jobID reads the {id} of a job route (false, with a 400 written, when it
// isn't one).
func jobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return 0, false
	}
	return id, true
}

func (a *API) handleJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	job, err := a.b.Job(r.Context(), id)
	if err != nil {
		a.writeBackendError(w, err, "read the job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (a *API) handleRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	if err := a.b.RetryJob(r.Context(), id); err != nil {
		a.writeBackendError(w, err, "retry the job")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) handleEngine(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.b.Engine(r.Context()))
}

func (a *API) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	if !a.b.CancelJob(id) {
		writeError(w, http.StatusNotFound, "that job isn't running here")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	level := slog.LevelInfo
	if v := q.Get("level"); v != "" {
		if err := level.UnmarshalText([]byte(v)); err != nil {
			writeError(w, http.StatusBadRequest, "invalid level")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": a.b.Logs(after, limit, level)})
}

func (a *API) handleWorkers(w http.ResponseWriter, r *http.Request) {
	ws, err := a.b.Workers(r.Context())
	if err != nil {
		a.writeBackendError(w, err, "list the workers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": ws})
}

func (a *API) handleCreateWorker(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	nw, err := a.b.CreateWorker(r.Context(), req.Name)
	if err != nil {
		a.writeBackendError(w, err, "add the worker")
		return
	}
	writeJSON(w, http.StatusCreated, nw)
}

func (a *API) handleRevokeWorker(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid worker id")
		return
	}
	if err := a.b.RevokeWorker(r.Context(), id); err != nil {
		a.writeBackendError(w, err, "remove the worker")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
