CREATE TABLE podcast_shows (
    server_id TEXT NOT NULL,
    library_id INTEGER NOT NULL,
    show_id TEXT NOT NULL,
    show_path TEXT NOT NULL,
    feed_url_sha256 TEXT NOT NULL,
    private INTEGER NOT NULL DEFAULT 0 CHECK (private IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (server_id, show_id)
);
CREATE INDEX podcast_shows_feed ON podcast_shows(server_id, library_id, feed_url_sha256);
