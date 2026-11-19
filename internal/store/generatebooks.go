package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GenerateBook maps a sidecars.generate server job to the engine book it created
// (migration 0003). BookID is nil once that book was deleted (a regenerate of the
// same book); EndedAt is when the job ended ("" while it is open); SweptAt when
// the book's transcripts were deleted ("" while they are kept).
type GenerateBook struct {
	ServerJobID int64
	BookID      *int64
	LibraryID   int64
	RelPath     string
	CreatedAt   string
	EndedAt     string
	SweptAt     string
}

// PutGenerateBook records (or replaces) the engine book of a server job: a book
// that exists already (a new one is mapped as it is made, CreateMappedBook). A
// job that maps to a new book is open again, so its end is cleared. A job that
// heard share_off has the book local at once (keepSharedOffLocal).
func (db *DB) PutGenerateBook(ctx context.Context, g GenerateBook) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := putGenerateBook(ctx, tx, g); err != nil {
		return err
	}
	return tx.Commit()
}

// execer is what putGenerateBook writes through: the database or a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// putGenerateBook writes the mapping, then keeps the book local when the job
// heard share_off: callers run it in a transaction, so the two land together.
func putGenerateBook(ctx context.Context, ex execer, g GenerateBook) error {
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO generate_books (server_job_id, book_id, library_id, rel_path, created_at, ended_at, swept_at)
		 VALUES (?, ?, ?, ?, ?, '', '')
		 ON CONFLICT(server_job_id) DO UPDATE SET book_id = excluded.book_id, library_id = excluded.library_id,
		     rel_path = excluded.rel_path, ended_at = '', swept_at = ''`,
		g.ServerJobID, g.BookID, g.LibraryID, g.RelPath, timestamp(nowFn())); err != nil {
		return err
	}
	return keepSharedOffLocal(ctx, ex, g.ServerJobID)
}

// keepSharedOffLocal is the store's share_off rule (server#218): every engine
// book a sidecars.generate job follows (its mapping) is local while the job's
// row says share_off, so the contributing stage, which reads the book's own
// mode, files nothing for it whether or not a run of the job is up. It runs
// wherever either side changes: the claim (RecordClaim), the answer heard since
// (SetServerJobShareOff) and the mapping (putGenerateBook, the continues). A
// book done keeps its mode (it shared as asked, and its job reports what it
// filed), and this rule never turns a book back: a share dropped stays dropped
// (only a later job's own continue sets the mode it asks for). Like
// SetBookChapters it leaves updated_at alone: the mode is not a change of
// pipeline position, and a bump would reorder the Running list. A book kept
// local only because no share job followed it (ShareSkippedNoShareJob, done or
// not) takes contributions_off as its reason: the share_off then outlives the
// job's row, which a later claim of the job writes over (RecordClaim), so
// ResumeShare never takes that share up again.
func keepSharedOffLocal(ctx context.Context, ex execer, serverJobID int64) error {
	_, err := ex.ExecContext(ctx,
		`UPDATE books SET contrib_mode = ?2, share_skipped = ?3
		  WHERE ((state <> 'done' AND contrib_mode <> ?2) OR share_skipped = ?4)
		    AND id IN (SELECT book_id FROM generate_books WHERE server_job_id = ?1)
		    AND EXISTS (SELECT 1 FROM server_jobs WHERE id = ?1 AND share_off = 1 AND kind = 'sidecars.generate')`,
		serverJobID, ContribModeLocal, ShareSkippedContributionsOff, ShareSkippedNoShareJob)
	return err
}

// Book.ShareSkipped values: why a book that was to be shared was kept local.
const (
	// ShareSkippedContributionsOff: the server turned the share of a
	// sidecars.generate job that follows the book off (its Contributions
	// module off, server#218).
	ShareSkippedContributionsOff = "contributions_off"
	// ShareSkippedNoShareJob: the contributing stage found no open
	// generate-and-share job following the book in issue mode, so nothing
	// could say the share still stood (a fault left the book running with no
	// job: a job's end that couldn't stop it, a lease that lapsed).
	ShareSkippedNoShareJob = "no_share_job"
)

// KeepBookLocal turns engine book id local for why (a ShareSkipped value),
// unless it is local already or done (a done book shared as asked). Like
// keepSharedOffLocal it never turns a book back and leaves updated_at alone.
// A book not found, done or local already is no error.
func (db *DB) KeepBookLocal(ctx context.Context, id int64, why string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE books SET contrib_mode = ?2, share_skipped = ?3 WHERE id = ?1 AND state <> 'done' AND contrib_mode <> ?2`,
		id, ContribModeLocal, why)
	return err
}

// shareResumableSQL holds when engine book ?3 was kept local only because no
// share job followed it (ShareSkippedNoShareJob, ?5), attempt ?2 of server job
// ?1 follows it again (its mapping open, its row running under that attempt)
// with its share on, and no sidecars.generate job mapped to the book has
// share_off (a share_off always wins).
const shareResumableSQL = `id = ?3 AND share_skipped = ?5
		    AND EXISTS (SELECT 1 FROM generate_books g JOIN server_jobs j ON j.id = g.server_job_id
		                 WHERE g.server_job_id = ?1 AND g.book_id = ?3 AND g.ended_at = '' AND j.state = 'running'
		                   AND j.attempt = ?2 AND j.share_off = 0 AND j.kind = 'sidecars.generate')
		    AND NOT EXISTS (SELECT 1 FROM generate_books g JOIN server_jobs j ON j.id = g.server_job_id
		                     WHERE g.book_id = ?3 AND j.share_off = 1 AND j.kind = 'sidecars.generate')`

// ShareResumable reports whether engine book bookID, kept local because no
// share job followed it, may be shared again by attempt of server job jobID
// (shareResumableSQL; the caller checks the job's params): a done book's
// share, which its stage can't file any more, is filed by the job itself
// before ResumeShare records it.
func (db *DB) ShareResumable(ctx context.Context, jobID int64, attempt int, bookID int64) (bool, error) {
	var ok bool
	err := db.sql.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM books WHERE `+shareResumableSQL+`)`,
		jobID, attempt, bookID, ContribModeIssue, ShareSkippedNoShareJob).Scan(&ok)
	return ok, err
}

// ResumeShare is the one way back from ShareSkippedNoShareJob: engine book
// bookID is to be shared again (contrib_mode issue, share_skipped cleared)
// while shareResumableSQL holds for attempt of server job jobID, which shares
// it (the caller checks the job's params). A book kept local for any other
// reason is never turned back. Like KeepBookLocal it leaves updated_at alone.
// It reports whether the book turned.
func (db *DB) ResumeShare(ctx context.Context, jobID int64, attempt int, bookID int64) (bool, error) {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET contrib_mode = ?4, share_skipped = '' WHERE `+shareResumableSQL,
		jobID, attempt, bookID, ContribModeIssue, ShareSkippedNoShareJob)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetGenerateBook returns a server job's engine book mapping, or ErrNotFound.
func (db *DB) GetGenerateBook(ctx context.Context, serverJobID int64) (GenerateBook, error) {
	var (
		g      GenerateBook
		bookID sql.NullInt64
	)
	err := db.sql.QueryRowContext(ctx,
		`SELECT server_job_id, book_id, library_id, rel_path, created_at, ended_at, swept_at
		   FROM generate_books WHERE server_job_id = ?`, serverJobID).
		Scan(&g.ServerJobID, &bookID, &g.LibraryID, &g.RelPath, &g.CreatedAt, &g.EndedAt, &g.SweptAt)
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

// SetGenerateBookPath records a server job's book at another path (the server
// moved the job with its book), so a restart follows the same book there.
func (db *DB) SetGenerateBookPath(ctx context.Context, serverJobID int64, relPath string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE generate_books SET rel_path = ? WHERE server_job_id = ?`, relPath, serverJobID)
	return err
}

// StopGuard is what a job's end read before it stops its engine book: the
// job's claim (its id, attempt, server and pairing) and whether the job's
// mapping recorded its end already (Ended). A later claim of the same job
// records its attempt (RecordClaim) before its attach reopens the mapping and
// re-admits the book, so does another server's claim of the same id (job ids
// are each server's own, its attempt maybe the same; a claim of a kind that
// follows no engine book is none, engineKindsSQL), and another open job
// may follow the book: either way the stop is no longer this end's to make.
// ServerID "" is a claim no runner made (a test's, a local run's): only a
// later attempt is another claim then.
type StopGuard struct {
	JobID   int64
	Attempt int
	Ended   bool
	// ServerID and Pairing are the claim's server and the Bridge's pairing it
	// was recorded under (ServerJob.ServerID, ServerJob.Pairing).
	ServerID, Pairing string
}

// args are the guard's statement arguments, ?1 to ?5 (otherClaimSQL,
// stopGuardSQL); a statement's own follow from ?6.
func (g StopGuard) args(more ...any) []any {
	return append([]any{g.JobID, g.Attempt, g.Ended, g.ServerID, g.Pairing}, more...)
}

// ErrFollowed is a guarded write (SetBookStatusUnlessFollowed, EndGenerateBook)
// refused: another claim of the job's id, or another open job, follows the
// engine book now.
var ErrFollowed = errors.New("another claim of the job, or another open job, follows the engine book")

// engineKindsSQL are the job kinds whose claims follow an engine book under
// their id (their mapping: sidecars.generate, chapters.detect and
// readalong.align). A claim of any other kind under the same id (another
// server's podcast.fetch) follows no engine book, so the book the id's mapping
// holds is still the earlier claim's to stop and end.
const engineKindsSQL = `('sidecars.generate', 'chapters.detect', 'readalong.align')`

// otherClaimSQL is true while the row of server job ?1 is another claim than
// the guard's (StopGuard.args), of a kind that follows an engine book
// (engineKindsSQL): a later attempt than ?2, or, for a claim of a server (?4,
// its pairing ?5), a claim of another server or pairing.
const otherClaimSQL = `EXISTS (SELECT 1 FROM server_jobs WHERE id = ?1 AND kind IN ` + engineKindsSQL + `
		    AND (attempt > ?2 OR (?4 != '' AND (server_id != ?4 OR pairing != ?5))))`

// openJobsSQL is the open server jobs' mappings (g): the job still running here,
// its mapping not ended. Callers add the book and the job to leave out.
const openJobsSQL = `generate_books g JOIN server_jobs j ON j.id = g.server_job_id
		  WHERE g.ended_at = '' AND j.state = 'running'`

// ErrSettled is SetBookStatusUnlessFollowed for a book that settled since the
// caller read it while its job still owns it: it finished, or its status or
// error changed (a stage's real failure written meanwhile is never overwritten).
var ErrSettled = errors.New("the engine book finished or its status changed since it was read")

// stopGuardSQL is a StopGuard holding on the books row (StopGuard.args: ?1 the
// job, ?3 Ended): no other claim of the job (otherClaimSQL), the job's mapping
// still ended when Ended, and no other open job on the book.
const stopGuardSQL = `NOT ` + otherClaimSQL + `
		    AND (?3 = 0 OR EXISTS (SELECT 1 FROM generate_books WHERE server_job_id = ?1 AND ended_at != ''))
		    AND NOT EXISTS (SELECT 1 FROM ` + openJobsSQL + ` AND g.book_id = books.id AND g.server_job_id != ?1)`

// SetBookStatusUnlessFollowed is SetBookStatus with no park code nor retry, but
// only while g holds and the book is as the caller read it (in the one statement,
// so a later claim that reopens the mapping between a check and the write is
// never stopped, nor a failure written meanwhile overwritten): no other claim of
// g's job is recorded, g's mapping is still ended when g.Ended, no other open
// job follows the book (OpenGenerateJobsForBook), and the book isn't done and
// still has fromStatus and fromErr. ErrFollowed when g doesn't hold (or the book
// went), ErrSettled when only the book changed.
func (db *DB) SetBookStatusUnlessFollowed(ctx context.Context, id int64, fromStatus, fromErr, status, errMsg string, g StopGuard) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE books SET status = ?6, error = ?7, park_code = ?8, retry_at = ?9, updated_at = ?10
		  WHERE id = ?11 AND state != 'done' AND status = ?12 AND error = ?13 AND `+stopGuardSQL,
		g.args(status, errMsg, enforceParkCode(status, ""), enforceRetryAt(status, ""), timestamp(nowFn()), id,
			fromStatus, fromErr)...)
	if err := checkAffected(res, err); !errors.Is(err, ErrNotFound) {
		return err
	}
	// Told apart in one read: settled only while g holds and the book did change.
	// A book still as read means g refused the write (another open job that has
	// ended since): ErrFollowed, so the caller tries again after its own end.
	var owned, asRead bool
	if err := db.sql.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM books WHERE id = ?6 AND `+stopGuardSQL+`),
		        EXISTS (SELECT 1 FROM books WHERE id = ?6 AND state != 'done' AND status = ?7 AND error = ?8)`,
		g.args(id, fromStatus, fromErr)...).Scan(&owned, &asRead); err != nil {
		return err
	}
	if owned && !asRead {
		return ErrSettled
	}
	return ErrFollowed
}

// EndGenerateBook records when g's claim of a server job ended (done, failed
// or cancelled): the transcript retention window counts from it. It is written
// only while no other claim of the job is recorded (in the one statement): a
// later claim, or another server's of the same id, whose attach found the
// mapping open keeps it open. ErrFollowed when one is; nil with nothing
// written when the job has no mapping. g.Ended is not read.
func (db *DB) EndGenerateBook(ctx context.Context, g StopGuard, at time.Time) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE generate_books SET ended_at = ?6 WHERE server_job_id = ?1 AND NOT `+otherClaimSQL,
		g.args(timestamp(at))...)
	if err := checkAffected(res, err); !errors.Is(err, ErrNotFound) {
		return err
	}
	if other, err := db.OtherClaim(ctx, g); err != nil || !other {
		return err
	}
	return ErrFollowed
}

// OtherClaim reports whether the row of g's job is another claim than g's (a
// later attempt, or another server's or pairing's claim of the same id,
// RecordClaim) of a kind that follows an engine book (engineKindsSQL): that
// claim follows the job's engine book now, even before its attach reopens the
// job's mapping. g.Ended is not read.
func (db *DB) OtherClaim(ctx context.Context, g StopGuard) (bool, error) {
	var other bool
	err := db.sql.QueryRowContext(ctx, `SELECT `+otherClaimSQL, g.args()...).Scan(&other)
	return other, err
}

// ReopenGenerateBook marks a server job's mapping open again (a job the server
// handed out again after its end was recorded follows its book once more).
func (db *DB) ReopenGenerateBook(ctx context.Context, serverJobID int64) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE generate_books SET ended_at = '' WHERE server_job_id = ?`, serverJobID)
	return err
}

// GenerateBookFollowed reports whether an open sidecars.generate job (a mapping
// whose end isn't recorded) follows the engine book bookID.
func (db *DB) GenerateBookFollowed(ctx context.Context, bookID int64) (bool, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM generate_books WHERE book_id = ? AND ended_at = ''`, bookID).Scan(&n)
	return n > 0, err
}

// OpenGenerateJobsForBook returns the ids of the open server jobs (still running
// here, their mapping not ended) other than exceptJob that follow the engine book
// bookID.
func (db *DB) OpenGenerateJobsForBook(ctx context.Context, bookID, exceptJob int64) ([]int64, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT g.server_job_id FROM `+openJobsSQL+` AND g.book_id = ? AND g.server_job_id != ?
		  ORDER BY g.server_job_id`,
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

// LatestGenerateBookAt returns the engine book of the newest sidecars.generate
// job for the book at (libraryID, relPath) whose book still exists (the path the
// job last followed it to), or ErrNotFound. sidecars.describe finds the run whose
// notes it reads with it when the book's source path no longer leads there, so
// the audio run beside an ebook run (AudioRunKey: it holds no notes) is passed
// over for the ebook run's own job.
func (db *DB) LatestGenerateBookAt(ctx context.Context, libraryID int64, relPath string) (int64, error) {
	var id int64
	err := db.sql.QueryRowContext(ctx,
		`SELECT g.book_id FROM generate_books g JOIN books b ON b.id = g.book_id
		  WHERE g.library_id = ? AND g.rel_path = ? AND substr(b.source_path, 1, ?) != ?
		  ORDER BY g.created_at DESC, g.server_job_id DESC LIMIT 1`,
		libraryID, relPath, len(audioRunPrefix), audioRunPrefix).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// SweepBook is an engine book whose transcripts the retention sweep may delete:
// only what the sweep needs.
type SweepBook struct {
	ID      int64
	WorkDir string
	Kind    string
	State   string
	Status  string
}

// UnsweptGenerateBooks returns the engine books whose jobs all ended, the last at or
// before cutoff, and whose transcripts weren't swept yet (the retention sweep's work
// list), each book once. A book a generate job continued (Phase 1d) has two jobs'
// mappings: its retention counts from the later job's end, never the chapters job's.
func (db *DB) UnsweptGenerateBooks(ctx context.Context, cutoff time.Time) ([]SweepBook, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT id, work_dir, kind, state, status FROM books WHERE id IN (
		     SELECT book_id FROM generate_books
		      WHERE book_id IS NOT NULL AND swept_at = ''
		      GROUP BY book_id HAVING MIN(ended_at) != '' AND MAX(ended_at) <= ?)
		 ORDER BY id`, timestamp(cutoff))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SweepBook
	for rows.Next() {
		var b SweepBook
		if err := rows.Scan(&b.ID, &b.WorkDir, &b.Kind, &b.State, &b.Status); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GenerateBookSwept reports whether the retention swept engine book bookID's
// transcripts (any job's mapping to it records the sweep).
func (db *DB) GenerateBookSwept(ctx context.Context, bookID int64) (bool, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM generate_books WHERE book_id = ? AND swept_at != ''`, bookID).Scan(&n)
	return n > 0, err
}

// MarkGenerateBookSwept records that an engine book's transcripts were deleted,
// so the sweep skips it from now on.
func (db *DB) MarkGenerateBookSwept(ctx context.Context, bookID int64, at time.Time) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE generate_books SET swept_at = ? WHERE book_id = ?`, timestamp(at), bookID)
	return err
}

// SumBudgetCostSince returns the agent spend of every book's stage runs started at
// or after since, on the same basis as SumStageRunBudgetCost (provider cost, else a
// complete configured estimate) and, like it, superseded runs included, deleted
// books' runs too (deleted_stage_spend): the monthly budget's figure.
func (db *DB) SumBudgetCostSince(ctx context.Context, since time.Time) (float64, error) {
	var cost float64
	err := db.sql.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT SUM(`+budgetCostSQL+`) FROM stage_runs WHERE started_at >= ?1), 0) +
		COALESCE((SELECT SUM(cost_usd) FROM deleted_stage_spend WHERE started_at >= ?1), 0)`,
		timestamp(since)).Scan(&cost)
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
// agent spend (superseded runs included), oldest first. A chapters-only book
// (goal chapters) is left out: it is done without characters or recaps, so its
// zero spend would drag the per-hour estimate down.
func (db *DB) FinishedBooks(ctx context.Context) ([]FinishedBook, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT b.kind, b.duration_sec, b.chapters, COALESCE((SELECT SUM(`+budgetCostSQL+`)
		     FROM stage_runs WHERE stage_runs.book_id = b.id), 0)
		   FROM books b WHERE b.state = 'done' AND b.goal = '' ORDER BY b.id`)
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
