package audible

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLastPositions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/1.0/annotations/lastpositions") {
			http.NotFound(w, r)
			return
		}
		// position_ms as a string for one and a number for another (Audible varies).
		_, _ = io.WriteString(w, `{"asin_last_position_heard_annots":[
			{"asin":"B01","last_position_heard":{"status":"Exists","position_ms":"123456","last_updated":"2022-03-10 12:10:13.093"}},
			{"asin":"B02","last_position_heard":{"status":"Exists","position_ms":7890}},
			{"asin":"B03","last_position_heard":{"status":"DoesNotExist","position_ms":"0"}}
		]}`)
	}))
	defer ts.Close()

	c := testClient(t, ts.URL)
	got, err := c.LastPositions(context.Background(), []string{"B01", "B02", "B03"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["B01"].PositionMs != 123456 || got["B02"].PositionMs != 7890 {
		t.Fatalf("positions = %+v", got)
	}
	// last_updated is read as UTC; a missing one is zero, never a guess.
	if want := time.Date(2022, 3, 10, 12, 10, 13, 93e6, time.UTC); !got["B01"].LastUpdated.Equal(want) {
		t.Fatalf("B01 last_updated = %v, want %v", got["B01"].LastUpdated, want)
	}
	if !got["B02"].LastUpdated.IsZero() {
		t.Fatalf("B02 last_updated = %v, want zero", got["B02"].LastUpdated)
	}
	if _, ok := got["B03"]; ok {
		t.Fatal("non-existent position should be omitted")
	}
}

// TestLastPositionsChunks proves a large library is split across several requests
// (so the query never overflows into an HTTP 414) and the results merge.
func TestLastPositionsChunks(t *testing.T) {
	const n = 250 // > 2 * lastPositionsBatch, so several requests
	asins := make([]string, n)
	for i := range asins {
		asins[i] = fmt.Sprintf("B%09d", i)
	}

	var maxQueryLen int
	srv := lastPositionsServer(t, 0, &maxQueryLen) // 0 = no server-side cap
	defer srv.Close()

	got, err := testClient(t, srv.URL).LastPositions(context.Background(), asins, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("merged positions = %d, want %d", len(got), n)
	}
	// Each request must stay well under any reasonable URL limit (the first bug was
	// a single ~all-ASINs query returning HTTP 414, Request-URI Too Long).
	if maxQueryLen > 2048 {
		t.Fatalf("max query length = %d, want <= 2048", maxQueryLen)
	}
}

// A batch that fails ends the read, and the places of the batches read before
// it come back with the error (bridge#50), never thrown away.
func TestLastPositionsKeepsBatchesBeforeAFailure(t *testing.T) {
	asins := make([]string, 2*lastPositionsBatch+3)
	for i := range asins {
		asins[i] = fmt.Sprintf("B%09d", i)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			http.Error(w, `{"message":"gone"}`, http.StatusNotFound)
			return
		}
		var annots []string
		for a := range strings.SplitSeq(r.URL.Query().Get("asins"), ",") {
			annots = append(annots, fmt.Sprintf(`{"asin":%q,"last_position_heard":{"status":"Exists","position_ms":5000}}`, a))
		}
		_, _ = io.WriteString(w, `{"asin_last_position_heard_annots":[`+strings.Join(annots, ",")+`]}`)
	}))
	defer srv.Close()

	got, err := testClient(t, srv.URL).LastPositions(context.Background(), asins, nil)
	if err == nil || HTTPStatus(err) != http.StatusNotFound {
		t.Fatalf("err = %v, want the second batch's 404", err)
	}
	if len(got) != lastPositionsBatch || got[asins[0]].PositionMs != 5000 || calls != 2 {
		t.Fatalf("places = %d (calls %d), want the first batch's %d", len(got), calls, lastPositionsBatch)
	}
	if _, ok := got[asins[lastPositionsBatch]]; ok {
		t.Fatal("a place of the failed batch came back")
	}
}

// TestLastPositionsAdaptiveSplit proves the batch self-tunes below Audible's
// undocumented ASIN-list cap: when the endpoint 400s an over-long list, the client
// splits and retries, converging on the real limit and still returning every
// position (the observed bug: a 50-ASIN batch still returned HTTP 400).
func TestLastPositionsAdaptiveSplit(t *testing.T) {
	const n = 250
	asins := make([]string, n)
	for i := range asins {
		asins[i] = fmt.Sprintf("B%09d", i)
	}

	// Server rejects any request carrying more than 7 ASINs - below the starting
	// batch, so the splitter must recurse to converge.
	const maxAsins = 7
	srv := lastPositionsServer(t, maxAsins, nil)
	defer srv.Close()

	got, err := testClient(t, srv.URL).LastPositions(context.Background(), asins, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("merged positions = %d, want %d (adaptive split lost entries)", len(got), n)
	}
}

// A URL too long for Amazon's front end (414) splits the batch as a 400 does.
func TestLastPositionsSplitsOn414(t *testing.T) {
	asins := make([]string, 25)
	for i := range asins {
		asins[i] = fmt.Sprintf("B%09d", i)
	}
	var sizes []int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.Split(r.URL.Query().Get("asins"), ",")
		sizes = append(sizes, len(got))
		if len(got) > 6 {
			w.WriteHeader(http.StatusRequestURITooLong)
			return
		}
		var b strings.Builder
		b.WriteString(`{"asin_last_position_heard_annots":[`)
		for i, a := range got {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"asin":%q,"last_position_heard":{"status":"Exists","position_ms":1000}}`, a)
		}
		b.WriteString(`]}`)
		_, _ = io.WriteString(w, b.String())
	}))
	defer ts.Close()
	got, err := testClient(t, ts.URL).LastPositions(context.Background(), asins, nil)
	if err != nil || len(got) != len(asins) {
		t.Fatalf("got %d positions, err %v (sizes %v)", len(got), err, sizes)
	}
	if sizes[0] != 25 {
		t.Fatalf("the first request carried %d ASINs, want the batch of 25", sizes[0])
	}
}

// lastPositionsServer returns a fake annotations endpoint. If maxAsins > 0 it
// mimics Audible's HTTP 400 when a request carries more than maxAsins ASINs. If
// maxQueryLen is non-nil it records the longest query string seen.
func lastPositionsServer(t *testing.T, maxAsins int, maxQueryLen *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/1.0/annotations/lastpositions") {
			http.NotFound(w, r)
			return
		}
		if maxQueryLen != nil {
			if l := len(r.URL.RawQuery); l > *maxQueryLen {
				*maxQueryLen = l
			}
		}
		reqAsins := strings.Split(r.URL.Query().Get("asins"), ",")
		if maxAsins > 0 && len(reqAsins) > maxAsins {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"1 validation error detected: Value at 'asins' failed to satisfy constraint"}`)
			return
		}
		var b strings.Builder
		b.WriteString(`{"asin_last_position_heard_annots":[`)
		for i, a := range reqAsins {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"asin":%q,"last_position_heard":{"status":"Exists","position_ms":%d}}`, a, i+1)
		}
		b.WriteString(`]}`)
		_, _ = io.WriteString(w, b.String())
	}))
}

func TestSetLastPosition(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := testClient(t, ts.URL)
	if err := c.SetLastPosition(context.Background(), "B01", "ACR123", 555000); err != nil {
		t.Fatal(err)
	}
	// PUT /1.0/lastpositions/{asin} with {acr, asin, position_ms:<number>}.
	if gotPath != "/1.0/lastpositions/B01" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody["asin"] != "B01" || gotBody["acr"] != "ACR123" {
		t.Fatalf("body = %+v", gotBody)
	}
	if ms, ok := gotBody["position_ms"].(float64); !ok || ms != 555000 {
		t.Fatalf("position_ms should be a number 555000, got %#v", gotBody["position_ms"])
	}

	// acr is required.
	if err := c.SetLastPosition(context.Background(), "B01", "", 1); err == nil {
		t.Fatal("expected an error when acr is empty")
	}
}

func TestParseLastUpdated(t *testing.T) {
	want := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	for _, in := range []string{"2026-11-02 09:30:00", "2026-11-02T09:30:00", "2026-11-02T09:30:00Z", "2026-11-02T10:30:00+01:00"} {
		if got := parseLastUpdated(in); !got.Equal(want) {
			t.Errorf("parseLastUpdated(%q) = %v, want %v", in, got, want)
		}
	}
	for _, in := range []string{"", "yesterday", "2026-13-40"} {
		if got := parseLastUpdated(in); !got.IsZero() {
			t.Errorf("parseLastUpdated(%q) = %v, want zero", in, got)
		}
	}
}
