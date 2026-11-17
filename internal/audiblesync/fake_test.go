//go:build audiblefake

package audiblesync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audible/audiblefake"
	"github.com/kodestar/audiosilo-bridge/internal/audibleacct"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

type mapServer struct{}

func (mapServer) PutAudibleAccount(_ context.Context, id string, _ link.AudibleAccountInput) (*link.AudibleAccount, error) {
	return &link.AudibleAccount{ID: id}, nil
}
func (mapServer) DeleteAudibleAccount(context.Context, string) error { return nil }

// fakeAccount is an account signed in against the Audible fake through the
// real client and accounts, its credentials sealed in a vault under data.
type fakeAccount struct {
	fake  *audiblefake.Fake
	m     *audibleacct.Manager
	acct  store.AudibleAccount
	db    *store.DB
	data  string
	vault *secrets.Vault
	logs  *bytes.Buffer
	log   *slog.Logger
}

// newFakeAccount signs an account in against the fake at now; wrap, when set,
// stands in front of the fake's handler.
func newFakeAccount(t *testing.T, now func() time.Time, wrap func(http.Handler) http.Handler) *fakeAccount {
	t.Helper()
	fake := audiblefake.New(audiblefake.Options{KeyBits: 1024, Now: now, FFmpeg: "-"})
	h := fake.Handler()
	if wrap != nil {
		h = wrap(h)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	data := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(data, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	vault, _, err := secrets.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	m := audibleacct.New(audibleacct.Options{Vault: vault, DB: db, Log: log,
		Service: audibleacct.Audible(audible.WithEndpoints(audible.Endpoints{Amazon: ts.URL, AmazonAPI: ts.URL, API: ts.URL}))})
	st, err := m.Start("us")
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := browser.Get(st.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	acct, err := m.Finish(context.Background(), mapServer{}, "srv-1", audibleacct.Finish{Login: st.Login,
		RedirectURL: resp.Header.Get("Location"), Mapping: audibleacct.Mapping{UserID: 3, UserName: "Chris", LibraryID: 1, Policy: "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	return &fakeAccount{fake: fake, m: m, acct: acct, db: db, data: data, vault: vault, logs: &logs, log: log}
}

// Both ways against the fake, through the real client and accounts: the job
// reads all 30 titles' places in batches of 25 split under the fake's cap of
// 10, posts the ones it has, and the push loop writes a server place back
// once (PUT lastpositions recorded with the content reference from a license
// request), not again within 5 seconds, and the next sync doesn't echo it.
func TestSyncBothWaysAgainstTheFake(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fa := newFakeAccount(t, func() time.Time { return now }, nil)
	fake, m, acct, db, logs, log := fa.fake, fa.m, fa.acct, fa.db, fa.logs, fa.log

	srv := &stubServer{server: map[string]float64{}}
	var titles []link.AudibleSyncTitle
	for i := 1; i <= 30; i++ {
		asin := fmt.Sprintf("B0FAKE%04d", i)
		titles = append(titles, link.AudibleSyncTitle{ASIN: asin, LibraryID: 1, Path: "Books/" + asin, RuntimeS: 60000})
	}
	o := Options{Accounts: m, DB: db, Server: func() Server { return srv }, ServerID: func() string { return "srv-1" }, Log: log,
		Now: func() time.Time { return now }}
	// srv's posts name the account the stub knows; this account's id is the
	// fake's, so answer for it.
	posted := &accountStub{stubServer: srv, id: acct.ID}
	o.Server = func() Server { return posted }
	raw := mustParams(t, acct.ID, titles)
	out, err := New(o).Run(context.Background(), jobs.Job{ID: 1, Kind: Kind, Params: raw, Attempt: 1}, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	res := out.(*Result)
	// The fake's 7 places: the two heard 200 and 400 days ago are too old for
	// the server, the other 5 are sent. 25 ASINs a request, split (12 + 13,
	// then halves) under the fake's 10, then the last 5.
	if res.Heard != 7 || res.Sent != 5 || res.Applied != 5 || res.SkippedOld != 2 {
		t.Fatalf("result = %+v", res)
	}
	sizes := fake.Calls().LastPositionsSizes
	if sizes[0] != 25 || !contains(sizes, 5) {
		t.Fatalf("lastpositions sizes = %v (the batch of 25 split under the cap)", sizes)
	}
	for _, n := range sizes {
		if n > 25 {
			t.Fatalf("a request carried %d ASINs", n)
		}
	}

	// The push: a place on the server, still for 3 minutes since the Bridge
	// read it, is written once.
	p := NewPusher(o)
	p.Tick(context.Background()) // the feed's start
	srv.add("Books/B0FAKE0006", "B0FAKE0006", 1234.5, now, "web-1")
	p.Tick(context.Background()) // read: still from now
	now = now.Add(4 * time.Minute)
	p.Tick(context.Background())
	puts := fake.Calls().PutPositions
	if len(puts) != 1 || puts[0].ASIN != "B0FAKE0006" || puts[0].PositionMs != 1_234_500 || puts[0].ACR != "FAKEACR-B0FAKE0006" ||
		puts[0].At == "" {
		t.Fatalf("puts = %+v", puts)
	}
	// Within 5 s of it: not again.
	srv.add("Books/B0FAKE0006", "B0FAKE0006", 1238, now, "web-1")
	p.Tick(context.Background()) // read: still from now
	now = now.Add(4 * time.Minute)
	p.Tick(context.Background())
	if puts := fake.Calls().PutPositions; len(puts) != 1 {
		t.Fatalf("a second push within 5 s: %+v", puts)
	}
	// The next sync reads the place written back and doesn't send it.
	before := len(posted.posted)
	out, err = New(o).Run(context.Background(), jobs.Job{ID: 2, Kind: Kind, Params: raw, Attempt: 1}, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	if res := out.(*Result); res.SkippedEcho != 1 {
		t.Fatalf("the echo was sent: %+v", res)
	}
	for _, call := range posted.posted[before:] {
		for _, ev := range call.Events {
			if ev.Path == "Books/B0FAKE0006" {
				t.Fatalf("the place written back was posted again: %+v", ev)
			}
		}
	}
	// A place heard on another device (the fake's control) is sent.
	fake.SetPosition("B0FAKE0007", 90_000, time.Minute)
	out, _ = New(o).Run(context.Background(), jobs.Job{ID: 3, Kind: Kind, Params: raw, Attempt: 1}, jobs.LocalReporter())
	if res := out.(*Result); res.Applied != 1 {
		t.Fatalf("a new Audible place: %+v", res)
	}
	for _, secret := range []string{"Atna|", "Atnr|", "{enc:", "PRIVATE KEY"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log holds a credential: %s", logs.String())
		}
	}
}

// The push loop's failures carry no credential: Audible answering a license
// request (a 500) and a place (a 400) with a body quoting the account's real
// tokens, customer id and serial leaves none in sync_error, the log, or any file under the data
// dir but the vault's sealed one (bridge#50: the sync job's walk is in
// internal/app; this is the push loop's).
func TestPushFailuresHoldNoCredential(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var failing atomic.Value // "license" | "put", the body set below
	var body atomic.Value
	fa := newFakeAccount(t, func() time.Time { return now }, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			what, _ := failing.Load().(string)
			msg, _ := body.Load().(string)
			switch {
			case what == "license" && strings.HasSuffix(r.URL.Path, "/licenserequest"):
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, msg)
			case what == "put" && r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/1.0/lastpositions/"):
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, msg)
			default:
				next.ServeHTTP(w, r)
			}
		})
	})
	raw, err := fa.vault.Get(secrets.AudibleName(fa.acct.ID))
	if err != nil {
		t.Fatal(err)
	}
	creds, err := audible.ParseCredentials(raw)
	if err != nil || creds.AccessToken == "" || creds.ADPToken == "" {
		t.Fatalf("sealed = %+v, %v", creds, err)
	}
	// Audible's answers can quote what Amazon holds; the device key signs
	// requests and is never sent, so no answer holds it, but no file or log
	// line may either.
	canaries := []string{creds.AccessToken, creds.RefreshToken, creds.ADPToken, creds.CustomerID, creds.DeviceSerial}
	body.Store("refused " + strings.Join(canaries, " "))
	if lines := strings.Split(creds.DevicePrivateKey, "\n"); len(lines) > 1 {
		canaries = append(canaries, lines[1])
	}

	ctx := context.Background()
	const asin = "B0FAKE0006"
	if err := fa.db.ListAudibleSync(ctx, fa.acct.ID, []store.AudibleSyncTitle{{ASIN: asin, LibraryID: 1, Path: "Books/" + asin, RuntimeS: 60000}}); err != nil {
		t.Fatal(err)
	}
	srv := &stubServer{server: map[string]float64{}}
	p := NewPusher(Options{Accounts: fa.m, DB: fa.db, Server: func() Server { return srv }, ServerID: func() string { return "srv-1" },
		Log: fa.log, Now: func() time.Time { return now }})
	clean := func(step, want string) {
		t.Helper()
		a, err := fa.db.GetAudibleAccount(ctx, fa.acct.ID)
		if err != nil || !strings.Contains(a.SyncError, want) {
			t.Fatalf("%s: sync_error = %q, %v (want %q)", step, a.SyncError, err, want)
		}
		texts := map[string]string{"sync_error": a.SyncError, "log": fa.logs.String()}
		// A file or dir the walk can't read fails the test: it would leave a
		// credential written there unseen.
		if err := filepath.WalkDir(fa.data, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			b, err := os.ReadFile(path)
			texts[path] = string(b)
			return err
		}); err != nil {
			t.Fatalf("%s: reading the data dir: %v", step, err)
		}
		for what, text := range texts {
			for _, c := range canaries {
				if c != "" && strings.Contains(text, c) {
					t.Fatalf("%s: %s holds a credential", step, what)
				}
			}
		}
	}

	p.Tick(ctx) // the feed's start
	srv.add("Books/"+asin, asin, 1234.5, now, "web-1")
	p.Tick(ctx) // read: still from now
	now = now.Add(4 * time.Minute)
	failing.Store("license")
	p.Tick(ctx)
	clean("a failed license request", "reading the title's content reference on Audible")
	now = now.Add(3 * time.Minute) // past the loop's wait
	failing.Store("put")
	p.Tick(ctx)
	clean("a refused place", "writing the place to Audible")
	if puts := fa.fake.Calls().PutPositions; len(puts) != 0 {
		t.Fatalf("a place was written: %+v", puts)
	}
}

// accountStub is stubServer answering for another account id.
type accountStub struct {
	*stubServer
	id string
}

func (s *accountStub) PostAudibleListening(ctx context.Context, account string, in link.AudibleListening) (*link.SyncAnswer, error) {
	if account != s.id {
		return nil, link.ErrUnknownAccount
	}
	return s.stubServer.PostAudibleListening(ctx, acctID, in)
}

func mustParams(t *testing.T, account string, titles []link.AudibleSyncTitle) []byte {
	t.Helper()
	raw := params(t, titles...)
	return bytes.Replace(raw, []byte(`"account":"`+acctID+`"`), []byte(`"account":"`+account+`"`), 1)
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
