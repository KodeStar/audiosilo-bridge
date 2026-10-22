package podcastjob

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
)

func TestRetentionSharedVectorsAndStaleness(t *testing.T) {
	var vectors struct {
		Cases []struct {
			Name            string
			Mode            string
			Outside         bool `json:"outside_keep_window"`
			Followers       int
			Finished        bool    `json:"all_finished"`
			FinishedAt      *string `json:"all_finished_at"`
			FetchedAt       string  `json:"fetched_at"`
			Days            int
			Starred, Queued bool
			Active          bool   `json:"active_stream"`
			FetchOpen       bool   `json:"fetch_open"`
			Matches         bool   `json:"identity_matches"`
			CheckedAt       string `json:"checked_at"`
			Eligible        bool
		}
	}
	if err := json.Unmarshal(data(t, "retention-vectors.json"), &vectors); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Request  link.PodcastRetentionRequest `json:"retention_check_request"`
		Response link.PodcastRetentionCheck   `json:"retention_check_response"`
	}
	json.Unmarshal(data(t, "wire-2a.json"), &wire)
	for _, v := range vectors.Cases {
		t.Run(v.Name, func(t *testing.T) {
			c := wire.Response
			c.Policy = link.PodcastPolicy{Mode: v.Mode, Keep: 3, Days: v.Days}
			c.OutsideKeepWindow = v.Outside
			c.Followers = v.Followers
			c.AllFinished = v.Finished
			c.AllFinishedAt = v.FinishedAt
			c.Starred = v.Starred
			c.Queued = v.Queued
			c.ActiveStream = v.Active
			c.FetchOpen = v.FetchOpen
			c.CheckedAt = v.CheckedAt
			c.Eligible = true
			if !v.Matches {
				c.ShowID = "1111111111111111"
			}
			f := &podcast.Fetch{Generation: wire.Request.Generation, SHA256: wire.Request.SHA256, Bytes: wire.Request.Bytes, DurationMS: 4056, FetchedAt: v.FetchedAt}
			c.DeleteAfter = nil
			if v.FinishedAt != nil {
				finished, _ := time.Parse(time.RFC3339, *v.FinishedAt)
				fetched, _ := time.Parse(time.RFC3339, v.FetchedAt)
				if fetched.After(finished) {
					finished = fetched
				}
				after := finished.Add(time.Duration(v.Days) * 24 * time.Hour).Format(time.RFC3339)
				c.DeleteAfter = &after
			}
			now, _ := time.Parse(time.RFC3339, v.CheckedAt)
			if got := RetentionEligible(&c, 3, "Fixture Show", wire.Request, f, now, 0); got != v.Eligible {
				t.Fatalf("got %v want %v", got, v.Eligible)
			}
			if got := RetentionEligible(&c, 3, "Fixture Show", wire.Request, f, now, 5*time.Second); got != v.Eligible {
				t.Fatal("snapshot expired before its five-second limit")
			}
			for _, age := range []time.Duration{-time.Nanosecond, 5*time.Second + time.Nanosecond} {
				if RetentionEligible(&c, 3, "Fixture Show", wire.Request, f, now, age) {
					t.Fatal("invalid snapshot age authorized deletion", age)
				}
			}
		})
	}
}

type retainServer struct {
	*scanServer
	policy link.PodcastPolicy
	check  func(link.PodcastRetentionRequest) *link.PodcastRetentionCheck
	err    error
}

func (s *retainServer) PodcastRetention(context.Context, int64, string, string) (*link.PodcastPolicy, error) {
	return &s.policy, nil
}
func (s *retainServer) CheckPodcastRetention(_ context.Context, _ int64, r link.PodcastRetentionRequest) (*link.PodcastRetentionCheck, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.check(r), nil
}
func TestRetainDeletesOnlyVerifiedOwnedGeneration(t *testing.T) {
	requireProbe(t)
	for _, protection := range []string{"none", "clock-ahead", "clock-behind", "fresh-refresh", "starred", "queued", "stream", "fetch", "zero", "unfinished", "stale", "slow-final", "slow-refresh", "error", "archive", "identity", "changed-bytes"} {
		t.Run(protection, func(t *testing.T) {
			h, fetch := audioHarness(t)
			audio := data(t, "chap-v24-reverse.mp3")
			setAudio(h, func(w http.ResponseWriter, r *http.Request) { w.Write(audio) })
			if _, err := h.handler.FetchJob().Run(context.Background(), fetch, jobs.LocalReporter()); err != nil {
				t.Fatal(err)
			}
			keep := link.PodcastPolicy{Mode: "keep", Keep: 3, Days: 0}
			if protection == "archive" {
				keep.Mode = "archive"
			}
			server := &retainServer{scanServer: h.scan, policy: keep}
			now := h.handler.s.o.Now()
			clock := time.Now()
			h.handler.s.o.BudgetNow = func() time.Time { return clock }
			checks := 0
			if protection == "fresh-refresh" || protection == "slow-refresh" {
				h.handler.s.o.Fault = func(at string) error {
					if at == "journal" {
						clock = clock.Add(6 * time.Second)
					}
					return nil
				}
			}
			finished := now.Add(-time.Hour).Format(time.RFC3339)
			after := now.Format(time.RFC3339)
			server.check = func(r link.PodcastRetentionRequest) *link.PodcastRetentionCheck {
				checks++
				if protection == "stale" || protection == "slow-final" && checks == 2 || protection == "slow-refresh" && checks == 3 {
					clock = clock.Add(6 * time.Second)
				}
				c := &link.PodcastRetentionCheck{LibraryID: 3, ShowPath: "Fixture Show", ShowID: r.ShowID, Path: r.Path, Generation: r.Generation, SHA256: r.SHA256, Bytes: r.Bytes, CheckedAt: now.Format(time.RFC3339), Policy: keep, Followers: 2, AllFinished: true, AllFinishedAt: &finished, DeleteAfter: &after, OutsideKeepWindow: true, Eligible: true}
				switch protection {
				case "starred":
					c.Starred = true
				case "queued":
					c.Queued = true
				case "stream":
					c.ActiveStream = true
				case "fetch":
					c.FetchOpen = true
				case "zero":
					c.Followers = 0
				case "unfinished":
					c.AllFinished = false
				case "clock-ahead":
					c.CheckedAt = now.Add(time.Hour).Format(time.RFC3339)
				case "clock-behind":
					c.CheckedAt = now.Add(-time.Hour).Format(time.RFC3339)
				case "identity":
					c.ShowID = "1111111111111111"
				}
				return c
			}
			if protection == "error" {
				server.err = errors.New("private upstream error")
			}
			if protection == "changed-bytes" {
				os.WriteFile(filepath.Join(h.mount, *fetch.Path), []byte("foreign"), 0644)
			}
			h.handler.s.o.Server = func() Server { return server }
			show := "Fixture Show"
			params, _ := json.Marshal(map[string]string{"show_id": "a1b2c3d4e5f60718"})
			job := jobs.Job{ID: 82, Kind: RetainKind, LibraryID: fetch.LibraryID, Path: &show, Params: params, Attempt: 1}
			result, err := h.handler.RetainJob().Run(context.Background(), job, jobs.LocalReporter())
			if err != nil {
				t.Fatal(result, err)
			}
			got := result.(retainResult)
			_, statErr := os.Stat(filepath.Join(h.mount, *fetch.Path))
			f := h.read(show)
			if protection == "none" || protection == "clock-ahead" || protection == "clock-behind" || protection == "fresh-refresh" {
				if got.Deleted != 1 || !errors.Is(statErr, os.ErrNotExist) || f.Episodes[0].Fetch.DeletedAt == "" || f.Episodes[0].Fetch.Codec != "mp3" {
					t.Fatal(got, statErr, f.Episodes[0])
				}
			} else if got.Deleted != 0 || statErr != nil || f.Episodes[0].Fetch.DeletedAt != "" {
				t.Fatal("protected generation removed", got, statErr)
			}
			if len(f.Episodes) != 3 {
				t.Fatal("retention dropped feed entries", len(f.Episodes))
			}
			if protection == "slow-final" && checks != 2 || (protection == "fresh-refresh" || protection == "slow-refresh") && checks != 3 {
				t.Fatal("did not exercise the delayed deletion check", checks)
			}
			if _, err := os.Lstat(filepath.Join(h.mount, podcast.AttachmentPath(*fetch.Path, ".publish.json"))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("retention left its barrier", err)
			}
		})
	}
}

func TestRetainRecoveryAcrossFinalRefreshAndAudioMove(t *testing.T) {
	requireProbe(t)
	for _, boundary := range []string{"refresh", "audio"} {
		t.Run(boundary, func(t *testing.T) {
			h, fetch := audioHarness(t)
			audio := data(t, "chap-v24-reverse.mp3")
			setAudio(h, func(w http.ResponseWriter, _ *http.Request) { w.Write(audio) })
			if _, err := h.handler.FetchJob().Run(context.Background(), fetch, jobs.LocalReporter()); err != nil {
				t.Fatal(err)
			}
			before := *h.read("Fixture Show").Episodes[0].Fetch
			root, err := os.OpenRoot(h.mount)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			now := h.handler.s.o.Now()
			clock := time.Now()
			h.handler.s.o.BudgetNow = func() time.Time { return clock }
			keep := link.PodcastPolicy{Mode: "keep", Keep: 3, Days: 0}
			finished, after := now.Add(-time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)
			crash := errors.New("injected process stop")
			checks := 0
			server := &retainServer{scanServer: h.scan, policy: keep}
			server.check = func(r link.PodcastRetentionRequest) *link.PodcastRetentionCheck {
				checks++
				if boundary == "refresh" && checks == 3 {
					if _, err := root.Lstat(podcast.AttachmentPath(*fetch.Path, ".publish.json")); err != nil {
						t.Fatal("refresh did not follow the durable journal", err)
					}
					panic(crash) // Bypass normal error cleanup, as a process stop does.
				}
				return &link.PodcastRetentionCheck{LibraryID: 3, ShowPath: "Fixture Show", ShowID: r.ShowID, Path: r.Path, Generation: r.Generation, SHA256: r.SHA256, Bytes: r.Bytes, CheckedAt: now.Format(time.RFC3339), Policy: keep, Followers: 2, AllFinished: true, AllFinishedAt: &finished, DeleteAfter: &after, OutsideKeepWindow: true, Eligible: true}
			}
			h.handler.s.o.Server = func() Server { return server }
			h.handler.s.o.Fault = func(at string) error {
				if boundary == "refresh" && at == "journal" {
					clock = clock.Add(6 * time.Second)
				}
				if boundary == "audio" && at == "audio" {
					panic(crash)
				}
				return nil
			}
			show := "Fixture Show"
			params, _ := json.Marshal(map[string]string{"show_id": "a1b2c3d4e5f60718"})
			job := jobs.Job{ID: 82, Kind: RetainKind, LibraryID: fetch.LibraryID, Path: &show, Params: params, Attempt: 1}
			func() {
				defer func() {
					if got := recover(); got != crash {
						t.Fatalf("expected process stop, got %v", got)
					}
				}()
				_, _ = h.handler.RetainJob().Run(context.Background(), job, jobs.LocalReporter())
			}()
			journalPath := podcast.AttachmentPath(*fetch.Path, ".publish.json")
			b, err := root.ReadFile(journalPath)
			if err != nil {
				t.Fatal("process stop left no recovery journal", err)
			}
			var j podcast.Journal
			if err := json.Unmarshal(b, &j); err != nil {
				t.Fatal(err)
			}
			moved := boundary == "audio"
			if podcast.Matches(root, *fetch.Path, &before) == moved || podcast.Matches(root, show+"/"+j.TrashDir+"/audio", &before) != moved {
				t.Fatal("unexpected audio location at process stop")
			}
			if err := podcast.RecoverShow(root, show, now.Add(time.Hour), nil); err != nil {
				t.Fatal(err)
			}
			if _, err := root.Lstat(journalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery left its barrier", err)
			}
			want := before
			if moved {
				want.DeletedAt = j.Fetch.DeletedAt
				if _, err := root.Lstat(*fetch.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("recovery did not complete the moved deletion", err)
				}
				if err := podcast.CleanupTrash(root, j); err != nil {
					t.Fatal(err)
				}
			} else if !podcast.Matches(root, *fetch.Path, &before) || podcast.ReadChapters(root, *fetch.Path, &before) == nil {
				t.Fatal("recovery changed an installed episode during refresh")
			}
			if got := h.read(show).Episodes[0].Fetch; !reflect.DeepEqual(got, &want) {
				t.Fatal("recovery changed fetch metadata", got, want)
			}
		})
	}
}

// A deletion refused before the audio moves (Windows refusing to move open
// audio) is skipped and leaves no barrier: recovery must never finish it later
// without a fresh aggregate.
func TestRetainRenameRefusalLeavesNoJournal(t *testing.T) {
	requireProbe(t)
	h, fetch := audioHarness(t)
	audio := data(t, "chap-v24-reverse.mp3")
	setAudio(h, func(w http.ResponseWriter, r *http.Request) { w.Write(audio) })
	if _, err := h.handler.FetchJob().Run(context.Background(), fetch, jobs.LocalReporter()); err != nil {
		t.Fatal(err)
	}
	keep := link.PodcastPolicy{Mode: "keep", Keep: 3, Days: 0}
	now := h.handler.s.o.Now()
	finished, after := now.Add(-time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)
	h.handler.s.o.Server = func() Server {
		return &retainServer{scanServer: h.scan, policy: keep, check: func(r link.PodcastRetentionRequest) *link.PodcastRetentionCheck {
			return &link.PodcastRetentionCheck{LibraryID: 3, ShowPath: "Fixture Show", ShowID: r.ShowID, Path: r.Path, Generation: r.Generation, SHA256: r.SHA256, Bytes: r.Bytes, CheckedAt: now.Format(time.RFC3339), Policy: keep, Followers: 2, AllFinished: true, AllFinishedAt: &finished, DeleteAfter: &after, OutsideKeepWindow: true, Eligible: true}
		}}
	}
	h.handler.s.o.Fault = func(at string) error {
		if at == "deleting" {
			return &os.PathError{Op: "rename", Path: *fetch.Path, Err: os.ErrPermission}
		}
		return nil
	}
	show := "Fixture Show"
	params, _ := json.Marshal(map[string]string{"show_id": "a1b2c3d4e5f60718"})
	job := jobs.Job{ID: 82, Kind: RetainKind, LibraryID: fetch.LibraryID, Path: &show, Params: params, Attempt: 1}
	result, err := h.handler.RetainJob().Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(result, err)
	}
	if got := result.(retainResult); got.Deleted != 0 || got.Skipped != 1 {
		t.Fatal(got)
	}
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	if _, err := root.Lstat(podcast.AttachmentPath(*fetch.Path, ".publish.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused deletion left its barrier", err)
	}
	h.handler.s.o.Fault = nil
	if err := podcast.RecoverShow(root, show, now, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.mount, *fetch.Path)); err != nil || h.read(show).Episodes[0].Fetch.DeletedAt != "" {
		t.Fatal("recovery deleted an unauthorized episode", err)
	}
}

func TestLeaseFailureKeepsIdentityForLostClaimsOnly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		err  error
		code string
	}{
		{link.ErrNotHeld, "identity_changed"},
		{link.ErrUnauthorized, "identity_changed"},
		{context.Canceled, "identity_changed"},
		{errors.New("connection refused"), "network"},
	} {
		var failure *netguard.Error
		if !errors.As(leaseFailure(ctx, tc.err, "lost"), &failure) || failure.Code != tc.code {
			t.Fatal(tc.err, failure)
		}
	}
}
