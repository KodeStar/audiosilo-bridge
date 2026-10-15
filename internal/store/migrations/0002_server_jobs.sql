-- server_jobs: the server jobs this Bridge claimed (BRIDGE-PLAN.md Phase 0), its
-- own record of running them. The server's bridge_jobs row is the intent and
-- the lease; this row is the execution truth across a restart: a job still
-- 'running' here when the Bridge starts is resumed under the same attempt (or
-- given up as 'lost' when the server says the claim no longer holds it), and a
-- finished job whose report never reached the server (reported = 0) is
-- reported again.
--
-- id is the server's job id; a job claimed again after its lease ran out
-- replaces its row with the new attempt. state: running | done | failed |
-- needs_attention | cancelled | lost. progress, result and params are JSON.
CREATE TABLE server_jobs (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,
    library_id  INTEGER,
    path        TEXT,
    params      TEXT NOT NULL DEFAULT '{}',
    attempt     INTEGER NOT NULL,
    state       TEXT NOT NULL DEFAULT 'running',
    progress    TEXT NOT NULL DEFAULT '{}',
    result      TEXT NOT NULL DEFAULT '{}',
    error       TEXT NOT NULL DEFAULT '',
    reported    INTEGER NOT NULL DEFAULT 0,
    claimed_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    finished_at TEXT
);
-- The jobs a start must pick up again: running, or finished but not reported.
CREATE INDEX idx_server_jobs_open ON server_jobs(id) WHERE state = 'running' OR reported = 0;
