package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/agent"
	"github.com/kodestar/audiosilo-bridge/internal/agentbox"
	"github.com/kodestar/audiosilo-bridge/internal/asr"
	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/events"
	"github.com/kodestar/audiosilo-bridge/internal/generate"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/metaops"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
	"github.com/kodestar/audiosilo-bridge/internal/supervisor"
	"github.com/kodestar/audiosilo-bridge/internal/toolfetch"
)

// The engine (the sidecars pipeline imported from audiosilo-sidecars), wired the
// way sidecars' daemon built it (its internal/server.Run), minus the folder scan:
// the event hub with its durable persister, the media tools, the ASR backend, the
// agent runner (ONLY the agent container's, decision 15), the composite
// executor, the scheduler, the supervisor and the contribution poller. It starts
// in the background once the Bridge runs (the first start may download ffmpeg),
// and the sidecars.generate job waits for it.

// eventRetention is how long the durable event log is kept.
const eventRetention = 30 * 24 * time.Hour

// eventQueueSize bounds the durable-event persister's backlog.
const eventQueueSize = 1024

// engine is the running engine, nil until it has started.
type engine struct {
	sched *scheduler.Scheduler
	exec  *pipeline.Executor
}

// engineState holds the engine once it is up.
type engineState struct {
	cur atomic.Pointer[engine]
}

func (s *engineState) get() *engine { return s.cur.Load() }

// generateEngine is the scheduler for the sidecars.generate job (nil until it
// started: never a typed nil inside the interface).
func (s *engineState) generateEngine() generate.Engine {
	if e := s.get(); e != nil {
		return e.sched
	}
	return nil
}

// agentBackend is the CLI the agent container runs (claude unless codex is set).
func agentBackend(cfg config.Config) string {
	if cfg.Agent.Backend == config.AgentBackendCodex {
		return config.AgentBackendCodex
	}
	return config.AgentBackendClaude
}

// runEngine builds and runs the engine until ctx ends, then waits for every part
// of it to stop (no stage writes to the store once it returns).
func (b *Bridge) runEngine(ctx context.Context) {
	cfg := b.config()
	log := b.log

	hub := events.NewHub(events.DefaultRingSize)
	persister := newEventPersister(b.db, log, eventQueueSize)
	persister.start()
	hub.SetPersister(persister.enqueue)
	defer persister.Close()

	tools := toolfetch.Resolve(ctx, toolfetch.ResolveConfig{FFmpegPath: cfg.Tools.FFmpegPath, FFprobePath: cfg.Tools.FFprobePath,
		AutoDownload: cfg.Tools.AutoDownload}, filepath.Join(b.dataDir, "tools"), log)
	if ctx.Err() != nil {
		return
	}
	asrSelect := asr.SelectConfig{Backend: cfg.ASR.Backend, Model: cfg.ASR.Model, Language: cfg.ASR.Language,
		WhisperCLIPath: cfg.ASR.WhisperCLIPath, AutoDownload: cfg.Tools.AutoDownload, DataDir: b.dataDir, Log: log}
	asrBackend, asrCap, err := asr.Select(ctx, asrSelect)
	if err != nil {
		// A misconfiguration (an explicit whisper-cli that doesn't resolve): the
		// engine still runs, and a book's asr stage parks asr_unavailable saying so.
		log.Error("bridge: the ASR backend can't be used", "err", err)
		asrBackend, asrCap = nil, asr.Capability{Backend: cfg.ASR.Backend, Detail: err.Error()}
	}
	asrModel := cfg.ASR.Model
	if asrModel == "" {
		asrModel = asr.DefaultModelFor(asrCap.Backend)
	}

	// The agent container's slots are the agent concurrency: a book in an agent
	// stage waits for a slot rather than piling CLI runs onto one container.
	slots := max(b.box.Status().Slots, 1)
	box := b.box
	redetect := func(ctx context.Context) (agent.Runner, agent.Availability) { return box, box.Detect(ctx) }
	metaClient := metaops.NewClient(cfg.Metadata.BaseURL)
	tokens := contrib.NewTokenSource(b.vault)
	workRoot := filepath.Join(b.dataDir, "work")
	exec := pipeline.NewExecutor(pipeline.Config{
		DB: b.db, FFmpeg: tools.FFmpeg, FFprobe: tools.FFprobe, DataDir: b.dataDir,
		Tools:     pipeline.ToolConfig{FFmpegPath: cfg.Tools.FFmpegPath, FFprobePath: cfg.Tools.FFprobePath},
		ASR:       pipeline.ASRSetup{Backend: asrBackend, Cap: asrCap, Model: asrModel, Language: cfg.ASR.Language},
		ASRSelect: asrSelect,
		Agent:     box, AgentAvail: box.Detect(ctx), AgentRedetect: redetect,
		AgentModels:      pipeline.AgentModels{Claude: cfg.Agent.Claude, OpenAI: cfg.Agent.OpenAI},
		AgentTimeout:     time.Duration(cfg.Agent.TimeoutMinutes) * time.Minute,
		AgentConcurrency: slots, MaxAgentsPerBook: slots,
		BookBudgetUSD: cfg.Agent.BookBudgetUSD, MonthlyBudgetUSD: cfg.Agent.MonthlyBudgetUSD,
		Pricing: cfg.Pricing, Secrets: b.vault, Log: log,
		Fallback: scheduler.NewStubExecutor(0, 0),
		Meta:     metaClient, TokenSource: tokens,
		ContribMode: cfg.Contribution.Mode, ContribCommunityRepo: cfg.Contribution.CommunityRepo,
		ContribBaseURL: cfg.Contribution.APIBaseURL, ExportRoot: filepath.Join(b.dataDir, "export"),
	})
	sched := scheduler.New(b.db, hub, exec, slots, workRoot, cfg.Contribution.AutoPurge)

	// The supervisor's deterministic lane only: config normalization keeps the
	// model-assisted lane and the backend failover off (decision 15).
	sup := supervisor.New(b.db, cfg.Supervisor, cfg.Pricing, nil, supervisor.Hooks{
		Runtime: func(books []store.Book) supervisor.Runtime {
			r := sched.SupervisorRuntime(books)
			return supervisor.Runtime{ActiveBooks: r.ActiveBooks, AgentActive: r.AgentActive, AgentCapacity: r.AgentCapacity,
				EligibleAgentBooks: r.EligibleAgentBooks, EligibleAgentIDs: r.EligibleAgentIDs, AgentInvocations: r.AgentInvocations,
				InvocationCapacity: r.InvocationCapacity, InvocationsByBook: r.InvocationsByBook, MaxAgentsPerBook: r.MaxAgentsPerBook}
		},
		Apply: supervisorApply(b.db, sched),
		Publish: func(eventType string, bookID int64, payload any) {
			if bookID > 0 {
				_ = hub.PublishBook(eventType, bookID, payload)
			} else {
				_ = hub.Publish(eventType, payload)
			}
		},
	})
	contribSvc := contrib.NewService(contrib.ServiceDeps{
		DB: b.db, CoreRepo: cfg.Contribution.CoreRepo, BaseURL: cfg.Contribution.APIBaseURL, Tokens: tokens,
		Publish: func(u contrib.ContribUpdate) { _ = hub.PublishBook("contrib.update", u.BookID, u) },
		Readmit: sched.Retry,
		ResolveWork: func(ctx context.Context, workID string) (string, error) {
			cov, err := metaClient.FreshCoverageForWork(ctx, workID)
			switch {
			case err == nil:
				return cov.WorkID, nil
			case errors.Is(err, metaops.ErrDisabled):
				return workID, nil
			case errors.Is(err, metaops.ErrWorkNotFound):
				return "", contrib.ErrWorkNotFound
			default:
				return "", err
			}
		},
		CorePendingMsg: pipeline.CorePendingMsg, Log: log,
	})

	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		if err := sched.Start(ctx); err != nil && ctx.Err() == nil {
			log.Error("bridge: the engine's scheduler stopped", "err", err)
		}
	}()
	go func() { defer wg.Done(); sup.Run(ctx) }()
	go func() {
		defer wg.Done()
		contribSvc.RunPoller(ctx, time.Duration(cfg.Contribution.PollMinutes)*time.Minute)
	}()
	go func() { defer wg.Done(); b.daily(ctx, b.pruneEvents) }()
	go func() { defer wg.Done(); b.daily(ctx, b.sweepTranscripts) }()
	b.engine.cur.Store(&engine{sched: sched, exec: exec})
	log.Info("bridge: the engine started", "ffmpeg", tools.FFmpeg != "", "asr", asrCap.Backend, "asr_available", asrCap.Available,
		"agent_backend", box.ID(), "agent_slots", slots, "staging", cfg.AgentStagingDir(b.dataDir))
	b.runner.Wake()
	wg.Wait()
}

// supervisorApply is the supervisor's hook for its automatic actions. Only a
// book a sidecars.generate job still follows is acted on: one whose job ended
// (done, failed, cancelled or lost) would run again, and spend, with nothing to
// take its result. An ASR-lane stage (asr, retranscribing) isn't stopped for its
// length: on a CPU it runs for hours on a long book, bounded chapter by chapter,
// and a whole-stage time limit would stop it again and again. The Bridge has no
// fallback agent backend.
func supervisorApply(db *store.DB, sched *scheduler.Scheduler) func(context.Context, supervisor.Action, supervisor.Incident) (string, error) {
	return func(ctx context.Context, action supervisor.Action, inc supervisor.Incident) (string, error) {
		if action == supervisor.ActionFallbackBackend {
			return "", errors.New("the Bridge has no fallback agent backend")
		}
		if action == supervisor.ActionStopBudget && state.LaneOf(state.State(inc.Stage)) == state.LaneASR {
			return "", errors.New("a transcription stage bounds itself chapter by chapter, not by its length")
		}
		followed, err := db.GenerateBookFollowed(ctx, inc.BookID)
		if err != nil {
			return "", err
		}
		if !followed {
			return "", errors.New("no sidecars.generate job follows this book any more")
		}
		detail := inc.Diagnosis
		if len(inc.Evidence) > 0 {
			detail += ": " + strings.Join(inc.Evidence, "; ")
		}
		return sched.SupervisorApply(ctx, string(action), inc.BookID, inc.Stage, detail)
	}
}

// daily runs fn now and then every day until ctx ends.
func (b *Bridge) daily(ctx context.Context, fn func(context.Context)) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pruneEvents drops durable events older than eventRetention.
func (b *Bridge) pruneEvents(ctx context.Context) {
	if n, err := b.db.PruneEvents(ctx, time.Now().Add(-eventRetention)); err != nil && ctx.Err() == nil {
		b.log.Warn("bridge: pruning the engine's event log failed", "err", err)
	} else if n > 0 {
		b.log.Info("bridge: pruned old engine events", "count", n)
	}
}

// sweepTranscripts deletes the transcripts of the books whose job ended more than
// transcripts.retention_days ago (decision 5).
func (b *Bridge) sweepTranscripts(ctx context.Context) {
	days := b.config().Transcripts.RetentionDays
	n, err := b.gen.SweepTranscripts(ctx, time.Duration(days)*24*time.Hour)
	if err != nil && ctx.Err() == nil {
		b.log.Warn("bridge: deleting old transcripts failed", "err", err)
	}
	if n > 0 {
		b.log.Info("bridge: deleted the transcripts of finished books", "count", n, "retention_days", days)
	}
}

// eventPersister writes published events to the durable log off the hot path
// (sidecars' persister): the hub enqueues under its lock, so ids stay ordered,
// and one goroutine does the writes. A full queue drops the event (the log is
// best effort; the supervisor reads what is there).
type eventPersister struct {
	db  *store.DB
	log *slog.Logger
	// mu guards closed against the send on ch and its close: a publisher outside
	// the engine (the page's Retry, a job ending) can still publish after Close.
	mu      sync.Mutex
	closed  bool
	ch      chan events.Event
	done    chan struct{}
	dropped atomic.Uint64
}

func newEventPersister(db *store.DB, log *slog.Logger, buffer int) *eventPersister {
	return &eventPersister{db: db, log: log, ch: make(chan events.Event, max(buffer, 1)), done: make(chan struct{})}
}

func (p *eventPersister) start() { go p.drain() }

// enqueue is called by the hub under its lock and never blocks.
func (p *eventPersister) enqueue(ev events.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return // the engine stopped: dropped, like a full queue's
	}
	select {
	case p.ch <- ev:
	default:
		if n := p.dropped.Add(1); n == 1 || n%1000 == 0 {
			p.log.Warn("bridge: the engine's event log is overloaded", "dropped", n)
		}
	}
}

func (p *eventPersister) drain() {
	defer close(p.done)
	for ev := range p.ch {
		_ = p.db.InsertEvent(context.Background(), time.Now(), ev.ID, ev.Type, ev.BookID, ev.Data)
	}
}

// Close stops taking events (a later one is dropped) and waits for the queue to
// drain.
func (p *eventPersister) Close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.ch)
	}
	p.mu.Unlock()
	<-p.done
}

// newAgentBox is the agent container runner for cfg.
func newAgentBox(cfg config.Config, dataDir string, log *slog.Logger) *agentbox.Runner {
	return agentbox.New(agentbox.Options{Dir: cfg.AgentStagingDir(dataDir), Backend: agentBackend(cfg),
		ClaimTimeout: time.Duration(cfg.Agent.ClaimTimeoutSeconds) * time.Second, Log: log})
}

// estimatesEvery is how often the heartbeat's estimates are computed again: they
// read every finished book and this month's stage runs, and only move when a book
// finishes (which recomputes them at once) or an agent run is paid for.
const estimatesEvery = 5 * time.Minute

// estimatesCache is the last computed estimates, when, and how many jobs the
// generate handler had finished then.
type estimatesCache struct {
	mu   sync.Mutex
	e    *link.SidecarEstimates
	at   time.Time
	done int64
}

// estimates are the heartbeat's figures (nil until the engine has started):
// the cached figures, with the agent container's state and the budget read now.
func (b *Bridge) estimates(ctx context.Context) *link.Estimates {
	if b.engine.get() == nil {
		return nil
	}
	cfg := b.config()
	st := b.box.Status()
	backend := b.box.ID()
	agentState := generate.AgentState{Backend: backend, Ready: st.Available, Subscription: st.Subscription(backend)}
	c := &b.est
	c.mu.Lock()
	defer c.mu.Unlock()
	if done := b.gen.Finished(); c.e == nil || time.Since(c.at) >= estimatesEvery || done != c.done {
		e, err := generate.Estimates(ctx, b.db, agentState, cfg.Agent.MonthlyBudgetUSD, time.Now())
		switch {
		case err == nil:
			c.e, c.at, c.done = e, time.Now(), done
		case ctx.Err() == nil:
			b.log.Warn("bridge: computing the estimates failed", "err", err)
		}
		if c.e == nil {
			return nil
		}
	}
	e := *c.e
	e.AgentBackend, e.AgentReady, e.Subscription = agentState.Backend, agentState.Ready, agentState.Subscription
	e.MonthlyBudgetUSD = cfg.Agent.MonthlyBudgetUSD
	return &link.Estimates{Sidecars: &e}
}
