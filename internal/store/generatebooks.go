package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GenerateBook maps a sidecars.generate server job to the engine book it created
// (migration 0003). BookID is nil once that book was deleted (a regenerate of the
// same book); EndedAt is when the job ended ("" while it is open).
type GenerateBook struct {
	ServerJobID int64
	BookID      *int64
	ServerID    string
	LibraryID   int64
	RelPath     string
	CreatedAt   string
	EndedAt     string
}

// PutGenerateBook records (or replaces) the engine book of a server job. A job
// that maps to a new book is open again, so its end is cleared.
func (db *DB) PutGenerateBook(ctx context.Context, g GenerateBook) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO generate_books (server_job_id, book_id, server_id, library_id, rel_path, created_at, ended_at)
		 VALUES (?, ?, ?, ?, ?, ?, '')
		 ON CONFLICT(server_job_id) DO UPDATE SET book_id = excluded.book_id, server_id = excluded.server_id,
		     library_id = excluded.library_id, rel_path = excluded.rel_path, ended_at = ''`,
		g.ServerJobID, g.BookID, g.ServerID, g.LibraryID, g.RelPath, timestamp(nowFn()))
	return err
}

// GetGenerateBook returns a server job's engine book mapping, or ErrNotFound.
func (db *DB) GetGenerateBook(ctx context.Context, serverJobID int64) (GenerateBook, error) {
	var (
		g      GenerateBook
		bookID sql.NullInt64
	)
	err := db.sql.QueryRowContext(ctx,
		`SELECT server_job_id, book_id, server_id, library_id, rel_path, created_at, ended_at
		   FROM generate_books WHERE server_job_id = ?`, serverJobID).
		Scan(&g.ServerJobID, &bookID, &g.ServerID, &g.LibraryID, &g.RelPath, &g.CreatedAt, &g.EndedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return GenerateBook{}, ErrNotFound
	}
	if err != nil {
		return GenerateBook{}, err
	}
	if bookID.Valid {
		g.BookID = &bookID.Int64
	}
	return g, nil
}

// EndGenerateBook records when a server job ended (done, failed or cancelled):
// the transcript retention window counts from it.
func (db *DB) EndGenerateBook(ctx context.Context, serverJobID int64, at time.Time) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE generate_books SET ended_at = ? WHERE server_job_id = ?`, timestamp(at), serverJobID)
	return err
}

// OpenGenerateJobsForBook returns the ids of the open server jobs (still running
// here) other than exceptJob that follow the engine book bookID.
func (db *DB) OpenGenerateJobsForBook(ctx context.Context, bookID, exceptJob int64) ([]int64, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT g.server_job_id FROM generate_books g JOIN server_jobs j ON j.id = g.server_job_id
		  WHERE g.book_id = ? AND g.server_job_id != ? AND j.state = 'running' ORDER BY g.server_job_id`,
		bookID, exceptJob)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// EndedGenerateBooks returns the engine books of the jobs that ended at or before
// cutoff (the transcript retention sweep's work list), each book once.
func (db *DB) EndedGenerateBooks(ctx context.Context, cutoff time.Time) ([]Book, error) {
	return db.queryBooks(ctx,
		`SELECT `+bookCols+` FROM books WHERE id IN (
		     SELECT book_id FROM generate_books WHERE book_id IS NOT NULL AND ended_at != '' AND ended_at <= ?)
		 ORDER BY id`, timestamp(cutoff))
}

// SumBudgetCostSince returns the agent spend of every book's stage runs started at
// or after since, on the same basis as SumStageRunBudgetCost (provider cost, else a
// complete configured estimate) and, like it, superseded runs included: the
// monthly budget's figure.
func (db *DB) SumBudgetCostSince(ctx context.Context, since time.Time) (float64, error) {
	var cost float64
	err := db.sql.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE
			WHEN cost_reported=1 THEN cost_usd
			WHEN estimate_complete=1 AND estimated_api_cost_usd IS NOT NULL THEN estimated_api_cost_usd
			ELSE 0 END),0)
		FROM stage_runs WHERE started_at >= ?`, timestamp(since)).Scan(&cost)
	return cost, err
}

// FinishedBook is one done book's size and agent spend, for the estimates.
type FinishedBook struct {
	Kind        string
	DurationSec float64
	Chapters    int
	CostUSD     float64
}

// FinishedBooks returns every done book's kind, duration, chapter count and
// agent spend (superseded runs included), oldest first.
func (db *DB) FinishedBooks(ctx context.Context) ([]FinishedBook, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT b.kind, b.duration_sec, b.chapters, COALESCE((SELECT SUM(CASE
			WHEN r.cost_reported=1 THEN r.cost_usd
			WHEN r.estimate_complete=1 AND r.estimated_api_cost_usd IS NOT NULL THEN r.estimated_api_cost_usd
			ELSE 0 END) FROM stage_runs r WHERE r.book_id = b.id), 0)
		   FROM books b WHERE b.state = 'done' ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FinishedBook
	for rows.Next() {
		var f FinishedBook
		if err := rows.Scan(&f.Kind, &f.DurationSec, &f.Chapters, &f.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
