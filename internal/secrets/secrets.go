// Package secrets stores sensitive values (the server token, LLM API keys, a
// GitHub PAT) OUTSIDE the config file, sealed at rest. The Bridge runs headless
// in a container, where there is no OS keychain, so every secret is an
// AES-256-GCM sealed file in <data>/secrets (0600), the manager's BlobStore
// format, keyed by one master key: AUDIOSILO_BRIDGE_SECRET_KEY when set (so the
// key can live outside the data volume), else <data>/secret.key, created 0600 on
// first start. Values are write-only from the API's point of view: callers can
// Set/Delete and probe Present, but the read API only ever exposes presence
// booleans, never the secret itself.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Named secrets recognized by the Bridge.
const (
	// ServerToken is the bridge token the server minted at pairing.
	ServerToken     = "server_token"
	AnthropicAPIKey = "anthropic_api_key"
	OpenAIAPIKey    = "openai_api_key"
	GitHubPAT       = "github_pat"
)

// Names returns the secret names an operator sets, in a stable order (drives a
// settings presence list). The server token is the pairing's, never set by hand.
func Names() []string {
	return []string{AnthropicAPIKey, OpenAIAPIKey, GitHubPAT}
}

// Store is a named-secret backend. Get returns ("", nil) for an absent name so
// callers can treat "no secret yet" as a normal state.
type Store interface {
	Get(name string) (string, error)
	Set(name, value string) error
	Delete(name string) error
	Present(name string) (bool, error)
}

// KeyEnv is the environment variable holding the master key: 32 bytes as 64 hex
// characters or as base64.
const KeyEnv = "AUDIOSILO_BRIDGE_SECRET_KEY"

// KeyFileName is the master key's file in the data directory when KeyEnv is not
// set.
const KeyFileName = "secret.key"

// dirName is the folder of sealed secrets in the data directory.
const dirName = "secrets"

// ErrWrongKey is a sealed secret the master key can't open: the key changed (a
// different AUDIOSILO_BRIDGE_SECRET_KEY, a replaced secret.key) or the file was
// tampered with or renamed.
var ErrWrongKey = errors.New("a stored secret can't be opened with this secret key")

// namePattern bounds a secret's name, which becomes a file name.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Vault is the sealed-file Store.
type Vault struct {
	mu   sync.Mutex
	dir  string
	aead cipher.AEAD
}

// KeySource says where the master key came from, for the startup log.
type KeySource string

// The master key's sources.
const (
	KeyFromEnv     KeySource = "environment"
	KeyFromFile    KeySource = "key file"
	KeyFromNewFile KeySource = "new key file"
)

// Open returns the vault in dataDir, its master key read from envKey (the
// value of KeyEnv; "" when unset) or from dataDir/secret.key, which it creates
// when there is none.
func Open(dataDir, envKey string) (*Vault, KeySource, error) {
	key, src, err := masterKey(dataDir, envKey)
	if err != nil {
		return nil, "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", err
	}
	return &Vault{dir: filepath.Join(dataDir, dirName), aead: aead}, src, nil
}

// masterKey reads the master key: envKey when set, else the key file, else a
// new random key written to the key file (0600, never overwritten).
func masterKey(dataDir, envKey string) ([]byte, KeySource, error) {
	if s := strings.TrimSpace(envKey); s != "" {
		key, err := parseKey(s)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", KeyEnv, err)
		}
		return key, KeyFromEnv, nil
	}
	path := filepath.Join(dataDir, KeyFileName)
	raw, err := os.ReadFile(path) //nolint:gosec // the operator's data directory
	switch {
	case err == nil:
		key, perr := parseKey(strings.TrimSpace(string(raw)))
		if perr != nil {
			return nil, "", fmt.Errorf("%s: %w", path, perr)
		}
		return key, KeyFromFile, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, "", err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, "", err
	}
	// O_EXCL: two starts racing on a fresh data dir must not both write a key.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator's data directory
	if err != nil {
		return nil, "", err
	}
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		_ = f.Close()
		return nil, "", err
	}
	if err := f.Close(); err != nil {
		return nil, "", err
	}
	return key, KeyFromNewFile, nil
}

// parseKey reads a 32-byte key written as 64 hex characters or as base64.
func parseKey(s string) ([]byte, error) {
	if key, err := hex.DecodeString(s); err == nil && len(key) == 32 {
		return key, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(s); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	return nil, errors.New("the secret key must be 32 bytes, as 64 hex characters or base64")
}

// path is name's sealed file, or an error for a name that isn't one.
func (v *Vault) path(name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	return filepath.Join(v.dir, name+".enc"), nil
}

// aad binds a sealed value to its name, so a file renamed onto another name
// doesn't open.
func aad(name string) []byte { return []byte("audiosilo-bridge/secret/" + name) }

// Set seals value under name (a fresh nonce each time), written to a temporary
// file and renamed into place.
func (v *Vault) Set(name, value string) error {
	path, err := v.path(name)
	if err != nil {
		return err
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := v.aead.Seal(nonce, nonce, []byte(value), aad(name))
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := os.MkdirAll(v.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(v.dir, "."+name+"-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(sealed); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Get returns the value for name, ("", nil) when absent, or ErrWrongKey when
// the stored value can't be opened.
func (v *Vault) Get(name string) (string, error) {
	path, err := v.path(name)
	if err != nil {
		return "", err
	}
	v.mu.Lock()
	sealed, err := os.ReadFile(path) //nolint:gosec // a validated name in the vault's folder
	v.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	n := v.aead.NonceSize()
	if len(sealed) < n {
		return "", ErrWrongKey
	}
	plain, err := v.aead.Open(nil, sealed[:n], sealed[n:], aad(name))
	if err != nil {
		return "", ErrWrongKey
	}
	return string(plain), nil
}

// Delete removes name; deleting an absent name is a no-op.
func (v *Vault) Delete(name string) error {
	path, err := v.path(name)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Present reports whether name has a non-empty value.
func (v *Vault) Present(name string) (bool, error) {
	s, err := v.Get(name)
	return s != "", err
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu sync.Mutex
	m  map[string]string
}

// NewMemStore returns an empty in-memory secrets store.
func NewMemStore() *MemStore { return &MemStore{m: map[string]string{}} }

// Get returns the value for name, or ("", nil) when absent.
func (s *MemStore) Get(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[name], nil
}

// Set stores value under name.
func (s *MemStore) Set(name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[name] = value
	return nil
}

// Delete removes name.
func (s *MemStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, name)
	return nil
}

// Present reports whether name has a non-empty value.
func (s *MemStore) Present(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[name] != "", nil
}
