-- Phase 2b (BRIDGE-PLAN.md): the Accounts page.
--
-- One row per external account the Bridge holds a credential for (provider
-- 'github', 'agent'). The credentials themselves live in the sealed vault
-- (internal/secrets), never here: this is what the page shows about them. login
-- is the account's name (a GitHub login; '' for an agent credential), signed_in_at
-- when it was set, last_ok_at when it last worked, refused_at when the provider
-- last refused it ('' while it works) and last_error why, in words.
CREATE TABLE accounts (
    provider     TEXT PRIMARY KEY,
    login        TEXT NOT NULL DEFAULT '',
    signed_in_at TEXT NOT NULL DEFAULT '',
    last_ok_at   TEXT NOT NULL DEFAULT '',
    refused_at   TEXT NOT NULL DEFAULT '',
    last_error   TEXT NOT NULL DEFAULT ''
);
