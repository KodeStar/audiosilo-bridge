package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AudibleRemoval is a server DELETE still owed for a removed account, and the
// failed attempts so far (the backoff's count).
type AudibleRemoval struct {
	Account, Pairing string
	Attempts         int
}

// NextAudibleRemoval returns pairing's removal due first at now, or ErrNotFound.
func (db *DB) NextAudibleRemoval(ctx context.Context, pairing string, now time.Time) (AudibleRemoval, error) {
	var r AudibleRemoval
	err := db.sql.QueryRowContext(ctx, `SELECT account, pairing, attempts FROM audible_removals
		WHERE pairing = ? AND retry_at <= ? ORDER BY retry_at, account LIMIT 1`, pairing, timestamp(now)).
		Scan(&r.Account, &r.Pairing, &r.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return AudibleRemoval{}, ErrNotFound
	}
	return r, err
}

// DropAudibleRemovals forgets the removals of every pairing but keep (they
// ended: their DELETEs must not reach another pairing's server) and returns
// how many it dropped.
func (db *DB) DropAudibleRemovals(ctx context.Context, keep string) (int64, error) {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM audible_removals WHERE pairing <> ?`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeferAudibleRemoval counts a failed attempt at account's removal and makes
// it due again at until.
func (db *DB) DeferAudibleRemoval(ctx context.Context, account string, until time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_removals SET attempts = attempts + 1, retry_at = ? WHERE account = ?`,
		timestamp(until), account)
	return err
}

// ClearAudibleRemoval forgets account's removal (the server answered it).
func (db *DB) ClearAudibleRemoval(ctx context.Context, account string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM audible_removals WHERE account = ?`, account)
	return err
}
