package podcastjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// repairedScan is the harness's server paired again: pairing p2.
type repairedScan struct{ *scanServer }

func (repairedScan) PairingID() string { return "p2" }

// otherServerScan is a pairing (p2) with another server.
type otherServerScan struct{ *scanServer }

func (otherServerScan) ServerID() string  { return "server-2" }
func (otherServerScan) PairingID() string { return "p2" }

// How a pairing ends under a fetch: the token goes or is replaced, and the
// link no longer reaches the claim's pairing.
const (
	pairedP1 int32 = iota
	unpaired
	repairedSame
	repairedOther
)

// pairings switches the harness's link and server token between the ways a
// pairing ends (pairedP1 to start with, token "t1").
func pairings(t *testing.T, h *harness) func(int32) {
	t.Helper()
	var state atomic.Int32
	h.handler.s.o.Server = func() Server {
		switch state.Load() {
		case pairedP1:
			return pairedScan{h.scan}
		case repairedSame:
			return repairedScan{h.scan}
		case repairedOther:
			return otherServerScan{h.scan}
		}
		return nil
	}
	if err := h.vault.Set(secrets.ServerToken, "t1"); err != nil {
		t.Fatal(err)
	}
	return func(to int32) {
		t.Helper()
		var err error
		if to == unpaired {
			err = h.vault.Delete(secrets.ServerToken)
		} else {
			err = h.vault.Set(secrets.ServerToken, "t2")
		}
		if err != nil {
			t.Fatal(err)
		}
		state.Store(to)
	}
}

// claimFetch claims job under pairing p1 as the runner does: its params
// sealed (SealParams records the pairing hash) and its row recorded.
func claimFetch(t *testing.T, h *harness, job jobs.Job) jobs.Job {
	t.Helper()
	job = jobs.ClaimedUnder(job, "server-1", "p1")
	sealed, err := h.handler.FetchJob().SealParams(job)
	if err != nil {
		t.Fatal(err)
	}
	job.Params = sealed
	if err := h.db.RecordClaim(context.Background(), store.ServerJob{ID: job.ID, Kind: FetchKind, LibraryID: job.LibraryID, Path: job.Path,
		Params: job.Params, Attempt: job.Attempt, ServerID: "server-1", Pairing: "p1"}); err != nil {
		t.Fatal(err)
	}
	return job
}

// leftovers is what the fetch's pairings left in the Bridge's data folder:
// download records (any pairing's) and sealed ETags.
func leftovers(t *testing.T, h *harness) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(h.data, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(h.data, p)
		if strings.HasPrefix(rel, "podcast-fetch"+string(filepath.Separator)) || strings.HasPrefix(d.Name(), secrets.PodcastETagPrefix) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The claim records the pairing hash it was made under, from the token of
// the pairing its link reaches; a value the server put in the params is never
// kept, a claim whose link no longer reaches its pairing records none, and
// the page never sees it (RedactParams).
func TestFetchClaimRecordsItsPairing(t *testing.T) {
	h, job := audioHarness(t)
	end := pairings(t, h)
	var forged map[string]any
	_ = json.Unmarshal(job.Params, &forged)
	forged[podcast.ClaimHashParam] = "forged"
	job.Params, _ = json.Marshal(forged)
	job = jobs.ClaimedUnder(job, "server-1", "p1")
	sealed, err := h.handler.FetchJob().SealParams(job)
	if err != nil {
		t.Fatal(err)
	}
	want := podcast.FeedURLHash("server-1|t1")
	if c, ok := claimOf(jobs.Job{Kind: FetchKind, Params: sealed}); !ok || c.ServerHash != want {
		t.Fatalf("the claim's pairing was not recorded: %s", sealed)
	}
	if v := h.handler.s.forJob(jobs.Job{Kind: FetchKind, Params: sealed}); v.pairHash() != want {
		t.Fatal("a job's records are not kept under its claim's pairing", v.pairHash())
	}
	if red := podcast.RedactParams(sealed); bytes.Contains(red, []byte(podcast.ClaimHashParam)) || !bytes.Contains(red, []byte("show_id")) {
		t.Fatalf("the page sees the claim's pairing hash: %s", red)
	}
	end(unpaired)
	sealed, err = h.handler.FetchJob().SealParams(job)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := claimOf(jobs.Job{Kind: FetchKind, Params: sealed}); ok || bytes.Contains(sealed, []byte("forged")) {
		t.Fatalf("a claim whose pairing ended recorded one: %s", sealed)
	}
}

// A fetch dropped mid-download, by an unpair or a pairing made again (with
// its server or another), leaves nothing: its partial, its download records
// (under any pairing's hash: none saved after the drop) and its sealed ETags
// go, though the library's mapping is no longer the Bridge's to read. Before,
// the exit and the release both gave up on the ended pairing, and the
// interrupted download saved a record under the pairing now. A partial with
// no record yet (one write in: a record is saved before the next) goes at
// the run's exit, as nothing names it for the release.
func TestAFetchDroppedMidDownloadLeavesNothing(t *testing.T) {
	for _, c := range []struct {
		name   string
		to     int32
		writes int
	}{
		{"unpaired", unpaired, 2}, {"paired again", repairedSame, 2}, {"paired with another server", repairedOther, 2},
		{"unpaired before its first record", unpaired, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, job := audioHarness(t)
			end := pairings(t, h)
			job = claimFetch(t, h, job)
			started := make(chan struct{})
			setAudio(h, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"one"`)
				w.Header().Set("Content-Length", "10000")
				w.WriteHeader(200)
				for range c.writes {
					w.Write(bytes.Repeat([]byte("a"), 1000/c.writes))
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			})
			ctx, drop := jobs.DropContext(context.Background())
			defer drop()
			done := make(chan error, 1)
			go func() { _, err := h.handler.FetchJob().Run(ctx, job, jobs.LocalReporter()); done <- err }()
			<-started
			show := filepath.Join(h.mount, "Fixture Show")
			if !partialAt(show, 1000) {
				t.Fatal("the download did not start")
			}
			end(c.to)
			drop()
			if err := <-done; err == nil {
				t.Fatal("the dropped fetch succeeded")
			}
			if left := partials(show); len(left) != 0 {
				t.Fatal("a dropped fetch left its partial:", left)
			}
			if left := leftovers(t, h); len(left) != 0 {
				t.Fatal("a dropped fetch left records or ETags:", left)
			}
		})
	}
}

// A fetch dropped while it was not running (resumed after a restart and
// waiting, then the pairing ended) is released by the runner's Abandon: its
// claim's records, partial and ETag go, found by the claim's pairing and the
// record's mount, and the ended pairing's records folder with them. What the
// pairing now keeps of the same episode (a later claim's download) stays.
func TestAbandonReleasesAFetchOfAnEndedPairing(t *testing.T) {
	for _, c := range []struct {
		name string
		to   int32
	}{{"unpaired", unpaired}, {"paired again", repairedSame}, {"paired with another server", repairedOther}} {
		t.Run(c.name, func(t *testing.T) {
			h, job := audioHarness(t)
			end := pairings(t, h)
			job = claimFetch(t, h, job)
			root, err := os.OpenRoot(h.mount)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			s := h.handler.s
			old := abandonRecord(t, s.forJob(job), root, job)
			end(c.to)
			var mine checkpoint
			now := jobs.ClaimedUnder(job, "server-1", "p2")
			if c.to == repairedSame {
				now.Params, _ = json.Marshal(fetchParams{fixtureShowID, old.EpisodeID})
				now = claimFetchUnder(t, h, now)
				mine = abandonRecord(t, s.forJob(now), root, now)
			}
			h.handler.FetchJob().Abandon(jobs.AbandonContext(context.Background(), false), job)
			if _, err := root.Lstat(old.partialName()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the ended pairing's partial was left", err)
			}
			if v, _ := h.vault.Get(old.Validator); v != "" {
				t.Fatal("the ended pairing's ETag was left")
			}
			if _, err := os.Stat(filepath.Join(h.data, "podcast-fetch", old.ServerHash)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the ended pairing's records folder was left", err)
			}
			if c.to != repairedSame {
				return
			}
			v := s.forJob(now)
			if saved, ok := v.readCheckpoint(now, mine.ShowID); !ok || saved.Temp != mine.Temp {
				t.Fatal("the pairing now's record of the episode was cleared")
			}
			if _, err := root.Lstat(mine.partialName()); err != nil {
				t.Fatal("the pairing now's partial was removed", err)
			}
			if val, _ := h.vault.Get(mine.Validator); val == "" {
				t.Fatal("the pairing now's ETag was removed")
			}
		})
	}
}

// claimFetchUnder seals job's params under the pairing its link reaches now,
// without recording a row (the pairing's own later claim).
func claimFetchUnder(t *testing.T, h *harness, job jobs.Job) jobs.Job {
	t.Helper()
	sealed, err := h.handler.FetchJob().SealParams(job)
	if err != nil {
		t.Fatal(err)
	}
	job.Params = sealed
	return job
}

// Nothing of an ended pairing's fetch goes while the Bridge cannot tell that
// the pairing ended (its token cannot be read: a live job's resumable
// download is never released for a vault's fault), nor while its library's
// marker is gone from the mount its record names (a share unmounted: the
// path there is not the library).
func TestAbandonOfAnEndedPairingNeedsItToBeKnownAndItsLibraryThere(t *testing.T) {
	for _, c := range []string{"token unreadable", "marker gone"} {
		t.Run(c, func(t *testing.T) {
			h, job := audioHarness(t)
			end := pairings(t, h)
			job = claimFetch(t, h, job)
			root, err := os.OpenRoot(h.mount)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			s := h.handler.s
			rec := abandonRecord(t, s.forJob(job), root, job)
			end(unpaired)
			switch c {
			case "token unreadable":
				s.o.Vault = lockedVault{s.o.Vault}
			default:
				if err := root.Remove(mapping.MarkerPath); err != nil {
					t.Fatal(err)
				}
			}
			h.handler.FetchJob().Abandon(jobs.AbandonContext(context.Background(), false), job)
			if _, err := root.Lstat(rec.partialName()); err != nil {
				t.Fatal("the partial was removed", err)
			}
			if v, _ := h.vault.Get(rec.Validator); v == "" {
				t.Fatal("the ETag was removed")
			}
		})
	}
}

// A download record is never saved once the claim's pairing has ended: not
// under that pairing (no job resumes under it), nor under the pairing now
// (another server's, or the same server's paired again).
func TestNoRecordIsSavedForAnEndedPairing(t *testing.T) {
	h, job := audioHarness(t)
	end := pairings(t, h)
	job = claimFetch(t, h, job)
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	v := h.handler.s.forJob(job)
	c := checkpoint{ServerHash: v.pairHash(), LibraryID: *job.LibraryID, ShowID: fixtureShowID, EpisodeID: "e", Path: *job.Path, Temp: podcast.TempPrefix + nonce(), Offset: 6, Total: 100}
	if err := root.WriteFile(c.partialName(), []byte("prefix"), 0o600); err != nil {
		t.Fatal(err)
	}
	end(unpaired)
	if err := v.saveCheckpoint(job, root, &c, `"one"`); code(t, err) != "identity_changed" {
		t.Fatal("a record was saved for an ended pairing:", err)
	}
	if left := leftovers(t, h); len(left) != 0 {
		t.Fatal("an ended pairing's save left:", left)
	}
}
