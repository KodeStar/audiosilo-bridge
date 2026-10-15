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
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	LibraryID  *int64          `json:"library_id"`
	Path       *string         `json:"path"`
	Params     json.RawMessage `json:"params"`
	Attempt    int             `json:"attempt"`
	State      string          `json:"state"`
	Progress   json.RawMessage `json:"progress"`
	Result     json.RawMessage `json:"result"`
	Error      string          `json:"error"`
	Reported   bool            `json:"reported"`
	ClaimedAt  string          `json:"claimed_at"`
	UpdatedAt  string          `json:"updated_at"`
	FinishedAt *string         `json:"finished_at"`
}

const serverJobColumns = `id, kind, library_id, path, params, attempt, state, progress, result, error,
	reported, claimed_at, updated_at, finished_at`

func scanServerJob(row interface{ Scan(...any) error }) (*ServerJob, error) {
	var (
		j                        ServerJob
		lib                      sql.NullInt64
		path, finished           sql.NullString
		params, progress, result string
	)
	if err := row.Scan(&j.ID, &j.Kind, &lib, &path, &params, &j.Attempt, &j.State, &progress, &result,
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

// RecordClaim records a claimed job as running under attempt, replacing the
// row of an earlier attempt of the same job.
func (db *DB) RecordClaim(ctx context.Context, j ServerJob) error {
	now := timestamp(nowFn())
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO server_jobs (id, kind, library_id, path, params, attempt, state, claimed_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'running', ?, ?)
		 ON CONFLICT(id) DO UPDATE SET kind = excluded.kind, library_id = excluded.library_id, path = excluded.path,
		     params = excluded.params, attempt = excluded.attempt, state = 'running', progress = '{}', result = '{}',
		     error = '', reported = 0, claimed_at = excluded.claimed_at, updated_at = excluded.updated_at,
		     finished_at = NULL`,
		j.ID, j.Kind, j.LibraryID, j.Path, jsonOr(j.Params), j.Attempt, now, now)
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
	now := timestamp(nowFn())
	reported := 0
	if state == JobLost {
		reported = 1
	}
	_, err := db.sql.ExecContext(ctx,
		`UPDATE server_jobs SET state = ?, result = ?, error = ?, reported = ?, updated_at = ?, finished_at = ?
		  WHERE id = ? AND attempt = ?`,
		state, jsonOr(result), errText, reported, now, now, id, attempt)
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

// DeleteSetting removes a key/value setting (a no-op when absent).
func (db *DB) DeleteSetting(ctx context.Context, key string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}
