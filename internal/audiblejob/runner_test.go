package audiblejob

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// Deliver only jobs of a kind for which the dispatcher has a free slot.
type pollQueue struct {
	*link.Client
	next   chan *link.Job
	done   chan int64
	update func(link.Update) error
}

func (q *pollQueue) Claim(ctx context.Context, kinds []string, wait time.Duration) (*link.Job, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(wait):
		return nil, nil
	case job := <-q.next:
		if !slices.Contains(kinds, job.Kind) {
			q.next <- job
			return nil, nil
		}
		return job, nil
	}
}

func (q *pollQueue) UpdateJob(_ context.Context, id int64, u link.Update) (*link.Job, error) {
	if q.update != nil {
		if err := q.update(u); err != nil {
			return nil, err
		}
	}
	if u.State != nil && *u.State == link.StateDone && q.done != nil {
		q.done <- id
	}
	return &link.Job{ID: id, State: link.StateRunning}, nil
}

type podcastPoll struct{}

func (podcastPoll) Kind() string                                               { return "podcast.poll" }
func (podcastPoll) Lane() string                                               { return jobs.LaneLight }
func (podcastPoll) Run(context.Context, jobs.Job, *jobs.Reporter) (any, error) { return nil, nil }

func TestPodcastPollRunsWhileAudibleEnrichmentWaits(t *testing.T) {
	books, accounts := backfill(2)
	h := setup(t, books, accounts)
	client := h.o.Server().(*link.Client).WithServerID("srv-1")
	h.o.Server = func() Server { return client }
	waiting, resume := make(chan struct{}), make(chan struct{})
	h.sleep = func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-resume:
			return nil
		}
	}
	q := &pollQueue{Client: client, next: make(chan *link.Job, 2), done: make(chan int64, 2)}
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	r := jobs.New(db, func() jobs.Server { return q }, nil, jobs.Options{Wait: 100 * time.Millisecond, Lanes: map[string]int{jobs.LaneLight: 1}}, h, podcastPoll{})
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	stopped := make(chan struct{})
	go func() { defer close(stopped); r.Run(ctx) }()
	defer func() { stop(); <-stopped }()
	q.next <- &link.Job{ID: 1, Kind: Kind, Params: json.RawMessage(`{"account":"acct0001"}`), Attempts: 1}
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("Audible poll never reached its enrichment wait")
	}
	q.next <- &link.Job{ID: 2, Kind: "podcast.poll", Attempts: 1}
	for _, want := range []int64{2, 1} {
		select {
		case id := <-q.done:
			if id != want {
				t.Fatalf("job %d finished, want %d", id, want)
			}
		case <-ctx.Done():
			t.Fatalf("job %d did not finish", want)
		}
		if want == 2 {
			close(resume)
		}
	}
}

type namedImportServer struct{ *stubImportServer }

func (namedImportServer) ServerID() string { return "srv1" }

func TestImportLeaseRefusalFailsWithoutPublishing(t *testing.T) {
	e := newImportEnv(t)
	srv := namedImportServer{e.srv}
	e.opts.Server = func() ImportServer { return srv }
	var staged, confirmed atomic.Bool
	e.opts.onProgress = func(p jobs.Progress) {
		if p.Step == StepPlace && p.Done > 0 {
			staged.Store(true)
		}
	}
	cl, err := link.New("http://127.0.0.1", "token", "test")
	if err != nil {
		t.Fatal(err)
	}
	terminal := make(chan string, 1)
	q := &pollQueue{Client: cl.WithServerID("srv1"), next: make(chan *link.Job, 1), update: func(u link.Update) error {
		if u.State != nil {
			switch *u.State {
			case link.StateRunning:
				if staged.Load() && len(u.Progress) == 0 {
					confirmed.Store(true)
					return &link.APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Hour}
				}
			case link.StateDone, link.StateFailed, link.StateNeedsAttention:
				terminal <- *u.State
			}
		}
		return nil
	}}
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	r := jobs.New(db, func() jobs.Server { return q }, nil, jobs.Options{Wait: 100 * time.Millisecond}, NewImport(e.opts))
	stopped := make(chan struct{})
	go func() { defer close(stopped); r.Run(ctx) }()
	defer func() { stop(); <-stopped }()
	q.next <- &link.Job{ID: 1, Attempts: 1, Kind: ImportKind, Params: e.params(nil)}
	select {
	case state := <-terminal:
		if state != link.StateFailed || !confirmed.Load() || len(e.libraryFiles()) != 0 {
			t.Fatalf("lease checked=%t, state=%s, library=%v", confirmed.Load(), state, e.libraryFiles())
		}
	case <-ctx.Done():
		t.Fatal("no terminal report")
	}
}
