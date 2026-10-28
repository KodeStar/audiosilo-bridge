package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. Tuned for an interactive login on modest hardware; memory
// (64 MiB) comfortably exceeds the OWASP floor. Ported from audiosilo-server.
const (
	argonTime    = 2
	argonMemory  = 64 * 1024 // KiB => 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword returns an argon2id PHC-style encoded hash of password.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argonKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the encoded argon2id hash.
// Comparison of the derived keys is constant time.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, err
	}
	var mem, passes uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &passes, &threads); err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argonKey([]byte(password), salt, passes, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// argonKey is argon2.IDKey followed by handing its work memory back to the OS
// (hashMemory). Every argon2id hash and verify goes through it.
func argonKey(password, salt []byte, passes, memory uint32, threads uint8, keyLen uint32) []byte {
	key := argon2.IDKey(password, salt, passes, memory, threads, keyLen)
	hashMemory.release()
	return key
}

// hashReleaseDelay is how long the hashes must have been quiet before their
// memory is released, so a burst of logins costs one release, not one each.
const hashReleaseDelay = time.Second

// hashMemory is the release after the last hash (see memoryReleaser).
var hashMemory = &memoryReleaser{delay: hashReleaseDelay, free: debug.FreeOSMemory}

// memoryReleaser hands an argon2id hash's work memory (argonMemory, 64 MiB, a
// fresh allocation each time) back to the OS once the hashes go quiet. Without
// it a Bridge that hashes the page's password at first start or a login and
// then idles keeps that memory: the heap grew to hold the block, so the garbage
// it leaves stays under the next GC's goal and no GC runs to free it, and the
// background scavenger returns freed pages only slowly. An idle Bridge went
// from 30 MB RSS to 97 MB after one login and took ~5 minutes to come back to
// ~50 MB (the 2026-10 perf spike); the server fixed the same in server#244.
// The release (debug.FreeOSMemory: a GC, then every free page returned) runs on
// its own goroutine after delay with no further hash, never on the request's.
type memoryReleaser struct {
	delay time.Duration
	free  func()

	mu    sync.Mutex
	timer *time.Timer
}

// release schedules the release delay from now, pushing back one already due.
func (r *memoryReleaser) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer == nil {
		r.timer = time.AfterFunc(r.delay, r.free)
		return
	}
	r.timer.Reset(r.delay)
}

// generatePassword returns a human-typable one-time admin password. It uses an
// unambiguous alphabet (no look-alike 0/O/1/l/I) and hyphenated groups so it can
// be read off a terminal and typed by hand.
func generatePassword() (string, error) {
	const alphabet = "23456789abcdefghijkmnpqrstuvwxyz"
	const groups, per = 4, 4
	buf := make([]byte, groups*per)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, c := range buf {
		if i > 0 && i%per == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return b.String(), nil
}

// generateToken returns a URL-safe random session secret and its storage hash.
// The secret has full entropy (32 bytes), so a fast hash (SHA-256) is the right
// choice for constant-effort lookup; argon2id is reserved for the low-entropy
// user password above.
func generateToken() (secret, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	secret = base64.RawURLEncoding.EncodeToString(buf)
	return secret, hashSecret(secret), nil
}

// hashSecret hashes a presented session secret for storage/lookup.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
