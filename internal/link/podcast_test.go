package link

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
