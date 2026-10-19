package contrib

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeDevice is a scripted device sign-in.
type fakeDevice struct {
	token   string
	err     error
	refused int
}

func (f *fakeDevice) Token(context.Context) (string, error) { return f.token, f.err }
func (f *fakeDevice) Refused(context.Context, string)       { f.refused++ }

func newSource(env string, dev DeviceTokens, gh string) *TokenSource {
	ts := NewTokenSource(dev, nil)
	ts.getenv = func(name string) string {
		if name == GitHubTokenEnv {
			return env
		}
		return ""
	}
	ts.ghToken = func(context.Context) (string, error) {
		if gh == "" {
			return "", errors.New("gh not logged in")
		}
		return gh, nil
	}
	return ts
}

// TestResolveOrder: the environment wins (read live), then the sign-in, then gh.
func TestResolveOrder(t *testing.T) {
	ctx := context.Background()
	dev := &fakeDevice{token: "ghu_device"}
	for _, tc := range []struct {
		name, env, gh, want, from string
		dev                       DeviceTokens
	}{
		{"env", " ghp_env ", "gho_cli", "ghp_env", FromEnv, dev},
		{"device", "", "gho_cli", "ghu_device", FromDevice, dev},
		{"gh", "", "gho_cli", "gho_cli", FromGH, &fakeDevice{err: errors.New("not signed in")}},
		{"gh, no device", "", "gho_cli", "gho_cli", FromGH, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, from, err := newSource(tc.env, tc.dev, tc.gh).Resolve(ctx)
			if err != nil || tok != tc.want || from != tc.from {
				t.Fatalf("got (%q, %q, %v), want (%q, %q)", tok, from, err, tc.want, tc.from)
			}
		})
	}
}

// TestResolveEnvIsLive: the variable is read at every Resolve, never sealed.
func TestResolveEnvIsLive(t *testing.T) {
	env := "ghp_one"
	ts := newSource("", nil, "")
	ts.getenv = func(string) string { return env }
	if tok, _, _ := ts.Resolve(context.Background()); tok != "ghp_one" {
		t.Fatal(tok)
	}
	env = ""
	if _, _, err := ts.Resolve(context.Background()); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v (an unset variable is no credential)", err)
	}
}

// TestResolveNoCredential: nothing resolves -> ErrNoCredential.
func TestResolveNoCredential(t *testing.T) {
	ts := newSource("", &fakeDevice{err: errors.New("no")}, "")
	if _, _, err := ts.Resolve(context.Background()); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}
	ts.ghToken = func(context.Context) (string, error) { return "", nil }
	if _, _, err := ts.Resolve(context.Background()); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("empty-token err = %v, want ErrNoCredential", err)
	}
}

// TestRefusedTokens: a refused environment token is skipped (the sign-in
// serves), a refused device token is the sign-in's to record.
func TestRefusedTokens(t *testing.T) {
	ctx := context.Background()
	dev := &fakeDevice{token: "ghu_device"}
	changed := 0
	ts := newSource("ghp_env", dev, "")
	ts.onChange = func() { changed++ }
	ts.Refused(ctx, FromEnv, "ghp_env")
	if tok, from, _ := ts.Resolve(ctx); tok != "ghu_device" || from != FromDevice || changed != 1 {
		t.Fatalf("got (%q, %q), changed %d", tok, from, changed)
	}
	ts.Refused(ctx, FromDevice, "ghu_device")
	if dev.refused != 1 {
		t.Fatal("a refused device token is recorded on the sign-in")
	}
	// A new environment token is tried again.
	ts.getenv = func(string) string { return "ghp_new" }
	if tok, _, _ := ts.Resolve(ctx); tok != "ghp_new" {
		t.Fatal(tok)
	}
}

// TestClientReportsUnauthorized: a 401 on a call made with a resolved token
// records the refusal; a 2xx doesn't.
func TestClientReportsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer ghp_good" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"number":1}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	ts := newSource("ghp_bad", nil, "")
	tok, from, _ := ts.Resolve(ctx)
	if _, err := NewClientFor(srv.URL, tok, from, ts).GetIssue(ctx, "o/r", 1); err == nil {
		t.Fatal("a 401 is an error")
	}
	if _, _, err := ts.Resolve(ctx); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("after a 401: %v (the refused token isn't offered again)", err)
	}
	good := newSource("ghp_good", nil, "")
	tok, from, _ = good.Resolve(ctx)
	if _, err := NewClientFor(srv.URL, tok, from, good).GetIssue(ctx, "o/r", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := good.Resolve(ctx); err != nil {
		t.Fatal("a working token stays")
	}
}

// TestGhAuthTokenExecReal exercises the real argv-only shell-out against a fake
// gh script that prints a token to stdout.
func TestGhAuthTokenExecReal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script fixture is POSIX")
	}
	gh := writeFakeGh(t, "#!/bin/sh\necho gho_realtoken\n")
	tok, err := ghAuthTokenExec(context.Background(), gh)
	if err != nil {
		t.Fatalf("ghAuthTokenExec: %v", err)
	}
	if tok != "gho_realtoken" {
		t.Fatalf("token = %q, want gho_realtoken (trimmed)", tok)
	}
}

// TestGhAuthTokenExecNeverLeaksToken is the security regression: a gh that
// prints the token to stdout but exits non-zero must NOT surface the token in
// the returned error.
func TestGhAuthTokenExecNeverLeaksToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script fixture is POSIX")
	}
	const secret = "gho_TOPSECRET_leakcanary"
	// Print the token to stdout (as gh does), write a benign stderr line, then
	// fail. The token must never appear in the error.
	gh := writeFakeGh(t, "#!/bin/sh\necho "+secret+"\necho 'not logged in' 1>&2\nexit 1\n")
	_, err := ghAuthTokenExec(context.Background(), gh)
	if err == nil {
		t.Fatal("expected an error from a failing gh")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the token: %v", err)
	}
}

// writeFakeGh writes an executable fake gh script and returns its path.
func writeFakeGh(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "gh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
