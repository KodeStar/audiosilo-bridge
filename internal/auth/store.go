package auth

import (
	"sync"
	"time"
)

// Store persists the admin password hash and the set of live session-token
// hashes. It is deliberately storage-agnostic: tests use MemStore, and the
// daemon uses the SQLite-backed adapter from internal/store (store.AuthStore),
// which satisfies this interface structurally. Session tokens are stored ONLY as
// SHA-256 hashes, never in plaintext.
//
// M0 shipped a JSON file store here; M1 migrated durable auth into the database
// (one state file), so the file store was removed rather than left as dead code.
type Store interface {
	// LoadAuth returns the stored password hash, or "" if no admin exists yet.
	LoadAuth() (passwordHash string, err error)
	// AddSession records a live session by its token hash, but only while
	// passwordHash (the hash the sign-in was verified against) is still the
	// stored one, checked and written as one step. It reports false, adding
	// nothing, when the password changed since (a sign-in racing a change or a
	// reset never outlives it) or tokenHash is already a session.
	AddSession(tokenHash string, createdAt time.Time, passwordHash string) (bool, error)
	// HasSession reports whether a session with tokenHash exists.
	HasSession(tokenHash string) (bool, error)
	// RemoveSession deletes a session by its token hash (no-op if absent).
	RemoveSession(tokenHash string) error
	// ReplacePassword stores a new password hash (argon2id encoded) and deletes
	// every session but keepHash's in one transaction: both happen or neither
	// does, and no session can be added between them. It is the only password
	// writer beside CreatePassword, so no password is ever set without the
	// sign-out. A non-empty
	// prevHash makes it conditional: it replaces only while prevHash (the hash a
	// change verified) is still the stored one, and reports false, changing
	// nothing, when a reset or another change replaced it since. An empty prevHash
	// replaces whatever is stored (the first run, a reset).
	ReplacePassword(prevHash, passwordHash, keepHash string) (bool, error)
	// CreatePassword stores passwordHash only while no password is stored (the
	// first run), signing every session out with it in one transaction. It
	// reports false, changing nothing, when one is: a reset-password run on the
	// same data dir that landed first is never overwritten (bridge#16).
	CreatePassword(passwordHash string) (bool, error)
}

// MemStore is an in-memory Store for tests.
type MemStore struct {
	mu       sync.Mutex
	passHash string
	sessions map[string]time.Time
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{sessions: map[string]time.Time{}}
}

// LoadAuth returns the stored password hash.
func (m *MemStore) LoadAuth() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.passHash, nil
}

// AddSession records a session token hash while passwordHash is the stored one.
func (m *MemStore) AddSession(tokenHash string, createdAt time.Time, passwordHash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.sessions[tokenHash]; dup || m.passHash != passwordHash {
		return false, nil
	}
	m.sessions[tokenHash] = createdAt
	return true, nil
}

// HasSession reports whether the session token hash is present.
func (m *MemStore) HasSession(tokenHash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[tokenHash]
	return ok, nil
}

// RemoveSession removes a session token hash.
func (m *MemStore) RemoveSession(tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash)
	return nil
}

// CreatePassword stores passwordHash, signing every session out, only while no
// password is stored.
func (m *MemStore) CreatePassword(passwordHash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.passHash != "" {
		return false, nil
	}
	m.passHash = passwordHash
	clear(m.sessions)
	return true, nil
}

// ReplacePassword stores passwordHash and removes every session token hash but
// keepHash, under one lock, while prevHash (when set) is the stored hash.
func (m *MemStore) ReplacePassword(prevHash, passwordHash, keepHash string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prevHash != "" && m.passHash != prevHash {
		return false, nil
	}
	m.passHash = passwordHash
	for h := range m.sessions {
		if h != keepHash {
			delete(m.sessions, h)
		}
	}
	return true, nil
}
