//go:build audiblefake

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audible/audiblefake"
	"github.com/kodestar/audiosilo-bridge/internal/audiblejob"
	"github.com/kodestar/audiosilo-bridge/internal/audiblesync"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/setup"
)

// audibleServer adds the server's Audible routes to fakeServer.
type audibleServer struct {
	*fakeServer
	mu      sync.Mutex
	puts    map[string]link.AudibleAccountInput
	titles  []link.AudibleTitles
	deleted []string
}

func (s *audibleServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/v1/bridge/users") && !strings.HasPrefix(p, "/api/v1/bridge/audible/") &&
		!strings.HasSuffix(p, "/books") {
		s.fakeServer.ServeHTTP(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer bridge-secret-token" {
		http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case p == "/api/v1/bridge/users":
		_, _ = io.WriteString(w, `{"users":[{"id":3,"username":"chris","display_name":"Chris","admin":true}]}`)
	case p == "/api/v1/bridge/libraries/1/books":
		_ = json.NewEncoder(w).Encode(map[string]any{"books": []link.LibraryBook{
			{Path: "Quill/The Glass Orchard", Title: "The Glass Orchard", Authors: []string{"Mara Quill"}, ASIN: "B0FAKE0001"},
		}, "next": nil})
	case strings.HasSuffix(p, "/titles"):
		var t link.AudibleTitles
		_ = json.NewDecoder(r.Body).Decode(&t)
		s.titles = append(s.titles, t)
		_ = json.NewEncoder(w).Encode(map[string]int{"stored": len(t.Titles)})
	case r.Method == http.MethodPut:
		var in link.AudibleAccountInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		if s.puts == nil {
			s.puts = map[string]link.AudibleAccountInput{}
		}
		id := strings.TrimPrefix(p, "/api/v1/bridge/audible/accounts/")
		s.puts[id] = in
		_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"id": id}})
	case r.Method == http.MethodDelete:
		s.deleted = append(s.deleted, strings.TrimPrefix(p, "/api/v1/bridge/audible/accounts/"))
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// The whole Bridge with the Audible fake: sign in from the page's backend,
// poll, remove. No credential reaches the logs, the database, config.yaml,
// the agent staging dir, the heartbeat, the page's answers or a poll's
// params and result.
func TestAudibleAccountThroughTheBridge(t *testing.T) {
	fake := audiblefake.New(audiblefake.Options{KeyBits: 1024})
	var syncFailure atomic.Value // string once the error canaries are installed
	fakeHandler := fake.Handler()
	fts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if msg, _ := syncFailure.Load().(string); msg != "" && r.URL.Path == "/1.0/annotations/lastpositions" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, msg)
			return
		}
		fakeHandler.ServeHTTP(w, r)
	}))
	defer fts.Close()
	t.Setenv(audible.FakeURLEnv, fts.URL)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "A", "B"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "A", "B", "1.mp3"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &audibleServer{fakeServer: &fakeServer{t: t, root: root, acceptsSetup: true}}
	srv := httptest.NewServer(fs)
	defer srv.Close()

	data := t.TempDir()
	var logs bytes.Buffer
	var logMu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, _, err := New(ctx, Options{DataDir: data, Version: "0.1.0", Listen: "127.0.0.1:0", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	if err := b.Pair(ctx, srv.URL, goodCode); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the library", func() bool { _, ok := b.libs.Library(1); return ok })

	page := b.Audible(ctx)
	if !page.Paired || len(page.Users) != 1 || page.Users[0].Name != "Chris" || len(page.Libraries) != 1 || page.UsersError != "" {
		t.Fatalf("page = %+v", page)
	}
	if err := b.FinishAudibleSignIn(ctx, api.AudibleFinish{Login: "nosuch", AudibleMapping: api.AudibleMapping{UserID: 3, LibraryID: 1, Policy: "manual"}}); err == nil {
		t.Fatal("an unknown sign-in finished")
	}
	if err := b.FinishAudibleSignIn(ctx, api.AudibleFinish{Login: "x", AudibleMapping: api.AudibleMapping{UserID: 9, LibraryID: 1, Policy: "manual"}}); err == nil ||
		!strings.Contains(err.Error(), "no person 9") {
		t.Fatalf("an unknown person = %v", err)
	}
	st, err := b.StartAudibleSignIn(ctx, "us")
	if err != nil || !strings.HasPrefix(st.URL, fts.URL+"/ap/signin?") {
		t.Fatalf("start = %+v, %v", st, err)
	}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := browser.Get(st.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	redirect := resp.Header.Get("Location")
	if err := b.FinishAudibleSignIn(ctx, api.AudibleFinish{Login: st.Login, RedirectURL: redirect,
		AudibleMapping: api.AudibleMapping{UserID: 3, LibraryID: 1, Policy: "manual"}}); err != nil {
		t.Fatal(err)
	}
	page = b.Audible(ctx)
	if len(page.Accounts) != 1 {
		t.Fatalf("accounts = %+v", page.Accounts)
	}
	acct := page.Accounts[0]
	if acct.Label != "Chris (US)" || acct.UserName != "Chris" || acct.LibraryName != "Books" || acct.OtherServer || acct.Refused {
		t.Fatalf("account = %+v", acct)
	}
	if in := fs.puts[acct.ID]; in.UserID != 3 || in.LibraryID != 1 || in.Marketplace != "us" {
		t.Fatalf("server mapping = %+v", in)
	}
	// Mapped on another server: its library id names nothing here.
	row, err := b.db.GetAudibleAccount(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	here := row.ServerID
	row.ServerID = "another-server"
	if err := b.db.MapAudibleAccount(ctx, row); err != nil {
		t.Fatal(err)
	}
	if other := b.Audible(ctx).Accounts[0]; !other.OtherServer || other.LibraryName != "" {
		t.Fatalf("on another server = %+v", other)
	}
	row.ServerID = here
	if err := b.db.MapAudibleAccount(ctx, row); err != nil {
		t.Fatal(err)
	}
	if item := setupItem(b.setup(ctx), setup.ItemAudible); !item.Done || item.Detail != "1 account signed in" {
		t.Fatalf("setup item = %+v", item)
	}
	// An earlier Bridge's sign-in (its device on the older profile, none
	// recorded): the page and the checklist ask for a sign-in again.
	if acct.OldProfile {
		t.Fatalf("a fresh sign-in flagged = %+v", acct)
	}
	row.DeviceProfile = ""
	if err := b.db.SignInAudibleAccount(ctx, row, time.Now()); err != nil {
		t.Fatal(err)
	}
	if old := b.Audible(ctx).Accounts[0]; !old.OldProfile {
		t.Fatalf("an old-profile account isn't flagged: %+v", old)
	}
	if item := setupItem(b.setup(ctx), setup.ItemAudible); item.Done || item.Detail != "1 account needs signing in again to keep backups working" {
		t.Fatalf("setup item = %+v", item)
	}
	row.DeviceProfile = audible.ProfileVersion
	if err := b.db.SignInAudibleAccount(ctx, row, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(b.runner.Kinds(), ","), "audible.poll") {
		t.Fatalf("kinds = %v", b.runner.Kinds())
	}

	h := audiblejob.New(audiblejob.Options{Accounts: b.audible, ServerID: func() string { return b.conn.Pairing().ServerID },
		Server: func() audiblejob.Server { return b.conn.Client() }})
	params, _ := json.Marshal(audiblejob.Params{Account: acct.ID})
	out, err := h.Run(ctx, jobs.Job{ID: 1, Kind: audiblejob.Kind, Params: params, Attempt: 1}, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	res := out.(audiblejob.Result)
	if res.Titles != 30 || res.MatchedASIN != 1 || len(fs.titles) != 1 || !fs.titles[0].Final {
		t.Fatalf("poll = %+v (%d calls)", res, len(fs.titles))
	}
	resRaw, _ := json.Marshal(res)

	raw, _ := b.vault.Get(secrets.AudibleName(acct.ID))
	creds, err := audible.ParseCredentials(raw)
	if err != nil || creds.ADPToken == "" {
		t.Fatalf("sealed = %v", err)
	}
	// An Audible sync error carries real fake-issued credential canaries. It
	// must be redacted before it reaches stored health, the result or setup.
	syncFailure.Store(strings.Join([]string{"sync failed", creds.AccessToken, creds.RefreshToken, creds.ADPToken,
		creds.CustomerID, creds.DeviceSerial}, " "))
	syncHandler := audiblesync.New(audiblesync.Options{Accounts: b.audible, DB: b.db,
		Server: func() audiblesync.Server { return b.conn.Client() }, ServerID: func() string { return "srv-1" }, Log: log, OnChange: b.Kick})
	syncParams, _ := json.Marshal(link.AudibleSyncParams{Account: acct.ID, Titles: []link.AudibleSyncTitle{
		{ASIN: "B0FAKE0001", LibraryID: 1, Path: "Quill/The Glass Orchard", RuntimeS: 60000},
	}})
	syncOut, syncErr := syncHandler.Run(ctx, jobs.Job{ID: 2, Kind: audiblesync.Kind, Params: syncParams, Attempt: 1}, jobs.LocalReporter())
	if syncErr == nil {
		t.Fatal("the failing sync succeeded")
	}
	syncResult, _ := json.Marshal(syncOut)
	row, err = b.db.GetAudibleAccount(ctx, acct.ID)
	if err != nil || row.SyncError == "" {
		t.Fatalf("sync health = %+v, %v", row, err)
	}
	waitFor(t, "sync health in the heartbeat", func() bool {
		beat, ok := fs.lastBeat()
		return ok && beat.Setup != nil && strings.Contains(setupItem(*beat.Setup, setup.ItemAudible).Detail, "sync failed")
	})
	beat, _ := fs.lastBeat()
	beatRaw, _ := json.Marshal(beat)
	setupRaw, _ := json.Marshal(b.setup(ctx))
	pageRaw, _ := json.Marshal(b.Audible(ctx))
	code, _ := audible.ParseAuthCode(redirect)
	keyLines := strings.Split(creds.DevicePrivateKey, "\n")
	secretsOf := []string{creds.AccessToken, creds.RefreshToken, creds.ADPToken, creds.CustomerID, creds.DeviceSerial, code}
	if len(keyLines) > 1 {
		secretsOf = append(secretsOf, keyLines[1]) // a line of the device key
	}
	logMu.Lock()
	logText := logs.String()
	logMu.Unlock()
	texts := map[string]string{"log": logText, "heartbeat": string(beatRaw), "setup": string(setupRaw), "page": string(pageRaw),
		"params": string(params), "result": string(resRaw), "sync_error": row.SyncError,
		"sync result": string(syncResult), "sync error": syncErr.Error()}
	if _, err := b.db.ClearAudibleSyncError(ctx, acct.ID); err != nil {
		t.Fatal(err)
	}
	if got := b.audibleSetup(ctx); got.SyncFailing != 0 || got.SyncError != "" {
		t.Fatalf("setup kept the cleared sync error: %+v", got)
	}
	for _, s := range secretsOf {
		if s == "" {
			continue
		}
		for what, text := range texts {
			if strings.Contains(text, s) {
				t.Fatalf("the %s holds a credential", what)
			}
		}
	}
	// On disk: only the vault's sealed file may hold it (sealed), so no file
	// under the data dir (the staging dir included) holds a secret in the clear.
	staging := b.config().AgentStagingDir(data)
	if !strings.HasPrefix(staging, data) {
		t.Fatalf("staging %s outside the data dir", staging)
	}
	err = filepath.Walk(data, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		body, _ := os.ReadFile(path)
		for _, s := range secretsOf {
			if s != "" && bytes.Contains(body, []byte(s)) {
				t.Errorf("%s holds a credential in the clear", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	warning, err := b.RemoveAudibleAccount(ctx, acct.ID)
	if err != nil || warning != "" {
		t.Fatalf("remove = %q, %v", warning, err)
	}
	if calls := fake.Calls(); len(calls.Deregistered) != 1 || len(fs.deleted) != 1 || fs.deleted[0] != acct.ID {
		t.Fatalf("deregistered %v, server deleted %v", calls.Deregistered, fs.deleted)
	}
	if len(b.Audible(ctx).Accounts) != 0 {
		t.Fatal("the account is kept")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func setupItem(s link.Setup, id string) link.SetupItem {
	for _, it := range s.Items {
		if it.ID == id {
			return it
		}
	}
	return link.SetupItem{}
}
