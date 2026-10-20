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
	fail      bool
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
	if h.fail {
		return nil, errors.New("untrusted failure https://example.com/feed?auth=SECRET123")
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
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "vault-failure"}[fail], func(t *testing.T) {
			db := openStore(t)
			srv := newFakeServer()
			h := &sealingHandler{db: db, fail: fail, t: t}
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
			if fail {
				if h.ran || !h.abandoned || row.State != store.JobFailed {
					t.Fatalf("failed open: %+v", row)
				}
			} else if !h.ran || row.State != store.JobDone {
				t.Fatalf("sealed job did not finish: %+v", row)
			}
		})
	}
}
