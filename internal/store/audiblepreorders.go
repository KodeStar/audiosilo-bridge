package store

import (
	"context"
	"time"
)

// AudiblePreorder is a pending preorder an audible.poll remembers for an
// account (migration 0012): when it was ordered, when a poll last saw it
// pending in the orders listing, and the title as the catalog last described
// it (Record: the caller's JSON, "" until a catalog read succeeded). Zero
// times are unknown. (0012's filled_at column is no longer written: nothing
// read it.)
type AudiblePreorder struct {
	ASIN       string
	OrderedAt  time.Time
	LastSeenAt time.Time
	Record     string
}

// AudiblePreorders lists account's remembered preorders, by ASIN.
func (db *DB) AudiblePreorders(ctx context.Context, account string) ([]AudiblePreorder, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT asin, ordered_at, last_seen_at, record
		FROM audible_preorders WHERE account = ? ORDER BY asin`, account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AudiblePreorder{}
	for rows.Next() {
		var p AudiblePreorder
		var ordered, seen string
		if err := rows.Scan(&p.ASIN, &ordered, &seen, &p.Record); err != nil {
			return nil, err
		}
		p.OrderedAt, p.LastSeenAt = parseStamp(ordered), parseStamp(seen)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetAudiblePreorders replaces account's remembered preorders with ps, in one
// transaction.
func (db *DB) SetAudiblePreorders(ctx context.Context, account string, ps []AudiblePreorder) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM audible_preorders WHERE account = ?`, account); err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO audible_preorders (account, asin, ordered_at, last_seen_at, record)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = ins.Close() }()
	for _, p := range ps {
		if _, err := ins.ExecContext(ctx, account, p.ASIN, stamp(p.OrderedAt), stamp(p.LastSeenAt), p.Record); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// stamp is t as the store writes it, "" for a zero time.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return timestamp(t)
}

// parseStamp reads a stamp back, zero for "" or one that doesn't parse.
func parseStamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
