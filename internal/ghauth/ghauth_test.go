package ghauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub is a scripted GitHub: each poll of the token endpoint takes the
// next answer of polls.
type fakeGitHub struct {
	mu      sync.Mutex
	polls   []map[string]any
	status  int // the token endpoint's status (200 by default)
	forms   []map[string]string
	revoked []string
}

func (f *fakeGitHub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("device code Accept = %q", r.Header.Get("Accept"))
		}
		_ = r.ParseForm()
		f.record(r)
		writeJSON(w, http.StatusOK, map[string]any{"device_code": "dev-123", "user_code": "ABCD-1234",
			"verification_uri": "https://github.com/login/device", "expires_in": 900, "interval": 5})
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.record(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.polls) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"error": "authorization_pending"})
			return
		}
		next := f.polls[0]
		f.polls = f.polls[1:]
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		writeJSON(w, status, next)
	})
	mux.HandleFunc("POST /credentials/revoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("revoke must be unauthenticated")
		}
		var body struct {
			Credentials []string `json:"credentials"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.revoked = append(f.revoked, body.Credentials...)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghu_good" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"login": "octocat", "id": 1})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGitHub) record(r *http.Request) {
	m := map[string]string{}
	for k := range r.PostForm {
		m[k] = r.PostForm.Get(k)
	}
	f.mu.Lock()
	f.forms = append(f.forms, m)
	f.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newClient(srv *httptest.Server, now time.Time) *Client {
	return &Client{ClientID: "Iv23test", WebBase: srv.URL, APIBase: srv.URL, Now: func() time.Time { return now }}
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestRequestCode(t *testing.T) {
	f := &fakeGitHub{}
	srv := f.server(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	dc, err := newClient(srv, now).RequestCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode != "ABCD-1234" || dc.DeviceCode != "dev-123" || dc.Interval != 5*time.Second ||
		!dc.ExpiresAt.Equal(now.Add(15*time.Minute)) || dc.VerificationURI != VerificationURI {
		t.Fatalf("device code = %+v", dc)
	}
	if f.forms[0]["client_id"] != "Iv23test" {
		t.Fatalf("form = %v", f.forms[0])
	}
}

func TestRequestCodeNeedsClientID(t *testing.T) {
	c := &Client{}
	if _, err := c.RequestCode(context.Background()); !errors.Is(err, ErrNoClientID) {
		t.Fatalf("err = %v, want ErrNoClientID", err)
	}
}

func TestWaitPendingSlowDownThenToken(t *testing.T) {
	f := &fakeGitHub{polls: []map[string]any{
		{"error": "authorization_pending"},
		{"error": "slow_down", "interval": 10},
		{"access_token": "ghu_new", "expires_in": 28800, "refresh_token": "ghr_new", "refresh_token_expires_in": 15897600,
			"token_type": "bearer", "scope": ""},
	}}
	srv := f.server(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	c := newClient(srv, now)
	var waits []time.Duration
	sleep := func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	tok, err := c.Wait(context.Background(), DeviceCode{DeviceCode: "dev-123", Interval: 5 * time.Second,
		ExpiresAt: now.Add(time.Minute)}, sleep)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "ghu_new" || tok.RefreshToken != "ghr_new" || !tok.ExpiresAt.Equal(now.Add(8*time.Hour)) ||
		!tok.Expiring() {
		t.Fatalf("token = %+v", tok)
	}
	if len(waits) != 3 || waits[0] != 5*time.Second || waits[2] != 10*time.Second {
		t.Fatalf("waits = %v (slow_down must raise the interval)", waits)
	}
	last := f.forms[len(f.forms)-1]
	if last["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || last["device_code"] != "dev-123" ||
		last["client_secret"] != "" {
		t.Fatalf("poll form = %v", last)
	}
}

func TestWaitSlowDownWithoutIntervalAddsFive(t *testing.T) {
	f := &fakeGitHub{polls: []map[string]any{{"error": "slow_down"}, {"access_token": "ghu_x"}}}
	srv := f.server(t)
	var waits []time.Duration
	_, err := newClient(srv, time.Now()).Wait(context.Background(), DeviceCode{DeviceCode: "d", Interval: 5 * time.Second},
		func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if waits[1] != 10*time.Second {
		t.Fatalf("waits = %v", waits)
	}
}

func TestWaitErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer map[string]any
		want   error
	}{
		{"expired_token", map[string]any{"error": "expired_token"}, ErrExpired},
		{"token_expired", map[string]any{"error": "token_expired"}, ErrExpired},
		{"access_denied", map[string]any{"error": "access_denied"}, ErrDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGitHub{polls: []map[string]any{tc.answer}}
			srv := f.server(t)
			_, err := newClient(srv, time.Now()).Wait(context.Background(), DeviceCode{DeviceCode: "d"}, noSleep)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("other", func(t *testing.T) {
		f := &fakeGitHub{polls: []map[string]any{{"error": "device_flow_disabled", "error_description": "Device Flow must be enabled"}}}
		srv := f.server(t)
		_, err := newClient(srv, time.Now()).Wait(context.Background(), DeviceCode{DeviceCode: "d"}, noSleep)
		var ge *Error
		if !errors.As(err, &ge) || ge.Code != "device_flow_disabled" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("expiry passes locally", func(t *testing.T) {
		f := &fakeGitHub{}
		srv := f.server(t)
		now := time.Now()
		_, err := newClient(srv, now).Wait(context.Background(), DeviceCode{DeviceCode: "d", ExpiresAt: now}, noSleep)
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		f := &fakeGitHub{}
		srv := f.server(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newClient(srv, time.Now()).Wait(ctx, DeviceCode{DeviceCode: "d"}, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRefresh(t *testing.T) {
	// The live answer's shape.
	f := &fakeGitHub{polls: []map[string]any{{"access_token": "ghu_2", "expires_in": 28800, "refresh_token": "ghr_2",
		"refresh_token_expires_in": 15724800, "token_type": "bearer", "scope": ""}}}
	srv := f.server(t)
	now := time.Now()
	tok, err := newClient(srv, now).Refresh(context.Background(), "ghr_1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "ghu_2" || tok.RefreshToken != "ghr_2" {
		t.Fatalf("token = %+v", tok)
	}
	form := f.forms[0]
	if form["grant_type"] != "refresh_token" || form["refresh_token"] != "ghr_1" || form["client_id"] != "Iv23test" {
		t.Fatalf("form = %v", form)
	}
	if _, ok := form["client_secret"]; ok {
		t.Fatal("a device-flow refresh sends no client secret")
	}
}

func TestRefreshBadToken(t *testing.T) {
	// A spent refresh token answers HTTP 200 incorrect_client_credentials (live);
	// every error code means signing in again.
	for _, tc := range []struct {
		status int
		code   string
	}{{http.StatusOK, "bad_refresh_token"}, {http.StatusBadRequest, "bad_refresh_token"},
		{http.StatusOK, "incorrect_client_credentials"}, {http.StatusOK, "unsupported_grant_type"}} {
		f := &fakeGitHub{status: tc.status, polls: []map[string]any{{"error": tc.code}}}
		srv := f.server(t)
		_, err := newClient(srv, time.Now()).Refresh(context.Background(), "ghr_old")
		if !errors.Is(err, ErrBadRefresh) || strings.Contains(err.Error(), "ghr_old") {
			t.Fatalf("%d %s: err = %v, want ErrBadRefresh", tc.status, tc.code, err)
		}
	}
}

func TestRevoke(t *testing.T) {
	f := &fakeGitHub{}
	srv := f.server(t)
	if err := newClient(srv, time.Now()).Revoke(context.Background(), "ghu_a", "", "ghr_b"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.revoked, ",") != "ghu_a,ghr_b" {
		t.Fatalf("revoked = %v", f.revoked)
	}
	// Nothing to revoke: no call.
	if err := (&Client{APIBase: "http://127.0.0.1:1"}).Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	err := (&Client{APIBase: srv.URL}).Revoke(context.Background(), "ghu_x")
	var ge *Error
	if !errors.As(err, &ge) || ge.Status != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "ghu_x") {
		t.Fatal("an error must never carry the token")
	}
}

func TestUser(t *testing.T) {
	f := &fakeGitHub{}
	srv := f.server(t)
	c := newClient(srv, time.Now())
	u, err := c.User(context.Background(), "ghu_good")
	if err != nil || u.Login != "octocat" {
		t.Fatalf("user = %+v, %v", u, err)
	}
	if _, err := c.User(context.Background(), "ghu_bad"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}
