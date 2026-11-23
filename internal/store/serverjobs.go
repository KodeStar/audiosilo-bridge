package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Server job states as the Bridge records them (migration 0002). JobLost is a
// job the server no longer lets this claim report on (its lease ran out and it
// was queued again, or it was cancelled): nothing more is sent for it.
const (
	JobRunning        = "running"
	JobDone           = "done"
	JobFailed         = "failed"
	JobNeedsAttention = "needs_attention"
	JobCancelled      = "cancelled"
	JobLost           = "lost"
)

// ServerJob is one server job this Bridge claimed.
type ServerJob struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	LibraryID *int64          `json:"library_id"`
	Path      *string         `json:"path"`
	Params    json.RawMessage `json:"params"`
	Attempt   int             `json:"attempt"`
	// ServerID is the id of the server that handed the job out ('' on rows from
	// before migration 0003).
	ServerID string `json:"server_id"`
	// Priority is the priority the server gave the job (0 on rows from before
	// migration 0015): a resumed job of interactive priority waits for the
	// reserve again (jobs.launch).
	Priority int `json:"-"`
	// ShareOff says the server told the job to keep its results local (its
	// claim's answer or one since, link.Job.ShareOff; false on rows from before
	// migration 0016), so a resumed job has it before any answer. While it is
	// set the job's engine books are local (keepSharedOffLocal).
	ShareOff bool `json:"-"`
	// Pairing is the Bridge's pairing the claim was recorded under (migration
	// 0021; a random id, never shown): a row of another pairing is dropped,
	// never resumed or reported (jobs.Runner).
	Pairing  string          `json:"-"`
	State    string          `json:"state"`
	Progress json.RawMessage `json:"progress"`
	Result   json.RawMessage `json:"result"`
	// Error is a finished job's error, or, while the job runs, the message of
	// the park it reports (needs_attention), "" when it isn't parked.
	Error      string  `json:"error"`
	Reported   bool    `json:"reported"`
	ClaimedAt  string  `json:"claimed_at"`
	UpdatedAt  string  `json:"updated_at"`
	FinishedAt *string `json:"finished_at"`
}

const serverJobColumns = `id, kind, library_id, path, params, attempt, server_id, priority, share_off, pairing, state, progress, result, error,
	reported, claimed_at, updated_at, finished_at`

func scanServerJob(row interface{ Scan(...any) error }) (*ServerJob, error) {
	var (
		j                        ServerJob
		lib                      sql.NullInt64
		path, finished           sql.NullString
		params, progress, result string
	)
	if err := row.Scan(&j.ID, &j.Kind, &lib, &path, &params, &j.Attempt, &j.ServerID, &j.Priority, &j.ShareOff, &j.Pairing, &j.State, &progress, &result,
		&j.Error, &j.Reported, &j.ClaimedAt, &j.UpdatedAt, &finished); err != nil {
		return nil, err
	}
	if lib.Valid {
		j.LibraryID = &lib.Int64
	}
	if path.Valid {
		j.Path = &path.String
	}
	if finished.Valid {
		j.FinishedAt = &finished.String
	}
	j.Params, j.Progress, j.Result = json.RawMessage(params), json.RawMessage(progress), json.RawMessage(result)
	return &j, nil
}

// jsonOr is raw, or "{}" when it is empty.
func jsonOr(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// RecordClaim records a claimed job as running under attempt (from the server
// j.ServerID, under the pairing j.Pairing), replacing the row of an earlier attempt of the same job. A claim
// with share_off has the job's engine book local in the same write
// (keepSharedOffLocal): a resumed job's mapping names it already.
func (db *DB) RecordClaim(ctx context.Context, j ServerJob) error {
	now := timestamp(nowFn())
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO server_jobs (id, kind, library_id, path, params, attempt, server_id, priority, share_off, pairing, state,
		     claimed_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?)
		 ON CONFLICT(id) DO UPDATE SET kind = excluded.kind, library_id = excluded.library_id, path = excluded.path,
		     params = excluded.params, attempt = excluded.attempt, server_id = excluded.server_id,
		     priority = excluded.priority, share_off = excluded.share_off, pairing = excluded.pairing, state = 'running',
		     progress = '{}', result = '{}', error = '', reported = 0, claimed_at = excluded.claimed_at,
		     updated_at = excluded.updated_at, finished_at = NULL`,
		j.ID, j.Kind, j.LibraryID, j.Path, jsonOr(j.Params), j.Attempt, j.ServerID, j.Priority, j.ShareOff, j.Pairing, now, now); err != nil {
		return err
	}
	if j.ShareOff {
		if err := keepSharedOffLocal(ctx, tx, j.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetServerJobShareOff records that the server told attempt of a job to keep
// its results local (ServerJob.ShareOff), heard in an answer to one of its
// reports, and has the job's engine books local in the same write
// (keepSharedOffLocal). It holds for the attempt whatever its state: an answer
// read after the job ended (a Refresh that crossed a cancel) still keeps the
// book local, so a later Retry of it on the Bridge's page files nothing; a
// later claim of the job (another attempt) is left alone. It leaves updated_at
// alone: it is not the job's progress.
func (db *DB) SetServerJobShareOff(ctx context.Context, id int64, attempt int) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE server_jobs SET share_off = 1 WHERE id = ? AND attempt = ?`, id, attempt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	if err := keepSharedOffLocal(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetServerJobReport records a running job's progress and park message ("" when
// it isn't parked) in one write: the park lives in error, so a start resumes the
// job parked instead of reporting it running first.
func (db *DB) SetServerJobReport(ctx context.Context, id int64, attempt int, progress json.RawMessage, parkMsg string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE server_jobs SET progress = ?, error = ?, updated_at = ? WHERE id = ? AND attempt = ? AND state = 'running'`,
		jsonOr(progress), parkMsg, timestamp(nowFn()), id, attempt)
	return err
}

// SetServerJobProgress records a running job's progress (attempt must match).
func (db *DB) SetServerJobProgress(ctx context.Context, id int64, attempt int, progress json.RawMessage) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE server_jobs SET progress = ?, updated_at = ? WHERE id = ? AND attempt = ? AND state = 'running'`,
		jsonOr(progress), timestamp(nowFn()), id, attempt)
	return err
}

// FinishServerJob records a job's end (state one of the finished states,
// JobLost included, which needs no report), not yet reported unless lost.
func (db *DB) FinishServerJob(ctx context.Context, id int64, attempt int, state string, result json.RawMessage, errText string) error {
	_, err := db.finishServerJob(ctx, state, result, errText, ServerJob{ID: id, Attempt: attempt}, false)
	return err
}

// finishServerJob records the end of j's row (its id and attempt, and with
// claim its server and pairing too), and reports whether it did.
func (db *DB) finishServerJob(ctx context.Context, state string, result json.RawMessage, errText string, j ServerJob, claim bool) (bool, error) {
	now := timestamp(nowFn())
	reported := 0
	if state == JobLost {
		reported = 1
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE server_jobs SET state = ?, result = ?, error = ?, reported = ?, updated_at = ?, finished_at = ?
		  WHERE id = ? AND attempt = ? AND (NOT ? OR (server_id = ? AND pairing = ?))`,
		state, jsonOr(result), errText, reported, now, now, j.ID, j.Attempt, claim, j.ServerID, j.Pairing)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// LoseServerJob records a dropped job lost (FinishServerJob's JobLost) only
// while its row is still the claim it was dropped for: the same attempt,
// server and pairing. Job ids are each server's own, so a claim of another
// server or pairing may hold the row under the same id and attempt by now, and
// is left alone. It reports whether the row was the job's.
func (db *DB) LoseServerJob(ctx context.Context, j ServerJob, errText string) (bool, error) {
	return db.finishServerJob(ctx, JobLost, j.Result, errText, j, true)
}

// AdoptServerJobs gives the rows recorded before the Bridge kept a job's
// pairing (migration 0021: none recorded) a pairing at the first start since (an
// upgrade): those of server serverID (the stored pairing's, never "") to
// pairing, the one in place, so a job running then resumes under it; every
// other one (another server's, one whose server isn't known, or all of them
// while unpaired: serverID "") to orphan, an id no pairing has, so the runner
// drops it at its first look rather than hold it for the current pairing.
func (db *DB) AdoptServerJobs(ctx context.Context, serverID, pairing, orphan string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE server_jobs SET pairing = CASE WHEN ? <> '' AND server_id = ? THEN ? ELSE ? END WHERE pairing = ''`,
		serverID, serverID, pairing, orphan)
	return err
}

// MarkServerJobReported records that a finished job's report reached the server
// (or that the server had moved on, which ends the reporting too).
func (db *DB) MarkServerJobReported(ctx context.Context, id int64, attempt int) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE server_jobs SET reported = 1, updated_at = ? WHERE id = ? AND attempt = ?`,
		timestamp(nowFn()), id, attempt)
	return err
}

// GetServerJob returns one job, or ErrNotFound.
func (db *DB) GetServerJob(ctx context.Context, id int64) (*ServerJob, error) {
	j, err := scanServerJob(db.sql.QueryRowContext(ctx, `SELECT `+serverJobColumns+` FROM server_jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// OpenServerJobs returns the jobs a start must pick up again: still running,
// or finished and not yet reported. Oldest first.
func (db *DB) OpenServerJobs(ctx context.Context) ([]ServerJob, error) {
	return db.queryServerJobs(ctx,
		`SELECT `+serverJobColumns+` FROM server_jobs WHERE state = 'running' OR reported = 0 ORDER BY id`)
}

// ListServerJobs returns jobs newest first: at most limit (1-200), before the
// id before (0 for the first page).
func (db *DB) ListServerJobs(ctx context.Context, before int64, limit int) ([]ServerJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if before <= 0 {
		before = 1<<63 - 1
	}
	return db.queryServerJobs(ctx,
		`SELECT `+serverJobColumns+` FROM server_jobs WHERE id < ? ORDER BY id DESC LIMIT ?`, before, limit)
}

// EarlierBookJobs returns prior claims of this kind for the same server book.
func (db *DB) EarlierBookJobs(ctx context.Context, server, kind string, library int64, path string, before int64) ([]ServerJob, error) {
	return db.queryServerJobs(ctx, `SELECT `+serverJobColumns+` FROM server_jobs
        WHERE server_id=? AND kind=? AND library_id=? AND path=? AND id<? ORDER BY id DESC`, server, kind, library, path, before)
}

// PruneServerJobs deletes finished, reported jobs beyond the newest keep.
func (db *DB) PruneServerJobs(ctx context.Context, keep int) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`DELETE FROM server_jobs WHERE state != 'running' AND reported = 1
		   AND id NOT IN (SELECT id FROM server_jobs ORDER BY id DESC LIMIT ?)`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (db *DB) queryServerJobs(ctx context.Context, q string, args ...any) ([]ServerJob, error) {
	rows, err := db.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ServerJob{}
	for rows.Next() {
		j, err := scanServerJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}
