package link

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRejectedAudibleTitles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"paged decisions", 200, `{"titles":[{"asin":"B000000001","rejected":true},{"asin":"B000000002","rejected":false},{"asin":"B000000003"}],"next":"B000000003"}`, false},
		{"older server with only PUT", 405, "", false},
		{"older server with route fallback", 404, `{"code":"not_found"}`, false},
		{"unknown account", 404, `{"code":"unknown_account"}`, true},
		{"rate limit", 429, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/bridge/audible/accounts/acct0001/titles" ||
					r.Header.Get("Authorization") != "Bearer tok" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
				switch after := r.URL.Query().Get("after"); after {
				case "":
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				case "B000000003":
					_, _ = io.WriteString(w, `{"titles":[{"asin":"B000000004","rejected":true}],"next":""}`)
				default:
					t.Errorf("unexpected cursor %q", after)
				}
			}))
			defer server.Close()
			c, _ := New(server.URL, "tok", "test")
			got, err := c.RejectedAudibleTitles(t.Context(), "acct0001")
			if (err != nil) != tc.wantErr {
				t.Fatalf("decisions: %v", err)
			}
			if tc.name == "unknown account" && !errors.Is(err, ErrUnknownAccount) {
				t.Fatalf("account refusal = %v", err)
			}
			if tc.status == http.StatusOK {
				if len(got) != 2 || !got["B000000001"] || !got["B000000004"] {
					t.Fatalf("rejected = %v", got)
				}
			} else if len(got) != 0 {
				t.Fatalf("unexpected decisions: %v", got)
			}
		})
	}
}

// The Audible routes as the server's handlers_audible.go answers them: methods,
// paths, the bridge token in the header only, the bodies' field names, and the
// error codes each maps.
func TestAudibleWireMirror(t *testing.T) {
	t.Parallel()
	type seen struct{ method, path, query, body string }
	var calls []seen
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls = append(calls, seen{r.Method, r.URL.Path, r.URL.RawQuery, string(raw)})
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("token") != "" {
			t.Errorf("%s %s: the token goes in the header only", r.Method, r.URL)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/bridge/users":
			_, _ = w.Write([]byte(`{"users":[{"id":3,"username":"chris","display_name":"Chris","admin":true}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/bridge/audible/accounts/abcd1234":
			_, _ = w.Write([]byte(`{"account":{"id":"abcd1234","label":"Chris (US)","marketplace":"us","user_id":3,
				"user":{"id":3,"display_name":"Chris"},"library_id":1,"library":{"id":1,"name":"Books"},"paused":false,
				"policy":"manual","sync":true,"poll_hours":6,"created_at":"x","updated_at":"x","last_poll_at":null}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/bridge/audible/accounts/badbad99":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such user","code":"unknown_user"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/bridge/audible/accounts/abcd1234":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/bridge/audible/accounts/abcd1234/titles":
			_, _ = w.Write([]byte(`{"stored":2}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/bridge/audible/accounts/gone0000/titles":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such Audible account","code":"unknown_account"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/bridge/libraries/1/books":
			switch r.URL.Query().Get("after") {
			case "gone":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid cursor","code":"invalid_cursor"}`))
				return
			case "old":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid audible request: bad cursor","code":"invalid_request"}`))
				return
			}
			if r.URL.Query().Get("after") == "" {
				_, _ = w.Write([]byte(`{"books":[{"path":"A/One","title":"One","authors":["A"],"narrators":[],
					"series":[{"name":"S","position":"1-3"}],"asin":"B000000001","isbn":"","duration":36012.4}],"next":"QQ"}`))
				return
			}
			_, _ = w.Write([]byte(`{"books":[],"next":null}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/bridge/libraries/1/enrichment":
			switch r.URL.Query().Get("path") {
			case "A/One", ".":
				w.WriteHeader(http.StatusNoContent)
			case "A/Set":
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"the book already has an ASIN","code":"asin_set"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"no such book","code":"book_not_found"}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, "tok", "0.9.0")
	ctx := context.Background()

	users, err := c.Users(ctx)
	if err != nil || len(users) != 1 || users[0] != (User{ID: 3, Username: "chris", DisplayName: "Chris", Admin: true}) {
		t.Fatalf("users = %+v, %v", users, err)
	}

	in := AudibleAccountInput{Label: "Chris (US)", Marketplace: "us", UserID: 3, LibraryID: 1, Policy: "manual"}
	acct, err := c.PutAudibleAccount(ctx, "abcd1234", in)
	if err != nil || acct.ID != "abcd1234" || acct.User.DisplayName != "Chris" || acct.LibraryID == nil || *acct.LibraryID != 1 ||
		acct.PollHours != 6 || acct.Paused {
		t.Fatalf("account = %+v, %v", acct, err)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(calls[len(calls)-1].body), &body)
	for _, k := range []string{"label", "marketplace", "user_id", "library_id", "policy"} {
		if _, ok := body[k]; !ok {
			t.Errorf("account body has no %q: %v", k, body)
		}
	}
	// Whether the places sync is the console's: the server ignores a sync
	// sent here, so none is (bridge#50).
	if len(body) != 5 {
		t.Errorf("account body = %v, want the mapping alone", body)
	}
	var apiErr *APIError
	if _, err := c.PutAudibleAccount(ctx, "badbad99", in); !errors.As(err, &apiErr) || apiErr.Code != "unknown_user" {
		t.Fatalf("unknown user = %v", err)
	}
	if err := c.DeleteAudibleAccount(ctx, "abcd1234"); err != nil {
		t.Fatal(err)
	}

	n, err := c.PutAudibleTitles(ctx, "abcd1234", AudibleTitles{PollID: "p1", Final: true, Titles: []AudibleTitle{
		{ASIN: "B000000001", Title: "One", Authors: []string{"A"}, Series: []SeriesRef{{Name: "S", Position: "1"}},
			ReleaseDate: "2026-11-20", RuntimeS: 36000, State: AudibleAvailable,
			Match: &AudibleMatch{LibraryID: 1, Path: "A/One", How: MatchASIN}},
		{ASIN: "B000000002", State: AudiblePreorder},
		{ASIN: "B000000003", State: AudibleAvailable, Reason: ReasonContested, ContestedPath: "Ben Aar/LT02 - Moon over Tidewater",
			Planned: &AudiblePlan{LibraryID: 1, Path: "Ben Aar/Lanterns/LT01 - Lanterns/Lanterns.m4b", File: "Lanterns.m4b",
				Kind: PlanSeriesExisting, Siblings: 7, State: PlanJoins, Template: "{Author}/{Title}"}},
	}})
	if err != nil || n != 2 {
		t.Fatalf("titles = %d, %v", n, err)
	}
	raw := calls[len(calls)-1].body
	// The server decodes the titles strictly: every field by its exact name.
	var sent struct {
		Titles []map[string]json.RawMessage `json:"titles"`
	}
	if err := json.Unmarshal([]byte(raw), &sent); err != nil || len(sent.Titles) != 3 {
		t.Fatalf("titles body %s: %v", raw, err)
	}
	if _, ok := sent.Titles[0]["contested_path"]; ok {
		t.Errorf("an uncontested title sends contested_path: %s", raw)
	}
	if got := string(sent.Titles[0]["planned"]); got != "null" {
		t.Errorf("a matched title's planned = %s, want null", got)
	}
	if got := string(sent.Titles[2]["contested_path"]); got != `"Ben Aar/LT02 - Moon over Tidewater"` {
		t.Errorf("contested_path = %s", got)
	}
	if got, want := string(sent.Titles[2]["planned"]), `{"library_id":1,"path":"Ben Aar/Lanterns/LT01 - Lanterns/Lanterns.m4b",`+
		`"file":"Lanterns.m4b","kind":"series_existing","siblings":7,"state":"joins","conflict":"","template":"{Author}/{Title}"}`; got != want {
		t.Errorf("planned = %s\nwant      %s", got, want)
	}
	var back AudibleTitle
	if err := json.Unmarshal(mustJSON(t, sent.Titles[2]), &back); err != nil || back.Planned == nil || back.Planned.Siblings != 7 ||
		back.ContestedPath != "Ben Aar/LT02 - Moon over Tidewater" {
		t.Errorf("round trip = %+v, %v", back, err)
	}
	for _, want := range []string{`"poll_id":"p1"`, `"final":true`, `"release_date":"2026-11-20"`, `"runtime_s":36000`,
		`"state":"available"`, `"match":{"library_id":1,"path":"A/One","how":"asin"}`, `"match":null`, `"series":[{"name":"S","position":"1"}]`} {
		if !strings.Contains(raw, want) {
			t.Errorf("titles body lacks %s: %s", want, raw)
		}
	}
	// An empty final call still sends a list, never null.
	if _, err := c.PutAudibleTitles(ctx, "abcd1234", AudibleTitles{PollID: "p2", Final: true}); err != nil ||
		!strings.Contains(calls[len(calls)-1].body, `"titles":[]`) {
		t.Fatalf("empty final = %v %s", err, calls[len(calls)-1].body)
	}
	if _, err := c.PutAudibleTitles(ctx, "gone0000", AudibleTitles{PollID: "p3"}); !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("unknown account = %v", err)
	}

	page, err := c.LibraryBooks(ctx, 1, "", 500)
	if err != nil || page.Next != "QQ" || len(page.Books) != 1 || page.Books[0].Series[0].Position != "1-3" ||
		page.Books[0].ASIN != "B000000001" || page.Books[0].Duration != 36012.4 {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	if q := calls[len(calls)-1].query; q != "limit=500" {
		t.Fatalf("query = %q", q)
	}
	page, err = c.LibraryBooks(ctx, 1, "QQ", 0)
	if err != nil || page.Next != "" || page.Books == nil {
		t.Fatalf("last page = %+v, %v", page, err)
	}
	if q := calls[len(calls)-1].query; q != "after=QQ" {
		t.Fatalf("query = %q", q)
	}
	// A refused after is ErrBadCursor: the code, or a server before 2.7.0's
	// message (bridge#50).
	for _, after := range []string{"gone", "old"} {
		if _, err := c.LibraryBooks(ctx, 1, after, 0); !errors.Is(err, ErrBadCursor) {
			t.Fatalf("refused after %q = %v", after, err)
		}
	}

	if err := c.Enrich(ctx, 1, "A/One", "B000000001"); err != nil {
		t.Fatal(err)
	}
	if b := calls[len(calls)-1].body; b != `{"asin":"B000000001"}` {
		t.Fatalf("enrichment body = %s", b)
	}
	if q := calls[len(calls)-1].query; q != "path=A%2FOne" {
		t.Fatalf("enrichment query = %q", q)
	}
	// The library's root book (a library that is one book) is "." on the wire.
	if err := c.Enrich(ctx, 1, ".", "B000000001"); err != nil {
		t.Fatal(err)
	}
	if q := calls[len(calls)-1].query; q != "path=." {
		t.Fatalf("root enrichment query = %q", q)
	}
	if err := c.Enrich(ctx, 1, "A/Set", "B000000001"); !errors.Is(err, ErrASINSet) {
		t.Fatalf("asin set = %v", err)
	}
	if err := c.Enrich(ctx, 1, "A/None", "B000000001"); !errors.Is(err, ErrNoBook) {
		t.Fatalf("no book = %v", err)
	}
}

// A 429 carries its Retry-After (seconds or an HTTP date) on the APIError, so
// a caller can distinguish an explicit zero wait from a missing/invalid header.
func TestRateLimitedRetryAfter(t *testing.T) {
	t.Parallel()
	var retryAfter string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
	}))
	defer server.Close()
	c, _ := New(server.URL, "tok", "0.9.0")
	c.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	for _, tc := range []struct {
		header   string
		min, max time.Duration
		provided bool
	}{
		{"7", 7 * time.Second, 7 * time.Second, true},
		{"0", 0, 0, true},
		{"", 0, 0, false},
		{"soon", 0, 0, false},
		{"-3", 0, 0, false},
		{"+3", 0, 0, false},
		{"987654321012345678901234567890", 24 * time.Hour, 24 * time.Hour, true},
		{"99999999999999", 24 * time.Hour, 24 * time.Hour, true},
		{time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat), 80 * time.Second, 90 * time.Second, true},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0, 0, true},
		{time.Now().AddDate(1, 0, 0).UTC().Format(http.TimeFormat), 24 * time.Hour, 24 * time.Hour, true},
	} {
		retryAfter = tc.header
		err := c.Enrich(context.Background(), 1, "A/One", "B000000001")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
			t.Fatalf("Retry-After %q: err = %v", tc.header, err)
		}
		if apiErr.RetryAfter < tc.min || apiErr.RetryAfter > tc.max {
			t.Errorf("Retry-After %q: wait %v, want %v..%v", tc.header, apiErr.RetryAfter, tc.min, tc.max)
		}
		if apiErr.RetryAfterSet != tc.provided {
			t.Errorf("Retry-After %q: provided = %v, want %v", tc.header, apiErr.RetryAfterSet, tc.provided)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A DELETE's HTTP status settles the removal even when a proxy cuts off
// its body or sends an error code normally belonging to another route.
func TestDeleteAudibleAccountStatus(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusCreated, http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusBadGateway} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "200")
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"code":"no_book"}`))
			}))
			defer ts.Close()
			cl, err := New(ts.URL, "token", "test")
			if err != nil {
				t.Fatal(err)
			}
			err = cl.DeleteAudibleAccount(t.Context(), "abcd1234")
			var ae *APIError
			switch code {
			case http.StatusUnauthorized:
				if !errors.Is(err, ErrUnauthorized) {
					t.Fatalf("got %v", err)
				}
			case http.StatusForbidden, http.StatusBadGateway:
				if !errors.As(err, &ae) || ae.Status != code {
					t.Fatalf("got %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
