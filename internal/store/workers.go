package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Worker is one ASR worker paired with this Bridge (migration 0004). The token
// itself is never stored, only its hash.
type Worker struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	LastSeenAt string  `json:"last_seen_at"`
	Version    string  `json:"version"`
	Platform   string  `json:"platform"`
	Backend    string  `json:"backend"`
	Device     string  `json:"device"`
	Model      string  `json:"model"`
	Segments   int64   `json:"segments"`
	AudioSec   float64 `json:"audio_seconds"`
	DecodeSec  float64 `json:"decode_seconds"`
}

// WorkerInfo is what a worker says about itself in its hello.
type WorkerInfo struct {
	Version, Platform, Backend, Device, Model string
}

const workerColumns = `id, name, created_at, last_seen_at, version, platform, backend, device, model,
	segments, audio_seconds, decode_seconds`

func scanWorker(row interface{ Scan(...any) error }) (*Worker, error) {
	var w Worker
	err := row.Scan(&w.ID, &w.Name, &w.CreatedAt, &w.LastSeenAt, &w.Version, &w.Platform, &w.Backend,
		&w.Device, &w.Model, &w.Segments, &w.AudioSec, &w.DecodeSec)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// CreateWorker records a new worker under its token's hash.
func (db *DB) CreateWorker(ctx context.Context, name, tokenHash string) (*Worker, error) {
	res, err := db.sql.ExecContext(ctx, `INSERT INTO workers (name, token_hash, created_at) VALUES (?, ?, ?)`,
		name, tokenHash, timestamp(nowFn()))
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return db.GetWorker(ctx, id)
}

// GetWorker returns one worker, or ErrNotFound.
func (db *DB) GetWorker(ctx context.Context, id int64) (*Worker, error) {
	w, err := scanWorker(db.sql.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// WorkerByTokenHash returns the worker a token's hash names, or ErrNotFound.
func (db *DB) WorkerByTokenHash(ctx context.Context, tokenHash string) (*Worker, error) {
	w, err := scanWorker(db.sql.QueryRowContext(ctx, `SELECT `+workerColumns+` FROM workers WHERE token_hash = ?`, tokenHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// ListWorkers returns every worker, oldest first.
func (db *DB) ListWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT `+workerColumns+` FROM workers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Worker{}
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// DeleteWorker removes a worker, which revokes its token (ErrNotFound when
// there is none).
func (db *DB) DeleteWorker(ctx context.Context, id int64) error {
	return checkAffected(db.sql.ExecContext(ctx, `DELETE FROM workers WHERE id = ?`, id))
}

// SetWorkerInfo records what a worker said in its hello, and that it was seen.
func (db *DB) SetWorkerInfo(ctx context.Context, id int64, info WorkerInfo, seen time.Time) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE workers SET version = ?, platform = ?, backend = ?, device = ?, model = ?, last_seen_at = ? WHERE id = ?`,
		info.Version, info.Platform, info.Backend, info.Device, info.Model, timestamp(seen), id)
	return err
}

// TouchWorker records that a worker was seen.
func (db *DB) TouchWorker(ctx context.Context, id int64, seen time.Time) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE workers SET last_seen_at = ? WHERE id = ?`, timestamp(seen), id)
	return err
}

// AddWorkerSegment counts one finished segment for a worker.
func (db *DB) AddWorkerSegment(ctx context.Context, id int64, audioSec, decodeSec float64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE workers SET segments = segments + 1, audio_seconds = audio_seconds + ?, decode_seconds = decode_seconds + ?
		  WHERE id = ?`, audioSec, decodeSec, id)
	return err
}
