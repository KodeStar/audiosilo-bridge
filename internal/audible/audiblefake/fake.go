//go:build audiblefake

// Package audiblefake is a stand-in for Amazon's sign-in and identity API and
// the Audible API, so the Bridge's Audible flows run end to end without an
// account: in tests (httptest) and in a live check (`go run -tags audiblefake
// ./cmd/audiblefake`, with the Bridge built `-tags audiblefake` and
// AUDIOSILO_BRIDGE_AUDIBLE_FAKE_URL pointing at it). It exists only behind the
// audiblefake build tag; release builds never carry it.
//
// It checks what Audible checks: the PKCE verifier against the challenge the
// sign-in saw, each signed call's RSA signature against the device key it
// issued, the bearer token on deregister. It records what the Bridge does that
// the owner would see on Amazon (registrations, deregistrations, places written
// back) for tests and for its /_fake/calls page. Its data is synthetic
// (fixtures.go). Phase 4b: a license request answers a voucher built from the
// device and customer ids, and the download is an UNENCRYPTED fixture .m4b
// served as the "AAXC" (ffmpeg passes it through with the key set), with
// Range and If-Range, and controls to cut a download, expire the URLs and
// release a preorder (content.go). A preorder is not in the library until it
// is released: the orders listing (pending) and the catalog show it, shaped as
// a real account's were (orders.go).
//
// This package must never import internal/audible: audible's audiblefake build
// imports it (for the Recorder).
package audiblefake

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configure a Fake.
type Options struct {
	// LastPositionsMax is the most ASINs one lastpositions or catalog request
	// may carry before the fake answers 400, as Audible does at an undocumented
	// N (default 10, under the client's first batch of 25 so the split runs).
	LastPositionsMax int
	// Now is the clock release dates and places are relative to (default
	// time.Now).
	Now func() time.Time
	// CustomerID is the account's customer id (default a fixed fake one).
	CustomerID string
	// KeyBits is the RSA size of each issued device key (default 2048).
	KeyBits int
	// FFmpeg tags each title's download with its own title and authors (the
	// fixture's audio and chapters, copied); "" finds ffmpeg on PATH, "-" never
	// tags (every title is the fixture as it is).
	FFmpeg string
}

// Registration is one device registration the fake accepted, with the device
// profile it registered as (registration_data's app, os and software versions).
type Registration struct {
	Serial          string `json:"serial"`
	DeviceName      string `json:"device_name"`
	AppVersion      string `json:"app_version"`
	OSVersion       string `json:"os_version"`
	SoftwareVersion string `json:"software_version"`
}

// The device profile the fake licenses, as Amazon has since about September
// 2026 (mkb79/Audible#909, PR #910): a device registered with other versions,
// or a serial that isn't 40 ASCII letters and digits, is registered (and lists
// its library) but every license request is Denied with the three reasons a
// throttled device gets. Kept here, not read from the audible package (which
// the fake never imports), so a change on one side alone fails the tests.
const (
	ProfileApp      = "4.60"
	ProfileOS       = "26.2"
	ProfileSoftware = "46000856"
)

// currentProfile reports whether a registration's serial and versions are the
// profile the fake licenses.
func currentProfile(serial string, data map[string]string) bool {
	if len(serial) != 40 {
		return false
	}
	for _, c := range serial {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return data["app_version"] == ProfileApp && data["os_version"] == ProfileOS && data["software_version"] == ProfileSoftware
}

// PutPosition is one place written back with PUT /1.0/lastpositions/{asin},
// and when (the fake's clock, RFC 3339): a live check counts them and their
// spacing.
type PutPosition struct {
	ASIN       string `json:"asin"`
	ACR        string `json:"acr"`
	PositionMs int64  `json:"position_ms"`
	At         string `json:"at"`
}

// Calls is what the fake recorded.
type Calls struct {
	Registered   []Registration `json:"registered"`
	Deregistered []string       `json:"deregistered"` // device serials
	PutPositions []PutPosition  `json:"put_positions"`
	// LastPositionsSizes is the ASIN count of every lastpositions request,
	// refused ones included.
	LastPositionsSizes []int `json:"lastpositions_sizes"`
	// Licenses are the license requests, Downloads the content GETs (with
	// their Range, "" for a whole file).
	Licenses  []LicenseCall  `json:"licenses"`
	Downloads []DownloadCall `json:"downloads"`
}

// LicenseCall is one license request and how the fake answered it.
type LicenseCall struct {
	ASIN   string `json:"asin"`
	Status string `json:"status"` // Granted | Denied
}

// DownloadCall is one content GET.
type DownloadCall struct {
	ASIN    string `json:"asin"`
	Range   string `json:"range"`
	IfRange string `json:"if_range"`
	// Expired: the URL's license had expired (403).
	Expired bool `json:"expired,omitempty"`
}

// Fake is the fake service. Its zero value is not usable: call New.
type Fake struct {
	opts Options

	mu        sync.Mutex
	codes     map[string]pendingCode // authorization code -> what the sign-in saw
	devices   map[string]*device     // by adp token
	byAccess  map[string]*device
	byRefresh map[string]*device
	positions map[string]position
	refused   bool
	calls     Calls
	// released are preorders released early (Release); urls the download URLs
	// issued (token -> ASIN; ExpireURLs empties it); content the files served.
	released map[string]bool
	urls     map[string]string
	content  map[string][]byte
	cutNext  int64
}

type pendingCode struct {
	challenge string
	clientID  string
}

type device struct {
	serial  string
	devType string
	// licensed: registered with the current profile (currentProfile); a
	// device that isn't gets the throttled denial on every license request.
	licensed bool
	adp      string
	access   string
	refresh  string
	pub      *rsa.PublicKey
}

// New returns a fake holding the fixture library and places.
func New(opts Options) *Fake {
	if opts.LastPositionsMax <= 0 {
		opts.LastPositionsMax = 10
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.CustomerID == "" {
		opts.CustomerID = "amzn1.account.AFAKECUSTOMER0000000001"
	}
	if opts.KeyBits <= 0 {
		opts.KeyBits = 2048
	}
	f := &Fake{
		opts:      opts,
		codes:     map[string]pendingCode{},
		devices:   map[string]*device{},
		byAccess:  map[string]*device{},
		byRefresh: map[string]*device{},
		positions: map[string]position{},
		released:  map[string]bool{},
		urls:      map[string]string{},
		content:   map[string][]byte{},
	}
	now := opts.Now()
	for asin, p := range fixturePositions {
		f.positions[asin] = position{ms: p.ms, updated: now.AddDate(0, 0, -p.daysAgo).UTC().Truncate(time.Millisecond)}
	}
	return f
}

// Calls returns a copy of what the fake recorded.
func (f *Fake) Calls() Calls {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Calls{
		Registered:         append([]Registration(nil), f.calls.Registered...),
		Deregistered:       append([]string(nil), f.calls.Deregistered...),
		PutPositions:       append([]PutPosition(nil), f.calls.PutPositions...),
		LastPositionsSizes: append([]int(nil), f.calls.LastPositionsSizes...),
		Licenses:           append([]LicenseCall(nil), f.calls.Licenses...),
		Downloads:          append([]DownloadCall(nil), f.calls.Downloads...),
	}
}

// Refuse makes every signed call answer 403 from now on, as Audible does for a
// device the account no longer honours.
func (f *Fake) Refuse() {
	f.mu.Lock()
	f.refused = true
	f.mu.Unlock()
}

// OldProfile marks every device registered so far as registered with the
// older profile, as a device an earlier Bridge registered is: its license
// requests are Denied as throttled from now on (a sign-in again registers one
// that isn't).
func (f *Fake) OldProfile() {
	f.mu.Lock()
	for _, d := range f.devices {
		d.licensed = false
	}
	f.mu.Unlock()
}

// SetPosition sets the place Audible heard for asin: ms into the book, heard
// ago before the fake's clock (a listen on another device). It is not a write
// back (not recorded in PutPositions).
func (f *Fake) SetPosition(asin string, ms int64, ago time.Duration) {
	f.mu.Lock()
	f.positions[asin] = position{ms: ms, updated: f.opts.Now().Add(-ago).UTC().Truncate(time.Millisecond)}
	f.mu.Unlock()
}

// Position is the place Audible holds for asin (ms into the book, when heard),
// false when it holds none.
func (f *Fake) Position(asin string) (ms int64, heard time.Time, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.positions[asin]
	return p.ms, p.updated, ok
}

// Handler serves the fake. The /_fake/ routes are its own controls, for a live
// check: GET /_fake/calls, POST /_fake/refuse, POST /_fake/old-profile, GET
// /_fake/titles, GET /_fake/positions, POST /_fake/position?asin=&ms=&ago=.
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ap/signin", f.signin)
	mux.HandleFunc("GET /ap/maplanding", f.maplanding)
	mux.HandleFunc("POST /auth/register", f.register)
	mux.HandleFunc("POST /auth/token", f.token)
	mux.HandleFunc("POST /auth/deregister", f.deregister)
	mux.HandleFunc("GET /1.0/library", f.signed(f.library))
	mux.HandleFunc("GET /1.0/annotations/lastpositions", f.signed(f.lastPositions))
	mux.HandleFunc("PUT /1.0/lastpositions/{asin}", f.signed(f.putPosition))
	mux.HandleFunc("GET /1.0/library/{asin}", f.signed(f.libraryTitle))
	mux.HandleFunc("GET /1.0/orders", f.signed(f.orders))
	mux.HandleFunc("GET /1.0/catalog/products", f.signed(f.products))
	mux.HandleFunc("GET /1.0/catalog/products/{asin}", f.signed(f.product))
	mux.HandleFunc("POST /1.0/content/{asin}/licenserequest", f.signed(f.license))
	mux.HandleFunc("GET /_fake/content/{file}", f.signed(f.download))
	mux.HandleFunc("GET /_fake/calls", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, f.Calls()) })
	mux.HandleFunc("POST /_fake/refuse", func(w http.ResponseWriter, _ *http.Request) {
		f.Refuse()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /_fake/old-profile", func(w http.ResponseWriter, _ *http.Request) {
		f.OldProfile()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /_fake/cut", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if err != nil || n < 1 {
			http.Error(w, "after must be a byte count", http.StatusBadRequest)
			return
		}
		f.CutNextDownload(n)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /_fake/expire", func(w http.ResponseWriter, _ *http.Request) {
		f.ExpireURLs()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /_fake/release", func(w http.ResponseWriter, r *http.Request) {
		if !f.Release(r.URL.Query().Get("asin")) {
			http.Error(w, "no such preorder", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /_fake/position", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		asin := q.Get("asin")
		ms, err := strconv.ParseInt(q.Get("ms"), 10, 64)
		if asin == "" || err != nil || ms < 0 {
			http.Error(w, "want asin and ms >= 0 (and ago, seconds before now)", http.StatusBadRequest)
			return
		}
		ago := 0
		if v := q.Get("ago"); v != "" {
			if ago, err = strconv.Atoi(v); err != nil || ago < 0 {
				http.Error(w, "ago is seconds before now", http.StatusBadRequest)
				return
			}
		}
		f.SetPosition(asin, ms, time.Duration(ago)*time.Second)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /_fake/positions", func(w http.ResponseWriter, _ *http.Request) {
		type row struct {
			PositionMs  int64  `json:"position_ms"`
			LastUpdated string `json:"last_updated"`
		}
		f.mu.Lock()
		out := make(map[string]row, len(f.positions))
		for asin, p := range f.positions {
			out[asin] = row{p.ms, p.updated.Format(time.RFC3339Nano)}
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /_fake/titles", func(w http.ResponseWriter, _ *http.Request) {
		type row struct {
			Title
			State string `json:"state"`
		}
		titles := f.catalog()
		rows := make([]row, len(titles))
		for i, t := range titles {
			rows[i] = row{t, t.State()}
		}
		writeJSON(w, http.StatusOK, rows)
	})
	return mux
}

// signin is Amazon's sign-in page, already signed in: it checks the authorize
// URL the Bridge built and sends the browser to its maplanding with a code.
func (f *Fake) signin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	challenge := q.Get("openid.oa2.code_challenge")
	clientID, ok := strings.CutPrefix(q.Get("openid.oa2.client_id"), "device:")
	returnTo, err := url.Parse(q.Get("openid.return_to"))
	if challenge == "" || !ok || clientID == "" || q.Get("openid.oa2.code_challenge_method") != "S256" ||
		err != nil || returnTo.Path != "/ap/maplanding" {
		http.Error(w, "the fake's sign-in needs an authorize URL with a S256 challenge, a device client id and a maplanding return_to", http.StatusBadRequest)
		return
	}
	code := "ANFAKE" + randHex(10)
	f.mu.Lock()
	f.codes[code] = pendingCode{challenge: challenge, clientID: clientID}
	f.mu.Unlock()
	// Always the fake's own maplanding (a relative redirect), never wherever the
	// query pointed, carrying only what Amazon's adds.
	rq := url.Values{"openid.oa2.authorization_code": {code}, "openid.mode": {"id_res"}}
	http.Redirect(w, r, "/ap/maplanding?"+rq.Encode(), http.StatusFound)
}

// maplanding is the page a real sign-in ends on: the user copies its address.
func (f *Fake) maplanding(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<!doctype html><title>Audible fake: signed in</title>
<h1>Signed in to the Audible fake</h1>
<p>Copy this page's address and paste it into the Bridge.</p>
<p><code>%s</code></p>`, html.EscapeString(r.URL.String()))
}

// register is POST /auth/register: the code and verifier for a device.
func (f *Fake) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RegistrationData map[string]string `json:"registration_data"`
		AuthData         map[string]string `json:"auth_data"`
		Cookies          *struct {
			WebsiteCookies []string `json:"website_cookies"`
			Domain         string   `json:"domain"`
		} `json:"cookies"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		amazonError(w, http.StatusBadRequest, "InvalidValue", "the body is not JSON")
		return
	}
	if body.Cookies == nil || body.Cookies.WebsiteCookies == nil {
		amazonError(w, http.StatusBadRequest, "MissingValue", "cookies.website_cookies is required")
		return
	}
	code := body.AuthData["authorization_code"]
	verifier := body.AuthData["code_verifier"]
	serial := body.RegistrationData["device_serial"]
	f.mu.Lock()
	pending, ok := f.codes[code]
	delete(f.codes, code) // a code is good once
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(verifier))
	switch {
	case !ok:
		amazonError(w, http.StatusBadRequest, "InvalidValue", "unknown or used authorization code")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != pending.challenge:
		amazonError(w, http.StatusBadRequest, "InvalidValue", "the code verifier does not match the challenge")
		return
	case body.AuthData["client_id"] != pending.clientID || pending.clientID != hex.EncodeToString([]byte(serial+"#"+body.RegistrationData["device_type"])):
		amazonError(w, http.StatusBadRequest, "InvalidValue", "the client id does not match the sign-in")
		return
	}
	key, err := rsa.GenerateKey(rand.Reader, f.opts.KeyBits)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := &device{
		serial:   serial,
		devType:  body.RegistrationData["device_type"],
		licensed: currentProfile(serial, body.RegistrationData),
		adp:      "{enc:FAKE" + randHex(24) + "}",
		access:   "Atna|FAKE" + randHex(24),
		refresh:  "Atnr|FAKE" + randHex(24),
		pub:      &key.PublicKey,
	}
	f.mu.Lock()
	f.devices[d.adp], f.byAccess[d.access], f.byRefresh[d.refresh] = d, d, d
	f.calls.Registered = append(f.calls.Registered, Registration{Serial: serial, DeviceName: body.RegistrationData["device_name"],
		AppVersion: body.RegistrationData["app_version"], OSVersion: body.RegistrationData["os_version"],
		SoftwareVersion: body.RegistrationData["software_version"]})
	f.mu.Unlock()
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	writeJSON(w, http.StatusOK, map[string]any{"response": map[string]any{"success": map[string]any{
		"tokens": map[string]any{
			"bearer":  map[string]any{"access_token": d.access, "refresh_token": d.refresh, "expires_in": "3600"},
			"mac_dms": map[string]any{"adp_token": d.adp, "device_private_key": string(keyPEM)},
		},
		"extensions": map[string]any{
			"device_info":   map[string]any{"device_serial_number": serial, "device_name": "Fake's AudioSilo"},
			"customer_info": map[string]any{"user_id": f.opts.CustomerID, "given_name": "Fake", "name": "Fake Customer"},
		},
		"customer_id": f.opts.CustomerID,
	}}})
}

// token is POST /auth/token: a refresh token for a new access token.
func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		amazonError(w, http.StatusBadRequest, "InvalidValue", "the body is not a form")
		return
	}
	if r.PostForm.Get("source_token_type") != "refresh_token" || r.PostForm.Get("requested_token_type") != "access_token" {
		amazonError(w, http.StatusBadRequest, "InvalidValue", "only refresh_token -> access_token")
		return
	}
	f.mu.Lock()
	d, ok := f.byRefresh[r.PostForm.Get("source_token")]
	var access string
	if ok {
		delete(f.byAccess, d.access)
		d.access = "Atna|FAKE" + randHex(24)
		f.byAccess[d.access] = d
		access = d.access
	}
	f.mu.Unlock()
	if !ok {
		amazonError(w, http.StatusUnauthorized, "InvalidToken", "unknown refresh token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "expires_in": 3600, "token_type": "bearer"})
}

// deregister is POST /auth/deregister with the device's bearer token: the device
// leaves the account and its tokens stop working.
func (f *Fake) deregister(w http.ResponseWriter, r *http.Request) {
	access, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var body struct {
		All *bool `json:"deregister_all_existing_accounts"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	if body.All == nil || *body.All {
		// Deregistering every device would sign the owner's phones out: the
		// Bridge must never ask for it.
		amazonError(w, http.StatusBadRequest, "InvalidValue", "the fake refuses deregister_all_existing_accounts other than false")
		return
	}
	f.mu.Lock()
	d, ok := f.byAccess[access]
	if ok {
		delete(f.byAccess, d.access)
		delete(f.byRefresh, d.refresh)
		delete(f.devices, d.adp)
		f.calls.Deregistered = append(f.calls.Deregistered, d.serial)
	}
	f.mu.Unlock()
	if !ok {
		amazonError(w, http.StatusUnauthorized, "InvalidToken", "unknown access token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"response": map[string]any{"success": map[string]any{}}})
}

// signed checks a call's x-adp-* signature against the device that holds its
// token: an unknown token is 401, a refused account 403, a bad signature 401.
func (f *Fake) signed(next func(http.ResponseWriter, *http.Request, []byte)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		adp := r.Header.Get("x-adp-token")
		f.mu.Lock()
		d, ok := f.devices[adp]
		refused := f.refused
		f.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "unknown device token"})
			return
		}
		if refused {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "the account refuses this device"})
			return
		}
		if !verifySignature(r, body, adp, d.pub) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "bad signature"})
			return
		}
		next(w, r, body)
	}
}

// verifySignature checks x-adp-signature ("<base64 sig>:<date>") over the
// string Audible signs: method, path and query, date, body, adp token.
func verifySignature(r *http.Request, body []byte, adp string, pub *rsa.PublicKey) bool {
	if r.Header.Get("x-adp-alg") != "SHA256withRSA:1.0" {
		return false
	}
	sigB64, date, ok := strings.Cut(r.Header.Get("x-adp-signature"), ":")
	if !ok {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	pq := r.URL.Path
	if r.URL.RawQuery != "" {
		pq += "?" + r.URL.RawQuery
	}
	h := sha256.Sum256([]byte(r.Method + "\n" + pq + "\n" + date + "\n" + string(body) + "\n" + adp))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig) == nil
}

// library is GET /1.0/library, paged by num_results and page (1-based).
func (f *Fake) library(w http.ResponseWriter, r *http.Request, _ []byte) {
	size, err := strconv.Atoi(r.URL.Query().Get("num_results"))
	if err != nil || size < 1 || size > 1000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "num_results must be 1-1000"})
		return
	}
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	now := f.opts.Now()
	titles := f.titles()
	items := []map[string]any{}
	for i := (page - 1) * size; i < len(titles) && i < page*size; i++ {
		items = append(items, libraryItem(titles[i], now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "response_groups": strings.Split(r.URL.Query().Get("response_groups"), ",")})
}

// libraryItem is a title as the library endpoint sends it, its release date
// a day of now's UTC calendar (the Bridge reads a date as that day's UTC start).
func libraryItem(t Title, now time.Time) map[string]any {
	names := func(ns []string) []map[string]string {
		out := make([]map[string]string, len(ns))
		for i, n := range ns {
			out[i] = map[string]string{"name": n}
		}
		return out
	}
	delivery := t.DeliveryType
	if delivery == "" {
		delivery = "SinglePartBook"
	}
	item := map[string]any{
		"asin":                  t.ASIN,
		"title":                 t.Title,
		"subtitle":              t.Subtitle,
		"authors":               names(t.Authors),
		"narrators":             names(t.Narrators),
		"release_date":          now.UTC().AddDate(0, 0, t.ReleaseIn).Format("2006-01-02"),
		"content_type":          "Product",
		"content_delivery_type": delivery,
		"customer_rights":       map[string]bool{"is_consumable_offline": !t.NoRights},
	}
	if t.RuntimeMin > 0 {
		item["runtime_length_min"] = t.RuntimeMin
	}
	if t.Series != "" {
		item["series"] = []map[string]string{{"title": t.Series, "sequence": t.Sequence}}
	}
	return item
}

// lastPositions is GET /1.0/annotations/lastpositions?asins=a,b: 400 over
// LastPositionsMax ASINs, as Audible's list-length check does.
func (f *Fake) lastPositions(w http.ResponseWriter, r *http.Request, _ []byte) {
	asins := queryASINs(r)
	f.mu.Lock()
	f.calls.LastPositionsSizes = append(f.calls.LastPositionsSizes, len(asins))
	f.mu.Unlock()
	if len(asins) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "asins is required"})
		return
	}
	if f.refuseLongASINList(w, asins) {
		return
	}
	annots := make([]map[string]any, 0, len(asins))
	f.mu.Lock()
	for _, a := range asins {
		p, ok := f.positions[a]
		last := map[string]any{"status": "DoesNotExist"}
		if ok {
			last = map[string]any{
				"status":       "Exists",
				"position_ms":  p.ms,
				"last_updated": p.updated.Format("2006-01-02 15:04:05.000"),
			}
		}
		annots = append(annots, map[string]any{"asin": a, "last_position_heard": last})
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"asin_last_position_heard_annots": annots})
}

// putPosition is PUT /1.0/lastpositions/{asin} with {acr, asin, position_ms}.
func (f *Fake) putPosition(w http.ResponseWriter, r *http.Request, body []byte) {
	var in struct {
		ACR        string `json:"acr"`
		ASIN       string `json:"asin"`
		PositionMs *int64 `json:"position_ms"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.ACR == "" || in.ASIN != r.PathValue("asin") || in.PositionMs == nil || *in.PositionMs < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "want {acr, asin (the path's), position_ms >= 0}"})
		return
	}
	f.mu.Lock()
	now := f.opts.Now().UTC().Truncate(time.Millisecond)
	f.positions[in.ASIN] = position{ms: *in.PositionMs, updated: now}
	f.calls.PutPositions = append(f.calls.PutPositions, PutPosition{ASIN: in.ASIN, ACR: in.ACR, PositionMs: *in.PositionMs,
		At: now.Format(time.RFC3339Nano)})
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// amazonError answers in the identity API's error shape.
func amazonError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"response": map[string]any{"error": map[string]string{"code": code, "message": msg}}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

// queryASINs is the request's comma-separated asins query parameter, blanks
// left out.
func queryASINs(r *http.Request) []string {
	var asins []string
	for a := range strings.SplitSeq(r.URL.Query().Get("asins"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			asins = append(asins, a)
		}
	}
	return asins
}

// refuseLongASINList answers a 400 for a list over LastPositionsMax ASINs, as
// Audible's list-length check does, and reports whether it did.
func (f *Fake) refuseLongASINList(w http.ResponseWriter, asins []string) bool {
	if len(asins) <= f.opts.LastPositionsMax {
		return false
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"message": fmt.Sprintf(
		"1 validation error detected: Value at 'asins' failed to satisfy constraint: Member must have length less than or equal to %d", f.opts.LastPositionsMax)})
	return true
}
