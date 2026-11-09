//go:build audiblefake

package audibleacct

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audible/audiblefake"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The whole account life against the Audible fake: the browser half through
// the authorize URL Start answers, Register, the library, a refusal, and a
// removal the fake records as a deregistration.
func TestAccountAgainstTheFake(t *testing.T) {
	t.Parallel()
	fake := audiblefake.New(audiblefake.Options{KeyBits: 1024})
	ts := httptest.NewServer(fake.Handler())
	defer ts.Close()

	data := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(data, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vault, _, err := secrets.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	m := New(Options{Vault: vault, DB: db, Log: slog.New(slog.NewTextHandler(&logs, nil)),
		Service: Audible(audible.WithEndpoints(audible.Endpoints{Amazon: ts.URL, AmazonAPI: ts.URL, API: ts.URL}))})
	srv := &stubServer{}

	st, err := m.Start("us")
	if err != nil || !strings.HasPrefix(st.URL, ts.URL+"/ap/signin?") {
		t.Fatalf("start = %+v, %v", st, err)
	}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := browser.Get(st.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	redirect := resp.Header.Get("Location")
	a, err := m.Finish(context.Background(), srv, "srv-1", Finish{Login: st.Login, RedirectURL: redirect, Mapping: mapping()})
	if err != nil {
		t.Fatal(err)
	}
	if calls := fake.Calls(); len(calls.Registered) != 1 || !strings.HasSuffix(calls.Registered[0].DeviceName, audible.DeviceName) {
		t.Fatalf("registered = %+v", calls.Registered)
	}
	if a.DeviceProfile != audible.ProfileVersion || OldProfile(a) {
		t.Fatalf("the sign-in's profile = %q", a.DeviceProfile)
	}
	recs, err := m.Library(context.Background(), a.ID)
	if err != nil || len(recs) != len(audiblefake.Fixtures)-4 {
		t.Fatalf("library = %d, %v (the four preorders aren't in it)", len(recs), err)
	}
	// The preorders come from the orders, described by the catalog; the
	// cancelled one doesn't.
	pre := m.Preorders(context.Background(), a.ID, recs)
	if len(pre) != 4 || pre[0].ASIN != "B0FAKE0005" || pre[0].Title != "The Golden Canopy" || !pre[0].Preorder || pre[0].ReleaseDate == "" {
		t.Fatalf("preorders = %+v", pre)
	}

	creds, _ := m.credentials(a.ID)
	for _, secret := range []string{creds.AccessToken, creds.RefreshToken, creds.ADPToken, creds.CustomerID} {
		if secret != "" && strings.Contains(logs.String(), secret) {
			t.Fatalf("the log holds a credential: %s", logs.String())
		}
	}

	fake.Refuse()
	if _, err := m.Library(context.Background(), a.ID); !errors.Is(err, ErrRefused) {
		t.Fatalf("refused = %v", err)
	}
	if got, _ := m.Account(context.Background(), a.ID); got.RefusedAt == "" {
		t.Fatalf("not marked refused: %+v", got)
	}

	warning, err := m.Remove(context.Background(), srv, a.ID)
	if err != nil || warning != "" {
		t.Fatalf("remove = %q, %v", warning, err)
	}
	if calls := fake.Calls(); len(calls.Deregistered) != 1 || calls.Deregistered[0] != creds.DeviceSerial {
		t.Fatalf("deregistered = %v, want %s", calls.Deregistered, creds.DeviceSerial)
	}
}
