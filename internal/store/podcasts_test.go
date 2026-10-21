package store

import (
	"context"
	"testing"
)

func TestPodcastCacheServerAndLibraryIsolation(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for _, s := range []PodcastShow{{ServerID: "a", LibraryID: 3, ShowID: "one", ShowPath: "Show", FeedURLSHA256: "hash", Private: true}, {ServerID: "b", LibraryID: 3, ShowID: "one", ShowPath: "Other", FeedURLSHA256: "other"}, {ServerID: "a", LibraryID: 4, ShowID: "two", ShowPath: "Elsewhere", FeedURLSHA256: "hash"}} {
		if err := db.UpsertPodcastShow(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	shows, err := db.PodcastShows(ctx, "a", 3)
	if err != nil || len(shows) != 1 || shows[0].ShowPath != "Show" || !shows[0].Private || shows[0].CreatedAt == "" {
		t.Fatalf("cache: %+v %v", shows, err)
	}
	s := shows[0]
	s.ShowPath = "Renamed"
	s.FeedURLSHA256 = "new"
	if err := db.UpsertPodcastShow(ctx, s); err != nil {
		t.Fatal(err)
	}
	after, _ := db.PodcastShows(ctx, "a", 3)
	if len(after) != 1 || after[0].ShowPath != "Renamed" || after[0].CreatedAt != s.CreatedAt {
		t.Fatal(after)
	}
}
