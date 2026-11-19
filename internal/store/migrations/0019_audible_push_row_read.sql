-- bridge#50: the updated_at (the server's clock) of the feed row last recorded
-- for the title (row_read, '' none or not known). A read from the feed's start
-- after a refused cursor drops a row no later than it, server time against
-- server time; a re-arm (RearmAudiblePush) moves row_seen, never this. A row
-- recorded before this migration starts with none (the baseline's line alone
-- applies to it until it is read again): row_seen is the Bridge's clock, and
-- one that was wrong when it stamped the read would drop a newer place.
ALTER TABLE audible_push ADD COLUMN row_read TEXT NOT NULL DEFAULT '';
