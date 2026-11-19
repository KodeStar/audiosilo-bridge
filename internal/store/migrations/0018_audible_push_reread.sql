-- bridge#50: a cursor the server refuses sends the push loop back to the
-- start of the account's progress feed. push_reread marks that read (1 from
-- the refusal until a read reaches the feed's end): a row changed before the
-- account's baseline (push_from) is history again for it, never a new place,
-- so a server answering older places than the ones the Bridge read (one
-- restored from a backup) has none from before the baseline written over
-- Audible's.
ALTER TABLE audible_accounts ADD COLUMN push_reread INTEGER NOT NULL DEFAULT 0;
