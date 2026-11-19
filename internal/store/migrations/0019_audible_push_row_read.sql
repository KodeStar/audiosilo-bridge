-- bridge#50: when this Bridge last read a place from the server's feed for
-- the title (row_read, its own clock, '' never). row_seen is that time too
-- until a re-arm (RearmAudiblePush) moves it to count the push's wait again;
-- a read from the feed's start after a refused cursor compares a row's
-- updated_at with the read itself, so a place written on the server before a
-- re-arm, and never read, is still a new one. A row re-armed before this
-- migration keeps the re-arm's time: its read is not known.
ALTER TABLE audible_push ADD COLUMN row_read TEXT NOT NULL DEFAULT '';
UPDATE audible_push SET row_read = row_seen;
