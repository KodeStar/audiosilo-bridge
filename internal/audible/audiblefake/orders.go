//go:build audiblefake

package audiblefake

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

// The orders listing and the catalog, shaped as a real UK account answered
// them (2026-11): GET /1.0/orders ignores every query parameter, answers a
// 500 to any response_groups, sends about six months of orders on one page
// with the paging fields at 0, and an item has asin and is_preorder beside
// payment fields (synthetic here). A pending preorder is preorder true in
// ORDER_PENDING_PREORDER / ORDER_CREATED; a delivered one, or any purchase,
// ORDER_CLOSED / ORDER_SUCCEEDED. The catalog answers any title (owned or
// not), its title only with product_desc asked.

// CancelledPreorder is a preorder the account placed and cancelled: its order
// is listed (ORDER_CANCELLED: the real state of a cancelled order was not
// seen; any state but pending reads the same) and the catalog knows the
// title, but it is neither in the library nor pending.
var CancelledPreorder = Title{ASIN: "B0FAKE0031", Title: "The Unwritten Shore", Authors: []string{"Mara Quill"},
	Series: "Orchard Cycle", Sequence: "6", ReleaseIn: 200}

// OrdersWindow is how far back the orders listing reaches (about six months,
// as Audible's did): an order older than this isn't listed.
const OrdersWindow = 183 * 24 * time.Hour

// preorderPlacedDaysAgo is when the fixture preorders were ordered.
const preorderPlacedDaysAgo = 40

// orders is GET /1.0/orders: one order per fixture title placed within
// OrdersWindow (a title bought at its release, a preorder preorderPlacedDaysAgo
// ago, pending until Release closes it), plus the cancelled preorder, newest
// first.
func (f *Fake) orders(w http.ResponseWriter, r *http.Request, _ []byte) {
	if r.URL.Query().Has("response_groups") {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error_code": nil,
			"message": "Invalid response group(s) requested: " + r.URL.Query().Get("response_groups")})
		return
	}
	now := f.opts.Now().UTC()
	f.mu.Lock()
	released := make(map[string]bool, len(f.released))
	for a := range f.released {
		released[a] = true
	}
	f.mu.Unlock()
	type order struct {
		date time.Time
		body map[string]any
	}
	var out []order
	add := func(t Title, date time.Time, preorder bool, state, detail string) {
		if now.Sub(date) > OrdersWindow {
			return
		}
		out = append(out, order{date, map[string]any{
			"date": date.Format(time.RFC3339), "preorder": preorder, "state": state, "detail_state": detail,
			"type": "CONTENT", "order_type_specification": "Content",
			"items": []map[string]any{{
				"asin": t.ASIN, "is_preorder": preorder,
				// Synthetic payment fields, as Audible sends beside the ASIN:
				// the Bridge must never read or log them.
				"amount_charged": "FAKE-AMOUNT-0.00", "payment_method": "FAKE-PAYMENT-METHOD", "tax_amount": "FAKE-TAX-0.00",
				"credits_applied": 1, "merchant_id": "FAKE-MERCHANT",
			}},
		}})
	}
	for _, t := range Fixtures {
		switch {
		case t.ReleaseIn > 0 && released[t.ASIN]:
			add(t, now.AddDate(0, 0, -preorderPlacedDaysAgo), true, "ORDER_CLOSED", "ORDER_SUCCEEDED")
		case t.ReleaseIn > 0:
			add(t, now.AddDate(0, 0, -preorderPlacedDaysAgo), true, "ORDER_PENDING_PREORDER", "ORDER_CREATED")
		default:
			add(t, now.AddDate(0, 0, t.ReleaseIn), false, "ORDER_CLOSED", "ORDER_SUCCEEDED")
		}
	}
	add(CancelledPreorder, now.AddDate(0, 0, -preorderPlacedDaysAgo-5), true, "ORDER_CANCELLED", "ORDER_CANCELLED")
	slices.SortStableFunc(out, func(a, b order) int { return b.date.Compare(a.date) })
	orders := make([]map[string]any, len(out))
	for i, o := range out {
		orders[i] = o.body
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders, "number_of_orders_in_range": len(orders),
		"page_number": 0, "max_number_of_pages": 0, "response_groups": []string{"always-returned"}})
}

// catalogTitle is one title the catalog knows: a fixture as it stands, or the
// cancelled preorder.
func (f *Fake) catalogTitle(asin string) (Title, bool) {
	for _, t := range append(f.catalog(), CancelledPreorder) {
		if strings.EqualFold(t.ASIN, asin) {
			return t, true
		}
	}
	return Title{}, false
}

// catalogProduct is a title as the catalog sends it: a library item's fields
// without the customer's rights, its title null unless product_desc is asked.
func catalogProduct(t Title, now time.Time, groups string) map[string]any {
	p := libraryItem(t, now)
	delete(p, "customer_rights")
	p["issue_date"] = p["release_date"]
	if !slices.Contains(strings.Split(groups, ","), "product_desc") {
		p["title"], p["subtitle"] = nil, nil
	}
	return p
}

// products is GET /1.0/catalog/products?asins=a,b: the titles the catalog
// knows, in the order asked, an unknown one left out; 400 over
// LastPositionsMax ASINs (Audible's list-length check).
func (f *Fake) products(w http.ResponseWriter, r *http.Request, _ []byte) {
	q := r.URL.Query()
	var asins []string
	for a := range strings.SplitSeq(q.Get("asins"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			asins = append(asins, a)
		}
	}
	if len(asins) > f.opts.LastPositionsMax {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "1 validation error detected: Value at 'asins' failed to satisfy constraint"})
		return
	}
	now := f.opts.Now()
	out := []map[string]any{}
	for _, a := range asins {
		if t, ok := f.catalogTitle(a); ok {
			out = append(out, catalogProduct(t, now, q.Get("response_groups")))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": out, "total_results": len(out),
		"response_groups": strings.Split(q.Get("response_groups"), ",")})
}

// product is GET /1.0/catalog/products/{asin}: one title, 404 when the catalog
// doesn't know it.
func (f *Fake) product(w http.ResponseWriter, r *http.Request, _ []byte) {
	t, ok := f.catalogTitle(r.PathValue("asin"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error_code": "000404", "message": "Requested product not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"product": catalogProduct(t, f.opts.Now(), r.URL.Query().Get("response_groups")),
		"response_groups": strings.Split(r.URL.Query().Get("response_groups"), ",")})
}
