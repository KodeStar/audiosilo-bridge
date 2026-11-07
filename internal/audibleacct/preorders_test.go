package audibleacct

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/audible"
	"github.com/kodestar/audiosilo-bridge/internal/secrets"
	"github.com/kodestar/audiosilo-bridge/internal/source"
	"github.com/kodestar/audiosilo-bridge/internal/store"
)

var pnow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func pendingOrder(date, asin string) audible.Order {
	return audible.Order{Date: day(date), Preorder: true, State: audible.OrderPendingPreorder,
		Items: []audible.OrderItem{{ASIN: asin}}}
}

func closedOrder(date, asin string, preorder bool) audible.Order {
	return audible.Order{Date: day(date), Preorder: preorder, State: "ORDER_CLOSED", Items: []audible.OrderItem{{ASIN: asin}}}
}

func asinsOf(rows []store.AudiblePreorder) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ASIN
	}
	return out
}

// TestReconcilePreorders: the drop rules, one by one.
func TestReconcilePreorders(t *testing.T) {
	remembered := func(asin, ordered string) store.AudiblePreorder {
		return store.AudiblePreorder{ASIN: asin, OrderedAt: day(ordered), LastSeenAt: day("2026-11-01")}
	}
	cases := []struct {
		name    string
		rows    []store.AudiblePreorder
		library []string
		orders  []audible.Order
		want    []string
	}{
		{name: "a pending order adds its title",
			orders: []audible.Order{pendingOrder("2026-10-19", "B0HN716NQ7"), closedOrder("2026-05-07", "B0OLDBUY01", false)},
			want:   []string{"B0HN716NQ7"}},
		{name: "a lower-case ASIN is the same title",
			orders: []audible.Order{pendingOrder("2026-10-19", "b0hn716nq7")}, want: []string{"B0HN716NQ7"}},
		{name: "a delivered or ordinary order adds nothing",
			orders: []audible.Order{closedOrder("2026-10-07", "B0DELIVER1", true), closedOrder("2026-10-01", "B0BOUGHT01", false)},
			want:   []string{}},
		{name: "the library wins over a pending order",
			library: []string{"B0HN716NQ7"}, orders: []audible.Order{pendingOrder("2026-10-19", "B0HN716NQ7")},
			want: []string{}},
		{name: "the library listing a remembered one drops it, orders or not",
			rows: []store.AudiblePreorder{remembered("B0GV5H3V89", "2026-03-30")}, library: []string{"B0GV5H3V89"},
			want: []string{}},
		{name: "its order seen closed drops it",
			rows:   []store.AudiblePreorder{remembered("B0HG4PZMYQ", "2026-08-28")},
			orders: []audible.Order{closedOrder("2026-08-28", "B0HG4PZMYQ", true), closedOrder("2026-05-07", "B0OLDBUY01", false)},
			want:   []string{}},
		{name: "its order seen cancelled drops it",
			rows:   []store.AudiblePreorder{remembered("B0HG4PZMYQ", "2026-08-28")},
			orders: []audible.Order{{Date: day("2026-08-28"), Preorder: true, State: "ORDER_CANCELLED", Items: []audible.OrderItem{{ASIN: "B0HG4PZMYQ"}}}},
			want:   []string{}},
		{name: "a pending order wins over another order of the title",
			orders: []audible.Order{closedOrder("2026-06-01", "B0HG4PZMYQ", true), pendingOrder("2026-08-28", "B0HG4PZMYQ")},
			want:   []string{"B0HG4PZMYQ"}},
		{name: "missing from a listing that reaches past its order: cancelled",
			rows:   []store.AudiblePreorder{remembered("B0HG4PZMYQ", "2026-08-28")},
			orders: []audible.Order{closedOrder("2026-05-07", "B0OLDBUY01", false)}, want: []string{}},
		{name: "missing from a listing whose window starts after its order: kept",
			rows:   []store.AudiblePreorder{remembered("B0GV5H3V89", "2026-03-30")},
			orders: []audible.Order{closedOrder("2026-05-07", "B0OLDBUY01", false)}, want: []string{"B0GV5H3V89"}},
		{name: "an empty or failed listing (no orders) drops nothing",
			rows: []store.AudiblePreorder{remembered("B0HG4PZMYQ", "2026-08-28")}, want: []string{"B0HG4PZMYQ"}},
		{name: "an order with an ASIN that isn't one adds nothing",
			orders: []audible.Order{pendingOrder("2026-10-19", "not-an-asin")}, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lib := map[string]bool{}
			for _, a := range tc.library {
				lib[a] = true
			}
			got := asinsOf(reconcilePreorders(tc.rows, lib, tc.orders, pnow))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("kept %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReconcilePreordersTimes: a new one is ordered at its order's date (now
// for none) and seen now; a remembered one keeps its order date.
func TestReconcilePreordersTimes(t *testing.T) {
	rows := []store.AudiblePreorder{{ASIN: "B0HG4PZMYQ", OrderedAt: day("2026-08-28"), LastSeenAt: day("2026-11-01"), Record: "{}"}}
	undated := audible.Order{Preorder: true, State: audible.OrderPendingPreorder, Items: []audible.OrderItem{{ASIN: "B0UNDATED1"}}}
	got := reconcilePreorders(rows, nil, []audible.Order{pendingOrder("2026-10-19", "B0HN716NQ7"), pendingOrder("2026-08-28", "B0HG4PZMYQ"), undated}, pnow)
	if len(got) != 3 {
		t.Fatalf("rows = %+v", got)
	}
	by := map[string]store.AudiblePreorder{}
	for _, r := range got {
		by[r.ASIN] = r
	}
	if r := by["B0HN716NQ7"]; !r.OrderedAt.Equal(day("2026-10-19")) || !r.LastSeenAt.Equal(pnow) {
		t.Fatalf("new = %+v", r)
	}
	if r := by["B0HG4PZMYQ"]; !r.OrderedAt.Equal(day("2026-08-28")) || !r.LastSeenAt.Equal(pnow) || r.Record != "{}" {
		t.Fatalf("remembered = %+v", r)
	}
	if r := by["B0UNDATED1"]; !r.OrderedAt.Equal(pnow) {
		t.Fatalf("undated = %+v", r)
	}
}

// TestDropReleased: a preorder released PreorderDropAfter ago that the library
// still doesn't list goes; one released less long ago and one ahead stay. One
// the catalog never dated counts as released undatedPreorderYears after its
// order was last seen pending, and stays while that is unknown.
func TestDropReleased(t *testing.T) {
	rec := func(asin, release string) store.AudiblePreorder {
		return store.AudiblePreorder{ASIN: asin, Record: `{"ReleaseDate":"` + release + `"}`}
	}
	undated := func(asin string, seen time.Time) store.AudiblePreorder {
		return store.AudiblePreorder{ASIN: asin, LastSeenAt: seen, Record: `{"Title":"Undated"}`}
	}
	rows := []store.AudiblePreorder{
		rec("B0OLD00001", pnow.Add(-PreorderDropAfter).Format(time.DateOnly)),
		rec("B0RECENT01", pnow.AddDate(0, 0, -29).Format(time.DateOnly)),
		rec("B0AHEAD001", "2026-12-08"),
		{ASIN: "B0UNFILLED"},
		undated("B0UNDATED1", pnow.AddDate(-undatedPreorderYears, 0, 0).Add(-PreorderDropAfter)),
		undated("B0UNDATED2", pnow.AddDate(-undatedPreorderYears, 0, 0)),
	}
	if got := asinsOf(dropReleased(rows, pnow)); !slices.Equal(got, []string{"B0RECENT01", "B0AHEAD001", "B0UNFILLED", "B0UNDATED2"}) {
		t.Fatalf("kept %v", got)
	}
}

// TestPreordersRemembered: a poll reports the pending preorders with the
// catalog's description and remembers them; a later poll whose orders no
// longer reach the order still reports it; the library listing it drops it;
// removing the account forgets them.
func TestPreordersRemembered(t *testing.T) {
	e := newEnv(t)
	a := e.signIn(t)
	ctx := context.Background()
	e.m.o.Now = func() time.Time { return pnow }
	e.svc.orders = []audible.Order{pendingOrder("2026-10-19", "B0HN716NQ7"), pendingOrder("2026-05-08", "B0H1VSQCJM"),
		closedOrder("2026-05-07", "B0OLDBUY01", false)}
	e.svc.products = map[string]source.Record{
		"B0HN716NQ7": {ASIN: "B0HN716NQ7", Title: "Beware of Chicken 6", Author: "Casualfarmer", Series: "Beware of Chicken", SeriesPosition: "6", ReleaseDate: "2026-12-08", FileName: "Beware of Chicken 6.m4b"},
		"B0H1VSQCJM": {ASIN: "B0H1VSQCJM", Title: "Unique", ReleaseDate: "2026-11-10"},
	}
	recs := e.m.Preorders(ctx, a.ID, []source.Record{{ASIN: "B0FAKE0001"}})
	if len(recs) != 2 {
		t.Fatalf("preorders = %+v", recs)
	}
	r := recs[1]
	if r.ASIN != "B0HN716NQ7" || r.Title != "Beware of Chicken 6" || r.Series != "Beware of Chicken" || r.SeriesPosition != "6" ||
		r.ReleaseDate != "2026-12-08" || !r.Preorder || r.UnavailableReason != audible.PreorderReason || r.FileName != "Beware of Chicken 6.m4b" {
		t.Fatalf("record = %+v", r)
	}

	// The window moves on: Unique's order is no longer listed, and the oldest
	// order listed is after it. It is still reported, from the store.
	e.svc.orders = []audible.Order{pendingOrder("2026-10-19", "B0HN716NQ7"), closedOrder("2026-06-01", "B0OLDBUY02", false)}
	if got := asinsOfRecs(e.m.Preorders(ctx, a.ID, nil)); !slices.Equal(got, []string{"B0H1VSQCJM", "B0HN716NQ7"}) {
		t.Fatalf("after the window moved = %v", got)
	}

	// Released: the library lists Unique, so it is no longer a preorder here.
	if got := asinsOfRecs(e.m.Preorders(ctx, a.ID, []source.Record{{ASIN: "B0H1VSQCJM"}})); !slices.Equal(got, []string{"B0HN716NQ7"}) {
		t.Fatalf("after the release = %v", got)
	}
	rows, _ := e.db.AudiblePreorders(ctx, a.ID)
	if !slices.Equal(asinsOf(rows), []string{"B0HN716NQ7"}) {
		t.Fatalf("remembered = %v", asinsOf(rows))
	}

	// A remembered preorder read as a title (an import's first step) is its
	// preorder record, not "not in the library"; another title still is.
	e.svc.libraryErr = audible.ErrNotInLibrary
	if r, err := e.m.Title(ctx, a.ID, "B0HN716NQ7"); err != nil || !r.Preorder || r.Title != "Beware of Chicken 6" {
		t.Fatalf("a remembered preorder's title = %+v, %v", r, err)
	}
	if _, err := e.m.Title(ctx, a.ID, "B0NOTHERE1"); !errors.Is(err, audible.ErrNotInLibrary) {
		t.Fatalf("another title = %v", err)
	}
	e.svc.libraryErr = nil

	if _, err := e.m.Remove(ctx, e.srv, a.ID); err != nil {
		t.Fatal(err)
	}
	if rows, _ := e.db.AudiblePreorders(ctx, a.ID); len(rows) != 0 {
		t.Fatalf("a removed account's preorders = %+v", rows)
	}
}

func asinsOfRecs(recs []source.Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ASIN
	}
	return out
}

// TestPreordersSoftFailure: the orders failing reports the remembered
// preorders; the catalog failing reports the last description (and one never
// described by its ASIN alone); each failure logs one warning, without a
// credential, and nothing remembered is lost.
func TestPreordersSoftFailure(t *testing.T) {
	e := newEnv(t)
	a := e.signIn(t)
	ctx := context.Background()
	e.m.o.Now = func() time.Time { return pnow }
	e.svc.orders = []audible.Order{pendingOrder("2026-10-19", "B0HN716NQ7"), pendingOrder("2026-08-28", "B0HG4PZMYQ")}
	e.svc.products = map[string]source.Record{"B0HN716NQ7": {ASIN: "B0HN716NQ7", Title: "Beware of Chicken 6", ReleaseDate: "2026-12-08"}}
	if got := e.m.Preorders(ctx, a.ID, nil); len(got) != 2 || got[0].Title != "" || got[1].Title != "Beware of Chicken 6" {
		t.Fatalf("first poll = %+v", got)
	}

	// The orders fail (Audible's answer quoting a secret the redaction must
	// keep out of the log is the Service's job; this one carries none), and
	// the catalog answers nothing.
	e.svc.ordersErr = errors.New("GET /1.0/orders: HTTP 500: boom")
	e.svc.products, e.svc.productsErr = nil, errors.New("GET /1.0/catalog/products: HTTP 502: down")
	e.logs.Reset()
	got := e.m.Preorders(ctx, a.ID, nil)
	if len(got) != 2 {
		t.Fatalf("with both failing = %+v", got)
	}
	if got[1].Title != "Beware of Chicken 6" || got[1].ReleaseDate != "2026-12-08" || !got[1].Preorder {
		t.Fatalf("the last description = %+v", got[1])
	}
	if got[0].ASIN != "B0HG4PZMYQ" || got[0].Title != "" || !got[0].Preorder || got[0].UnavailableReason != audible.PreorderReason {
		t.Fatalf("one never described = %+v", got[0])
	}
	logs := e.logs.String()
	if strings.Count(logs, "listing the orders failed") != 1 || strings.Count(logs, "from the catalog failed") != 1 {
		t.Fatalf("warnings = %s", logs)
	}
	if rows, _ := e.db.AudiblePreorders(ctx, a.ID); len(rows) != 2 || rows[1].Record == "" {
		t.Fatalf("remembered after failures = %+v", rows)
	}

	// The catalog fails after describing one title: its new description is
	// used, the other keeps what it had.
	e.svc.ordersErr = nil
	e.svc.products = map[string]source.Record{"B0HN716NQ7": {ASIN: "B0HN716NQ7", Title: "Beware of Chicken 6", ReleaseDate: "2026-12-15"}}
	if got := e.m.Preorders(ctx, a.ID, nil); len(got) != 2 || got[1].ReleaseDate != "2026-12-15" || got[0].Title != "" {
		t.Fatalf("with the catalog failing part way = %+v", got)
	}
	e.noSecrets(t)
}

// TestPreordersWaitSparesTheCatalog: when Audible asks the Bridge to wait on
// the orders, the catalog isn't asked (it would only be asked to wait too);
// the remembered preorders are reported with their last description.
func TestPreordersWaitSparesTheCatalog(t *testing.T) {
	e := newEnv(t)
	a := e.signIn(t)
	ctx := context.Background()
	e.m.o.Now = func() time.Time { return pnow }
	e.svc.orders = []audible.Order{pendingOrder("2026-10-19", "B0HN716NQ7")}
	e.svc.products = map[string]source.Record{"B0HN716NQ7": {ASIN: "B0HN716NQ7", Title: "Beware of Chicken 6", ReleaseDate: "2026-12-08"}}
	if got := e.m.Preorders(ctx, a.ID, nil); len(got) != 1 || len(e.svc.asked) != 1 {
		t.Fatalf("first poll = %+v, catalog reads %v", got, e.svc.asked)
	}
	e.svc.ordersErr = waitAnswer(t)
	got := e.m.Preorders(ctx, a.ID, nil)
	if len(got) != 1 || got[0].Title != "Beware of Chicken 6" || len(e.svc.asked) != 1 {
		t.Fatalf("after a wait = %+v, catalog reads %v", got, e.svc.asked)
	}
}

// waitAnswer is Audible asking the Bridge to wait (a 429 whose Retry-After is
// past the backoff's cap), as the real Service answers it.
func waitAnswer(t *testing.T) error {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	_, err = Audible(audible.WithEndpoints(audible.Endpoints{API: ts.URL})).Orders(context.Background(),
		audible.Credentials{ADPToken: "ADP", DevicePrivateKey: keyPEM, DeviceType: "A2CZJZGLK2JJVM", DeviceSerial: "SER", Marketplace: "us"})
	if _, ok := audible.RetryWait(err); !ok {
		t.Fatalf("err = %v, want Audible asking to wait", err)
	}
	return err
}

// preorderStore counts the writes of the remembered preorders, and fails
// their reads when failRead is set.
type preorderStore struct {
	Store
	failRead bool
	writes   int
}

func (s *preorderStore) AudiblePreorders(ctx context.Context, account string) ([]store.AudiblePreorder, error) {
	if s.failRead {
		return nil, errors.New("database disk image is malformed")
	}
	return s.Store.AudiblePreorders(ctx, account)
}

func (s *preorderStore) SetAudiblePreorders(ctx context.Context, account string, ps []store.AudiblePreorder) error {
	s.writes++
	return s.Store.SetAudiblePreorders(ctx, account, ps)
}

// TestPreordersKeepWhatIsRemembered (denied): when the remembered preorders
// can't be read, the poll reports the orders' pending ones and writes nothing
// back (a write would forget the rest); when the sign-in can't be opened, it
// reports the remembered ones, asking Audible nothing and writing nothing.
func TestPreordersKeepWhatIsRemembered(t *testing.T) {
	e := newEnv(t)
	a := e.signIn(t)
	ctx := context.Background()
	e.m.o.Now = func() time.Time { return pnow }
	e.svc.orders = []audible.Order{pendingOrder("2026-10-19", "B0HN716NQ7"), pendingOrder("2026-05-08", "B0H1VSQCJM"),
		closedOrder("2026-05-07", "B0OLDBUY01", false)}
	if got := asinsOfRecs(e.m.Preorders(ctx, a.ID, nil)); !slices.Equal(got, []string{"B0H1VSQCJM", "B0HN716NQ7"}) {
		t.Fatalf("first poll = %v", got)
	}
	// The window moves on past Unique's order: only the store still knows it.
	e.svc.orders = []audible.Order{pendingOrder("2026-10-19", "B0HN716NQ7"), closedOrder("2026-06-01", "B0OLDBUY02", false)}

	broken := &preorderStore{Store: e.db, failRead: true}
	e.m.o.DB = broken
	e.logs.Reset()
	if got := asinsOfRecs(e.m.Preorders(ctx, a.ID, nil)); !slices.Equal(got, []string{"B0HN716NQ7"}) || broken.writes != 0 {
		t.Fatalf("with the store unreadable = %v, %d writes", got, broken.writes)
	}
	if !strings.Contains(e.logs.String(), "reading the remembered preorders failed") {
		t.Fatalf("logs = %s", e.logs.String())
	}
	if rows, _ := e.db.AudiblePreorders(ctx, a.ID); !slices.Equal(asinsOf(rows), []string{"B0H1VSQCJM", "B0HN716NQ7"}) {
		t.Fatalf("remembered after an unreadable store = %v", asinsOf(rows))
	}

	if err := e.vault.Delete(secrets.AudibleName(a.ID)); err != nil {
		t.Fatal(err)
	}
	counting := &preorderStore{Store: e.db}
	e.m.o.DB = counting
	asked := len(e.svc.asked)
	got := e.m.Preorders(ctx, a.ID, nil)
	if !slices.Equal(asinsOfRecs(got), []string{"B0H1VSQCJM", "B0HN716NQ7"}) || len(e.svc.asked) != asked || counting.writes != 0 {
		t.Fatalf("without a sign-in = %v, catalog reads %v, %d writes", asinsOfRecs(got), e.svc.asked[asked:], counting.writes)
	}
}
