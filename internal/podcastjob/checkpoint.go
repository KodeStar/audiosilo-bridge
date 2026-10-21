package podcastjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

type checkpoint struct {
	ServerHash string `json:"server_hash"`
	LibraryID  int64  `json:"library_id"`
	ShowID     string `json:"show_id"`
	EpisodeID  string `json:"episode_id"`
	Path       string `json:"path"`
	Temp       string `json:"temp"`
	Offset     int64  `json:"offset"`
	Total      int64  `json:"total"`
	Validator  string `json:"validator"`
	UpdatedAt  string `json:"updated_at"`
}

func nonce() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
func (s *service) pairHash() string {
	token, _ := s.o.Vault.Get(secrets.ServerToken)
	return podcast.FeedURLHash(s.o.ServerID() + "|" + token)
}
func (s *service) checkpointName(job jobs.Job) string {
	return podcast.FeedURLHash(strconv.FormatInt(*job.LibraryID, 10)+"|"+*job.Path) + ".json"
}
func (s *service) checkpointRoot() (*os.Root, error) {
	if s.o.DataDir == "" {
		return nil, errors.New("missing checkpoint directory")
	}
	dir := filepath.Join(s.o.DataDir, "podcast-fetch", s.pairHash())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}
func (s *service) saveCheckpoint(job jobs.Job, c *checkpoint, etag string) error {
	root, err := s.checkpointRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if c.Validator == "" {
		c.Validator = "podcast_etag_" + nonce()
	}
	if !netguard.StrongETag(etag) {
		return errors.New("unproven validator")
	}
	if err := s.o.Vault.Set(c.Validator, etag); err != nil {
		return err
	}
	c.UpdatedAt = s.o.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return fsutil.WriteRootAtomic(root, ".", s.checkpointName(job), b, 0o600, nil)
}
func (s *service) loadCheckpoint(job jobs.Job, root *os.Root, showID, episodeID string) (checkpoint, string) {
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: *job.LibraryID, ShowID: showID, EpisodeID: episodeID, Path: *job.Path}
	dir, err := s.checkpointRoot()
	if err != nil {
		return c, ""
	}
	defer func() { _ = dir.Close() }()
	rd, err := podcast.OpenRegular(dir, s.checkpointName(job))
	if err != nil {
		return c, ""
	}
	defer rd.Close()
	b, err := io.ReadAll(io.LimitReader(rd, 16385))
	if err != nil || len(b) > 16384 {
		return c, ""
	}
	var saved checkpoint
	if json.Unmarshal(b, &saved) != nil || saved.ServerHash != c.ServerHash || saved.LibraryID != c.LibraryID || saved.ShowID != showID || saved.EpisodeID != episodeID || saved.Path != c.Path || !ownedTemp(saved.Temp) || !strings.HasPrefix(saved.Validator, "podcast_etag_") {
		return c, ""
	}
	tag, _ := s.o.Vault.Get(saved.Validator)
	partial, err := podcast.OpenRegular(root, path.Dir(c.Path)+"/"+saved.Temp)
	if err == nil {
		defer partial.Close()
		st, e := partial.Stat()
		if e == nil && st.Size() == saved.Offset && saved.Offset > 0 && saved.Offset < saved.Total && saved.Total <= s.o.MaxEpisodeBytes && netguard.StrongETag(tag) {
			return saved, tag
		}
	}
	s.clearCheckpoint(job, root, &saved)
	return c, ""
}
func ownedTemp(s string) bool {
	return strings.HasPrefix(s, ".audiosilo-tmp-") && path.Base(s) == s && !strings.ContainsAny(s, "\\\x00")
}
func (s *service) clearCheckpoint(job jobs.Job, root *os.Root, c *checkpoint) {
	if c.ServerHash != s.pairHash() {
		return
	}
	if ownedTemp(c.Temp) {
		p := path.Dir(c.Path) + "/" + c.Temp
		if f, err := podcast.OpenRegular(root, p); err == nil {
			_ = f.Close()
			_ = root.Remove(p)
		}
	}
	if strings.HasPrefix(c.Validator, "podcast_etag_") {
		_ = s.o.Vault.Delete(c.Validator)
	}
	if dir, err := s.checkpointRoot(); err == nil {
		_ = dir.Remove(s.checkpointName(job))
		_ = dir.Close()
	}
}
func (s *service) fileLock(ctx context.Context, root *os.Root, episode string) (func(), error) {
	if err := podcast.EnsureDir(root, path.Dir(episode)+"/.audiosilo"); err != nil {
		return nil, err
	}
	p := podcast.AttachmentPath(episode, ".fetch.lock")
	if st, err := root.Lstat(p); err == nil && !st.Mode().IsRegular() {
		return nil, podcast.ErrInvalidFile
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := root.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		ok, err := tryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if ok {
			return func() { unlockFile(f); _ = f.Close() }, nil
		}
		if err := s.o.Wait(ctx, 100*time.Millisecond); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
}
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
