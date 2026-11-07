//go:build audiblefake

package audible

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible/audiblefake"
)

// startFake serves a fresh fake and returns it with options pointing a client at it.
func startFake(t *testing.T) (*audiblefake.Fake, []Option) {
	t.Helper()
	fake := audiblefake.New(audiblefake.Options{KeyBits: 1024})
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	return fake, []Option{WithEndpoints(Endpoints{Amazon: ts.URL, AmazonAPI: ts.URL, API: ts.URL}), noSleep()}
}

// signIn runs the browser half against the fake (open the authorize URL, land
// on maplanding) and returns the pasted-back redirect URL with the PKCE pair
// and serial the Bridge holds.
func signIn(t *testing.T, opts []Option) (redirect string, pkce PKCE, serial string) {
	t.Helper()
	mp, _ := MarketplaceFor("us")
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if serial, err = NewDeviceSerial(); err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := browser.Get(BuildAuthorizeURL(mp, pkce, serial, opts...))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("sign-in answered %d", resp.StatusCode)
	}
	return resp.Header.Get("Location"), pkce, serial
}

// registerWithFake signs in and registers a device against the fake.
func registerWithFake(t *testing.T, opts []Option) Credentials {
	t.Helper()
	mp, _ := MarketplaceFor("us")
	redirect, pkce, serial := signIn(t, opts)
	code, err := ParseAuthCode(redirect)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := Register(context.Background(), mp, code, pkce, serial, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

func TestFakeSignInAndRegister(t *testing.T) {
	fake, opts := startFake(t)
	redirect, _, serial := signIn(t, opts)
	if !strings.Contains(redirect, "/ap/maplanding?") {
		t.Fatalf("redirect = %q", redirect)
	}
	code, err := ParseAuthCode(redirect)
	if err != nil {
		t.Fatal(err)
	}
	mp, _ := MarketplaceFor("us")

	// Denied: a verifier that isn't the challenge's.
	if _, err := Register(context.Background(), mp, code, PKCE{Verifier: "wrong"}, serial, opts...); HTTPStatus(err) != http.StatusBadRequest {
		t.Fatalf("wrong verifier: err = %v", err)
	}
	// The failed attempt used the code up, as Amazon does: sign in again.
	redirect, pkce, serial := signIn(t, opts)
	code, _ = ParseAuthCode(redirect)
	creds, err := Register(context.Background(), mp, code, pkce, serial, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if creds.ADPToken == "" || creds.RefreshToken == "" || creds.DeviceSerial != serial || creds.Marketplace != "us" || creds.CustomerID == "" {
		t.Fatalf("creds = %+v", creds)
	}
	if _, err := creds.PrivateKey(); err != nil {
		t.Fatal(err)
	}
	// Denied: a code is good once.
	if _, err := Register(context.Background(), mp, code, pkce, serial, opts...); HTTPStatus(err) != http.StatusBadRequest {
		t.Fatalf("reused code: err = %v", err)
	}
	reg := fake.Calls().Registered
	if len(reg) != 1 || reg[0].Serial != serial || !strings.HasSuffix(reg[0].DeviceName, "%AudioSilo") {
		t.Fatalf("registered = %+v", reg)
	}
	// The profile the fake (and Amazon) licenses: one constant set here, its
	// copy there.
	if reg[0].AppVersion != audiblefake.ProfileApp || reg[0].OSVersion != audiblefake.ProfileOS ||
		reg[0].SoftwareVersion != audiblefake.ProfileSoftware || len(reg[0].Serial) != 40 || creds.Profile != ProfileVersion {
		t.Fatalf("registered profile = %+v, creds %q", reg[0], creds.Profile)
	}
}

// Allowed: a fresh sign-in's device gets licenses. Denied: a device
// registered with the older profile (the 32-hex serial and 3.56.2 an earlier
// Bridge registered with) lists its library but every license is
// ErrThrottled, as is one the fake ages (OldProfile); neither error carries
// the customer id or the serial.
func TestFakeOldProfileIsThrottled(t *testing.T) {
	const customer = "A1FAKECUSTOMER77"
	fake := audiblefake.New(audiblefake.Options{KeyBits: 1024, CustomerID: customer})
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	opts := []Option{WithEndpoints(Endpoints{Amazon: ts.URL, AmazonAPI: ts.URL, API: ts.URL}), noSleep()}
	ctx := context.Background()
	const asin = "B0FAKE0002"

	fresh, err := NewClient(registerWithFake(t, opts), opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.License(ctx, asin); err != nil {
		t.Fatalf("a fresh sign-in's license: %v", err)
	}

	old := registerOldProfile(t, opts)
	cl, err := NewClient(old, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if recs, err := cl.Library(ctx); err != nil || len(recs) == 0 {
		t.Fatalf("an old-profile device lists its library: %d, %v", len(recs), err)
	}
	noAccount := func(err error) {
		t.Helper()
		if !errors.Is(err, ErrThrottled) {
			t.Fatalf("err = %v, want ErrThrottled", err)
		}
		for _, id := range []string{customer, old.DeviceSerial} {
			if strings.Contains(err.Error(), id) {
				t.Fatalf("the error carries an account id: %s", err)
			}
		}
		if !strings.Contains(err.Error(), asin) || !strings.Contains(err.Error(), "being throttled") {
			t.Fatalf("the ASIN or the reason was lost: %s", err)
		}
	}
	_, err = cl.License(ctx, asin)
	noAccount(err)

	fake.OldProfile()
	_, err = fresh.License(ctx, asin)
	noAccount(err)
	if got := fake.Calls().Licenses; len(got) != 3 || got[0].Status != "Granted" || got[1].Status != "Denied" || got[2].Status != "Denied" {
		t.Fatalf("licenses = %+v", got)
	}
}

// registerOldProfile signs in and registers a device the way an earlier
// Bridge did (a 32-character uppercase-hex serial, app 3.56.2, iOS 17.0,
// software 35602678), straight against the fake's /auth/register.
func registerOldProfile(t *testing.T, opts []Option) Credentials {
	t.Helper()
	mp, _ := MarketplaceFor("us")
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	serial := strings.ToUpper(hex.EncodeToString(b))
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := browser.Get(BuildAuthorizeURL(mp, pkce, serial, opts...))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	code, err := ParseAuthCode(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(registerBody{
		RequestedTokenType: []string{"bearer", "mac_dms"},
		Cookies:            registerCookies{WebsiteCookies: []string{}, Domain: mp.CookieDomain()},
		RegistrationData: map[string]string{"domain": "Device", "app_version": "3.56.2", "device_serial": serial,
			"device_type": deviceType, "device_name": "old", "os_version": "17.0", "software_version": "35602678",
			"device_model": "iPhone", "app_name": "Audible"},
		AuthData: map[string]string{"client_id": clientID(serial), "authorization_code": code, "code_verifier": pkce.Verifier,
			"code_algorithm": "SHA-256", "client_domain": "DeviceLegacy"},
	})
	data, err := postAuth(context.Background(), resolve(mp, authTimeout, opts), "/auth/register", mp, "application/json", body, "")
	if err != nil {
		t.Fatal(err)
	}
	creds, err := parseRegister(data, mp, serial)
	if err != nil {
		t.Fatal(err)
	}
	creds.Profile = "" // made before the profile was recorded
	return creds
}

func TestFakeLibraryStates(t *testing.T) {
	fake, opts := startFake(t)
	c, err := NewClient(registerWithFake(t, opts), opts...)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := c.Library(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The library holds no preorder (Audible's lists one only once it is out).
	if len(recs) != 26 {
		t.Fatalf("library = %d titles", len(recs))
	}
	fixtures := map[string]audiblefake.Title{}
	for _, f := range audiblefake.Fixtures {
		fixtures[f.ASIN] = f
	}
	counts := map[string]int{}
	for i, r := range recs {
		want, ok := fixtures[r.ASIN]
		if !ok || r.Title != want.Title {
			t.Fatalf("record %d = %+v", i, r)
		}
		var got string
		switch {
		case r.UnavailableReason == "":
			got = "available"
		case strings.Contains(r.UnavailableReason, "pre-order"):
			got = "preorder"
		default:
			got = "unavailable"
		}
		if got != want.State() {
			t.Errorf("%s: state %s, want %s (%q)", r.ASIN, got, want.State(), r.UnavailableReason)
		}
		counts[got]++
	}
	if counts["available"] != 22 || counts["preorder"] != 0 || counts["unavailable"] != 4 {
		t.Fatalf("states = %v", counts)
	}
	if _, err := c.Title(context.Background(), "B0FAKE0005"); !errors.Is(err, ErrNotInLibrary) {
		t.Fatalf("a preorder's library read = %v, want not in the library", err)
	}

	// The orders: the four preorders pending, the cancelled one not, recent
	// purchases closed, nothing older than the window.
	orders, err := c.Orders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var pending []string
	cancelled := false
	for _, o := range orders {
		if time.Since(o.Date) > audiblefake.OrdersWindow {
			t.Fatalf("an order from %v, past the window", o.Date)
		}
		for _, it := range o.Items {
			if o.PendingPreorder() {
				pending = append(pending, it.ASIN)
			}
			if it.ASIN == audiblefake.CancelledPreorder.ASIN {
				cancelled = !o.PendingPreorder() && o.Preorder
			}
		}
	}
	slices.Sort(pending)
	if !slices.Equal(pending, []string{"B0FAKE0005", "B0FAKE0010", "B0FAKE0017", "B0FAKE0026"}) || !cancelled {
		t.Fatalf("pending = %v, cancelled listed %v", pending, cancelled)
	}
	// The catalog describes them (the split on the fake's 400 over 10 runs).
	asked := append(pending, audiblefake.CancelledPreorder.ASIN, "B0NOTKNOWN")
	for i := range 20 {
		asked = append(asked, audiblefake.Fixtures[i].ASIN)
	}
	prods, err := c.Products(context.Background(), asked)
	if err != nil {
		t.Fatal(err)
	}
	if len(prods) != len(asked)-1 || prods[0].Title != "The Golden Canopy" || !prods[0].Preorder || prods[0].ReleaseDate == "" ||
		prods[0].Series != "Orchard Cycle" || prods[0].SeriesPosition != "5" {
		t.Fatalf("catalog = %d, first %+v", len(prods), prods[0])
	}

	// Released: the library lists it, available, and its order is closed.
	fake.Release("B0FAKE0005")
	recs, _ = c.Library(context.Background())
	if len(recs) != 27 {
		t.Fatalf("library after a release = %d", len(recs))
	}
	if r, err := c.Title(context.Background(), "B0FAKE0005"); err != nil || r.UnavailableReason != "" {
		t.Fatalf("released title = %+v, %v", r, err)
	}
	orders, _ = c.Orders(context.Background())
	for _, o := range orders {
		if o.PendingPreorder() && o.Items[0].ASIN == "B0FAKE0005" {
			t.Fatal("a released preorder's order is still pending")
		}
	}
	thicket := recs[2]
	if thicket.ASIN != "B0FAKE0003" {
		t.Fatalf("third record = %+v", thicket)
	}
	if thicket.Series != "Orchard Cycle" || !thicket.HasSequence || thicket.Sequence != 3 || thicket.RuntimeSec != 745*60 || thicket.Narrator != "Iris Fenwick" {
		t.Fatalf("series record = %+v", thicket)
	}
}

// The whole library's places in 25s, split on the fake's 400 over 10, with
// last_updated; a place written back is recorded and read back.
func TestFakeLastPositions(t *testing.T) {
	fake, opts := startFake(t)
	c, err := NewClient(registerWithFake(t, opts), opts...)
	if err != nil {
		t.Fatal(err)
	}
	var asins []string
	for _, f := range audiblefake.Fixtures {
		asins = append(asins, f.ASIN)
	}
	got, err := c.LastPositions(context.Background(), asins, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 {
		t.Fatalf("positions = %d, want the 7 fixtures", len(got))
	}
	p := got["B0FAKE0002"]
	if p.PositionMs != 12_345_678 || p.LastUpdated.IsZero() || time.Since(p.LastUpdated) < 29*24*time.Hour {
		t.Fatalf("B0FAKE0002 = %+v", p)
	}
	sizes := fake.Calls().LastPositionsSizes
	if sizes[0] != 25 || slices.Max(sizes) != 25 || !slices.ContainsFunc(sizes, func(n int) bool { return n <= 10 }) {
		t.Fatalf("request sizes = %v, want 25 refused then split", sizes)
	}

	if err := c.SetLastPosition(context.Background(), "B0FAKE0006", "CR!FAKEACR", 5000); err != nil {
		t.Fatal(err)
	}
	if put := fake.Calls().PutPositions; len(put) != 1 || put[0].At == "" ||
		put[0] != (audiblefake.PutPosition{ASIN: "B0FAKE0006", ACR: "CR!FAKEACR", PositionMs: 5000, At: put[0].At}) {
		t.Fatalf("put = %+v", put)
	}
	again, err := c.LastPositions(context.Background(), []string{"B0FAKE0006"}, nil)
	if err != nil || again["B0FAKE0006"].PositionMs != 5000 {
		t.Fatalf("read back = %+v, %v", again, err)
	}
}

// Deregister removes the device (the fake records it) and its credentials stop
// working: a signed call is then refused, as is one from an account Audible
// refuses, and one signed with another key.
func TestFakeDeregisterAndRefusals(t *testing.T) {
	fake, opts := startFake(t)
	creds := registerWithFake(t, opts)
	c, err := NewClient(creds, opts...)
	if err != nil {
		t.Fatal(err)
	}

	// Denied: the right token signed with a key the fake never issued.
	other, _ := rsa.GenerateKey(rand.Reader, 1024)
	forged := creds
	forged.DevicePrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(other)}))
	fc, err := NewClient(forged, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fc.Library(context.Background()); !IsRefused(err) {
		t.Fatalf("forged signature: err = %v", err)
	}

	// The access token from registration is still fresh: no refresh needed.
	if err := Deregister(context.Background(), creds, opts...); err != nil {
		t.Fatal(err)
	}
	if d := fake.Calls().Deregistered; len(d) != 1 || d[0] != creds.DeviceSerial {
		t.Fatalf("deregistered = %v", d)
	}
	if _, err := c.Library(context.Background()); HTTPStatus(err) != http.StatusUnauthorized || !IsRefused(err) {
		t.Fatalf("after deregister: err = %v", err)
	}
	// Denied: deregistering again (the tokens are gone).
	if err := Deregister(context.Background(), creds, opts...); !IsRefused(err) {
		t.Fatalf("second deregister: err = %v", err)
	}

	// A refused account: 403 on every signed call.
	c2, _ := NewClient(registerWithFake(t, opts), opts...)
	fake.Refuse()
	if _, err := c2.Library(context.Background()); HTTPStatus(err) != http.StatusForbidden || !IsRefused(err) {
		t.Fatalf("refused account: err = %v", err)
	}
}

// An expired access token is refreshed before deregistering.
func TestFakeDeregisterRefreshes(t *testing.T) {
	fake, opts := startFake(t)
	creds := registerWithFake(t, opts)
	creds.AccessToken, creds.ExpiresAt = "stale", time.Now().Add(-time.Minute).Unix()
	if err := Deregister(context.Background(), creds, opts...); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls().Deregistered) != 1 {
		t.Fatal("not deregistered")
	}
}

// The tag's environment variable points every endpoint at the fake.
func TestFakeURLEnv(t *testing.T) {
	t.Setenv(FakeURLEnv, "http://127.0.0.1:8099/ ")
	mp, _ := MarketplaceFor("de")
	if e := EndpointsFor(mp); e.Amazon != "http://127.0.0.1:8099" || e.AmazonAPI != e.Amazon || e.API != e.Amazon {
		t.Fatalf("endpoints = %+v", e)
	}
	t.Setenv(FakeURLEnv, "")
	if e := EndpointsFor(mp); e.API != "https://api.audible.de" {
		t.Fatalf("endpoints without the fake = %+v", e)
	}
}

// A recorded run (the owner's recording transport) holds the shapes and titles
// but no credential: not the tokens, the device key, the code or verifier, the
// customer or device ids, nor the signature headers.
func TestRecorderScrubsCredentials(t *testing.T) {
	fake := audiblefake.New(audiblefake.Options{KeyBits: 1024})
	ts := httptest.NewServer(fake.Handler())
	defer ts.Close()
	dir := t.TempDir()
	hc := &http.Client{Transport: audiblefake.NewRecorder(http.DefaultTransport, dir)}
	opts := []Option{WithEndpoints(Endpoints{Amazon: ts.URL, AmazonAPI: ts.URL, API: ts.URL}), WithHTTPClient(hc), noSleep()}

	mp, _ := MarketplaceFor("us")
	redirect, pkce, serial := signIn(t, opts)
	code, _ := ParseAuthCode(redirect)
	creds, err := Register(context.Background(), mp, code, pkce, serial, opts...)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := NewClient(creds, opts...)
	if _, err := c.Library(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LastPositions(context.Background(), []string{"B0FAKE0001"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Orders(context.Background()); err != nil {
		t.Fatal(err)
	}
	refreshed, err := RefreshAccessToken(context.Background(), creds, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := Deregister(context.Background(), refreshed, opts...); err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "audible-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("recordings = %v", files)
	}
	info, _ := os.Stat(files[0])
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("recording mode = %v", info.Mode())
	}
	raw, _ := os.ReadFile(files[0])
	rec := string(raw)
	keyLine := strings.Split(creds.DevicePrivateKey, "\n")[1]
	for name, secret := range map[string]string{
		"access token": creds.AccessToken, "refreshed access token": refreshed.AccessToken,
		"refresh token": creds.RefreshToken, "adp token": creds.ADPToken, "device key": keyLine,
		"customer id": creds.CustomerID, "serial": serial, "code": code, "verifier": pkce.Verifier,
		"challenge": pkce.Challenge, "account name": "Fake Customer",
		"order amount": "FAKE-AMOUNT", "payment method": "FAKE-PAYMENT-METHOD", "tax": "FAKE-TAX", "merchant": "FAKE-MERCHANT",
	} {
		if strings.Contains(rec, secret) {
			t.Errorf("the recording holds the %s", name)
		}
	}
	for _, h := range []string{"x-adp-signature", "X-Adp-Signature", "Authorization"} {
		if strings.Contains(rec, h) {
			t.Errorf("the recording holds the %s header", h)
		}
	}
	for _, kept := range []string{"The Glass Orchard", "/1.0/library", "asin_last_position_heard_annots", "/auth/register", "REDACTED",
		"/1.0/orders", "ORDER_PENDING_PREORDER", "B0FAKE0005"} {
		if !strings.Contains(rec, kept) {
			t.Errorf("the recording lost %q", kept)
		}
	}
}
