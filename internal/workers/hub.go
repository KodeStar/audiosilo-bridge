// Package workers is the Bridge's side of its ASR workers (BRIDGE-PLAN.md Phase
// 1b): the paired workers and their tokens, a queue of segments with leases that
// the workers claim over the worker routes (internal/workerproto), and Router, the
// engine's ASR backend: remote when a worker is connected, else the Bridge's own
// ASR or a wait, by asr.worker_fallback.
//
// The queue lives in memory. The engine's work dir is the truth (a segment is
// done once its raw transcript is there), so a Bridge that restarts loses
// nothing: its ASR stage asks for the same segment again, under the same id, and
// a worker still decoding it, or holding its result, carries on.
package workers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/fsutil"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/transcript"
	"github.com/kodestar/audiosilo-bridge/internal/workerproto"
)

// ConnectedWindow is how long after its last call a worker counts as
// connected. An idle worker polls every MaxWaitSeconds and a busy one beats
// every HeartbeatEvery, so a live worker is never out of it.
const ConnectedWindow = 45 * time.Second

// goneFor is how long a finished or cancelled task's id is remembered, so a
// worker still beating it hears that it doesn't hold it (and stops) rather than
// "unknown", which means "the Bridge may be restarting, carry on".
const goneFor = 10 * time.Minute

// seenWriteEvery bounds the writes of a worker's last_seen_at.
const seenWriteEvery = time.Minute

// The hub answers the worker routes with workerproto's errors, and the engine
// with these. ErrNoWorker: a segment waited with no worker connected (none
// was, or the one decoding it went away); the Router falls back.
var (
	ErrNoWorker = errors.New("no ASR worker is connected")
	ErrNoName   = errors.New("a worker needs a name")
)

// Store is the hub's view of the database.
type Store interface {
	CreateWorker(ctx context.Context, name, tokenHash string) (*store.Worker, error)
	WorkerByTokenHash(ctx context.Context, tokenHash string) (*store.Worker, error)
	ListWorkers(ctx context.Context) ([]store.Worker, error)
	DeleteWorker(ctx context.Context, id int64) error
	SetWorkerInfo(ctx context.Context, id int64, info store.WorkerInfo, seen time.Time) error
	TouchWorker(ctx context.Context, id int64, seen time.Time) error
	AddWorkerSegment(ctx context.Context, id int64, audioSec, decodeSec float64) error
}

// Segment is one piece of audio to transcribe: a FLAC, where it starts in the
// book, how long it is, and the decode options.
type Segment struct {
	Audio         string
	Start         float64
	Duration      float64
	Language      string
	NoContext     bool
	InitialPrompt string
}

// Outcome is a segment a worker transcribed: the raw output and who made it.
type Outcome struct {
	Raw      []byte
	WorkerID int64
	Worker   string
	workerproto.ResultMeta
}

// task is a segment in the queue.
type task struct {
	id     string
	seg    Segment
	size   int64
	sha256 string
	// attempt counts the claims; holder is the worker holding it (0: queued)
	// until lease.
	attempt int
	holder  int64
	lease   time.Time
	done    chan struct{}
	out     *Outcome
	err     error
}

// live is a worker the hub has heard from since it started.
type live struct {
	name      string
	seen      time.Time
	seenSaved time.Time
	task      string
}

// Hub is the queue and the registry of connected workers.
type Hub struct {
	db  Store
	log *slog.Logger
	now func() time.Time

	// revokeMu orders a token's check against its worker's removal: a call
	// authenticated while its worker is removed can't bring it back.
	revokeMu sync.RWMutex

	mu    sync.Mutex
	tasks map[string]*task
	queue []string // queued task ids, oldest first
	gone  map[string]time.Time
	live  map[int64]*live
	// wake is closed (and replaced) when a task is queued, so a long poll
	// looks again.
	wake chan struct{}
	// connected is closed (and replaced) when a worker connects while none
	// was: a local decode yields to it, and waiting books go on.
	connected     chan struct{}
	bridgeVersion string
}

// Options configure a Hub.
type Options struct {
	DB            Store
	BridgeVersion string
	Log           *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time
}

// New makes a hub.
func New(o Options) *Hub {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Hub{db: o.DB, log: o.Log, now: o.Now, bridgeVersion: o.BridgeVersion,
		tasks: map[string]*task{}, gone: map[string]time.Time{}, live: map[int64]*live{},
		wake: make(chan struct{}), connected: make(chan struct{})}
}

// --- workers and tokens ---

// tokenPrefix marks a worker token (it is never a page session's).
const tokenPrefix = "asw_"

// MaxNameLen bounds a worker's name.
const MaxNameLen = 64

// CreateWorker pairs a new worker: the token is returned once and only its
// hash is kept.
func (h *Hub) CreateWorker(ctx context.Context, name string) (*store.Worker, string, error) {
	name = cleanText(name, MaxNameLen)
	if name == "" {
		return nil, "", ErrNoName
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, "", err
	}
	token := tokenPrefix + hex.EncodeToString(buf)
	w, err := h.db.CreateWorker(ctx, name, hashToken(token))
	if err != nil {
		return nil, "", err
	}
	return w, token, nil
}

// RevokeWorker deletes a worker: its token stops working at once and the
// segment it held goes back to the queue.
func (h *Hub) RevokeWorker(ctx context.Context, id int64) error {
	h.revokeMu.Lock()
	defer h.revokeMu.Unlock()
	if err := h.db.DeleteWorker(ctx, id); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.live, id)
	for _, t := range h.tasks {
		if t.holder == id {
			h.requeueLocked(t)
		}
	}
	return nil
}

// Authenticate resolves a worker token to its worker id.
func (h *Hub) Authenticate(ctx context.Context, token string) (int64, error) {
	if !strings.HasPrefix(token, tokenPrefix) {
		return 0, workerproto.ErrUnauthorized
	}
	h.revokeMu.RLock()
	defer h.revokeMu.RUnlock()
	w, err := h.db.WorkerByTokenHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return 0, workerproto.ErrUnauthorized
	}
	if err != nil {
		return 0, err
	}
	h.mu.Lock()
	h.liveLocked(w.ID).name = w.Name
	h.mu.Unlock()
	return w.ID, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Hello records what a worker runs and answers who it is.
func (h *Hub) Hello(ctx context.Context, id int64, hello workerproto.Hello) (workerproto.HelloReply, error) {
	info := store.WorkerInfo{Version: cleanText(hello.Version, workerproto.MaxTextBytes),
		Platform: cleanText(hello.Platform, workerproto.MaxTextBytes), Backend: cleanText(hello.Backend, workerproto.MaxTextBytes),
		Device: cleanText(hello.Device, workerproto.MaxTextBytes), Model: cleanText(hello.Model, workerproto.MaxTextBytes)}
	now := h.now()
	if err := h.db.SetWorkerInfo(ctx, id, info, now); err != nil {
		return workerproto.HelloReply{}, err
	}
	h.mu.Lock()
	l := h.live[id]
	if l == nil { // removed since the call was authenticated
		h.mu.Unlock()
		return workerproto.HelloReply{}, workerproto.ErrUnauthorized
	}
	l.seenSaved = now // SetWorkerInfo wrote last_seen_at
	name := l.name
	h.touchLocked(id, now)
	h.mu.Unlock()
	h.log.Info("bridge: an ASR worker connected", "worker", name, "backend", info.Backend, "device", info.Device,
		"version", info.Version, "platform", info.Platform)
	return workerproto.HelloReply{Protocol: workerproto.Version, BridgeVersion: h.bridgeVersion, WorkerID: id, Name: name}, nil
}

func (h *Hub) liveLocked(id int64) *live {
	l := h.live[id]
	if l == nil {
		l = &live{}
		h.live[id] = l
	}
	return l
}

// touchLocked records that worker id was heard from now, and closes connected
// when none was connected before. It reports false (and records nothing) for a
// worker removed since its call was authenticated: only Authenticate adds one.
func (h *Hub) touchLocked(id int64, now time.Time) bool {
	l := h.live[id]
	if l == nil {
		return false
	}
	was := h.connectedLocked(now)
	l.seen = now
	if now.Sub(l.seenSaved) >= seenWriteEvery {
		l.seenSaved = now
		go func() { _ = h.db.TouchWorker(context.Background(), id, now) }()
	}
	if !was {
		close(h.connected)
		h.connected = make(chan struct{})
	}
	return true
}

// connectedLocked reports a worker heard from within ConnectedWindow.
func (h *Hub) connectedLocked(now time.Time) bool {
	for _, l := range h.live {
		if !l.seen.IsZero() && now.Sub(l.seen) < ConnectedWindow {
			return true
		}
	}
	return false
}

// nameLocked is worker id's name ("" for one the hub doesn't know).
func (h *Hub) nameLocked(id int64) string {
	if l := h.live[id]; l != nil {
		return l.name
	}
	return ""
}

// Connected reports whether a worker is connected now.
func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connectedLocked(h.now())
}

// WhenConnected is closed when a worker connects while none is: a local
// decode yields its segment to the worker, and waiting books go on.
func (h *Hub) WhenConnected() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected
}

// View is a worker for the page and the heartbeat.
type View struct {
	store.Worker
	Connected bool `json:"connected"`
	// Task is the segment it decodes now ("" when idle).
	Task string `json:"task"`
}

// Workers lists the paired workers, with what the hub knows of them now.
func (h *Hub) Workers(ctx context.Context) ([]View, error) {
	ws, err := h.db.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	out := make([]View, 0, len(ws))
	for _, w := range ws {
		v := View{Worker: w}
		if l := h.live[w.ID]; l != nil && !l.seen.IsZero() {
			v.LastSeenAt = store.Timestamp(l.seen)
			v.Connected = now.Sub(l.seen) < ConnectedWindow
			if t := h.tasks[l.task]; t != nil && t.holder == w.ID {
				v.Task = t.id
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// --- the queue: the engine's side ---

// Run queues seg and waits for a worker's transcript. It returns ErrNoWorker
// as soon as the segment is waiting in the queue with no worker connected
// (none ever was, or the one decoding it stopped answering and its lease ran
// out), and ctx's error when ctx ends (the segment leaves the queue).
func (h *Hub) Run(ctx context.Context, seg Segment) (*Outcome, error) {
	size, sum, err := fsutil.FileSHA256(seg.Audio)
	if err != nil {
		return nil, err
	}
	t := &task{id: taskID(sum, seg), seg: seg, size: size, sha256: sum, done: make(chan struct{})}
	h.mu.Lock()
	if old := h.tasks[t.id]; old != nil {
		h.mu.Unlock()
		return nil, fmt.Errorf("segment %s is already queued", t.id)
	}
	delete(h.gone, t.id)
	h.tasks[t.id] = t
	h.requeueLocked(t)
	h.mu.Unlock()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return t.out, t.err
		case <-ctx.Done():
			h.drop(t, ctx.Err())
			return nil, ctx.Err()
		case <-tick.C:
		}
		h.mu.Lock()
		now := h.now()
		h.expireLocked(now)
		select {
		case <-t.done:
		default:
			if t.holder == 0 && !h.connectedLocked(now) {
				h.endLocked(t)
				h.mu.Unlock()
				return nil, ErrNoWorker
			}
		}
		h.mu.Unlock()
	}
}

// taskID is the segment's id: the same audio with the same options is the same
// task, across restarts of the Bridge.
func taskID(audioSHA string, seg Segment) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{audioSHA, seg.Language, fmt.Sprint(seg.NoContext), seg.InitialPrompt}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// releaseLocked clears t from its holder's record.
func (h *Hub) releaseLocked(t *task) {
	if l := h.live[t.holder]; l != nil && l.task == t.id {
		l.task = ""
	}
}

// requeueLocked puts t (back) at the end of the queue and wakes the polls.
func (h *Hub) requeueLocked(t *task) {
	h.releaseLocked(t)
	t.holder, t.lease = 0, time.Time{}
	h.queue = append(h.queue, t.id)
	close(h.wake)
	h.wake = make(chan struct{})
}

// expireLocked puts every task whose lease ran out back in the queue.
func (h *Hub) expireLocked(now time.Time) {
	for _, t := range h.tasks {
		if t.holder != 0 && now.After(t.lease) {
			h.log.Warn("bridge: an ASR worker stopped answering; its segment goes back in the queue", "worker", h.nameLocked(t.holder),
				"segment", t.id, "attempt", t.attempt)
			h.requeueLocked(t)
		}
	}
	for id, at := range h.gone {
		if now.Sub(at) > goneFor {
			delete(h.gone, id)
		}
	}
}

// endLocked takes t out of the hub (its id remembered as gone).
func (h *Hub) endLocked(t *task) {
	delete(h.tasks, t.id)
	h.queue = removeID(h.queue, t.id)
	h.releaseLocked(t)
	h.gone[t.id] = h.now()
}

// drop ends t with err (its waiter has gone).
func (h *Hub) drop(t *task, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tasks[t.id] == t {
		h.endLocked(t)
		t.err = err
	}
}

func removeID(ids []string, id string) []string {
	out := ids[:0]
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

// --- the queue: the workers' side ---

// Claim gives worker id the oldest queued segment, waiting up to wait for one
// (nil when none came).
func (h *Hub) Claim(ctx context.Context, id int64, wait time.Duration) (*workerproto.Task, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		now := h.now()
		if !h.touchLocked(id, now) {
			h.mu.Unlock()
			return nil, workerproto.ErrUnauthorized // revoked while it waited
		}
		h.expireLocked(now)
		if len(h.queue) > 0 {
			t := h.tasks[h.queue[0]]
			h.queue = h.queue[1:]
			t.attempt++
			t.holder, t.lease = id, now.Add(workerproto.LeaseDuration)
			l := h.live[id]
			l.task = t.id
			out, name := wireTask(t), l.name
			h.mu.Unlock()
			h.log.Info("bridge: an ASR worker took a segment", "worker", name, "segment", out.ID, "attempt", out.Attempt,
				"seconds", int(out.Duration))
			return &out, nil
		}
		wake := h.wake
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil
		case <-deadline.C:
			return nil, nil
		case <-wake:
		}
	}
}

func wireTask(t *task) workerproto.Task {
	return workerproto.Task{ID: t.id, Attempt: t.attempt, Start: t.seg.Start, Duration: t.seg.Duration, Size: t.size,
		SHA256: t.sha256, Language: t.seg.Language, NoContext: t.seg.NoContext, InitialPrompt: t.seg.InitialPrompt}
}

// Audio is the FLAC of a task worker id holds at attempt.
func (h *Hub) Audio(id int64, taskID string, attempt int) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.touchLocked(id, h.now()) {
		return "", workerproto.ErrUnauthorized
	}
	t, err := h.heldLocked(id, taskID, attempt)
	if err != nil {
		return "", err
	}
	return t.seg.Audio, nil
}

// heldLocked is the task worker id holds at attempt.
func (h *Hub) heldLocked(id int64, taskID string, attempt int) (*task, error) {
	t := h.tasks[taskID]
	switch {
	case t == nil:
		return nil, h.missingLocked(taskID)
	case t.holder != id || t.attempt != attempt:
		return nil, workerproto.ErrNotHeld
	}
	return t, nil
}

// missingLocked is the answer for a task the hub doesn't hold: not held when it
// ended lately (stop), unknown otherwise (the Bridge may be restarting).
func (h *Hub) missingLocked(id string) error {
	if _, ok := h.gone[id]; ok {
		return workerproto.ErrNotHeld
	}
	return workerproto.ErrUnknownTask
}

// Heartbeat renews worker id's lease on a task and answers the attempt it holds
// it under. A queued task (its lease ran out, or the Bridge restarted and asked
// for it again) is given back to the worker that is still decoding it.
func (h *Hub) Heartbeat(id int64, taskID string, attempt int) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if !h.touchLocked(id, now) {
		return 0, workerproto.ErrUnauthorized
	}
	t := h.tasks[taskID]
	if t != nil && t.holder == 0 {
		h.queue = removeID(h.queue, t.id)
		t.attempt++
		t.holder = id
		h.live[id].task = t.id
		h.log.Info("bridge: an ASR worker carries on with a segment it was decoding", "segment", t.id, "attempt", t.attempt)
	} else if _, err := h.heldLocked(id, taskID, attempt); err != nil {
		return 0, err
	}
	t.lease = now.Add(workerproto.LeaseDuration)
	return t.attempt, nil
}

// Result takes a worker's transcript for a task. It is taken whoever holds the
// task now, since the id names the same audio and options: a worker that
// finished while its lease ran out, or while the Bridge restarted, still
// counts. The raw output must be a complete transcript.
func (h *Hub) Result(ctx context.Context, id int64, taskID string, meta workerproto.ResultMeta, raw []byte) error {
	h.mu.Lock()
	if !h.touchLocked(id, h.now()) {
		h.mu.Unlock()
		return workerproto.ErrUnauthorized
	}
	t := h.tasks[taskID]
	if t == nil {
		err := h.missingLocked(taskID)
		h.mu.Unlock()
		return err
	}
	h.mu.Unlock()
	if !transcript.Complete(raw) {
		return workerproto.ErrInvalidResult
	}
	h.mu.Lock()
	if h.tasks[taskID] != t { // ended while the transcript was checked
		h.mu.Unlock()
		return workerproto.ErrNotHeld
	}
	name := h.nameLocked(id)
	meta.Backend, meta.Device, meta.Model = cleanText(meta.Backend, workerproto.MaxTextBytes),
		cleanText(meta.Device, workerproto.MaxTextBytes), cleanText(meta.Model, workerproto.MaxTextBytes)
	t.out = &Outcome{Raw: raw, WorkerID: id, Worker: name, ResultMeta: meta}
	h.endLocked(t)
	close(t.done)
	h.mu.Unlock()
	if err := h.db.AddWorkerSegment(ctx, id, t.seg.Duration, meta.DecodeSeconds); err != nil {
		h.log.Warn("bridge: counting a worker's segment failed", "err", err)
	}
	return nil
}

// Fail is a worker's report that it couldn't decode a task: the segment fails.
func (h *Hub) Fail(id int64, taskID string, f workerproto.Fail) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.touchLocked(id, h.now()) {
		return workerproto.ErrUnauthorized
	}
	t, err := h.heldLocked(id, taskID, f.Attempt)
	if err != nil {
		return err
	}
	t.err = fmt.Errorf("the ASR worker %q failed: %s", h.nameLocked(id), cleanText(f.Error, workerproto.MaxTextBytes))
	h.endLocked(t)
	close(t.done)
	return nil
}

// cleanText trims s, drops control characters and cuts it to max bytes (on a
// rune boundary).
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "")
	}
	return strings.TrimSpace(s)
}
