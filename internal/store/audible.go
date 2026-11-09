package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AudibleAccount is what the Bridge keeps about one Audible account (migration
// 0009): its label, marketplace and mapping, and how its polls went. The
// sign-in itself is in the vault (audible_<id>), never here.
type AudibleAccount struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Marketplace string `json:"marketplace"`
	// CustomerHash is the sha256 of the Amazon customer id (never the id).
	CustomerHash string `json:"-"`
	// ServerID is the server the mapping was last recorded on.
	ServerID    string `json:"server_id"`
	UserID      int64  `json:"user_id"`
	UserName    string `json:"user_name"`
	LibraryID   int64  `json:"library_id"`
	Policy      string `json:"policy"`
	Sync        bool   `json:"sync"`
	CreatedAt   string `json:"created_at"`
	LastOKAt    string `json:"last_ok_at"`
	LastPollAt  string `json:"last_poll_at"`
	LastTitles  int    `json:"last_titles"`
	LastMatched int    `json:"last_matched"`
	RefusedAt   string `json:"refused_at"`
	LastError   string `json:"last_error"`
	// DeviceProfile is the device profile the sign-in registered with
	// (audible.ProfileVersion then, migration 0010; "" before it was kept).
	DeviceProfile string `json:"device_profile"`
	// RefusalKind is why the account is refused (migration 0011, bridge#46):
	// RefusalSignIn or RefusalDevice, "" while it isn't.
	RefusalKind string `json:"refusal_kind"`
	// PushCursor and PushFrom are the push loop's place in the server's
	// progress feed and the moment a read of it first reached its end for this
	// mapping (the baseline's end: until then older rows are history); both ""
	// until then.
	PushCursor string `json:"-"`
	PushFrom   string `json:"-"`
	// SyncAt is when a sync job or a push last went through, SyncError why the
	// last one failed ("" when it went through), SyncRetryAt until when Audible
	// asked the Bridge to wait.
	SyncAt      string `json:"sync_at"`
	SyncError   string `json:"sync_error"`
	SyncRetryAt string `json:"sync_retry_at"`
}

// The kinds of an Audible refusal (AudibleAccount.RefusalKind).
const (
	// RefusalSignIn: Audible refused the sign-in (401/403 on a signed call);
	// any call Audible answers ends it, as does a sign-in again.
	RefusalSignIn = "sign_in"
	// RefusalDevice: Audible denied the device its licenses (a device on an
	// older profile, "customer throttled"); its library still lists, so only a
	// sign-in again ends it.
	RefusalDevice = "device"
)

// OnOtherServer reports whether a is mapped on a server other than serverID
// (the pairing's; "" while unpaired, which is never "other").
func (a AudibleAccount) OnOtherServer(serverID string) bool {
	return serverID != "" && a.ServerID != serverID
}

const audibleColumns = `id, label, marketplace, customer_hash, server_id, user_id, user_name, library_id, policy, sync,
	created_at, last_ok_at, last_poll_at, last_titles, last_matched, refused_at, last_error, device_profile, refusal_kind,
	push_cursor, push_from, sync_at, sync_error, sync_retry_at`

func scanAudibleAccount(row interface{ Scan(...any) error }) (AudibleAccount, error) {
	var a AudibleAccount
	err := row.Scan(&a.ID, &a.Label, &a.Marketplace, &a.CustomerHash, &a.ServerID, &a.UserID, &a.UserName, &a.LibraryID,
		&a.Policy, &a.Sync, &a.CreatedAt, &a.LastOKAt, &a.LastPollAt, &a.LastTitles, &a.LastMatched, &a.RefusedAt, &a.LastError,
		&a.DeviceProfile, &a.RefusalKind, &a.PushCursor, &a.PushFrom, &a.SyncAt, &a.SyncError, &a.SyncRetryAt)
	return a, err
}

// remapAudible starts the push loop's feed again, and forgets the places
// waiting to be pushed and the listed titles, when a's row (read in tx) is
// mapped to another server, person or library than a's: the next audible.sync
// lists its titles again. A stale sync error clears with the mapping; a wait
// Audible asked for stays (the device is the same). A new account has nothing
// to forget.
func remapAudible(ctx context.Context, tx *sql.Tx, a AudibleAccount) error {
	var server string
	var user, library int64
	err := tx.QueryRowContext(ctx, `SELECT server_id, user_id, library_id FROM audible_accounts WHERE id = ?`, a.ID).
		Scan(&server, &user, &library)
	if errors.Is(err, sql.ErrNoRows) || err == nil && server == a.ServerID && user == a.UserID && library == a.LibraryID {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE audible_accounts SET push_cursor = '', push_from = '',
		sync_error = '' WHERE id = ?`, a.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE audible_push SET want_ms = NULL, listed = 0, row_ms = -1, row_seen = ''
		WHERE account = ?`, a.ID)
	return err
}

// AudibleAccounts lists the accounts, oldest first.
func (db *DB) AudibleAccounts(ctx context.Context) ([]AudibleAccount, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+audibleColumns+` FROM audible_accounts ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AudibleAccount{}
	for rows.Next() {
		a, err := scanAudibleAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAudibleAccount returns one account, or ErrNotFound.
func (db *DB) GetAudibleAccount(ctx context.Context, id string) (AudibleAccount, error) {
	a, err := scanAudibleAccount(db.sql.QueryRowContext(ctx, `SELECT `+audibleColumns+` FROM audible_accounts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AudibleAccount{}, ErrNotFound
	}
	return a, err
}

// AudibleAccountByCustomer returns the account signed in to the Amazon account
// whose customer id hashes to hash in marketplace, or ErrNotFound.
func (db *DB) AudibleAccountByCustomer(ctx context.Context, hash, marketplace string) (AudibleAccount, error) {
	if hash == "" {
		return AudibleAccount{}, ErrNotFound
	}
	// customer_hash <> '' lets SQLite use the partial unique index.
	a, err := scanAudibleAccount(db.sql.QueryRowContext(ctx, `SELECT `+audibleColumns+` FROM audible_accounts
		WHERE customer_hash = ? AND customer_hash <> '' AND marketplace = ?`, hash, marketplace))
	if errors.Is(err, sql.ErrNoRows) {
		return AudibleAccount{}, ErrNotFound
	}
	return a, err
}

// SignInAudibleAccount records a new sign-in of a (insert, or a known id's
// sign-in replaced): its label, marketplace, customer hash, mapping and device
// profile are a's, it works as of at, and any refusal or error is forgotten. A
// known account keeps its created_at and poll counts.
func (db *DB) SignInAudibleAccount(ctx context.Context, a AudibleAccount, at time.Time) error {
	ts := timestamp(at)
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := remapAudible(ctx, tx, a); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audible_accounts (id, label, marketplace, customer_hash, server_id, user_id,
			user_name, library_id, policy, sync, created_at, last_ok_at, device_profile)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET label = excluded.label, marketplace = excluded.marketplace,
			customer_hash = excluded.customer_hash, server_id = excluded.server_id, user_id = excluded.user_id,
			user_name = excluded.user_name, library_id = excluded.library_id, policy = excluded.policy,
			sync = excluded.sync, last_ok_at = excluded.last_ok_at, device_profile = excluded.device_profile,
			refused_at = '', last_error = '', refusal_kind = '', sync_error = '', sync_retry_at = ''`,
		a.ID, a.Label, a.Marketplace, a.CustomerHash, a.ServerID, a.UserID, a.UserName, a.LibraryID, a.Policy, a.Sync, ts, ts,
		a.DeviceProfile); err != nil {
		return err
	}
	return tx.Commit()
}

// MapAudibleAccount records a's mapping (label, server, user, library, policy,
// sync) on its row; one to another server, person or library starts the push
// loop's feed again and forgets the places waiting to be pushed. ErrNotFound:
// no such account.
func (db *DB) MapAudibleAccount(ctx context.Context, a AudibleAccount) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := remapAudible(ctx, tx, a); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE audible_accounts SET label = ?, server_id = ?, user_id = ?, user_name = ?,
		library_id = ?, policy = ?, sync = ? WHERE id = ?`, a.Label, a.ServerID, a.UserID, a.UserName, a.LibraryID, a.Policy, a.Sync, a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// AudiblePolled records a complete poll of id at at: Audible answered (a
// sign-in refusal ends), titles and matched are its counts, and the last error
// clears. A device refusal (RefusalDevice) stays: a device Audible denies
// licenses still lists its library, and only a new sign-in ends that one.
func (db *DB) AudiblePolled(ctx context.Context, id string, titles, matched int, at time.Time) error {
	ts := timestamp(at)
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_accounts SET last_ok_at = ?, last_poll_at = ?, last_titles = ?,
		last_matched = ?,
		last_error = CASE WHEN refused_at <> '' AND refusal_kind = ?5 THEN last_error ELSE '' END,
		refused_at = CASE WHEN refusal_kind = ?5 THEN refused_at ELSE '' END,
		refusal_kind = CASE WHEN refusal_kind = ?5 THEN refusal_kind ELSE '' END
		WHERE id = ?6`, ts, ts, titles, matched, RefusalDevice, id)
	return err
}

// RefuseAudibleAccount records that Audible refused id at at: its kind
// (RefusalSignIn or RefusalDevice) and why, in words.
func (db *DB) RefuseAudibleAccount(ctx context.Context, id, kind, why string, at time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_accounts SET refused_at = ?, refusal_kind = ?, last_error = ? WHERE id = ?`,
		timestamp(at), kind, why, id)
	return err
}

// AudibleAccepted ends id's refusal when it is of kind (Audible answered a
// signed call, so a sign-in it refused before is accepted again), reporting
// whether it ended one. A refusal of another kind stays.
func (db *DB) AudibleAccepted(ctx context.Context, id, kind string) (bool, error) {
	res, err := db.sql.ExecContext(ctx, `UPDATE audible_accounts SET refused_at = '', last_error = '', refusal_kind = ''
		WHERE id = ? AND refused_at <> '' AND refusal_kind = ?`, id, kind)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// AudibleAccountError records why id's last poll failed (not a refusal). A
// refused account keeps its refusal's reason: that is what it needs fixed.
func (db *DB) AudibleAccountError(ctx context.Context, id, why string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE audible_accounts SET last_error = ? WHERE id = ? AND refused_at = ''`, why, id)
	return err
}

// DeleteAudibleAccount forgets id, its push rows and its remembered preorders.
func (db *DB) DeleteAudibleAccount(ctx context.Context, id string) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM audible_push WHERE account = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audible_preorders WHERE account = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audible_accounts WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
