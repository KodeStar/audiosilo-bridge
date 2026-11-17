//go:build audiblefake

package audiblejob

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/audible/audiblefake"
	"github.com/kodestar/audiosilo-bridge/internal/audibleacct"
	"github.com/kodestar/audiosilo-bridge/internal/jobs"
	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// accountServer adds the account route to fakeServer (the sign-in maps there).
type accountServer struct{ *fakeServer }

func (s accountServer) PutAudibleAccount(_ context.Context, id string, _ link.AudibleAccountInput) (*link.AudibleAccount, error) {
	return &link.AudibleAccount{ID: id}, nil
}
func (s accountServer) DeleteAudibleAccount(context.Context, string) error { return nil }

// A poll of the fake's 30 titles against a scratch library: three states,
// ASIN and fuzzy matches, a contested book left alone, a refusal marked; no
// credential in the log, the params or the result.
func TestPollAgainstTheFake(t *testing.T) {
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
	log := slog.New(slog.NewTextHandler(&logs, nil))
	m := audibleacct.New(audibleacct.Options{Vault: vault, DB: db, Log: log,
		Service: audibleacct.Audible(audible.WithEndpoints(audible.Endpoints{Amazon: ts.URL, AmazonAPI: ts.URL, API: ts.URL}))})

	fs := &fakeServer{pageSize: 3, books: []link.LibraryBook{
		// An ASIN match, a fuzzy match (no ASIN: enriched), a book two
		// titles land on (contested), and one nothing matches.
		{Path: "Quill/Orchard Cycle/1 The Glass Orchard", Title: "The Glass Orchard", Authors: []string{"Mara Quill"}, ASIN: "B0FAKE0001",
			Series: []link.SeriesRef{{Name: "Orchard Cycle", Position: "1"}}},
		{Path: "Liang/Mapmakers/1 The Cartographer's Apprentice", Title: "The Cartographer's Apprentice", Authors: []string{"Wen Liang"},
			Series: []link.SeriesRef{{Name: "Mapmakers", Position: "1"}}},
		{Path: "Dree/Salt and Ember", Title: "Salt and Ember", Authors: []string{"Callum Dree"}},
		{Path: "Nobody/Unrelated", Title: "An Unrelated Book", Authors: []string{"Nobody"}},
	}}
	srv := accountServer{fs}

	mp, _ := audible.MarketplaceFor("us")
	st, err := m.Start(mp.Code)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := browser.Get(st.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	acct, err := m.Finish(context.Background(), srv, "srv-1", audibleacct.Finish{Login: st.Login, RedirectURL: resp.Header.Get("Location"),
		Mapping: audibleacct.Mapping{UserID: 3, UserName: "Chris", LibraryID: 1, Policy: "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	// Contest "Salt and Ember": a second title with its name and author.
	// (The fake's own titles are distinct, so the poll's other title is a
	// copy under another ASIN; the matcher sees two records for one book.)
	contested := &contestAccounts{Manager: m, extraOf: "B0FAKE0019", extraASIN: "B0FAKE0119"}

	fs.t = t
	lts := httptest.NewServer(fs)
	defer lts.Close()
	cl, _ := link.New(lts.URL, "tok", "0.9.0")
	h := New(Options{Accounts: contested, Server: func() Server { return cl }, ServerID: func() string { return "srv-1" }, Log: log})
	params, _ := json.Marshal(Params{Account: acct.ID})
	out, err := h.Run(context.Background(), jobs.Job{ID: 1, Kind: Kind, Params: params, Attempt: 1}, jobs.LocalReporter())
	if err != nil {
		t.Fatal(err)
	}
	res := out.(Result)
	if res.Titles != 31 || res.Available != 23 || res.Preorder != 4 || res.Unavailable != 4 {
		t.Fatalf("result = %+v", res)
	}
	if res.MatchedASIN != 1 || res.MatchedFuzzy < 3 || res.Contested != 1 || res.Enriched != 1 {
		t.Fatalf("matches = %+v", res)
	}
	if fs.enriched["Liang/Mapmakers/1 The Cartographer's Apprentice"] != "B0FAKE0011" || fs.enriched["Dree/Salt and Ember"] != "" {
		t.Fatalf("enriched = %v", fs.enriched)
	}
	got, _ := m.Account(context.Background(), acct.ID)
	if got.LastTitles != 31 || got.LastPollAt == "" || got.RefusedAt != "" {
		t.Fatalf("account = %+v", got)
	}

	// A refused account: the poll fails and the account is marked.
	fake.Refuse()
	if _, err := h.Run(context.Background(), jobs.Job{ID: 2, Kind: Kind, Params: params, Attempt: 1}, jobs.LocalReporter()); err == nil {
		t.Fatal("a refused poll succeeded")
	}
	if got, _ := m.Account(context.Background(), acct.ID); got.RefusedAt == "" || got.LastError != audibleacct.RefusedText {
		t.Fatalf("refusal = %+v", got)
	}

	raw, _ := vault.Get(secrets.AudibleName(acct.ID))
	creds, _ := audible.ParseCredentials(raw)
	resRaw, _ := json.Marshal(res)
	for _, secret := range []string{creds.AccessToken, creds.RefreshToken, creds.ADPToken, creds.DeviceSerial, creds.CustomerID} {
		for what, text := range map[string]string{"log": logs.String(), "params": string(params), "result": string(resRaw)} {
			if secret != "" && strings.Contains(text, secret) {
				t.Fatalf("the %s holds a credential", what)
			}
		}
	}
}

// contestAccounts is the real manager with one extra title copied from
// extraOf under extraASIN.
type contestAccounts struct {
	*audibleacct.Manager
	extraOf, extraASIN string
}

func (c *contestAccounts) Library(ctx context.Context, id string) ([]source.Record, error) {
	recs, err := c.Manager.Library(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r.ASIN == c.extraOf {
			r.ASIN = c.extraASIN
			recs = append(recs, r)
			break
		}
	}
	return recs, nil
}
