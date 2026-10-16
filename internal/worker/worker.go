// Package worker is the ASR worker (BRIDGE-PLAN.md Phase 1b): `audiosilo-bridge
// worker --bridge <url> --token <t>`, run natively where the speech
// recognition is fast (a Mac's Metal, a GPU box), because Docker on macOS has
// no Metal. It connects out to the Bridge (never the reverse), long-polls for
// segments, downloads each one's FLAC, transcribes it with the engine's own
// internal/asr backends and uploads the raw transcript. It needs no library and
// no path mapping: the Bridge sends the audio.
//
// It resumes per segment. The spool (<data>/spool) holds the segment in hand
// (current.json and its FLAC) and every transcript not yet taken by the Bridge
// (results/): a worker that restarts first offers those again, then carries on
// with the segment it was decoding (the Bridge gives it back while the lease
// holds), so a kill costs at most the one decode it interrupted.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/asr"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/workerproto"
)

// Options run a worker.
type Options struct {
	// BridgeURL is the Bridge page's address; Token the worker token made on
	// that page.
	BridgeURL string
	Token     string
	// DataDir holds the speech recognition's tools (the mlx venv, the whisper
	// models) and the spool.
	DataDir string
	Version string
	// ASR chooses the backend (its DataDir is set from DataDir).
	ASR asr.SelectConfig
	Log *slog.Logger
	// HTTP is the client (tests); nil for a default one.
	HTTP *http.Client
	// Backend replaces asr.Select (tests).
	Backend asr.Backend
	// HeartbeatEvery is how often a decode's lease is renewed (0:
	// workerproto.HeartbeatEvery; tests).
	HeartbeatEvery time.Duration
}

// How long to wait before calling the Bridge again after each kind of answer.
var (
	retryRefused  = 5 * time.Minute  // the token: someone has to make a new one
	retryProtocol = 10 * time.Minute // the version: someone has to update one side
	retrySetUp    = 5 * time.Minute  // the speech recognition: someone has to install something
	retryError    = 5 * time.Second  // the Bridge can't be reached (yet)
	retryErrorMax = time.Minute
)

// unknownGrace is how long a task the Bridge doesn't know is still worked on
// (the decode), and its transcript kept (the result): a Bridge that restarts
// asks for the same segment again, under the same id, within a minute or two.
var unknownGrace = 30 * time.Minute

// Run works until ctx ends.
func Run(ctx context.Context, o Options) error {
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.HeartbeatEvery <= 0 {
		o.HeartbeatEvery = workerproto.HeartbeatEvery
	}
	if strings.TrimSpace(o.Token) == "" {
		return errors.New("no worker token: make one on the Bridge's page (Workers), then pass --token or run worker install")
	}
	c, err := newClient(o.BridgeURL, strings.TrimSpace(o.Token), o.Version, o.HTTP)
	if err != nil {
		return err
	}
	sp, err := openSpool(filepath.Join(o.DataDir, "spool"))
	if err != nil {
		return err
	}
	w := &worker{o: o, c: c, sp: sp, log: o.Log}
	// A speech recognition that can't be set up (no python3 or ffmpeg, a model
	// download that failed) is retried here rather than exiting, which an
	// installed service would answer with a restart loop.
	for {
		err := w.setUp(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		w.log.Error("worker: the speech recognition can't be set up; fix it and the worker carries on by itself",
			"err", err, "retry_in", retrySetUp)
		w.wait(ctx, retrySetUp)
	}
	return w.loop(ctx)
}

type worker struct {
	o       Options
	c       *client
	sp      *spool
	log     *slog.Logger
	backend asr.Backend
	cap     asr.Capability
	model   string
}

// setUp chooses and prepares the speech recognition (the first start may build
// the mlx venv or download a model).
func (w *worker) setUp(ctx context.Context) error {
	sel := w.o.ASR
	sel.DataDir, sel.Log = w.o.DataDir, w.log
	b := w.o.Backend
	if b == nil {
		var err error
		if b, _, err = asr.Select(ctx, sel); err != nil {
			return err
		}
	}
	c, _ := b.Detect(ctx)
	if !c.Available {
		return fmt.Errorf("speech recognition isn't available here (%s): %s", c.Backend, c.Detail)
	}
	if c.Backend == asr.IDMLXWhisper && w.o.Backend == nil {
		// mlx-whisper reads the audio through ffmpeg; without it every segment
		// would fail.
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			return errors.New("mlx-whisper needs ffmpeg on PATH (brew install ffmpeg), then start the worker again")
		}
	}
	w.log.Info("worker: preparing the speech recognition (the first start may take a few minutes)", "backend", c.Backend, "device", c.Device)
	if err := b.EnsureReady(ctx); err != nil {
		return fmt.Errorf("prepare %s: %w", c.Backend, err)
	}
	w.backend, w.cap = b, c
	w.model = sel.Model
	if w.model == "" {
		w.model = asr.DefaultModelFor(c.Backend)
	}
	return nil
}

func (w *worker) hello() workerproto.Hello {
	return workerproto.Hello{Protocol: workerproto.Version, Version: w.o.Version, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Backend: w.cap.Backend, Device: w.cap.Device, Model: w.model}
}

// loop says hello, then works: the transcripts the Bridge hasn't taken, the
// segment in hand, then new ones.
func (w *worker) loop(ctx context.Context) error {
	backoff := retryError
	greeted := false
	// retry waits out err; a refusal or a version change says hello again.
	retry := func(err error) {
		greeted = greeted && !needsHello(err)
		w.wait(ctx, w.pause(ctx, err, &backoff))
	}
	for ctx.Err() == nil {
		if !greeted {
			reply, err := w.c.hello(ctx, w.hello())
			if err != nil {
				retry(err)
				continue
			}
			greeted, backoff = true, retryError
			w.log.Info("worker: connected to the Bridge", "bridge", w.o.BridgeURL, "name", reply.Name,
				"bridge_version", reply.BridgeVersion, "backend", w.cap.Backend, "device", w.cap.Device, "model", w.model)
		}
		w.sendResults(ctx)
		if t := w.sp.current(); t != nil {
			if err := w.resume(ctx, *t); err != nil {
				retry(err)
			}
			continue
		}
		t, err := w.c.next(ctx)
		switch {
		case err != nil:
			retry(err)
			continue
		case t == nil:
			continue
		}
		backoff = retryError
		if err := w.work(ctx, *t); err != nil {
			retry(err)
		}
	}
	return nil
}

// needsHello reports an error after which the worker says hello again (a
// refusal or a version change: the hello says why).
func needsHello(err error) bool {
	return errors.Is(err, workerproto.ErrUnauthorized) || errors.Is(err, ErrProtocol)
}

// notOurs reports an answer that means the segment isn't this worker's (any
// more, or the Bridge doesn't know it): drop it.
func notOurs(err error) bool {
	return errors.Is(err, workerproto.ErrNotHeld) || errors.Is(err, workerproto.ErrUnknownTask)
}

// pause logs err and says how long to wait before trying again (nothing to
// say when the worker is stopping: the error is the stop's).
func (w *worker) pause(ctx context.Context, err error, backoff *time.Duration) time.Duration {
	switch {
	case ctx.Err() != nil:
		return 0
	case errors.Is(err, workerproto.ErrUnauthorized):
		w.log.Error("worker: the Bridge refused the token; make a new one on the Bridge's page (Workers) and run worker install again",
			"retry_in", retryRefused)
		return retryRefused
	case errors.Is(err, ErrProtocol):
		w.log.Error("worker: the Bridge speaks another worker protocol; update the worker or the Bridge so both are the same version",
			"worker_protocol", workerproto.Version, "bridge_protocol", w.c.bridgeV, "retry_in", retryProtocol)
		return retryProtocol
	}
	d := *backoff
	*backoff = min(*backoff*2, retryErrorMax)
	w.log.Warn("worker: can't reach the Bridge", "err", err, "retry_in", d)
	return d
}

func (w *worker) wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// resume carries on with the segment in hand after a restart: the Bridge gives
// it back while the lease holds (or it was queued again, unclaimed).
func (w *worker) resume(ctx context.Context, t workerproto.Task) error {
	attempt, err := w.c.heartbeat(ctx, t.ID, t.Attempt)
	switch {
	case notOurs(err):
		w.log.Info("worker: the segment in hand isn't ours any more; dropping it", "segment", t.ID)
		w.sp.drop(t.ID)
		return nil
	case err != nil:
		return err
	}
	t.Attempt = attempt
	w.log.Info("worker: carrying on with the segment in hand", "segment", t.ID, "attempt", t.Attempt)
	return w.work(ctx, t)
}

// work downloads, decodes and uploads one segment.
func (w *worker) work(ctx context.Context, t workerproto.Task) error {
	if err := w.sp.hold(t); err != nil {
		return err
	}
	// The lease is renewed from the claim on, through the download as well as
	// the decode (a long chapter's FLAC on a slow link takes longer than the
	// lease); both stop when the Bridge says the segment isn't ours any more.
	dctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	var held atomic.Int64
	held.Store(int64(t.Attempt))
	leaseDone := make(chan struct{})
	go func() { defer close(leaseDone); w.keepLease(dctx, stop, t, &held) }()

	flac := w.sp.flac(t.ID)
	if !w.sp.hasAudio(t) {
		start := time.Now()
		if err := w.c.download(dctx, t, flac); err != nil {
			cause := context.Cause(dctx)
			stop(nil)
			<-leaseDone
			if notOurs(err) || errors.Is(cause, workerproto.ErrNotHeld) {
				w.sp.drop(t.ID)
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err() // a stop: the segment stays in hand for the next start
			}
			return err
		}
		secs := time.Since(start).Seconds()
		w.log.Info("worker: got a segment", "segment", t.ID, "attempt", held.Load(), "audio_seconds", int(t.Duration),
			"mb", fmt.Sprintf("%.1f", float64(t.Size)/1e6), "mb_per_s", fmt.Sprintf("%.0f", float64(t.Size)/1e6/max(secs, 0.001)))
	}

	start := time.Now()
	outDir := w.sp.rawDir()
	err := w.backend.Transcribe(dctx, asr.Job{Audio: flac, OutDir: outDir, Language: t.Language, NoContext: t.NoContext,
		InitialPrompt: t.InitialPrompt, Start: t.Start, Duration: t.Duration})
	decode := time.Since(start).Seconds()
	cause := context.Cause(dctx)
	stop(nil)
	<-leaseDone
	t.Attempt = int(held.Load())
	if errors.Is(cause, workerproto.ErrNotHeld) {
		w.log.Info("worker: the Bridge took the segment back; stopping it", "segment", t.ID)
		w.sp.drop(t.ID)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err() // a stop: the segment stays in hand for the next start
	}
	rawPath := filepath.Join(outDir, asr.RawOutputName(flac))
	if err != nil {
		_ = os.Remove(rawPath)
		w.log.Error("worker: the decode failed", "segment", t.ID, "err", err)
		ferr := w.c.fail(ctx, t.ID, workerproto.Fail{Attempt: t.Attempt, Error: err.Error()})
		w.sp.drop(t.ID)
		if ferr != nil && !notOurs(ferr) {
			return ferr
		}
		return nil
	}
	meta := workerproto.ResultMeta{Attempt: t.Attempt, Backend: w.cap.Backend, Device: w.cap.Device, Model: w.model,
		DecodeSeconds: decode}
	if err := w.sp.keepResult(t.ID, meta, rawPath); err != nil {
		return err
	}
	w.sp.drop(t.ID) // the segment in hand is done; its result is in results/
	w.log.Info("worker: transcribed a segment", "segment", t.ID, "audio_seconds", int(t.Duration),
		"decode_seconds", int(decode), "speed", fmt.Sprintf("%.1fx", t.Duration/max(decode, 0.001)))
	w.sendResults(ctx)
	return nil
}

// keepLease beats every Options.HeartbeatEvery until ctx ends, and stops the decode
// (cause ErrNotHeld) once the Bridge says the segment isn't ours (or refuses the
// token: revoking a worker gives its segment back). A Bridge that
// can't be reached, or doesn't know the task (it may be restarting), is
// tolerated for unknownGrace. The attempt the Bridge answers goes to held.
func (w *worker) keepLease(ctx context.Context, stop context.CancelCauseFunc, t workerproto.Task, held *atomic.Int64) {
	tick := time.NewTicker(w.o.HeartbeatEvery)
	defer tick.Stop()
	var lostSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		attempt, err := w.c.heartbeat(ctx, t.ID, t.Attempt)
		switch {
		case err == nil:
			lostSince = time.Time{}
			if attempt != t.Attempt {
				t.Attempt = attempt
				_ = w.sp.hold(t)
				held.Store(int64(attempt))
			}
		case errors.Is(err, workerproto.ErrNotHeld), errors.Is(err, workerproto.ErrUnauthorized):
			// Not ours, or the token was revoked: the Bridge gave the segment
			// to another worker (or its own ASR) and will never take this one.
			stop(workerproto.ErrNotHeld)
			return
		case ctx.Err() != nil:
			return
		default:
			if lostSince.IsZero() {
				lostSince = time.Now()
			} else if time.Since(lostSince) > unknownGrace {
				stop(workerproto.ErrNotHeld)
				return
			}
		}
	}
}

// sendResults offers every kept transcript to the Bridge. One it took, or
// refused, or one older than unknownGrace that it doesn't know, is deleted; the
// rest are kept for the next round.
func (w *worker) sendResults(ctx context.Context) {
	for _, r := range w.sp.results() {
		raw, err := os.ReadFile(w.sp.result(r.id))
		if err != nil {
			w.sp.dropResult(r.id)
			continue
		}
		err = w.c.result(ctx, r.id, r.meta, raw)
		switch {
		case err == nil:
			w.log.Info("worker: the Bridge took a transcript", "segment", r.id)
			w.sp.dropResult(r.id)
		case errors.Is(err, workerproto.ErrNotHeld), errors.Is(err, workerproto.ErrInvalidResult):
			w.log.Warn("worker: the Bridge didn't take a transcript; dropping it", "segment", r.id, "err", err)
			w.sp.dropResult(r.id)
		case errors.Is(err, workerproto.ErrUnknownTask) && time.Since(r.at) > unknownGrace:
			w.sp.dropResult(r.id)
		case ctx.Err() != nil:
			return
		default:
			w.log.Warn("worker: a transcript waits to be sent", "segment", r.id, "err", err)
		}
	}
}

// --- the spool ---

type spool struct{ dir string }

func openSpool(dir string) (*spool, error) {
	for _, d := range []string{dir, filepath.Join(dir, "results"), filepath.Join(dir, "raw")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &spool{dir: dir}, nil
}

func (s *spool) currentPath() string     { return filepath.Join(s.dir, "current.json") }
func (s *spool) flac(id string) string   { return filepath.Join(s.dir, id+".flac") }
func (s *spool) rawDir() string          { return filepath.Join(s.dir, "raw") }
func (s *spool) result(id string) string { return filepath.Join(s.dir, "results", id+".json") }
func (s *spool) meta(id string) string   { return filepath.Join(s.dir, "results", id+".meta.json") }

// hold records t as the segment in hand.
func (s *spool) hold(t workerproto.Task) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.currentPath(), b, 0o600)
}

// current is the segment in hand (nil for none).
func (s *spool) current() *workerproto.Task {
	b, err := os.ReadFile(s.currentPath())
	if err != nil {
		return nil
	}
	var t workerproto.Task
	if json.Unmarshal(b, &t) != nil || t.ID == "" || strings.ContainsAny(t.ID, `/\.`) {
		_ = os.Remove(s.currentPath())
		return nil
	}
	return &t
}

// hasAudio reports whether the segment's FLAC is already in the spool whole
// (download renames it into place only once its size and sha256 matched).
func (s *spool) hasAudio(t workerproto.Task) bool {
	info, err := os.Stat(s.flac(t.ID))
	return err == nil && info.Size() == t.Size
}

// drop forgets the segment in hand (and its audio).
func (s *spool) drop(id string) {
	_ = os.Remove(s.flac(id))
	_ = os.Remove(s.flac(id) + ".part")
	if t := s.current(); t != nil && t.ID == id {
		_ = os.Remove(s.currentPath())
	}
}

type keptResult struct {
	id   string
	meta workerproto.ResultMeta
	at   time.Time
}

// keepResult keeps the transcript the backend wrote at rawPath until the
// Bridge takes it: its meta first, then the transcript renamed in (the spool is
// one folder tree), so a kept transcript always has its meta.
func (s *spool) keepResult(id string, meta workerproto.ResultMeta, rawPath string) error {
	m, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(s.meta(id), m, 0o600); err != nil {
		return err
	}
	return os.Rename(rawPath, s.result(id))
}

// results are the kept transcripts (their meta; the transcript is read when
// it is sent).
func (s *spool) results() []keptResult {
	paths, _ := filepath.Glob(filepath.Join(s.dir, "results", "*.meta.json"))
	var out []keptResult
	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), ".meta.json")
		mb, err := os.ReadFile(p) //nolint:gosec // the worker's own spool
		info, serr := os.Stat(p)
		var meta workerproto.ResultMeta
		if err != nil || serr != nil || json.Unmarshal(mb, &meta) != nil {
			s.dropResult(id)
			continue
		}
		out = append(out, keptResult{id: id, meta: meta, at: info.ModTime()})
	}
	return out
}

func (s *spool) dropResult(id string) {
	_ = os.Remove(s.result(id))
	_ = os.Remove(s.meta(id))
}
