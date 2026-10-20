package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/store"
)

type sealingHandler struct {
	db        *store.DB
	fail      error
	ran       bool
	abandoned bool
	t         *testing.T
}

func (h *sealingHandler) Kind() string { return "podcast.subscribe" }
func (h *sealingHandler) Lane() string { return LaneLight }
func (h *sealingHandler) SealParams(job Job) (json.RawMessage, error) {
	if _, err := h.db.GetServerJob(context.Background(), job.ID); !errors.Is(err, store.ErrNotFound) {
		h.t.Error("claim was written before sealing")
	}
	if h.fail != nil {
		return nil, h.fail
	}
	return json.RawMessage(`{"feed_url":"","private":true}`), nil
}
func (h *sealingHandler) Run(ctx context.Context, job Job, _ *Reporter) (any, error) {
	h.ran = true
	row, err := h.db.GetServerJob(ctx, job.ID)
	if err != nil || strings.Contains(string(row.Params), "SECRET123") || strings.Contains(string(job.Params), "SECRET123") {
		h.t.Error("unsealed params reached Run")
	}
	return map[string]bool{"ok": true}, nil
}
func (h *sealingHandler) Abandon(context.Context, Job) { h.abandoned = true }
func TestParamSealerPrecedesClaimAndFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		fail       error
		code, text string
	}{
		"allowed": {},
		// An error's text may hold the secret: only a generic failure is reported.
		"vault-failure": {fail: errors.New("untrusted failure https://example.com/feed?auth=SECRET123"), code: "write_failed", text: "The job's private parameters could not be sealed."},
		// A SealFailure is safe to report as it is.
		"coded-failure": {fail: &SealFailure{Code: "invalid_feed_url", Message: "The feed URL is invalid."}, code: "invalid_feed_url", text: "The feed URL is invalid."},
	} {
		t.Run(name, func(t *testing.T) {
			db := openStore(t)
			srv := newFakeServer()
			h := &sealingHandler{db: db, fail: tc.fail, t: t}
			r := New(db, func() Server { return srv }, nil, testOpts(), h)
			var wg sync.WaitGroup
			r.start(context.Background(), srv, Job{ID: 42, Kind: h.Kind(), Params: json.RawMessage(`{"feed_url":"https://example.com/feed?auth=SECRET123","private":true}`), Attempt: 1}, &wg)
			wg.Wait()
			row, err := db.GetServerJob(context.Background(), 42)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(row)
			if strings.Contains(string(raw), "SECRET123") {
				t.Fatal("private URL reached the store")
			}
			if tc.fail == nil {
				if !h.ran || row.State != store.JobDone {
					t.Fatalf("sealed job did not finish: %+v", row)
				}
				return
			}
			if h.ran || !h.abandoned || row.State != store.JobFailed {
				t.Fatalf("failed open: %+v", row)
			}
			state, u := srv.finalState(42)
			var result struct{ Code, Message string }
			_ = json.Unmarshal(u.Result, &result)
			if state != store.JobFailed || result.Code != tc.code || result.Message != tc.text || u.Error == nil || *u.Error != tc.text {
				t.Fatalf("reported %s %s %v", state, u.Result, u.Error)
			}
		})
	}
}

type plainSealer struct{ ran, abandoned bool }

func (h *plainSealer) Kind() string                            { return "podcast.subscribe" }
func (h *plainSealer) Lane() string                            { return LaneLight }
func (h *plainSealer) SealParams(Job) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
func (h *plainSealer) Run(context.Context, Job, *Reporter) (any, error) {
	h.ran = true
	return nil, nil
}
func (h *plainSealer) Abandon(context.Context, Job) { h.abandoned = true }

// A claim the store can't record still runs (as it always did), unless its
// handler sealed a secret: without the row that secret could be neither
// resumed nor released, so that job is abandoned for the server to hand out
// again.
func TestUnrecordedClaimFailsClosedOnlyForSealers(t *testing.T) {
	db := openStore(t)
	_ = db.Close() // every write fails
	srv := newFakeServer()
	sealer := &plainSealer{}
	ran := make(chan struct{})
	other := handler{kind: "bridge.verify", lane: LaneLight, fn: func(context.Context, Job, *Reporter) (any, error) {
		close(ran)
		return nil, nil
	}}
	r := New(db, func() Server { return srv }, nil, testOpts(), sealer, other)
	var wg sync.WaitGroup
	r.start(context.Background(), srv, Job{ID: 1, Kind: sealer.Kind(), Params: json.RawMessage(`{}`), Attempt: 1}, &wg)
	r.start(context.Background(), srv, Job{ID: 2, Kind: other.kind, Params: json.RawMessage(`{}`), Attempt: 1}, &wg)
	wg.Wait()
	select {
	case <-ran:
	default:
		t.Fatal("an unrecorded claim of a kind that seals nothing did not run")
	}
	if sealer.ran || !sealer.abandoned {
		t.Fatalf("an unrecorded sealed claim ran: %+v", sealer)
	}
}
