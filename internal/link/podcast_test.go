package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

func TestPodcastWireMirror(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/bridge/status":
			json.NewEncoder(w).Encode(map[string]any{"libraries": []Library{{ID: 3, Kind: "podcasts"}}})
		case "/api/v1/bridge/libraries/3/marker":
			json.NewEncoder(w).Encode(map[string]string{"nonce": "0123456789abcdef0123456789abcdef"})
		case "/api/v1/bridge/libraries/4/marker":
			w.WriteHeader(404)
			w.Write([]byte(`{"code":"no_marker"}`))
		case "/api/v1/bridge/rescan":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if len(body) != 2 || body["path"] != "Show" || body["library_id"] != float64(3) {
				t.Error("rescan request", body)
			}
			w.Write([]byte(`{"path":"Show","episodes":412}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, "token", "0.9.0")
	reply, err := c.Heartbeat(context.Background(), Status{})
	if err != nil || reply.Libraries[0].LibraryKind() != "podcasts" {
		t.Fatal(reply, err)
	}
	if (Library{}).LibraryKind() != "books" {
		t.Fatal("older heartbeat must mean books")
	}
	nonce, err := c.Marker(context.Background(), 3)
	if err != nil || nonce != "0123456789abcdef0123456789abcdef" {
		t.Fatal(nonce, err)
	}
	if _, err := c.Marker(context.Background(), 4); !errors.Is(err, ErrNoMarker) {
		t.Fatal(err)
	}
	out, err := c.RescanPodcast(context.Background(), 3, "Show")
	if err != nil || out.Path != "Show" || out.Episodes != 412 {
		t.Fatal(out, err)
	}
}

func TestRetentionRouteMirrorsAndRefusals(t *testing.T) {
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/wire-2a.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]json.RawMessage
	json.Unmarshal(raw, &fixture)
	var check PodcastRetentionCheck
	json.Unmarshal(fixture["retention_check_response"], &check)
	var request PodcastRetentionRequest
	json.Unmarshal(fixture["retention_check_request"], &request)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer paired" {
			w.WriteHeader(401)
			w.Write([]byte(`{"code":"unauthorized"}`))
			return
		}
		switch r.URL.Path {
		case "/api/v1/bridge/libraries/3/podcasts/retention":
			if r.Method != "GET" || r.URL.Query().Get("path") != "Fixture Show" || r.URL.Query().Get("show_id") != request.ShowID {
				t.Error("policy request", r.URL)
			}
			fmt.Fprintf(w, `{"retention":%s,"future":true}`, fixture["retention_policy"])
		case "/api/v1/bridge/libraries/3/podcasts/retention/check":
			var body PodcastRetentionRequest
			if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || body != request {
				t.Error("check request", body)
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Write(fixture["retention_check_response"])
		default:
			w.WriteHeader(409)
			w.Write([]byte(`{"code":"identity_changed","error":"Identity changed."}`))
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, "paired", "test")
	policy, err := c.PodcastRetention(context.Background(), 3, "Fixture Show", request.ShowID)
	if err != nil || policy == nil || policy.Mode != "keep" || policy.Keep != 3 || policy.Days != 7 {
		t.Fatal(policy, err)
	}
	got, err := c.CheckPodcastRetention(context.Background(), 3, request)
	if err != nil || !reflect.DeepEqual(got, &check) {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"", "session", "api", "cast"} {
		c, _ := New(server.URL, bad, "test")
		if _, err := c.PodcastRetention(context.Background(), 3, "Fixture Show", request.ShowID); !errors.Is(err, ErrUnauthorized) {
			t.Fatal("policy denial", err)
		}
		if _, err := c.CheckPodcastRetention(context.Background(), 3, request); !errors.Is(err, ErrUnauthorized) {
			t.Fatal("check denial", err)
		}
	}
	if _, err := c.PodcastRetention(context.Background(), 4, "Fixture Show", request.ShowID); err == nil {
		t.Fatal("identity denial missing")
	}
	if _, err := c.CheckPodcastRetention(context.Background(), 4, request); err == nil {
		t.Fatal("identity denial missing")
	}
}

func TestIncompleteRetentionSnapshotDeniesAuthority(t *testing.T) {
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/wire-2a.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]json.RawMessage
	json.Unmarshal(raw, &fixture)
	var fields map[string]json.RawMessage
	json.Unmarshal(fixture["retention_check_response"], &fields)
	for _, key := range []string{"starred", "queued", "active_stream", "fetch_open", "eligible", "followers", "policy"} {
		copy := map[string]json.RawMessage{}
		for k, v := range fields {
			copy[k] = v
		}
		delete(copy, key)
		b, _ := json.Marshal(copy)
		var check PodcastRetentionCheck
		if json.Unmarshal(b, &check) == nil {
			t.Fatalf("missing %s accepted", key)
		}
		copy[key] = json.RawMessage(`null`)
		b, _ = json.Marshal(copy)
		if json.Unmarshal(b, &check) == nil {
			t.Fatalf("null %s accepted", key)
		}
	}
}
