package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"gopkg.in/yaml.v3"
)

// The Bridge's own settings: the server it pairs with, the address of its page,
// and where it sees each server library. The engine's settings (tools, asr,
// agent, supervisor, contribution) are config.go's.

// DefaultMappingCheckMinutes is how often the library mappings are checked: a
// stat of a known book's files and a probe write per library. Not every
// heartbeat (30 s): on an Unraid array that would keep spun-down disks awake.
const DefaultMappingCheckMinutes = 10

// MaxMappingCheckMinutes bounds mapping.check_minutes (a day).
const MaxMappingCheckMinutes = 24 * 60

// What a segment does when no ASR worker is connected (asr.worker_fallback,
// Phase 1b): the Bridge's own ASR, or a wait for a worker.
const (
	ASRWorkerFallbackLocal   = "local"
	ASRWorkerFallbackWait    = "wait"
	DefaultASRWorkerFallback = ASRWorkerFallbackLocal
)

// The agent container's settings (decision 15) and the transcript cache's
// (decision 5).
const (
	// DefaultStagingDirName is agent.staging_dir's default, under the data dir.
	DefaultStagingDirName = "staging"
	// DefaultClaimTimeoutSeconds is how long an agent job waits for the agent
	// container to pick it up before the book parks agent_unavailable.
	DefaultClaimTimeoutSeconds = 120
	MinClaimTimeoutSeconds     = 10
	MaxClaimTimeoutSeconds     = 60 * 60
	// DefaultMonthlyBudgetUSD is the monthly agent spend cap (decision 7: on by
	// default). 0 is no cap, so unlike book_budget_usd a 0 is kept as written.
	DefaultMonthlyBudgetUSD = 100.0
	// DefaultTranscriptRetentionDays keeps a finished book's transcripts this
	// long; 0 deletes them when the book's job ends.
	DefaultTranscriptRetentionDays = 30
	MaxTranscriptRetentionDays     = 10 * 365
)

// TranscriptsConfig is the transcript cache (decision 5).
type TranscriptsConfig struct {
	// RetentionDays keeps a finished book's transcripts in the data dir this many
	// days (0 = deleted when its job ends).
	RetentionDays int `yaml:"retention_days"`
}

// AgentStagingDir is the dir shared with the agent container: agent.staging_dir,
// else <dataDir>/staging. Not normalized into the config, so a saved config.yaml
// doesn't pin the data dir's path.
func (c Config) AgentStagingDir(dataDir string) string {
	if c.Agent.StagingDir != "" {
		return c.Agent.StagingDir
	}
	return filepath.Join(dataDir, DefaultStagingDirName)
}

// ServerConfig is the AudioSilo server the Bridge pairs with.
type ServerConfig struct {
	// URL is the server's address as the Bridge reaches it (an absolute http(s)
	// URL, e.g. http://audiosilo:8080 on a compose network): where pairing with
	// AUDIOSILO_BRIDGE_PAIR_CODE goes, and the page's pairing form's starting
	// value. A pairing keeps the address it used in the database.
	URL string `yaml:"url"`
}

// LibraryMapping is where the Bridge sees one server library's root.
type LibraryMapping struct {
	LibraryID int64  `yaml:"library_id" json:"library_id"`
	MountPath string `yaml:"mount_path" json:"mount_path"`
}

// MappingConfig controls the mapping checks.
type MappingConfig struct {
	// CheckMinutes is how often each mapping is checked (1 to a day).
	CheckMinutes int `yaml:"check_minutes"`
}

// The Bridge settings the environment can set, by the name EnvSet takes.
const (
	KeyServerURL     = "server.url"
	KeyPublicURL     = "public_url"
	KeyLibraries     = "libraries"
	KeyMappingCheck  = "mapping.check_minutes"
	envServerURL     = "AUDIOSILO_BRIDGE_SERVER_URL"
	envPublicURL     = "AUDIOSILO_BRIDGE_PUBLIC_URL"
	envLibraries     = "AUDIOSILO_BRIDGE_LIBRARIES"
	envMappingChecks = "AUDIOSILO_BRIDGE_MAPPING_CHECK_MINUTES"

	KeyAgentStagingDir      = "agent.staging_dir"
	KeyAgentClaimTimeout    = "agent.claim_timeout_seconds"
	KeyAgentMonthlyBudget   = "agent.monthly_budget_usd"
	KeyTranscriptsRetention = "transcripts.retention_days"
	KeyASRWorkerFallback    = "asr.worker_fallback"
	envAgentStagingDir      = "AUDIOSILO_BRIDGE_AGENT_STAGING_DIR"
	envAgentClaimTimeout    = "AUDIOSILO_BRIDGE_AGENT_CLAIM_TIMEOUT_SECONDS"
	envAgentMonthlyBudget   = "AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD"
	envTranscriptsRetention = "AUDIOSILO_BRIDGE_TRANSCRIPTS_RETENTION_DAYS"
	envASRWorkerFallback    = "AUDIOSILO_BRIDGE_ASR_WORKER_FALLBACK"
)

// EnvSet reports whether the environment set a Bridge setting (KeyServerURL,
// ...): the Bridge's page shows such a setting locked, since a value saved to
// config.yaml would be overridden again at the next start.
func (c Config) EnvSet(key string) bool { return c.fromEnv[key] }

// MountFor is where the Bridge sees a library: its mapping, else the server's
// own root path (a compose stack mounting the libraries at the same path in
// both containers needs no mapping). configured reports a mapping.
func (c Config) MountFor(libraryID int64, serverRoot string) (path string, configured bool) {
	for _, m := range c.Libraries {
		if m.LibraryID == libraryID {
			return m.MountPath, true
		}
	}
	return serverRoot, false
}

// applyBridgeEnv overlays the Bridge's AUDIOSILO_BRIDGE_* variables. A value
// that doesn't parse is kept as is for Validate to refuse, so a typo fails the
// start instead of being ignored. An empty variable counts as unset (compose's
// ${VAR} of an unset variable, a blank field of a container template): it
// neither locks the setting on the page nor fails the start.
func applyBridgeEnv(cfg *Config) {
	cfg.fromEnv = map[string]bool{}
	if v, ok := lookupEnv(envServerURL); ok {
		cfg.Server.URL = v
		cfg.fromEnv[KeyServerURL] = true
	}
	if v, ok := lookupEnv(envPublicURL); ok {
		cfg.PublicURL = v
		cfg.fromEnv[KeyPublicURL] = true
	}
	if v, ok := lookupEnv(envLibraries); ok {
		cfg.Libraries = parseLibraries(v)
		cfg.fromEnv[KeyLibraries] = true
	}
	if v, ok := lookupEnv(envMappingChecks); ok {
		cfg.Mapping.CheckMinutes = envInt(v)
		cfg.fromEnv[KeyMappingCheck] = true
	}
	if v, ok := lookupEnv(envAgentStagingDir); ok {
		cfg.Agent.StagingDir = v
		cfg.fromEnv[KeyAgentStagingDir] = true
	}
	if v, ok := lookupEnv(envAgentClaimTimeout); ok {
		cfg.Agent.ClaimTimeoutSeconds = envInt(v)
		cfg.fromEnv[KeyAgentClaimTimeout] = true
	}
	if v, ok := lookupEnv(envAgentMonthlyBudget); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			f = -1
		}
		cfg.Agent.MonthlyBudgetUSD = f
		cfg.fromEnv[KeyAgentMonthlyBudget] = true
	}
	if v, ok := lookupEnv(envTranscriptsRetention); ok {
		cfg.Transcripts.RetentionDays = envInt(v)
		cfg.fromEnv[KeyTranscriptsRetention] = true
	}
	if v, ok := lookupEnv(envASRWorkerFallback); ok {
		cfg.ASR.WorkerFallback = v
		cfg.fromEnv[KeyASRWorkerFallback] = true
	}
}

// envInt parses an integer variable; one that doesn't parse becomes -1, which
// every Validate rule it reaches refuses.
func envInt(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return -1
	}
	return n
}

// lookupEnv is a variable's trimmed value, ok only when it is set and not empty.
func lookupEnv(name string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	return v, v != ""
}

// parseLibraries reads AUDIOSILO_BRIDGE_LIBRARIES: "<id>=<path>" entries
// separated by ";" (a path may hold a comma). An entry that doesn't parse
// becomes id 0, which Validate refuses.
func parseLibraries(v string) []LibraryMapping {
	out := []LibraryMapping{}
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idText, path, _ := strings.Cut(part, "=")
		id, err := strconv.ParseInt(strings.TrimSpace(idText), 10, 64)
		if err != nil {
			id = 0
		}
		out = append(out, LibraryMapping{LibraryID: id, MountPath: strings.TrimSpace(path)})
	}
	return out
}

// normalizeBridge fills the Bridge settings an older or partial file leaves out.
func normalizeBridge(cfg *Config) {
	if cfg.Libraries == nil {
		cfg.Libraries = []LibraryMapping{}
	}
	if cfg.Mapping.CheckMinutes == 0 {
		cfg.Mapping.CheckMinutes = DefaultMappingCheckMinutes
	}
	if cfg.Agent.ClaimTimeoutSeconds == 0 {
		cfg.Agent.ClaimTimeoutSeconds = DefaultClaimTimeoutSeconds
	}
	cfg.ASR.WorkerFallback = strings.ToLower(strings.TrimSpace(cfg.ASR.WorkerFallback))
	if cfg.ASR.WorkerFallback == "" {
		cfg.ASR.WorkerFallback = DefaultASRWorkerFallback
	}
	if p := strings.TrimSpace(cfg.Agent.StagingDir); p != "" {
		cfg.Agent.StagingDir = filepath.Clean(p)
	}
	// Decision 15: the Bridge never runs an agent CLI itself, and a model-assisted
	// supervisor (or its automatic actions) or a backend failover would. They are
	// off whatever the file or the environment says, so what the config shows is
	// what runs.
	cfg.Supervisor.ModelAssisted, cfg.Supervisor.ModelAutomaticActions, cfg.Supervisor.AllowBackendFailover = false, false, false
	cfg.Server.URL = strings.TrimRight(strings.TrimSpace(cfg.Server.URL), "/")
	for i := range cfg.Libraries {
		if p := strings.TrimSpace(cfg.Libraries[i].MountPath); p != "" {
			cfg.Libraries[i].MountPath = filepath.Clean(p)
		}
	}
}

// ignoredOnBridge words a notice for each engine setting cfg sets to a
// non-default value that the Bridge doesn't use, so a setting carried over from
// sidecars doesn't silently do nothing. cfg is as loaded (file and environment),
// before normalizeBridge forces the supervisor's model lanes off. The agent
// container decides the agent concurrency (its --slots) and runs the CLIs
// (decision 15).
func ignoredOnBridge(cfg Config) []string {
	var out []string
	note := func(key, why string) { out = append(out, key+" is ignored on the Bridge: "+why) }
	const slots = "the agent container's --slots sets the agent concurrency"
	if capacity := cfg.Agent.Capacity(); capacity.Legacy {
		if cfg.Agent.Concurrency != DefaultConcurrency {
			note("agent.concurrency", slots)
		}
	} else {
		if cfg.Agent.QueueConcurrency != DefaultAgentQueueConcurrency {
			note("agent.queue_concurrency", slots)
		}
		if cfg.Agent.MaxAgentsPerBook != DefaultMaxAgentsPerBook {
			note("agent.max_agents_per_book", slots)
		}
	}
	const clis = "the agent container runs the CLIs"
	if strings.TrimSpace(cfg.Agent.ClaudePath) != "" {
		note("agent.claude_path", clis)
	}
	if strings.TrimSpace(cfg.Agent.CodexPath) != "" {
		note("agent.codex_path", clis)
	}
	const forcedOff = "it is always off, as it would run an agent CLI in the Bridge"
	sup := cfg.Supervisor
	for _, f := range []struct {
		key string
		on  bool
	}{
		{"supervisor.model_assisted", sup.ModelAssisted},
		{"supervisor.model_automatic_actions", sup.ModelAutomaticActions},
		{"supervisor.allow_backend_failover", sup.AllowBackendFailover},
	} {
		if f.on {
			note(f.key, forcedOff)
		}
	}
	// The settings only those lanes read (normalizeSupervisor has already
	// defaulted the zero values).
	const modelLane = "the supervisor's model-assisted lane and backend failover are always off"
	for _, f := range []struct {
		key string
		set bool
	}{
		{"supervisor.fallback_backend", strings.TrimSpace(sup.FallbackBackend) != ""},
		{"supervisor.fallback_model", strings.TrimSpace(sup.FallbackModel) != ""},
		{"supervisor.model_backend", strings.TrimSpace(sup.ModelBackend) != ""},
		{"supervisor.model", strings.TrimSpace(sup.Model) != ""},
		{"supervisor.max_model_calls", sup.MaxModelCalls != DefaultSupervisorMaxModelCalls},
		{"supervisor.max_turns", sup.MaxTurns != DefaultSupervisorMaxTurns},
		{"supervisor.timeout_seconds", sup.TimeoutSeconds != DefaultSupervisorTimeoutSeconds},
		{"supervisor.invocations_per_hour", sup.InvocationsPerHour != DefaultSupervisorCallsPerHour},
		{"supervisor.per_book_budget_usd", sup.PerBookBudgetUSD != DefaultSupervisorBookBudgetUSD},
		{"supervisor.overall_batch_budget_usd", sup.OverallBatchBudgetUSD != DefaultSupervisorBatchBudgetUSD},
	} {
		if f.set {
			note(f.key, modelLane)
		}
	}
	return out
}

// validateBridge checks the Bridge settings.
func (c Config) validateBridge() error {
	if c.Server.URL != "" {
		if err := checkHTTPURL(c.Server.URL); err != nil {
			return fmt.Errorf("server.url %q %w", c.Server.URL, err)
		}
	}
	if c.PublicURL != "" {
		if err := checkHTTPURL(c.PublicURL); err != nil {
			return fmt.Errorf("public_url %q %w", c.PublicURL, err)
		}
	}
	seen := map[int64]bool{}
	for _, m := range c.Libraries {
		if m.LibraryID <= 0 {
			return errors.New("libraries: each entry needs a library_id above 0 (the environment form is <id>=<path>;...)")
		}
		if seen[m.LibraryID] {
			return fmt.Errorf("libraries: library %d is mapped twice", m.LibraryID)
		}
		seen[m.LibraryID] = true
		if !filepath.IsAbs(m.MountPath) {
			return fmt.Errorf("libraries: library %d's mount_path %q must be an absolute path", m.LibraryID, m.MountPath)
		}
	}
	if c.Mapping.CheckMinutes < 1 || c.Mapping.CheckMinutes > MaxMappingCheckMinutes {
		return fmt.Errorf("mapping.check_minutes must be between 1 and %d, got %d", MaxMappingCheckMinutes, c.Mapping.CheckMinutes)
	}
	if c.Agent.StagingDir != "" && !filepath.IsAbs(c.Agent.StagingDir) {
		return fmt.Errorf("agent.staging_dir %q must be an absolute path", c.Agent.StagingDir)
	}
	if t := c.Agent.ClaimTimeoutSeconds; t < MinClaimTimeoutSeconds || t > MaxClaimTimeoutSeconds {
		return fmt.Errorf("agent.claim_timeout_seconds must be between %d and %d, got %d", MinClaimTimeoutSeconds, MaxClaimTimeoutSeconds, t)
	}
	if b := c.Agent.MonthlyBudgetUSD; b < 0 || math.IsNaN(b) || math.IsInf(b, 0) {
		return fmt.Errorf("agent.monthly_budget_usd must be 0 (no cap) or more, got %v", b)
	}
	if d := c.Transcripts.RetentionDays; d < 0 || d > MaxTranscriptRetentionDays {
		return fmt.Errorf("transcripts.retention_days must be between 0 and %d, got %d", MaxTranscriptRetentionDays, d)
	}
	if f := c.ASR.WorkerFallback; f != ASRWorkerFallbackLocal && f != ASRWorkerFallbackWait {
		return fmt.Errorf("asr.worker_fallback must be %q or %q, got %q", ASRWorkerFallbackLocal, ASRWorkerFallbackWait, f)
	}
	return nil
}

// checkHTTPURL accepts an absolute http(s) URL with a host and no credentials,
// query or fragment.
func checkHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not carry credentials, a query or a fragment")
	}
	return nil
}

// SaveFile changes config.yaml's own values: it loads the file without the
// environment, applies edit, validates the result and writes it (0600, a
// temporary file renamed into place). So a value the environment sets is never
// written to the file by a save of something else.
func SaveFile(dataDir string, edit func(*Config) error) error {
	cfg, err := load(dataDir, false)
	if err != nil {
		return err
	}
	if err := edit(&cfg); err != nil {
		return err
	}
	normalizeBridge(&cfg)
	if err := cfg.Validate(); err != nil {
		return err
	}
	return writeFile(dataDir, cfg)
}

// writeFile writes cfg to config.yaml (0600) through a temporary file.
func writeFile(dataDir string, cfg Config) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return fsutil.WriteFileAtomic(filepath.Join(dataDir, FileName), out, 0o600)
}
