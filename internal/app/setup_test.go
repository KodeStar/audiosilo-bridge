package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/setup"
	"github.com/kodestar/audiosilo-bridge/internal/state"
)

// setupServer answers the heartbeat as a server of one of three kinds: "old"
// (no accepts, and its decoder refuses the setup field it doesn't know), "new"
// (accepts setup), "refusing" (lists setup, then refuses a beat carrying it).
type setupServer struct {
	kind  string
	mu    sync.Mutex
	beats []map[string]json.RawMessage
	codes []int
}

func (f *setupServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/bridge/pair" {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"tok","server_id":"srv-1","server_name":"Home"}`)
		return
	}
	if r.URL.Path != "/api/v1/bridge/status" {
		http.NotFound(w, r)
		return
	}
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	_, hasSetup := body["setup"]
	code := http.StatusOK
	if hasSetup && f.kind != "new" {
		code = http.StatusBadRequest
	}
	f.mu.Lock()
	f.beats = append(f.beats, body)
	f.codes = append(f.codes, code)
	f.mu.Unlock()
	if code != http.StatusOK {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":"invalid status body"}`)
		return
	}
	reply := link.StatusReply{ServerTime: "now", Libraries: []link.Library{}}
	if f.kind != "old" {
		reply.Accepts = []string{link.AcceptSetup}
	}
	_ = json.NewEncoder(w).Encode(reply)
}

// sent reports, per beat, whether it carried setup, and the statuses answered.
func (f *setupServer) sent() (withSetup []bool, codes []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.beats {
		_, ok := b["setup"]
		withSetup = append(withSetup, ok)
	}
	return withSetup, append([]int(nil), f.codes...)
}

func pairedBridge(t *testing.T, f *setupServer) (*Bridge, context.Context) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	b, _, err := New(ctx, Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.db.Close() })
	if err := b.Pair(ctx, srv.URL, "CODE"); err != nil {
		t.Fatal(err)
	}
	drainKick(b)
	return b, ctx
}

func drainKick(b *Bridge) bool {
	select {
	case <-b.kick:
		return true
	default:
		return false
	}
}

// D1: the checklist goes only to a server whose reply accepts it, the beat
// right after that reply is kicked, and an old server never sees the field.
func TestHeartbeatSendsSetupOnlyWhenAccepted(t *testing.T) {
	f := &setupServer{kind: "new"}
	b, ctx := pairedBridge(t, f)
	b.beat(ctx)
	if !drainKick(b) {
		t.Fatal("the first reply accepting setup didn't kick a beat")
	}
	b.beat(ctx)
	b.beat(ctx)
	if drainKick(b) {
		t.Fatal("a beat that already carried setup kicked again")
	}
	with, codes := f.sent()
	if len(with) != 3 || with[0] || !with[1] || !with[2] || codes[1] != http.StatusOK {
		t.Fatalf("setup sent = %v (%v), want [false true true]", with, codes)
	}
	var s link.Setup
	f.mu.Lock()
	err := json.Unmarshal(f.beats[1]["setup"], &s)
	f.mu.Unlock()
	if err != nil || len(s.Items) != 6 || s.Items[0].ID != setup.ItemPaired || !s.Items[0].Done {
		t.Fatalf("setup = %+v (%v)", s, err)
	}

	// A new pairing starts over: nothing is sent until the server says again.
	b.linkChanged()
	drainKick(b)
	b.beat(ctx)
	if with, _ := f.sent(); with[3] {
		t.Fatal("the first beat after a re-pair carried setup")
	}
}

func TestHeartbeatOldServerNeverGetsSetup(t *testing.T) {
	f := &setupServer{kind: "old"}
	b, ctx := pairedBridge(t, f)
	for range 3 {
		b.beat(ctx)
	}
	with, codes := f.sent()
	for i := range with {
		if with[i] || codes[i] != http.StatusOK {
			t.Fatalf("beat %d: setup %v, code %d", i, with[i], codes[i])
		}
	}
	if drainKick(b) {
		t.Fatal("an old server's reply kicked a beat")
	}
}

// A 400 on a beat carrying setup sends it again without at once, and the block
// stays off (the server's accepts ignored) rather than doubling every beat.
func TestHeartbeatRefusedSetupIsDropped(t *testing.T) {
	f := &setupServer{kind: "refusing"}
	b, ctx := pairedBridge(t, f)
	b.beat(ctx) // accepts listed
	drainKick(b)
	b.beat(ctx) // with setup: 400, then again without
	b.beat(ctx) // without
	with, codes := f.sent()
	want := []bool{false, true, false, false}
	if len(with) != len(want) {
		t.Fatalf("beats = %v, want %v", with, want)
	}
	for i := range want {
		if with[i] != want[i] {
			t.Fatalf("setup sent = %v, want %v (codes %v)", with, want, codes)
		}
	}
	if codes[1] != http.StatusBadRequest || codes[2] != http.StatusOK {
		t.Fatalf("codes = %v", codes)
	}
	b.mu.Lock()
	beatErr := b.beatErr
	b.mu.Unlock()
	if beatErr != "" {
		t.Fatalf("the resent beat went through, yet beatErr = %q", beatErr)
	}
}

// Saving the monthly budget on the page marks it reviewed (the checklist's
// budget item), even unchanged; one the environment sets is locked and counts
// as reviewed already.
func TestSaveBudgetMarksItReviewed(t *testing.T) {
	ctx := context.Background()
	b, _, err := New(ctx, Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.db.Close() }()
	s := b.Settings()
	if s.BudgetReviewed || s.MonthlyBudgetUSD == nil || *s.MonthlyBudgetUSD != 100 {
		t.Fatalf("settings = %+v", s)
	}
	if b.Status().SetupComplete {
		t.Fatal("an unpaired, unreviewed Bridge is set up")
	}
	s.MonthlyBudgetUSD = nil // a save without it keeps it, unreviewed
	if err := b.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if b.Settings().BudgetReviewed {
		t.Fatal("a save without the budget reviewed it")
	}
	same := 100.0
	s.MonthlyBudgetUSD = &same
	if err := b.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got := b.Settings(); !got.BudgetReviewed || *got.MonthlyBudgetUSD != 100 {
		t.Fatalf("after saving = %+v", got)
	}
	for _, it := range b.Status().Setup.Items {
		if it.ID == setup.ItemBudget && !it.Done {
			t.Fatalf("budget item = %+v", it)
		}
	}
	bad := -1.0
	s.MonthlyBudgetUSD = &bad
	if err := b.SaveSettings(ctx, s); err == nil {
		t.Fatal("a negative budget saved")
	}

	t.Setenv("AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD", "25")
	b2, _, err := New(ctx, Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b2.db.Close() }()
	s2 := b2.Settings()
	if !s2.BudgetReviewed || *s2.MonthlyBudgetUSD != 25 {
		t.Fatalf("env settings = %+v", s2)
	}
	other := 30.0
	s2.MonthlyBudgetUSD = &other
	if err := b2.SaveSettings(ctx, s2); err == nil || !strings.Contains(err.Error(), "AGENT_MONTHLY_BUDGET_USD") {
		t.Fatalf("a locked budget = %v", err)
	}
}

// A budget changed on the page wakes the describe jobs parked on the budget (they
// wait in their own loop, not as engine books); a save that keeps it doesn't. A
// changed agent credential wakes the ones parked on a refused or missing one.
func TestBudgetAndCredentialChangesWakeParkedDescribeJobs(t *testing.T) {
	ctx := context.Background()
	b := newTestBridge(t)
	var mu sync.Mutex
	var woke []state.ParkCode
	b.wakeParked = func(code state.ParkCode) int {
		mu.Lock()
		defer mu.Unlock()
		woke = append(woke, code)
		return 0
	}
	got := func() []state.ParkCode {
		mu.Lock()
		defer mu.Unlock()
		out := woke
		woke = nil
		return out
	}
	s := b.Settings()
	same := *s.MonthlyBudgetUSD
	s.MonthlyBudgetUSD = &same
	if err := b.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if w := got(); len(w) != 0 {
		t.Fatalf("an unchanged budget woke %v", w)
	}
	raised := same + 50
	s.MonthlyBudgetUSD = &raised
	if err := b.SaveSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if w := got(); !slices.Equal(w, []state.ParkCode{state.ParkBudgetExceeded}) {
		t.Fatalf("a raised budget woke %v", w)
	}
	b.agentCredentialChanged()
	if w := got(); !slices.Equal(w, []state.ParkCode{state.ParkAgentCredentialRefused, state.ParkAgentUnavailable}) {
		t.Fatalf("a changed credential woke %v", w)
	}
}

// The Accounts tab's agent credential: sealed, reported by kind only, handed
// to the agent box, and a malformed one refused without its value.
func TestAgentCredentialOnTheAccountsTab(t *testing.T) {
	ctx := context.Background()
	b, _, err := New(ctx, Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.db.Close() }()
	err = b.SetAgentCredential(ctx, "claude_oauth_token", "sk-ant oat-secret")
	if err == nil || strings.Contains(err.Error(), "secret") || !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("a value with a space = %v", err)
	}
	if err := b.SetAgentCredential(ctx, "nope", "x"); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("an unknown kind = %v", err)
	}
	if err := b.SetAgentCredential(ctx, "claude_oauth_token", " sk-ant-oat01-secret\n"); err != nil {
		t.Fatal(err)
	}
	acc := b.Accounts(ctx)
	raw, _ := json.Marshal(acc)
	if !acc.Agent.Set["claude_oauth_token"] || acc.Agent.SetAt == "" || acc.Agent.SetAtByKind["claude_oauth_token"] == "" ||
		len(acc.Agent.SetAtByKind) != 1 || strings.Contains(string(raw), "secret") {
		t.Fatalf("accounts = %s", raw)
	}
	if kind, value, _, ok := b.agentCredential("claude"); !ok || kind != "oauth_token" || value != "sk-ant-oat01-secret" {
		t.Fatalf("hand-off = %q %v", kind, ok)
	}
	if _, _, _, ok := b.agentCredential("codex"); ok {
		t.Fatal("codex got claude's credential")
	}
	if err := b.DeleteAgentCredential(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := b.agentCredential("claude"); ok || b.Accounts(ctx).Agent.Set["claude_oauth_token"] {
		t.Fatal("the credential survived its removal")
	}
	// The default App client ID: the sign-in is available, and none is stored.
	if gh := b.Accounts(ctx).GitHub; !gh.Available || gh.SignedIn || gh.ManageURL == "" {
		t.Fatalf("github = %+v", gh)
	}
}
