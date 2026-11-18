package audiblesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audibleacct"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

const acctID = "abcd1234"

// stubAccounts is the account manager: one account in the store, Audible's
// places, and the writes and content-reference reads asked of it.
type stubAccounts struct {
	db *store.DB

	mu        sync.Mutex
	places    map[string]audible.Position
	placesErr error
	readFirst []string // with placesErr: the ASINs whose places came back before it
	reads     int
	onRead    func() // runs as Audible answers a read (outside the lock)
	acrs      int
	acrErr    error
	sets      []string // "asin@ms"
	setErr    error
	wrongACR  error  // the answer to a write with another content reference
	onSet     func() // runs as a write reaches Audible (outside the lock)
	settled   int    // SettledAccount reads of a refused account (its wait)
	onSettle  func() // runs as SettledAccount waits (a sign-in in flight storing itself)
}

func (s *stubAccounts) Account(ctx context.Context, id string) (store.AudibleAccount, error) {
	a, err := s.db.GetAudibleAccount(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return a, audibleacct.ErrNotFound
	}
	return a, err
}

func (s *stubAccounts) SettledAccount(ctx context.Context, id string) (store.AudibleAccount, error) {
	if a, err := s.Account(ctx, id); err != nil || a.RefusedAt == "" {
		return a, err
	}
	s.mu.Lock()
	s.settled++
	hook := s.onSettle
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.Account(ctx, id)
}

func (s *stubAccounts) Accounts(ctx context.Context) ([]store.AudibleAccount, error) {
	return s.db.AudibleAccounts(ctx)
}

func (s *stubAccounts) Positions(_ context.Context, _ string, asins []string) (map[string]audible.Position, error) {
	s.mu.Lock()
	s.reads++
	read := asins
	if s.placesErr != nil { // only the ones read before the failure
		read = slices.DeleteFunc(slices.Clone(s.readFirst), func(a string) bool { return !slices.Contains(asins, a) })
	}
	out := map[string]audible.Position{}
	for _, a := range read {
		if p, ok := s.places[a]; ok {
			out[a] = p
		}
	}
	if s.placesErr != nil {
		s.mu.Unlock()
		return out, s.placesErr
	}
	hook := s.onRead
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}

func (s *stubAccounts) ContentACR(_ context.Context, _, asin string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acrs++
	if s.acrErr != nil {
		return "", s.acrErr
	}
	return "CR!" + asin, nil
}

func (s *stubAccounts) SetPosition(_ context.Context, _, asin, acr string, positionMs int64) error {
	s.mu.Lock()
	hook := s.onSet
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if acr != "CR!"+asin {
		if s.wrongACR != nil {
			return s.wrongACR
		}
		return fmt.Errorf("wrong acr %q", acr)
	}
	if s.setErr != nil {
		return s.setErr
	}
	s.sets = append(s.sets, fmt.Sprintf("%s@%d", asin, positionMs))
	// Audible holds the place written now (a later read answers it).
	s.places[asin] = audible.Position{PositionMs: positionMs}
	return nil
}

// stubServer is the server's sync routes: it answers a listen ahead of its
// place applied and one not ahead behind (furthest only), and serves a feed of
// rows with an opaque cursor (the index past the last row read).
type stubServer struct {
	mu       sync.Mutex
	server   map[string]float64 // path -> the server's place
	posted   []link.AudibleListening
	postErr  error
	rows     []link.AudibleProgressRow
	feedErr  error
	badSince string   // a cursor the feed answers 400 for
	feedAsks []string // since values
	pageSize int
	clock    time.Time          // the feed's server_time (zero: none, a server before 2.7.0)
	duration map[string]float64 // path -> the book's length on the server (0 unknown)
	rejected map[string]string  // path -> the reason the server rejects its events
}

func (s *stubServer) PostAudibleListening(_ context.Context, account string, in link.AudibleListening) (*link.SyncAnswer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if account != acctID {
		return nil, link.ErrUnknownAccount
	}
	if s.postErr != nil {
		return nil, s.postErr
	}
	s.posted = append(s.posted, in)
	ans := &link.SyncAnswer{ServerTime: "2026-11-03T12:00:00Z"}
	seen := map[string]bool{}
	for _, ev := range in.Events {
		r := link.SyncResult{ID: ev.ID, Status: link.SyncApplied}
		switch {
		case s.rejected[ev.Path] != "":
			r.Status, r.Reason = link.SyncRejected, s.rejected[ev.Path]
		case seen[ev.ID]:
			r.Status = link.SyncDuplicate
		case ev.Kind == link.EventListen && *ev.ToPos <= s.server[ev.Path]:
			r.Status, r.Reason = link.SyncNotApplied, link.SyncBehind
		case ev.Kind == link.EventListen:
			s.server[ev.Path] = *ev.ToPos
		}
		seen[ev.ID] = true
		ans.Results = append(ans.Results, r)
	}
	return ans, nil
}

func (s *stubServer) AudibleProgress(_ context.Context, account, since string, limit int) (*link.AudibleProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.feedAsks = append(s.feedAsks, since)
	if s.feedErr != nil {
		return nil, s.feedErr
	}
	if since != "" && since == s.badSince {
		return nil, fmt.Errorf("%w: %w", link.ErrBadCursor, &link.APIError{Status: http.StatusBadRequest, Code: "invalid_request"})
	}
	start := 0
	if since != "" {
		_, _ = fmt.Sscanf(since, "c%d", &start)
	}
	size := limit
	if s.pageSize > 0 {
		size = s.pageSize
	}
	end := min(start+size, len(s.rows))
	page := &link.AudibleProgress{Rows: append([]link.AudibleProgressRow{}, s.rows[start:end]...), Next: since, More: end-start == size}
	if !s.clock.IsZero() {
		page.ServerTime = s.clock.UTC().Format(time.RFC3339)
	}
	if end > start {
		page.Next = fmt.Sprintf("c%d", end)
	}
	return page, nil
}

// resend appends row i again, changed by f (the server wrote it again: the
// feed pages in write order, so it comes again whatever its updated_at).
func (s *stubServer) resend(i int, f func(*link.AudibleProgressRow)) {
	s.mu.Lock()
	row := s.rows[i]
	f(&row)
	s.rows = append(s.rows, row)
	s.mu.Unlock()
}

// add appends a server row (the feed's newest change).
func (s *stubServer) add(path, asin string, pos float64, at time.Time, device string) {
	s.mu.Lock()
	s.rows = append(s.rows, link.AudibleProgressRow{LibraryID: 1, Path: path, ASIN: asin, Position: pos, Duration: s.duration[path],
		UpdatedAt: at.UTC().Format(time.RFC3339Nano), DeviceID: device, Version: int64(len(s.rows) + 1)})
	s.mu.Unlock()
}

type env struct {
	db      *store.DB
	accts   *stubAccounts
	srv     *stubServer
	now     time.Time
	changed int
	o       Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &env{db: db, accts: &stubAccounts{db: db, places: map[string]audible.Position{}}, srv: &stubServer{server: map[string]float64{}},
		now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	if err := db.SignInAudibleAccount(context.Background(), store.AudibleAccount{ID: acctID, Label: "Chris (US)", Marketplace: "us",
		ServerID: "srv-1", UserID: 3, LibraryID: 1, Policy: "manual", Sync: true}, e.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.o = Options{Accounts: e.accts, DB: db, Server: func() Server { return e.srv }, ServerID: func() string { return "srv-1" },
		OnChange: func() { e.changed++ }, Now: func() time.Time { return e.now }}
	return e
}

func (e *env) account(t *testing.T) store.AudibleAccount {
	t.Helper()
	a, err := e.db.GetAudibleAccount(context.Background(), acctID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func params(t *testing.T, titles ...link.AudibleSyncTitle) json.RawMessage {
	t.Helper()
	if titles == nil {
		titles = []link.AudibleSyncTitle{}
	}
	raw, err := json.Marshal(link.AudibleSyncParams{Account: acctID, Titles: titles})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func title(asin, path string, runtimeS int64) link.AudibleSyncTitle {
	return link.AudibleSyncTitle{ASIN: asin, LibraryID: 1, Path: path, RuntimeS: runtimeS}
}

func (e *env) runSync(t *testing.T, raw json.RawMessage) (*Result, error) {
	t.Helper()
	out, err := New(e.o).Run(context.Background(), jobs.Job{ID: 7, Kind: Kind, Params: raw, Attempt: 1}, jobs.LocalReporter())
	if err != nil {
		return nil, err
	}
	return out.(*Result), nil
}

var eventIDPattern = regexp.MustCompile(`^aud-[0-9a-f]{32}$`)

// The job sends each Audible place as a listen of no length at the place (a
// finish beside it at the book's end), furthest only on the server: a place
// ahead applies, one behind is "behind", and the same place again is the same
// event. Places at the start, past the runtime (a wrong match) or too old for
// the server are never sent.
func TestSyncSendsAudiblePlaces(t *testing.T) {
	e := newEnv(t)
	heard := e.now.Add(-time.Hour).Truncate(time.Millisecond)
	e.accts.places = map[string]audible.Position{
		"B000000001": {PositionMs: 1_234_500, LastUpdated: heard},                  // ahead of the server's
		"B000000002": {PositionMs: 60_000, LastUpdated: heard},                     // behind it
		"B000000003": {PositionMs: 3_550_000, LastUpdated: heard},                  // at the end of an hour
		"B000000004": {PositionMs: 500, LastUpdated: heard},                        // at the start
		"B000000005": {PositionMs: 9_000_000, LastUpdated: heard},                  // past an hour's runtime
		"B000000006": {PositionMs: 70_000, LastUpdated: heard.AddDate(0, 0, -100)}, // too old
	}
	e.srv.server["A/Two"] = 600
	raw := params(t, title("B000000001", "A/One", 36000), title("B000000002", "A/Two", 36000), title("B000000003", "A/Three", 3600),
		title("B000000004", "A/Four", 3600), title("B000000005", "A/Five", 3600), title("B000000006", "A/Six", 3600),
		title("B000000007", "A/Seven", 3600)) // no place on Audible
	res, err := e.runSync(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Titles != 7 || res.Heard != 6 || res.Sent != 4 || res.Finished != 1 || res.Applied != 3 || res.Behind != 1 ||
		res.SkippedStart != 1 || res.SkippedPastRuntime != 1 || res.SkippedOld != 1 {
		t.Fatalf("result = %+v", res)
	}
	evs := e.srv.posted[0].Events
	one := evs[0]
	stamp := heard.Format(time.RFC3339)
	if one.Kind != link.EventListen || one.Path != "A/One" || *one.FromPos != 1234.5 || *one.ToPos != 1234.5 || one.Start != "jump" ||
		one.StartedAt != stamp || one.EndedAt != stamp || one.At != "" || !eventIDPattern.MatchString(one.ID) ||
		one.ID != EventID(acctID, title("B000000001", "A/One", 36000), audible.Position{PositionMs: 1_234_500, LastUpdated: heard}, "") {
		t.Fatalf("listen = %+v", one)
	}
	fin := evs[3]
	if fin.Kind != link.EventFinish || fin.Path != "A/Three" || fin.At != stamp || fin.FromPos != nil || fin.ID == evs[2].ID ||
		!eventIDPattern.MatchString(fin.ID) {
		t.Fatalf("finish = %+v", fin)
	}
	if _, err := time.Parse(time.RFC3339, e.srv.posted[0].SentAt); err != nil {
		t.Fatalf("sent_at = %q", e.srv.posted[0].SentAt)
	}
	// The place heard is kept for the push's echo guard.
	if p, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000001"); p.HeardMs != 1_234_500 || !p.Listed || p.RuntimeS != 36000 {
		t.Fatalf("push row = %+v", p)
	}
	if a := e.account(t); a.SyncAt == "" || a.SyncError != "" || a.RefusedAt != "" {
		t.Fatalf("account = %+v", a)
	}

	// The same places again are the same events: the ids repeat.
	res, err = e.runSync(t, raw)
	if err != nil || e.srv.posted[1].Events[0].ID != one.ID || res.Applied != 1 /* the finish */ || res.Behind != 3 {
		t.Fatalf("again = %+v, %v", res, err)
	}
}

// The event id is sha256(account|asin|position_ms|last_updated|kind|library|path),
// cut to 32 hex characters: the same place on the same book is the same id; a
// moved or re-heard one, or the same place on another book (the title matched
// again: the server keeps ids per person), another.
func TestEventID(t *testing.T) {
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	one := title("B000000001", "A/One", 3600)
	pl := audible.Position{PositionMs: 5000, LastUpdated: at}
	a := EventID(acctID, one, pl, "")
	if !eventIDPattern.MatchString(a) || a != EventID(acctID, one, audible.Position{PositionMs: 5000, LastUpdated: at.In(time.FixedZone("x", 3600))}, "") ||
		a != EventID(acctID, title("B000000001", "A/One", 60), pl, "") {
		t.Fatalf("id = %q", a)
	}
	other := one
	other.LibraryID = 2
	for _, id := range []string{
		EventID(acctID, one, audible.Position{PositionMs: 5001, LastUpdated: at}, ""), EventID(acctID, title("B000000002", "A/One", 3600), pl, ""),
		EventID("efgh5678", one, pl, ""), EventID(acctID, one, audible.Position{PositionMs: 5000, LastUpdated: at.Add(time.Millisecond)}, ""),
		EventID(acctID, one, pl, link.EventFinish), EventID(acctID, one, audible.Position{PositionMs: 5000}, ""),
		EventID(acctID, title("B000000001", "A/One (right)", 3600), pl, ""), EventID(acctID, other, pl, ""),
	} {
		if id == a {
			t.Fatal("two different places share an id")
		}
	}
	// A place Audible sent no last_updated for hashes an empty last_updated.
	// The server depends on this form: it re-derives these ids to find the
	// places the Bridge dated on its own clock and corrects them by its
	// sent_at (audiosilo-server catalog.audibleUndatedID, CROSS-REPO §23), with
	// these same vectors in its TestAudibleUndatedID. A change to the formula
	// must change both.
	book := link.AudibleSyncTitle{ASIN: "B000000001", LibraryID: 1, Path: "Author/Book"}
	for kind, want := range map[string]string{
		"":               "aud-bcba95b12c2bb665a13b0dcc0cb5ddac",
		link.EventFinish: "aud-71841ac49adbb0c3fce441ddea6c829a",
	} {
		if got := EventID("acct0001", book, audible.Position{PositionMs: 500000}, kind); got != want {
			t.Errorf("undated, kind %q: id %s, want %s", kind, got, want)
		}
	}
}

// Denied: a refused account (the "Sign in again" state) doesn't sync, failing
// before asking Audible anything: a device refusal (only a sign-in again ends
// it) audible_refused, which the server keeps until the account is mapped
// again; a sign-in refusal, which ends by itself once Audible answers a call,
// audible_sign_in, which the server leaves alone (its syncs stay queued). A
// sign-in refusal after a device one keeps the device refusal (bridge#64), so
// the steps run in order. An unknown account, or one mapped on another
// server, is unknown_account.
func TestSyncRefusedAccount(t *testing.T) {
	e := newEnv(t)
	e.accts.places["B000000001"] = audible.Position{PositionMs: 60_000, LastUpdated: e.now}
	for _, step := range []struct{ kind, code string }{
		{store.RefusalSignIn, CodeSignIn},
		{store.RefusalDevice, CodeRefused},
		{store.RefusalSignIn, CodeRefused}, // the device refusal stands
	} {
		if err := e.db.RefuseAudibleAccount(context.Background(), acctID, step.kind, "refused", e.now); err != nil {
			t.Fatal(err)
		}
		_, err := e.runSync(t, params(t, title("B000000001", "A/One", 3600)))
		var f *Failure
		if !errors.As(err, &f) || f.Code != step.code {
			t.Fatalf("%s: err = %v, want %s", step.kind, err, step.code)
		}
		if e.accts.reads != 0 || len(e.srv.posted) != 0 {
			t.Fatalf("%s: a refused account asked Audible (%d) or posted (%d)", step.kind, e.accts.reads, len(e.srv.posted))
		}
	}
	// Allowed: signed in again, it syncs.
	if err := e.db.SignInAudibleAccount(context.Background(), e.account(t), e.now); err != nil {
		t.Fatal(err)
	}
	if res, err := e.runSync(t, params(t, title("B000000001", "A/One", 3600))); err != nil || res.Applied != 1 {
		t.Fatalf("signed in again = %+v, %v", res, err)
	}

	raw, _ := json.Marshal(link.AudibleSyncParams{Account: "nope0000", Titles: []link.AudibleSyncTitle{}})
	_, err := e.runSync(t, raw)
	var f *Failure
	if !errors.As(err, &f) || f.Code != CodeUnknownAccount {
		t.Fatalf("unknown account = %v", err)
	}
	e.o.ServerID = func() string { return "srv-2" }
	if _, err := e.runSync(t, params(t)); !errors.As(err, &f) || f.Code != CodeUnknownAccount {
		t.Fatalf("another server = %v", err)
	}
}

// A sync claimed while a sign-in again is finishing (the server already
// recorded it, so it ended the refusal there, but the new sign-in isn't stored
// here yet) reads the account again past it and syncs with the new sign-in:
// answering the old device refusal would mark the account refused again on
// the server. A sign-in refusal waits the same way. A refusal that stands
// once nothing is in flight is still answered (TestSyncRefusedAccount).
func TestSyncWaitsForASignInInFlight(t *testing.T) {
	for _, kind := range []string{store.RefusalDevice, store.RefusalSignIn} {
		e := newEnv(t)
		e.accts.places["B000000001"] = audible.Position{PositionMs: 60_000, LastUpdated: e.now}
		if err := e.db.RefuseAudibleAccount(context.Background(), acctID, kind, "refused", e.now); err != nil {
			t.Fatal(err)
		}
		signedIn := e.account(t)
		e.accts.onSettle = func() {
			if err := e.db.SignInAudibleAccount(context.Background(), signedIn, e.now); err != nil {
				t.Error(err)
			}
		}
		res, err := e.runSync(t, params(t, title("B000000001", "A/One", 3600)))
		if err != nil || res.Applied != 1 || e.accts.settled != 1 || e.accts.reads != 1 {
			t.Fatalf("%s: a sync during a sign-in again = %+v, %v (settled %d, reads %d)", kind, res, err, e.accts.settled, e.accts.reads)
		}
	}
	// An account that isn't refused reads once: no wait on a sign-in.
	e := newEnv(t)
	if _, err := e.runSync(t, params(t)); err != nil || e.accts.settled != 0 {
		t.Fatalf("a signed-in account = %v (settled %d)", err, e.accts.settled)
	}
}

// Denied: the params are read strictly (an unknown field, a bad account, a
// repeated or malformed ASIN, a title with no book).
func TestSyncParamsStrict(t *testing.T) {
	for name, raw := range map[string]string{
		"unknown field": `{"account":"abcd1234","titles":[],"extra":1}`,
		"title field":   `{"account":"abcd1234","titles":[{"asin":"B000000001","library_id":1,"path":"A","runtime_s":1,"x":2}]}`,
		"bad account":   `{"account":"ABC","titles":[]}`,
		"twice":         `{"account":"abcd1234","titles":[{"asin":"B000000001","library_id":1,"path":"A"},{"asin":"B000000001","library_id":1,"path":"B"}]}`,
		"bad asin":      `{"account":"abcd1234","titles":[{"asin":"b01","library_id":1,"path":"A"}]}`,
		"no book":       `{"account":"abcd1234","titles":[{"asin":"B000000001","library_id":0,"path":"A"}]}`,
		"no path":       `{"account":"abcd1234","titles":[{"asin":"B000000001","library_id":1,"path":""}]}`,
	} {
		if _, err := ParseParams(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if p, err := ParseParams(json.RawMessage(`{"account":"abcd1234","titles":[{"asin":"B000000001","library_id":1,"path":"A","runtime_s":60}]}`)); err != nil ||
		len(p.Titles) != 1 || p.Titles[0].RuntimeS != 60 {
		t.Fatalf("good params = %+v, %v", p, err)
	}
}

// A sync error never refuses the account: Audible asking to wait records the
// wait (its Retry-After) and the next job doesn't call Audible until it has
// passed; a sync turned off meanwhile ends the job quietly.
func TestSyncErrorBacksOffWithoutRefusing(t *testing.T) {
	e := newEnv(t)
	e.accts.places["B000000001"] = audible.Position{PositionMs: 60_000, LastUpdated: e.now}
	e.accts.placesErr = retryErr(t, http.StatusTooManyRequests, "600")
	raw := params(t, title("B000000001", "A/One", 3600))
	_, err := e.runSync(t, raw)
	var f *Failure
	if !errors.As(err, &f) || f.Code != CodeAudible {
		t.Fatalf("err = %v", err)
	}
	a := e.account(t)
	if a.RefusedAt != "" || a.SyncError == "" || a.SyncRetryAt != store.Timestamp(e.now.Add(10*time.Minute)) || e.changed != 1 {
		t.Fatalf("after a 429: %+v (changed %d)", a, e.changed)
	}
	e.accts.placesErr = nil
	res, err := e.runSync(t, raw)
	if err != nil || !res.Waiting || e.accts.reads != 1 {
		t.Fatalf("while waiting = %+v, %v (reads %d)", res, err, e.accts.reads)
	}
	e.now = e.now.Add(11 * time.Minute)
	if res, err := e.runSync(t, raw); err != nil || res.Applied != 1 {
		t.Fatalf("after the wait = %+v, %v", res, err)
	}
	if a := e.account(t); a.SyncError != "" || a.SyncRetryAt != "" || e.changed != 2 {
		t.Fatalf("after it went through: %+v (changed %d)", a, e.changed)
	}

	// The server's sync off since the job was queued: done, nothing recorded.
	e.srv.postErr = link.ErrSyncOff
	e.accts.places["B000000001"] = audible.Position{PositionMs: 90_000, LastUpdated: e.now}
	if res, err := e.runSync(t, raw); err != nil || !res.SyncOff {
		t.Fatalf("sync off = %+v, %v", res, err)
	}
	// A person who can't be synced for: a coded failure, recorded, not refused.
	e.srv.postErr = link.ErrUserUnavailable
	if _, err := e.runSync(t, raw); !errors.As(err, &f) || f.Code != CodeUserUnavailable {
		t.Fatalf("user unavailable = %v", err)
	}
	if a := e.account(t); a.RefusedAt != "" || a.SyncError == "" {
		t.Fatalf("after user_unavailable: %+v", a)
	}
}

// A read Audible stops partway (a rate limit after some batches) still syncs
// the places it gave (bridge#50), then fails as a whole failed read does: the
// wait is recorded and the sync isn't counted as done.
func TestSyncKeepsPlacesReadBeforeAFailure(t *testing.T) {
	e := newEnv(t)
	e.accts.places["B000000001"] = audible.Position{PositionMs: 60_000, LastUpdated: e.now}
	e.accts.places["B000000002"] = audible.Position{PositionMs: 90_000, LastUpdated: e.now}
	e.accts.placesErr = retryErr(t, http.StatusTooManyRequests, "600")
	e.accts.readFirst = []string{"B000000001"}
	raw := params(t, title("B000000001", "A/One", 3600), title("B000000002", "A/Two", 3600))
	_, err := e.runSync(t, raw)
	var f *Failure
	if !errors.As(err, &f) || f.Code != CodeAudible {
		t.Fatalf("err = %v, want the read's failure", err)
	}
	if len(e.srv.posted) != 1 || len(e.srv.posted[0].Events) != 1 || e.srv.posted[0].Events[0].Path != "A/One" {
		t.Fatalf("posted = %+v, want the one place read", e.srv.posted)
	}
	if a := e.account(t); a.SyncError == "" || a.SyncRetryAt != store.Timestamp(e.now.Add(10*time.Minute)) {
		t.Fatalf("after a partial read: %+v, want the wait recorded", a)
	}
	// Nothing read at all: nothing posted.
	e.srv.posted = nil
	e.accts.readFirst = nil
	e.now = e.now.Add(11 * time.Minute)
	if _, err := e.runSync(t, raw); !errors.As(err, &f) || len(e.srv.posted) != 0 {
		t.Fatalf("a read that failed whole = %v, posted %+v", err, e.srv.posted)
	}
}

// retryErr is an Audible 429/503 answer the client gave up on, made by the
// real client against a server that answers it.
func retryErr(t *testing.T, status int, retryAfter string) error {
	t.Helper()
	return audibleAnswer(t, status, retryAfter)
}

// The listening calls carry at most link.MaxAudibleEvents events each.
func TestSyncBatchesEvents(t *testing.T) {
	e := newEnv(t)
	var titles []link.AudibleSyncTitle
	for i := range 450 {
		asin := fmt.Sprintf("B%09d", i)
		titles = append(titles, title(asin, "A/"+asin, 36000))
		e.accts.places[asin] = audible.Position{PositionMs: 60_000, LastUpdated: e.now}
	}
	res, err := e.runSync(t, params(t, titles...))
	if err != nil || res.Sent != 450 || len(e.srv.posted) != 3 || len(e.srv.posted[0].Events) != link.MaxAudibleEvents {
		t.Fatalf("result = %+v, %v (%d calls)", res, err, len(e.srv.posted))
	}
}

// The job's result and failures carry no title or credential.
func TestSyncFailureRedacts(t *testing.T) {
	f := fail(CodeAudible, "Audible didn't answer: %v", errors.New("customer_id=A1B2C3D4E5F6G7 refused"))
	if strings.Contains(f.Message, "A1B2C3D4E5F6G7") {
		t.Fatalf("failure = %q", f.Message)
	}
}

// The job reads the push's rows before Audible's places: a push that lands
// while they are read (a move back on AudioSilo) is compared as the write
// before it, so Audible's old place, read before the write, is still the
// echo and never undoes the move back on the server.
func TestSyncReadsPushesFirst(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	raw := params(t, title("B000000001", "A/One", 36000))
	if err := e.db.ListAudibleSync(ctx, acctID, []store.AudibleSyncTitle{{ASIN: "B000000001", LibraryID: 1, Path: "A/One", RuntimeS: 36000}}); err != nil {
		t.Fatal(err)
	}
	if err := e.db.AudiblePushed(ctx, acctID, "B000000001", 7_200_000, e.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	e.srv.server["A/One"] = 3600 // moved back to 1:00:00 on AudioSilo
	e.accts.places["B000000001"] = audible.Position{PositionMs: 7_200_000, LastUpdated: e.now.Add(-time.Hour)}
	e.accts.onRead = func() {
		// The push writes the move back while Audible's answer is on its way.
		if err := e.db.AudiblePushed(ctx, acctID, "B000000001", 3_600_000, e.now); err != nil {
			t.Error(err)
		}
	}
	res, err := e.runSync(t, raw)
	if err != nil || res.SkippedEcho != 1 || res.Sent != 0 || e.srv.server["A/One"] != 3600 {
		t.Fatalf("result = %+v, %v (server %v)", res, err, e.srv.server["A/One"])
	}
}

// A sync that waits on Audible still lists its titles (the push reads the
// listing), and a wait Audible gave with no time in it still holds for at
// least errorWait. The server's own failure is no wait for Audible.
func TestSyncWaitsAndLists(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.accts.places["B000000001"] = audible.Position{PositionMs: 60_000, LastUpdated: e.now}
	e.accts.placesErr = audibleAnswer(t, http.StatusTooManyRequests, "0")
	if _, err := e.runSync(t, params(t, title("B000000001", "A/One", 3600))); err == nil {
		t.Fatal("a 429 went through")
	}
	if a := e.account(t); a.SyncRetryAt != store.Timestamp(e.now.Add(errorWait)) {
		t.Fatalf("a 429 with no wait in it: wait %q", a.SyncRetryAt)
	}
	e.accts.placesErr = nil
	res, err := e.runSync(t, params(t, title("B000000002", "A/Two", 3600)))
	if err != nil || !res.Waiting {
		t.Fatalf("while waiting = %+v, %v", res, err)
	}
	if two, err := e.db.GetAudiblePush(ctx, acctID, "B000000002"); err != nil || !two.Listed {
		t.Fatalf("a waiting sync didn't list its titles: %+v, %v", two, err)
	}
	if one, _ := e.db.GetAudiblePush(ctx, acctID, "B000000001"); one.Listed {
		t.Fatalf("a waiting sync kept a title it no longer lists: %+v", one)
	}

	e.now = e.now.Add(errorWait)
	e.srv.postErr = &link.APIError{Status: http.StatusServiceUnavailable, Code: "unavailable", RetryAfter: time.Hour}
	if _, err := e.runSync(t, params(t, title("B000000001", "A/One", 3600))); err == nil {
		t.Fatal("the server's 503 went through")
	}
	if a := e.account(t); a.SyncError == "" {
		t.Fatalf("the server's 503 wasn't recorded: %+v", a)
	} else if _, held := waiting(a, e.now); held {
		t.Fatalf("the server's 503 became a wait for Audible: %+v", a)
	}
}

// The server rejects the places on a book whose length disagrees with the
// title's runtime (duration_mismatch: another edition matched): the job counts
// them, sends each once (no retry within the job), records no place heard for
// them, and still ends done with the other titles synced.
func TestSyncDurationMismatch(t *testing.T) {
	e := newEnv(t)
	heard := e.now.Add(-time.Hour).Truncate(time.Millisecond)
	e.accts.places = map[string]audible.Position{
		"B000000001": {PositionMs: 3_590_000, LastUpdated: heard}, // at the end: a listen and a finish
		"B000000002": {PositionMs: 600_000, LastUpdated: heard},
	}
	e.srv.rejected = map[string]string{"A/One": link.SyncDurationMismatch}
	res, err := e.runSync(t, params(t, title("B000000001", "A/One", 3600), title("B000000002", "A/Two", 36000)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 2 || res.Mismatched != 2 || res.Applied != 1 || res.Sent != 3 || len(e.srv.posted) != 1 {
		t.Fatalf("result = %+v (%d posts)", res, len(e.srv.posted))
	}
	if x, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000001"); x.HeardMs != -1 {
		t.Fatalf("a rejected place was recorded as heard: %+v", x)
	}
	if x, _ := e.db.GetAudiblePush(context.Background(), acctID, "B000000002"); x.HeardMs != 600_000 {
		t.Fatalf("the other title's place wasn't recorded: %+v", x)
	}
	if a := e.account(t); a.SyncError != "" || a.SyncAt == "" {
		t.Fatalf("a mismatch is no sync error: %+v", a)
	}
}

func TestSyncOffClearsStaleHealth(t *testing.T) {
	for _, caller := range []string{"job", "push"} {
		t.Run(caller, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			wantWait := store.Timestamp(e.now.Add(-time.Minute)) // passed: inert, left as it is
			if err := e.db.AudibleSyncFailed(ctx, acctID, "previous failure", e.now.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			e.srv.postErr, e.srv.feedErr = link.ErrSyncOff, link.ErrSyncOff
			if caller == "job" {
				e.accts.places["B000000001"] = audible.Position{PositionMs: 60_000, LastUpdated: e.now}
				// Audible asks for a future wait while this request is in flight.
				// A sync-off reply clears the error but keeps that wait, as OK
				// does: the server's switch never shortens Audible's wait.
				wantWait = store.Timestamp(e.now.Add(time.Hour))
				e.accts.onRead = func() {
					if err := e.db.AudibleSyncFailed(ctx, acctID, "asked to wait", e.now.Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
				res, err := e.runSync(t, params(t, title("B000000001", "A/One", 3600)))
				if err != nil || !res.SyncOff {
					t.Fatalf("sync off: %+v, %v", res, err)
				}
			} else {
				NewPusher(e.o).Tick(ctx)
			}
			if a := e.account(t); a.SyncError != "" || a.SyncRetryAt != wantWait || a.SyncAt != "" || a.RefusedAt != "" {
				t.Fatalf("sync off kept the error, moved Audible's wait or recorded a success/refusal: %+v", a)
			}
			if e.changed != 1 {
				t.Errorf("checklist updates = %d, want 1", e.changed)
			}
		})
	}
}
