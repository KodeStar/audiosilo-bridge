-- server#218: the server's share_off for a claimed job (its Contributions module
-- off while a sidecars.generate job with contribute "issue" runs), heard at the
-- claim or in an answer since, so a job resumed after a restart keeps its
-- results local even when the server can't be reached at its first report.
-- Rows from before this migration read 0 (share as asked).
ALTER TABLE server_jobs ADD COLUMN share_off INTEGER NOT NULL DEFAULT 0;
