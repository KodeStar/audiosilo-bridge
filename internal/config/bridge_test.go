package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBridgeEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIOSILO_BRIDGE_SERVER_URL", " http://audiosilo:8080/ ")
	t.Setenv("AUDIOSILO_BRIDGE_PUBLIC_URL", "http://192.168.1.20:8091")
	t.Setenv("AUDIOSILO_BRIDGE_LIBRARIES", "1=/books ; 2=/media/a, b;")
	t.Setenv("AUDIOSILO_BRIDGE_MAPPING_CHECK_MINUTES", "5")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.URL != "http://audiosilo:8080" || cfg.PublicURL != "http://192.168.1.20:8091" || cfg.Mapping.CheckMinutes != 5 {
		t.Fatalf("env = %+v %q %+v", cfg.Server, cfg.PublicURL, cfg.Mapping)
	}
	if len(cfg.Libraries) != 2 || cfg.Libraries[1] != (LibraryMapping{2, "/media/a, b"}) {
		t.Fatalf("libraries = %+v", cfg.Libraries)
	}
	for _, k := range []string{KeyServerURL, KeyPublicURL, KeyLibraries, KeyMappingCheck} {
		if !cfg.EnvSet(k) {
			t.Errorf("EnvSet(%q) = false", k)
		}
	}
}

func TestBridgeEnvTyposFailTheStart(t *testing.T) {
	for name, env := range map[string][2]string{
		"library without an id": {"AUDIOSILO_BRIDGE_LIBRARIES", "/books"},
		"relative mount":        {"AUDIOSILO_BRIDGE_LIBRARIES", "1=books"},
		"library twice":         {"AUDIOSILO_BRIDGE_LIBRARIES", "1=/a;1=/b"},
		"check not a number":    {"AUDIOSILO_BRIDGE_MAPPING_CHECK_MINUTES", "often"},
		"check too long":        {"AUDIOSILO_BRIDGE_MAPPING_CHECK_MINUTES", "100000"},
		"server not http":       {"AUDIOSILO_BRIDGE_SERVER_URL", "ftp://audiosilo"},
		"server with a query":   {"AUDIOSILO_BRIDGE_SERVER_URL", "http://audiosilo?x=1"},
		"server with a user":    {"AUDIOSILO_BRIDGE_SERVER_URL", "http://me:pw@audiosilo"},
		"public url relative":   {"AUDIOSILO_BRIDGE_PUBLIC_URL", "/bridge"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(env[0], env[1])
			if _, err := Load(t.TempDir()); err == nil {
				t.Fatalf("%s=%q loaded", env[0], env[1])
			}
		})
	}
}

// An empty variable (compose's ${VAR} of an unset one, a blank template
// field) is no setting: the start doesn't fail and the page isn't locked,
// while a set one still is (TestBridgeEnv) and a typo still fails the start.
func TestBridgeEnvEmptyIsUnset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("public_url: http://file.lan:8091\nlibraries:\n  - library_id: 1\n    mount_path: /books\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AUDIOSILO_BRIDGE_SERVER_URL", "AUDIOSILO_BRIDGE_PUBLIC_URL", "AUDIOSILO_BRIDGE_LIBRARIES", "AUDIOSILO_BRIDGE_MAPPING_CHECK_MINUTES"} {
		t.Setenv(name, " ")
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("empty variables failed the start: %v", err)
	}
	for _, k := range []string{KeyServerURL, KeyPublicURL, KeyLibraries, KeyMappingCheck} {
		if cfg.EnvSet(k) {
			t.Errorf("an empty variable locked %q", k)
		}
	}
	if cfg.PublicURL != "http://file.lan:8091" || len(cfg.Libraries) != 1 || cfg.Mapping.CheckMinutes != DefaultMappingCheckMinutes {
		t.Fatalf("config = %q %+v %+v", cfg.PublicURL, cfg.Libraries, cfg.Mapping)
	}
}

func TestMountFor(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Libraries = []LibraryMapping{{LibraryID: 2, MountPath: "/mnt/books"}}
	if p, ok := cfg.MountFor(2, "/audiobooks"); p != "/mnt/books" || !ok {
		t.Errorf("mapped = %q, %v", p, ok)
	}
	if p, ok := cfg.MountFor(3, "/audiobooks"); p != "/audiobooks" || ok {
		t.Errorf("unmapped = %q, %v", p, ok)
	}
}

// SaveFile edits the file's own values: a setting the environment overrides is
// neither written from the environment nor lost.
func TestSaveFileKeepsTheEnvironmentOut(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("public_url: http://file.lan:8091\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUDIOSILO_BRIDGE_PUBLIC_URL", "http://env.lan:8091")
	t.Setenv("AUDIOSILO_BRIDGE_SERVER_URL", "http://env-server:8080")
	err := SaveFile(dir, func(c *Config) error {
		c.Libraries = []LibraryMapping{{LibraryID: 1, MountPath: "/books/"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "env.lan") || strings.Contains(string(raw), "env-server") || !strings.Contains(string(raw), "file.lan") {
		t.Fatalf("the file took the environment's values:\n%s", raw)
	}
	info, _ := os.Stat(filepath.Join(dir, FileName))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v", info.Mode().Perm())
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "http://env.lan:8091" || len(cfg.Libraries) != 1 || cfg.Libraries[0].MountPath != "/books" {
		t.Fatalf("after save: %q %+v", cfg.PublicURL, cfg.Libraries)
	}
	if err := SaveFile(dir, func(c *Config) error { c.PublicURL = "not a url"; return nil }); err == nil {
		t.Fatal("an invalid edit was saved")
	}
}

func TestAgentContainerDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.ClaimTimeoutSeconds != 120 || cfg.Agent.MonthlyBudgetUSD != 100 || cfg.Transcripts.RetentionDays != 30 {
		t.Fatalf("defaults = %d %v %d", cfg.Agent.ClaimTimeoutSeconds, cfg.Agent.MonthlyBudgetUSD, cfg.Transcripts.RetentionDays)
	}
	if got := cfg.AgentStagingDir(dir); got != filepath.Join(dir, "staging") {
		t.Errorf("staging dir = %q", got)
	}
	// A config written before these settings existed gets the defaults too.
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("agent:\n  backend: claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = Load(dir); err != nil || cfg.Agent.ClaimTimeoutSeconds != 120 || cfg.Agent.MonthlyBudgetUSD != 100 || cfg.Transcripts.RetentionDays != 30 {
		t.Fatalf("old file: %v %+v %+v", err, cfg.Agent, cfg.Transcripts)
	}
}

// 0 means "no cap" and "delete at the job's end", so an explicit 0 is kept, while
// a 0 claim timeout (absent) is the default.
func TestAgentContainerZeroes(t *testing.T) {
	dir := t.TempDir()
	body := "agent:\n  monthly_budget_usd: 0\n  claim_timeout_seconds: 0\n  staging_dir: /staging/\ntranscripts:\n  retention_days: 0\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.MonthlyBudgetUSD != 0 || cfg.Transcripts.RetentionDays != 0 || cfg.Agent.ClaimTimeoutSeconds != DefaultClaimTimeoutSeconds {
		t.Fatalf("zeroes = %+v %+v", cfg.Agent, cfg.Transcripts)
	}
	if got := cfg.AgentStagingDir(dir); got != "/staging" {
		t.Errorf("staging dir = %q", got)
	}
}

func TestAgentContainerEnv(t *testing.T) {
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_STAGING_DIR", "/bridge-data/staging")
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_CLAIM_TIMEOUT_SECONDS", "300")
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD", "25.5")
	t.Setenv("AUDIOSILO_BRIDGE_TRANSCRIPTS_RETENTION_DAYS", "7")
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.StagingDir != "/bridge-data/staging" || cfg.Agent.ClaimTimeoutSeconds != 300 ||
		cfg.Agent.MonthlyBudgetUSD != 25.5 || cfg.Transcripts.RetentionDays != 7 {
		t.Fatalf("env = %+v %+v", cfg.Agent, cfg.Transcripts)
	}
	for _, k := range []string{KeyAgentStagingDir, KeyAgentClaimTimeout, KeyAgentMonthlyBudget, KeyTranscriptsRetention} {
		if !cfg.EnvSet(k) {
			t.Errorf("EnvSet(%q) = false", k)
		}
	}
}

func TestAgentContainerEnvEmptyIsUnset(t *testing.T) {
	for _, name := range []string{"AUDIOSILO_BRIDGE_AGENT_STAGING_DIR", "AUDIOSILO_BRIDGE_AGENT_CLAIM_TIMEOUT_SECONDS",
		"AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD", "AUDIOSILO_BRIDGE_TRANSCRIPTS_RETENTION_DAYS"} {
		t.Setenv(name, "")
	}
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("empty variables failed the start: %v", err)
	}
	if cfg.EnvSet(KeyAgentMonthlyBudget) || cfg.Agent.MonthlyBudgetUSD != DefaultMonthlyBudgetUSD {
		t.Fatalf("an empty variable counted: %+v", cfg.Agent)
	}
}

func TestAgentContainerInvalid(t *testing.T) {
	for name, env := range map[string][2]string{
		"relative staging dir":   {"AUDIOSILO_BRIDGE_AGENT_STAGING_DIR", "staging"},
		"claim not a number":     {"AUDIOSILO_BRIDGE_AGENT_CLAIM_TIMEOUT_SECONDS", "soon"},
		"claim too short":        {"AUDIOSILO_BRIDGE_AGENT_CLAIM_TIMEOUT_SECONDS", "5"},
		"claim too long":         {"AUDIOSILO_BRIDGE_AGENT_CLAIM_TIMEOUT_SECONDS", "7200"},
		"budget negative":        {"AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD", "-1"},
		"budget not a number":    {"AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD", "lots"},
		"budget NaN":             {"AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD", "NaN"},
		"budget infinite":        {"AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD", "+Inf"},
		"retention negative":     {"AUDIOSILO_BRIDGE_TRANSCRIPTS_RETENTION_DAYS", "-3"},
		"retention not a number": {"AUDIOSILO_BRIDGE_TRANSCRIPTS_RETENTION_DAYS", "a month"},
		"retention too long":     {"AUDIOSILO_BRIDGE_TRANSCRIPTS_RETENTION_DAYS", "100000"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(env[0], env[1])
			if _, err := Load(t.TempDir()); err == nil {
				t.Fatalf("%s=%q loaded", env[0], env[1])
			}
		})
	}
}

// The environment's agent settings are never written into config.yaml by a save.
func TestAgentContainerSaveKeepsTheEnvironmentOut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_STAGING_DIR", "/env/staging")
	t.Setenv("AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD", "7")
	if err := SaveFile(dir, func(c *Config) error { c.Transcripts.RetentionDays = 9; return nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "/env/staging") || strings.Contains(string(raw), "monthly_budget_usd: 7") ||
		!strings.Contains(string(raw), "retention_days: 9") || !strings.Contains(string(raw), "monthly_budget_usd: 100") {
		t.Fatalf("the file:\n%s", raw)
	}
}
