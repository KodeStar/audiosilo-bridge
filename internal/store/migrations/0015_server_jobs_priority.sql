-- bridge#13: the priority the server gave a claimed job, so a job resumed after
-- a restart or a refused token waits for the lane it was claimed for: a job of
-- interactive priority (a listener waiting) for the negotiated reserve, as when
-- it was claimed. Rows from before this migration read 0 (the ordinary lane).
ALTER TABLE server_jobs ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
