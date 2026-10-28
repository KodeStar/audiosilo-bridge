package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// authPasswordKey is the settings key holding the admin argon2id password hash.
// Keeping it in the settings KV (rather than a separate auth.json) means the
// daemon has a single durable state file - the DB - which the M1 migration off
// the JSON files establishes.
const authPasswordKey = "auth.password_hash"

// AuthStore adapts *DB to the auth.Store interface (the password hash +
// session CRUD) so the auth Manager persists to SQLite. It is defined here
// (rather than in internal/auth) to avoid a store->auth import; internal/auth
// depends only on its own Store interface, which this satisfies structurally.
type AuthStore struct {
	db  *DB
	ctx context.Context //nolint:containedctx // background ctx for a long-lived adapter
}

// AuthStore returns an auth.Store-compatible adapter over the database. The
// adapter carries a background context because the auth.Store interface predates
// context threading; store operations here are trivial local SQLite writes.
func (db *DB) AuthStore() *AuthStore {
	return &AuthStore{db: db, ctx: context.Background()}
}

// LoadAuth returns the stored admin password hash, or "" when unset.
func (a *AuthStore) LoadAuth() (string, error) {
	v, _, err := a.db.GetSetting(a.ctx, authPasswordKey)
	return v, err
}

// AddSession records a live session by its token hash, only while
// passwordHash is still the stored password hash. The check and the insert are
// one statement, so a password change or reset committed by another connection
// (the reset-password command runs as its own process) can't slip between
// them. It reports false, adding nothing, when the hash differs or tokenHash is
// already a session.
func (a *AuthStore) AddSession(tokenHash string, createdAt time.Time, passwordHash string) (bool, error) {
	res, err := a.db.sql.ExecContext(a.ctx,
		`INSERT INTO sessions (token_hash, created_at)
		 SELECT ?, ? WHERE EXISTS (SELECT 1 FROM settings WHERE key=? AND value=?)
		 ON CONFLICT(token_hash) DO NOTHING`,
		tokenHash, timestamp(createdAt), authPasswordKey, passwordHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// HasSession reports whether tokenHash names a live session.
func (a *AuthStore) HasSession(tokenHash string) (bool, error) {
	var one int
	err := a.db.sql.QueryRowContext(a.ctx,
		`SELECT 1 FROM sessions WHERE token_hash=?`, tokenHash).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RemoveSession deletes a session by token hash (no-op if absent).
func (a *AuthStore) RemoveSession(tokenHash string) error {
	_, err := a.db.sql.ExecContext(a.ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

// CreatePassword stores passwordHash and deletes every session in one
// transaction, only while no password is stored: the first start's EnsureAdmin
// (bridge#16). The check and the write are one statement (an upsert that
// updates only an empty value), so a reset-password process that committed a
// password first is never overwritten; it reports false, changing nothing.
func (a *AuthStore) CreatePassword(passwordHash string) (bool, error) {
	return a.swapPassword("", func(tx *sql.Tx) (sql.Result, error) {
		return tx.ExecContext(a.ctx,
			`INSERT INTO settings (key, value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE settings.value = ''`,
			authPasswordKey, passwordHash)
	})
}

// ReplacePassword stores passwordHash and deletes every session but keepHash's
// in one transaction (bridge#11): both commit or neither does. The password
// write comes first, so the transaction holds the write lock from its first
// statement and no session can be added in between, by this process or
// another. A non-empty prevHash makes the write an UPDATE of exactly that hash:
// when a reset or another change (possibly the reset-password process) replaced
// it since, nothing is written and it reports false.
func (a *AuthStore) ReplacePassword(prevHash, passwordHash, keepHash string) (bool, error) {
	return a.swapPassword(keepHash, func(tx *sql.Tx) (sql.Result, error) {
		if prevHash == "" {
			return nil, setSetting(a.ctx, tx, authPasswordKey, passwordHash)
		}
		return tx.ExecContext(a.ctx, `UPDATE settings SET value=? WHERE key=? AND value=?`,
			passwordHash, authPasswordKey, prevHash)
	})
}

// swapPassword is both password writers' transaction: write first (it takes
// the write lock, so no session is added in between), then, when it wrote,
// every session but keepHash's is deleted. A write's result must affect exactly
// one row (a nil result: an unconditional write); otherwise nothing changes and
// it reports false.
func (a *AuthStore) swapPassword(keepHash string, write func(*sql.Tx) (sql.Result, error)) (bool, error) {
	tx, err := a.db.sql.BeginTx(a.ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	res, err := write(tx)
	if err != nil {
		return false, err
	}
	if res != nil {
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return false, err
		}
	}
	if _, err := tx.ExecContext(a.ctx, `DELETE FROM sessions WHERE token_hash != ?`, keepHash); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
