-- An Audible account's pending preorders (the Bridge's fix for preorders
-- from the real API). A pending preorder isn't in the account's Audible
-- library until it is released; only the orders listing shows one, and that
-- goes back about six months. So each audible.poll remembers the preorders it
-- saw there and reports them until the library lists them or they are dropped
-- (cancelled, delivered, or released 30 days ago and still not listed), even
-- after their order leaves the listing's window.
--
-- One row per (account, ASIN): when it was ordered (ordered_at: the order's
-- date, or when first seen if the order had none), when a poll last saw it
-- pending in the orders (last_seen_at), and the title as the catalog last
-- described it (record: JSON of the title, authors, series and dates, '' until
-- a catalog read succeeds) and when (filled_at). No credential, no payment
-- field. An account's rows go with it.
CREATE TABLE audible_preorders (
    account      TEXT NOT NULL,
    asin         TEXT NOT NULL,
    ordered_at   TEXT NOT NULL DEFAULT '',
    last_seen_at TEXT NOT NULL DEFAULT '',
    record       TEXT NOT NULL DEFAULT '',
    filled_at    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (account, asin)
);
