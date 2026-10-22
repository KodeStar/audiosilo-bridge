package store

import (
	"context"
	"path/filepath"
	"strings"
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

func TestArtworkStateIsolationAndReopen(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), FileName)
	db, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	want := PodcastArtworkState{SourceSHA256: strings.Repeat("a", 64), Failures: 3, NextAttemptAt: "2026-10-22T08:00:00Z"}
	if err := db.SavePodcastArtwork(ctx, "server", 3, "show", want); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.PodcastArtwork(ctx, "server", 3, "show")
	if err != nil || got != want {
		t.Fatal(got, err)
	}
	for _, key := range []struct {
		server string
		lib    int64
		show   string
	}{{"other", 3, "show"}, {"server", 4, "show"}, {"server", 3, "replacement"}} {
		got, err := db.PodcastArtwork(ctx, key.server, key.lib, key.show)
		if err != nil || got.Failures != 0 {
			t.Fatal(got, err)
		}
	}
	if err := db.SavePodcastArtwork(ctx, "server", 3, "show", PodcastArtworkState{SourceSHA256: want.SourceSHA256}); err != nil {
		t.Fatal(err)
	}
	got, err = db.PodcastArtwork(ctx, "server", 3, "show")
	if err != nil || got.Failures != 0 || got.NextAttemptAt != "" {
		t.Fatal(got, err)
	}
}
