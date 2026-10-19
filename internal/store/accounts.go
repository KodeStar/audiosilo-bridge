package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Account is what the Accounts page shows about one external account (migration
// 0006); the credential itself is in the vault.
type Account struct {
	Provider   string `json:"provider"`
	Login      string `json:"login"`
	SignedInAt string `json:"signed_in_at"`
	LastOKAt   string `json:"last_ok_at"`
	RefusedAt  string `json:"refused_at"`
	LastError  string `json:"last_error"`
}

// GetAccount returns provider's account, or ErrNotFound.
func (db *DB) GetAccount(ctx context.Context, provider string) (Account, error) {
	var a Account
	err := db.sql.QueryRowContext(ctx, `SELECT provider, login, signed_in_at, last_ok_at, refused_at, last_error
		FROM accounts WHERE provider = ?`, provider).Scan(&a.Provider, &a.Login, &a.SignedInAt, &a.LastOKAt, &a.RefusedAt, &a.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// SignInAccount records a new sign-in for provider (login, at): it works, and
// any earlier refusal is forgotten.
func (db *DB) SignInAccount(ctx context.Context, provider, login string, at time.Time) error {
	ts := timestamp(at)
	_, err := db.sql.ExecContext(ctx, `INSERT INTO accounts (provider, login, signed_in_at, last_ok_at, refused_at, last_error)
		VALUES (?, ?, ?, ?, '', '')
		ON CONFLICT(provider) DO UPDATE SET login = excluded.login, signed_in_at = excluded.signed_in_at,
			last_ok_at = excluded.last_ok_at, refused_at = '', last_error = ''`, provider, login, ts, ts)
	return err
}

// AccountOK records that provider's credential worked at at (a refusal ends).
// No row: nothing to record.
func (db *DB) AccountOK(ctx context.Context, provider string, at time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE accounts SET last_ok_at = ?, refused_at = '', last_error = '' WHERE provider = ?`,
		timestamp(at), provider)
	return err
}

// RefuseAccount records that the provider refused provider's credential at at,
// and why. No row: nothing to record.
func (db *DB) RefuseAccount(ctx context.Context, provider, why string, at time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE accounts SET refused_at = ?, last_error = ? WHERE provider = ?`,
		timestamp(at), why, provider)
	return err
}

// SetAccountError records a problem with provider's account that isn't a
// refusal (a refresh that couldn't reach GitHub). No row: nothing to record.
func (db *DB) SetAccountError(ctx context.Context, provider, why string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE accounts SET last_error = ? WHERE provider = ?`, why, provider)
	return err
}

// DeleteAccount forgets provider's account (sign-out).
func (db *DB) DeleteAccount(ctx context.Context, provider string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM accounts WHERE provider = ?`, provider)
	return err
}
