package store

import "context"

// PodcastShow is a rebuildable lookup cache. Credentials never enter this row.
type PodcastShow struct {
	ServerID      string
	LibraryID     int64
	ShowID        string
	ShowPath      string
	FeedURLSHA256 string
	Private       bool
	CreatedAt     string
	UpdatedAt     string
}

func (db *DB) UpsertPodcastShow(ctx context.Context, s PodcastShow) error {
	now := timestamp(nowFn())
	if s.CreatedAt == "" {
		s.CreatedAt = now
	}
	_, err := db.sql.ExecContext(ctx, `INSERT INTO podcast_shows (server_id,library_id,show_id,show_path,feed_url_sha256,private,created_at,updated_at)
 VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(server_id,show_id) DO UPDATE SET library_id=excluded.library_id,show_path=excluded.show_path,feed_url_sha256=excluded.feed_url_sha256,private=excluded.private,updated_at=excluded.updated_at`, s.ServerID, s.LibraryID, s.ShowID, s.ShowPath, s.FeedURLSHA256, s.Private, s.CreatedAt, now)
	return err
}
func (db *DB) PodcastShows(ctx context.Context, serverID string, libraryID int64) ([]PodcastShow, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT server_id,library_id,show_id,show_path,feed_url_sha256,private,created_at,updated_at FROM podcast_shows WHERE server_id=? AND library_id=? ORDER BY show_path`, serverID, libraryID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []PodcastShow{}
	for rows.Next() {
		var s PodcastShow
		if err := rows.Scan(&s.ServerID, &s.LibraryID, &s.ShowID, &s.ShowPath, &s.FeedURLSHA256, &s.Private, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
