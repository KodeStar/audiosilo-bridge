-- bridge#50, bridge#80: the push loop's health in columns of its own. Until
-- now the audible.sync job and the push loop shared sync_at and sync_error, so
-- a push that went through cleared the job's failure (and a job's success the
-- push's), and each one's failure overwrote the other's. sync_at and
-- sync_error are now the job's alone; push_at is when a push last went
-- through, push_error why the last one failed ('' when it went through).
-- sync_retry_at stays shared: it is Audible's wait for the device, which both
-- honour. A sync_error the push wrote before this migration clears at the
-- next job that goes through.
ALTER TABLE audible_accounts ADD COLUMN push_at TEXT NOT NULL DEFAULT '';
ALTER TABLE audible_accounts ADD COLUMN push_error TEXT NOT NULL DEFAULT '';
