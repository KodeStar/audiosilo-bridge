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
	"strconv"
	"strings"
	"sync/atomic"
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
	url, err := s.enclosure(context.Background(), f, f.Episodes[0])
	if err != nil || !strings.Contains(url, "secret=TOKEN_CANARY") {
		t.Fatal(url, err)
	}
	root, _ := os.OpenRoot(h.mount)
	defer root.Close()
	temp := ".audiosilo-tmp-private"
	root.WriteFile("Fixture Show/"+temp, []byte("prefix"), 0600)
	c := checkpoint{ServerHash: s.pairHash(), LibraryID: 3, ShowID: f.ShowID, EpisodeID: f.Episodes[0].ID, Path: *job.Path, Temp: temp, Offset: 6, Total: 100}
	if err := s.saveCheckpoint(job, &c, `"https://private.example/ETAG_CANARY"`); err != nil {
		t.Fatal(err)
	}
	checkpointRoot, _ := s.checkpointRoot()
	b, _ := checkpointRoot.ReadFile(s.checkpointName(job))
	checkpointRoot.Close()
	if bytes.Contains(b, []byte("ETAG_CANARY")) || bytes.Contains(b, []byte("TOKEN_CANARY")) || bytes.Contains(b, []byte("http")) {
		t.Fatal("checkpoint leak", string(b))
	}
	loaded, tag := s.loadCheckpoint(job, root, f.ShowID, f.Episodes[0].ID)
	if loaded.Offset != 6 || tag != `"https://private.example/ETAG_CANARY"` {
		t.Fatal(loaded, tag)
	}
	s.o.Vault.Set(secrets.ServerToken, "new-pairing")
	loaded, tag = s.loadCheckpoint(job, root, f.ShowID, f.Episodes[0].ID)
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
