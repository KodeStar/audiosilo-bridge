package audible

import (
	"context"
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

// The state and detail_state of a delivered order (Order.Delivered).
const (
	OrderClosed    = "ORDER_CLOSED"
	OrderSucceeded = "ORDER_SUCCEEDED"
)

// Order is one order of the account, as far as the Bridge reads it: never a
// payment, price or address field.
type Order struct {
	// Date is when it was placed (zero when Audible sent none that parses).
	Date        time.Time
	Preorder    bool
	State       string
	DetailState string
	Items       []OrderItem
}

// OrderItem is one title of an order.
type OrderItem struct {
	ASIN string
}

// PendingPreorder reports whether o is a preorder still waiting for its
// release: a preorder (preorder true) whose state is OrderPendingPreorder.
func (o Order) PendingPreorder() bool {
	return o.Preorder && o.State == OrderPendingPreorder
}

// Delivered reports whether o went through: ORDER_CLOSED with detail_state
// ORDER_SUCCEEDED. A delivered preorder's title joins the library, though the
// library may list it a little later than the order closes.
func (o Order) Delivered() bool {
	return o.State == OrderClosed && o.DetailState == OrderSucceeded
}

type ordersResp struct {
	Orders   []orderWire `json:"orders"`
	MaxPages int         `json:"max_number_of_pages"`
}

// orderWire decodes only the fields an Order keeps: the payment fields an
// item carries are never read into memory as values.
type orderWire struct {
	Date        string `json:"date"`
	Preorder    bool   `json:"preorder"`
	State       string `json:"state"`
	DetailState string `json:"detail_state"`
	Items       []struct {
		ASIN string `json:"asin"`
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
			o := Order{Date: parseLastUpdated(w.Date), Preorder: w.Preorder, State: strings.TrimSpace(w.State),
				DetailState: strings.TrimSpace(w.DetailState)}
			key := w.Date + "|" + o.State + "|" + o.DetailState
			for _, it := range w.Items {
				asin := strings.ToUpper(strings.TrimSpace(it.ASIN))
				o.Items = append(o.Items, OrderItem{ASIN: asin})
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

// catalogGroups are the response groups a catalog read asks for: a product's
// title needs product_desc (without it Audible sends title null), its release
// date and length product_attrs.
const catalogGroups = "contributors,product_attrs,product_desc,series"

// catalogBatch is the starting number of ASINs per catalog request, split on a
// 400 or 414 as lastpositions' list is (the cap isn't documented).
const catalogBatch = lastPositionsBatch

// Products reads titles from Audible's catalog (not the account's library:
// a preorder isn't there) as records, as a library listing would make them,
// with no customer rights (the catalog says nothing about this account's).
// It asks GET /1.0/catalog/products?asins=a,b,... in batches of catalogBatch,
// splitting a batch Audible refuses for its length; an ASIN the batched reads
// don't answer with its title (left out, one alone still refused with a 400,
// or in a batch that failed) is read alone (GET /1.0/catalog/products/{asin}),
// and one the catalog doesn't know is left out. A failed batch ends the
// batches; Audible asking the Bridge to wait, ctx ending or a single read
// failing ends the reads, and the records read before are returned with that
// error. Each returned record's ASIN is one asked for.
func (c *Client) Products(ctx context.Context, asins []string) ([]source.Record, error) {
	got := map[string]libItem{}
	var err error
	for start := 0; start < len(asins) && err == nil; start += catalogBatch {
		err = c.productsChunk(ctx, asins[start:min(start+catalogBatch, len(asins))], got)
	}
	if _, wait := RetryWait(err); !wait && ctx.Err() == nil {
		err = nil // the single reads answer for the batches that didn't
	}
	now := time.Now()
	out := make([]source.Record, 0, len(asins))
	for _, a := range asins {
		it, ok := got[strings.ToUpper(a)]
		if !ok && err == nil {
			it, ok, err = c.product(ctx, a)
		}
		if ok {
			it.ASIN = a
			out = append(out, it.toRecord(now))
		}
	}
	return out, err
}

// productsChunk reads one batch into got (by upper-case ASIN), split by
// splitOnOverflow. A single ASIN still refused, or a title answered without
// its title (the response groups not applied), is left for Products to read
// alone.
func (c *Client) productsChunk(ctx context.Context, asins []string, got map[string]libItem) error {
	return splitOnOverflow(asins, func(batch []string) error {
		q := url.Values{"asins": {strings.Join(batch, ",")}, "response_groups": {catalogGroups}}
		var resp struct {
			Products []libItem `json:"products"`
		}
		err := c.doSigned(ctx, http.MethodGet, "/1.0/catalog/products?"+q.Encode(), nil, &resp)
		if len(batch) == 1 && listOverflow(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, p := range resp.Products {
			if a := strings.ToUpper(strings.TrimSpace(p.ASIN)); a != "" && strings.TrimSpace(p.Title) != "" {
				got[a] = p
			}
		}
		return nil
	})
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
