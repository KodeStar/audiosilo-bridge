package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-bridge/internal/api"
	"github.com/kodestar/audiosilo-bridge/internal/config"
	"github.com/kodestar/audiosilo-bridge/internal/logring"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

func TestPodcastPageAPISecrecyAndAllowListWarning(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "config.yaml"), []byte("podcasts:\n  allow_networks: [127.0.0.1/32]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	ring := logring.New(100)
	log := slog.New(logring.NewHandler(slog.NewTextHandler(&captured, nil), ring))
	b, password, err := New(context.Background(), Options{DataDir: data, Version: "0.9.0", Logger: log, Ring: ring})
	if err != nil {
		t.Fatal(err)
	}
	defer b.db.Close()
	defer b.podcastHTTP.Close()
	privateURL := "https://user:pass@example.com/feed?auth=SECRET123"
	// The API boundary also protects rows written by an older Bridge.
	params, _ := json.Marshal(map[string]any{"feed_url": privateURL, "private": true})
	if err := b.db.RecordClaim(context.Background(), store.ServerJob{ID: 7, Kind: "podcast.subscribe", Params: params, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	rawResult, _ := json.Marshal(map[string]string{"message": "failed " + privateURL})
	if err := b.db.FinishServerJob(context.Background(), 7, 1, store.JobFailed, rawResult, "failed "+privateURL); err != nil {
		t.Fatal(err)
	}
	log.Warn("podcast failed", "feed_url", privateURL)
	token, err := b.auth.Login(password)
	if err != nil {
		t.Fatal(err)
	}
	handler := api.New(b, b.auth, nil, http.NotFoundHandler(), log).Handler()
	for _, route := range []string{"/api/jobs", "/api/jobs/7", "/api/status", "/api/logs"} {
		request := httptest.NewRequest(http.MethodGet, route, nil)
		request.RemoteAddr = "127.0.0.1:1234"
		request.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, request)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", route, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "SECRET123") || strings.Contains(rec.Body.String(), "user:pass") {
			t.Fatalf("%s leaked URL: %s", route, rec.Body)
		}
		if route == "/api/status" && !strings.Contains(rec.Body.String(), `"podcast_allow_networks":["127.0.0.1/32"]`) {
			t.Fatal("missing warning status", rec.Body)
		}
	}
	if !strings.Contains(captured.String(), "explicitly allowed non-public networks") || strings.Contains(captured.String(), "SECRET123") {
		t.Fatal("startup warning or log secrecy", captured.String())
	}
	st := b.Status()
	if !strings.Contains(fmt.Sprint(st.Kinds), "podcast.subscribe") || !strings.Contains(fmt.Sprint(st.Kinds), "podcast.poll") {
		t.Fatal("podcast kinds not advertised")
	}
	// A settings save must not hide the networks the running client still allows.
	if err := os.WriteFile(filepath.Join(data, "config.yaml"), []byte("podcasts:\n  allow_networks: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.save(func(*config.Config) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(b.Status().PodcastAllowNetworks) != 1 {
		t.Fatal("warning no longer reflects the running guarded client")
	}
}
