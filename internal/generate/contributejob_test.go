package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/contrib"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/pipeline"
	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
)

// fakeSharer is the engine's share step.
type fakeSharer struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (f *fakeSharer) Share(_ context.Context, bookID int64, workID string, dims []string) (pipeline.ShareResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, workID+"|"+strings.Join(dims, ","))
	if f.err != nil {
		return pipeline.ShareResult{}, f.err
	}
	_ = bookID
	return pipeline.ShareResult{WorkID: "the-book", Contributed: map[string]string{"characters": "https://github.com/x/y/issues/1"},
		Covered: []string{"recaps"}}, nil
}

// shareRunner runs the generate and contribute jobs with s as the share step.
func (hr *harness) shareRunner(s Sharer, cred func(context.Context) error) (stop func()) {
	h := New(Options{DB: hr.db, Engine: func() Engine { return hr.sched }, Sharer: func() Sharer { return s },
		Libraries: hr.libs, Mount: func(int64, string) (string, bool) { return hr.libs.root, false },
		Server: func() Server { return hr.cl }, WorkRoot: hr.work, Version: "0.2.0", RetentionDays: 30, ContribCredential: cred,
		Poll: 10 * time.Millisecond, IdlePoll: 10 * time.Millisecond, Recheck: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	r := jobs.New(hr.db, func() jobs.Server { return hr.cl }, nil, jobs.Options{Keepalive: 30 * time.Millisecond,
		ProgressEvery: 5 * time.Millisecond, Wait: 10 * time.Millisecond, RetryEvery: 20 * time.Millisecond,
		ServerID: func() string { return "srv" }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, h, h.ContributeJob())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	return func() { cancel(); <-done }
}

// addContribute queues a sidecars.contribute job.
func (f *fakeServer) addContribute(id int64, path, params string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lib := int64(1)
	f.queue = append(f.queue, link.Job{ID: id, Kind: ContributeKind, LibraryID: &lib, Path: &path, Params: json.RawMessage(params),
		Attempts: 1, State: "claimed"})
}

func TestContributeJob(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(71)
	s := &fakeSharer{}
	hr.srv.addContribute(72, "Author/Book", `{"work_id":"the-book","dimensions":["characters","recaps"],"later":1}`)
	stop := hr.shareRunner(s, func(context.Context) error { return nil })
	defer stop()
	hr.waitFor("done", func() bool { s, _ := hr.srv.final(72); return s == link.StateDone })
	var res ContributeResult
	hr.result(72, &res)
	if res.LibraryID != 1 || res.Path != "Author/Book" || res.WorkID != "the-book" ||
		res.Contributed["characters"] == "" || !slices.Equal(res.Covered, []string{"recaps"}) {
		t.Fatalf("result = %+v", res)
	}
	if s.calls[0] != "the-book|characters,recaps" {
		t.Fatalf("share calls = %v", s.calls)
	}
}

func TestContributeJobRefusals(t *testing.T) {
	t.Parallel()
	hr := newHarness(t)
	hr.generated(81)
	cases := []struct {
		id     int64
		path   string
		params string
		err    error
		cred   error // what ContribCredential answers
		want   describeFailure
	}{
		{82, "Loose.m4b", `{}`, nil, nil, errShareNothing},
		{83, "Author/Book", `{}`, nil, contrib.ErrNoCredential, errShareCantContribute},
		{84, "Author/Book", `{}`, pipeline.ErrNotMatched, nil, errShareNotMatched},
		{85, "Author/Book", `{}`, scheduler.ParkWithCode(state.ParkContribUnavailable, pipeline.ContribUnavailableMsg), nil, errShareCantContribute},
		{86, "Author/Book", `{}`, pipeline.ErrNoSidecars, nil, errShareNothing},
		{87, "Author/Book", `{}`, fmt.Errorf("%w (the characters issue): x", pipeline.ErrFilingUnknown), nil, errShareFilingUnknown},
		{88, "Author/Book", `{}`, nil, contrib.ErrSignInUnrefreshed, errShareUnrefreshed},
		{89, "Author/Book", `{}`, &scheduler.ParkError{Code: state.ParkContribUnavailable, Reason: pipeline.ContribUnrefreshedMsg,
			Cause: contrib.ErrSignInUnrefreshed}, nil, errShareUnrefreshed},
	}
	for _, c := range cases {
		s := &fakeSharer{err: c.err}
		cred := c.cred
		hr.srv.addContribute(c.id, c.path, c.params)
		stop := hr.shareRunner(s, func(context.Context) error { return cred })
		hr.waitFor("the refusal", func() bool { st, _ := hr.srv.final(c.id); return st == link.StateFailed })
		stop()
		if _, u := hr.srv.final(c.id); u.Error == nil || *u.Error != string(c.want) {
			t.Fatalf("job %d failed with %v, want %q", c.id, u.Error, c.want)
		}
	}
}

func TestParseContributeParams(t *testing.T) {
	for raw, ok := range map[string]bool{
		``: true, `null`: true, `{}`: true, `{"dimensions":["recaps"]}`: true, `{"work_id":"w","new_field":1}`: true,
		`{"dimensions":["notes"]}`: false, `{"dimensions":["recaps","recaps"]}`: false, `[]`: false,
		`{"work_id":"` + strings.Repeat("w", 201) + `"}`: false,
	} {
		if _, err := ParseContributeParams(json.RawMessage(raw)); (err == nil) != ok {
			t.Errorf("%s: err = %v", raw, err)
		}
	}
}
