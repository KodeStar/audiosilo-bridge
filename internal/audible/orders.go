package audible

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/source"
)

// A pending preorder is not in the account's library (GET /1.0/library lists
// none, GET /1.0/library/{asin} is a 404) until it is released: the orders
// listing is the only place the API shows one. Seen on a real UK account
// (2026-11): GET /1.0/orders answers {orders, number_of_orders_in_range,
// page_number, max_number_of_pages, response_groups} with about six months of
// orders; every query parameter tried (page, num_results, start_date and
// end_date, purchased_after, date_range, period) was ignored, any
// response_groups but the default was a 500, and the paging fields read 0. An
// order has date, preorder, state, detail_state, type and items; an item has
// asin and is_preorder, beside payment fields this client never decodes.

// OrderPendingPreorder is the state of a preorder's order until it is
// delivered (ORDER_CLOSED with detail_state ORDER_SUCCEEDED) or cancelled.
const OrderPendingPreorder = "ORDER_PENDING_PREORDER"

// Order is one order of the account, as far as the Bridge reads it: never a
// payment, price or address field.
type Order struct {
	// Date is when it was placed (zero when Audible sent none that parses).
	Date     time.Time
	Preorder bool
	State    string
	Items    []OrderItem
}

// OrderItem is one title of an order.
type OrderItem struct {
	ASIN       string
	IsPreorder bool
}

// PendingPreorder reports whether o is a preorder still waiting for its
// release: a preorder (preorder true) whose state is OrderPendingPreorder.
func (o Order) PendingPreorder() bool {
	return o.Preorder && o.State == OrderPendingPreorder
}

type ordersResp struct {
	Orders   []orderWire `json:"orders"`
	MaxPages int         `json:"max_number_of_pages"`
}

// orderWire decodes only the fields an Order keeps: the payment fields an
// item carries are never read into memory as values.
type orderWire struct {
	Date     string `json:"date"`
	Preorder bool   `json:"preorder"`
	State    string `json:"state"`
	Items    []struct {
		ASIN       string `json:"asin"`
		IsPreorder bool   `json:"is_preorder"`
	} `json:"items"`
}

// maxOrderPages bounds the orders pager. Audible sent one page with the paging
// fields at 0; should it ever page, a page that adds nothing new stops it too.
const maxOrderPages = 20

// Orders lists the account's orders as far back as Audible offers (about six
// months). It follows max_number_of_pages when Audible sends one over 1,
// bounded, and stops at a page that adds no order it hasn't seen.
func (c *Client) Orders(ctx context.Context) ([]Order, error) {
	var out []Order
	seen := map[string]bool{}
	for page := 1; page <= maxOrderPages; page++ {
		path := "/1.0/orders"
		if page > 1 {
			path += "?" + url.Values{"page": {strconv.Itoa(page)}}.Encode()
		}
		var resp ordersResp
		if err := c.doSigned(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		added := 0
		for _, w := range resp.Orders {
			o := Order{Date: parseOrderDate(w.Date), Preorder: w.Preorder, State: strings.TrimSpace(w.State)}
			key := w.Date + "|" + o.State
			for _, it := range w.Items {
				asin := strings.ToUpper(strings.TrimSpace(it.ASIN))
				o.Items = append(o.Items, OrderItem{ASIN: asin, IsPreorder: it.IsPreorder})
				key += "|" + asin
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, o)
			added++
		}
		if page >= resp.MaxPages || added == 0 {
			break
		}
	}
	return out, nil
}

// parseOrderDate reads an order's date (RFC 3339, "2026-10-19T19:49:51Z") as
// UTC; zero when it doesn't parse.
func parseOrderDate(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// catalogGroups are the response groups a catalog read asks for: a product's
// title needs product_desc (without it Audible sends title null), its release
// date and length product_attrs.
const catalogGroups = "contributors,product_attrs,product_desc,series"

// catalogBatch is the starting number of ASINs per catalog request, split on a
// 400 or 414 as lastpositions' list is (the cap isn't documented).
const catalogBatch = 25

// Products reads titles from Audible's catalog (not the account's library:
// a preorder isn't there) as records, as a library listing would make them,
// with no customer rights (the catalog says nothing about this account's).
// It asks GET /1.0/catalog/products?asins=a,b,... in batches of catalogBatch,
// splitting a batch Audible refuses for its length; an ASIN the batched read
// doesn't answer (or one alone it still refuses with a 400) is read alone
// (GET /1.0/catalog/products/{asin}), and one the catalog doesn't know is left
// out. Each returned record's ASIN is one asked for.
func (c *Client) Products(ctx context.Context, asins []string) ([]source.Record, error) {
	got := map[string]libItem{}
	for start := 0; start < len(asins); start += catalogBatch {
		end := min(start+catalogBatch, len(asins))
		if err := c.productsChunk(ctx, asins[start:end], got); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	out := make([]source.Record, 0, len(asins))
	for _, a := range asins {
		it, ok := got[strings.ToUpper(a)]
		if !ok {
			var err error
			if it, ok, err = c.product(ctx, a); err != nil {
				return nil, err
			}
		}
		if ok {
			it.ASIN = a
			out = append(out, it.toRecord(now))
		}
	}
	return out, nil
}

// productsChunk reads one batch into got (by upper-case ASIN), splitting it on
// a 400 or 414 while it holds more than one ASIN. A single ASIN still refused
// is left for Products to read alone.
func (c *Client) productsChunk(ctx context.Context, asins []string, got map[string]libItem) error {
	if len(asins) == 0 {
		return nil
	}
	q := url.Values{"asins": {strings.Join(asins, ",")}, "response_groups": {catalogGroups}}
	var resp struct {
		Products []libItem `json:"products"`
	}
	if err := c.doSigned(ctx, http.MethodGet, "/1.0/catalog/products?"+q.Encode(), nil, &resp); err != nil {
		var he *httpError
		if errors.As(err, &he) && (he.StatusCode == http.StatusBadRequest || he.StatusCode == http.StatusRequestURITooLong) {
			if len(asins) == 1 {
				return nil
			}
			mid := len(asins) / 2
			if err := c.productsChunk(ctx, asins[:mid], got); err != nil {
				return err
			}
			return c.productsChunk(ctx, asins[mid:], got)
		}
		return err
	}
	for _, p := range resp.Products {
		if a := strings.ToUpper(strings.TrimSpace(p.ASIN)); a != "" {
			got[a] = p
		}
	}
	return nil
}

// product reads one title from the catalog; false (no error) when the catalog
// doesn't know it (a 404 or 400, or an answer for another title).
func (c *Client) product(ctx context.Context, asin string) (libItem, bool, error) {
	var resp struct {
		Product *libItem `json:"product"`
	}
	q := url.Values{"response_groups": {catalogGroups}}
	err := c.doSigned(ctx, http.MethodGet, "/1.0/catalog/products/"+url.PathEscape(asin)+"?"+q.Encode(), nil, &resp)
	if s := HTTPStatus(err); s == http.StatusNotFound || s == http.StatusBadRequest {
		return libItem{}, false, nil
	}
	if err != nil {
		return libItem{}, false, err
	}
	if resp.Product == nil || !strings.EqualFold(strings.TrimSpace(resp.Product.ASIN), asin) {
		return libItem{}, false, nil
	}
	return *resp.Product, true, nil
}
