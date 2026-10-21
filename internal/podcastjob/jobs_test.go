package podcastjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

type markerReader struct{ mount string }

func (m markerReader) Book(context.Context, int64, string) (*link.Book, error) {
	return nil, link.ErrNoBook
}
func (m markerReader) Marker(context.Context, int64) (string, error) {
	b, err := os.ReadFile(filepath.Join(m.mount, mapping.MarkerPath))
	var data struct{ Nonce string }
	if err == nil {
		err = json.Unmarshal(b, &data)
	}
	return data.Nonce, err
}

type scanServer struct {
	mount string
	calls atomic.Int32
	fail  atomic.Bool
}

func (s *scanServer) RescanPodcast(_ context.Context, _ int64, show string) (*link.PodcastRescan, error) {
	s.calls.Add(1)
	if s.fail.Load() {
		return nil, errors.New("server unavailable")
	}
	root, err := os.OpenRoot(s.mount)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := podcast.Read(root, show)
	if err != nil {
		return nil, err
	}
	return &link.PodcastRescan{Path: show, Episodes: len(f.Episodes)}, nil
}

type harness struct {
	t           *testing.T
	handler     *Subscribe
	db          *store.DB
	vault       *secrets.Vault
	libs        *mapping.Registry
	client      *netguard.Client
	http        *httptest.Server
	scan        *scanServer
	mount, data string
	newEpisode  atomic.Bool
	emptyFeed   atomic.Bool
	slowCover   atomic.Bool
	onCover     atomic.Pointer[func()]
	covers      atomic.Int32
	claimID     atomic.Int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, mount: t.TempDir(), data: t.TempDir()}
	ctx := context.Background()
	var err error
	h.db, err = store.Open(ctx, filepath.Join(h.data, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.db.Close() })
	h.vault, _, err = secrets.Open(h.data, strings.Repeat("01", 32))
	if err != nil {
		t.Fatal(err)
	}
	h.libs = mapping.NewRegistry()
	h.libs.SetLibraries([]link.Library{{ID: 3, Kind: "podcasts", Root: h.mount}})
	h.libs.Check(ctx, markerReader{h.mount}, func(int64, string) (string, bool) { return h.mount, true }, time.Now)
	h.scan = &scanServer{mount: h.mount}
	h.client = netguard.New(netguard.Options{AllowNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, Version: "0.9.0"})
	t.Cleanup(h.client.Close)
	h.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.emptyFeed.Load() {
			fmt.Fprint(w, `<rss version="2.0"><channel><title>Example Show</title></channel></rss>`)
			return
		}
		if r.URL.Path == "/cover.png" {
			h.covers.Add(1)
			if hook := h.onCover.Load(); hook != nil {
				(*hook)()
			}
			if h.slowCover.Load() {
				<-r.Context().Done()
				return
			}
			png.Encode(w, image.NewRGBA(image.Rect(0, 0, 1, 1)))
			return
		}
		if r.URL.Path == "/flip-a" {
			http.Redirect(w, r, "/flip-b", http.StatusMovedPermanently)
			return
		}
		if r.URL.Path == "/permanent" {
			http.Redirect(w, r, "/feed", http.StatusMovedPermanently)
			return
		}
		if r.URL.Path == "/temporary" {
			http.Redirect(w, r, "/feed", http.StatusFound)
			return
		}
		if r.URL.Path == "/blocked" {
			http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
			return
		}
		if r.URL.Path == "/error" {
			http.Error(w, "SECRET123", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("If-None-Match") == `"abc"` && !h.newEpisode.Load() {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", "Mon, 19 Oct 2026 21:00:00 GMT")
		move := ""
		if r.URL.Path == "/new-feed" {
			move = "<itunes:new-feed-url>" + h.http.URL + "/feed</itunes:new-feed-url>"
		}
		if r.URL.Path == "/flip-b" {
			// A stale new-feed-url naming the URL that redirects here.
			move = "<itunes:new-feed-url>" + h.http.URL + "/flip-a</itunes:new-feed-url>"
		}
		extra := ""
		if h.newEpisode.Load() {
			extra = `<item><guid>new</guid><title>New episode</title><enclosure url="/new.mp3" type="audio/mpeg"/></item>`
		}
		fmt.Fprintf(w, `<rss version="2.0" xmlns:itunes="http://www.itunes.com/DTDs/PodCast-1.0.dtd" xmlns:podcast="https://podcastindex.org/namespace/1.0"><channel><title>Example / Show</title><itunes:author>Author</itunes:author><itunes:image href="/cover.png"/>%s<item><guid>one</guid><title>First</title><pubDate>Mon, 19 Oct 2026 21:00:00 +0000</pubDate><enclosure url="/one.mp3?auth=SECRET123" type="audio/mpeg"/><podcast:chapters url="/chapters.json?auth=SECRET123"/><podcast:transcript url="/transcript.vtt?auth=SECRET123" type="text/vtt"/></item><item><guid>one</guid><title>Duplicate</title><enclosure url="/duplicate.mp3" type="audio/mpeg"/></item><item><title>Second</title><enclosure url="/two.m4a?auth=SECRET123" type="audio/mp4"/></item><item><guid>video</guid><enclosure url="/video.mp4" type="video/mp4"/></item>%s</channel></rss>`, move, extra)
	}))
	t.Cleanup(h.http.Close)
	h.handler = New(Options{DB: h.db, Vault: h.vault, Libraries: h.libs, Mount: func(int64, string) (string, bool) { return h.mount, true }, Server: func() Server { return h.scan }, Fetcher: h.client, Version: "0.9.0", ServerID: func() string { return "server-1" }, Now: func() time.Time { return time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC) }, CheckPoll: 5 * time.Millisecond})
	return h
}
func (h *harness) claim(url string, private bool) jobs.Job {
	h.t.Helper()
	raw, _ := json.Marshal(subscribeParams{FeedURL: url, FeedURLSHA256: podcast.FeedURLHash(url), Private: private})
	id := h.claimID.Add(1)
	lib := int64(3)
	job := jobs.Job{ID: id, Kind: SubscribeKind, LibraryID: &lib, Params: raw, Attempt: 1}
	sealed, err := h.handler.SealParams(job)
	if err != nil {
		h.t.Fatal(err)
	}
	job.Params = sealed
	if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: id, Kind: job.Kind, LibraryID: &lib, Params: sealed, Attempt: 1, ServerID: "server-1"}); err != nil {
		h.t.Fatal(err)
	}
	return job
}
func (h *harness) finish(job jobs.Job, result any, err error) {
	h.t.Helper()
	raw, e := json.Marshal(result)
	if e != nil {
		h.t.Fatal(e)
	}
	state, text := store.JobDone, ""
	if err != nil {
		state, text = store.JobFailed, err.Error()
	}
	if e := h.db.FinishServerJob(context.Background(), job.ID, job.Attempt, state, raw, text); e != nil {
		h.t.Fatal(e)
	}
}
func (h *harness) read(show string) *podcast.Feed {
	h.t.Helper()
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		h.t.Fatal(err)
	}
	defer root.Close()
	f, err := podcast.Read(root, show)
	if err != nil {
		h.t.Fatal(err)
	}
	return f
}
func fields(t *testing.T, result any) map[string]any {
	t.Helper()
	b, e := json.Marshal(result)
	if e != nil {
		t.Fatal(e)
	}
	var data map[string]any
	if json.Unmarshal(b, &data) != nil {
		t.Fatal("invalid result")
	}
	return data
}

func TestSubscribePollMergeArtworkAnd304(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	job := h.claim(h.http.URL+"/feed", false)
	res, err := h.handler.Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	h.finish(job, res, err)
	r := fields(t, res)
	if r["show_path"] != "Example Show" || r["episodes"] != float64(2) || r["skipped"] != float64(1) || r["next_poll_seconds"] != float64(1800) || r["created"] != true {
		t.Fatalf("subscribe: %+v", r)
	}
	f := h.read("Example Show")
	if f.Show.Image == nil || f.Show.Image.File != "cover.png" || f.Episodes[0].Title != "First" {
		t.Fatalf("feed: %+v", f)
	}
	show := "Example Show"
	poll := jobs.Job{ID: 10, Kind: PollKind, LibraryID: job.LibraryID, Path: &show, Params: json.RawMessage(`{"etag":"\"abc\""}`)}
	before, _ := os.Stat(filepath.Join(h.mount, show, "feed.json"))
	scans := h.scan.calls.Load()
	res, err = h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	r = fields(t, res)
	after, _ := os.Stat(filepath.Join(h.mount, show, "feed.json"))
	if r["status"] != float64(304) || r["changed"] != false || !before.ModTime().Equal(after.ModTime()) || h.scan.calls.Load() != scans {
		t.Fatal("304 wrote or rescanned", r)
	}
	h.newEpisode.Store(true)
	res, err = h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	r = fields(t, res)
	if r["new_episodes"] != float64(1) || r["changed"] != true {
		t.Fatal(r)
	}
	afterFeed := h.read(show)
	for _, old := range f.Episodes {
		found := false
		for _, e := range afterFeed.Episodes {
			if e.ID == old.ID {
				found = true
				if e.Path != old.Path {
					t.Fatal("path changed")
				}
			}
		}
		if !found {
			t.Fatal("kept episode disappeared")
		}
	}
	// Subscribing twice deduplicates by the stored feed hash.
	res, err = h.handler.Run(context.Background(), h.claim(h.http.URL+"/feed", false), jobs.LocalReporter())
	if err != nil || fields(t, res)["created"] != false {
		t.Fatal(res, err)
	}
}
func TestPrivateSealingNoLeakResumeAndMissingSecret(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	privateURL := h.http.URL + "/private/feed?auth=SECRET123"
	job := h.claim(privateURL, false)
	if strings.Contains(string(job.Params), "SECRET123") {
		t.Fatal("params were not sealed")
	}
	if got, _ := h.vault.Get(secrets.PodcastJobName(job.ID, job.Attempt)); got != privateURL {
		t.Fatal("missing sealed job URL")
	}
	// A new handler resumes using only the persisted params and the vault.
	resumed := New(h.handler.s.o)
	job.Resumed = true
	res, err := resumed.Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	h.finish(job, res, err)
	f := h.read("Example Show")
	if !f.Private || f.FeedURL != "" || f.Show.Link != "" || f.Episodes[0].Enclosure.URL != "" || f.Episodes[0].ChaptersURL != "" || len(f.Episodes[0].Transcripts) != 0 {
		t.Fatal("private fields retained")
	}
	if got, _ := h.vault.Get(secrets.PodcastFeedName(f.ShowID)); got != privateURL {
		t.Fatal("feed URL was not resealed")
	}
	if present, _ := h.vault.Present(secrets.PodcastJobName(job.ID, job.Attempt)); present {
		t.Fatal("ended job secret remains")
	}
	rows, _ := h.db.ListServerJobs(context.Background(), 0, 200)
	raw, _ := json.Marshal(rows)
	if bytes.Contains(raw, []byte("SECRET123")) {
		t.Fatal("URL leaked into server_jobs")
	}
	for _, dir := range []string{h.mount, h.data} {
		if err := filepath.WalkDir(dir, func(name string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				b, err := os.ReadFile(name)
				if err != nil {
					return err
				}
				if bytes.Contains(b, []byte("SECRET123")) {
					t.Errorf("plaintext private URL in %s", name)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	show := "Example Show"
	poll := jobs.Job{Kind: PollKind, LibraryID: job.LibraryID, Path: &show, Params: json.RawMessage(`{}`)}
	res, err = resumed.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil || fields(t, res)["new_episodes"] != float64(0) {
		t.Fatal("private merge duplicated identities", res, err)
	}
	h.vault.Delete(secrets.PodcastFeedName(f.ShowID))
	res, err = resumed.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err == nil || fields(t, res)["code"] != "feed_secret_missing" {
		t.Fatal(res, err)
	}
	// Re-following restores the missing feed secret into the same folder.
	res, err = resumed.Run(context.Background(), h.claim(privateURL, true), jobs.LocalReporter())
	if err != nil || fields(t, res)["created"] != false {
		t.Fatal("secret restore", res, err)
	}
}
func TestMovesRefusalsAndCacheRebuild(t *testing.T) {
	t.Parallel()
	for endpoint, moved := range map[string]bool{"/permanent": true, "/temporary": false, "/new-feed": true} {
		t.Run(endpoint, func(t *testing.T) {
			h := newHarness(t)
			source := h.http.URL + endpoint
			res, err := h.handler.Run(context.Background(), h.claim(source, false), jobs.LocalReporter())
			if err != nil {
				t.Fatal(err)
			}
			f := h.read(fields(t, res)["show_path"].(string))
			want := source
			if moved {
				want = h.http.URL + "/feed"
			}
			if f.FeedURL != want || len(f.PreviousFeedURLSHA256) != map[bool]int{true: 1, false: 0}[moved] {
				t.Fatal("move", f.FeedURL, f.PreviousFeedURLSHA256)
			}
			// Empty cache is rebuilt from disk before deduplication.
			db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "new.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			o := h.handler.s.o
			o.DB = db
			again := New(o)
			res, err = again.Run(context.Background(), h.claim(want, false), jobs.LocalReporter())
			shows, _ := db.PodcastShows(context.Background(), "server-1", 3)
			if err != nil || fields(t, res)["created"] != false || len(shows) != 1 {
				t.Fatal("rebuild", res, err, shows)
			}
		})
	}
	h := newHarness(t)
	for _, endpoint := range []string{"/blocked", "/error"} {
		job := h.claim(h.http.URL+endpoint+"?auth=SECRET123", true)
		res, err := h.handler.Run(context.Background(), job, jobs.LocalReporter())
		if err == nil {
			t.Fatal("expected failure")
		}
		r := fields(t, res)
		want := "blocked_address"
		if endpoint == "/error" {
			want = "http_401"
		}
		if r["code"] != want || strings.Contains(err.Error(), "SECRET123") {
			t.Fatal(r, err)
		}
		if present, _ := h.vault.Present(secrets.PodcastJobName(job.ID, job.Attempt)); present {
			t.Fatal("failed job secret remains")
		}
	}
	job := h.claim(h.http.URL+"/feed", false)
	h.libs.SetLibraries([]link.Library{{ID: 3, Kind: "books", Root: h.mount}})
	res, err := h.handler.Run(context.Background(), job, jobs.LocalReporter())
	if err == nil || fields(t, res)["code"] != "not_podcast_library" {
		t.Fatal(res, err)
	}
	// A checked library that isn't mapped refuses at once.
	h.libs.SetLibraries([]link.Library{{ID: 3, Kind: "podcasts", Root: h.mount}})
	h.libs.Check(context.Background(), markerReader{t.TempDir()}, func(int64, string) (string, bool) { return h.mount, true }, time.Now)
	res, err = h.handler.Run(context.Background(), job, jobs.LocalReporter())
	if err == nil || fields(t, res)["code"] != "library_not_writable" {
		t.Fatal(res, err)
	}
}

// A job claimed before its library's mapping check (the first claims after a
// start or a re-pair run beside the first check) waits for the check, then runs.
func TestJobWaitsForTheMappingCheck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.libs.SetLibraries(nil)
	h.libs.SetLibraries([]link.Library{{ID: 3, Kind: "podcasts", Root: h.mount}})
	rep := jobs.LocalReporter()
	done := make(chan error, 1)
	go func() {
		_, err := h.handler.Run(context.Background(), h.claim(h.http.URL+"/feed", false), rep)
		done <- err
	}()
	for rep.Last().Step != "waiting" {
		select {
		case err := <-done:
			t.Fatal("ran before the mapping check", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	h.libs.Check(context.Background(), markerReader{h.mount}, func(int64, string) (string, bool) { return h.mount, true }, time.Now)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	h.read("Example Show")
}

// The rescan a failed job left owed is asked for by the next poll, even when
// the feed hasn't changed since (a 304), and only once.
func TestPollRescansWhatAFailedJobLeftOwed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	job := h.claim(h.http.URL+"/feed", false)
	if _, err := h.handler.Run(context.Background(), job, jobs.LocalReporter()); err != nil {
		t.Fatal(err)
	}
	show := "Example Show"
	poll := jobs.Job{ID: 10, Kind: PollKind, LibraryID: job.LibraryID, Path: &show, Params: json.RawMessage(`{"etag":"\"abc\""}`)}
	h.newEpisode.Store(true)
	h.scan.fail.Store(true)
	res, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err == nil || fields(t, res)["code"] != "write_failed" || len(h.read(show).Episodes) != 3 {
		t.Fatal("a failed rescan", res, err)
	}
	h.scan.fail.Store(false)
	h.newEpisode.Store(false)
	scans := h.scan.calls.Load()
	res, err = h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil || fields(t, res)["status"] != float64(304) || h.scan.calls.Load() != scans+1 {
		t.Fatal("the owed rescan was not asked for", res, err)
	}
	if _, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter()); err != nil || h.scan.calls.Load() != scans+1 {
		t.Fatal("an indexed show was rescanned again", err)
	}
}

// Each attempt of a job seals the URL under its own name: the end of an
// earlier attempt (its lease ran out and the server handed the job out again)
// never deletes the later one's.
func TestEachAttemptSealsItsOwnSecret(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	privateURL := h.http.URL + "/feed?auth=SECRET123"
	first := h.claim(privateURL, true)
	second := first
	second.Attempt = 2
	second.Params, _ = json.Marshal(subscribeParams{FeedURL: privateURL, FeedURLSHA256: podcast.FeedURLHash(privateURL), Private: true})
	sealed, err := h.handler.SealParams(second)
	if err != nil {
		t.Fatal(err)
	}
	second.Params = sealed
	h.handler.Abandon(context.Background(), first)
	if res, err := h.handler.Run(context.Background(), second, jobs.LocalReporter()); err != nil {
		t.Fatal("the later attempt lost its sealed URL", res, err)
	}
}

// A follow that fails before its feed.json is written takes its folder back,
// so following again makes the same folder, not "Example Show (2)".
func TestFailedFollowTakesBackItsFolder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	books := func() { h.libs.SetLibraries([]link.Library{{ID: 3, Kind: "books", Root: h.mount}}) }
	h.onCover.Store(&books)
	res, err := h.handler.Run(context.Background(), h.claim(h.http.URL+"/feed", false), jobs.LocalReporter())
	if err == nil {
		t.Fatal("expected a failure", res)
	}
	if _, err := os.Lstat(filepath.Join(h.mount, "Example Show")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the failed follow left its folder", err)
	}
	h.onCover.Store(nil)
	h.libs.SetLibraries([]link.Library{{ID: 3, Kind: "podcasts", Root: h.mount}})
	res, err = h.handler.Run(context.Background(), h.claim(h.http.URL+"/feed", false), jobs.LocalReporter())
	if err != nil || fields(t, res)["show_path"] != "Example Show" {
		t.Fatal(res, err)
	}
}

// Artwork is best effort: a slow image host gets at most half of the time a
// job has left, so feed.json, the cache and the rescan still get theirs.
func TestSlowArtworkLeavesTimeForTheFeed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.slowCover.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := h.handler.Run(ctx, h.claim(h.http.URL+"/feed", false), jobs.LocalReporter())
	if err != nil || h.covers.Load() == 0 {
		t.Fatal("slow artwork failed the follow", res, err)
	}
	if f := h.read("Example Show"); f.Show.Image != nil {
		t.Fatal("recorded artwork that never came", f.Show.Image)
	}
}

// A cover deleted by hand is fetched again with the next change, not only when
// the feed's image changes.
func TestDeletedCoverIsFetchedAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	job := h.claim(h.http.URL+"/feed", false)
	if _, err := h.handler.Run(context.Background(), job, jobs.LocalReporter()); err != nil {
		t.Fatal(err)
	}
	cover := filepath.Join(h.mount, "Example Show", "cover.png")
	if err := os.Remove(cover); err != nil {
		t.Fatal(err)
	}
	show := "Example Show"
	h.newEpisode.Store(true)
	poll := jobs.Job{ID: 10, Kind: PollKind, LibraryID: job.LibraryID, Path: &show, Params: json.RawMessage(`{}`)}
	if _, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cover); err != nil {
		t.Fatal("the deleted cover did not come back", err)
	}
}

// A stale itunes:new-feed-url naming the URL that redirects to the feed never
// moves the show back: it would flip on every poll.
func TestNewFeedURLNeverFlipsBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	job := h.claim(h.http.URL+"/flip-a", false)
	res, err := h.handler.Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	show := fields(t, res)["show_path"].(string)
	if f := h.read(show); f.FeedURL != h.http.URL+"/flip-b" {
		t.Fatal("permanent redirect not followed", f.FeedURL)
	}
	poll := jobs.Job{ID: 10, Kind: PollKind, LibraryID: job.LibraryID, Path: &show, Params: json.RawMessage(`{}`)}
	for range 2 {
		res, err = h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
		if err != nil || fields(t, res)["moved"] != false {
			t.Fatal("flipped back", res, err)
		}
	}
	if f := h.read(show); f.FeedURL != h.http.URL+"/flip-b" || len(f.PreviousFeedURLSHA256) != 1 {
		t.Fatal("moved", f.FeedURL, f.PreviousFeedURLSHA256)
	}
}
func TestMoveURLKeepsHTTPSAndEachPreviousOnce(t *testing.T) {
	t.Parallel()
	hash := podcast.FeedURLHash
	a, b, c := "https://a.example/feed", "https://b.example/feed", "https://c.example/feed"
	f := &podcast.Feed{FeedURLSHA256: hash(a), PreviousFeedURLSHA256: []string{hash(b), hash(c)}}
	got, moved := moveURL(f, a, &netguard.Response{Status: 200, URL: a}, &podcast.Parsed{NewFeedURL: "http://d.example/feed"})
	if moved || got != a {
		t.Fatal("an https feed moved to http", got)
	}
	got, moved = moveURL(f, a, &netguard.Response{Status: 200, URL: c, Permanent: true}, &podcast.Parsed{})
	if !moved || got != c || !slices.Equal(f.PreviousFeedURLSHA256, []string{hash(a), hash(b)}) {
		t.Fatal("previous hashes", got, f.PreviousFeedURLSHA256)
	}
}
func TestSubscribeLockAndTerminalSecretCleanup(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a, b := h.claim(h.http.URL+"/feed", false), h.claim(h.http.URL+"/feed", false)
	var wg sync.WaitGroup
	for _, job := range []jobs.Job{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.handler.Run(context.Background(), job, jobs.LocalReporter()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	rows, _ := h.db.PodcastShows(context.Background(), "server-1", 3)
	if len(rows) != 1 {
		t.Fatalf("concurrent subscribe created %d shows", len(rows))
	}
	job := h.claim(h.http.URL+"/feed?auth=SECRET123", true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.handler.Run(ctx, job, jobs.LocalReporter())
	if present, _ := h.vault.Present(secrets.PodcastJobName(job.ID, job.Attempt)); !present {
		t.Fatal("restart lost resumable secret")
	}
	h.handler.Abandon(context.Background(), job)
	if present, _ := h.vault.Present(secrets.PodcastJobName(job.ID, job.Attempt)); present {
		t.Fatal("abandoned secret remains")
	}
	job = h.claim(h.http.URL+"/feed?auth=SECRET123", true)
	h.vault.Delete(secrets.PodcastJobName(job.ID, job.Attempt))
	res, err := h.handler.Run(context.Background(), job, jobs.LocalReporter())
	var failure *netguard.Error
	if !errors.As(err, &failure) || fields(t, res)["code"] != "feed_secret_missing" {
		t.Fatal(res, err)
	}
}

func TestEmptyPollRetainsRemovedEpisodesAndOccupiedPaths(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	job := h.claim(h.http.URL+"/feed", false)
	_, err := h.handler.Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	show := "Example Show"
	poll := jobs.Job{Kind: PollKind, LibraryID: job.LibraryID, Path: &show, Params: []byte(`{}`)}
	h.emptyFeed.Store(true)
	result, err := h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	if fields(t, result)["episodes"] != float64(2) {
		t.Fatal(result)
	}
	for _, e := range h.read(show).Episodes {
		if e.RemovedAt == "" {
			t.Fatal("missing item was not marked removed")
		}
	}
	h.emptyFeed.Store(false)
	h.newEpisode.Store(true)
	newEpisode := podcast.Episode{GUID: "new", Title: "New episode", Enclosure: podcast.Enclosure{URL: h.http.URL + "/new.mp3", Type: "audio/mpeg"}}
	occupied := podcast.EpisodePath(show, newEpisode, "mp3", nil)
	if err := os.WriteFile(filepath.Join(h.mount, filepath.FromSlash(occupied)), []byte("external audio"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = h.handler.PollJob().Run(context.Background(), poll, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range h.read(show).Episodes {
		if e.RemovedAt != "" {
			t.Fatal("returning episode stayed removed")
		}
		if e.GUID == "new" && e.Path == occupied {
			t.Fatal("assigned path collides with an existing file")
		}
	}
}
