package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openVault(t *testing.T, dir, envKey string) *Vault {
	t.Helper()
	v, _, err := Open(dir, envKey)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return v
}

func TestVaultLifecycle(t *testing.T) {
	t.Parallel()
	v := openVault(t, t.TempDir(), "")

	if s, err := v.Get(ServerToken); s != "" || err != nil {
		t.Fatalf("absent Get = %q, %v", s, err)
	}
	if p, _ := v.Present(ServerToken); p {
		t.Fatal("absent Present = true")
	}
	if err := v.Set(ServerToken, "tok-secret"); err != nil {
		t.Fatal(err)
	}
	if s, err := v.Get(ServerToken); s != "tok-secret" || err != nil {
		t.Fatalf("Get = %q, %v", s, err)
	}
	if p, _ := v.Present(ServerToken); !p {
		t.Fatal("after Set, Present = false")
	}
	if err := v.Set(ServerToken, "tok-2"); err != nil {
		t.Fatal(err)
	}
	if s, _ := v.Get(ServerToken); s != "tok-2" {
		t.Fatalf("overwrite: Get = %q", s)
	}
	if err := v.Delete(ServerToken); err != nil {
		t.Fatal(err)
	}
	if p, _ := v.Present(ServerToken); p {
		t.Fatal("after Delete, Present = true")
	}
	if err := v.Delete(ServerToken); err != nil {
		t.Fatalf("deleting an absent secret: %v", err)
	}
}

// The value is sealed on disk: the file holds neither the plaintext nor a copy
// of it, and is owner-only, as are the folder and the key file.
func TestVaultSealsAtRest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	v := openVault(t, dir, "")
	if err := v.Set(GitHubPAT, "ghp_plaintext_value"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "secrets", GitHubPAT+".enc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("ghp_plaintext_value")) {
		t.Fatal("the sealed file holds the plaintext")
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(dir, "secrets", GitHubPAT+".enc"): 0o600,
		filepath.Join(dir, "secrets"):                   0o700,
		filepath.Join(dir, KeyFileName):                 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %v, want %v", path, got, want)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "secrets"))
	if len(entries) != 1 {
		t.Errorf("secrets folder holds %d entries, want 1 (no temp file left)", len(entries))
	}
}

// The key file made on the first start is the key of every later start.
func TestVaultKeyFileReused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	v, src, err := Open(dir, "")
	if err != nil || src != KeyFromNewFile {
		t.Fatalf("first Open: %v, %q", err, src)
	}
	if err := v.Set(OpenAIAPIKey, "sk-1"); err != nil {
		t.Fatal(err)
	}
	again, src, err := Open(dir, "")
	if err != nil || src != KeyFromFile {
		t.Fatalf("second Open: %v, %q", err, src)
	}
	if s, err := again.Get(OpenAIAPIKey); s != "sk-1" || err != nil {
		t.Fatalf("Get after reopen = %q, %v", s, err)
	}
}

// Security: a value sealed with one key never opens with another (allowed with
// the same env key, denied with a different one), and a file moved onto another
// name doesn't open either.
func TestVaultKeyAndNameBinding(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyA := strings.Repeat("ab", 32)
	keyB := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	a := openVault(t, dir, keyA)
	if err := a.Set(AnthropicAPIKey, "sk-ant"); err != nil {
		t.Fatal(err)
	}
	if s, err := openVault(t, dir, keyA).Get(AnthropicAPIKey); s != "sk-ant" || err != nil {
		t.Fatalf("allowed: same key = %q, %v", s, err)
	}
	if _, err := openVault(t, dir, keyB).Get(AnthropicAPIKey); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("denied: other key = %v, want ErrWrongKey", err)
	}
	if _, err := openVault(t, dir, "").Get(AnthropicAPIKey); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("denied: the key file's key = %v, want ErrWrongKey", err)
	}
	src := filepath.Join(dir, "secrets", AnthropicAPIKey+".enc")
	dst := filepath.Join(dir, "secrets", OpenAIAPIKey+".enc")
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get(OpenAIAPIKey); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("denied: a renamed file = %v, want ErrWrongKey", err)
	}
	if err := os.WriteFile(src, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get(AnthropicAPIKey); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("denied: a truncated file = %v, want ErrWrongKey", err)
	}
}

func TestParseKey(t *testing.T) {
	t.Parallel()
	raw := bytes.Repeat([]byte{0x42}, 32)
	for name, s := range map[string]string{
		"hex":        hex.EncodeToString(raw),
		"base64":     base64.StdEncoding.EncodeToString(raw),
		"raw base64": base64.RawStdEncoding.EncodeToString(raw),
		"url base64": base64.URLEncoding.EncodeToString(raw),
	} {
		key, err := parseKey(s)
		if err != nil || !bytes.Equal(key, raw) {
			t.Errorf("%s: parseKey = %x, %v", name, key, err)
		}
	}
	for _, bad := range []string{"", "too-short", hex.EncodeToString(raw[:16]), base64.StdEncoding.EncodeToString(raw[:31])} {
		if _, err := parseKey(bad); err == nil {
			t.Errorf("parseKey(%q) accepted a bad key", bad)
		}
	}
	if _, _, err := Open(t.TempDir(), "not-a-key"); err == nil || !strings.Contains(err.Error(), KeyEnv) {
		t.Errorf("Open with a bad env key = %v, want an error naming %s", err, KeyEnv)
	}
}

// Security: a secret's name becomes a file name, so only plain names are
// accepted (allowed: the known names; denied: anything that could leave the
// folder or hide a file).
func TestVaultNames(t *testing.T) {
	t.Parallel()
	v := openVault(t, t.TempDir(), "")
	for _, name := range append(Names(), ServerToken) {
		if err := v.Set(name, "x"); err != nil {
			t.Errorf("allowed: Set(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "../escape", "a/b", ".hidden", "UPPER", "with space", strings.Repeat("a", 65)} {
		if err := v.Set(name, "x"); err == nil {
			t.Errorf("denied: Set(%q) succeeded", name)
		}
		if _, err := v.Get(name); err == nil {
			t.Errorf("denied: Get(%q) succeeded", name)
		}
		if err := v.Delete(name); err == nil {
			t.Errorf("denied: Delete(%q) succeeded", name)
		}
	}
}

func TestMemStore(t *testing.T) {
	t.Parallel()
	var s Store = NewMemStore()
	if err := s.Set(GitHubPAT, "p"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get(GitHubPAT); v != "p" {
		t.Fatalf("Get = %q", v)
	}
	if err := s.Delete(GitHubPAT); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Present(GitHubPAT); p {
		t.Fatal("Present after Delete")
	}
}
