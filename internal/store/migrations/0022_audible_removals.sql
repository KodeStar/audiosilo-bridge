-- The local account is gone. Only the pairing that owned this DELETE may
-- retry it. The existing customer hash and marketplace identify a re-add.
CREATE TABLE audible_removals (
    account       TEXT PRIMARY KEY,
    pairing       TEXT NOT NULL,
    customer_hash TEXT NOT NULL,
    marketplace   TEXT NOT NULL,
    -- Set before dispatch. A lost answer can mean the DELETE is still in flight.
    uncertain     INTEGER NOT NULL DEFAULT 0,
    attempts      INTEGER NOT NULL DEFAULT 0,
    retry_at      TEXT NOT NULL DEFAULT ''
);
