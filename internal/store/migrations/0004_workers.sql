-- Phase 1b (BRIDGE-PLAN.md): the ASR workers.
--
-- A worker is a native process (a Mac's Metal, a GPU box) that transcribes the
-- Bridge's segments. It is paired with a token made on the Bridge's page and
-- shown once; only its sha256 is kept. Deleting the row revokes the token.
-- last_seen_at, version, platform, backend, device and model are what the
-- worker last said (its hello and its polls, written at most once a minute);
-- segments, audio_seconds and decode_seconds count the segments it finished.
CREATE TABLE workers (
    id             INTEGER PRIMARY KEY,
    name           TEXT NOT NULL,
    token_hash     TEXT NOT NULL UNIQUE,
    created_at     TEXT NOT NULL,
    last_seen_at   TEXT NOT NULL DEFAULT '',
    version        TEXT NOT NULL DEFAULT '',
    platform       TEXT NOT NULL DEFAULT '',
    backend        TEXT NOT NULL DEFAULT '',
    device         TEXT NOT NULL DEFAULT '',
    model          TEXT NOT NULL DEFAULT '',
    segments       INTEGER NOT NULL DEFAULT 0,
    audio_seconds  REAL NOT NULL DEFAULT 0,
    decode_seconds REAL NOT NULL DEFAULT 0
);
