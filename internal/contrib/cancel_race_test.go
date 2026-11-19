package contrib

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/scheduler"
	"github.com/kodestar/audiosilo-bridge/internal/state"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// publishSpy records the books a service published as stored (PublishStored).
type publishSpy struct {
	mu  sync.Mutex
	ids []int64
}

func (p *publishSpy) publish(_ context.Context, id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids = append(p.ids, id)
}

func (p *publishSpy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ids)
}

// cancelBook cancels book id as the Bridge does at a job's end with no engine
// running (scheduler.CancelStored), failing the test on an error.
func cancelBook(t *testing.T, db *store.DB, id int64) {
	t.Helper()
	if err := scheduler.CancelStored(context.Background(), db, id); err != nil {
		t.Errorf("cancel book %d: %v", id, err)
	}
}

// requireCancelled fails unless book id is still cancelled.
func requireCancelled(t *testing.T, db *store.DB, id int64) {
	t.Helper()
	b, err := db.GetBook(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !scheduler.IsCancelled(b) {
		t.Fatalf("the cancel was written over: status=%q error=%q park=%q", b.Status, b.Error, b.ParkCode)
	}
}

// A Cancel landing between the release gate's read of a core_pending book and
// its release-wait message is kept: the message is not written over it, and the
// book, no longer core_pending, is never re-admitted once the work is released
// (a book no job follows would run).
func TestReleaseWaitNeverWritesOverACancel(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Gated", string(state.ParkCorePending))
	mergedCoreRow(t, db, b.ID)
	if err := db.SetBookWorkID(context.Background(), b.ID, "my-work"); err != nil {
		t.Fatal(err)
	}
	live := false
	resolve := func(context.Context, string) (string, error) {
		if !live {
			cancelBook(t, db, b.ID) // lands after the poller listed the book
			return "", ErrWorkNotFound
		}
		return "my-work", nil
	}
	spy := &readmitSpy{}
	svc := newService(t, db, "http://127.0.0.1:0", fakeTokenResolver{err: ErrNoCredential}, &capture{}, spy, resolve)
	pub := &publishSpy{}
	svc.deps.PublishStored = pub.publish
	var logged bytes.Buffer
	svc.deps.Log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	svc.Poll(context.Background())
	requireCancelled(t, db, b.ID)
	if pub.count() != 0 {
		t.Fatalf("published %d times for a write that never happened", pub.count())
	}
	// The cancel winning is not a fault: it is logged at Debug, never as a warning.
	if out := logged.String(); strings.Contains(out, "level=WARN") || !strings.Contains(out, "level=DEBUG") {
		t.Fatalf("a park message losing to a cancel logged %q, want Debug only", out)
	}
	live = true
	svc.Poll(context.Background())
	if got := spy.called(); len(got) != 0 {
		t.Fatalf("readmit = %v after the cancel, want none", got)
	}
	requireCancelled(t, db, b.ID)
}

// The release-wait message, written over the park as read, is published as
// stored.
func TestReleaseWaitIsPublished(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Gated", string(state.ParkCorePending))
	mergedCoreRow(t, db, b.ID)
	if err := db.SetBookWorkID(context.Background(), b.ID, "my-work"); err != nil {
		t.Fatal(err)
	}
	svc := newService(t, db, "http://127.0.0.1:0", fakeTokenResolver{err: ErrNoCredential}, &capture{}, &readmitSpy{},
		func(context.Context, string) (string, error) { return "", ErrWorkNotFound })
	pub := &publishSpy{}
	svc.deps.PublishStored = pub.publish
	svc.Poll(context.Background())
	if nb, _ := db.GetBook(context.Background(), b.ID); nb.Error != ReleaseWaitMsg {
		t.Fatalf("message %q, want the release wait", nb.Error)
	}
	if pub.count() != 1 {
		t.Fatalf("published %d times, want 1", pub.count())
	}
}

// A Cancel landing while the poller reads a merged add-work PR (its read of the
// book done) is kept when the PR names no single new work.
func TestUnresolvedSlugNeverWritesOverACancel(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "No Slug", string(state.ParkCorePending))
	mergedCoreRow(t, db, b.ID)
	var once sync.Once
	gh := coreRepoFake{
		files: `[]`,
		extra: func(_ http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/pulls/40") {
				once.Do(func() { cancelBook(t, db, b.ID) })
			}
			return false
		},
	}.server(t)
	spy := &readmitSpy{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, spy, nil)

	svc.Poll(context.Background())
	if row := getRow(t, db, b.ID, store.ContribKindCore); !strings.Contains(row.Note, store.ContribNoteCoreSlugUnresolvedPrefix) {
		t.Fatalf("row note %q, want the unresolved slug noted", row.Note)
	}
	requireCancelled(t, db, b.ID)
	svc.Poll(context.Background())
	if got := spy.called(); len(got) != 0 {
		t.Fatalf("readmit = %v after the cancel, want none", got)
	}
	requireCancelled(t, db, b.ID)
}

// A Cancel landing between the verdict follow-up's read of a core_pending book
// and its verdict message is kept.
func TestCoreVerdictNeverWritesOverACancel(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Core Human", string(state.ParkCorePending))
	addRow(t, db, b.ID, store.ContribKindCore, store.ContribModeIssue, 12, store.ContribStatusSubmitted)
	gh, _ := verdictFake(t, "open", []string{"data:needs-human"}, `[]`)
	spy := &readmitSpy{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, spy, nil)
	svc.beforePark = func(id int64) { cancelBook(t, db, id) }

	svc.Poll(context.Background())
	requireCancelled(t, db, b.ID)
	if got := spy.called(); len(got) != 0 {
		t.Fatalf("readmit = %v, want none", got)
	}
}

// A Cancel landing while SubmitCore opens the add-work issue (its read of the
// book done) is kept: the issue is recorded, the book stays cancelled, and a
// resubmit reusing the recorded issue does not park the cancelled book either.
func TestSubmitCoreNeverWritesOverACancel(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Needs Core", string(state.ParkCoreNeeded))
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/issues") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
			return
		}
		cancelBook(t, db, b.ID) // lands while the issue is being opened
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":1,"html_url":"https://gh/issues/1","labels":[{"name":"data:add-work"}]}`)
	}))
	t.Cleanup(gh.Close)
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, nil, nil)
	p := CoreProposal{Title: "Needs Core", Authors: []string{"A"}, Language: "en", Narrators: []string{"N"}, Sources: "scan"}

	row, err := svc.SubmitCore(context.Background(), b, p)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if row.Number != 1 {
		t.Fatalf("row = %+v, want the opened issue recorded", row)
	}
	requireCancelled(t, db, b.ID)
	if _, err := svc.SubmitCore(context.Background(), b, p); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	requireCancelled(t, db, b.ID)
}
