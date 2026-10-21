package podcastjob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

const FetchKind = "podcast.fetch"
const RetainKind = "podcast.retain"

type Streamer interface {
	Stream(context.Context, netguard.StreamRequest) (*netguard.StreamResponse, error)
}
type Fetch struct{ s *service }

func (h *Subscribe) FetchJob() *Fetch { return &Fetch{s: h.s} }
func (h *Fetch) Kind() string         { return FetchKind }
func (h *Fetch) Lane() string         { return jobs.LaneDownload }

type fetchParams struct {
	ShowID    string `json:"show_id"`
	EpisodeID string `json:"episode_id"`
}
type fetchResult struct {
	LibraryID int64         `json:"library_id"`
	Path      string        `json:"path"`
	ShowID    string        `json:"show_id"`
	EpisodeID string        `json:"episode_id"`
	Fetch     podcast.Fetch `json:"fetch"`
	Chapters  int           `json:"chapters"`
	Reused    bool          `json:"reused"`
}

// EpisodeCeiling uses overflow-safe division; all admitted sizes are <=4096 MiB.
func EpisodeCeiling(bytes int64) time.Duration {
	if bytes < 0 || bytes > 4096<<20 {
		bytes = 4096 << 20
	}
	return 5*time.Minute + time.Duration(3*(bytes/262144+boolInt(bytes%262144 != 0))+6)*time.Second
}
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

type Timer interface {
	Reset(time.Duration) bool
	Stop() bool
}
type budget struct {
	start, deadline time.Time
	now             func() time.Time
	timer           Timer
	cancel          context.CancelFunc
}

func (s *service) budget(parent context.Context) (context.Context, *budget) {
	ctx, cancel := context.WithCancel(parent)
	start := s.o.BudgetNow()
	d := EpisodeCeiling(s.o.MaxEpisodeBytes)
	b := &budget{start: start, deadline: start.Add(d), now: s.o.BudgetNow, cancel: cancel}
	b.timer = s.o.AfterFunc(d, cancel)
	return ctx, b
}
func (b *budget) tighten(total int64) {
	d := b.start.Add(EpisodeCeiling(total))
	if d.Before(b.deadline) {
		b.deadline = d
		left := d.Sub(b.now())
		if left <= 0 {
			b.cancel()
		} else {
			b.timer.Reset(left)
		}
	}
}
func (b *budget) close() { b.timer.Stop(); b.cancel() }

func (h *Fetch) Run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	result, err := h.run(parent, job, rep)
	if err != nil && (parent.Err() == nil || !jobs.Resumes(parent)) {
		h.Abandon(parent, job)
	}
	return outcome(result, err)
}
func (h *Fetch) run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	s := h.s
	var params fetchParams
	if job.Path == nil || !podcast.ValidEpisodePath(*job.Path) || json.Unmarshal(job.Params, &params) != nil || !podcast.ValidID(params.ShowID) || !podcast.ValidID(params.EpisodeID) {
		return nil, fail("episode_not_found", "The podcast episode was not found.")
	}
	waitCtx, cancel := context.WithTimeout(parent, ceiling)
	lib, mount, err := s.library(waitCtx, job, rep)
	cancel()
	if err != nil {
		return nil, err
	}
	unlock, err := s.acquire(parent, s.o.ServerID()+"|fetch|"+mount+"/"+*job.Path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	root, err := os.OpenRoot(mount)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	// Read the show before creating lock artifacts, so invalid paths write nothing.
	if _, _, err = s.readEpisode(root, *job.Path, params); err != nil {
		return nil, err
	}
	if err := s.beforeWrite(lib, mount, path.Dir(*job.Path)); err != nil {
		return nil, err
	}
	release, err := s.fileLock(parent, root, *job.Path)
	if err != nil {
		return nil, err
	}
	defer release()
	pairing := s.pairHash()
	f, e, err := s.fetchSnapshot(parent, root, lib, mount, *job.Path, params)
	if err != nil {
		return nil, err
	}
	if e.Fetch.Valid() && e.Fetch.DeletedAt == "" && podcast.Matches(root, e.Path, e.Fetch) {
		return s.reuse(parent, lib, e, f.ShowID)
	}
	if _, err := root.Lstat(e.Path); !errors.Is(err, os.ErrNotExist) {
		return nil, fail("write_failed", "An unverified episode file needs administrator repair.")
	}
	ctx, b := s.budget(parent)
	defer b.close()
	c, tag := s.loadCheckpoint(job, root, f.ShowID, e.ID)
	committed := false
	defer func() {
		if s.writable(lib, mount) != nil || pairing != s.pairHash() {
			return
		}
		keep := false
		if !committed && parent.Err() != nil && jobs.Resumes(parent) && c.Temp != "" {
			saved, validator := s.loadCheckpoint(job, root, params.ShowID, params.EpisodeID)
			keep = saved.Temp == c.Temp && saved.Offset == c.Offset && saved.Total == c.Total && netguard.StrongETag(validator)
		}
		if !keep {
			s.clearCheckpoint(job, root, &c)
		}
	}()
	rep.Progress(jobs.Progress{Step: "resolving", Message: "Resolving the episode"})
	received, err := s.download(ctx, b, job, root, f, e, &c, tag, rep)
	if err != nil {
		return nil, err
	}
	rep.Progress(jobs.Progress{Step: "probing", Message: "Checking episode audio"})
	rd, err := podcast.OpenRegular(root, path.Dir(e.Path)+"/"+c.Temp)
	if err != nil {
		return nil, err
	}
	probe, err := s.o.Probe(ctx, rd)
	if err != nil {
		_ = rd.Close()
		return nil, err
	}
	if probe.DurationMS <= 0 || probe.Codec == "" || probe.ContentType == "" {
		_ = rd.Close()
		return nil, fail("invalid_audio", "The episode is not supported audio.")
	}
	received.DurationMS = probe.DurationMS
	received.Codec = probe.Codec
	received.ContentType = probe.ContentType
	received.FetchedAt = s.o.Now().UTC().Format(time.RFC3339)
	received, err = podcast.NextFetch(e.Fetch, received)
	if err != nil {
		_ = rd.Close()
		return nil, err
	}
	_, err = rd.Seek(0, io.SeekStart)
	if err != nil {
		_ = rd.Close()
		return nil, err
	}
	chapters := podcast.ReadCHAP(rd, e.Path, received)
	_ = rd.Close()
	rep.Progress(jobs.Progress{Step: "writing", Message: "Saving the episode"})
	unlockShow, err := s.acquire(ctx, s.showKey(mount, path.Dir(e.Path)))
	if err != nil {
		return nil, err
	}
	err = func() error {
		defer unlockShow()
		if err := rep.ConfirmLease(ctx); err != nil {
			return fail("identity_changed", "The episode job no longer owns its claim.")
		}
		if pairing != s.pairHash() {
			return fail("identity_changed", "The Bridge pairing changed.")
		}
		if err := s.beforeWrite(lib, mount, path.Dir(e.Path)); err != nil {
			return err
		}
		if err := podcast.RecoverShow(root, path.Dir(e.Path), s.o.Now(), s.o.Fault); err != nil {
			return err
		}
		_, latest, err := s.readEpisode(root, e.Path, params)
		if err != nil {
			return err
		}
		if !sameFetch(latest.Fetch, e.Fetch) {
			return fail("identity_changed", "The episode generation changed.")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j := podcast.Journal{Version: 1, Op: "install", ShowID: params.ShowID, EpisodeID: e.ID, Path: e.Path, AudioTemp: c.Temp, Fetch: received, Chapters: chapters}
		// Once a journal exists recovery owns the candidate, including on a failed write.
		err = podcast.Publish(root, j, s.o.Now(), s.o.Fault)
		if _, e := root.Lstat(podcast.AttachmentPath(j.Path, ".publish.json")); e == nil {
			c.Temp = ""
		}
		return err
	}()
	if err != nil {
		return nil, err
	}
	committed = true
	s.setIndexed(lib.ID, path.Dir(e.Path), false)
	rep.Progress(jobs.Progress{Step: "indexing", Message: "Indexing the episode"})
	if err := s.episodeRescan(ctx, lib.ID, e.Path); err != nil {
		return nil, err
	}
	n := 0
	if chapters != nil {
		n = len(chapters.Chapters)
	}
	return fetchResult{lib.ID, e.Path, f.ShowID, e.ID, received, n, false}, nil
}
func (s *service) showKey(mount, show string) string {
	return s.o.ServerID() + "|" + mount + "/" + show
}
func (s *service) readEpisode(root *os.Root, p string, params fetchParams) (*podcast.Feed, podcast.Episode, error) {
	f, err := podcast.Read(root, path.Dir(p))
	if err != nil {
		return nil, podcast.Episode{}, fail("episode_not_found", "The podcast episode was not found.")
	}
	if f.ShowID != params.ShowID {
		return nil, podcast.Episode{}, fail("identity_changed", "The podcast show identity changed.")
	}
	for _, e := range f.Episodes {
		if e.Path == p {
			if e.ID != params.EpisodeID {
				return nil, e, fail("identity_changed", "The podcast episode identity changed.")
			}
			return f, e, nil
		}
	}
	return nil, podcast.Episode{}, fail("episode_not_found", "The podcast episode was not found.")
}
func (s *service) fetchSnapshot(ctx context.Context, root *os.Root, lib link.Library, mount, p string, params fetchParams) (*podcast.Feed, podcast.Episode, error) {
	unlock, err := s.acquire(ctx, s.showKey(mount, path.Dir(p)))
	if err != nil {
		return nil, podcast.Episode{}, err
	}
	defer unlock()
	if err := s.beforeWrite(lib, mount, path.Dir(p)); err != nil {
		return nil, podcast.Episode{}, err
	}
	if err := podcast.RecoverShow(root, path.Dir(p), s.o.Now(), s.o.Fault); err != nil {
		return nil, podcast.Episode{}, err
	}
	return s.readEpisode(root, p, params)
}
func sameFetch(a, b *podcast.Fetch) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func (s *service) reuse(ctx context.Context, lib link.Library, e podcast.Episode, showID string) (any, error) {
	if err := s.episodeRescan(ctx, lib.ID, e.Path); err != nil {
		return nil, err
	}
	n := 0
	// Chapters are counted only from the owned, bounded attachment, never publisher tags.
	mount, _ := s.o.Mount(lib.ID, lib.Root)
	if root, err := os.OpenRoot(mount); err == nil {
		defer func() { _ = root.Close() }()
		if rd, err := podcast.OpenRegular(root, podcast.AttachmentPath(e.Path, ".chapters.json")); err == nil {
			defer rd.Close()
			var list podcast.ChapterList
			if json.NewDecoder(io.LimitReader(rd, 4<<20)).Decode(&list) == nil {
				n = len(list.Chapters)
			}
		}
	}
	return fetchResult{lib.ID, e.Path, showID, e.ID, *e.Fetch, n, true}, nil
}
func (s *service) episodeRescan(ctx context.Context, id int64, p string) error {
	srv := s.o.Server()
	if srv == nil {
		return fail("index_failed", "The episode could not be indexed. Retry the job.")
	}
	if _, err := srv.RescanPodcast(ctx, id, p); err != nil {
		return fail("index_failed", "The episode could not be indexed. Retry the job.")
	}
	s.setIndexed(id, path.Dir(p), true)
	return nil
}

func (s *service) enclosure(ctx context.Context, f *podcast.Feed, e podcast.Episode) (string, error) {
	if !f.Private {
		if e.Enclosure.URL == "" {
			return "", fail("enclosure_missing", "The episode has no available enclosure.")
		}
		return e.Enclosure.URL, nil
	}
	raw, err := s.feedURL("", true, secrets.PodcastFeedName(f.ShowID), "The private feed address is invalid.")
	if err != nil {
		return "", err
	}
	resp, err := s.o.Fetcher.Fetch(ctx, netguard.Request{URL: raw, MaxBytes: s.o.MaxFeedBytes})
	if err != nil {
		return "", err
	}
	parsed, err := podcast.ParseExisting(resp.Body, resp.URL)
	if err != nil {
		return "", err
	}
	var exact, weak []podcast.Episode
	for _, candidate := range parsed.Episodes {
		guid := candidate.GUID
		if guid != "" && e.GUID == "sha256:"+podcast.FeedURLHash(guid) {
			guid = e.GUID
		}
		if podcast.EpisodeID(candidate) == e.ID || e.GUID != "" && e.GUID == guid {
			exact = append(exact, candidate)
		}
		if podcast.RedactText(candidate.Title) == e.Title && candidate.Published == e.Published {
			weak = append(weak, candidate)
		}
	}
	if len(exact) == 1 && exact[0].Enclosure.URL != "" {
		return exact[0].Enclosure.URL, nil
	}
	if len(exact) == 0 && len(weak) == 1 && weak[0].Enclosure.URL != "" {
		return weak[0].Enclosure.URL, nil
	}
	return "", fail("enclosure_missing", "The episode enclosure could not be identified.")
}

func (s *service) download(ctx context.Context, b *budget, job jobs.Job, root *os.Root, f *podcast.Feed, e podcast.Episode, c *checkpoint, tag string, rep *jobs.Reporter) (podcast.Fetch, error) {
	if s.o.Streamer == nil {
		return podcast.Fetch{}, fail("network", "The episode download is unavailable.")
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return podcast.Fetch{}, ctx.Err()
		}
		raw, err := s.enclosure(ctx, f, e)
		if err != nil {
			return podcast.Fetch{}, err
		}
		response, err := s.o.Streamer.Stream(ctx, netguard.StreamRequest{URL: raw, Offset: c.Offset, ETag: tag})
		if err == nil {
			action := "restart_with_200_body"
			if c.Offset > 0 {
				action = netguard.ResumeAction(c.Offset, c.Total, tag, response)
			} else if response.Status == 206 {
				action = "fresh_get"
			}
			if response.Status != 200 && response.Status != 206 && (response.Status != 416 || c.Offset == 0) {
				err = &netguard.Error{Code: "http_" + strconv.Itoa(response.Status), Message: fmt.Sprintf("The episode returned HTTP %d.", response.Status), RetryAfterSeconds: response.RetryAfterSeconds}
			} else if action == "fresh_get" {
				_ = response.Body.Close()
				s.clearCheckpoint(job, root, c)
				c.Temp = ""
				c.Offset = 0
				c.Total = 0
				c.Validator = ""
				tag = ""
				last = fail("network", "The episode resume could not be verified.")
				continue
			} else {
				var fetch podcast.Fetch
				fetch, tag, err = s.consume(ctx, b, job, root, c, tag, response, action, rep)
				_ = response.Body.Close()
				if err == nil {
					return fetch, nil
				}
			}
			_ = response.Body.Close()
		}
		last = netguard.Classify(err)
		failure := netguard.Classify(err)
		if attempt == 2 || !netguard.Retryable(err) || failure.RetryAfterSeconds > 10 || ctx.Err() != nil {
			break
		}
		if !netguard.StrongETag(tag) || c.Offset <= 0 || c.Total <= c.Offset {
			s.clearCheckpoint(job, root, c)
			c.Temp = ""
			c.Offset = 0
			c.Total = 0
			c.Validator = ""
			tag = ""
		}
		if err := s.o.Wait(ctx, netguard.RetryDelay(attempt, failure.RetryAfterSeconds)); err != nil {
			return podcast.Fetch{}, err
		}
	}
	if last == nil {
		last = fail("network", "The episode download could not be completed.")
	}
	return podcast.Fetch{}, last
}
func (s *service) consume(ctx context.Context, b *budget, job jobs.Job, root *os.Root, c *checkpoint, tag string, r *netguard.StreamResponse, action string, rep *jobs.Reporter) (podcast.Fetch, string, error) {
	if r.Encoding != "" && r.Encoding != "identity" {
		return podcast.Fetch{}, "", fail("unsupported_encoding", "The episode uses an unsupported encoding.")
	}
	total := c.Total
	if action != "append" {
		total = r.ContentLength
		c.Offset = 0
		tag = r.ETag
	}
	if total > s.o.MaxEpisodeBytes {
		return podcast.Fetch{}, "", fail("too_large", "The episode exceeds its size limit.")
	}
	if total > 0 {
		b.tighten(total)
	}
	freshTemp := c.Temp == ""
	if freshTemp {
		c.Temp = ".audiosilo-tmp-" + nonce()
	}
	name := path.Dir(c.Path) + "/" + c.Temp
	flags := os.O_RDWR
	if freshTemp {
		flags |= os.O_EXCL | os.O_CREATE
	}
	if action != "append" {
		if st, err := root.Lstat(name); err == nil && !st.Mode().IsRegular() {
			return podcast.Fetch{}, "", podcast.ErrInvalidFile
		}
		flags |= os.O_CREATE
	}
	file, err := root.OpenFile(name, flags, 0o600)
	if err != nil {
		return podcast.Fetch{}, "", err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return podcast.Fetch{}, "", err
	}
	h := sha256.New()
	if action == "append" {
		if n, err := io.CopyN(h, file, c.Offset); err != nil || n != c.Offset {
			return podcast.Fetch{}, "", fail("network", "The episode partial is incomplete.")
		}
	} else if err := file.Truncate(0); err != nil {
		return podcast.Fetch{}, "", err
	}
	c.Total = total
	out := &downloadWriter{file: file, hash: h, rep: rep, received: c.Offset, total: total}
	lastCheckpoint := time.Time{}
	out.check = func(received int64) error {
		lib, ok := s.o.Libraries.Library(*job.LibraryID)
		if !ok {
			return fail("library_not_writable", "The podcast mapping changed.")
		}
		if err := s.writable(lib, root.Name()); err != nil {
			return err
		}
		if c.ServerHash != s.pairHash() {
			return fail("identity_changed", "The Bridge pairing changed.")
		}
		if _, err := mapping.SafeJoin(root.Name(), name); err != nil {
			return err
		}
		if received > 0 && received < total && netguard.StrongETag(tag) && time.Since(lastCheckpoint) >= 10*time.Second {
			if err := file.Sync(); err != nil {
				return err
			}
			c.Offset = received
			if err := s.saveCheckpoint(job, c, tag); err != nil {
				return err
			}
			lastCheckpoint = time.Now()
		}
		return ctx.Err()
	}
	out.progress()
	_, copyErr := io.CopyBuffer(out, io.LimitReader(r.Body, s.o.MaxEpisodeBytes-c.Offset+1), make([]byte, 64<<10))
	c.Offset = out.received
	if err := file.Sync(); err != nil {
		return podcast.Fetch{}, "", err
	}
	if c.Offset > s.o.MaxEpisodeBytes {
		return podcast.Fetch{}, "", fail("too_large", "The episode exceeds its size limit.")
	}
	if copyErr == nil && (c.Offset == 0 || total >= 0 && c.Offset != total) {
		copyErr = io.ErrUnexpectedEOF
	}
	if copyErr != nil {
		var guard *netguard.Error
		if errors.As(copyErr, &guard) {
			return podcast.Fetch{}, tag, guard
		}
		if c.Offset > 0 && c.Offset < total && netguard.StrongETag(tag) {
			if err := s.saveCheckpoint(job, c, tag); err != nil {
				return podcast.Fetch{}, "", err
			}
		}
		if ctx.Err() != nil {
			return podcast.Fetch{}, tag, ctx.Err()
		}
		return podcast.Fetch{}, tag, fail("network", "The episode download was interrupted.")
	}
	return podcast.Fetch{Bytes: c.Offset, SHA256: hex.EncodeToString(h.Sum(nil))}, tag, nil
}

type downloadWriter struct {
	check           func(int64) error
	file            *os.File
	hash            hash.Hash
	rep             *jobs.Reporter
	received, total int64
	last            time.Time
}

func (w *downloadWriter) Write(p []byte) (int, error) {
	if w.check != nil {
		if err := w.check(w.received); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.received += int64(n)
		if time.Since(w.last) >= time.Second {
			w.progress()
		}
	}
	return n, err
}
func (w *downloadWriter) progress() {
	var total any
	if w.total >= 0 {
		total = w.total
	}
	w.rep.Progress(jobs.Progress{Step: "fetching", Message: "Fetching episode", Extra: map[string]any{"bytes_received": w.received, "bytes_total": total}})
	w.last = time.Now()
}

// Abandon handles a terminal cancel/lost claim after a resumable stop. Mapping
// and identities must still match before any owned partial is removed.
func (h *Fetch) Abandon(ctx context.Context, job jobs.Job) {
	if job.Path == nil || job.LibraryID == nil || !podcast.ValidEpisodePath(*job.Path) {
		return
	}
	var p fetchParams
	if json.Unmarshal(job.Params, &p) != nil {
		return
	}
	s := h.s
	lib, ok := s.o.Libraries.Library(*job.LibraryID)
	if !ok {
		return
	}
	mount, _ := s.o.Mount(lib.ID, lib.Root)
	if s.writable(lib, mount) != nil {
		return
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	release, err := s.fileLock(ctx, root, *job.Path)
	if err != nil {
		return
	}
	defer release()
	c, _ := s.loadCheckpoint(job, root, p.ShowID, p.EpisodeID)
	s.clearCheckpoint(job, root, &c)
}

var _ jobs.Abandoner = (*Fetch)(nil)
