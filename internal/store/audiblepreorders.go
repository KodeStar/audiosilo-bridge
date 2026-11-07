package store

import (
	"context"
	"time"
)

// AudiblePreorder is a pending preorder an audible.poll remembers for an
// account (migration 0012): when it was ordered, when a poll last saw it
// pending in the orders listing, and the title as the catalog last described
// it (Record: the caller's JSON, "" until a catalog read succeeded) with when
// (FilledAt). Zero times are unknown.
type AudiblePreorder struct {
	Account    string
	ASIN       string
	OrderedAt  time.Time
	LastSeenAt time.Time
	Record     string
	FilledAt   time.Time
}

// AudiblePreorders lists account's remembered preorders, by ASIN.
func (db *DB) AudiblePreorders(ctx context.Context, account string) ([]AudiblePreorder, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT account, asin, ordered_at, last_seen_at, record, filled_at
		FROM audible_preorders WHERE account = ? ORDER BY asin`, account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AudiblePreorder{}
	for rows.Next() {
		var p AudiblePreorder
		var ordered, seen, filled string
		if err := rows.Scan(&p.Account, &p.ASIN, &ordered, &seen, &p.Record, &filled); err != nil {
			return nil, err
		}
		p.OrderedAt, p.LastSeenAt, p.FilledAt = parseStamp(ordered), parseStamp(seen), parseStamp(filled)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetAudiblePreorders replaces account's remembered preorders with ps (each
// one's Account is ignored: they are account's), in one transaction.
func (db *DB) SetAudiblePreorders(ctx context.Context, account string, ps []AudiblePreorder) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM audible_preorders WHERE account = ?`, account); err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO audible_preorders (account, asin, ordered_at, last_seen_at, record, filled_at)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = ins.Close() }()
	for _, p := range ps {
		if _, err := ins.ExecContext(ctx, account, p.ASIN, stamp(p.OrderedAt), stamp(p.LastSeenAt), p.Record, stamp(p.FilledAt)); err != nil {
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
