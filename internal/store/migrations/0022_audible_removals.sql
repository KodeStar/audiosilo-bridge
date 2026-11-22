-- An Audible account removed here whose server DELETE is still owed. Only
-- the pairing that recorded it sends it; another pairing's row is dropped.
CREATE TABLE audible_removals (
    account  TEXT PRIMARY KEY,
    pairing  TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    retry_at TEXT NOT NULL DEFAULT ''
);
