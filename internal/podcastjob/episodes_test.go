package podcastjob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
)

func data(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../podcast/testdata/phase2c2/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func audioHarness(t *testing.T) (*harness, jobs.Job) {
	t.Helper()
	h := newHarness(t)
	s := h.handler.s
	s.o.DataDir = h.data
	s.o.Wait = func(context.Context, time.Duration) error { return nil }
	s.o.Now = func() time.Time { return time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC) }
	var f podcast.Feed
	if err := json.Unmarshal(data(t, "feed-fetch-v1.json"), &f); err != nil {
		t.Fatal(err)
	}
	f.Episodes[0].Fetch = nil
	f.Episodes[0].Enclosure.URL = h.http.URL + "/audio"
	os.Mkdir(filepath.Join(h.mount, "Fixture Show"), 0755)
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := podcast.Write(root, "Fixture Show", &f, s.o.Now()); err != nil {
		t.Fatal(err)
	}
	lib := int64(3)
	p := f.Episodes[0].Path
	params, _ := json.Marshal(fetchParams{f.ShowID, f.Episodes[0].ID})
	return h, jobs.Job{ID: 81, Kind: FetchKind, LibraryID: &lib, Path: &p, Params: params, Attempt: 1}
}
func setAudio(h *harness, fn http.HandlerFunc) { h.audio.Store(&fn) }
func requireProbe(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Fatal("ffprobe is required by the episode gate")
	}
}
func TestFetchProbePublishPinAndIndexRepair(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	audio := data(t, "chap-v24-reverse.mp3")
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Authorization") != "" {
			t.Error("audio request", r.Header)
		}
		w.Header().Set("ETag", `"one"`)
		w.Write(audio)
	})
	h.scan.fail.Store(true)
	result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	var failure *netguard.Error
	if !errors.As(err, &failure) || failure.Code != "index_failed" {
		t.Fatal(result, err)
	}
	h.scan.fail.Store(false)
	result, err = h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(result, err)
	}
	got := result.(fetchResult)
	if !got.Reused || got.Fetch.DurationMS != 4056 || got.Fetch.Bytes != 16493 || got.Fetch.Codec != "mp3" || got.Chapters != 3 || calls.Load() != 1 {
		t.Fatal(got, calls.Load())
	}
	f := h.read("Fixture Show")
	if f.Episodes[0].Title != "Current generation" || f.Episodes[0].DurationSeconds == nil || *f.Episodes[0].DurationSeconds != 999 {
		t.Fatal("publisher/probe metadata were conflated", f.Episodes[0])
	}
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	if !podcast.Matches(root, *job.Path, &got.Fetch) {
		t.Fatal("bad committed pin")
	}
	if st, _ := root.Stat(*job.Path); st.Mode().Perm() != 0644 {
		t.Fatal(st.Mode())
	}
	if _, err := root.Lstat(podcast.AttachmentPath(*job.Path, ".publish.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// The server's 429 to an episode's rescan is waited out (its Retry-After, or
// a doubling backoff when it sends none) and asked again; past the bounds (a
// wait over rescanMaxWait, rescanTries asks) the fetch succeeds with the
// episode saved and the show left for the next poll's rescan, never failing
// index_failed (which would fetch the episode again).
func TestEpisodeRescanWaitsOutTheServersLimit(t *testing.T) {
	second, hour := time.Second, time.Hour
	for _, tc := range []struct {
		name    string
		limited int32
		limit   *time.Duration
		asks    int32
		waits   []time.Duration
		indexed bool
	}{
		{"retry-after", 1, &second, 2, []time.Duration{time.Second}, true},
		{"no retry-after", 2, nil, 3, []time.Duration{rescanBackoff, 2 * rescanBackoff}, true},
		{"too long a wait", 1, &hour, 1, nil, false},
		{"still limited", 100, &second, rescanTries, []time.Duration{time.Second, time.Second}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, job := audioHarness(t)
			s := h.handler.s
			var waits []time.Duration
			s.o.Wait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
			h.scan.limited.Store(tc.limited)
			h.scan.limit = tc.limit
			// One left for the poll is marked not indexed, whatever it was.
			s.setIndexed(3, "Fixture Show", !tc.indexed)
			if err := s.episodeRescan(context.Background(), 3, *job.Path); err != nil {
				t.Fatal("a limited rescan failed the job", err)
			}
			if got := h.scan.calls.Load(); got != tc.asks || !slices.Equal(waits, tc.waits) {
				t.Fatal("asks", got, "waits", waits)
			}
			if s.isIndexed(3, "Fixture Show") != tc.indexed {
				t.Fatal("indexed", !tc.indexed)
			}
		})
	}
}
func TestFetchInterruptedStrongResumeAndFreshFallback(t *testing.T) {
	requireProbe(t)
	for _, scenario := range []string{"strong", "weak", "no-tag", "200", "bad206", "416", "encoded206"} {
		t.Run(scenario, func(t *testing.T) {
			h, job := audioHarness(t)
			audio := data(t, "chap-v23.mp3")
			var calls atomic.Int32
			setAudio(h, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				tag := `"one"`
				if scenario == "weak" {
					tag = `W/"one"`
				}
				if scenario != "no-tag" {
					w.Header().Set("ETag", tag)
				}
				if n == 1 {
					w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
					w.WriteHeader(200)
					w.Write(audio[:1000])
					return
				}
				if n == 2 && r.Header.Get("Range") != "" {
					if r.Header.Get("Range") != "bytes=1000-" || r.Header.Get("If-Range") != `"one"` {
						t.Error("unproven range", r.Header)
					}
					switch scenario {
					case "416":
						w.WriteHeader(416)
						return
					case "200":
						w.Write(audio)
						return
					case "bad206":
						w.Header().Set("Content-Range", fmt.Sprintf("bytes 999-%d/%d", len(audio)-1, len(audio)))
					case "encoded206":
						w.Header().Set("Content-Encoding", "gzip")
						fallthrough
					default:
						w.Header().Set("Content-Range", fmt.Sprintf("bytes 1000-%d/%d", len(audio)-1, len(audio)))
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(audio)-1000))
					w.WriteHeader(206)
					w.Write(audio[1000:])
					return
				}
				if (scenario == "weak" || scenario == "no-tag") && r.Header.Get("Range") != "" {
					t.Error("range sent without strong tag")
				}
				w.Write(audio)
			})
			result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if err != nil {
				t.Fatal(result, err)
			}
			expected := int32(2)
			if scenario == "bad206" || scenario == "416" || scenario == "encoded206" {
				expected = 3
			}
			if calls.Load() != expected {
				t.Fatal("attempt budget", calls.Load())
			}
			var vectors struct {
				Assets []struct{ Fetch podcast.Fetch }
			}
			json.Unmarshal(data(t, "generation-vectors.json"), &vectors)
			got := result.(fetchResult)
			if got.Fetch.SHA256 != vectors.Assets[0].Fetch.SHA256 || got.Fetch.Bytes != vectors.Assets[0].Fetch.Bytes {
				t.Fatal("200 appended or wrong hash", got)
			}
		})
	}
}
func TestFetchDenialsNeverInstall(t *testing.T) {
	for _, scenario := range []string{"cap", "unknown-cap", "encoded", "invalid", "probe", "identity", "mapping", "foreign", "record", "attempts", "retry-after"} {
		t.Run(scenario, func(t *testing.T) {
			h, job := audioHarness(t)
			var calls atomic.Int32
			audio := data(t, "plain.mp3")
			h.handler.s.o.MaxEpisodeBytes = 20000
			setAudio(h, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch scenario {
				case "cap":
					w.Header().Set("Content-Length", "20001")
					w.WriteHeader(200)
				case "unknown-cap":
					w.(http.Flusher).Flush()
					w.Write(bytes.Repeat([]byte("x"), 20001))
				case "encoded":
					w.Header().Set("Content-Encoding", "gzip")
					w.Write(audio)
				case "invalid":
					w.Write([]byte("not audio https://PRIVATE_CANARY"))
				case "attempts":
					w.WriteHeader(503)
				case "retry-after":
					w.Header().Set("Retry-After", "20")
					w.WriteHeader(429)
				default:
					w.Write(audio)
				}
			})
			switch scenario {
			case "probe":
				h.handler.s.o.Probe = LocalProbe(filepath.Join(t.TempDir(), "missing-ffprobe"))
			case "identity":
				job.Params = json.RawMessage(`{"show_id":"1111111111111111","episode_id":"1cc306d831645e2b"}`)
			case "mapping":
				h.libs.SetLibraries([]link.Library{{ID: 3, Kind: "books", Root: h.mount}})
			case "foreign":
				os.WriteFile(filepath.Join(h.mount, *job.Path), []byte("foreign"), 0644)
			case "record":
				// An invalid saved Fetch can never take a new generation: no GET.
				f := h.read("Fixture Show")
				f.Episodes[0].Fetch = &podcast.Fetch{Generation: 0, SHA256: "bad"}
				root, _ := os.OpenRoot(h.mount)
				podcast.Write(root, "Fixture Show", f, h.handler.s.o.Now())
				root.Close()
			}
			result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if err == nil {
				t.Fatal("unsafe install", result)
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "PRIVATE_CANARY") || strings.Contains(string(raw), h.http.URL) {
				t.Fatal("private error leak", string(raw))
			}
			if scenario == "identity" || scenario == "mapping" || scenario == "foreign" || scenario == "record" {
				if calls.Load() != 0 {
					t.Fatal("denial made GET")
				}
			}
			if scenario == "attempts" && calls.Load() != 3 {
				t.Fatal("retry count", calls.Load())
			}
			if scenario == "retry-after" && calls.Load() != 1 {
				t.Fatal("ignored long Retry-After")
			}
			if scenario != "foreign" {
				if _, err := os.Stat(filepath.Join(h.mount, *job.Path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed audio installed", err)
				}
			}
			matches, _ := filepath.Glob(filepath.Join(h.mount, "Fixture Show", ".audiosilo-tmp-*"))
			if len(matches) != 0 {
				t.Fatal("terminal partials", matches)
			}
		})
	}
}

func TestPrivateEnclosureIdentityAndNoPlaintextCheckpoint(t *testing.T) {
	h, job := audioHarness(t)
	f := h.read("Fixture Show")
	f.Private = true
	f.Episodes[0].GUID = "sha256:" + podcast.FeedURLHash("https://private.example/guid/TOKEN_CANARY")
	f.Episodes[0].Enclosure.URL = ""
	s := h.handler.s
	s.o.Vault.Set(secrets.PodcastFeedName(f.ShowID), h.http.URL+"/private?token=TOKEN_CANARY")
	s.o.Fetcher = feedFetcher(func(context.Context, netguard.Request) (*netguard.Response, error) {
		return &netguard.Response{Status: 200, URL: h.http.URL, Body: []byte(`<rss version="2.0"><channel><title>Private</title><item><guid>https://private.example/guid/TOKEN_CANARY</guid><title>Current generation</title><enclosure url="` + h.http.URL + `/audio?secret=TOKEN_CANARY" type="audio/mpeg" /></item></channel></rss>`)}, nil
	})
	enc, err := s.enclosure(context.Background(), f, f.Episodes[0])
	if err != nil || !strings.Contains(enc.URL, "secret=TOKEN_CANARY") {
		t.Fatal(enc, err)
	}
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	temp := ".audiosilo-tmp-private"
	root.WriteFile("Fixture Show/"+temp, []byte("prefix"), 0600)
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: f.ShowID, EpisodeID: f.Episodes[0].ID, Path: *job.Path, Temp: temp, Offset: 6, Total: 100}
	c.setSource(enc) // a hash only: the private enclosure URL is never written
	if err := s.saveCheckpoint(job, root, &c, `"https://private.example/ETAG_CANARY"`); err != nil {
		t.Fatal(err)
	}
	checkpointRoot, _ := s.checkpointRoot()
	b, _ := checkpointRoot.ReadFile(s.checkpointName(job, c.ShowID))
	checkpointRoot.Close()
	if bytes.Contains(b, []byte("ETAG_CANARY")) || bytes.Contains(b, []byte("TOKEN_CANARY")) || bytes.Contains(b, []byte("http")) {
		t.Fatal("checkpoint leak", string(b))
	}
	loaded, tag, _ := s.loadCheckpoint(job, root, f.ShowID, f.Episodes[0].ID)
	if loaded.Offset != 6 || tag != `"https://private.example/ETAG_CANARY"` {
		t.Fatal(loaded, tag)
	}
	s.o.Vault.Set(secrets.ServerToken, "new-pairing")
	loaded, tag, _ = s.loadCheckpoint(job, root, f.ShowID, f.Episodes[0].ID)
	if loaded.Offset != 0 || tag != "" {
		t.Fatal("old pairing resumed")
	}
}

type feedFetcher func(context.Context, netguard.Request) (*netguard.Response, error)

func (f feedFetcher) Fetch(ctx context.Context, r netguard.Request) (*netguard.Response, error) {
	return f(ctx, r)
}

type fakeTimer struct {
	duration time.Duration
	fn       func()
}

func (t *fakeTimer) Reset(d time.Duration) bool { t.duration = d; return true }
func (t *fakeTimer) Stop() bool                 { return true }
func TestEpisodeCeilingDoesNotResetOrExtend(t *testing.T) {
	h, _ := audioHarness(t)
	s := h.handler.s
	now := time.Now()
	s.o.BudgetNow = func() time.Time { return now }
	var timer *fakeTimer
	s.o.AfterFunc = func(d time.Duration, fn func()) Timer { timer = &fakeTimer{d, fn}; return timer }
	ctx, b := s.budget(context.Background())
	defer b.close()
	start := now
	b.tighten(300 << 20)
	if timer.duration != 65*time.Minute+6*time.Second {
		t.Fatal(timer.duration)
	}
	now = now.Add(11 * time.Minute)
	if ctx.Err() != nil {
		t.Fatal("job stopped before slow transfer could finish")
	}
	b.tighten(300 << 20)
	if !b.deadline.Equal(start.Add(65*time.Minute + 6*time.Second)) {
		t.Fatal("retry reset deadline")
	}
	b.tighten(400 << 20)
	if !b.deadline.Equal(start.Add(65*time.Minute + 6*time.Second)) {
		t.Fatal("deadline extended")
	}
	b.tighten(1)
	if ctx.Err() == nil {
		t.Fatal("smaller total did not cancel expired job")
	}
}

func TestArtworkBackoffRestartAndChangedHash(t *testing.T) {
	h := newHarness(t)
	s := h.handler.s
	s.o.Fetcher = feedFetcher(func(context.Context, netguard.Request) (*netguard.Response, error) {
		return nil, fail("network", "unavailable")
	})
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	root.Mkdir("Show", 0755)
	f := &podcast.Feed{ShowID: "a1b2c3d4e5f60718"}
	s.artwork(context.Background(), root, link.Library{ID: 3, Kind: "podcasts", Root: h.mount}, h.mount, "Show", f, "https://private.example/art/CANARY")
	state, err := h.db.PodcastArtwork(context.Background(), "server-1", 3, f.ShowID)
	if err != nil || state.Failures != 1 || state.SourceSHA256 != podcast.FeedURLHash("https://private.example/art/CANARY") {
		t.Fatal(state, err)
	}
	due, _ := time.Parse(time.RFC3339, state.NextAttemptAt)
	if due.Sub(s.o.Now()) != time.Hour {
		t.Fatal(state)
	}
	restarted := New(s.o).s
	var calls int
	restarted.o.Fetcher = feedFetcher(func(context.Context, netguard.Request) (*netguard.Response, error) {
		calls++
		return nil, errors.New("unavailable")
	})
	lib := link.Library{ID: 3, Kind: "podcasts", Root: h.mount}
	restarted.artwork(context.Background(), root, lib, h.mount, "Show", f, "https://private.example/art/CANARY")
	if calls != 0 {
		t.Fatal("restart lost backoff")
	}
	restarted.artwork(context.Background(), root, lib, h.mount, "Show", f, "https://private.example/art/CHANGED")
	if calls != 1 {
		t.Fatal("source change did not retry")
	}
}

func TestProbeAllowedAndDenied(t *testing.T) {
	requireProbe(t)
	for _, name := range []string{"chap-v23.mp3", "chap-v24-reverse.mp3", "plain.mp3"} {
		f, err := os.Open("../podcast/testdata/phase2c2/" + name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := LocalProbe("ffprobe")(context.Background(), f)
		f.Close()
		if err != nil || got.DurationMS != 4056 || got.Codec != "mp3" || got.ContentType != "audio/mpeg" {
			t.Fatal(name, got, err)
		}
	}
	f, err := os.CreateTemp(t.TempDir(), "invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	io.WriteString(f, "#EXTM3U\nhttps://PRIVATE_CANARY/audio.mp3\n")
	_, err = LocalProbe("ffprobe")(context.Background(), f)
	if err == nil || strings.Contains(err.Error(), "PRIVATE_CANARY") {
		t.Fatal("invalid audio", err)
	}
}

func TestFetchStopRestartRehashesProvenPrefix(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	audio := data(t, "chap-v23.mp3")
	started := make(chan struct{})
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("ETag", `"one"`)
		if n == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
			w.WriteHeader(200)
			w.Write(audio[:1000])
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			return
		}
		if r.Header.Get("Range") != "bytes=1000-" || r.Header.Get("If-Range") != `"one"` {
			t.Error("restart lost resume proof", r.Header)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 1000-%d/%d", len(audio)-1, len(audio)))
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)-1000))
		w.WriteHeader(206)
		w.Write(audio[1000:])
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := h.handler.FetchJob().Run(ctx, job, jobs.LocalReporter()); done <- err }()
	<-started
	// Wait for a proven prefix to reach disk before stopping the Bridge.
	deadline := time.Now().Add(time.Second)
	for {
		matches, _ := filepath.Glob(filepath.Join(h.mount, "Fixture Show", ".audiosilo-tmp-*"))
		if len(matches) == 1 {
			st, _ := os.Stat(matches[0])
			if st.Size() == 1000 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("prefix not written")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("stopped job succeeded")
	}
	restarted := New(h.handler.s.o)
	job.Resumed = true
	result, err := restarted.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(result, err)
	}
	if got := result.(fetchResult); got.Fetch.Bytes != 16403 || calls.Load() != 2 {
		t.Fatal(got, calls.Load())
	}
}

func TestAudioGETDoesNotHoldShowLockAndPreservesPollEdits(t *testing.T) {
	requireProbe(t)
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			h, job := audioHarness(t)
			s := h.handler.s
			audio := data(t, "plain.mp3")
			setAudio(h, func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
				defer cancel()
				unlock, err := s.acquire(ctx, s.showKey(h.mount, "Fixture Show"))
				if err != nil {
					t.Error("audio GET holds show lock", err)
					return
				}
				defer unlock()
				root, _ := os.OpenRoot(h.mount)
				defer root.Close()
				f := h.read("Fixture Show")
				f.Episodes[0].Title = "Changed during fetch"
				if replace {
					f.ShowID = strings.Repeat("1", 16)
				}
				if _, err := podcast.Write(root, "Fixture Show", f, s.o.Now()); err != nil {
					t.Error(err)
				}
				w.Write(audio)
			})
			_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if replace {
				if err == nil {
					t.Fatal("replacement adopted old work")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if f := h.read("Fixture Show"); f.Episodes[0].Title != "Changed during fetch" {
				t.Fatal("poll edits lost")
			}
		})
	}
}

func TestResumableStopDiscardsPartialWithoutProof(t *testing.T) {
	for _, scenario := range []string{"weak", "missing", "unknown-total"} {
		t.Run(scenario, func(t *testing.T) {
			h, job := audioHarness(t)
			started := make(chan struct{})
			setAudio(h, func(w http.ResponseWriter, r *http.Request) {
				if scenario != "unknown-total" {
					w.Header().Set("Content-Length", "10000")
				}
				switch scenario {
				case "weak":
					w.Header().Set("ETag", `W/"one"`)
				case "unknown-total":
					w.Header().Set("ETag", `"one"`)
				}
				w.WriteHeader(200)
				w.Write(bytes.Repeat([]byte("a"), 1000))
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { _, err := h.handler.FetchJob().Run(ctx, job, jobs.LocalReporter()); done <- err }()
			<-started
			deadline := time.Now().Add(time.Second)
			for {
				matches, _ := filepath.Glob(filepath.Join(h.mount, "Fixture Show", ".audiosilo-tmp-*"))
				if len(matches) == 1 {
					st, err := os.Stat(matches[0])
					if err == nil && st.Size() == 1000 {
						break
					}
				}
				if time.Now().After(deadline) {
					cancel()
					<-done
					t.Fatal("prefix not written")
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			if err := <-done; err == nil {
				t.Fatal("stopped job succeeded")
			}
			matches, _ := filepath.Glob(filepath.Join(h.mount, "Fixture Show", ".audiosilo-tmp-*"))
			if len(matches) != 0 {
				t.Fatal("unproven partial retained", matches)
			}
		})
	}
}

func TestOnlyFetchOptsIntoInteractivePool(t *testing.T) {
	h := newHarness(t)
	var fetch jobs.Handler = h.handler.FetchJob()
	if opt, ok := fetch.(jobs.InteractiveHandler); !ok || !opt.Interactive() || fetch.Lane() != jobs.LaneDownload {
		t.Fatal("fetch must opt into reserve and retain its ordinary download lane")
	}
	for _, ordinary := range []jobs.Handler{h.handler, h.handler.PollJob(), h.handler.RetainJob()} {
		if opt, ok := ordinary.(jobs.InteractiveHandler); ok && opt.Interactive() {
			t.Fatalf("%s unexpectedly opted into the reserve", ordinary.Kind())
		}
	}
}

// server#403: an episode download's codes say whose fault it was. The ceiling
// passing (anywhere, here while the audio is checked) is the episode's timeout,
// never the Bridge stopping (bridge_lost), a retry wait it cuts short
// included; a fault of the Bridge's own (no streaming client) is write_failed,
// never network.
func TestEpisodeFailureCodes(t *testing.T) {
	ceiling := func(h *harness) *fakeTimer {
		timer := &fakeTimer{}
		h.handler.s.o.AfterFunc = func(d time.Duration, fn func()) Timer { timer.duration, timer.fn = d, fn; return timer }
		return timer
	}
	t.Run("ceiling", func(t *testing.T) {
		h, job := audioHarness(t)
		timer := ceiling(h)
		setAudio(h, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("audio")) })
		h.handler.s.o.Probe = func(ctx context.Context, _ *os.File) (ProbeResult, error) {
			timer.fn()
			return ProbeResult{}, ctx.Err()
		}
		_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		if c := code(t, err); c != "timeout" {
			t.Fatalf("the ceiling passed: %q, want timeout", c)
		}
	})
	// The ceiling passing while the host has not answered yet: net/http reports
	// the ceiling's cause (errOverBudget), still the episode's timeout, never
	// network (BR-CANCEL-RACE).
	t.Run("ceiling mid-request", func(t *testing.T) {
		h, job := audioHarness(t)
		timer := ceiling(h)
		setAudio(h, func(_ http.ResponseWriter, r *http.Request) {
			timer.fn()
			<-r.Context().Done()
		})
		_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		var failure *netguard.Error
		if !errors.As(err, &failure) || failure.Code != "timeout" || failure.Message != "The episode download did not finish in time." {
			t.Fatalf("the ceiling passed mid-request: %v, want the episode's timeout", err)
		}
	})
	t.Run("cut retry wait", func(t *testing.T) {
		h, job := audioHarness(t)
		timer := ceiling(h)
		setAudio(h, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
		h.handler.s.o.Wait = func(ctx context.Context, _ time.Duration) error {
			timer.fn()
			return ctx.Err()
		}
		_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		var failure *netguard.Error
		if !errors.As(err, &failure) || failure.Code != "timeout" || failure.Message != "The episode download did not finish in time." {
			t.Fatalf("a retry wait the ceiling cut short: %v, want the episode's timeout", err)
		}
	})
	// A job cancelled (or its lease lost) while it waits to try the host again
	// ends on the cancel, a timeout, never the host's last http_503 or 429:
	// the server counts an HTTP code of a cancelled job against the feed
	// (cancelledFailureCodes), though the next try might have had the episode.
	// A host's answer the job ends on with no wait cut short stays its code.
	for _, status := range []int{503, 429} {
		t.Run(fmt.Sprintf("cancelled retry wait %d", status), func(t *testing.T) {
			h, job := audioHarness(t)
			var calls atomic.Int32
			setAudio(h, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.handler.s.o.Wait = func(ctx context.Context, _ time.Duration) error {
				cancel()
				return ctx.Err()
			}
			_, err := h.handler.FetchJob().Run(ctx, job, jobs.LocalReporter())
			if c := code(t, err); c != "timeout" || calls.Load() != 1 {
				t.Fatalf("a retry wait the job's cancel cut short: %q after %d calls, want timeout after 1", c, calls.Load())
			}
		})
	}
	t.Run("host's last answer", func(t *testing.T) {
		h, job := audioHarness(t)
		setAudio(h, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
		_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		if c := code(t, err); c != "http_503" {
			t.Fatalf("a host that answered 503 to every try: %q, want http_503", c)
		}
	})
	// An episode host's 410 is the feed's word for it, gone (the server says
	// the episode is no longer available), never a bare http_410.
	t.Run("gone", func(t *testing.T) {
		h, job := audioHarness(t)
		setAudio(h, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(410) })
		_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		if c := code(t, err); c != "gone" {
			t.Fatalf("an episode's 410: %q, want gone", c)
		}
	})
	t.Run("no streamer", func(t *testing.T) {
		h, job := audioHarness(t)
		h.handler.s.o.Streamer = nil
		_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		if c := code(t, err); c != "write_failed" {
			t.Fatalf("no streaming client: %q, want write_failed", c)
		}
	})
	// The ceiling passing once the audio checked out still stops the commit as
	// the episode's timeout.
	t.Run("ceiling before the commit", func(t *testing.T) {
		h, job := audioHarness(t)
		timer := ceiling(h)
		setAudio(h, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("audio")) })
		h.handler.s.o.Probe = func(context.Context, *os.File) (ProbeResult, error) {
			timer.fn()
			return ProbeResult{DurationMS: 1000, Codec: "mp3", ContentType: "audio/mpeg"}, nil
		}
		_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		if c := code(t, err); c != "timeout" {
			t.Fatalf("the ceiling passed before the commit: %q, want timeout", c)
		}
	})
	// The real probe the job's context stopped answers that context's error (the
	// ceiling's timeout above), never invalid_audio.
	t.Run("probe stopped", func(t *testing.T) {
		program, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.CreateTemp(t.TempDir(), "audio")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := LocalProbe(program)(ctx, f); !errors.Is(err, context.Canceled) {
			t.Fatalf("a stopped probe: %v, want its context's error", err)
		}
	})
}

// A partial proven changed when it resumes (here cut short on disk) is the
// Bridge's own copy at fault: the episode starts again from zero once inside
// the job, with no ranged GET sent for it, and only a second such partial
// fails the job write_failed.
func TestFetchChangedPartialRestartsOnce(t *testing.T) {
	requireProbe(t)
	for _, scenario := range []string{"restarts", "twice"} {
		t.Run(scenario, func(t *testing.T) {
			h, job := audioHarness(t)
			audio, calls, ranged := cutThenWhole(t, h, scenario == "twice", func() {})
			show := filepath.Join(h.mount, filepath.Dir(*job.Path))
			var reads atomic.Int32
			h.handler.s.o.Fault = func(at string) error {
				if at == "partial" {
					reads.Add(1)
					// Shorter than its record says: the restart removes it,
					// never leaves it behind with its record gone.
					truncatePartials(t, show)
				}
				return nil
			}
			result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if scenario == "twice" {
				var failure *netguard.Error
				if !errors.As(err, &failure) || failure.Code != "write_failed" || reads.Load() != 2 || calls.Load() != 2 {
					t.Fatalf("a second unreadable partial: %v, %d reads, %d calls; want write_failed after 2 and 2", err, reads.Load(), calls.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(result, err)
			}
			// The first GET, then a GET from zero: never the resume's ranged GET.
			if got := result.(fetchResult); got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 || reads.Load() != 1 || ranged.Load() != 0 {
				t.Fatal(got, calls.Load(), reads.Load(), ranged.Load())
			}
			if left := partials(show); len(left) != 0 {
				t.Fatal("the changed partial was left behind:", left)
			}
		})
	}
}

// truncatePartials cuts every owned temp in the show folder to 10 bytes.
func truncatePartials(t *testing.T, show string) {
	t.Helper()
	for _, p := range partials(show) {
		if err := os.Truncate(p, 10); err != nil {
			t.Error(err)
		}
	}
}

// A partial that cannot be read to tell whether it changed (a network
// share's hiccup, here an I/O error) is never thrown away: each host attempt
// finds it so and waits, with no GET from zero, and the job fails
// write_failed with the partial and its record kept, Abandon included. The
// next attempt, the share back, resumes it with a ranged GET. A share that
// comes back within the job's attempts resumes it there.
func TestFetchKeepsAPartialItCannotReadToResume(t *testing.T) {
	requireProbe(t)
	for _, scenario := range []string{"next attempt", "same job"} {
		t.Run(scenario, func(t *testing.T) {
			h, job := audioHarness(t)
			s := h.handler.s
			audio, calls, ranged := cutThenWhole(t, h, false, func() {})
			show := filepath.Join(h.mount, filepath.Dir(*job.Path))
			var unreadable atomic.Bool
			s.o.Fault = func(at string) error {
				if at == "partial" && unreadable.Load() {
					return syscall.EIO
				}
				return nil
			}
			waits := 0
			s.o.Wait = func(context.Context, time.Duration) error {
				waits++
				unreadable.Store(scenario != "same job" || waits != 2)
				return nil
			}
			result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if scenario == "same job" {
				if err != nil {
					t.Fatal(result, err)
				}
				if got := result.(fetchResult); got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 || ranged.Load() != 1 {
					t.Fatal(got, calls.Load(), ranged.Load())
				}
				return
			}
			if code(t, err) != "write_failed" || calls.Load() != 1 {
				t.Fatalf("a partial that could not be read: %v after %d calls, want write_failed after the first", err, calls.Load())
			}
			saved, ok := s.readCheckpoint(job, fixtureShowID)
			if left := partials(show); !ok || len(left) != 1 || saved.Temp != filepath.Base(left[0]) {
				t.Fatalf("the partial or its record was dropped: %+v %v", saved, left)
			}
			s.o.Wait = func(context.Context, time.Duration) error { return nil }
			unreadable.Store(false)
			result, err = h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if err != nil {
				t.Fatal(result, err)
			}
			if got := result.(fetchResult); got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 || ranged.Load() != 1 {
				t.Fatal("the kept partial was not resumed:", got, calls.Load(), ranged.Load())
			}
			noRecords(t, s)
		})
	}
}

// A resume's start (loadCheckpoint) clears a record only for a partial proven
// gone or changed, or barred to the Bridge (a mode 000 file: EACCES is a
// lasting fault): one it cannot open to tell (an I/O error) keeps the record
// and the partial, and the job fails write_failed with no GET. A partial gone
// or barred costs the record and its bytes, and the episode is fetched from
// zero.
func TestFetchStartKeepsARecordWhosePartialCannotBeRead(t *testing.T) {
	requireProbe(t)
	for _, scenario := range []string{"unreadable", "gone", "barred"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "barred" {
				requireNonRoot(t)
			}
			h, job := audioHarness(t)
			s := h.handler.s
			audio := data(t, "chap-v24-reverse.mp3")
			var calls, ranged atomic.Int32
			setAudio(h, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Range") != "" {
					ranged.Add(1)
				}
				w.Header().Set("ETag", `"one"`)
				w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
				w.WriteHeader(200)
				w.Write(audio)
			})
			root, err := os.OpenRoot(h.mount)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			c := abandonRecord(t, s, root, job)
			partial := filepath.Join(h.mount, c.partialName())
			t.Cleanup(func() { os.Chmod(partial, 0o600) })
			switch scenario {
			case "gone":
				os.Remove(partial)
			case "barred":
				os.Chmod(partial, 0)
			default:
				s.o.Fault = func(at string) error {
					if at == "load" {
						return syscall.EIO
					}
					return nil
				}
			}
			result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if scenario != "unreadable" {
				if err != nil {
					t.Fatal(result, err)
				}
				if got := result.(fetchResult); got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 1 || ranged.Load() != 0 {
					t.Fatal(got, calls.Load(), ranged.Load())
				}
				if _, err := os.Lstat(partial); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("the lost partial was left behind:", err)
				}
				noRecords(t, s)
				return
			}
			if code(t, err) != "write_failed" || calls.Load() != 0 {
				t.Fatalf("a partial that could not be opened: %v after %d calls, want write_failed with none", err, calls.Load())
			}
			saved, ok := s.readCheckpoint(job, fixtureShowID)
			if _, perr := os.Lstat(partial); !ok || saved.Temp != c.Temp || perr != nil {
				t.Fatalf("the record or partial was dropped: %+v %v", saved, perr)
			}
			if v, _ := s.o.Vault.Get(saved.Validator); v != `"one"` {
				t.Fatal("the validator was dropped:", v)
			}
		})
	}
}

// code is err's failure code, failing t when err carries none.
func code(t *testing.T, err error) string {
	t.Helper()
	var failure *netguard.Error
	if !errors.As(err, &failure) {
		t.Fatalf("not a coded failure: %v", err)
	}
	return failure.Code
}

func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("runs as root: a mode 000 file or a read-only directory does not stop root")
	}
}

// partials is the owned temps in the show folder.
func partials(show string) []string {
	m, _ := filepath.Glob(filepath.Join(show, podcast.TempPrefix+"*"))
	return m
}

// chmodPartials sets every owned temp in the show folder to mode.
func chmodPartials(t *testing.T, show string, mode os.FileMode) {
	t.Helper()
	for _, p := range partials(show) {
		if err := os.Chmod(p, mode); err != nil {
			t.Error(err)
		}
	}
}

// cutThenWhole answers the first GET from zero (every one when cutAll) with a
// cut proven prefix and every later one in full; a ranged GET is answered 206
// after onRange runs.
func cutThenWhole(t *testing.T, h *harness, cutAll bool, onRange func()) (audio []byte, calls, ranged *atomic.Int32) {
	audio = data(t, "chap-v23.mp3")
	calls, ranged = new(atomic.Int32), new(atomic.Int32)
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("ETag", `"one"`)
		if r.Header.Get("Range") != "" {
			ranged.Add(1)
			onRange()
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 1000-%d/%d", len(audio)-1, len(audio)))
			w.Header().Set("Content-Length", strconv.Itoa(len(audio)-1000))
			w.WriteHeader(206)
			w.Write(audio[1000:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		if n > 1 && !cutAll {
			w.Write(audio)
			return
		}
		// Cut after a proven prefix: the next attempt resumes it.
		w.WriteHeader(200)
		w.Write(audio[:1000])
	})
	return audio, calls, ranged
}

// A partial lost between attempts (cut short, removed, swapped for a link,
// barred to the Bridge: a mode 000 file, EACCES a lasting fault) is found
// before the resume's ranged GET: the next attempt goes out from zero and the
// episode is fetched whole.
func TestFetchLostPartialRestartsBeforeTheRangedGet(t *testing.T) {
	requireProbe(t)
	for _, scenario := range []string{"truncated", "removed", "link", "barred"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "barred" {
				requireNonRoot(t)
			}
			h, job := audioHarness(t)
			audio, calls, ranged := cutThenWhole(t, h, false, func() {})
			show := filepath.Join(h.mount, filepath.Dir(*job.Path))
			victim := filepath.Join(h.mount, "victim")
			if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			h.handler.s.o.Wait = func(context.Context, time.Duration) error {
				for _, p := range partials(show) {
					var err error
					switch scenario {
					case "truncated":
						err = os.Truncate(p, 10)
					case "removed":
						err = os.Remove(p)
					case "link":
						if err = os.Remove(p); err == nil {
							err = os.Symlink("../victim", p)
						}
					case "barred":
						err = os.Chmod(p, 0)
					}
					if err != nil {
						t.Error(err)
					}
				}
				return nil
			}
			result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if err != nil {
				t.Fatal(result, err)
			}
			if got := result.(fetchResult); got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 2 || ranged.Load() != 0 {
				t.Fatal(got, calls.Load(), ranged.Load())
			}
			if b, _ := os.ReadFile(victim); string(b) != "keep" {
				t.Fatalf("the link's target was written: %q", b)
			}
			if scenario != "link" {
				if m := partials(show); len(m) != 0 {
					t.Fatal("the lost partial was left behind:", m)
				}
			}
		})
	}
}

// A partial cut short after its check, while the ranged GET is out, or
// barred to the Bridge then (a mode 000 file, EACCES a lasting fault), is
// still the restart from zero (consume's open), never a write_failed job.
func TestFetchPartialChangedAtTheAppendRestarts(t *testing.T) {
	requireProbe(t)
	for _, scenario := range []string{"truncated", "barred"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "barred" {
				requireNonRoot(t)
			}
			h, job := audioHarness(t)
			show := filepath.Join(h.mount, filepath.Dir(*job.Path))
			audio, calls, ranged := cutThenWhole(t, h, false, func() {
				if scenario == "barred" {
					chmodPartials(t, show, 0)
				} else {
					truncatePartials(t, show)
				}
			})
			result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if err != nil {
				t.Fatal(result, err)
			}
			// The cut GET, the resume whose partial failed at the open, then a GET from zero.
			if got := result.(fetchResult); got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 3 || ranged.Load() != 1 {
				t.Fatal(got, calls.Load(), ranged.Load())
			}
			if m := partials(show); len(m) != 0 {
				t.Fatal("the changed partial was left behind:", m)
			}
		})
	}
}

// One that could not be read then (a share's hiccup at the append's open,
// here an I/O error) is kept: the attempt is tried again and resumes it once
// it reads, with no GET from zero.
func TestFetchPartialUnreadableAtTheAppendIsResumedAgain(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	var once sync.Once
	audio, calls, ranged := cutThenWhole(t, h, false, func() {})
	h.handler.s.o.Fault = func(at string) error {
		var err error
		if at == "append" {
			once.Do(func() { err = syscall.EIO })
		}
		return err
	}
	result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(result, err)
	}
	// The cut GET, the resume whose partial failed at the open, then the resume again.
	if got := result.(fetchResult); got.Fetch.Bytes != int64(len(audio)) || calls.Load() != 3 || ranged.Load() != 2 {
		t.Fatal(got, calls.Load(), ranged.Load())
	}
}

// A partial the restart could not remove (barred, in a show folder the Bridge
// may not write) fails the attempt write_failed, and keeps its record and
// validator through the job's exit, so a later clear, here or cleanOwned's,
// still finds and removes it.
func TestFetchKeepsTheRecordOfAPartialTheRestartCouldNotRemove(t *testing.T) {
	requireProbe(t)
	requireNonRoot(t)
	h, job := audioHarness(t)
	s := h.handler.s
	show := filepath.Join(h.mount, filepath.Dir(*job.Path))
	t.Cleanup(func() { os.Chmod(show, 0o755) })
	_, calls, _ := cutThenWhole(t, h, false, func() {})
	var stuck []string
	s.o.Wait = func(context.Context, time.Duration) error {
		stuck = partials(show)
		chmodPartials(t, show, 0)
		if err := os.Chmod(show, 0o500); err != nil {
			t.Error(err)
		}
		return nil
	}
	// The restart from zero cannot remove the barred partial: the attempt
	// fails write_failed with no GET from zero, never a loop of restarts
	// beside it. The next attempt fails too (here at the folder's own write
	// check), with no GET.
	for try := range 2 {
		result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		got := code(t, err)
		if try == 0 && (got != "write_failed" || !strings.Contains(err.Error(), "could not be removed")) || calls.Load() != 1 {
			t.Fatalf("a partial the restart cannot remove: %v %v (%s) after %d calls, want write_failed after 1", result, err, got, calls.Load())
		}
	}
	saved, ok := s.readCheckpoint(job, fixtureShowID)
	if len(stuck) != 1 || !ok || saved.Temp != filepath.Base(stuck[0]) {
		t.Fatalf("the record of the partial still on disk was dropped: %v %+v", stuck, saved)
	}
	if v, _ := s.o.Vault.Get(saved.Validator); v == "" {
		t.Fatal("the validator of the partial still on disk was dropped")
	}
	os.Chmod(show, 0o755)
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	s.clearCheckpoint(job, root, &checkpoint{ServerHash: s.pairHash(), ShowID: saved.ShowID, EpisodeID: saved.EpisodeID, Path: *job.Path})
	if _, ok := s.readCheckpoint(job, fixtureShowID); ok {
		t.Fatal("record kept once the partial was removable")
	}
	if left := partials(show); len(left) != 0 {
		t.Fatal("the earlier partial was not tried again:", left)
	}
	if v, _ := s.o.Vault.Get(saved.Validator); v != "" {
		t.Fatal("validator kept once the partial was removed")
	}
}

// A partial of another size than its record says, that the start's clear
// cannot remove (a show folder the Bridge may not write), fails the attempt
// write_failed with no GET, its record kept for a later clear, never a
// download started beside it unrecorded.
func TestFetchStartFailsOnAWrongSizePartialItCannotRemove(t *testing.T) {
	requireNonRoot(t)
	h, job := audioHarness(t)
	s := h.handler.s
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	})
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: fixtureShowID, EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-wrongsize", Offset: 5, Total: 100}
	partial := "Fixture Show/" + c.Temp
	if err := root.WriteFile(partial, []byte("prefix"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
		t.Fatal(err)
	}
	show := barShowAtLoad(t, h)
	_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if code(t, err) != "write_failed" || !strings.Contains(err.Error(), "could not be removed") || calls.Load() != 0 {
		t.Fatalf("a wrong-size partial the start cannot remove: %v after %d calls, want write_failed after none", err, calls.Load())
	}
	if saved, ok := s.readCheckpoint(job, fixtureShowID); !ok || saved.Temp != c.Temp {
		t.Fatalf("the record of the partial still on disk was dropped: %+v", saved)
	}
	if left := partials(show); len(left) != 1 {
		t.Fatal("a download was started beside the partial:", left)
	}
}

// A Publish that fails once its journal is written leaves the staged audio to
// the journal: the job's exit drops the record of the resumed partial without
// removing the file, and so does any later clear of a record naming it
// (cleanOwned's), so RecoverShow then installs the episode.
func TestFetchFailedPublishKeepsTheJournalsStagedAudio(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	audio, _, ranged := cutThenWhole(t, h, false, func() {})
	show := filepath.Join(h.mount, filepath.Dir(*job.Path))
	s.o.Fault = func(at string) error {
		if at == "journal" {
			return errors.New("stopped after the journal")
		}
		return nil
	}
	if result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter()); err == nil {
		t.Fatal("a failed publish succeeded:", result)
	}
	if ranged.Load() != 1 {
		t.Fatal("the episode was not resumed from a recorded partial:", ranged.Load())
	}
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	b, err := root.ReadFile(podcast.AttachmentPath(*job.Path, ".publish.json"))
	if err != nil {
		t.Fatal("no journal after the failed publish:", err)
	}
	var j podcast.Journal
	if err := json.Unmarshal(b, &j); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(show, j.AudioTemp)
	if got, err := os.ReadFile(staged); err != nil || !bytes.Equal(got, audio) {
		t.Fatal("the journal's staged audio was removed:", err)
	}
	if _, ok := s.readCheckpoint(job, fixtureShowID); ok {
		t.Fatal("the record still claims the journal's staged audio")
	}
	// A record naming the staged audio (one left by a stopped process) is
	// cleared without it.
	s.clearCheckpoint(job, root, &checkpoint{ServerHash: s.pairHash(), Path: *job.Path, Temp: j.AudioTemp})
	if _, err := os.Stat(staged); err != nil {
		t.Fatal("a clear removed the journal's staged audio:", err)
	}
	s.o.Fault = nil
	if err := podcast.RecoverShow(root, filepath.Dir(*job.Path), s.o.Now(), nil); err != nil {
		t.Fatal("recovery after the failed publish:", err)
	}
	if !podcast.Matches(root, *job.Path, &j.Fetch) {
		t.Fatal("recovery did not install the staged audio")
	}
	if left := partials(show); len(left) != 0 {
		t.Fatal("partials left after recovery:", left)
	}
}

// A show an older Bridge left stuck (a journal whose staged audio its failed
// publish removed) is recovered by the next fetch: the journal goes and the
// episode is downloaded again and installed.
func TestFetchAfterAStuckJournalFetchesTheEpisodeAgain(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	audio := data(t, "chap-v24-reverse.mp3")
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write(audio)
	})
	show := filepath.Join(h.mount, filepath.Dir(*job.Path))
	s.o.Fault = func(at string) error {
		if at == "journal" {
			return errors.New("stopped after the journal")
		}
		return nil
	}
	if result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter()); err == nil {
		t.Fatal("a failed publish succeeded:", result)
	}
	for _, p := range partials(show) {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	s.o.Fault = nil
	result, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal("the stuck show still refuses the fetch:", err)
	}
	got := result.(fetchResult)
	if got.Reused || calls.Load() != 2 {
		t.Fatal("the episode was not downloaded again:", got, calls.Load())
	}
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if !podcast.Matches(root, *job.Path, &got.Fetch) {
		t.Fatal("the episode was not installed")
	}
	if _, err := root.Lstat(podcast.AttachmentPath(*job.Path, ".publish.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the stuck journal was kept:", err)
	}
	if f := h.read("Fixture Show"); f.Episodes[0].Fetch == nil || *f.Episodes[0].Fetch != got.Fetch {
		t.Fatal("the feed does not pin the installed episode", f.Episodes[0].Fetch)
	}
}

// A clear removes a recorded partial beside a journal that stages another file.
func TestClearRemovesAPartialNoJournalStages(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	show := filepath.Join(h.mount, filepath.Dir(*job.Path))
	if err := os.MkdirAll(filepath.Join(show, ".audiosilo"), 0o755); err != nil {
		t.Fatal(err)
	}
	other, mine := podcast.TempPrefix+strings.Repeat("a", 24), podcast.TempPrefix+strings.Repeat("b", 24)
	b, _ := json.Marshal(podcast.Journal{Version: 1, Op: "install", Path: *job.Path, AudioTemp: other})
	if err := os.WriteFile(filepath.Join(h.mount, podcast.AttachmentPath(*job.Path, ".publish.json")), b, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{other, mine} {
		if err := os.WriteFile(filepath.Join(show, name), []byte("part"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s.clearCheckpoint(job, root, &checkpoint{ServerHash: s.pairHash(), Path: *job.Path, Temp: mine})
	if _, err := os.Stat(filepath.Join(show, mine)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a partial no journal stages was kept:", err)
	}
	if _, err := os.Stat(filepath.Join(show, other)); err != nil {
		t.Fatal("the journal's staged file was removed:", err)
	}
}

// A clear keeps a recorded partial beside a journal it cannot read (it may
// stage that partial), and drops only the record.
func TestClearKeepsAPartialBesideAnUnreadableJournal(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	show := filepath.Join(h.mount, filepath.Dir(*job.Path))
	if err := os.MkdirAll(filepath.Join(show, ".audiosilo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.mount, podcast.AttachmentPath(*job.Path, ".publish.json")), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	mine := podcast.TempPrefix + strings.Repeat("c", 24)
	if err := os.WriteFile(filepath.Join(show, mine), []byte("part"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s.clearCheckpoint(job, root, &checkpoint{ServerHash: s.pairHash(), Path: *job.Path, Temp: mine})
	if _, err := os.Stat(filepath.Join(show, mine)); err != nil {
		t.Fatal("a partial an unreadable journal may stage was removed:", err)
	}
	if _, ok := s.readCheckpoint(job, fixtureShowID); ok {
		t.Fatal("the record was kept")
	}
}

// A clear keeps a recorded partial a journal stages under another name for the
// same file (a case-insensitive or normalising filesystem's other spelling,
// here a hard link): RecoverShow installs that file.
func TestClearKeepsAPartialAJournalStagesUnderAnotherName(t *testing.T) {
	h, job := audioHarness(t)
	s := h.handler.s
	show := filepath.Join(h.mount, filepath.Dir(*job.Path))
	if err := os.MkdirAll(filepath.Join(show, ".audiosilo"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged, mine := podcast.TempPrefix+strings.Repeat("d", 24), podcast.TempPrefix+strings.Repeat("e", 24)
	b, _ := json.Marshal(podcast.Journal{Version: 1, Op: "install", Path: *job.Path, AudioTemp: staged})
	if err := os.WriteFile(filepath.Join(h.mount, podcast.AttachmentPath(*job.Path, ".publish.json")), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(show, staged), []byte("part"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(show, staged), filepath.Join(show, mine)); err != nil {
		t.Skip("no hard links here:", err)
	}
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s.clearCheckpoint(job, root, &checkpoint{ServerHash: s.pairHash(), Path: *job.Path, Temp: mine})
	if _, err := os.Stat(filepath.Join(show, mine)); err != nil {
		t.Fatal("the journal's staged file, recorded under another name, was removed:", err)
	}
	if _, ok := s.readCheckpoint(job, fixtureShowID); ok {
		t.Fatal("the record was kept")
	}
}

// sameSource tells another enclosure by its URL, and by its length only when
// both the record and the feed give one; a record with no source cannot tell.
func TestCheckpointSameSource(t *testing.T) {
	var c checkpoint
	if !c.sameSource(podcast.Enclosure{URL: "https://a.example/1.mp3"}) {
		t.Fatal("a record with no source refused an enclosure")
	}
	c.setSource(podcast.Enclosure{URL: "https://a.example/1.mp3", Length: 100})
	for enc, want := range map[podcast.Enclosure]bool{
		{URL: "https://a.example/1.mp3", Length: 100}: true,
		{URL: "https://a.example/1.mp3"}:              true,
		{URL: "https://a.example/1.mp3", Length: 99}:  false,
		{URL: "https://a.example/2.mp3", Length: 100}: false,
	} {
		if c.sameSource(enc) != want {
			t.Error(enc, "want", want)
		}
	}
	c.setSource(podcast.Enclosure{URL: "https://a.example/1.mp3", Length: -1})
	if c.SourceLength != 0 || !c.sameSource(podcast.Enclosure{URL: "https://a.example/1.mp3", Length: 5}) {
		t.Fatal("an unknown length was recorded as one", c.SourceLength)
	}
}

// resumes takes a partial's own enclosure, and one whose URL differs only in
// its query (a signature, a tracking id) while the feed gives the same known
// length; never another path or host, another length, or a query change with
// no length to tell by. A record from before SourceBase, with no base, takes
// only its own URL; one with no source at all cannot tell, and takes any.
func TestCheckpointResumes(t *testing.T) {
	var c checkpoint
	if !c.resumes(podcast.Enclosure{URL: "https://a.example/1.mp3?sig=2"}) {
		t.Fatal("a record with no source refused an enclosure")
	}
	c.setSource(podcast.Enclosure{URL: "https://a.example/1.mp3?sig=1&exp=9", Length: 100})
	for enc, want := range map[podcast.Enclosure]bool{
		{URL: "https://a.example/1.mp3?sig=1&exp=9", Length: 100}:  true,
		{URL: "https://a.example/1.mp3?sig=1&exp=9"}:               true,
		{URL: "https://a.example/1.mp3?sig=2&exp=10", Length: 100}: true,
		{URL: "https://a.example/1.mp3", Length: 100}:              true,
		{URL: "https://a.example/1.mp3?sig=2&exp=10"}:              false,
		{URL: "https://a.example/1.mp3?sig=2&exp=10", Length: 99}:  false,
		{URL: "https://a.example/2.mp3?sig=1&exp=9", Length: 100}:  false,
		{URL: "https://b.example/1.mp3?sig=1&exp=9", Length: 100}:  false,
	} {
		if c.resumes(enc) != want {
			t.Error(enc, "want", want)
		}
		if enc.URL != "https://a.example/1.mp3?sig=1&exp=9" && c.sameSource(enc) {
			t.Error("a whole download took another URL as its own:", enc)
		}
	}
	c.SourceBase = ""
	if c.resumes(podcast.Enclosure{URL: "https://a.example/1.mp3?sig=2&exp=10", Length: 100}) {
		t.Fatal("a record with no base took another query")
	}
	if sourceBase("https://a.example/secret/1.mp3?token=x#t=5") != podcast.FeedURLHash("https://a.example/secret/1.mp3") || sourceBase("https://a.example/1.mp3?a=1") != sourceBase("https://a.example/1.mp3#t=5") {
		t.Fatal("the base is not a hash of the URL without its query and fragment")
	}
	// A path naming no audio file keeps the query that tells its episodes
	// apart (podcast.EnclosureBase): another id there is another file, never
	// resumed, however equal its length.
	var d checkpoint
	d.setSource(podcast.Enclosure{URL: "https://a.example/uc?export=download&id=1", Length: 100})
	if d.resumes(podcast.Enclosure{URL: "https://a.example/uc?export=download&id=2", Length: 100}) {
		t.Fatal("a partial took another file's query on a path naming no audio file")
	}
	if !d.resumes(podcast.Enclosure{URL: "https://a.example/uc?export=download&id=1#t=5", Length: 100}) {
		t.Fatal("a partial refused its own enclosure with a fragment")
	}
	// A whole download takes no query leeway: it is published with no GET.
	c.setSource(podcast.Enclosure{URL: "https://a.example/1.mp3?sig=1", Length: 100})
	c.Offset, c.Total, c.SHA256 = 100, 100, strings.Repeat("a", 64)
	if !c.complete() || c.resumes(podcast.Enclosure{URL: "https://a.example/1.mp3?sig=2", Length: 100}) {
		t.Fatal("a whole download took another query as its own")
	}
}

// A partial kept for a resume is resumed (Range, If-Range with its ETag) only
// from the enclosure it was downloaded from. One the feed moved (another URL)
// or remade (another length) under the same episode id since is cleared and
// the episode downloaded from byte 0 in the one request, never resumed
// against the new URL with the old ETag; one the clear cannot remove fails
// write_failed with no request, never a download beside it. A URL whose
// query alone changed (a signed or tracked enclosure) still resumes while the
// feed gives the same known length, unless its path names no audio file
// (podcast.EnclosureBase), where the query tells episodes apart. A record from before the source was kept
// resumes on its ETag alone, as it did.
func TestAKeptPartialIsDownloadedAgainWhenItsEnclosureChanged(t *testing.T) {
	requireProbe(t)
	for _, scenario := range []string{"same", "path", "query with no length", "length", "query", "query and length", "query on no file", "no base", "legacy", "stuck"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "stuck" {
				requireNonRoot(t)
			}
			h, job := audioHarness(t)
			s := h.handler.s
			root, err := os.OpenRoot(h.mount)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			audio := data(t, "chap-v24-reverse.mp3")
			f := h.read("Fixture Show")
			enc := f.Episodes[0].Enclosure
			enc.Length = int64(len(audio))
			if scenario != "query on no file" {
				enc.URL = h.http.URL + "/audio.mp3" // a path naming an audio file: its query may change
			}
			if scenario == "query with no length" {
				enc.Length = 0 // no length to tell a query change by
			}
			c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: fixtureShowID, EpisodeID: f.Episodes[0].ID, Path: *job.Path, Temp: ".audiosilo-tmp-kept", Offset: 1000, Total: int64(len(audio))}
			c.setSource(enc)
			if scenario == "legacy" {
				c.Source, c.SourceLength, c.SourceBase = "", 0, ""
			}
			if scenario == "no base" {
				c.SourceBase = ""
			}
			partial := "Fixture Show/" + c.Temp
			if err := root.WriteFile(partial, audio[:1000], 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
				t.Fatal(err)
			}
			f.Episodes[0].Enclosure = enc
			switch scenario {
			case "path":
				f.Episodes[0].Enclosure.URL = h.http.URL + "/audio" // another file at the same host, the same length
			case "query with no length", "query", "query on no file", "no base", "legacy":
				f.Episodes[0].Enclosure.URL += "?sig=2"
			case "length", "stuck":
				f.Episodes[0].Enclosure.Length = int64(len(audio)) + 1
			case "query and length":
				f.Episodes[0].Enclosure.URL += "?sig=2"
				f.Episodes[0].Enclosure.Length = int64(len(audio)) + 1
			}
			if _, err := podcast.Write(root, "Fixture Show", f, s.o.Now()); err != nil {
				t.Fatal(err)
			}
			resumes := scenario == "same" || scenario == "query" || scenario == "legacy"
			var calls atomic.Int32
			setAudio(h, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("ETag", `"one"`)
				if ranged := r.Header.Get("Range") != ""; ranged != resumes {
					t.Errorf("ranged %v, want %v: %v", ranged, resumes, r.Header)
				}
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
					w.WriteHeader(200)
					w.Write(audio)
					return
				}
				if r.Header.Get("Range") != "bytes=1000-" || r.Header.Get("If-Range") != `"one"` {
					t.Error("unproven range", r.Header)
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 1000-%d/%d", len(audio)-1, len(audio)))
				w.Header().Set("Content-Length", strconv.Itoa(len(audio)-1000))
				w.WriteHeader(206)
				w.Write(audio[1000:])
			})
			if scenario == "stuck" {
				show := barShowAtLoad(t, h)
				_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
				if code(t, err) != "write_failed" || !strings.Contains(err.Error(), "could not be removed") || calls.Load() != 0 {
					t.Fatal(err, calls.Load())
				}
				if saved, ok := s.readCheckpoint(job, fixtureShowID); !ok || saved.Temp != c.Temp {
					t.Fatalf("the record of the partial still on disk was dropped: %+v", saved)
				}
				if left := partials(show); len(left) != 1 {
					t.Fatal("a download was started beside the partial:", left)
				}
				return
			}
			res, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if _, isResult := res.(fetchResult); err != nil || !isResult {
				t.Fatal(res, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("%d requests, want 1", calls.Load())
			}
			if b, err := os.ReadFile(filepath.Join(h.mount, *job.Path)); err != nil || !bytes.Equal(b, audio) {
				t.Fatal("the published episode is not the host's audio", err)
			}
			if left := partials(filepath.Join(h.mount, "Fixture Show")); len(left) != 0 {
				t.Fatal("a partial was left behind", left)
			}
			noRecords(t, s)
		})
	}
}

// A partial proven changed on disk while it downloaded (a copy made over the
// show folder in place) is downloaded again from byte 0 only once it is
// cleared: one the clear cannot remove fails write_failed with no second
// request, never a download beside it.
func TestAPartialChangedOnDiskThatCannotBeClearedFails(t *testing.T) {
	requireProbe(t)
	requireNonRoot(t)
	h, job := audioHarness(t)
	s := h.handler.s
	audio := data(t, "chap-v24-reverse.mp3")
	var calls atomic.Int32
	setAudio(h, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", `"one"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
		w.WriteHeader(200)
		w.Write(audio)
	})
	show, bar := barShow(t, h)
	var once sync.Once
	s.o.Fault = func(at string) error {
		if at == "rehash" {
			once.Do(func() {
				left := partials(show)
				if len(left) != 1 {
					t.Error("no partial to change:", left)
					return
				}
				changed := bytes.Repeat([]byte{0xAA}, len(audio))
				if err := os.WriteFile(left[0], changed, 0o600); err != nil {
					t.Error(err)
				}
				bar()
			})
		}
		return nil
	}
	_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
	if code(t, err) != "write_failed" || !strings.Contains(err.Error(), "could not be removed") || calls.Load() != 1 {
		t.Fatalf("a changed partial the restart cannot remove: %v after %d calls, want write_failed after 1", err, calls.Load())
	}
	if left := partials(show); len(left) != 1 {
		t.Fatal("a download was started beside the partial:", left)
	}
	if _, err := os.Lstat(filepath.Join(h.mount, *job.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the changed partial was published", err)
	}
}

// A host's retry that must start from byte 0 (a resume the host did not
// prove, a cut with no strong ETag to resume by, a resume the host answered
// with the whole file) goes out (or is written) only once the
// partial is cleared: one the clear cannot remove fails write_failed with no
// further request, never a download beside it.
func TestARetryFromZeroThatCannotClearItsPartialFails(t *testing.T) {
	requireProbe(t)
	requireNonRoot(t)
	for _, scenario := range []string{"unproven resume", "cut with no etag", "resume answered whole"} {
		t.Run(scenario, func(t *testing.T) {
			h, job := audioHarness(t)
			s := h.handler.s
			audio := data(t, "chap-v24-reverse.mp3")
			show, bar := barShow(t, h)
			if scenario != "cut with no etag" {
				root, err := os.OpenRoot(h.mount)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: fixtureShowID, EpisodeID: "1cc306d831645e2b", Path: *job.Path, Temp: ".audiosilo-tmp-kept", Offset: 1000, Total: int64(len(audio))}
				if err := root.WriteFile("Fixture Show/"+c.Temp, audio[:1000], 0o600); err != nil {
					t.Fatal(err)
				}
				if err := s.saveCheckpoint(job, root, &c, `"one"`); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			setAudio(h, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if scenario == "unproven resume" {
					bar()
					w.Header().Set("ETag", `"one"`)
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 999-%d/%d", len(audio)-1, len(audio)))
					w.Header().Set("Content-Length", strconv.Itoa(len(audio)-999))
					w.WriteHeader(206)
					w.Write(audio[999:])
					return
				}
				if scenario == "resume answered whole" {
					bar()
					w.Header().Set("ETag", `"two"`)
					w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
					w.WriteHeader(200)
					w.Write(audio)
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
				w.WriteHeader(200)
				w.Write(audio[:1000])
				w.(http.Flusher).Flush()
				if !partialAt(show, 1000) {
					t.Error("the download did not start")
					return
				}
				bar() // then cut short of its Content-Length
			})
			_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
			if code(t, err) != "write_failed" || !strings.Contains(err.Error(), "could not be removed") || calls.Load() != 1 {
				t.Fatalf("a retry from zero past a partial it cannot remove: %v after %d calls, want write_failed after 1", err, calls.Load())
			}
			if left := partials(show); len(left) != 1 {
				t.Fatal("a download was started beside the partial:", left)
			}
		})
	}
}

// barShowAtLoad makes the fixture show's folder one the Bridge may not write
// once a fetch's own write check has passed (at its record's load), and
// answers the folder's path.
func barShowAtLoad(t *testing.T, h *harness) string {
	t.Helper()
	show, bar := barShow(t, h)
	h.handler.s.o.Fault = func(at string) error {
		if at == "load" {
			bar()
		}
		return nil
	}
	return show
}

// barShow answers the fixture show's folder and a bar that makes it one the
// Bridge may not write (lifted when the test ends).
func barShow(t *testing.T, h *harness) (string, func()) {
	t.Helper()
	show := filepath.Join(h.mount, "Fixture Show")
	t.Cleanup(func() { os.Chmod(show, 0o755) })
	return show, func() {
		if err := os.Chmod(show, 0o500); err != nil {
			t.Error(err)
		}
	}
}
