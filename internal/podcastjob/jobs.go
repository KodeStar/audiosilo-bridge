// Package podcastjob runs the light podcast.subscribe and podcast.poll jobs.
package podcastjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
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
}
type Server interface {
	RescanPodcast(context.Context, int64, string) (*link.PodcastRescan, error)
}
type Fetcher interface {
	Fetch(context.Context, netguard.Request) (*netguard.Response, error)
}
type Options struct {
	DB           *store.DB
	Libraries    Libraries
	Mount        mapping.MountFunc
	Server       func() Server
	Vault        secrets.Store
	Fetcher      Fetcher
	MaxFeedBytes int64
	Version      string
	ServerID     func() string
	Now          func() time.Time
}
type service struct {
	o     Options
	mu    sync.Mutex
	locks map[string]*lock
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
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ServerID == nil {
		o.ServerID = func() string { return "" }
	}
	return &Subscribe{s: &service{o: o, locks: map[string]*lock{}}}
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
}

// SealParams is called before RecordClaim. Run receives only the sealed form.
func (h *Subscribe) SealParams(job jobs.Job) (json.RawMessage, error) {
	var p subscribeParams
	if json.Unmarshal(job.Params, &p) != nil {
		return nil, errors.New("invalid podcast params")
	}
	norm, err := podcast.NormalizeFeedURL(p.FeedURL)
	if err != nil {
		return nil, err
	}
	p.FeedURL = norm
	p.Private = p.Private || podcast.IsPrivateFeedURL(norm)
	p.FeedURLSHA256 = podcast.FeedURLHash(norm)
	u, _ := url.Parse(norm)
	p.FeedHost = u.Hostname()
	if p.Private {
		if err := h.s.o.Vault.Set(secrets.PodcastJobName(job.ID), norm); err != nil {
			return nil, err
		}
		p.FeedURL = ""
	}
	return json.Marshal(p)
}
func (h *Subscribe) Abandon(_ context.Context, job jobs.Job) {
	_ = h.s.o.Vault.Delete(secrets.PodcastJobName(job.ID))
}

func (h *Subscribe) Run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	defer func() {
		if parent.Err() == nil || !jobs.Resumes(parent) {
			h.Abandon(parent, job)
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	result, err := h.run(ctx, job, rep)
	return outcome(result, err)
}
func (h *Poll) Run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	result, err := h.run(ctx, job, rep)
	return outcome(result, err)
}

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

func (s *service) library(job jobs.Job) (link.Library, string, error) {
	if job.LibraryID == nil {
		return link.Library{}, "", fail("not_podcast_library", "This job needs a podcast library.")
	}
	lib, ok := s.o.Libraries.Library(*job.LibraryID)
	if !ok || lib.LibraryKind() != "podcasts" {
		return lib, "", fail("not_podcast_library", "This job needs a podcast library.")
	}
	mount, _ := s.o.Mount(lib.ID, lib.Root)
	return lib, mount, s.writable(lib, mount)
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

func (h *Subscribe) run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	s := h.s
	lib, mount, err := s.library(job)
	if err != nil {
		return nil, err
	}
	unlock, err := s.acquire(ctx, s.o.ServerID()+"|"+lib.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var p subscribeParams
	if json.Unmarshal(job.Params, &p) != nil {
		return nil, fail("invalid_feed_url", "The feed request is invalid.")
	}
	feedURL := p.FeedURL
	if p.Private {
		feedURL, err = s.o.Vault.Get(secrets.PodcastJobName(job.ID))
		if err != nil || feedURL == "" {
			return nil, fail("feed_secret_missing", "The private feed secret is missing. Follow the feed again.")
		}
	}
	feedURL, err = podcast.NormalizeFeedURL(feedURL)
	if err != nil {
		return nil, fail("invalid_feed_url", "The feed URL is invalid.")
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
	if err := s.rebuild(ctx, root, lib, entries); err != nil {
		return nil, fail("write_failed", "The podcast cache could not be rebuilt.")
	}
	show, f, err := s.find(root, entries, p, feedURL)
	if err != nil {
		return nil, err
	}
	created := f == nil
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
		var names []string
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
		if err := root.Chmod(show, 0o755); err != nil {
			return nil, fail("write_failed", "The show folder permissions could not be set.")
		}
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, fail("write_failed", "The show identity could not be created.")
		}
		u, _ := url.Parse(feedURL)
		f = &podcast.Feed{Version: 1, Generator: "audiosilo-bridge/" + s.o.Version, ShowID: hex.EncodeToString(random[:]), FeedURL: feedURL, FeedURLSHA256: podcast.FeedURLHash(feedURL), FeedHost: u.Hostname(), PreviousFeedURLSHA256: []string{}, Private: p.Private || podcast.IsPrivateFeedURL(feedURL), SubscribedAt: s.o.Now().UTC().Format(time.RFC3339), Episodes: []podcast.Episode{}}
	}
	unlockShow, err := s.acquire(ctx, s.o.ServerID()+"|"+mount+"/"+show)
	if err != nil {
		return nil, err
	}
	defer unlockShow()
	// A poll may have updated it while subscribe was fetching.
	if !created {
		f, err = podcast.Read(root, show)
		if err != nil {
			return nil, fail("write_failed", "The saved podcast feed could not be read.")
		}
	}
	f.Private = f.Private || p.Private
	newURL, _ := moveURL(f, feedURL, resp, parsed)
	if _, err := s.merge(root, f, parsed, show); err != nil {
		return nil, err
	}
	rep.Progress(jobs.Progress{Step: "writing", Message: "Saving the podcast show"})
	if err := s.beforeWrite(lib, mount, show); err != nil {
		return nil, err
	}
	s.artwork(ctx, root, lib, mount, show, f, parsed.ImageURL)
	if err := s.beforeWrite(lib, mount, show); err != nil {
		return nil, err
	}
	if _, err := podcast.Write(root, show, f, s.o.Now()); err != nil {
		return nil, err
	}
	if err := s.save(ctx, lib, show, f, newURL); err != nil {
		return nil, err
	}
	rep.Progress(jobs.Progress{Step: "indexing", Message: "Indexing the podcast show"})
	if err := s.rescan(ctx, lib.ID, show); err != nil {
		return nil, err
	}
	return map[string]any{"library_id": lib.ID, "show_path": show, "show_id": f.ShowID, "created": created, "private": f.Private, "episodes": len(f.Episodes), "skipped": parsed.Skipped, "status": resp.Status, "etag": resp.ETag, "last_modified": resp.LastModified, "next_poll_seconds": podcast.NextPoll(f, resp.CacheControl, s.o.Now())}, nil
}

func (h *Poll) run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	s := h.s
	lib, mount, err := s.library(job)
	if err != nil {
		return nil, err
	}
	if job.Path == nil || !podcast.ValidShow(*job.Path) {
		return nil, fail("not_a_feed", "The podcast show path is invalid.")
	}
	show := *job.Path
	unlock, err := s.acquire(ctx, s.o.ServerID()+"|"+mount+"/"+show)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := mapping.SafeJoin(mount, show); err != nil {
		return nil, fail("write_failed", "The show path is invalid.")
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return nil, fail("library_not_writable", "The podcast library could not be opened.")
	}
	defer func() { _ = root.Close() }()
	f, err := podcast.Read(root, show)
	if err != nil {
		return nil, fail("not_a_feed", "The saved podcast feed could not be read.")
	}
	feedURL := f.FeedURL
	if f.Private {
		feedURL, err = s.o.Vault.Get(secrets.PodcastFeedName(f.ShowID))
		if err != nil || feedURL == "" {
			return nil, fail("feed_secret_missing", "The private feed secret is missing. Follow the feed again.")
		}
	}
	feedURL, err = podcast.NormalizeFeedURL(feedURL)
	if err != nil {
		return nil, fail("invalid_feed_url", "The saved feed URL is invalid.")
	}
	var params struct {
		ETag         string `json:"etag"`
		LastModified string `json:"last_modified"`
	}
	if len(job.Params) > 0 && json.Unmarshal(job.Params, &params) != nil || len(params.ETag) > 512 || len(params.LastModified) > 512 {
		return nil, fail("not_a_feed", "The feed check parameters are invalid.")
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
		if err := s.rebuild(ctx, root, lib, entries); err != nil {
			return nil, err
		}
		rep.Progress(jobs.Progress{Step: "parsing", Message: "Reading the podcast feed"})
		parsed, err := podcast.ParseExisting(resp.Body, resp.URL)
		if err != nil {
			return nil, err
		}
		skipped = parsed.Skipped
		feedURL, moved = moveURL(f, feedURL, resp, parsed)
		added, err = s.merge(root, f, parsed, show)
		if err != nil {
			return nil, err
		}
		rep.Progress(jobs.Progress{Step: "writing", Message: "Saving changed podcast details"})
		if err := s.beforeWrite(lib, mount, show); err != nil {
			return nil, err
		}
		s.artwork(ctx, root, lib, mount, show, f, parsed.ImageURL)
		if err := s.beforeWrite(lib, mount, show); err != nil {
			return nil, err
		}
		changed, err = podcast.Write(root, show, f, s.o.Now())
		if err != nil {
			return nil, err
		}
		if err := s.save(ctx, lib, show, f, feedURL); err != nil {
			return nil, err
		}
		if changed {
			rep.Progress(jobs.Progress{Step: "indexing", Message: "Indexing changed podcast details"})
			if err := s.rescan(ctx, lib.ID, show); err != nil {
				return nil, err
			}
		}
	}
	return map[string]any{"library_id": lib.ID, "show_path": show, "status": resp.Status, "changed": changed, "new_episodes": added, "episodes": len(f.Episodes), "skipped": skipped, "moved": moved, "etag": resp.ETag, "last_modified": resp.LastModified, "next_poll_seconds": podcast.NextPoll(f, resp.CacheControl, s.o.Now())}, nil
}

func moveURL(f *podcast.Feed, original string, resp *netguard.Response, p *podcast.Parsed) (string, bool) {
	current := original
	if resp.Permanent {
		if norm, err := podcast.NormalizeFeedURL(resp.URL); err == nil {
			current = norm
		}
	}
	if norm, err := podcast.NormalizeFeedURL(p.NewFeedURL); err == nil && norm != original {
		current = norm
	}
	hash := podcast.FeedURLHash(current)
	moved := hash != f.FeedURLSHA256
	if moved {
		f.PreviousFeedURLSHA256 = append([]string{f.FeedURLSHA256}, f.PreviousFeedURLSHA256...)
		f.PreviousFeedURLSHA256 = slices.Compact(f.PreviousFeedURLSHA256)
		if len(f.PreviousFeedURLSHA256) > 5 {
			f.PreviousFeedURLSHA256 = f.PreviousFeedURLSHA256[:5]
		}
		f.FeedURLSHA256 = hash
	}
	f.Private = f.Private || podcast.IsPrivateFeedURL(current)
	f.FeedURL = current
	u, _ := url.Parse(current)
	f.FeedHost = u.Hostname()
	f.ScrubPrivate()
	return current, moved
}

func (s *service) save(ctx context.Context, lib link.Library, show string, f *podcast.Feed, feedURL string) error {
	if f.Private {
		if err := s.o.Vault.Set(secrets.PodcastFeedName(f.ShowID), feedURL); err != nil {
			return fail("write_failed", "The private feed credentials could not be saved.")
		}
	}
	return s.o.DB.UpsertPodcastShow(ctx, store.PodcastShow{ServerID: s.o.ServerID(), LibraryID: lib.ID, ShowID: f.ShowID, ShowPath: show, FeedURLSHA256: f.FeedURLSHA256, Private: f.Private, CreatedAt: f.SubscribedAt})
}
func (s *service) rescan(ctx context.Context, id int64, show string) error {
	srv := s.o.Server()
	if srv == nil {
		return fail("write_failed", "The server is unavailable for indexing. Check the feed again.")
	}
	if _, err := srv.RescanPodcast(ctx, id, show); err != nil {
		return fail("write_failed", "The server could not index the podcast show. Check the feed again.")
	}
	return nil
}
func readDir(root *os.Root) ([]os.DirEntry, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
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
func (s *service) rebuild(ctx context.Context, root *os.Root, lib link.Library, entries []os.DirEntry) error {
	shows, err := s.o.DB.PodcastShows(ctx, s.o.ServerID(), lib.ID)
	if err != nil || len(shows) > 0 {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !podcast.ValidShow(e.Name()) {
			continue
		}
		f, err := podcast.Read(root, e.Name())
		if err != nil {
			continue
		}
		if err := s.o.DB.UpsertPodcastShow(ctx, store.PodcastShow{ServerID: s.o.ServerID(), LibraryID: lib.ID, ShowID: f.ShowID, ShowPath: e.Name(), FeedURLSHA256: f.FeedURLSHA256, Private: f.Private, CreatedAt: f.SubscribedAt}); err != nil {
			return err
		}
	}
	return nil
}
func (s *service) find(root *os.Root, entries []os.DirEntry, p subscribeParams, feedURL string) (string, *podcast.Feed, error) {
	hash := podcast.FeedURLHash(feedURL)
	for _, e := range entries {
		if !e.IsDir() || !podcast.ValidShow(e.Name()) {
			continue
		}
		f, err := podcast.Read(root, e.Name())
		if err != nil {
			continue
		}
		if f.FeedURLSHA256 == hash || slices.Contains(f.PreviousFeedURLSHA256, hash) {
			if p.ShowPath != "" && p.ShowPath != e.Name() {
				continue
			}
			return e.Name(), f, nil
		}
	}
	if p.ShowPath != "" {
		return "", nil, fail("feed_secret_missing", "The show no longer matches this private feed. Follow the feed again.")
	}
	return "", nil, nil
}
func (s *service) artwork(ctx context.Context, root *os.Root, lib link.Library, mount, show string, f *podcast.Feed, imageURL string) {
	if imageURL == "" {
		return
	}
	hash := podcast.FeedURLHash(imageURL)
	if f.Show.Image != nil && f.Show.Image.SourceSHA256 == hash {
		return
	}
	resp, err := s.o.Fetcher.Fetch(ctx, netguard.Request{URL: imageURL, MaxBytes: 10 << 20})
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
	if _, err := mapping.SafeJoin(mount, show+"/"+name); err != nil {
		return
	}
	if podcast.WriteAtomic(root, show, name, resp.Body) != nil {
		return
	}
	if f.Show.Image != nil && f.Show.Image.File != name && (f.Show.Image.File == "cover.jpg" || f.Show.Image.File == "cover.png") {
		_ = root.Remove(show + "/" + f.Show.Image.File)
	}
	f.Show.Image = &podcast.Image{File: name, SourceSHA256: hash, FetchedAt: s.o.Now().UTC().Format(time.RFC3339)}
}
func fail(code, message string) *netguard.Error { return &netguard.Error{Code: code, Message: message} }
func outcome(result any, err error) (any, error) {
	if err == nil {
		return result, nil
	}
	var failure *netguard.Error
	if errors.As(err, &failure) {
		if failure.Code == "network" {
			failure = fail("timeout", failure.Message)
		}
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
		failure = fail("timeout", "The feed check did not finish in time.")
	default:
		failure = fail("write_failed", "The podcast show could not be saved.")
	}
	return failure, failure
}

var _ jobs.ParamSealer = (*Subscribe)(nil)
var _ jobs.Abandoner = (*Subscribe)(nil)
