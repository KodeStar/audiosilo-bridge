// Package podcastjob runs the light podcast.subscribe and podcast.poll jobs.
package podcastjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

const SubscribeKind = "podcast.subscribe"
const PollKind = "podcast.poll"

type Libraries interface {
	Library(int64) (link.Library, bool)
	Status(int64) (link.LibraryStatus, bool)
	Statuses() []link.LibraryStatus
}
type Server interface {
	RescanPodcast(context.Context, int64, string) (*link.PodcastRescan, error)
}
type Fetcher interface {
	Fetch(context.Context, netguard.Request) (*netguard.Response, error)
}
type Options struct {
	DB              *store.DB
	Libraries       Libraries
	Mount           mapping.MountFunc
	Server          func() Server
	Vault           secrets.Store
	Fetcher         Fetcher
	MaxFeedBytes    int64
	MaxEpisodeBytes int64
	DataDir         string
	Probe           ProbeFunc
	Streamer        Streamer
	Wait            func(context.Context, time.Duration) error
	Fault           podcast.Boundary // the journal's boundaries, and "partial" when a resume checks its partial before the ranged GET
	BudgetNow       func() time.Time
	AfterFunc       func(time.Duration, func()) Timer
	Version         string
	ServerID        func() string
	Now             func() time.Time
	// CheckPoll is how often a job claimed before its library's mapping check
	// looks for the result (default 1 s).
	CheckPoll time.Duration
}
type service struct {
	o     Options
	mu    sync.Mutex
	locks map[string]*lock
	// indexed holds the shows whose current feed.json the server indexed since
	// this start: a poll asks for a rescan of any other, so an index update a
	// failed job left owed is not lost when the feed stays the same.
	indexed map[string]bool
	// swept is when SweepPartials last started a whole-library sweep.
	swept time.Time
	// recMu guards migrated, the pairings whose records an older Bridge
	// named by path alone were renamed by show this start (migrateRecords).
	recMu    sync.Mutex
	migrated map[string]bool
	// unfound holds, until when, the show folders that could not be read and
	// whose show no other folder held (lockShow): the next jobs there fail
	// without searching the whole library again.
	unfound map[string]time.Time
}
type lock struct {
	gate  chan struct{}
	users int
}
type Subscribe struct{ s *service }
type Poll struct{ s *service }

func New(o Options) *Subscribe {
	if o.MaxFeedBytes <= 0 {
		o.MaxFeedBytes = 32 << 20
	}
	if o.BudgetNow == nil {
		o.BudgetNow = time.Now
	}
	if o.AfterFunc == nil {
		o.AfterFunc = func(d time.Duration, fn func()) Timer { return time.AfterFunc(d, fn) }
	}
	if o.MaxEpisodeBytes <= 0 {
		o.MaxEpisodeBytes = 1024 << 20
	}
	if o.Wait == nil {
		o.Wait = netguard.Wait
	}
	if o.Probe == nil {
		o.Probe = LocalProbe("ffprobe")
	}
	if o.Streamer == nil {
		o.Streamer, _ = o.Fetcher.(Streamer)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ServerID == nil {
		o.ServerID = func() string { return "" }
	}
	if o.CheckPoll <= 0 {
		o.CheckPoll = time.Second
	}
	return &Subscribe{s: &service{o: o, locks: map[string]*lock{}, indexed: map[string]bool{}, migrated: map[string]bool{}, unfound: map[string]time.Time{}}}
}
func (h *Subscribe) PollJob() *Poll { return &Poll{s: h.s} }
func (h *Subscribe) Kind() string   { return SubscribeKind }
func (h *Subscribe) Lane() string   { return jobs.LaneLight }
func (h *Poll) Kind() string        { return PollKind }
func (h *Poll) Lane() string        { return jobs.LaneLight }

type subscribeParams struct {
	FeedURL       string `json:"feed_url"`
	FeedURLSHA256 string `json:"feed_url_sha256"`
	FeedHost      string `json:"feed_host"`
	Private       bool   `json:"private"`
	ShowPath      string `json:"show_path"`
	ShowID        string `json:"show_id,omitempty"`
}

// SealParams is called before RecordClaim. Run receives only the sealed form.
func (h *Subscribe) SealParams(job jobs.Job) (json.RawMessage, error) {
	var p subscribeParams
	if json.Unmarshal(job.Params, &p) != nil {
		return nil, &jobs.SealFailure{Code: "invalid_feed_url", Message: "The feed request is invalid."}
	}
	norm, err := podcast.NormalizeFeedURL(p.FeedURL)
	if err != nil {
		return nil, &jobs.SealFailure{Code: "invalid_feed_url", Message: "The feed URL is invalid."}
	}
	p.FeedURL = norm
	p.Private = p.Private || podcast.IsPrivateFeedURL(norm)
	p.FeedURLSHA256 = podcast.FeedURLHash(norm)
	p.FeedHost = podcast.FeedHost(norm)
	if p.Private {
		if err := h.s.o.Vault.Set(secrets.PodcastJobName(job.ID, job.Attempt), norm); err != nil {
			return nil, &jobs.SealFailure{Code: "write_failed", Message: "The private feed credentials could not be sealed."}
		}
		p.FeedURL = ""
	}
	return json.Marshal(p)
}
func (h *Subscribe) Abandon(_ context.Context, job jobs.Job) {
	_ = h.s.o.Vault.Delete(secrets.PodcastJobName(job.ID, job.Attempt))
}

func (h *Subscribe) Run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	defer func() {
		if parent.Err() == nil || !jobs.Resumes(parent) {
			h.Abandon(parent, job)
		}
	}()
	return outcome(h.run(parent, job, rep))
}
func (h *Poll) Run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	return outcome(h.run(parent, job, rep))
}

// ceiling bounds a subscribe's or a poll's work, which starts once it holds its
// lock: a wait never leaves the feed's own fetch short of time. The wait for
// the library's mapping check is bounded by one more ceiling, which a poll's
// wait for its show shares (a subscribe's for its library waits on that
// follow's own ceiling). A retain's covers its whole run.
const ceiling = 3 * time.Minute

func (s *service) acquire(ctx context.Context, key string) (func(), error) {
	s.mu.Lock()
	l := s.locks[key]
	if l == nil {
		l = &lock{gate: make(chan struct{}, 1)}
		s.locks[key] = l
	}
	l.users++
	s.mu.Unlock()
	drop := func() {
		s.mu.Lock()
		l.users--
		if l.users == 0 {
			delete(s.locks, key)
		}
		s.mu.Unlock()
	}
	select {
	case l.gate <- struct{}{}:
		return func() { <-l.gate; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// library is the job's podcast library and the mount it writes at. A job
// claimed before the library's mapping check has a result for that mount (the
// first claims after a start run beside the first check) waits for it, as
// generate's waitMapped does, within the job's ceiling.
func (s *service) library(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (link.Library, string, error) {
	if job.LibraryID == nil {
		return link.Library{}, "", fail("not_podcast_library", "This job needs a podcast library.")
	}
	for {
		lib, ok := s.o.Libraries.Library(*job.LibraryID)
		if !ok || lib.LibraryKind() != "podcasts" {
			return lib, "", fail("not_podcast_library", "This job needs a podcast library.")
		}
		mount, _ := s.o.Mount(lib.ID, lib.Root)
		if st, checked := s.o.Libraries.Status(lib.ID); checked && st.MountPath == mount {
			return lib, mount, s.writable(lib, mount)
		}
		rep.Progress(jobs.Progress{Step: "waiting", Message: "Waiting for the library's mapping check"})
		t := time.NewTimer(s.o.CheckPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return lib, "", ctx.Err()
		case <-t.C:
		}
	}
}
func (s *service) writable(lib link.Library, mount string) error {
	current, ok := s.o.Libraries.Library(lib.ID)
	if !ok || current.LibraryKind() != "podcasts" {
		return fail("not_podcast_library", "This job needs a podcast library.")
	}
	now, _ := s.o.Mount(lib.ID, current.Root)
	st, ok := s.o.Libraries.Status(lib.ID)
	if now != mount || !ok || !st.Mapped || !st.Writable || st.MountPath != mount {
		return fail("library_not_writable", "The podcast library must be checked, mapped and writable.")
	}
	return nil
}
func (s *service) beforeWrite(lib link.Library, mount, rel string) error {
	if err := s.writable(lib, mount); err != nil {
		return err
	}
	full, err := mapping.SafeJoin(mount, rel)
	if err != nil {
		return fail("write_failed", "The podcast path is invalid.")
	}
	if err := mapping.ProbeWritable(full); err != nil {
		return fail("library_not_writable", "The podcast folder is not writable.")
	}
	return nil
}

// feedURL is a job's normalized feed URL: plain, or the vault entry secret
// when the show is private.
func (s *service) feedURL(plain string, private bool, secret, invalid string) (string, error) {
	if private {
		v, err := s.o.Vault.Get(secret)
		if err != nil || v == "" {
			return "", fail("feed_secret_missing", "The private feed secret is missing. Follow the feed again.")
		}
		plain = v
	}
	norm, err := podcast.NormalizeFeedURL(plain)
	if err != nil {
		return "", fail("invalid_feed_url", invalid)
	}
	return norm, nil
}

func (h *Subscribe) run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	s := h.s
	wait, stop := context.WithTimeout(parent, ceiling)
	lib, mount, err := s.library(wait, job, rep)
	stop()
	if err != nil {
		return nil, err
	}
	// A follow queued behind another one into the same library waits its turn
	// (that one is bounded by its own ceiling), then gets a whole ceiling: the
	// wait never times it out unfetched.
	unlock, err := s.acquire(parent, s.o.ServerID()+"|"+lib.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(parent, ceiling)
	defer cancel()
	var p subscribeParams
	if json.Unmarshal(job.Params, &p) != nil {
		return nil, fail("invalid_feed_url", "The feed request is invalid.")
	}
	feedURL, err := s.feedURL(p.FeedURL, p.Private, secrets.PodcastJobName(job.ID, job.Attempt), "The feed URL is invalid.")
	if err != nil {
		return nil, err
	}
	if p.FeedURLSHA256 != "" && podcast.FeedURLHash(feedURL) != p.FeedURLSHA256 {
		return nil, fail("feed_secret_missing", "The private feed secret does not match this request.")
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return nil, fail("library_not_writable", "The podcast library could not be opened.")
	}
	defer func() { _ = root.Close() }()
	entries, err := readDir(root)
	if err != nil {
		return nil, fail("write_failed", "The podcast library could not be read.")
	}
	rows, err := s.cache(ctx, root, lib, entries)
	if err != nil {
		return nil, fail("write_failed", "The podcast cache could not be rebuilt.")
	}
	show, f, renamed, err := s.find(root, entries, rows, p, feedURL)
	if err != nil {
		return nil, err
	}
	created, done := f == nil, false
	if !created {
		current := f.FeedURL
		if f.Private {
			current, _ = s.o.Vault.Get(secrets.PodcastFeedName(f.ShowID))
		}
		if norm, e := podcast.NormalizeFeedURL(current); e == nil && podcast.FeedURLHash(norm) == f.FeedURLSHA256 {
			feedURL = norm
		}
	}
	rep.Progress(jobs.Progress{Step: "fetching", Message: "Fetching the podcast feed"})
	resp, err := s.o.Fetcher.Fetch(ctx, netguard.Request{URL: feedURL, MaxBytes: s.o.MaxFeedBytes})
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, fail("not_a_feed", "The feed returned no podcast data.")
	}
	rep.Progress(jobs.Progress{Step: "parsing", Message: "Reading the podcast feed"})
	parse := podcast.Parse
	if !created {
		parse = podcast.ParseExisting
	}
	parsed, err := parse(resp.Body, resp.URL)
	if err != nil {
		return nil, err
	}
	if created {
		// The names a new show cannot take: every entry's, and the old name of
		// every folder renamed by hand and not taken in yet, which the server
		// still indexes as that show until its rescan: a new show there would
		// stand in for it, and its jobs would find another show at that name.
		names := renamed
		for _, e := range entries {
			names = append(names, e.Name())
		}
		title := parsed.Show.Title
		if p.Private || podcast.IsPrivateFeedURL(feedURL) || (parsed.NewFeedURL != "" && podcast.IsPrivateFeedURL(parsed.NewFeedURL)) || (resp.Permanent && podcast.IsPrivateFeedURL(resp.URL)) {
			title = podcast.RedactText(title)
		}
		show = podcast.ShowName(title, names)
		if err := s.beforeWrite(lib, mount, ""); err != nil {
			return nil, err
		}
		if _, err := mapping.SafeJoin(mount, show); err != nil {
			return nil, fail("write_failed", "The show path is invalid.")
		}
		if err := root.Mkdir(show, 0o755); err != nil {
			return nil, fail("write_failed", "The show folder could not be created.")
		}
		// A follow that fails before its feed.json is written takes back the
		// folder it made (and its artwork), or each retry would add "Title (2)".
		defer func() {
			if done {
				return
			}
			if _, err := root.Lstat(show + "/feed.json"); errors.Is(err, os.ErrNotExist) {
				_ = root.Remove(show + "/cover.jpg")
				_ = root.Remove(show + "/cover.png")
				_ = root.Remove(show)
			}
		}()
		if err := root.Chmod(show, 0o755); err != nil {
			return nil, fail("write_failed", "The show folder permissions could not be set.")
		}
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, fail("write_failed", "The show identity could not be created.")
		}
		// moveURL below sets FeedURL and FeedHost.
		f = &podcast.Feed{Version: 1, Generator: "audiosilo-bridge/" + s.o.Version, ShowID: hex.EncodeToString(random[:]), FeedURLSHA256: podcast.FeedURLHash(feedURL), PreviousFeedURLSHA256: []string{}, Private: p.Private || podcast.IsPrivateFeedURL(feedURL), SubscribedAt: s.o.Now().UTC().Format(time.RFC3339), Episodes: []podcast.Episode{}}
	}
	unlockShow, err := s.acquire(ctx, s.showKey(mount, show))
	if err != nil {
		return nil, err
	}
	defer unlockShow()
	// A poll may have updated it while subscribe was fetching.
	if !created {
		if f, err = s.readShow(ctx, root, lib, mount, show); err != nil {
			return nil, err
		}
	}
	if p.ShowID != "" && f.ShowID != p.ShowID {
		return nil, fail("identity_changed", "The podcast show identity changed.")
	}
	f.Private = f.Private || p.Private
	newURL, _ := moveURL(f, feedURL, resp, parsed)
	rep.Progress(jobs.Progress{Step: "writing", Message: "Saving the podcast show"})
	if _, _, err := s.persist(ctx, root, lib, mount, show, f, parsed, newURL); err != nil {
		return nil, err
	}
	done = true
	rep.Progress(jobs.Progress{Step: "indexing", Message: "Indexing the podcast show"})
	if err := s.rescan(ctx, lib.ID, show); err != nil {
		return nil, err
	}
	return map[string]any{"library_id": lib.ID, "show_path": show, "show_id": f.ShowID, "created": created, "private": f.Private, "episodes": len(f.Episodes), "skipped": parsed.Skipped, "status": resp.Status, "etag": resp.ETag, "last_modified": resp.LastModified, "next_poll_seconds": podcast.NextPoll(f, resp.CacheControl, s.o.Now())}, nil
}

func (h *Poll) run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	s := h.s
	wait, stop := context.WithTimeout(parent, ceiling)
	defer stop()
	lib, mount, err := s.library(wait, job, rep)
	if err != nil {
		return nil, err
	}
	// The faults before the fetch are the Bridge's or the library's, never
	// the feed's (write_failed): the server counts only the feed's toward a
	// failing feed.
	if job.Path == nil || !podcast.ValidShow(*job.Path) {
		return nil, fail("write_failed", "The podcast show path is invalid.")
	}
	var params struct {
		ShowID       string `json:"show_id"`
		ETag         string `json:"etag"`
		LastModified string `json:"last_modified"`
	}
	if len(job.Params) > 0 && json.Unmarshal(job.Params, &params) != nil || len(params.ETag) > 512 || len(params.LastModified) > 512 {
		return nil, fail("write_failed", "The feed check parameters are invalid.")
	}
	show := *job.Path
	if _, err := mapping.SafeJoin(mount, show); err != nil {
		return nil, fail("write_failed", "The show path is invalid.")
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return nil, fail("library_not_writable", "The podcast library could not be opened.")
	}
	defer func() { _ = root.Close() }()
	// A show folder renamed by hand is polled where it is now: the server still
	// names the old folder until the rescan below indexes the new one.
	show, f, unlock, err := s.lockShow(wait, root, lib, mount, show, params.ShowID, nil)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(parent, ceiling)
	defer cancel()
	feedURL, err := s.feedURL(f.FeedURL, f.Private, secrets.PodcastFeedName(f.ShowID), "The saved feed URL is invalid.")
	if err != nil {
		return nil, err
	}
	if params.ShowID != "" && params.ShowID != f.ShowID {
		return nil, fail("identity_changed", "The podcast show identity changed.")
	}
	rep.Progress(jobs.Progress{Step: "fetching", Message: "Checking the podcast feed"})
	resp, err := s.o.Fetcher.Fetch(ctx, netguard.Request{URL: feedURL, ETag: params.ETag, LastModified: params.LastModified, MaxBytes: s.o.MaxFeedBytes})
	if err != nil {
		return nil, err
	}
	changed, moved := false, false
	added, skipped := 0, 0
	if resp.Status != 304 {
		entries, err := readDir(root)
		if err != nil {
			return nil, err
		}
		if _, err := s.cache(ctx, root, lib, entries); err != nil {
			return nil, err
		}
		rep.Progress(jobs.Progress{Step: "parsing", Message: "Reading the podcast feed"})
		parsed, err := podcast.ParseExisting(resp.Body, resp.URL)
		if err != nil {
			return nil, err
		}
		skipped = parsed.Skipped
		feedURL, moved = moveURL(f, feedURL, resp, parsed)
		rep.Progress(jobs.Progress{Step: "writing", Message: "Saving changed podcast details"})
		added, changed, err = s.persist(ctx, root, lib, mount, show, f, parsed, feedURL)
		if err != nil {
			return nil, err
		}
	}
	if !s.isIndexed(lib.ID, show) {
		rep.Progress(jobs.Progress{Step: "indexing", Message: "Indexing the podcast show"})
		if err := s.rescan(ctx, lib.ID, show); err != nil {
			return nil, err
		}
	}
	return map[string]any{"library_id": lib.ID, "show_path": show, "show_id": f.ShowID, "status": resp.Status, "changed": changed, "new_episodes": added, "episodes": len(f.Episodes), "skipped": skipped, "moved": moved, "etag": resp.ETag, "last_modified": resp.LastModified, "next_poll_seconds": podcast.NextPoll(f, resp.CacheControl, s.o.Now())}, nil
}

// moveURL applies a permanent redirect or an itunes:new-feed-url to f and
// answers the URL to fetch from now on. A new-feed-url counts only when it
// names neither the URL fetched nor where that redirected, nor a URL the show
// already moved away from (one that redirects back would otherwise flip the
// show on every poll), and never takes an https feed to http.
func moveURL(f *podcast.Feed, original string, resp *netguard.Response, p *podcast.Parsed) (string, bool) {
	current := original
	if resp.Permanent {
		if norm, err := podcast.NormalizeFeedURL(resp.URL); err == nil {
			current = norm
		}
	}
	if norm, err := podcast.NormalizeFeedURL(p.NewFeedURL); err == nil && norm != original && norm != current &&
		!slices.Contains(f.PreviousFeedURLSHA256, podcast.FeedURLHash(norm)) && !downgrade(current, norm) {
		current = norm
	}
	hash := podcast.FeedURLHash(current)
	moved := hash != f.FeedURLSHA256
	if moved {
		// The hashes it moved away from, newest first, each once and never
		// the current one.
		previous := []string{f.FeedURLSHA256}
		for _, h := range f.PreviousFeedURLSHA256 {
			if h != hash && !slices.Contains(previous, h) {
				previous = append(previous, h)
			}
		}
		f.PreviousFeedURLSHA256 = previous[:min(len(previous), 5)]
		f.FeedURLSHA256 = hash
	}
	f.Private = f.Private || podcast.IsPrivateFeedURL(current)
	f.FeedURL = current
	f.FeedHost = podcast.FeedHost(current)
	f.ScrubPrivate()
	return current, moved
}

// downgrade reports whether a move from one feed URL to another drops TLS.
func downgrade(from, to string) bool {
	return strings.HasPrefix(from, "https://") && strings.HasPrefix(to, "http://")
}

func (s *service) save(ctx context.Context, lib link.Library, show string, f *podcast.Feed, feedURL string) error {
	if f.Private {
		if err := s.o.Vault.Set(secrets.PodcastFeedName(f.ShowID), feedURL); err != nil {
			return fail("write_failed", "The private feed credentials could not be saved.")
		}
	}
	return s.o.DB.UpsertPodcastShow(ctx, s.showRow(lib, show, f))
}

// showRow is the lookup cache row for a show's feed.json.
func (s *service) showRow(lib link.Library, show string, f *podcast.Feed) store.PodcastShow {
	return store.PodcastShow{ServerID: s.o.ServerID(), LibraryID: lib.ID, ShowID: f.ShowID, ShowPath: show, FeedURLSHA256: f.FeedURLSHA256, Private: f.Private, CreatedAt: f.SubscribedAt}
}

// persist merges a parsed feed into f, refreshes the artwork and saves
// feed.json, the vault entry and the cache row. It answers the number of new
// episodes and whether feed.json changed.
func (s *service) persist(ctx context.Context, root *os.Root, lib link.Library, mount, show string, f *podcast.Feed, parsed *podcast.Parsed, feedURL string) (int, bool, error) {
	added, err := s.merge(root, f, parsed, show)
	if err != nil {
		return 0, false, err
	}
	s.artwork(ctx, root, lib, mount, show, f, parsed.ImageURL) // checks writability before its own write
	if err := s.beforeWrite(lib, mount, show); err != nil {
		return 0, false, err
	}
	changed, err := podcast.Write(root, show, f, s.o.Now())
	if err != nil {
		return 0, false, err
	}
	if changed {
		s.setIndexed(lib.ID, show, false) // before anything else can fail
	}
	if err := s.save(ctx, lib, show, f, feedURL); err != nil {
		return 0, false, err
	}
	return added, changed, nil
}

// rescan asks the server to index show, waiting out its 429s (askRescan).
func (s *service) rescan(ctx context.Context, id int64, show string) error {
	srv := s.o.Server()
	if srv == nil {
		return fail("write_failed", "The server is unavailable for indexing. Check the feed again.")
	}
	if err := s.askRescan(ctx, srv, id, show); err != nil {
		return fail("write_failed", "The server could not index the podcast show. Check the feed again.")
	}
	s.setIndexed(id, show, true)
	return nil
}

// isIndexed reports whether the server indexed the show's current feed.json
// since this start (setIndexed marks it after a rescan and drops it when
// feed.json changes, before the rescan that follows can fail).
func (s *service) isIndexed(id int64, show string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.indexed[s.indexKey(id, show)]
}
func (s *service) setIndexed(id int64, show string, indexed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if indexed {
		s.indexed[s.indexKey(id, show)] = true
	} else {
		delete(s.indexed, s.indexKey(id, show))
	}
}
func (s *service) indexKey(id int64, show string) string {
	return s.o.ServerID() + "|" + strconv.FormatInt(id, 10) + "|" + show
}

// readDir is root's own entries by name: a folder a job picks among several
// that would do (relocate, find) never depends on the order the filesystem
// lists them in.
func readDir(root *os.Root) ([]os.DirEntry, error) {
	entries, err := listDir(root, ".")
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}

func (s *service) merge(root *os.Root, f *podcast.Feed, p *podcast.Parsed, show string) (int, error) {
	dir, err := root.Open(show)
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return podcast.Merge(f, p, show, s.o.Now(), names...)
}

// cache is the library's show cache rows, rebuilt from every show folder's
// feed.json when the library has none.
func (s *service) cache(ctx context.Context, root *os.Root, lib link.Library, entries []os.DirEntry) ([]store.PodcastShow, error) {
	shows, err := s.o.DB.PodcastShows(ctx, s.o.ServerID(), lib.ID)
	if err != nil || len(shows) > 0 {
		return shows, err
	}
	for _, e := range entries {
		if !e.IsDir() || !podcast.ValidShow(e.Name()) {
			continue
		}
		f, _, err := podcast.ReadMoved(root, e.Name())
		if err != nil {
			continue
		}
		row := s.showRow(lib, e.Name(), f)
		if err := s.o.DB.UpsertPodcastShow(ctx, row); err != nil {
			return nil, err
		}
		shows = append(shows, row)
	}
	return shows, nil
}

// find is the show folder a follow of feedURL joins, with its feed, or "" for
// a new show. The folders the cache rows name for the feed (or for the named
// show's id) are read first, every other show folder only when none of those
// holds it (a show copied in by hand, a feed URL the show moved from). A
// folder renamed by hand counts (ReadMoved), so it is never followed again as
// a new show. A follow naming its show (p.ShowPath: a private feed's secret
// entered again) joins that folder, or the one holding its id when that folder
// was renamed.
//
// It also answers the old names of the folders it read that were renamed by
// hand and are not taken in yet: for a new show every show folder was read, so
// these are all the names it must not take.
func (s *service) find(root *os.Root, entries []os.DirEntry, rows []store.PodcastShow, p subscribeParams, feedURL string) (string, *podcast.Feed, []string, error) {
	hash := podcast.FeedURLHash(feedURL)
	missing := fail("feed_secret_missing", "The show no longer matches this private feed. Follow the feed again.")
	gone := false
	if p.ShowPath != "" {
		var err error
		p.ShowPath, err = podcast.RealName(root, p.ShowPath)
		gone = errors.Is(err, os.ErrNotExist)
		if gone && p.ShowID == "" {
			return "", nil, nil, missing // no id to find the renamed folder by
		}
	}
	var renamed []string
	match := func(name string) *podcast.Feed {
		if !podcast.ValidShow(name) {
			return nil
		}
		f, from, err := podcast.ReadMoved(root, name)
		warnHeld(name, from, err)
		if err == nil && from != "" {
			renamed = append(renamed, from)
		}
		if err != nil || f.FeedURLSHA256 != hash && !slices.Contains(f.PreviousFeedURLSHA256, hash) {
			return nil
		}
		if gone && f.ShowID != p.ShowID {
			return nil
		}
		return f
	}
	if p.ShowPath != "" && !gone {
		if f := match(p.ShowPath); f != nil {
			return p.ShowPath, f, nil, nil
		}
		return "", nil, nil, missing
	}
	tried := map[string]bool{}
	for _, r := range rows {
		if r.FeedURLSHA256 == hash || p.ShowID != "" && r.ShowID == p.ShowID {
			at, _ := podcast.RealName(root, r.ShowPath) // spelt as on disk, as entries are
			tried[at] = true
			if f := match(at); f != nil {
				return at, f, nil, nil
			}
		}
	}
	for _, e := range entries {
		if e.IsDir() && !tried[e.Name()] {
			if f := match(e.Name()); f != nil {
				return e.Name(), f, nil, nil
			}
		}
	}
	if p.ShowPath != "" {
		return "", nil, nil, missing
	}
	return "", nil, renamed, nil
}

// lockShow takes the lock of the folder a job naming show, the server's name
// for show id (kept until its rescan), works in, and reads that folder
// (readShow), running guard first when given. It is show, or the folder
// holding id now when show was renamed by hand (relocate): show is gone, holds
// another show (names swapped by hand, or another show moved to its old name)
// or cannot be read (a feed.json the Bridge cannot read, or a copy of another
// show adopt refuses). A show folder that reads as id's but cannot be taken in
// (a copy of it, refused) is never left for another: its read's failure
// stands. A job never holds two show locks: show's is released before the
// other folder's is taken, and id is checked again under it. identity_changed
// when show holds another show and no other folder holds id; show's read
// failure when it cannot be read and none does. An invalid id (a poll's may
// be empty) reads show as it is. unlock releases the lock held; it is nil on
// an error.
func (s *service) lockShow(ctx context.Context, root *os.Root, lib link.Library, mount, show, id string, guard func(string) error) (_ string, _ *podcast.Feed, unlock func(), _ error) {
	// Keyed and read under its spelling on disk (a case-only rename by hand).
	show, _ = podcast.RealName(root, show)
	valid := podcast.ValidID(id)
	// searched: show is gone and no folder holds id, so the read of show that
	// then fails does not search every folder a second time.
	searched := false
	if _, err := root.Lstat(show); valid && errors.Is(err, os.ErrNotExist) {
		if moved := s.relocate(ctx, root, lib, id); moved != "" {
			show = moved
		} else {
			searched = true
		}
	}
	// open reports, with an error, whether it was show's read that failed.
	open := func(show string) (*podcast.Feed, func(), bool, error) {
		unlock, err := s.acquire(ctx, s.showKey(mount, show))
		if err != nil {
			return nil, nil, false, err
		}
		if guard != nil {
			if err := guard(show); err != nil {
				unlock()
				return nil, nil, false, err
			}
		}
		f, err := s.readShow(ctx, root, lib, mount, show)
		if err != nil {
			unlock()
			return nil, nil, true, err
		}
		return f, unlock, false, nil
	}
	changed := fail("identity_changed", "The podcast show identity changed.")
	f, unlock, unread, err := open(show)
	switch {
	case err == nil && (!valid || f.ShowID == id):
		return show, f, unlock, nil
	case err == nil:
		unlock()
		err = changed
	case !valid || !unread || searched || ctx.Err() != nil || holds(root, show, id) || s.wasUnfound(lib.ID, show, id):
		return "", nil, nil, err
	}
	moved := s.relocate(ctx, root, lib, id)
	if moved == "" || moved == show {
		s.markUnfound(lib.ID, show, id)
		return "", nil, nil, err
	}
	if f, unlock, _, err = open(moved); err != nil {
		return "", nil, nil, err
	}
	if f.ShowID != id {
		unlock()
		return "", nil, nil, changed // moved again since relocate read it
	}
	return moved, f, unlock, nil
}

// unfoundFor is how long a show folder that could not be read, its show held
// by no other folder, fails its jobs without a search of the library: a
// feed.json the Bridge cannot read would otherwise cost every job of the
// show (a fetch per episode) a read of every show's feed.json.
const unfoundFor = time.Minute

// wasUnfound reports whether lockShow searched the library for show id within
// unfoundFor, its folder show unreadable, and no folder held it.
func (s *service) wasUnfound(lib int64, show, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.unfound[s.unfoundKey(lib, show, id)]
	return ok && s.o.BudgetNow().Before(until)
}

// markUnfound records such a search, dropping the records past their time.
func (s *service) markUnfound(lib int64, show, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.o.BudgetNow() // the monotonic clock: a clock set back never extends one
	for k, until := range s.unfound {
		if !now.Before(until) {
			delete(s.unfound, k)
		}
	}
	s.unfound[s.unfoundKey(lib, show, id)] = now.Add(unfoundFor)
}

func (s *service) unfoundKey(lib int64, show, id string) string {
	return s.o.ServerID() + "|" + strconv.FormatInt(lib, 10) + "|" + show + "|" + id
}

// holds reports whether show's feed.json, read as a folder that may have been
// renamed by hand (podcast.ReadMoved), names show id.
func holds(root *os.Root, show, id string) bool {
	f, from, err := podcast.ReadMoved(root, show)
	warnHeld(show, from, err)
	return err == nil && f.ShowID == id
}

// warnHeld logs a show folder podcast.ReadMoved refused because held, the
// folder its feed.json names, is still there with no feed.json, holding audio
// or a partial (podcast.ErrHeld). Nothing sweeps a folder with no feed.json,
// so the refusal stands until someone acts on it: without the log a rename by
// hand that left such a file behind would never be taken in, with no word why.
func warnHeld(show, held string, err error) {
	if errors.Is(err, podcast.ErrHeld) {
		slog.Warn("podcasts: a show folder names an old folder that is still there without a feed.json, holding audio or a download's partial; it is not taken in until those files are removed or that folder has its feed.json again", "show", show, "holds", held)
	}
}

// searchHook, when a test sets it, is called once per library search
// (relocate reading every show folder).
var searchHook func()

// relocate is the folder holding show id, for a job whose show folder is gone,
// holds another show or cannot be read: one renamed by hand. The folder its
// cache row names is read first, every show folder only when that one does
// not hold it. "" when none does.
func (s *service) relocate(ctx context.Context, root *os.Root, lib link.Library, id string) string {
	tried := ""
	if row, ok, err := s.o.DB.PodcastShowByID(ctx, s.o.ServerID(), id); err == nil && ok && row.LibraryID == lib.ID {
		// A row written before a case-only rename by hand names the folder
		// under its old spelling, which still opens it.
		at, _ := podcast.RealName(root, row.ShowPath)
		if holds(root, at, id) {
			return at
		}
		tried = at
	}
	if searchHook != nil {
		searchHook()
	}
	entries, err := readDir(root)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && podcast.ValidShow(e.Name()) && e.Name() != tried && holds(root, e.Name(), id) {
			return e.Name()
		}
	}
	return ""
}

// readShow recovers show's pending journals and reads its feed.json, under
// the show's lock. A folder renamed by hand, whose feed.json and journals
// still name the old folder, is adopted first (adopt). Only a show either
// refuses pays for that: the common read parses feed.json once.
func (s *service) readShow(ctx context.Context, root *os.Root, lib link.Library, mount, show string) (*podcast.Feed, error) {
	read := func() (*podcast.Feed, error) {
		if err := podcast.RecoverShow(root, show, s.o.Now(), s.o.Fault); err != nil {
			return nil, err
		}
		return podcast.Read(root, show)
	}
	f, err := read()
	if errors.Is(err, podcast.ErrInvalidFile) {
		adopted, aerr := s.adopt(ctx, root, lib, mount, show)
		if aerr != nil {
			return nil, aerr
		}
		if adopted {
			f, err = read()
		}
	}
	if errors.Is(err, podcast.ErrTooLarge) {
		return nil, err // too_large (outcome), not a write fault
	}
	if err != nil {
		return nil, fail("write_failed", "The saved podcast feed could not be read.")
	}
	return f, nil
}

// adopt takes in show when it is a folder renamed by hand (podcast.Adopt):
// the episodes, their downloads and the show's state stay, the records of its
// interrupted downloads (adoptCheckpoints, first: a stop before feed.json
// moves takes the folder in again) and the cache row follow, and the server
// is asked to index the folder again. It reports whether show was such a
// folder.
func (s *service) adopt(ctx context.Context, root *os.Root, lib link.Library, mount, show string) (bool, error) {
	f, from, err := podcast.ReadMoved(root, show)
	warnHeld(show, from, err)
	if err != nil || from == "" {
		return false, nil
	}
	// A copy of a renamed folder made before either was taken in also names
	// the gone folder: once one of them holds the show in its own right (the
	// folder its cache row names), the other is a copy, never taken in (two
	// folders would hold one show).
	if row, ok, err := s.o.DB.PodcastShowByID(ctx, s.o.ServerID(), f.ShowID); err == nil && ok && row.LibraryID == lib.ID && row.ShowPath != show {
		if held, err := podcast.Read(root, row.ShowPath); err == nil && held.ShowID == f.ShowID {
			slog.Warn("podcasts: a show folder naming a gone folder is a copy of a show another folder holds; it is not taken in", "library", lib.ID, "show", show, "holder", row.ShowPath)
			return false, nil
		}
	}
	if err := s.beforeWrite(lib, mount, show); err != nil {
		return false, err
	}
	if err := s.adoptCheckpoints(ctx, root, lib, show, from, f.ShowID, 0); err != nil {
		slog.Warn("podcasts: the download records of a show folder renamed by hand could not be read; it is taken in later", "library", lib.ID, "show", show, "err", err)
		return false, nil
	}
	if err := podcast.Adopt(root, show, from, f, s.o.Now()); err != nil {
		slog.Warn("podcasts: a show folder renamed by hand could not be taken in", "library", lib.ID, "show", show, "err", err)
		return false, nil
	}
	s.setIndexed(lib.ID, show, false)
	if err := s.o.DB.UpsertPodcastShow(ctx, s.showRow(lib, show, f)); err != nil {
		slog.Warn("podcasts: the show cache could not record a renamed show folder", "library", lib.ID, "err", err)
	}
	slog.Info("podcasts: took in a show folder renamed by hand", "library", lib.ID, "show", show, "from", from)
	return true, nil
}
func (s *service) artwork(ctx context.Context, root *os.Root, lib link.Library, mount, show string, f *podcast.Feed, imageURL string) {
	if imageURL == "" {
		return
	}
	hash := podcast.FeedURLHash(imageURL)
	if f.Show.Image != nil && f.Show.Image.SourceSHA256 == hash {
		// Fetched already, unless its file is gone (deleted by hand): then the
		// show would have no artwork until the feed's image changed.
		if _, err := root.Lstat(show + "/" + f.Show.Image.File); err == nil {
			return
		}
	}
	state, err := s.o.DB.PodcastArtwork(ctx, s.o.ServerID(), lib.ID, f.ShowID)
	if err != nil {
		return
	}
	if state.SourceSHA256 != hash {
		state = store.PodcastArtworkState{SourceSHA256: hash}
	} else if due, e := time.Parse(time.RFC3339, state.NextAttemptAt); e == nil && s.o.Now().Before(due) {
		return
	}
	success := false
	defer func() {
		if success {
			state.Failures = 0
			state.NextAttemptAt = ""
		} else {
			state.Failures = min(state.Failures+1, 25)
			delay := time.Hour * time.Duration(1<<min(state.Failures-1, 5))
			delay = min(delay, 24*time.Hour)
			state.NextAttemptAt = s.o.Now().Add(delay).UTC().Format(time.RFC3339)
		}
		_ = s.o.DB.SavePodcastArtwork(ctx, s.o.ServerID(), lib.ID, f.ShowID, state)
	}()
	// Best effort: at most a minute, and half of what the job has left, so a
	// slow image host can't use up the time feed.json, the cache and the
	// rescan still need.
	budget := time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	actx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	resp, err := s.o.Fetcher.Fetch(actx, netguard.Request{URL: imageURL, MaxBytes: 10 << 20})
	if err != nil || resp.Status != 200 {
		return
	}
	name := ""
	switch http.DetectContentType(resp.Body) {
	case "image/jpeg":
		name = "cover.jpg"
	case "image/png":
		name = "cover.png"
	default:
		return
	}
	if s.beforeWrite(lib, mount, show) != nil {
		return
	}
	if podcast.WriteAtomic(root, show, name, resp.Body) != nil {
		return
	}
	if f.Show.Image != nil && f.Show.Image.File != name && (f.Show.Image.File == "cover.jpg" || f.Show.Image.File == "cover.png") {
		_ = root.Remove(show + "/" + f.Show.Image.File)
	}
	success = true
	f.Show.Image = &podcast.Image{File: name, SourceSHA256: hash, FetchedAt: s.o.Now().UTC().Format(time.RFC3339)}
}
func fail(code, message string) *netguard.Error { return &netguard.Error{Code: code, Message: message} }
func outcome(result any, err error) (any, error) {
	if err == nil {
		return result, nil
	}
	var failure *netguard.Error
	if errors.As(err, &failure) {
		return failure, failure
	}
	switch {
	case errors.Is(err, podcast.ErrInvalidFeedURL):
		failure = fail("invalid_feed_url", "The feed URL is invalid.")
	case errors.Is(err, podcast.ErrNotFeed):
		failure = fail("not_a_feed", "The response is not a feed.")
	case errors.Is(err, podcast.ErrNotPodcast):
		failure = fail("not_a_podcast", "The feed has no supported podcast episodes.")
	case errors.Is(err, podcast.ErrTooLarge):
		failure = fail("too_large", "The podcast feed exceeds its size or episode limit.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// The Bridge's own deadline or stop outside the feed's fetch (that
		// one's is the netguard's timeout): no fault of the feed's. Every
		// podcast job reports through here, so the message names none.
		failure = fail("bridge_lost", "The Bridge stopped the job before it finished.")
	default:
		failure = fail("write_failed", "The podcast show could not be saved.")
	}
	return failure, failure
}

var _ jobs.ParamSealer = (*Subscribe)(nil)
var _ jobs.Abandoner = (*Subscribe)(nil)
