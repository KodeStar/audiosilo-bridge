package audible

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// ordersBody is GET /1.0/orders as a real account answered it (2026-11),
// synthetic values: a pending preorder, a delivered one, a cancelled one and an
// ordinary purchase, each item with the payment fields Audible sends beside
// its ASIN.
const ordersBody = `{"orders":[
 {"date":"2026-10-19T19:49:51Z","preorder":true,"state":"ORDER_PENDING_PREORDER","detail_state":"ORDER_CREATED","type":"CONTENT",
  "items":[{"asin":"b0pending1","is_preorder":true,"amount_charged":"PAY-SECRET","payment_method":"CARD-SECRET","tax_amount":"1.23"}]},
 {"date":"2026-10-07T15:59:59Z","preorder":true,"state":"ORDER_CLOSED","detail_state":"ORDER_SUCCEEDED","type":"CONTENT",
  "items":[{"asin":"B0DELIVER1","is_preorder":true,"amount_charged":"PAY-SECRET"}]},
 {"date":"2026-08-01T10:00:00Z","preorder":true,"state":"ORDER_CANCELLED","detail_state":"ORDER_CANCELLED","type":"CONTENT",
  "items":[{"asin":"B0CANCEL01","is_preorder":true}]},
 {"date":"2026-05-07T22:44:48Z","preorder":false,"state":"ORDER_CLOSED","detail_state":"ORDER_SUCCEEDED","type":"CONTENT",
  "items":[{"asin":"B0BOUGHT01","is_preorder":false,"billing_address":"ADDR-SECRET"}]}
],"number_of_orders_in_range":4,"page_number":0,"max_number_of_pages":0,"response_groups":["always-returned"]}`

// TestOrders: the listing parses into orders (dates as UTC, ASINs upper
// case), only a preorder in ORDER_PENDING_PREORDER is pending, one request is
// made when the paging fields read 0, and no payment field is kept.
func TestOrders(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		_, _ = io.WriteString(w, ordersBody)
	}))
	defer ts.Close()
	orders, err := testClient(t, ts.URL).Orders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "/1.0/orders" {
		t.Fatalf("requests = %v, want one plain /1.0/orders (Audible 500s a response_groups)", paths)
	}
	if len(orders) != 4 {
		t.Fatalf("orders = %+v", orders)
	}
	var pending []string
	for _, o := range orders {
		if o.PendingPreorder() {
			for _, it := range o.Items {
				pending = append(pending, it.ASIN)
			}
		}
	}
	if !slices.Equal(pending, []string{"B0PENDING1"}) {
		t.Fatalf("pending = %v, want only B0PENDING1 (delivered, cancelled and bought ones aren't)", pending)
	}
	// Delivered: closed and succeeded (the delivered preorder and the
	// purchase), never the pending or the cancelled one.
	var delivered []bool
	for _, o := range orders {
		delivered = append(delivered, o.Delivered())
	}
	if !slices.Equal(delivered, []bool{false, true, false, true}) || orders[1].DetailState != OrderSucceeded {
		t.Fatalf("delivered = %v (%+v)", delivered, orders[1])
	}
	if want := time.Date(2026, 10, 19, 19, 49, 51, 0, time.UTC); !orders[0].Date.Equal(want) {
		t.Fatalf("date = %v", orders[0].Date)
	}
	raw, _ := json.Marshal(orders)
	for _, secret := range []string{"PAY-SECRET", "CARD-SECRET", "ADDR-SECRET", "1.23"} {
		if strings.Contains(fmt.Sprintf("%+v %s", orders, raw), secret) {
			t.Fatalf("a payment field (%s) was kept", secret)
		}
	}
}

// TestOrdersPages: a listing that says it has more pages is followed, bounded,
// and a page that adds nothing new (a replayed one) stops the pager.
func TestOrdersPages(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		page := r.URL.Query().Get("page")
		date := "2026-10-01T00:00:00Z"
		if page == "2" {
			date = "2026-09-01T00:00:00Z"
		}
		// Page 3 replays page 2: the pager must stop there, not run to its cap.
		if page == "3" {
			date = "2026-09-01T00:00:00Z"
		}
		_, _ = fmt.Fprintf(w, `{"orders":[{"date":%q,"preorder":true,"state":"ORDER_PENDING_PREORDER","items":[{"asin":"B0PAGE0001"}]}],"max_number_of_pages":50}`, date)
	}))
	defer ts.Close()
	orders, err := testClient(t, ts.URL).Orders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 || calls != 3 {
		t.Fatalf("orders = %d over %d calls, want 2 over 3", len(orders), calls)
	}
}

// TestOrdersError: a failing listing is an error, never an empty list (the
// caller must not read "no orders" into it).
func TestOrdersError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"Invalid response group(s) requested"}`)
	}))
	defer ts.Close()
	if _, err := testClient(t, ts.URL).Orders(context.Background()); HTTPStatus(err) != http.StatusInternalServerError {
		t.Fatalf("err = %v", err)
	}
}

// catalogServer answers the catalog: a batch over limit ASINs is a 400, an
// ASIN in skip is left out of a batched answer (read alone instead), and one
// in unknown is a 404 alone. Titles come only with product_desc.
func catalogServer(t *testing.T, limit int, skip, unknown map[string]bool, sizes *[]int, singles *[]string) *httptest.Server {
	t.Helper()
	release := time.Now().AddDate(0, 0, 60).Format(time.DateOnly)
	product := func(asin string, groups string) map[string]any {
		p := map[string]any{"asin": asin, "release_date": release, "issue_date": release,
			"content_delivery_type": "SinglePartBook", "runtime_length_min": 800,
			"authors": []map[string]string{{"name": "Actus"}}, "narrators": []map[string]string{{"name": "Some Narrator"}},
			"series": []map[string]string{{"title": "Return of the Runebound Professor", "sequence": "9"}}}
		if strings.Contains(groups, "product_desc") {
			p["title"] = "Title " + asin
		} else {
			p["title"] = nil
		}
		return p
	}
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		groups := r.URL.Query().Get("response_groups")
		if asin, ok := strings.CutPrefix(r.URL.Path, "/1.0/catalog/products/"); ok {
			*singles = append(*singles, asin)
			if unknown[asin] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Requested product not found"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"product": product(asin, groups)})
			return
		}
		if r.URL.Path != "/1.0/catalog/products" {
			http.NotFound(w, r)
			return
		}
		asins := strings.Split(r.URL.Query().Get("asins"), ",")
		*sizes = append(*sizes, len(asins))
		if len(asins) > limit {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"1 validation error detected"}`)
			return
		}
		out := []map[string]any{}
		for _, a := range asins {
			if !skip[a] && !unknown[a] {
				out = append(out, product(a, groups))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"products": out})
	}))
}

// TestProducts: the catalog read in batches of catalogBatch, each split on a
// 400 until Audible takes it, records made as a library listing's (title from
// product_desc, series, runtime, release date, a future date a pre-order), an
// ASIN the batch left out read alone, one the catalog doesn't know left out.
func TestProducts(t *testing.T) {
	asins := make([]string, 60)
	for i := range asins {
		asins[i] = fmt.Sprintf("B0CAT%05d", i)
	}
	var sizes []int
	var singles []string
	ts := catalogServer(t, 10, map[string]bool{"B0CAT00007": true}, map[string]bool{"B0CAT00042": true}, &sizes, &singles)
	defer ts.Close()
	recs, err := testClient(t, ts.URL).Products(context.Background(), asins)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 59 {
		t.Fatalf("records = %d, want 59 (one unknown)", len(recs))
	}
	for _, n := range sizes {
		if n > catalogBatch {
			t.Fatalf("a batch of %d ASINs", n)
		}
	}
	if !slices.Contains(singles, "B0CAT00007") || !slices.Contains(singles, "B0CAT00042") || len(singles) != 2 {
		t.Fatalf("read alone = %v, want the two the batches didn't answer", singles)
	}
	r := recs[0]
	if r.ASIN != "B0CAT00000" || r.Title != "Title B0CAT00000" || r.Series != "Return of the Runebound Professor" ||
		r.SeriesPosition != "9" || r.ReleaseDate != time.Now().AddDate(0, 0, 60).Format(time.DateOnly) || r.RuntimeSec != 800*60 || r.Author != "Actus" ||
		!r.Preorder || r.UnavailableReason != PreorderReason {
		t.Fatalf("record = %+v", r)
	}
}

// TestProductsFallbacks: what the batched read doesn't answer with a title is
// read alone (a batch that failed, one still refused with a 400 down to one
// ASIN, a product sent without its title); a single read that fails ends the
// reads but keeps every record read, returned with its error; Audible asking
// the Bridge to wait ends the reads at once.
func TestProductsFallbacks(t *testing.T) {
	asins := []string{"B0CAT00001", "B0CAT00002", "B0CAT00003"}
	cases := []struct {
		name     string
		batch    int    // the batched read's status (0: it answers)
		untitled bool   // the batched read sends its products without their titles
		omit     string // an ASIN the batched read leaves out
		single   int    // every single read's status (0: it answers)
		want     []string
		reads    []string // the single reads made
		status   int      // the error's status (0: no error)
	}{
		{name: "a batch that failed is read alone", batch: http.StatusInternalServerError, want: asins, reads: asins},
		{name: "a 400 down to one ASIN is read alone", batch: http.StatusBadRequest, want: asins, reads: asins},
		{name: "a product without its title is read alone", untitled: true, want: asins, reads: asins},
		{name: "a failed single read keeps what was read", omit: "B0CAT00002", single: http.StatusInternalServerError,
			want: []string{"B0CAT00001", "B0CAT00003"}, reads: []string{"B0CAT00002"}, status: http.StatusInternalServerError},
		{name: "Audible asking to wait ends the reads", batch: http.StatusTooManyRequests, want: []string{}, status: http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var reads []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if asin, ok := strings.CutPrefix(r.URL.Path, "/1.0/catalog/products/"); ok {
					reads = append(reads, asin)
					if tc.single != 0 {
						w.WriteHeader(tc.single)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"product": map[string]any{"asin": asin, "title": "Title " + asin}})
					return
				}
				if tc.batch != 0 {
					w.WriteHeader(tc.batch)
					return
				}
				out := []map[string]any{}
				for _, a := range strings.Split(r.URL.Query().Get("asins"), ",") {
					if a == tc.omit {
						continue
					}
					p := map[string]any{"asin": a, "title": "Title " + a}
					if tc.untitled {
						p["title"] = nil
					}
					out = append(out, p)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"products": out})
			}))
			defer ts.Close()
			recs, err := testClient(t, ts.URL).Products(context.Background(), asins)
			if HTTPStatus(err) != tc.status || (tc.status == 0) != (err == nil) {
				t.Fatalf("err = %v, want status %d", err, tc.status)
			}
			got := make([]string, len(recs))
			for i, r := range recs {
				if r.Title != "Title "+r.ASIN {
					t.Fatalf("record = %+v", r)
				}
				got[i] = r.ASIN
			}
			if !slices.Equal(got, tc.want) || !slices.Equal(reads, tc.reads) {
				t.Fatalf("records %v (want %v), read alone %v (want %v)", got, tc.want, reads, tc.reads)
			}
		})
	}
}

// TestProductsError: a catalog that fails (not a length refusal) is an error.
func TestProductsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()
	if _, err := testClient(t, ts.URL).Products(context.Background(), []string{"B0CAT00001"}); err == nil {
		t.Fatal("a failing catalog read answered no error")
	}
}
