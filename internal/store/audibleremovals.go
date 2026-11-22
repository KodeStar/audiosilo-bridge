package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AudibleRemoval identifies a pending server DELETE, including the number
// of attempts that have backed off. The account id belongs to the removed row.
type AudibleRemoval struct {
	Account, Pairing string
	Attempts         int
	Uncertain        bool
}

func scanAudibleRemoval(row *sql.Row) (AudibleRemoval, error) {
	var r AudibleRemoval
	err := row.Scan(&r.Account, &r.Pairing, &r.Attempts, &r.Uncertain)
	if errors.Is(err, sql.ErrNoRows) {
		return AudibleRemoval{}, ErrNotFound
	}
	return r, err
}

// NextAudibleRemoval returns one due removal, or one whose pairing ended
// even if it is backing off. The caller discards an ended pairing's removal.
func (db *DB) NextAudibleRemoval(ctx context.Context, pairing string, now time.Time) (AudibleRemoval, error) {
	return scanAudibleRemoval(db.sql.QueryRowContext(ctx, `SELECT account, pairing, attempts, uncertain FROM audible_removals
		WHERE retry_at <= ? OR pairing <> ? ORDER BY retry_at, account LIMIT 1`, timestamp(now), pairing))
}

// RemovedAudibleCustomer finds the old server row for a re-add on this pairing.
func (db *DB) RemovedAudibleCustomer(ctx context.Context, pairing, hash, marketplace string) (AudibleRemoval, error) {
	return scanAudibleRemoval(db.sql.QueryRowContext(ctx, `SELECT account, pairing, attempts, uncertain FROM audible_removals
		WHERE pairing = ? AND customer_hash = ? AND customer_hash <> '' AND marketplace = ?`, pairing, hash, marketplace))
}

// MarkAudibleRemovalSent records that a request could reach the server, so a
// restart without its answer never assumes the old account id is safe to reuse.
func (db *DB) MarkAudibleRemovalSent(ctx context.Context, r AudibleRemoval) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_removals SET uncertain = 1 WHERE account = ? AND pairing = ?`, r.Account, r.Pairing)
	return err
}

func (db *DB) DeferAudibleRemoval(ctx context.Context, r AudibleRemoval, until time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_removals SET attempts = attempts + 1, retry_at = ?, uncertain = ?
		WHERE account = ? AND pairing = ?`, timestamp(until), r.Uncertain, r.Account, r.Pairing)
	return err
}

func (db *DB) ClearAudibleRemoval(ctx context.Context, r AudibleRemoval) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM audible_removals WHERE account = ? AND pairing = ?`, r.Account, r.Pairing)
	return err
}
