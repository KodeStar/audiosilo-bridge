-- bridge#51: the place the Bridge wrote to Audible that an audible.sync has
-- already written again once (reasserted_ms, -1 none). The Audible app may
-- upload an older place over a written one, and the sync heals that once; if
-- Audible goes behind the same place again, the listener chose it there (a
-- rewind in the Audible app), so it is left alone until the Bridge writes a
-- new place.
ALTER TABLE audible_push ADD COLUMN reasserted_ms INTEGER NOT NULL DEFAULT -1;
