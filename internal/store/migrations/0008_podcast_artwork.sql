CREATE TABLE podcast_artwork_state (
 server_id TEXT NOT NULL,
 library_id INTEGER NOT NULL,
 show_id TEXT NOT NULL,
 source_sha256 TEXT NOT NULL,
 failures INTEGER NOT NULL CHECK(failures >= 0),
 next_attempt_at TEXT NOT NULL,
 PRIMARY KEY(server_id,library_id,show_id)
);
