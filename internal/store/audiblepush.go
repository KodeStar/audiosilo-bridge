package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AudiblePush is one (account, ASIN) of the Audible position sync (migration
// 0011): the book the server's last audible.sync listed for it (Listed false:
// it wasn't listed the last time, so neither direction touches it), Audible's
// runtime, the cached content reference, the place last written to Audible
// (LastMs, -1 none) and when, the Audible place the last sync sent to the
// server (HeardMs, -1 none), the place waiting to be pushed (WantMs, nil
// none), and the place of the server row last read for the title (RowMs, -1
// none) with when this Bridge first read that place (RowSeen).
type AudiblePush struct {
	Account   string
	ASIN      string
	LibraryID int64
	Path      string
	RuntimeS  int64
	Listed    bool
	ACR       string
	LastMs    int64
	At        string
	HeardMs   int64
	WantMs    *int64
	// The server row last read: its place, and when this Bridge first read
	// that place (its own clock).
	RowMs   int64
	RowSeen string
}

// AudibleRow is a server progress row as the push records it: its place (ms)
// and when this Bridge first read it.
type AudibleRow struct {
	Ms   int64
	Seen time.Time
}

// AudibleSyncTitle is one title an audible.sync job lists: the ASIN, the book
// it matched and Audible's runtime (seconds, 0 unknown).
type AudibleSyncTitle struct {
	ASIN      string
	LibraryID int64
	Path      string
	RuntimeS  int64
}

const audiblePushColumns = `account, asin, library_id, path, runtime_s, listed, acr, last_ms, at, heard_ms, want_ms, row_ms, row_seen`

func scanAudiblePush(row interface{ Scan(...any) error }) (AudiblePush, error) {
	var p AudiblePush
	var want sql.NullInt64
	err := row.Scan(&p.Account, &p.ASIN, &p.LibraryID, &p.Path, &p.RuntimeS, &p.Listed, &p.ACR, &p.LastMs, &p.At, &p.HeardMs,
		&want, &p.RowMs, &p.RowSeen)
	if want.Valid {
		p.WantMs = &want.Int64
	}
	return p, err
}

// ListAudibleSync records the titles an audible.sync job of account lists:
// each one's book and runtime, listed; every other row of the account is
// unlisted (a title the console stopped syncing, or one that lost its match),
// and its waiting place forgotten. The pushes, places heard and cached
// content references stay. A title whose book changed forgets its waiting
// place too, and the other book's row last read (the next one read for the
// new book is a new place).
func (db *DB) ListAudibleSync(ctx context.Context, account string, titles []AudibleSyncTitle) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE audible_push SET listed = 0 WHERE account = ?`, account); err != nil {
		return err
	}
	upsert, err := tx.PrepareContext(ctx, `INSERT INTO audible_push (account, asin, library_id, path, runtime_s, listed)
		VALUES (?, ?, ?, ?, ?, 1)
		ON CONFLICT(account, asin) DO UPDATE SET
			want_ms = CASE WHEN library_id = excluded.library_id AND path = excluded.path THEN want_ms ELSE NULL END,
			row_ms = CASE WHEN library_id = excluded.library_id AND path = excluded.path THEN row_ms ELSE -1 END,
			row_seen = CASE WHEN library_id = excluded.library_id AND path = excluded.path THEN row_seen ELSE '' END,
			library_id = excluded.library_id, path = excluded.path, runtime_s = excluded.runtime_s, listed = 1`)
	if err != nil {
		return err
	}
	defer func() { _ = upsert.Close() }()
	for _, t := range titles {
		if _, err := upsert.ExecContext(ctx, account, t.ASIN, t.LibraryID, t.Path, t.RuntimeS); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audible_push SET want_ms = NULL WHERE account = ? AND listed = 0`, account); err != nil {
		return err
	}
	return tx.Commit()
}

// AudiblePushes lists account's rows, by ASIN.
func (db *DB) AudiblePushes(ctx context.Context, account string) ([]AudiblePush, error) {
	return db.audiblePushes(ctx, `SELECT `+audiblePushColumns+` FROM audible_push WHERE account = ? ORDER BY asin`, account)
}

// AudibleWaiting lists account's listed rows with a place waiting to be
// pushed, by ASIN.
func (db *DB) AudibleWaiting(ctx context.Context, account string) ([]AudiblePush, error) {
	return db.audiblePushes(ctx, `SELECT `+audiblePushColumns+` FROM audible_push
		WHERE account = ? AND listed = 1 AND want_ms IS NOT NULL ORDER BY asin`, account)
}

func (db *DB) audiblePushes(ctx context.Context, query, account string) ([]AudiblePush, error) {
	rows, err := db.sql.QueryContext(ctx, query, account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AudiblePush{}
	for rows.Next() {
		p, err := scanAudiblePush(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetAudiblePush returns one row, or ErrNotFound.
func (db *DB) GetAudiblePush(ctx context.Context, account, asin string) (AudiblePush, error) {
	p, err := scanAudiblePush(db.sql.QueryRowContext(ctx, `SELECT `+audiblePushColumns+` FROM audible_push
		WHERE account = ? AND asin = ?`, account, asin))
	if errors.Is(err, sql.ErrNoRows) {
		return AudiblePush{}, ErrNotFound
	}
	return p, err
}

// WantAudiblePush records a server place waiting to be pushed: row's place,
// with the row. Only a listed row takes one.
func (db *DB) WantAudiblePush(ctx context.Context, account, asin string, row AudibleRow) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_push SET want_ms = ?1, row_ms = ?1, row_seen = ?2
		WHERE account = ?3 AND asin = ?4 AND listed = 1`, row.Ms, timestamp(row.Seen), account, asin)
	return err
}

// SeenAudibleRow records a server row that was read and needs no push
// (history: changed before the feed's baseline): the waiting place goes.
func (db *DB) SeenAudibleRow(ctx context.Context, account, asin string, row AudibleRow) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_push SET want_ms = NULL, row_ms = ?, row_seen = ?
		WHERE account = ? AND asin = ?`, row.Ms, timestamp(row.Seen), account, asin)
	return err
}

// PendAudiblePush records a server row (want: its place waits; false:
// history, only the row) on a title the server's feed syncs for the book (libraryID, path) but
// the last audible.sync didn't list for it (a new match, a re-match, a title
// synced again, a remap): it waits unlisted, never pushed, until the next
// sync lists the title for that book (ListAudibleSync keeps it) or doesn't
// (it goes).
func (db *DB) PendAudiblePush(ctx context.Context, account, asin string, libraryID int64, path string, want bool,
	row AudibleRow) error {
	var ms *int64
	if want {
		ms = &row.Ms
	}
	_, err := db.sql.ExecContext(ctx, `INSERT INTO audible_push (account, asin, library_id, path, listed, want_ms, row_ms, row_seen)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?)
		ON CONFLICT(account, asin) DO UPDATE SET library_id = excluded.library_id, path = excluded.path, listed = 0,
			want_ms = excluded.want_ms, row_ms = excluded.row_ms, row_seen = excluded.row_seen`,
		account, asin, libraryID, path, ms, row.Ms, timestamp(row.Seen))
	return err
}

// DropAudiblePush forgets (account, asin)'s waiting place without a write.
func (db *DB) DropAudiblePush(ctx context.Context, account, asin string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_push SET want_ms = NULL WHERE account = ? AND asin = ?`, account, asin)
	return err
}

// AudiblePushed records that ms was written to Audible at at: the waiting
// place goes, and the place heard before it is no longer Audible's (heard_ms
// -1: a server place there is a move, written like any other).
func (db *DB) AudiblePushed(ctx context.Context, account, asin string, ms int64, at time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_push SET want_ms = NULL, last_ms = ?, at = ?, heard_ms = -1
		WHERE account = ? AND asin = ?`, ms, timestamp(at), account, asin)
	return err
}

// AudibleHeard records the Audible place (ms) an audible.sync that started at
// since sent to the server for x's title (x as the job read it at since). The
// place last written is no longer Audible's (last_ms -1), unless the push wrote
// again since. applied: the server took the place (it was further on), so a
// place waiting to be pushed from an older server row goes, unless the push
// recorded a new place since (row_ms or row_seen moved). One recorded since
// that is not further on than ms goes too while the push hasn't written since
// the job started: the server row it came from was replaced by ms (a book has
// one row), and Audible is still at ms, so writing it would move Audible back
// behind the server.
func (db *DB) AudibleHeard(ctx context.Context, x AudiblePush, ms int64, applied bool, since time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_push SET heard_ms = ?1,
			last_ms = CASE WHEN at < ?2 THEN -1 ELSE last_ms END,
			want_ms = CASE WHEN ?3 AND ((row_ms = ?4 AND row_seen = ?5) OR (want_ms <= ?1 AND at < ?2)) THEN NULL
				ELSE want_ms END
		WHERE account = ?6 AND asin = ?7`, ms, timestamp(since), applied, x.RowMs, x.RowSeen, x.Account, x.ASIN)
	return err
}

// RearmAudiblePush makes the server row last read for x's title (row_ms) wait
// to be written to Audible again, first read at now (the push's stillness
// counts from it), and reports whether it did. An audible.sync calls it when
// Audible answered a place behind the one the Bridge wrote, and the server
// still holds that place (the Audible app uploaded an older place over it).
// Only while nothing changed since the job read x (as x): the title is listed,
// no place waits, the push hasn't written since (at) and no new server row was
// recorded (row_ms, row_seen). The push's own checks still run before the
// write (Audible further on is never overwritten).
func (db *DB) RearmAudiblePush(ctx context.Context, x AudiblePush, now time.Time) (bool, error) {
	res, err := db.sql.ExecContext(ctx, `UPDATE audible_push SET want_ms = row_ms, row_seen = ?1
		WHERE account = ?2 AND asin = ?3 AND listed = 1 AND want_ms IS NULL AND row_ms >= 0
		  AND at = ?4 AND row_ms = ?5 AND row_seen = ?6`, timestamp(now), x.Account, x.ASIN, x.At, x.RowMs, x.RowSeen)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetAudibleACR caches (account, asin)'s content reference ("" forgets it).
func (db *DB) SetAudibleACR(ctx context.Context, account, asin, acr string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_push SET acr = ? WHERE account = ? AND asin = ?`, acr, account, asin)
	return err
}

// SetAudiblePushCursor records the push loop's place in id's progress feed,
// and when its baseline read the feed to the end (from; kept once set, and a
// zero from, a baseline still under way, leaves it).
func (db *DB) SetAudiblePushCursor(ctx context.Context, id, cursor string, from time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_accounts SET push_cursor = ?,
		push_from = CASE WHEN push_from = '' THEN ? ELSE push_from END WHERE id = ?`, cursor, stamp(from), id)
	return err
}

// AudibleSyncOK records that id's sync (a job or a push) went through at at:
// its error clears, and a wait once it has passed (the job and the push run
// side by side: one's success never ends a wait Audible gave the other).
func (db *DB) AudibleSyncOK(ctx context.Context, id string, at time.Time) error {
	ts := timestamp(at)
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_accounts SET sync_at = ?1, sync_error = '',
		sync_retry_at = CASE WHEN sync_retry_at > ?1 THEN sync_retry_at ELSE '' END WHERE id = ?2`, ts, id)
	return err
}

// ClearAudibleSyncError forgets id's stale sync error after sync is turned
// off, reporting whether there was one (what the checklist shows). A wait
// Audible asked for stays: the device is the same, so the wait still holds
// (and a passed one is inert). This is not a successful sync, so sync_at stays
// as it was.
func (db *DB) ClearAudibleSyncError(ctx context.Context, id string) (bool, error) {
	res, err := db.sql.ExecContext(ctx, `UPDATE audible_accounts SET sync_error = ''
		WHERE id = ? AND sync_error != ''`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AudibleSyncFailed records why id's sync failed, and until when Audible asked
// the Bridge to wait (zero: no new wait). A wait is never shortened: the later
// of the one stored and retryAt stays. It never refuses the account.
func (db *DB) AudibleSyncFailed(ctx context.Context, id, why string, retryAt time.Time) error {
	retry := ""
	if !retryAt.IsZero() {
		retry = timestamp(retryAt)
	}
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_accounts SET sync_error = ?1,
		sync_retry_at = CASE WHEN ?2 > sync_retry_at THEN ?2 ELSE sync_retry_at END WHERE id = ?3`, why, retry, id)
	return err
}
