package workers

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/kodestar/audiosilo-bridge/internal/asr"
	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
)

// IDWorker is the Router's backend id while it sends segments to a worker.
const IDWorker = "worker"

// The park messages of a book waiting for a worker: the fallback is wait, or
// the Bridge has no speech recognition of its own to fall back on.
const (
	WaitingMsg = "speech recognition waits for an ASR worker (the ASR fallback is wait); " +
		"the book goes on by itself once one connects"
	noLocalMsg = "no worker is connected and the Bridge has no speech recognition of its own"
)

// Router is the engine's ASR backend: a segment goes to a connected worker,
// else to the Bridge's own ASR (Local: whisper.cpp on the CPU in the full
// image, CUDA in cuda, mlx-whisper on a native Mac) or, when Wait says so or
// there is no Local, a wait (the book parks asr_worker_waiting until a worker
// connects). Wait is read for every segment, so a change on the page applies to
// the next one. A local decode yields to a worker that connects while it runs:
// the worker is many times faster than a CPU, so the segment starts again
// there.
type Router struct {
	Hub *Hub
	// Local is the Bridge's own backend and LocalCap its capability (Local
	// nil: none).
	Local    asr.Backend
	LocalCap asr.Capability
	Wait     func() bool
	Log      *slog.Logger

	readyMu    sync.Mutex
	localReady bool
}

var _ asr.Backend = (*Router)(nil)

// local reports whether a segment would run on the Bridge's own backend now.
func (r *Router) local() bool {
	return !r.Hub.Connected() && !r.Wait() && r.Local != nil && r.LocalCap.Available
}

// CanRun reports whether a segment would run now (on a connected worker or the
// local backend) rather than wait: the books waiting for a worker can go on.
func (r *Router) CanRun() bool { return r.Hub.Connected() || r.local() }

// ID is the route a segment takes now: the local backend's id, else the worker.
func (r *Router) ID() string {
	if r.local() {
		return r.Local.ID()
	}
	return IDWorker
}

// Detect reports what a segment would run on now: the local backend, or a
// worker (connected, or waited for: Detail says so). Always available: a book
// with nowhere to transcribe waits for a worker rather than failing.
func (r *Router) Detect(ctx context.Context) (asr.Capability, error) {
	switch {
	case r.local():
		return r.LocalCap, nil
	case r.Hub.Connected():
		return asr.Capability{Backend: IDWorker, Available: true, Device: "remote"}, nil
	case r.Wait():
		return asr.Capability{Backend: IDWorker, Available: true, Device: "remote", Detail: "no worker connected; segments wait for one"}, nil
	}
	return asr.Capability{Backend: IDWorker, Available: true, Device: "remote", Detail: noLocalMsg + "; segments wait for a worker"}, nil
}

// EnsureReady prepares the local backend when segments would run on it now
// (a worker prepares its own).
func (r *Router) EnsureReady(ctx context.Context) error {
	if !r.local() {
		return nil
	}
	return r.ensureLocal(ctx)
}

func (r *Router) ensureLocal(ctx context.Context) error {
	r.readyMu.Lock()
	defer r.readyMu.Unlock()
	if r.localReady {
		return nil
	}
	if err := r.Local.EnsureReady(ctx); err != nil {
		return err
	}
	r.localReady = true
	return nil
}

// Transcribe runs one segment, writing the raw output where the local backend
// would (Job.OutDir/asr.RawOutputName(Job.Audio)).
func (r *Router) Transcribe(ctx context.Context, job asr.Job) error {
	seg := Segment{Audio: job.Audio, Language: job.Language, NoContext: job.NoContext, InitialPrompt: job.InitialPrompt,
		Start: job.Start, Duration: job.Duration}
	for {
		if r.Hub.Connected() {
			out, err := r.Hub.Run(ctx, seg)
			if err == nil {
				r.Log.Info("bridge: a worker transcribed a segment", "worker", out.Worker, "segment", filepath.Base(job.Audio),
					"backend", out.Backend, "device", out.Device, "decode_seconds", int(out.DecodeSeconds))
				return writeRaw(job, out.Raw)
			}
			if !errors.Is(err, ErrNoWorker) {
				return err
			}
			r.Log.Warn("bridge: no ASR worker is connected; the segment falls back", "segment", filepath.Base(job.Audio),
				"wait", r.Wait())
		}
		if r.Wait() {
			return scheduler.ParkWithCode(state.ParkASRWorkerWaiting, WaitingMsg)
		}
		if r.Local == nil || !r.LocalCap.Available {
			msg := noLocalMsg
			if r.Local != nil && r.LocalCap.Detail != "" {
				msg += " (" + r.LocalCap.Detail + ")"
			}
			return scheduler.ParkWithCode(state.ParkASRWorkerWaiting, msg+"; the book goes on by itself once a worker connects")
		}
		yielded, err := r.decodeLocal(ctx, job)
		if !yielded {
			return err
		}
		r.Log.Info("bridge: a worker connected; the segment moves to it", "segment", filepath.Base(job.Audio))
	}
}

// decodeLocal decodes job on the Bridge's own backend. It stops (yielded) when
// a worker connects meanwhile.
func (r *Router) decodeLocal(ctx context.Context, job asr.Job) (yielded bool, err error) {
	// Take the edge before looking at the state: a worker that connected since
	// the caller looked (or while the local backend is prepared, which may
	// download a model) would otherwise close the channel before it is taken,
	// and the whole segment would decode here.
	connected := r.Hub.WhenConnected()
	if r.Hub.Connected() {
		return true, nil
	}
	if err := r.ensureLocal(ctx); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, scheduler.ParkWithCode(state.ParkASRUnavailable, "ASR setup failed ("+r.Local.ID()+"): "+err.Error()+" - fix this, then retry")
	}
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Local.Transcribe(lctx, job) }()
	select {
	case err := <-done:
		return false, err
	case <-connected:
		cancel()
		<-done
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		_ = os.Remove(filepath.Join(job.OutDir, asr.RawOutputName(job.Audio)))
		return true, nil
	}
}

// writeRaw writes a worker's raw output where a local decode of job would
// have.
func writeRaw(job asr.Job, raw []byte) error {
	return fsutil.WriteFileAtomic(filepath.Join(job.OutDir, asr.RawOutputName(job.Audio)), raw, 0o640)
}
