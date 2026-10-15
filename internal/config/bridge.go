package config

import (
	"errors"
	"fmt"
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
// start instead of being ignored.
func applyBridgeEnv(cfg *Config) {
	cfg.fromEnv = map[string]bool{}
	if v, ok := os.LookupEnv(envServerURL); ok {
		cfg.Server.URL = strings.TrimSpace(v)
		cfg.fromEnv[KeyServerURL] = true
	}
	if v, ok := os.LookupEnv(envPublicURL); ok {
		cfg.PublicURL = strings.TrimSpace(v)
		cfg.fromEnv[KeyPublicURL] = true
	}
	if v, ok := os.LookupEnv(envLibraries); ok {
		cfg.Libraries = parseLibraries(v)
		cfg.fromEnv[KeyLibraries] = true
	}
	if v, ok := os.LookupEnv(envMappingChecks); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			n = -1
		}
		cfg.Mapping.CheckMinutes = n
		cfg.fromEnv[KeyMappingCheck] = true
	}
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
	cfg.Server.URL = strings.TrimRight(strings.TrimSpace(cfg.Server.URL), "/")
	for i := range cfg.Libraries {
		if p := strings.TrimSpace(cfg.Libraries[i].MountPath); p != "" {
			cfg.Libraries[i].MountPath = filepath.Clean(p)
		}
	}
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
