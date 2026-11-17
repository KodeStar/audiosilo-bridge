package link

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The sync routes as the server's handlers_audible_sync.go answers them: the
// listening POST's body (a listen of no length, a finish with only at), the
// progress GET's query and page, the bridge token in the header only, and the
// refusals each maps.
func TestAudibleSyncWireMirror(t *testing.T) {
	t.Parallel()
	var posted map[string]any
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("token") != "" {
			t.Errorf("%s %s: the token goes in the header only", r.Method, r.URL)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/bridge/audible/accounts/abcd1234/listening":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &posted)
			_, _ = w.Write([]byte(`{"server_time":"2026-11-03T12:00:00Z","results":[{"id":"aud-1","status":"not_applied","reason":"behind"},
				{"id":"aud-2","status":"applied"}],"progress":[{"library_id":1}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/bridge/audible/accounts/offoff00/listening":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"sync with Audible is off for this account","code":"sync_off"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/bridge/progress":
			queries = append(queries, r.URL.RawQuery)
			switch r.URL.Query().Get("account") {
			case "abcd1234":
				_, _ = w.Write([]byte(`{"rows":[{"library_id":1,"path":"A/One","asin":"B000000001","position":1234.5,"duration":36000,
					"finished":false,"updated_at":"2026-11-03T11:00:00Z","device_id":"web-1","version":42}],"next":"Q1","more":true}`))
			case "kidkid00":
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"x","code":"user_unavailable"}`))
			case "gone0000":
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"x","code":"unknown_account"}`))
			case "badbad00":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"x","code":"invalid_request"}`))
			default:
				_, _ = w.Write([]byte(`{"rows":null,"next":"","more":false}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, "tok", "0.9.0")
	ctx := context.Background()

	pos := 1234.5
	ans, err := c.PostAudibleListening(ctx, "abcd1234", AudibleListening{SentAt: "2026-11-03T12:00:00Z", Events: []AudibleEvent{
		{ID: "aud-1", Kind: EventListen, LibraryID: 1, Path: "A/One", FromPos: &pos, ToPos: &pos,
			StartedAt: "2026-11-03T10:00:00Z", EndedAt: "2026-11-03T10:00:00Z", Start: "jump"},
		{ID: "aud-2", Kind: EventFinish, LibraryID: 1, Path: "A/Two", At: "2026-11-03T10:00:00Z"},
	}})
	if err != nil || len(ans.Results) != 2 || ans.Results[0].Reason != SyncBehind || ans.Results[1].Status != SyncApplied {
		t.Fatalf("answer = %+v, %v", ans, err)
	}
	events := posted["events"].([]any)
	listen, finish := events[0].(map[string]any), events[1].(map[string]any)
	if posted["sent_at"] != "2026-11-03T12:00:00Z" || listen["from_pos"] != 1234.5 || listen["to_pos"] != 1234.5 ||
		listen["start"] != "jump" || listen["started_at"] != listen["ended_at"] || listen["kind"] != "listen" {
		t.Fatalf("posted = %v", posted)
	}
	// A finish carries only at (the server's event decoder is strict, and a
	// listen's places would read as a span).
	for _, k := range []string{"from_pos", "to_pos", "started_at", "ended_at", "start"} {
		if _, ok := finish[k]; ok {
			t.Errorf("a finish carries %s: %v", k, finish)
		}
	}
	if finish["at"] != "2026-11-03T10:00:00Z" || finish["kind"] != "finish" {
		t.Fatalf("finish = %v", finish)
	}
	if _, err := c.PostAudibleListening(ctx, "offoff00", AudibleListening{SentAt: "x"}); !errors.Is(err, ErrSyncOff) {
		t.Fatalf("sync off = %v", err)
	}
	// A 404 naming no account is a server with no such route (before 2.6.0).
	if _, err := c.PostAudibleListening(ctx, "nosuch00", AudibleListening{SentAt: "x"}); !errors.Is(err, ErrNoAudibleSync) {
		t.Fatalf("old server = %v", err)
	}

	page, err := c.AudibleProgress(ctx, "abcd1234", "Q0", 0)
	if err != nil || len(page.Rows) != 1 || page.Next != "Q1" || !page.More ||
		page.Rows[0] != (AudibleProgressRow{LibraryID: 1, Path: "A/One", ASIN: "B000000001", Position: 1234.5, Duration: 36000,
			UpdatedAt: "2026-11-03T11:00:00Z", DeviceID: "web-1", Version: 42}) {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if queries[0] != "account=abcd1234&limit=200&since=Q0" {
		t.Fatalf("query = %q", queries[0])
	}
	if page, err := c.AudibleProgress(ctx, "empty000", "", 5); err != nil || page.Rows == nil || queries[1] != "account=empty000&limit=5" {
		t.Fatalf("empty = %+v, %v (%q)", page, err, queries[1])
	}
	if _, err := c.AudibleProgress(ctx, "kidkid00", "", 0); !errors.Is(err, ErrUserUnavailable) {
		t.Fatalf("kid = %v", err)
	}
	if _, err := c.AudibleProgress(ctx, "gone0000", "", 0); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("gone = %v", err)
	}
	// A 400 blames the cursor only when one was sent.
	if _, err := c.AudibleProgress(ctx, "badbad00", "Q9", 0); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("bad cursor = %v", err)
	}
	if _, err := c.AudibleProgress(ctx, "badbad00", "", 0); err == nil || errors.Is(err, ErrBadCursor) {
		t.Fatalf("400 from the start = %v", err)
	}
}
