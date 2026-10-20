package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/state"
)

// parkBook drives a book to a parked (needs_attention) agent stage carrying code and a
// scheduled retry_at, mirroring what the scheduler's setStatus writes on a timed park.
func parkBook(t *testing.T, s *Scheduler, id int64, code state.ParkCode, retryAt string) {
	t.Helper()
	ctx := context.Background()
	if err := s.db.SetBookState(ctx, id, string(state.FactPass), "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetBookStatusRetry(ctx, id, string(state.StatusNeedsAttention), "parked", string(code), retryAt); err != nil {
		t.Fatal(err)
	}
}

// TestAutoReadmitDueReadmitsTransientPark: a book parked agent_rate_limited with a past
// retry_at is re-admitted (status cleared) by the timed self-resume; the readmit drops a
// durable "auto-retry" note.
func TestAutoReadmitDueReadmitsTransientPark(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "due", "", "")
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)

	_, sub := h.hub.Subscribe(0)
	defer sub.Close()

	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	parkBook(t, s, b.ID, state.ParkAgentRateLimited, past)

	s.autoReadmitDue(context.Background())

	cur, _ := db.GetBook(context.Background(), b.ID)
	if cur.Status != "" || cur.ParkCode != "" || cur.RetryAt != "" {
		t.Fatalf("after auto-readmit: status=%q park_code=%q retry_at=%q, want all empty",
			cur.Status, cur.ParkCode, cur.RetryAt)
	}
	if cur.State != string(state.FactPass) {
		t.Errorf("state = %q, want the parked stage preserved (fact_pass)", cur.State)
	}
}

// A refused agent credential self-resumes too: once its retry_at is due the book
// looks again, so it goes on by itself after someone replaced the credential.
func TestAutoReadmitDueReadmitsCredentialRefusedPark(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	b := h.addBook(t, db, "refused", "", "")
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	parkBook(t, s, b.ID, state.ParkAgentCredentialRefused, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	s.autoReadmitDue(context.Background())
	if cur, _ := db.GetBook(context.Background(), b.ID); cur.Status != "" || cur.ParkCode != "" {
		t.Fatalf("after auto-readmit: status=%q park_code=%q, want readmitted", cur.Status, cur.ParkCode)
	}
}

// TestAutoReadmitDueSkipsNotDueAndNonAuto: a future retry_at, an empty retry_at
// (pre-migration park), and a non-auto park code are all left parked.
func TestAutoReadmitDueSkipsNotDueAndNonAuto(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	ctx := context.Background()

	future := h.addBook(t, db, "future", "", "")
	parkBook(t, s, future.ID, state.ParkAgentUnavailable, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))

	legacy := h.addBook(t, db, "legacy", "", "") // parked before the feature: retry_at=''
	parkBook(t, s, legacy.ID, state.ParkAgentRateLimited, "")

	// A DUE retry_at but a code the timed resume does not own (defensive: a human
	// park never sets retry_at, but the code filter must still exclude it).
	wrongCode := h.addBook(t, db, "wrong", "", "")
	parkBook(t, s, wrongCode.ID, state.ParkSpellingGateFailure, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))

	s.autoReadmitDue(ctx)

	for _, id := range []int64{future.ID, legacy.ID, wrongCode.ID} {
		cur, _ := db.GetBook(ctx, id)
		if cur.Status != string(state.StatusNeedsAttention) {
			t.Errorf("book %d status = %q, want still needs_attention", id, cur.Status)
		}
	}
}

func TestRestartReadmitsChangedAvailabilityParksOnce(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	ctx := context.Background()

	rateLimited := h.addBook(t, db, "rate limited", "", "")
	parkBook(t, s, rateLimited.ID, state.ParkAgentRateLimited, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	auth := h.addBook(t, db, "auth", "", "")
	parkBook(t, s, auth.ID, state.ParkSupervisorEscalated, "")
	if err := db.SetBookStatus(ctx, auth.ID, string(state.StatusNeedsAttention), "supervisor: authentication failure: not logged in", string(state.ParkSupervisorEscalated)); err != nil {
		t.Fatal(err)
	}
	// A budget raised in config.yaml or the environment applies at the start: both
	// budget parks (the per-book one, none; the monthly one, the 1st) look again.
	perBook := h.addBook(t, db, "per-book budget", "", "")
	parkBook(t, s, perBook.ID, state.ParkBudgetExceeded, "")
	monthly := h.addBook(t, db, "monthly budget", "", "")
	parkBook(t, s, monthly.ID, state.ParkBudgetExceeded, time.Now().Add(240*time.Hour).UTC().Format(time.RFC3339))
	// A park a person answers stays.
	human := h.addBook(t, db, "human", "", "")
	parkBook(t, s, human.ID, state.ParkSpellingGateFailure, "")

	s.readmitRestartRecoverable(ctx)

	for _, id := range []int64{rateLimited.ID, auth.ID, perBook.ID, monthly.ID} {
		got, _ := db.GetBook(ctx, id)
		if got.Status != "" || got.ParkCode != "" || got.RetryAt != "" {
			t.Errorf("restart-recoverable book %d remained parked: %+v", id, got)
		}
	}
	if got, _ := db.GetBook(ctx, human.ID); got.ParkCode != string(state.ParkSpellingGateFailure) {
		t.Fatalf("a person's park was readmitted: %+v", got)
	}
}

// TestAutoReadmitMonthlyBudgetPark: the monthly budget park carries retry_at (the
// 1st of the next month) and is re-admitted once it passes; the per-book budget
// park carries none and stays parked for a human.
func TestAutoReadmitMonthlyBudgetPark(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	ctx := context.Background()

	monthly := h.addBook(t, db, "monthly", "", "")
	parkBook(t, s, monthly.ID, state.ParkBudgetExceeded, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	perBook := h.addBook(t, db, "per book", "", "")
	parkBook(t, s, perBook.ID, state.ParkBudgetExceeded, "")

	s.autoReadmitDue(ctx)

	if got, _ := db.GetBook(ctx, monthly.ID); got.Status != "" || got.ParkCode != "" {
		t.Fatalf("a due monthly budget park stayed parked: %+v", got)
	}
	if got, _ := db.GetBook(ctx, perBook.ID); got.Status != string(state.StatusNeedsAttention) {
		t.Fatalf("the per-book budget park was readmitted: %+v", got)
	}
}

// ReadmitParked (an ASR worker connecting) re-admits only the books parked with
// the codes it names; any other park waits.
func TestReadmitParked(t *testing.T) {
	h := newHarness(t)
	db := h.openDB(t)
	waiting := h.addBook(t, db, "waiting", "", "")
	other := h.addBook(t, db, "other", "", "")
	s := New(db, h.hub, NewStubExecutor(0, 0), 1, h.workRoot, false)
	parkBook(t, s, waiting.ID, state.ParkASRWorkerWaiting, "")
	parkBook(t, s, other.ID, state.ParkMarkersNotConfident, "")
	if n := s.ReadmitParked(context.Background(), "an ASR worker connected", state.ParkASRWorkerWaiting); n != 1 {
		t.Fatalf("re-admitted %d, want 1", n)
	}
	if cur, _ := db.GetBook(context.Background(), waiting.ID); cur.Status != "" || cur.ParkCode != "" {
		t.Fatalf("the waiting book: status=%q park_code=%q", cur.Status, cur.ParkCode)
	}
	if cur, _ := db.GetBook(context.Background(), other.ID); cur.ParkCode != string(state.ParkMarkersNotConfident) {
		t.Fatalf("another park was re-admitted: %q", cur.ParkCode)
	}
	if n := s.ReadmitParked(context.Background(), "again", state.ParkASRWorkerWaiting); n != 0 {
		t.Fatalf("re-admitted %d the second time", n)
	}
}
