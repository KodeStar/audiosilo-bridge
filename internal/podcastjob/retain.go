package podcastjob

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
)

type RetentionServer interface {
	PodcastRetention(context.Context, int64, string, string) (*link.PodcastPolicy, error)
	CheckPodcastRetention(context.Context, int64, link.PodcastRetentionRequest) (*link.PodcastRetentionCheck, error)
}
type Retain struct{ s *service }

func (h *Subscribe) RetainJob() *Retain { return &Retain{s: h.s} }
func (h *Retain) Kind() string          { return RetainKind }
func (h *Retain) Lane() string          { return jobs.LaneLight }

type retainResult struct {
	LibraryID int64  `json:"library_id"`
	ShowPath  string `json:"show_path"`
	ShowID    string `json:"show_id"`
	Deleted   int    `json:"deleted"`
	Skipped   int    `json:"skipped"`
}

func (h *Retain) Run(ctx context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	return outcome(h.run(ctx, job, rep))
}
func (h *Retain) run(parent context.Context, job jobs.Job, rep *jobs.Reporter) (any, error) {
	s := h.s
	ctx, cancel := context.WithTimeout(parent, ceiling)
	defer cancel()
	var p struct {
		ShowID string `json:"show_id"`
	}
	if job.Path == nil || !podcast.ValidShow(*job.Path) || json.Unmarshal(job.Params, &p) != nil || !podcast.ValidID(p.ShowID) {
		return nil, fail("identity_changed", "The podcast show identity is invalid.")
	}
	lib, mount, err := s.library(ctx, job, rep)
	if err != nil {
		return nil, err
	}
	show := *job.Path
	result := retainResult{LibraryID: lib.ID, ShowPath: show, ShowID: p.ShowID}
	rep.Progress(jobs.Progress{Step: "retaining", Message: "Checking retained episodes"})
	srv, ok := s.o.Server().(RetentionServer)
	if !ok {
		return nil, fail("network", "The retention policy could not be verified.")
	}
	policy, err := srv.PodcastRetention(ctx, lib.ID, show, p.ShowID)
	var apiErr *link.APIError
	if err != nil && !errors.As(err, &apiErr) {
		// The server could not be reached: a retryable failure, not a changed show.
		return nil, fail("network", "The retention policy could not be verified.")
	}
	if err != nil || policy == nil || !policy.Valid() {
		return nil, fail("identity_changed", "The retention policy could not be verified.")
	}
	root, err := os.OpenRoot(mount)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	unlock, err := s.acquire(ctx, s.showKey(mount, show))
	if err != nil {
		return nil, err
	}
	var feed *podcast.Feed
	err = func() error {
		defer unlock()
		if err := s.beforeWrite(lib, mount, show); err != nil {
			return err
		}
		if err := podcast.RecoverShow(root, show, s.o.Now(), s.o.Fault); err != nil {
			return err
		}
		var err error
		feed, err = podcast.Read(root, show)
		return err
	}()
	if err != nil {
		return nil, err
	}
	if feed.ShowID != p.ShowID {
		return nil, fail("identity_changed", "The podcast show identity changed.")
	}
	s.cleanOwned(ctx, root, lib, mount, show, job.ID)
	pairing := s.pairHash()
	// ask stamps dispatch on the Bridge's monotonic clock: a snapshot's age runs from there.
	ask := func(r link.PodcastRetentionRequest) (*link.PodcastRetentionCheck, time.Time, error) {
		sent := s.o.BudgetNow()
		check, err := srv.CheckPodcastRetention(ctx, lib.ID, r)
		return check, sent, err
	}
	for _, candidate := range feed.Episodes {
		if candidate.Fetch == nil || candidate.Fetch.DeletedAt != "" {
			continue
		}
		if !candidate.Fetch.Valid() || policy.Mode == "archive" {
			result.Skipped++
			continue
		}
		// Most kept episodes are ineligible: ask before the locked reread and full
		// hash. This grants nothing; the locked path asks again before deleting.
		pre := link.PodcastRetentionRequest{ShowID: p.ShowID, Path: candidate.Path, Generation: candidate.Fetch.Generation, SHA256: candidate.Fetch.SHA256, Bytes: candidate.Fetch.Bytes}
		if check, sent, err := ask(pre); err != nil || !RetentionEligible(check, lib.ID, show, pre, candidate.Fetch, s.o.Now(), s.o.BudgetNow().Sub(sent)) {
			result.Skipped++
			continue
		}
		busy, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		release, err := s.fileLock(busy, root, candidate.Path)
		cancel()
		if err != nil {
			result.Skipped++
			continue
		}
		var transaction podcast.Journal
		deleted, err := func() (bool, error) {
			defer release()
			unlock, err := s.acquire(ctx, s.showKey(mount, show))
			if err != nil {
				return false, err
			}
			defer unlock()
			if err := s.beforeWrite(lib, mount, show); err != nil {
				return false, err
			}
			if pairing != s.pairHash() {
				return false, fail("identity_changed", "The Bridge pairing changed.")
			}
			if err := podcast.RecoverShow(root, show, s.o.Now(), s.o.Fault); err != nil {
				return false, err
			}
			_, current, err := s.readEpisode(root, candidate.Path, fetchParams{p.ShowID, candidate.ID})
			if err != nil {
				return false, err
			}
			if !sameFetch(current.Fetch, candidate.Fetch) || current.Fetch.DeletedAt != "" || !podcast.Matches(root, current.Path, current.Fetch) {
				return false, nil
			}
			if err := rep.ConfirmLease(ctx); err != nil {
				return false, leaseFailure(ctx, err, "The retention job no longer owns its claim.")
			}
			request := link.PodcastRetentionRequest{ShowID: p.ShowID, Path: current.Path, Generation: current.Fetch.Generation, SHA256: current.Fetch.SHA256, Bytes: current.Fetch.Bytes}
			check, sent, err := ask(request)
			eligible := func() bool {
				return RetentionEligible(check, lib.ID, show, request, current.Fetch, s.o.Now(), s.o.BudgetNow().Sub(sent))
			}
			if err != nil || !eligible() {
				return false, nil
			}
			if err := s.beforeWrite(lib, mount, show); err != nil {
				return false, err
			}
			// Check the elapsed request age again after the last mapping probe.
			if !eligible() {
				return false, nil
			}
			tombstone := *current.Fetch
			tombstone.DeletedAt = s.o.Now().UTC().Format(time.RFC3339)
			transaction = podcast.Journal{Version: 1, Op: "delete", ShowID: p.ShowID, EpisodeID: current.ID, Path: current.Path, TrashDir: ".audiosilo-trash/podcasts/" + current.ID + "-g" + strconv.Itoa(tombstone.Generation) + "-" + nonce(), Fetch: tombstone}
			j := transaction
			denied := errors.New("retention snapshot denied deletion")
			boundary := func(at string) error {
				if at == "deleting" {
					if !eligible() {
						var err error
						if check, sent, err = ask(request); err != nil {
							return denied
						}
					}
					if !eligible() {
						return denied
					}
				}
				if s.o.Fault != nil {
					return s.o.Fault(at)
				}
				return nil
			}
			if err := podcast.Publish(root, j, s.o.Now(), boundary); err != nil {
				if errors.Is(err, denied) {
					if err := podcast.AbortDelete(root, j); err != nil {
						return false, err
					}
					return false, nil
				}
				// A failure before the audio moved (Windows refusing to move open audio,
				// a trash folder that can't be made) should remove the barrier now.
				// Recovery also aborts unmoved deletions. AbortDelete refuses once the
				// payload moved, and then recovery owns the deletion. A refused rename
				// (open audio) counts as skipped.
				var refused *os.LinkError
				if podcast.AbortDelete(root, j) == nil && (errors.Is(err, os.ErrPermission) || errors.As(err, &refused)) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		}()
		if err != nil {
			return nil, err
		}
		if !deleted {
			result.Skipped++
			continue
		}
		result.Deleted++
		s.setIndexed(lib.ID, show, false)
		if err := s.episodeRescan(ctx, lib.ID, candidate.Path); err != nil {
			return nil, err
		}
		if err := podcast.CleanupTrash(root, transaction); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// RetentionEligible independently checks the server snapshot. Missing, stale or
// inconsistent aggregate fields cannot grant deletion authority. Age is measured
// from request dispatch on the Bridge's monotonic clock, independently of checked_at.
func RetentionEligible(c *link.PodcastRetentionCheck, library int64, show string, r link.PodcastRetentionRequest, f *podcast.Fetch, now time.Time, age time.Duration) bool {
	if c == nil || !f.Valid() || f.DeletedAt != "" || !c.Eligible || !c.Policy.Valid() || c.Policy.Mode == "archive" || c.Policy.Mode == "keep" && !c.OutsideKeepWindow || c.LibraryID != library || c.ShowPath != show || c.ShowID != r.ShowID || c.Path != r.Path || c.Generation != r.Generation || c.SHA256 != r.SHA256 || c.Bytes != r.Bytes || c.Followers <= 0 || !c.AllFinished || c.AllFinishedAt == nil || c.DeleteAfter == nil || c.Starred || c.Queued || c.ActiveStream || c.FetchOpen {
		return false
	}
	if age < 0 || age > 5*time.Second {
		return false
	}
	finished, err := time.Parse(time.RFC3339, *c.AllFinishedAt)
	if err != nil {
		return false
	}
	fetched, err := time.Parse(time.RFC3339, f.FetchedAt)
	if err != nil {
		return false
	}
	after, err := time.Parse(time.RFC3339, *c.DeleteAfter)
	if err != nil {
		return false
	}
	if fetched.After(finished) {
		finished = fetched
	}
	expected := finished.Add(time.Duration(c.Policy.Days) * 24 * time.Hour)
	return after.Equal(expected) && !now.Before(after) && path.Dir(r.Path) == show
}
