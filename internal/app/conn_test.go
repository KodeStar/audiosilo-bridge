package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

// The paired link knows the id of the server it reaches, from the pairing and
// again after a restart: the job runner's one rule for which jobs are the
// link's reads the two together (link.Client.ServerID, BR-LINK-RULE).
func TestPairedLinkKnowsItsServer(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"tok","server_id":"srv-9","server_name":"Home"}`)
	}))
	defer srv.Close()
	data := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(data, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	vault, _, err := secrets.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	c, err := openConn(ctx, db, vault, "0.1.0", log)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Pair(ctx, srv.URL, "CODE"); err != nil {
		t.Fatal(err)
	}
	if id := c.Client().ServerID(); id != "srv-9" {
		t.Fatalf("the paired link's server = %q, want srv-9", id)
	}
	again, err := openConn(ctx, db, vault, "0.1.0", log)
	if err != nil {
		t.Fatal(err)
	}
	if cl := again.Client(); cl == nil || cl.ServerID() != "srv-9" {
		t.Fatalf("after a restart the link's server = %v, want srv-9", cl)
	}
}

// A pairing stored without its server's id (or a pairing answer that named
// none) leaves the link of no known server, on which no job runs (denied:
// Bridge.server is nil); the heartbeat's first step asks the server for its
// id (GET /api/v1/server) and keeps it, so the link counts from then on and
// after a restart (allowed).
func TestUnknownServerIDIsLearnedBeforeJobsRun(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/bridge/pair":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"tok","server_name":"Home"}`)
		case "/api/v1/server":
			_, _ = io.WriteString(w, `{"name":"Home","server_id":"srv-7"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	data := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(data, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	vault, _, err := secrets.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	c, err := openConn(ctx, db, vault, "0.1.0", log)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Pair(ctx, srv.URL, "CODE"); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{conn: c, lastBeat: time.Now()}
	if b.server() != nil {
		t.Fatal("a link of no known server counts for jobs")
	}
	cl := c.learnServerID(ctx, c.Client())
	if cl.ServerID() != "srv-7" || c.Client() != cl || c.Pairing().ServerID != "srv-7" {
		t.Fatalf("learned %q, pairing %q", cl.ServerID(), c.Pairing().ServerID)
	}
	if b.server() == nil {
		t.Fatal("the link of a learned server doesn't count")
	}
	again, err := openConn(ctx, db, vault, "0.1.0", log)
	if err != nil {
		t.Fatal(err)
	}
	if again.Client().ServerID() != "srv-7" {
		t.Fatal("the learned id didn't outlive a restart")
	}
}

// An id that arrives for a link the pairing no longer uses (unpaired while the
// question was out) is recorded nowhere (denied): kept, a pairing with another
// server made meanwhile would read as this server's after a restart, and this
// server's jobs would be reported there.
func TestServerIDOfAReplacedLinkIsNotRecorded(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/bridge/pair":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"tok","server_name":"Home"}`)
		case "/api/v1/server":
			_, _ = io.WriteString(w, `{"name":"Home","server_id":"srv-7"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	data := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(data, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	vault, _, err := secrets.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := openConn(ctx, db, vault, "0.1.0", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Pair(ctx, srv.URL, "CODE"); err != nil {
		t.Fatal(err)
	}
	old := c.Client()
	if err := c.Unpair(ctx, "unpaired on the page"); err != nil {
		t.Fatal(err)
	}
	if cl := c.learnServerID(ctx, old); cl != old || cl.ServerID() != "" {
		t.Fatalf("a replaced link learned %q", cl.ServerID())
	}
	if id, _, _ := db.GetSetting(ctx, keyServerID); id != "" {
		t.Fatalf("a replaced link's server id %q was recorded as the pairing's", id)
	}
	if c.Client() != nil || c.Pairing().ServerID != "" {
		t.Fatal("a replaced link's answer changed the pairing")
	}
}

// An unpair on the page hands the link it ended to onUnpair, so the runner
// stops that pairing's jobs (allowed), a pairing whose token the server
// refuses included (its jobs wait to resume); with no pairing there is nothing
// to stop (denied).
func TestUnpairHandsOverTheLinkItEnded(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"tok","server_id":"srv-9","server_name":"Home"}`)
	}))
	defer srv.Close()
	data := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(data, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	vault, _, err := secrets.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := openConn(ctx, db, vault, "0.1.0", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	var ended []*link.Client
	c.onUnpair = func(cl *link.Client) {
		if c.Client() != nil {
			t.Error("the jobs were stopped while the link was still the pairing's")
		}
		// The token and the pairing's identity are forgotten first: the jobs'
		// rows are another pairing's before the runner hears of it, so a
		// restart from here resumes none of them.
		if tok, _ := vault.Get(secrets.ServerToken); tok != "" {
			t.Error("the jobs were stopped before the token was forgotten")
		}
		if id, ok, _ := db.GetSetting(ctx, keyPairingID); ok || id != "" || c.jobsPairing().ID != "" {
			t.Error("the jobs were stopped before the pairing's identity was forgotten", id)
		}
		ended = append(ended, cl)
	}
	if err := c.Unpair(ctx, "unpaired on the page"); err != nil || len(ended) != 0 {
		t.Fatalf("an unpair with no pairing: %v, ended %d links", err, len(ended))
	}
	for _, refused := range []bool{false, true} {
		if err := c.Pair(ctx, srv.URL, "CODE"); err != nil {
			t.Fatal(err)
		}
		cl := c.Client()
		if refused {
			c.Refuse(ctx, cl, "refused")
		}
		if err := c.Unpair(ctx, "unpaired on the page"); err != nil {
			t.Fatal(err)
		}
		if len(ended) == 0 || ended[len(ended)-1] != cl {
			t.Fatalf("refused %v: the unpair ended %v, want the pairing's link", refused, ended)
		}
	}
	if len(ended) != 2 {
		t.Fatalf("ended %d links, want one per unpair", len(ended))
	}
}

// A pairing's identity (recorded with every job claimed under it) is new for
// every pairing after an unpair, the same server's included (denied: kept, the
// earlier pairing's jobs would resume under it), and for a pairing with
// another server; a pairing with the same server while the token is refused
// keeps it (allowed: the jobs the refusal stopped resume on the new link), as
// does a restart. The link carries it.
func TestPairingIdentity(t *testing.T) {
	ctx := context.Background()
	serverID := "srv-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"tok","server_id":"`+serverID+`","server_name":"Home"}`)
	}))
	defer srv.Close()
	data := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(data, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	vault, _, err := secrets.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	c, err := openConn(ctx, db, vault, "0.1.0", log)
	if err != nil {
		t.Fatal(err)
	}
	pair := func() string {
		t.Helper()
		if err := c.Pair(ctx, srv.URL, "CODE"); err != nil {
			t.Fatal(err)
		}
		id := c.jobsPairing().ID
		stored, _, _ := db.GetSetting(ctx, keyPairingID)
		if id == "" || stored != id || c.Client().PairingID() != id || c.jobsPairing().ServerID != serverID {
			t.Fatalf("pairing %q, stored %q, link %q", id, stored, c.Client().PairingID())
		}
		return id
	}
	first := pair()
	again, err := openConn(ctx, db, vault, "0.1.0", log)
	if err != nil {
		t.Fatal(err)
	}
	if again.jobsPairing().ID != first || again.Client().PairingID() != first {
		t.Fatal("a restart changed the pairing's identity")
	}
	c.Refuse(ctx, c.Client(), "refused")
	if c.jobsPairing().ID != first {
		t.Fatal("a refusal changed the pairing's identity")
	}
	if id := pair(); id != first {
		t.Fatal("a pairing with the same server while refused made a new identity")
	}
	c.Refuse(ctx, c.Client(), "refused")
	serverID = "srv-2"
	second := pair()
	if second == first {
		t.Fatal("a pairing with another server kept the identity")
	}
	serverID = "srv-1"
	if err := c.Unpair(ctx, "unpaired on the page"); err != nil {
		t.Fatal(err)
	}
	if c.jobsPairing().ID != "" {
		t.Fatal("an unpair kept the identity")
	}
	if id := pair(); id == first || id == second {
		t.Fatal("a pairing after an unpair kept an earlier identity")
	}
}
