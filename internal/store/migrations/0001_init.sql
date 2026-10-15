-- AudioSilo Bridge: the engine's schema, squashed from audiosilo-sidecars'
-- migrations 0001-0013 at the import (BRIDGE-PLAN.md Phase 0). The Bridge's
-- library scan, its candidate overrides and its library sightings were not
-- carried over: the server's index is the library now.
--
-- SQLite is the SCHEDULING truth (which book is at which stage); the work-dir
-- artifacts + _done/<stage>.json sentinels are the CONTENT truth. Startup
-- reconciles the two. Durable auth for the Bridge's own page (password hash,
-- sessions) also lives here so the Bridge keeps a single state file.

-- books tracks each pipeline book through the engine. source_path is the
-- identity (a book is enqueued once; on the Bridge, its absolute path through
-- the library mapping); work_dir is where its scratch and artifacts live. JSON
-- columns (authors, narrators, identity_sources, coverage) hold the shapes the
-- coverage client produces without reshaping the schema.
CREATE TABLE books (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    source_path       TEXT NOT NULL UNIQUE,
    work_dir          TEXT NOT NULL,
    title             TEXT NOT NULL,
    authors           TEXT NOT NULL DEFAULT '[]',   -- JSON array of names
    -- The narrator credits, so the contributing stage can compose a core
    -- (add-work) proposal (metaissue requires at least one narrator).
    narrators         TEXT NOT NULL DEFAULT '[]',   -- JSON array of names
    series            TEXT NOT NULL DEFAULT '',
    series_pos        TEXT NOT NULL DEFAULT '',
    asin              TEXT NOT NULL DEFAULT '',
    isbn              TEXT NOT NULL DEFAULT '',
    identity_sources  TEXT NOT NULL DEFAULT '{}',   -- JSON provenance map
    -- The work the book was matched to when it was enqueued, so the pipeline and
    -- contribution stages can reference it without re-resolving. Advisory.
    work_id           TEXT NOT NULL DEFAULT '',
    state             TEXT NOT NULL DEFAULT 'queued',
    status            TEXT NOT NULL DEFAULT ''
        CHECK (status IN ('', 'paused', 'needs_attention', 'failed')),
    error             TEXT NOT NULL DEFAULT '',
    -- The typed park reason beside the free-text error: set whenever status
    -- becomes needs_attention, cleared whenever status clears.
    park_code         TEXT NOT NULL DEFAULT '',
    -- An RFC3339 (UTC) instant at which the scheduler may re-admit a book that
    -- parked on a transient agent condition (agent_unavailable /
    -- agent_rate_limited); '' = no scheduled retry. Set only alongside a
    -- needs_attention status and cleared whenever status clears.
    retry_at          TEXT NOT NULL DEFAULT '',
    coverage          TEXT NOT NULL DEFAULT '',      -- JSON coverage snapshot ('' = unknown)
    -- The book's on-disk scratch (chapters + durables), written by the split
    -- stage and recomputed by PurgeScratch, so no read walks the work dir.
    scratch_bytes     INTEGER NOT NULL DEFAULT 0,
    -- The manifest chapter count (0 = not yet known), the ETA engine's per-book
    -- chapter total for the per-chapter stages.
    chapters          INTEGER NOT NULL DEFAULT 0,
    -- Total audio duration in seconds (0 = not yet known).
    duration_sec      REAL NOT NULL DEFAULT 0,
    batch_id          TEXT NOT NULL DEFAULT 'legacy',
    -- 'audio' runs inspect -> split -> ASR -> QA -> spelling before the authoring
    -- tail; 'ebook' runs extracting over ebook_path and skips all of it.
    kind              TEXT NOT NULL DEFAULT 'audio',
    -- The DRM-free .epub the pipeline reads ('' for an audio book). For an epub
    -- beside an audiobook, source_path stays the audiobook folder (the identity,
    -- and its tags carry the ASIN) while ebook_path names the epub in it; for an
    -- ebook-only book the two are the same path.
    ebook_path        TEXT NOT NULL DEFAULT '',
    -- Word count of the extracted chapter universe (0 = unknown / audio).
    words             INTEGER NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);
CREATE INDEX idx_books_state ON books(state);
CREATE INDEX idx_books_batch ON books(batch_id);

-- stage_runs is the per-execution ledger. An open run (finished_at IS NULL) is
-- an in-flight stage; startup closes any it finds as interrupted (ok=0). A run
-- with ok=1 is the DB's claim that the stage completed (cross-checked against the
-- sentinel on disk during reconcile).
--
-- Agent usage accumulates onto the OPEN run for its (book, stage), so a crash
-- keeps spend already incurred. cost_usd is the provider-reported total; a zero
-- is not assumed to be free: cost_reported says whether the provider gave one,
-- and estimated_api_cost_usd is NULL unless the pricing table priced every
-- invocation. superseded = 1 marks an ok=1 run whose success no longer counts
-- for scheduling (a Retry reset that stage) but whose cost is still real spend:
-- round counters read `ok=1 AND superseded=0`, cost aggregates read every row.
CREATE TABLE stage_runs (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id                INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    stage                  TEXT NOT NULL,
    attempt                INTEGER NOT NULL DEFAULT 1,
    started_at             TEXT NOT NULL,
    finished_at            TEXT,
    ok                     INTEGER,                    -- NULL=running, 0=failed, 1=ok
    metrics                TEXT NOT NULL DEFAULT '{}', -- JSON
    model                  TEXT NOT NULL DEFAULT '',
    input_tokens           INTEGER NOT NULL DEFAULT 0,
    output_tokens          INTEGER NOT NULL DEFAULT 0,
    cost_usd               REAL NOT NULL DEFAULT 0,
    superseded             INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens      INTEGER NOT NULL DEFAULT 0,
    cost_reported          INTEGER NOT NULL DEFAULT 0,
    estimated_api_cost_usd REAL,
    estimate_complete      INTEGER NOT NULL DEFAULT 0,
    heartbeat_at           TEXT NOT NULL DEFAULT '',
    progress_at            TEXT NOT NULL DEFAULT '',
    process_id             INTEGER NOT NULL DEFAULT 0,
    process_active         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_stage_runs_book ON stage_runs(book_id);

-- progress is the live within-stage counter (chapter i/N, chunk i/N). One row
-- per (book, stage), last-write-wins.
CREATE TABLE progress (
    book_id INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    stage   TEXT NOT NULL,
    done    INTEGER NOT NULL DEFAULT 0,
    total   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (book_id, stage)
);

-- events is the durable event log (the supervisor reads it, so it is engine
-- state). The hub remains the live fan-out; every published (non-heartbeat)
-- event is also appended here. book_id is NULL for Bridge-wide events.
CREATE TABLE events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    hub_id  INTEGER NOT NULL DEFAULT 0,               -- the hub's monotonic event id
    ts      TEXT NOT NULL,
    type    TEXT NOT NULL,
    book_id INTEGER,
    payload TEXT NOT NULL DEFAULT '{}'                -- JSON
);
CREATE INDEX idx_events_ts ON events(ts);

-- rates is the EWMA per-stage unit-rate table the ETA engine reads.
CREATE TABLE rates (
    stage      TEXT PRIMARY KEY,
    unit_rate  REAL NOT NULL,
    updated_at TEXT NOT NULL
);

-- settings is a small key/value store for durable state that is not config.yaml,
-- including the admin password hash (auth.password_hash).
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- sessions holds live session tokens of the Bridge's own page by SHA-256 hash
-- only (never the raw token).
CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);

-- Contribution tracking: a book's validated sidecars contributed as intake
-- issues or a local export; one row per (book_id, kind) records the live status
-- of each contributed artifact, and the unique index makes UpsertContribution
-- idempotent, so a crash between submit and sentinel never double-posts.
-- issue_seen_at is the intake issue's GitHub updated_at when the poller last
-- read the intake bot's verdict comments for the row.
CREATE TABLE contributions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    book_id       INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL CHECK (kind IN ('characters', 'recaps', 'core')),
    mode          TEXT NOT NULL CHECK (mode IN ('issue', 'pr', 'local')),
    repo          TEXT NOT NULL DEFAULT '',
    number        INTEGER NOT NULL DEFAULT 0,   -- issue number (issue mode) or PR number (pr mode)
    url           TEXT NOT NULL DEFAULT '',
    pr_number     INTEGER NOT NULL DEFAULT 0,   -- intake bot PR number (issue mode)
    pr_url        TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL CHECK (status IN
        ('submitted', 'pr_open', 'merged', 'closed', 'local', 'already_covered')),
    note          TEXT NOT NULL DEFAULT '',     -- e.g. "labels missing - needs maintainer label"
    issue_seen_at TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE UNIQUE INDEX contributions_book_kind ON contributions(book_id, kind);

-- Bounded batch supervision. Supervision is an orchestration concern, never a
-- production stage: supervisor_runs has no stage name and is deliberately
-- separate from stage_runs. 'legacy' is the batch a book without one belongs to.
CREATE TABLE batches (
    id         TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);
INSERT INTO batches(id, created_at) VALUES ('legacy', '1970-01-01T00:00:00.000000000Z');

CREATE TABLE supervisor_runs (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    incident_key             TEXT NOT NULL DEFAULT '',
    batch_id                 TEXT NOT NULL REFERENCES batches(id),
    book_id                  INTEGER REFERENCES books(id) ON DELETE SET NULL,
    stage_run_id             INTEGER REFERENCES stage_runs(id) ON DELETE SET NULL,
    trigger                  TEXT NOT NULL,
    diagnosis                TEXT NOT NULL DEFAULT '',
    confidence               REAL NOT NULL DEFAULT 1,
    evidence                 TEXT NOT NULL DEFAULT '[]',
    decision                 TEXT NOT NULL DEFAULT '',
    selected_action          TEXT NOT NULL DEFAULT 'observe',
    suggested_retry_limit    INTEGER NOT NULL DEFAULT 0,
    suggested_termination_limit INTEGER NOT NULL DEFAULT 0,
    action_outcome           TEXT NOT NULL DEFAULT '',
    automatic                INTEGER NOT NULL DEFAULT 0,
    approval_required        INTEGER NOT NULL DEFAULT 0,
    state                    TEXT NOT NULL DEFAULT 'open',
    model                    TEXT NOT NULL DEFAULT '',
    backend                  TEXT NOT NULL DEFAULT '',
    model_calls              INTEGER NOT NULL DEFAULT 0,
    input_tokens             INTEGER NOT NULL DEFAULT 0,
    output_tokens            INTEGER NOT NULL DEFAULT 0,
    cached_tokens            INTEGER NOT NULL DEFAULT 0,
    provider_cost_usd        REAL,
    provider_cost_complete   INTEGER NOT NULL DEFAULT 0,
    estimated_api_cost_usd   REAL,
    estimate_complete        INTEGER NOT NULL DEFAULT 0,
    pricing_version          TEXT NOT NULL DEFAULT '',
    started_at               TEXT NOT NULL,
    completed_at             TEXT
);
CREATE INDEX idx_supervisor_runs_batch ON supervisor_runs(batch_id, id DESC);
CREATE INDEX idx_supervisor_runs_book ON supervisor_runs(book_id, id DESC);
CREATE INDEX idx_supervisor_runs_incident ON supervisor_runs(incident_key, id DESC);

-- The per-agent-invocation ledger. stage_runs remains the stage summary; these
-- rows are the source of truth for child process liveness and invocation-level
-- spend. Failed, cancelled and retried invocations are never removed.
CREATE TABLE agent_invocations (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    stage_run_id             INTEGER NOT NULL REFERENCES stage_runs(id) ON DELETE CASCADE,
    book_id                  INTEGER NOT NULL REFERENCES books(id) ON DELETE CASCADE,
    stage                    TEXT NOT NULL,
    work_unit                TEXT NOT NULL,
    backend                  TEXT NOT NULL DEFAULT '',
    model                    TEXT NOT NULL DEFAULT '',
    process_id               INTEGER NOT NULL DEFAULT 0,
    active                   INTEGER NOT NULL DEFAULT 1,
    heartbeat_at             TEXT NOT NULL,
    progress_at              TEXT NOT NULL,
    started_at               TEXT NOT NULL,
    completed_at             TEXT,
    status                   TEXT NOT NULL DEFAULT 'running'
        CHECK (status IN ('running','success','validation_failed','failure','cancelled')),
    input_tokens             INTEGER NOT NULL DEFAULT 0,
    output_tokens            INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens        INTEGER NOT NULL DEFAULT 0,
    cost_usd                 REAL NOT NULL DEFAULT 0,
    cost_reported            INTEGER NOT NULL DEFAULT 0,
    estimated_api_cost_usd   REAL,
    estimate_complete        INTEGER NOT NULL DEFAULT 0,
    error                    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_agent_invocations_stage_run ON agent_invocations(stage_run_id, id);
CREATE INDEX idx_agent_invocations_active ON agent_invocations(active, book_id);
