package audibleacct

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The secrets a stub registration hands out: none may appear anywhere but the
// vault.
const (
	stubAccess  = "Atna|ACCESS-SECRET-1"
	stubRefresh = "Atnr|REFRESH-SECRET-1"
	stubADP     = "{enc:}ADP-SECRET-1"
	stubKey     = "-----BEGIN RSA PRIVATE KEY-----\nKEY-SECRET-1\n-----END RSA PRIVATE KEY-----\n"
	stubCode    = "AUTHCODE-SECRET-1"
	stubCust    = "amzn1.account.CUSTOMER-SECRET-1"
)

var stubSecrets = []string{"ACCESS-SECRET", "REFRESH-SECRET", "ADP-SECRET", "KEY-SECRET", "AUTHCODE-SECRET", "CUSTOMER-SECRET"}

// stubService is Audible without a network: a registration per code, a
// library, refusals and the deregistrations it was asked for.
type stubService struct {
	mu           sync.Mutex
	n            int
	registerErr  error
	libraryErr   error
	library      func(audible.Credentials) ([]source.Record, error) // replaces the library when set
	deregistered []string                                           // device serials
	deregErr     error
	customer     string
	barrier      *sync.WaitGroup // when set, each Register waits on it after its Done
	oldProfile   bool            // Register answers a sign-in with no profile recorded (an earlier Bridge's)
	// orders and products are the preorders' calls: the orders listing (or
	// ordersErr), the catalog by ASIN (with productsErr, what it read before
	// failing), and the ASINs each catalog read asked for.
	orders      []audible.Order
	ordersErr   error
	products    map[string]source.Record
	productsErr error
	asked       [][]string
}

func (s *stubService) AuthorizeURL(mp audible.Marketplace, pkce audible.PKCE, serial string) string {
	return "https://www.amazon." + mp.TLD + "/ap/signin?challenge=" + pkce.Challenge
}

func (s *stubService) Register(_ context.Context, mp audible.Marketplace, code string, _ audible.PKCE, serial string) (audible.Credentials, error) {
	if s.barrier != nil {
		// Each sign-in waits for the others here, but never forever: a test
		// whose sign-ins can't all reach Register fails instead of hanging.
		s.barrier.Done()
		met := make(chan struct{})
		go func() { s.barrier.Wait(); close(met) }()
		select {
		case <-met:
		case <-time.After(10 * time.Second):
			return audible.Credentials{}, errors.New("barrier: the other sign-ins never reached Register")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registerErr != nil {
		return audible.Credentials{}, s.registerErr
	}
	if code != stubCode {
		return audible.Credentials{}, errors.New("bad code")
	}
	s.n++
	cust := s.customer
	if cust == "" {
		cust = stubCust
	}
	profile := audible.ProfileVersion
	if s.oldProfile {
		profile = ""
	}
	return audible.Credentials{AccessToken: stubAccess, RefreshToken: stubRefresh, ADPToken: stubADP, DevicePrivateKey: stubKey,
		DeviceSerial: serial, CustomerID: cust, Marketplace: mp.Code, ExpiresAt: time.Now().Add(time.Hour).Unix(), Profile: profile}, nil
}

func (s *stubService) Deregister(ctx context.Context, creds audible.Credentials) error {
	if err := ctx.Err(); err != nil {
		return err // as the HTTP call would
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deregistered = append(s.deregistered, creds.DeviceSerial)
	return s.deregErr
}

func (s *stubService) Library(_ context.Context, creds audible.Credentials) ([]source.Record, error) {
	if s.library != nil {
		return s.library(creds)
	}
	if s.libraryErr != nil {
		return nil, s.libraryErr
	}
	return []source.Record{{ASIN: "B0FAKE0001", Title: "One"}}, nil
}

func (s *stubService) Title(_ context.Context, creds audible.Credentials, asin string) (source.Record, error) {
	if s.libraryErr != nil {
		return source.Record{}, s.libraryErr
	}
	return source.Record{ASIN: asin, Title: "Title " + asin + " " + creds.DeviceSerial}, nil
}

func (s *stubService) License(_ context.Context, creds audible.Credentials, _ string) (audible.License, error) {
	if s.libraryErr != nil {
		return audible.License{}, s.libraryErr
	}
	return audible.License{URL: "https://cdn.example/f", Key: "KEY", IV: "IV" + creds.DeviceSerial}, nil
}

func (s *stubService) Download(context.Context, audible.Credentials, string, string, audible.DownloadOptions) error {
	return nil
}

func (s *stubService) LastPositions(_ context.Context, creds audible.Credentials, asins []string) (map[string]audible.Position, error) {
	if s.libraryErr != nil {
		return nil, s.libraryErr
	}
	out := map[string]audible.Position{}
	for _, a := range asins {
		out[a] = audible.Position{PositionMs: 1000}
	}
	return out, nil
}

func (s *stubService) ContentACR(_ context.Context, _ audible.Credentials, asin string) (string, error) {
	if s.libraryErr != nil {
		return "", s.libraryErr
	}
	return "ACR-" + asin, nil
}

func (s *stubService) SetLastPosition(context.Context, audible.Credentials, string, string, int64) error {
	return s.libraryErr
}

func (s *stubService) Orders(context.Context, audible.Credentials) ([]audible.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.orders, s.ordersErr
}

func (s *stubService) Products(_ context.Context, _ audible.Credentials, asins []string) ([]source.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, asins)
	var out []source.Record
	for _, a := range asins {
		if r, ok := s.products[a]; ok {
			out = append(out, r)
		}
	}
	return out, s.productsErr
}

// stubServer is the server's account routes.
type stubServer struct {
	mu      sync.Mutex
	put     map[string]link.AudibleAccountInput
	deleted []string
	putErr  error
	delErr  error
	onPut   func() // runs as a PUT is applied
}

func (s *stubServer) PutAudibleAccount(_ context.Context, id string, in link.AudibleAccountInput) (*link.AudibleAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.onPut != nil {
		s.onPut()
	}
	if s.putErr != nil {
		return nil, s.putErr
	}
	if s.put == nil {
		s.put = map[string]link.AudibleAccountInput{}
	}
	s.put[id] = in
	return &link.AudibleAccount{ID: id}, nil
}

func (s *stubServer) DeleteAudibleAccount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err // as the HTTP call would
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return s.delErr
}

type env struct {
	m     *Manager
	svc   *stubService
	srv   *stubServer
	vault *secrets.Vault
	db    *store.DB
	logs  *bytes.Buffer
	data  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
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
	svc := &stubService{}
	m := New(Options{Vault: vault, DB: db, Service: svc, Log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	return &env{m: m, svc: svc, srv: &stubServer{}, vault: vault, db: db, logs: &logs, data: data}
}

func mapping() Mapping {
	return Mapping{UserID: 3, UserName: "Chris", LibraryID: 1, Policy: "manual", Sync: true}
}

func (e *env) signIn(t *testing.T) store.AudibleAccount {
	t.Helper()
	st, err := e.m.Start("us")
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: st.Login,
		RedirectURL: "https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=" + stubCode, Mapping: mapping()})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// noSecrets fails when any stub secret is anywhere under the data dir but the
// vault's sealed files, or in the logs.
func (e *env) noSecrets(t *testing.T) {
	t.Helper()
	for _, s := range stubSecrets {
		if strings.Contains(e.logs.String(), s) {
			t.Fatalf("the log holds %s: %s", s, e.logs)
		}
	}
	err := filepath.Walk(e.data, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, _ := os.ReadFile(path)
		for _, s := range stubSecrets {
			if bytes.Contains(raw, []byte(s)) {
				t.Errorf("%s holds %s in the clear", path, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSignInSealsTheCredentialsAndMapsOnTheServerFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	st, err := e.m.Start("US")
	if err != nil || st.Marketplace != "us" || !strings.Contains(st.URL, "amazon.com/ap/signin") ||
		time.Until(st.ExpiresAt) > LoginTTL || !IDPattern.MatchString(st.Login) {
		t.Fatalf("start = %+v, %v", st, err)
	}
	if _, err := e.m.Start("zz"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an unknown marketplace = %v", err)
	}
	// A pasted page with no code keeps the sign-in for another try.
	_, err = e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: st.Login, RedirectURL: "https://www.amazon.com/", Mapping: mapping()})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("no code = %v", err)
	}
	a, err := e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: st.Login,
		RedirectURL: " https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=" + stubCode + " ", Mapping: mapping()})
	if err != nil {
		t.Fatal(err)
	}
	if !IDPattern.MatchString(a.ID) || a.Label != "Chris (US)" || a.ServerID != "srv-1" || a.UserID != 3 || a.LibraryID != 1 ||
		a.Policy != "manual" || !a.Sync || a.CustomerHash == "" || a.LastOKAt == "" {
		t.Fatalf("account = %+v", a)
	}
	if in := e.srv.put[a.ID]; in != (link.AudibleAccountInput{Label: "Chris (US)", Marketplace: "us", UserID: 3, LibraryID: 1, Policy: "manual", Sync: true}) {
		t.Fatalf("server mapping = %+v", in)
	}
	// The login is spent.
	if _, err := e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: st.Login,
		RedirectURL: "https://x/ap/maplanding?openid.oa2.authorization_code=" + stubCode, Mapping: mapping()}); !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("a spent login = %v", err)
	}

	// The vault round trip: the sealed value opens to the same credentials.
	raw, err := e.vault.Get(secrets.AudibleName(a.ID))
	if err != nil || raw == "" {
		t.Fatalf("sealed = %q, %v", raw, err)
	}
	creds, err := audible.ParseCredentials(raw)
	if err != nil || creds.AccessToken != stubAccess || creds.RefreshToken != stubRefresh || creds.ADPToken != stubADP ||
		creds.DevicePrivateKey != stubKey || creds.CustomerID != stubCust || creds.Marketplace != "us" {
		t.Fatalf("round trip = %+v, %v", creds, err)
	}
	if names := secrets.Names(); strings.Contains(strings.Join(names, ","), "audible") {
		t.Fatalf("an operator never sets an Audible sign-in: %v", names)
	}
	e.noSecrets(t)
}

func TestSignInRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	st, _ := e.m.Start("uk")
	redirect := "https://www.amazon.co.uk/ap/maplanding?openid.oa2.authorization_code=" + stubCode
	for _, bad := range []Mapping{
		{UserID: 3, LibraryID: 1, Policy: "everything"},
		{UserID: 0, LibraryID: 1, Policy: "all"},
		{UserID: 3, LibraryID: 0, Policy: "all"},
	} {
		if _, err := e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: st.Login, RedirectURL: redirect, Mapping: bad}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v = %v", bad, err)
		}
	}
	if _, err := e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: "nosuchlogin", RedirectURL: redirect, Mapping: mapping()}); !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("an unknown login = %v", err)
	}
	if _, err := e.m.Finish(context.Background(), nil, "", Finish{Login: st.Login, RedirectURL: redirect, Mapping: mapping()}); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("finishing while unpaired = %v", err)
	}

	// The server refuses the mapping: the new device is deregistered again and
	// nothing is kept.
	e.srv.putErr = &link.APIError{Status: 404, Code: "unknown_user", Message: "no such user"}
	_, err := e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: st.Login, RedirectURL: redirect, Mapping: mapping()})
	var apiErr *link.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "unknown_user" {
		t.Fatalf("a refused mapping = %v", err)
	}
	if len(e.svc.deregistered) != 1 {
		t.Fatalf("the device stays registered: %v", e.svc.deregistered)
	}
	if list, _ := e.m.Accounts(context.Background()); len(list) != 0 {
		t.Fatalf("a refused sign-in kept an account: %+v", list)
	}
	// The PUT may have been applied before its answer was lost: the new
	// account is removed on the server too.
	if len(e.srv.deleted) != 1 || !IDPattern.MatchString(e.srv.deleted[0]) {
		t.Fatalf("server deletes = %v", e.srv.deleted)
	}

	// Amazon refuses the code.
	e.srv.putErr = nil
	e.svc.registerErr = errors.New("400 invalid_grant")
	st, _ = e.m.Start("uk")
	if _, err := e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: st.Login, RedirectURL: redirect, Mapping: mapping()}); !errors.Is(err, ErrRegister) {
		t.Fatalf("a refused code = %v", err)
	}

	// An expired sign-in.
	now := time.Now()
	e.m.o.Now = func() time.Time { return now }
	e.svc.registerErr = nil
	st, _ = e.m.Start("uk")
	e.m.o.Now = func() time.Time { return now.Add(LoginTTL) }
	if _, err := e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: st.Login, RedirectURL: redirect, Mapping: mapping()}); !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("an expired login = %v", err)
	}
	e.noSecrets(t)
}

func TestLoginsAreBounded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first, _ := e.m.Start("us")
	for range maxLogins {
		if _, err := e.m.Start("us"); err != nil {
			t.Fatal(err)
		}
	}
	e.m.mu.Lock()
	n := len(e.m.logins)
	_, kept := e.m.logins[first.Login]
	e.m.mu.Unlock()
	if n != maxLogins || kept {
		t.Fatalf("logins = %d, oldest kept %v", n, kept)
	}
}

// A second sign-in of the same Amazon account replaces the first: the same id,
// the new device's sign-in, the old device deregistered, a refusal forgotten.
func TestSignInAgainReplacesTheDevice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	first, _ := e.m.credentials(a.ID)
	if err := e.db.RefuseAudibleAccount(context.Background(), a.ID, store.RefusalSignIn, RefusedText, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.db.AudibleSyncFailed(context.Background(), a.ID, "old sync failure", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	b := e.signIn(t)
	second, _ := e.m.credentials(b.ID)
	if b.ID != a.ID || b.RefusedAt != "" || b.SyncError != "" || b.SyncRetryAt != "" || second.DeviceSerial == first.DeviceSerial {
		t.Fatalf("again = %+v (first %+v)", b, a)
	}
	if len(e.svc.deregistered) != 1 || e.svc.deregistered[0] != first.DeviceSerial {
		t.Fatalf("deregistered = %v, want the first device %s", e.svc.deregistered, first.DeviceSerial)
	}
	// Another Amazon account is another account.
	e.svc.customer = "amzn1.account.OTHER"
	c := e.signIn(t)
	if c.ID == a.ID {
		t.Fatal("another Amazon account replaced the first")
	}
	if list, _ := e.m.Accounts(context.Background()); len(list) != 2 {
		t.Fatalf("accounts = %+v", list)
	}
}

// An account whose device registered on an older profile (or before the
// profile was recorded) is flagged; signing in again records the current
// profile, ends the flag and deregisters the old device.
func TestSignInAgainEndsAnOldProfile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.svc.oldProfile = true
	a := e.signIn(t)
	if a.DeviceProfile != "" || !OldProfile(a) {
		t.Fatalf("an earlier Bridge's sign-in = %+v", a)
	}
	first, _ := e.m.credentials(a.ID)
	e.svc.oldProfile = false
	b := e.signIn(t)
	if b.ID != a.ID || b.DeviceProfile != audible.ProfileVersion || OldProfile(b) {
		t.Fatalf("signed in again = %+v", b)
	}
	if len(e.svc.deregistered) != 1 || e.svc.deregistered[0] != first.DeviceSerial {
		t.Fatalf("deregistered = %v, want the old device", e.svc.deregistered)
	}
	if !OldProfile(store.AudibleAccount{DeviceProfile: "3.56.2/17.0/35602678"}) {
		t.Fatal("an older recorded profile reads as current")
	}
}

// Two sign-ins of one Amazon account finishing at once (both past Amazon's
// registration before either looks for the account) make one account: the
// later replaces the earlier, whose device is deregistered.
func TestConcurrentSignInsOfOneAmazonAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	logins := make([]string, 2)
	for i := range logins {
		st, err := e.m.Start("us")
		if err != nil {
			t.Fatal(err)
		}
		logins[i] = st.Login
	}
	var barrier sync.WaitGroup
	barrier.Add(len(logins))
	e.svc.barrier = &barrier
	got := make([]store.AudibleAccount, len(logins))
	errs := make([]error, len(logins))
	var wg sync.WaitGroup
	for i, login := range logins {
		wg.Go(func() {
			got[i], errs[i] = e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: login,
				RedirectURL: "https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=" + stubCode, Mapping: mapping()})
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("sign-in %d: %v", i, err)
		}
	}
	if got[0].ID != got[1].ID {
		t.Fatalf("two accounts: %s and %s", got[0].ID, got[1].ID)
	}
	if list, _ := e.m.Accounts(context.Background()); len(list) != 1 {
		t.Fatalf("accounts = %+v", list)
	}
	kept, err := e.m.credentials(got[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.mu.Lock()
	defer e.svc.mu.Unlock()
	if e.svc.n != 2 || len(e.svc.deregistered) != 1 || e.svc.deregistered[0] == kept.DeviceSerial {
		t.Fatalf("registered %d, deregistered %v, the vault keeps %s", e.svc.n, e.svc.deregistered, kept.DeviceSerial)
	}
	if len(e.srv.deleted) != 0 {
		t.Fatalf("the server lost the account: %v", e.srv.deleted)
	}
	e.noSecrets(t)
}

// A sign-in again that fails once the server recorded it puts the account back
// as it was: its earlier sign-in in the vault (its device still works), its
// earlier mapping on the server, only the new device deregistered.
func TestSignInAgainThatFailsKeepsTheEarlierSignIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	retry := time.Now().Add(time.Hour)
	if err := e.db.AudibleSyncFailed(context.Background(), a.ID, "previous sync failure", retry); err != nil {
		t.Fatal(err)
	}
	first, _ := e.m.credentials(a.ID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The page goes away just as the server answers: the store write fails.
	e.srv.onPut = cancel
	st, _ := e.m.Start("us")
	_, err := e.m.Finish(ctx, e.srv, "srv-1", Finish{Login: st.Login,
		RedirectURL: "https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=" + stubCode,
		Mapping:     Mapping{UserID: 4, UserName: "Sam", LibraryID: 2, Policy: "all"}})
	if err == nil {
		t.Fatal("the sign-in was kept")
	}
	if now, err := e.m.credentials(a.ID); err != nil || now.DeviceSerial != first.DeviceSerial {
		t.Fatalf("the vault holds %+v, %v; want the first device %s", now.DeviceSerial, err, first.DeviceSerial)
	}
	if len(e.svc.deregistered) != 1 || e.svc.deregistered[0] == first.DeviceSerial {
		t.Fatalf("deregistered = %v, want only the new device", e.svc.deregistered)
	}
	if in := e.srv.put[a.ID]; in.UserID != 3 || in.LibraryID != 1 || in.Label != "Chris (US)" {
		t.Fatalf("the server keeps %+v", in)
	}
	if kept, _ := e.m.Account(context.Background(), a.ID); kept.UserID != 3 || kept.LibraryID != 1 ||
		kept.SyncError != "previous sync failure" || kept.SyncRetryAt != store.Timestamp(retry) {
		t.Fatalf("the account = %+v", kept)
	}

	// An earlier sign-in that can't be opened can't be deregistered: the log
	// says so.
	e.srv.onPut = nil
	if err := e.vault.Delete(secrets.AudibleName(a.ID)); err != nil {
		t.Fatal(err)
	}
	if b := e.signIn(t); b.ID != a.ID {
		t.Fatalf("again = %+v", b)
	}
	if !strings.Contains(e.logs.String(), "earlier sign-in couldn't be opened") {
		t.Fatalf("no warning: %s", e.logs)
	}
	e.noSecrets(t)
}

func TestMapRecordsOnTheServerThenHere(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	// The name the Bridge made follows the person (the form sends it back as it
	// is); a typed one stays.
	made := Mapping{Label: a.Label, UserID: 4, UserName: "Sam", LibraryID: 1, Policy: "manual"}
	if got, err := e.m.Map(context.Background(), e.srv, "srv-1", a.ID, made); err != nil || got.Label != "Sam (US)" ||
		e.srv.put[a.ID].Label != "Sam (US)" {
		t.Fatalf("mapped to Sam = %+v, %v", got, err)
	}
	mp := Mapping{Label: "  Kids'   shelf ", UserID: 4, UserName: "Sam", LibraryID: 2, Policy: "SERIES"}
	got, err := e.m.Map(context.Background(), e.srv, "srv-2", a.ID, mp)
	if err != nil || got.Label != "Kids' shelf" || got.UserID != 4 || got.LibraryID != 2 || got.Policy != "series" || got.Sync ||
		got.ServerID != "srv-2" {
		t.Fatalf("mapped = %+v, %v", got, err)
	}
	if in := e.srv.put[a.ID]; in.UserID != 4 || in.Policy != "series" || in.Marketplace != "us" {
		t.Fatalf("server = %+v", in)
	}
	e.srv.putErr = &link.APIError{Status: 400, Code: "not_books"}
	if _, err := e.m.Map(context.Background(), e.srv, "srv-2", a.ID, mapping()); err == nil {
		t.Fatal("a refused mapping")
	}
	if kept, _ := e.m.Account(context.Background(), a.ID); kept.LibraryID != 2 {
		t.Fatalf("a refused mapping changed the account: %+v", kept)
	}
	if _, err := e.m.Map(context.Background(), e.srv, "srv-2", "nosuch00", mapping()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if _, err := e.m.Account(context.Background(), "../../x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a malformed id = %v", err)
	}
}

// bridge#40: Map runs one at a time with a sign-in and a removal (finishMu):
// a removal started while a remap waits on the server waits for it, rather
// than forget the account between the server's record and this one.
func TestMapHoldsTheAccountsLock(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.srv.onPut = func() { once.Do(func() { close(entered); <-release }) }
	mapped := make(chan error, 1)
	go func() {
		_, err := e.m.Map(context.Background(), e.srv, "srv-1", a.ID, Mapping{UserID: 4, UserName: "Sam", LibraryID: 2, Policy: "manual"})
		mapped <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the remap never reached the server")
	}
	removed := make(chan error, 1)
	go func() {
		_, err := e.m.Remove(context.Background(), e.srv, a.ID)
		removed <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := e.db.GetAudibleAccount(context.Background(), a.ID); err != nil {
		t.Fatalf("the removal ran during the remap: %v", err)
	}
	close(release)
	if err := <-mapped; err != nil {
		t.Fatalf("map = %v", err)
	}
	if err := <-removed; err != nil {
		t.Fatalf("remove = %v", err)
	}
	if _, err := e.m.Account(context.Background(), a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the account is kept: %v", err)
	}
}

// bridge#40: a mapping that comes with no name for the person (the server's
// people couldn't be read) keeps the stored name when it is the same person,
// on a remap and on a sign-in again; another person has none to keep.
func TestMappingKeepsTheUserName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	got, err := e.m.Map(context.Background(), e.srv, "srv-1", a.ID, Mapping{Label: a.Label, UserID: 3, LibraryID: 1, Policy: "manual"})
	if err != nil || got.UserName != "Chris" || got.Label != "Chris (US)" {
		t.Fatalf("same person = %+v, %v", got, err)
	}
	st, err := e.m.Start("us")
	if err != nil {
		t.Fatal(err)
	}
	nameless := mapping()
	nameless.UserName = ""
	again, err := e.m.Finish(context.Background(), e.srv, "srv-1", Finish{Login: st.Login,
		RedirectURL: "https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=" + stubCode, Mapping: nameless})
	if err != nil || again.ID != a.ID || again.UserName != "Chris" || again.Label != "Chris (US)" {
		t.Fatalf("signed in again = %+v, %v", again, err)
	}
	other, err := e.m.Map(context.Background(), e.srv, "srv-1", a.ID, Mapping{Label: again.Label, UserID: 4, LibraryID: 1, Policy: "manual"})
	if err != nil || other.UserName != "" || other.Label != "Audible (US)" {
		t.Fatalf("another person = %+v, %v", other, err)
	}
	// Denied: the same person id on another server is someone else.
	if _, err := e.m.Map(context.Background(), e.srv, "srv-1", a.ID, Mapping{UserID: 3, UserName: "Chris", LibraryID: 1, Policy: "manual"}); err != nil {
		t.Fatal(err)
	}
	moved, err := e.m.Map(context.Background(), e.srv, "srv-2", a.ID, Mapping{Label: "Chris (US)", UserID: 3, LibraryID: 1, Policy: "manual"})
	if err != nil || moved.UserName != "" || moved.Label != "Audible (US)" {
		t.Fatalf("same id on another server = %+v, %v", moved, err)
	}
}

// Remove forgets locally first, then tells the server and deregisters; a
// failure of either is a warning, never the account kept.
func TestRemoveForgetsFirstThenDeregisters(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	creds, _ := e.m.credentials(a.ID)
	warning, err := e.m.Remove(context.Background(), e.srv, a.ID)
	if err != nil || warning != "" {
		t.Fatalf("remove = %q, %v", warning, err)
	}
	if ok, _ := e.vault.Present(secrets.AudibleName(a.ID)); ok {
		t.Fatal("the sign-in is still sealed")
	}
	if _, err := e.m.Account(context.Background(), a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the account is kept: %v", err)
	}
	if len(e.srv.deleted) != 1 || e.srv.deleted[0] != a.ID || len(e.svc.deregistered) != 1 || e.svc.deregistered[0] != creds.DeviceSerial {
		t.Fatalf("server %v, deregistered %v", e.srv.deleted, e.svc.deregistered)
	}
	if _, err := e.m.Remove(context.Background(), e.srv, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing twice = %v", err)
	}

	b := e.signIn(t)
	e.srv.delErr = errors.New("unreachable")
	e.svc.deregErr = errors.New("500")
	warning, err = e.m.Remove(context.Background(), e.srv, b.ID)
	if err != nil || !strings.Contains(warning, "server didn't confirm") || !strings.Contains(warning, "device list") {
		t.Fatalf("remove with failures = %q, %v", warning, err)
	}
	if list, _ := e.m.Accounts(context.Background()); len(list) != 0 {
		t.Fatalf("kept = %+v", list)
	}
	c := e.signIn(t)
	if warning, err = e.m.Remove(context.Background(), nil, c.ID); err != nil || !strings.Contains(warning, "isn't paired") {
		t.Fatalf("remove unpaired = %q, %v", warning, err)
	}
	e.noSecrets(t)
}

// A page that goes away once Remove has forgotten the account doesn't stop it:
// the server and Amazon are still told (the sealed sign-in is gone).
func TestRemoveRunsToItsEnd(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	creds, _ := e.m.credentials(a.ID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.m.o.OnChange = cancel // runs once the account is forgotten here
	warning, err := e.m.Remove(ctx, e.srv, a.ID)
	if err != nil || warning != "" {
		t.Fatalf("remove = %q, %v", warning, err)
	}
	if len(e.srv.deleted) != 1 || len(e.svc.deregistered) != 1 || e.svc.deregistered[0] != creds.DeviceSerial {
		t.Fatalf("server %v, deregistered %v", e.srv.deleted, e.svc.deregistered)
	}
}

// The position sync's calls go through the sign-in, but a 401/403 on one
// never refuses the account (decision 6: sync errors back off, the poll and
// the imports own the refusal).
func TestSyncCallsNeverRefuse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	ctx := context.Background()
	got, err := e.m.Positions(ctx, a.ID, []string{"B0FAKE0001"})
	if err != nil || got["B0FAKE0001"].PositionMs != 1000 {
		t.Fatalf("positions = %v, %v", got, err)
	}
	if acr, err := e.m.ContentACR(ctx, a.ID, "B0FAKE0001"); err != nil || acr != "ACR-B0FAKE0001" {
		t.Fatalf("acr = %q, %v", acr, err)
	}
	if err := e.m.SetPosition(ctx, a.ID, "B0FAKE0001", "ACR", 5000); err != nil {
		t.Fatal(err)
	}
	changed := 0
	e.m.o.OnChange = func() { changed++ }
	e.svc.libraryErr = fmt.Errorf("%w (HTTP 403)", ErrRefused)
	if _, err := e.m.Positions(ctx, a.ID, []string{"B0FAKE0001"}); err == nil {
		t.Fatal("a refused read answered")
	}
	if _, err := e.m.ContentACR(ctx, a.ID, "B0FAKE0001"); err == nil {
		t.Fatal("a refused acr answered")
	}
	if err := e.m.SetPosition(ctx, a.ID, "B0FAKE0001", "ACR", 5000); err == nil {
		t.Fatal("a refused write answered")
	}
	if acct, _ := e.m.Account(ctx, a.ID); acct.RefusedAt != "" || acct.RefusalKind != "" || changed != 0 {
		t.Fatalf("a sync call refused the account: %+v (changed %d)", acct, changed)
	}
	if _, err := e.m.Positions(ctx, "nope0000", nil); err == nil {
		t.Fatal("an unknown account read places")
	}
	e.noSecrets(t)
}

func TestLibraryMarksARefusal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	changed := 0
	e.m.o.OnChange = func() { changed++ }
	recs, err := e.m.Library(context.Background(), a.ID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("library = %v, %v", recs, err)
	}
	e.svc.libraryErr = fmt.Errorf("%w (HTTP 403)", ErrRefused)
	if _, err := e.m.Library(context.Background(), a.ID); !errors.Is(err, ErrRefused) {
		t.Fatalf("refused = %v", err)
	}
	got, _ := e.m.Account(context.Background(), a.ID)
	if got.RefusedAt == "" || got.LastError != RefusedText || got.RefusalKind != store.RefusalSignIn || changed != 1 {
		t.Fatalf("after a refusal = %+v (changed %d)", got, changed)
	}
	// Another failure is no refusal.
	e.svc.libraryErr = errors.New("connection reset")
	if _, err := e.m.Library(context.Background(), a.ID); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("a network failure = %v", err)
	}
	if err := e.m.Polled(context.Background(), a.ID, 30, 4); err != nil {
		t.Fatal(err)
	}
	got, _ = e.m.Account(context.Background(), a.ID)
	if got.RefusedAt != "" || got.LastTitles != 30 || got.LastMatched != 4 {
		t.Fatalf("after a poll = %+v", got)
	}
	if err := e.vault.Delete(secrets.AudibleName(a.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Library(context.Background(), a.ID); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("a missing sign-in = %v", err)
	}
	e.noSecrets(t)
}

// A poll whose device a sign-in again replaced mid-way lists the library with
// the new sign-in: the old device's refusal isn't the account's.
func TestLibraryAfterASignInAgainIsNoRefusal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	first, _ := e.m.credentials(a.ID)
	e.svc.library = func(c audible.Credentials) ([]source.Record, error) {
		if c.DeviceSerial != first.DeviceSerial {
			return []source.Record{{ASIN: "B0FAKE0001", Title: "One"}}, nil
		}
		// The sign-in again lands while this device's call is out.
		next := c
		next.DeviceSerial = "SERIAL-NEW"
		raw, err := next.Marshal()
		if err != nil {
			return nil, err
		}
		if err := e.vault.Set(secrets.AudibleName(a.ID), raw); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w (HTTP 401)", ErrRefused)
	}
	recs, err := e.m.Library(context.Background(), a.ID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("library = %v, %v", recs, err)
	}
	if got, _ := e.m.Account(context.Background(), a.ID); got.RefusedAt != "" || got.LastError != "" {
		t.Fatalf("marked refused: %+v", got)
	}
}

// A refusal whose device another sign-in again replaced by the time it would be
// recorded (here: during the retry) isn't recorded on the account.
func TestRefusalOfAReplacedDeviceIsNotRecorded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	calls := 0
	e.svc.library = func(c audible.Credentials) ([]source.Record, error) {
		calls++
		next := c
		next.DeviceSerial = fmt.Sprintf("SERIAL-NEW-%d", calls)
		raw, err := next.Marshal()
		if err != nil {
			return nil, err
		}
		if err := e.vault.Set(secrets.AudibleName(a.ID), raw); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w (HTTP 401)", ErrRefused)
	}
	if _, err := e.m.Library(context.Background(), a.ID); !errors.Is(err, ErrRefused) || calls != 2 {
		t.Fatalf("library = %v (calls %d)", err, calls)
	}
	if got, _ := e.m.Account(context.Background(), a.ID); got.RefusedAt != "" {
		t.Fatalf("a replaced device's refusal was recorded: %+v", got)
	}
}

// A call Audible answers ends a sign-in refusal (a transient 401/403), never a
// device's throttling.
func TestAnAnsweredCallEndsOnlyASignInRefusal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	ctx := context.Background()
	e.svc.libraryErr = fmt.Errorf("%w (HTTP 401)", ErrRefused)
	if _, err := e.m.Title(ctx, a.ID, "B0FAKE0003"); !errors.Is(err, ErrRefused) {
		t.Fatalf("refused title = %v", err)
	}
	e.svc.libraryErr = nil
	if _, err := e.m.Title(ctx, a.ID, "B0FAKE0003"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.m.Account(ctx, a.ID); got.RefusedAt != "" || got.LastError != "" {
		t.Fatalf("an answered call kept the sign-in refusal: %+v", got)
	}

	e.svc.libraryErr = fmt.Errorf("license denied: %w", audible.ErrThrottled)
	if _, err := e.m.License(ctx, a.ID, "B0FAKE0003"); !errors.Is(err, audible.ErrThrottled) {
		t.Fatalf("throttled license = %v", err)
	}
	e.svc.libraryErr = nil
	if _, err := e.m.Title(ctx, a.ID, "B0FAKE0003"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.m.Account(ctx, a.ID); !Throttled(got) {
		t.Fatalf("an answered call ended the throttling: %+v", got)
	}
}

// An import's title read and license use the account's sign-in, and a refusal
// on either marks the account refused as a poll's does.
func TestImportCallsAndTheirRefusal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	creds, _ := e.m.credentials(a.ID)
	ctx := context.Background()
	if rec, err := e.m.Title(ctx, a.ID, "B0FAKE0003"); err != nil || rec.Title != "Title B0FAKE0003 "+creds.DeviceSerial {
		t.Fatalf("title = %+v, %v", rec, err)
	}
	if lic, err := e.m.License(ctx, a.ID, "B0FAKE0003"); err != nil || lic.IV != "IV"+creds.DeviceSerial {
		t.Fatalf("license: %v", err)
	}
	if err := e.m.Download(ctx, a.ID, "https://cdn.example/f", filepath.Join(t.TempDir(), "f"), audible.DownloadOptions{}); err != nil {
		t.Fatal(err)
	}
	e.svc.libraryErr = fmt.Errorf("%w (HTTP 403)", ErrRefused)
	if _, err := e.m.License(ctx, a.ID, "B0FAKE0003"); !errors.Is(err, ErrRefused) {
		t.Fatalf("refused license = %v", err)
	}
	if got, _ := e.m.Account(ctx, a.ID); got.RefusedAt == "" {
		t.Fatalf("a refused license didn't mark the account: %+v", got)
	}
	if _, err := e.m.Title(ctx, "nosuchaccount1", "B0FAKE0003"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("an unknown account's title = %v", err)
	}
	e.noSecrets(t)
}

// A license denied to the device (audible.ErrThrottled) marks the account
// refused with ThrottledText and answers the denial; a poll (its library still
// lists) keeps it and a poll's error doesn't hide it; a sign-in again ends it.
// Denied: another denial, or a title's, leaves the account alone.
func TestThrottledLicenseRefusesTheAccountUntilASignIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.signIn(t)
	ctx := context.Background()
	changed := 0
	e.m.o.OnChange = func() { changed++ }

	for _, denial := range []error{audible.ErrNoRights, audible.ErrDenied, audible.ErrNotReleased} {
		e.svc.libraryErr = fmt.Errorf("license denied: %w", denial)
		if _, err := e.m.License(ctx, a.ID, "B0FAKE0003"); !errors.Is(err, denial) {
			t.Fatalf("license = %v, want %v", err, denial)
		}
		if got, _ := e.m.Account(ctx, a.ID); got.RefusedAt != "" || Throttled(got) || changed != 0 {
			t.Fatalf("%v marked the account: %+v", denial, got)
		}
	}

	e.svc.libraryErr = fmt.Errorf("license denied: %w", audible.ErrThrottled)
	if _, err := e.m.License(ctx, a.ID, "B0FAKE0003"); !errors.Is(err, audible.ErrThrottled) || errors.Is(err, ErrRefused) {
		t.Fatalf("throttled license = %v", err)
	}
	got, _ := e.m.Account(ctx, a.ID)
	if got.RefusedAt == "" || got.LastError != ThrottledText || !Throttled(got) || changed != 1 {
		t.Fatalf("after a throttled license = %+v (changed %d)", got, changed)
	}

	if err := e.m.Failed(ctx, a.ID, "could not read the library's books on the server"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Polled(ctx, a.ID, 30, 4); err != nil {
		t.Fatal(err)
	}
	if got, _ = e.m.Account(ctx, a.ID); !Throttled(got) || got.LastTitles != 30 {
		t.Fatalf("a poll ended the throttling: %+v", got)
	}

	e.svc.libraryErr = nil
	if b := e.signIn(t); b.ID != a.ID || b.RefusedAt != "" || b.LastError != "" || Throttled(b) {
		t.Fatalf("signed in again = %+v", b)
	}
	if _, err := e.m.License(ctx, a.ID, "B0FAKE0003"); err != nil {
		t.Fatalf("license after the sign-in again = %v", err)
	}
	e.noSecrets(t)
}

// Denied: the real Service's errors carry neither the account's customer id
// nor its device serial, even in shapes no pattern knows (Amazon's answer
// quoting them back), while errors.Is still sees the cause.
func TestServiceRedactsTheAccountByValue(t *testing.T) {
	t.Parallel()
	const customer, serial = "odd-cust-value", "odd-serial-value"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/licenserequest") {
			_, _ = fmt.Fprintf(w, `{"content_license":{"status_code":"Denied","license_denial_reasons":[
				{"validationType":"Ownership","rejectionReason":"CustomerThrottled","message":"%s on %s being throttled"}]}}`, customer, serial)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"message":"no such customer %s for device %s"}`, customer, serial)
	}))
	defer ts.Close()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	creds := audible.Credentials{ADPToken: "ADP", DevicePrivateKey: keyPEM, DeviceType: "A2CZJZGLK2JJVM", DeviceSerial: serial,
		CustomerID: customer, Marketplace: "us"}
	svc := Audible(audible.WithEndpoints(audible.Endpoints{Amazon: ts.URL, AmazonAPI: ts.URL, API: ts.URL}))
	ctx := context.Background()
	_, libErr := svc.Library(ctx, creds)
	_, titleErr := svc.Title(ctx, creds, "B0FAKE0001")
	_, licErr := svc.License(ctx, creds, "B0FAKE0001")
	if !errors.Is(licErr, audible.ErrThrottled) {
		t.Fatalf("license = %v, want ErrThrottled", licErr)
	}
	for _, err := range []error{libErr, titleErr, licErr} {
		if err == nil || strings.Contains(err.Error(), customer) || strings.Contains(err.Error(), serial) {
			t.Fatalf("err = %v", err)
		}
	}
}
