package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/workers"
)

// The ASR workers (Phase 1b) as the page and the heartbeat see them.

// waitingEvery is how often the books waiting for a worker are looked at. A
// worker connecting, a settings save and the engine's start look at once; this
// catches a book that parked in the moment one of them happened.
const waitingEvery = 30 * time.Second

// kickWaiting asks followWaiting to look now.
func (b *Bridge) kickWaiting() {
	select {
	case b.waitKick <- struct{}{}:
	default:
	}
}

// followWaiting re-admits the books parked waiting for a worker
// (asr_worker_waiting) whenever a segment can run (router.CanRun: a worker is
// connected, or the fallback is local with a usable backend), looking every
// `every`, when a worker connects and when kicked, until ctx ends.
func (b *Bridge) followWaiting(ctx context.Context, router *workers.Router, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.waitKick:
		case <-b.hub.WhenConnected():
		}
		e := b.engine.get()
		if e == nil || !router.CanRun() {
			continue
		}
		if n := e.sched.ReadmitParked(ctx, "speech recognition can run again", state.ParkASRWorkerWaiting); n > 0 {
			b.log.Info("bridge: books waiting for an ASR worker go on", "books", n)
		}
	}
}

// Workers lists the paired workers.
func (b *Bridge) Workers(ctx context.Context) ([]api.Worker, error) {
	vs, err := b.hub.Workers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.Worker, 0, len(vs))
	for _, v := range vs {
		out = append(out, api.Worker{Worker: v.Worker, Connected: v.Connected, Task: v.Task})
	}
	return out, nil
}

// CreateWorker pairs a worker; its token is in the answer only.
func (b *Bridge) CreateWorker(ctx context.Context, name string) (*api.NewWorker, error) {
	w, token, err := b.hub.CreateWorker(ctx, name)
	if errors.Is(err, workers.ErrNoName) {
		return nil, api.Errorf(api.ErrInvalid, "give the worker a name")
	}
	if err != nil {
		return nil, err
	}
	b.log.Info("bridge: an ASR worker was added", "worker", w.Name)
	b.Kick()
	return &api.NewWorker{Worker: api.Worker{Worker: *w}, Token: token}, nil
}

// RevokeWorker removes a worker, revoking its token.
func (b *Bridge) RevokeWorker(ctx context.Context, id int64) error {
	err := b.hub.RevokeWorker(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return api.Errorf(api.ErrNotFound, "no worker %d", id)
	}
	if err == nil {
		b.log.Info("bridge: an ASR worker was removed", "worker_id", id)
		b.Kick()
	}
	return err
}

// The server's bounds on the heartbeat's workers (its bridge.Status: 64 of
// them, 128 bytes a field, inside a 64 KiB body). More would refuse the whole
// heartbeat, so the most recently seen are sent, each field cut to fit.
const (
	maxLinkWorkers    = 64
	maxLinkWorkerText = 128
)

// linkWorkers are the workers for the server's console: each one that has
// connected, with its platform and speech recognition in one line.
func (b *Bridge) linkWorkers(ctx context.Context) []link.Worker {
	vs, err := b.hub.Workers(ctx)
	if err != nil {
		b.log.Warn("bridge: listing the ASR workers failed", "err", err)
		return []link.Worker{}
	}
	out := []link.Worker{}
	for _, v := range vs {
		if v.LastSeenAt == "" {
			continue
		}
		asrPart := v.Backend
		if v.Device != "" {
			asrPart += " (" + v.Device + ")"
		}
		parts := slices.DeleteFunc([]string{v.Platform, strings.TrimSpace(asrPart)}, func(p string) bool { return p == "" })
		platform := strings.Join(parts, ", ")
		out = append(out, link.Worker{Name: clipText(v.Name), Version: clipText(v.Version), Platform: clipText(platform),
			LastSeen: v.LastSeenAt})
	}
	if len(out) > maxLinkWorkers {
		// Timestamps are fixed-width UTC, so they sort as strings.
		slices.SortStableFunc(out, func(a, b link.Worker) int { return strings.Compare(b.LastSeen, a.LastSeen) })
		out = out[:maxLinkWorkers]
	}
	return out
}

// clipText cuts s to maxLinkWorkerText bytes on a character's boundary.
func clipText(s string) string {
	if len(s) <= maxLinkWorkerText {
		return s
	}
	return strings.ToValidUTF8(s[:maxLinkWorkerText], "")
}
