-- books.share_skipped is why a book that was to be shared was kept local
-- ('' while it shares as asked, or was never to): contributions_off when a
-- sidecars.generate job that follows it heard the server's share_off (store's
-- keepSharedOffLocal, or the contributing stage on the server's answer), and
-- no_share_job when the contributing stage found no open generate-and-share
-- job following it (a fault left the book running with nothing to ask). A
-- later run of the job reports it as its result's share_skipped. Written only
-- beside contrib_mode = 'local'; a continue that sets the mode again clears it.
ALTER TABLE books ADD COLUMN share_skipped TEXT NOT NULL DEFAULT '';
