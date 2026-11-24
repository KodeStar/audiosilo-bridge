package podcastjob

import (
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
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/mapping"
	"github.com/kodestar/audiosilo-bridge/internal/netguard"
	"github.com/kodestar/audiosilo-bridge/internal/podcast"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// outage is the harness's Registry while the library's mapping is unknown:
// down, its last check doesn't stand (the server or the folder didn't answer
// it), which may last; unchecked, it has no check yet (the first one after a
// start).
type outage struct {
	*mapping.Registry
	down, unchecked atomic.Bool
	asked           atomic.Int32
}

func (l *outage) Status(id int64) (link.LibraryStatus, bool) {
	st, ok := l.Registry.Status(id)
	switch {
	case l.unchecked.Load():
		l.asked.Add(1)
		return link.LibraryStatus{}, false
	case l.down.Load():
		l.asked.Add(1)
		st.Mapped, st.Writable, st.Error = false, false, "the server hasn't answered a check of this mapping, so writes wait for one it answers"
		return st, false
	}
	return st, ok
}

// showLockFree reports whether the show's lock can be taken now, as a poll or
// another episode's fetch would take it.
func showLockFree(s *service, mount, show string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	unlock, err := s.acquire(ctx, s.showKey(mount, show))
	if err != nil {
		return false
	}
	unlock()
	return true
}

// awaitTrue waits (at most 10 s) for ok, failing as what when the job ends
// first or the time runs out.
func awaitTrue(t *testing.T, what string, done <-chan error, ok func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !ok() {
		select {
		case err := <-done:
			t.Fatalf("the job ended before %s: %v", what, err)
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// A fetch whose library's mapping stops standing while its episode downloads
// (a check the server didn't answer) waits for one before it publishes, and
// holds neither the show's lock (the show's polls and its other episodes go
// on) nor its download-lane slot (the lane holds one download: an Audible
// import or a LibriVox recording would wait behind it) meanwhile. Once a check
// stands it takes a slot again and publishes the episode.
func TestFetchWaitsToPublishWithoutTheShowLockOrItsSlot(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	libs := &outage{Registry: h.libs}
	s.o.Libraries = libs
	s.o.CheckPoll = 2 * time.Millisecond
	audio := data(t, "plain.mp3")
	setAudio(h, func(w http.ResponseWriter, _ *http.Request) {
		libs.down.Store(true) // the mapping's check goes unanswered mid-download
		w.Write(audio)
	})
	rep := jobs.LocalReporter()
	done := make(chan error, 1)
	go func() {
		_, err := h.handler.FetchJob().Run(context.Background(), job, rep)
		done <- err
	}()
	awaitTrue(t, "the fetch gave its slot back", done, rep.Yielded)
	if !showLockFree(s, h.mount, "Fixture Show") {
		t.Fatal("the fetch holds its show's lock while it waits for the mapping")
	}
	if p := rep.Last(); p.Step != "waiting" || p.Code != jobs.SayMappingWait {
		t.Fatalf("a fetch waiting for its mapping reports %+v", p)
	}
	if _, err := os.Stat(filepath.Join(h.mount, *job.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the episode was published while no check stood: %v", err)
	}
	libs.down.Store(false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rep.Yielded() {
		t.Fatal("the fetch went on without taking a lane slot again")
	}
	if f := h.read("Fixture Show"); f.Episodes[0].Fetch == nil || f.Episodes[0].Fetch.DeletedAt != "" {
		t.Fatal("the episode was not published once the mapping stood", f.Episodes[0])
	}
}

// A fetch claimed while its library's last check doesn't stand gives its lane
// slot back for the wait too, and takes one again before it downloads
// anything. Not while the library merely has no check yet (the first after a
// start is seconds away: it would only queue behind a download claimed after
// it), and never a light job (a follow), which holds no download slot.
func TestFetchYieldsItsSlotOnlyThroughAnOutage(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	libs := &outage{Registry: h.libs}
	s.o.Libraries = libs
	s.o.CheckPoll = 2 * time.Millisecond
	audio := data(t, "plain.mp3")
	var gets atomic.Int32
	rep := jobs.LocalReporter()
	setAudio(h, func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		if rep.Yielded() {
			t.Error("the episode downloads without a lane slot")
		}
		w.Write(audio)
	})
	run := func() chan error {
		done := make(chan error, 1)
		go func() {
			_, err := h.handler.FetchJob().Run(context.Background(), job, rep)
			done <- err
		}()
		return done
	}

	libs.unchecked.Store(true)
	done := run()
	awaitTrue(t, "the fetch asked for its mapping", done, func() bool { return libs.asked.Load() >= 5 })
	if rep.Yielded() {
		t.Fatal("a fetch waiting for the library's first check gave its slot back")
	}
	libs.down.Store(true)
	libs.unchecked.Store(false)
	awaitTrue(t, "the fetch gave its slot back", done, rep.Yielded)
	if gets.Load() != 0 {
		t.Fatal("the episode was asked for while no check stood")
	}
	libs.down.Store(false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rep.Yielded() || gets.Load() != 1 {
		t.Fatalf("after the outage: yielded %v, %d downloads", rep.Yielded(), gets.Load())
	}

	// A follow waits too, but has no download slot to give back.
	libs.asked.Store(0)
	libs.down.Store(true)
	follow := jobs.LocalReporter()
	followed := make(chan error, 1)
	go func() {
		_, err := h.handler.Run(context.Background(), h.claim(h.http.URL+"/feed", false), follow)
		followed <- err
	}()
	awaitTrue(t, "the follow asked for its mapping", followed, func() bool { return libs.asked.Load() >= 5 })
	if follow.Yielded() {
		t.Fatal("a follow gave a lane slot back")
	}
	libs.down.Store(false)
	if err := <-followed; err != nil {
		t.Fatal(err)
	}
}

// A fetch whose folder stops answering while its episode downloads reports
// that it waits for the folder, not the save it is about to make, and once the
// folder answers it reports that step again and publishes.
func TestAFetchWaitingForItsFolderSaysSo(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	s.o.CheckPoll = 2 * time.Millisecond
	var slow atomic.Bool
	var slowProbes atomic.Int32
	s.o.WriteProbe = func(dir string) error {
		if !slow.Load() {
			return mapping.ProbeWritable(dir)
		}
		slowProbes.Add(1)
		return &fs.PathError{Op: "probe", Path: dir, Err: fsutil.ErrStatTimeout}
	}
	audio := data(t, "plain.mp3")
	setAudio(h, func(w http.ResponseWriter, _ *http.Request) {
		slow.Store(true) // the mount hangs mid-download
		w.Write(audio)
	})
	rep := jobs.LocalReporter()
	done := make(chan error, 1)
	go func() {
		_, err := h.handler.FetchJob().Run(context.Background(), job, rep)
		done <- err
	}()
	awaitTrue(t, "the fetch waited on its folder", done, func() bool { return slowProbes.Load() >= 3 })
	if p := rep.Last(); p.Step != "waiting" || p.Code != jobs.SayFolderWait {
		t.Fatalf("a fetch waiting for its folder reports %+v", p)
	}
	slow.Store(false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f := h.read("Fixture Show"); f.Episodes[0].Fetch == nil {
		t.Fatal("the episode was not published once the folder answered")
	}
}

// Once the folder answers, a wait reports again the step it interrupted, but
// never a wait it was at before (the library's), which is over.
func TestAWaitReportsTheStepItInterruptedAgain(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	s.o.CheckPoll = 2 * time.Millisecond
	lib, _ := s.o.Libraries.Library(*job.LibraryID)
	saving := jobs.Progress{Step: "writing", Code: jobs.SaySavingEpisode}
	for _, tc := range []struct {
		at    jobs.Progress
		again bool
	}{
		{saving, true},
		{waitingFor(errMappingUnknown), false},
	} {
		rep := jobs.LocalReporter()
		rep.Progress(tc.at)
		var slow atomic.Bool
		slow.Store(true)
		var seen atomic.Value
		s.o.WriteProbe = func(dir string) error {
			if slow.CompareAndSwap(true, false) {
				return &fs.PathError{Op: "probe", Path: dir, Err: fsutil.ErrStatTimeout}
			}
			seen.Store(rep.Last())
			return mapping.ProbeWritable(dir)
		}
		if err := s.awaitWrite(context.Background(), rep, lib, h.mount, "Fixture Show"); err != nil {
			t.Fatal(err)
		}
		if p, _ := seen.Load().(jobs.Progress); p.Step != "waiting" {
			t.Fatalf("at %+v: the look after a slow one saw %+v; want the wait reported", tc.at, p)
		}
		if p := rep.Last(); (p.Step == tc.at.Step && p.Code == tc.at.Code && p.Text() == tc.at.Text()) != tc.again {
			t.Fatalf("at %+v: after the wait the fetch reports %+v; reported again: want %v", tc.at, p, tc.again)
		}
	}
}

// retainHarness is a fetched episode a retain may delete, with the server that
// grants it: hook runs at each of the server's retention answers (1: the
// policy; 2, 3: the episode's two checks).
func retainHarness(t *testing.T, hook func(n int)) (*harness, *outage, jobs.Job, string) {
	t.Helper()
	requireProbe(t)
	h, fetch := audioHarness(t)
	audio := data(t, "chap-v24-reverse.mp3")
	setAudio(h, func(w http.ResponseWriter, _ *http.Request) { w.Write(audio) })
	if _, err := h.handler.FetchJob().Run(context.Background(), fetch, jobs.LocalReporter()); err != nil {
		t.Fatal(err)
	}
	s := h.handler.s
	libs := &outage{Registry: h.libs}
	s.o.Libraries = libs
	s.o.CheckPoll = 2 * time.Millisecond
	keep := link.PodcastPolicy{Mode: "keep", Keep: 3, Days: 0}
	now := s.o.Now()
	var answers atomic.Int32
	server := &retainServer{scanServer: h.scan, policy: keep}
	server.check = func(r link.PodcastRetentionRequest) *link.PodcastRetentionCheck {
		hook(int(answers.Add(1)) + 1)
		return eligibleCheck(r, keep, now)
	}
	s.o.Server = func() Server { return policyHook{server, func() { hook(1) }} }
	show := "Fixture Show"
	params, _ := json.Marshal(map[string]string{"show_id": "a1b2c3d4e5f60718"})
	return h, libs, jobs.Job{ID: 82, Kind: RetainKind, LibraryID: fetch.LibraryID, Path: &show, Params: params, Attempt: 1}, *fetch.Path
}

// policyHook runs hook as the server answers a retain's policy read.
type policyHook struct {
	*retainServer
	hook func()
}

func (s policyHook) PodcastRetention(ctx context.Context, id int64, show, showID string) (*link.PodcastPolicy, error) {
	s.hook()
	return s.retainServer.PodcastRetention(ctx, id, show, showID)
}

// A retain whose library's mapping stops standing once it has started waits
// for a check that stands without the show's lock, then goes on and deletes
// what the server grants.
func TestRetainWaitsForItsMappingWithoutTheShowLock(t *testing.T) {
	var libs *outage
	var waiting atomic.Bool
	h, libs, job, episode := retainHarness(t, func(n int) {
		if n == 1 {
			libs.down.Store(true) // unanswered between the job's start and its show's lock
			waiting.Store(true)
		}
	})
	s := h.handler.s
	done := make(chan error, 1)
	var result any
	go func() {
		var err error
		result, err = h.handler.RetainJob().Run(context.Background(), job, jobs.LocalReporter())
		done <- err
	}()
	awaitTrue(t, "the retain waited on its mapping", done, func() bool { return waiting.Load() && libs.asked.Load() >= 5 })
	if !showLockFree(s, h.mount, "Fixture Show") {
		t.Fatal("the retain holds its show's lock while it waits for the mapping")
	}
	libs.down.Store(false)
	if err := <-done; err != nil {
		t.Fatal(result, err)
	}
	if got := result.(retainResult); got.Deleted != 1 {
		t.Fatal("the retain did not go on once the mapping stood", got)
	}
	if _, err := os.Stat(filepath.Join(h.mount, episode)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the granted episode is still there: %v", err)
	}
}

// A mapping that stops standing under the show's lock, between the server's
// grant and the deletion, deletes nothing and waits for nothing there: the
// episode is left for the next run.
func TestRetainLeavesAnEpisodeItsMappingNoLongerBacks(t *testing.T) {
	var libs *outage
	h, libs, job, episode := retainHarness(t, func(n int) {
		if n == 3 {
			libs.down.Store(true) // the locked check's answer, right before the deletion
		}
	})
	result, err := h.handler.RetainJob().Run(context.Background(), job, jobs.LocalReporter())
	if err != nil {
		t.Fatal(result, err)
	}
	if got := result.(retainResult); got.Deleted != 0 || got.Skipped != 1 {
		t.Fatal("a deletion no check backed", got)
	}
	if _, err := os.Stat(filepath.Join(h.mount, episode)); err != nil {
		t.Fatalf("the episode was deleted while no check stood: %v", err)
	}
	if f := h.read("Fixture Show"); f.Episodes[0].Fetch == nil || f.Episodes[0].Fetch.DeletedAt != "" {
		t.Fatal("the episode's record says deleted", f.Episodes[0])
	}
}

// showLockHeld reports whether a job holds the show's lock now.
func showLockHeld(s *service, mount, show string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.locks[s.showKey(mount, show)]
	return l != nil && len(l.gate) == 1
}

// A fetch whose show folder was renamed by hand takes it in (adopt) under the
// show's lock: a write probe of the folder that doesn't answer in time (a disk
// spinning up) is waited for without that lock, so the show's other jobs go
// on (a poll of the show takes the lock and looks at the folder while the
// fetch waits), and the fetch takes the folder in and publishes there once it
// answers.
func TestATakeInWaitsForASlowFolderWithoutTheShowLock(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	s.o.CheckPoll = 2 * time.Millisecond
	audio := data(t, "chap-v24-reverse.mp3")
	setAudio(h, func(w http.ResponseWriter, _ *http.Request) { w.Write(audio) })
	var slow atomic.Bool
	var slowProbes, underLock atomic.Int32
	slow.Store(true)
	s.o.WriteProbe = func(dir string) error {
		if !slow.Load() || !strings.Contains(dir, "Renamed Show") {
			return mapping.ProbeWritable(dir)
		}
		slowProbes.Add(1)
		if showLockHeld(s, h.mount, "Renamed Show") {
			underLock.Add(1) // a take-in's one look, under the lock
		}
		return &fs.PathError{Op: "probe", Path: dir, Err: fsutil.ErrStatTimeout}
	}
	renameShow(t, h, "Fixture Show", "Renamed Show")
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	done := make(chan error, 1)
	go func() {
		_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		done <- err
	}()
	awaitTrue(t, "the take-in waited on its folder", done, func() bool { return slowProbes.Load() >= 5 })
	if !showLockFree(s, h.mount, "Renamed Show") {
		t.Fatal("the fetch holds its show's lock while it waits for the folder to answer")
	}
	if n := underLock.Load(); n != 1 {
		t.Fatalf("%d looks under the show's lock, want the take-in's one", n)
	}
	// A poll of the show gets its lock and makes its own take-in's look
	// while the fetch waits.
	show := "Fixture Show"
	params, _ := json.Marshal(map[string]string{"show_id": "a1b2c3d4e5f60718"})
	poll := jobs.Job{ID: 11, Kind: PollKind, LibraryID: job.LibraryID, Path: &show, Params: params}
	pollCtx, stopPoll := context.WithCancel(context.Background())
	polled := make(chan error, 1)
	go func() {
		_, err := h.handler.PollJob().Run(pollCtx, poll, jobs.LocalReporter())
		polled <- err
	}()
	awaitTrue(t, "the poll looked at the folder under the show's lock", done, func() bool { return underLock.Load() >= 2 })
	// Its feed's host is not the harness's (and a poll holds the show's lock
	// through its feed's fetch): it has shown what it had to.
	stopPoll()
	<-polled
	slow.Store(false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	moved := "Renamed Show/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	if _, err := os.Stat(filepath.Join(h.mount, moved)); err != nil {
		t.Fatal("the episode was not published in the folder taken in", err)
	}
}

// A show folder renamed by hand again while a take-in waits for it (without
// the show's lock) is found again where it is now, not failed as a folder
// that may not be written: the fetch takes the new folder in and publishes
// there.
func TestATakeInFollowsAFolderRenamedWhileItWaits(t *testing.T) {
	requireProbe(t)
	h, job := audioHarness(t)
	s := h.handler.s
	s.o.CheckPoll = 2 * time.Millisecond
	audio := data(t, "chap-v24-reverse.mp3")
	setAudio(h, func(w http.ResponseWriter, _ *http.Request) { w.Write(audio) })
	var slow atomic.Bool
	var slowProbes atomic.Int32
	slow.Store(true)
	s.o.WriteProbe = func(dir string) error {
		if !slow.Load() || !strings.Contains(dir, "Renamed Show") {
			return mapping.ProbeWritable(dir)
		}
		slowProbes.Add(1)
		return &fs.PathError{Op: "probe", Path: dir, Err: fsutil.ErrStatTimeout}
	}
	renameShow(t, h, "Fixture Show", "Renamed Show")
	h.scan.knows = func(p string) bool { return strings.HasPrefix(p, "Fixture Show/") }
	done := make(chan error, 1)
	go func() {
		_, err := h.handler.FetchJob().Run(context.Background(), job, jobs.LocalReporter())
		done <- err
	}()
	awaitTrue(t, "the take-in waited on its folder", done, func() bool { return slowProbes.Load() >= 3 })
	renameShow(t, h, "Renamed Show", "Renamed Again")
	slow.Store(false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	moved := "Renamed Again/" + strings.TrimPrefix(*job.Path, "Fixture Show/")
	if _, err := os.Stat(filepath.Join(h.mount, moved)); err != nil {
		t.Fatal("the episode was not published in the folder it was renamed to", err)
	}
}

// Artwork is written under the show's lock its poll or follow holds, so its
// look at the folder never waits there: a probe the folder doesn't answer in
// time leaves the image for the next poll at once, counting no failure toward
// its backoff, and the next look that answers writes it. Denied: a probe the
// folder refuses still counts as a failed attempt.
func TestArtworkNeverWaitsForASlowFolder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	s := h.handler.s
	s.o.CheckPoll = time.Hour // a wait would outlast the test
	jpeg := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 64)...)
	s.o.Fetcher = feedFetcher(func(context.Context, netguard.Request) (*netguard.Response, error) {
		return &netguard.Response{Status: 200, Body: jpeg}, nil
	})
	var probe error = &fs.PathError{Op: "probe", Path: "Show", Err: fsutil.ErrStatTimeout}
	s.o.WriteProbe = func(string) error { return probe }
	root, err := os.OpenRoot(h.mount)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	root.Mkdir("Show", 0o755)
	lib, _ := s.o.Libraries.Library(3)
	f := &podcast.Feed{ShowID: "a1b2c3d4e5f60718"}
	const image = "https://art.example/cover"

	s.artwork(context.Background(), root, lib, h.mount, "Show", f, image)
	if state, err := h.db.PodcastArtwork(context.Background(), "server-1", 3, f.ShowID); err != nil || state.Failures != 0 || state.NextAttemptAt != "" {
		t.Fatal("a slow folder counted against the artwork", state, err)
	}
	if f.Show.Image != nil {
		t.Fatal("artwork recorded with no write", f.Show.Image)
	}

	probe = fs.ErrPermission
	s.artwork(context.Background(), root, lib, h.mount, "Show", f, image)
	if state, err := h.db.PodcastArtwork(context.Background(), "server-1", 3, f.ShowID); err != nil || state.Failures != 1 {
		t.Fatal("a refused folder was not counted", state, err)
	}
	if err := h.db.SavePodcastArtwork(context.Background(), "server-1", 3, f.ShowID, store.PodcastArtworkState{}); err != nil {
		t.Fatal(err)
	}

	s.o.WriteProbe = mapping.ProbeWritable
	s.artwork(context.Background(), root, lib, h.mount, "Show", f, image)
	if f.Show.Image == nil || f.Show.Image.File != "cover.jpg" {
		t.Fatal("the artwork was not written once the folder answered", f.Show.Image)
	}
	if _, err := os.Stat(filepath.Join(h.mount, "Show", "cover.jpg")); err != nil {
		t.Fatal(err)
	}
}
