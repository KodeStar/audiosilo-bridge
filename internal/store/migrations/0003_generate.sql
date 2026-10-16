-- Phase 1a (BRIDGE-PLAN.md): the sidecars.generate job and the server's identity.
--
-- server_jobs.server_id is the id of the server that handed the job out (the
-- pairing answer's server_id): a job id is only meaningful on its own server, so
-- a start that finds an open row of another server treats it as lost instead of
-- reporting it to the wrong one. '' on rows from before this migration.
ALTER TABLE server_jobs ADD COLUMN server_id TEXT NOT NULL DEFAULT '';

-- books.contrib_mode is the contribution mode of one book ('' = the Bridge's
-- contribution.mode; local | issue per sidecars.generate job), which the
-- contributing stage reads before the executor's configured mode.
ALTER TABLE books ADD COLUMN contrib_mode TEXT NOT NULL DEFAULT '';

-- generate_books maps a server job (sidecars.generate) to the engine book it
-- created, so a job resumed after a restart follows the same book. book_id is
-- set null when the engine book is deleted (a regenerate of the same book).
-- ended_at is when the job ended (done, failed or cancelled; '' while open): the
-- transcript retention window (transcripts.retention_days) counts from it.
-- swept_at is when the book's transcripts were deleted ('' until then), so the
-- daily sweep never looks at a swept book again.
CREATE TABLE generate_books (
    server_job_id INTEGER PRIMARY KEY,
    book_id       INTEGER REFERENCES books(id) ON DELETE SET NULL,
    library_id    INTEGER NOT NULL,
    rel_path      TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    ended_at      TEXT NOT NULL DEFAULT '',
    swept_at      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_generate_books_book ON generate_books(book_id);

-- The monthly budget, the page and the heartbeat's estimates sum this month's
-- stage runs by started_at.
CREATE INDEX idx_stage_runs_started ON stage_runs(started_at);

-- deleted_stage_spend keeps the budget-basis spend of a deleted engine book's
-- stage runs (a regenerate deletes the earlier book, and its stage_runs go with
-- it), one row per run by started_at, so the monthly budget still counts it.
CREATE TABLE deleted_stage_spend (
    started_at TEXT NOT NULL,
    cost_usd   REAL NOT NULL
);
CREATE INDEX idx_deleted_stage_spend_started ON deleted_stage_spend(started_at);
