//go:build audiblefake

package audiblefake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Recorder is an http.RoundTripper that writes every exchange it carries to a
// JSON-lines file, for the owner's one recording run against a real account
// (the fake's later fixtures come from it). It scrubs AS IT WRITES: tokens, the
// device key, codes, the customer and device ids, cookies, vouchers, signed URLs,
// the account holder's name, and an order's payment fields and any price never
// reach the file; titles, authors and the
// shapes are kept. A body it can't parse as JSON or a form is recorded by size
// only. Headers are recorded from an allowlist (never Authorization, Cookie or
// the x-adp-* signature). It writes to its own file, never the log.
type Recorder struct {
	base http.RoundTripper
	dir  string

	mu   sync.Mutex
	file *os.File
	err  error
}

// NewRecorder records base's exchanges into a new file in dir (created 0700;
// the file 0600) on the first request.
func NewRecorder(base http.RoundTripper, dir string) *Recorder {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Recorder{base: base, dir: dir}
}

// maxRecordedBody is the most of one body the recorder buffers; a longer one
// (an audio download) is recorded by size only and streamed through.
const maxRecordedBody = 16 << 20

// Entry is one recorded exchange.
type Entry struct {
	Time            time.Time         `json:"time"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	RequestBody     any               `json:"request_body,omitempty"`
	Status          int               `json:"status,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	ResponseBody    any               `json:"response_body,omitempty"`
	Error           string            `json:"error,omitempty"`
}

// RoundTrip carries req through base and records the exchange. A failure to
// record never fails the request.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	e := Entry{Time: time.Now().UTC(), Method: req.Method, URL: ScrubURL(req.URL), RequestHeaders: keptHeaders(req.Header)}
	if req.Body != nil && req.Body != http.NoBody {
		raw, err := io.ReadAll(io.LimitReader(req.Body, maxRecordedBody+1))
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		e.RequestBody = scrubBody(raw, req.Header.Get("Content-Type"))
	}
	resp, err := r.base.RoundTrip(req)
	if err != nil {
		e.Error = "request failed"
		r.write(e)
		return nil, err
	}
	e.Status, e.ResponseHeaders = resp.StatusCode, keptHeaders(resp.Header)
	if recordable(resp.Header.Get("Content-Type")) {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxRecordedBody+1))
		if rerr != nil {
			_ = resp.Body.Close()
			return nil, rerr
		}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(raw), resp.Body), resp.Body}
		e.ResponseBody = scrubBody(raw, resp.Header.Get("Content-Type"))
	} else {
		e.ResponseBody = fmt.Sprintf("<%s body, %d bytes, not recorded>", resp.Header.Get("Content-Type"), resp.ContentLength)
	}
	r.write(e)
	return resp, nil
}

// write appends e to the recording file, opening it on first use.
func (r *Recorder) write(e Entry) {
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil && r.err == nil {
		if r.err = os.MkdirAll(r.dir, 0o700); r.err == nil {
			name := filepath.Join(r.dir, fmt.Sprintf("audible-%s-%d.jsonl", time.Now().UTC().Format("20060102-150405"), os.Getpid()))
			r.file, r.err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator's recording dir
		}
	}
	if r.file != nil {
		_, _ = r.file.Write(append(line, '\n'))
	}
}

// recordable reports whether a body of this type is buffered and recorded.
func recordable(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "json") || strings.HasPrefix(ct, "text/") || strings.Contains(ct, "x-www-form-urlencoded")
}

// keptHeaderNames are the headers recorded; everything else (Authorization,
// Cookie, Set-Cookie, x-adp-token, x-adp-signature) is left out.
var keptHeaderNames = []string{"Content-Type", "Content-Length", "Content-Range", "Range", "Retry-After", "X-Amzn-Identity-Auth-Domain", "Location"}

func keptHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, n := range keptHeaderNames {
		if v := h.Get(n); v != "" {
			if n == "Location" {
				if u, err := url.Parse(v); err == nil {
					v = ScrubURL(u)
				} else {
					v = redacted
				}
			}
			out[n] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

const redacted = "REDACTED"

// secretKeys are JSON object keys and form/query names whose values are
// credentials or identify the account or device (lowercase).
var secretKeys = map[string]bool{
	"access_token": true, "refresh_token": true, "adp_token": true, "device_private_key": true,
	"authorization_code": true, "openid.oa2.authorization_code": true, "code_verifier": true,
	"code_challenge": true, "openid.oa2.code_challenge": true, "client_id": true, "openid.oa2.client_id": true,
	"source_token": true, "device_serial": true, "device_serial_number": true, "serial": true,
	"user_id": true, "customer_id": true, "license_response": true, "offline_url": true,
	"key": true, "iv": true, "acr": true, "token": true, "password": true, "email": true,
	"given_name": true, "website_cookies": true, "store_authentication_cookie": true,
	"x-amz-signature": true, "x-amz-credential": true, "x-amz-security-token": true,
	"signature": true, "policy": true, "key-pair-id": true,
	// An order item's payment fields (GET /1.0/orders) and any price.
	"amount_charged": true, "payment_method": true, "tax_amount": true, "taxes": true, "tax_transaction_id": true,
	"coupon_amount": true, "credits_applied": true, "promotion_amount": true, "promo_ids": true, "credit_promo_id": true,
	"merchant_id": true, "plan_id": true, "plan_status": true, "next_billing_date": true, "combined_order_id": true,
	"institution_id": true, "institution_benefit_plan_id": true, "emp_provider_id": true, "return_information": true,
	"billing_address": true, "shipping_address": true, "address": true, "price": true,
}

// secretSubtrees are JSON keys whose whole subtree is scrubbed (every string
// leaf), the structure kept: the account holder, the device, the token sets.
var secretSubtrees = map[string]bool{
	"customer_info": true, "device_info": true, "tokens": true, "cookies": true,
	"mac_dms": true, "bearer": true, "store_authentication_cookie": true,
}

// ScrubURL is u with secret query values redacted, and with no query at all off
// the API paths (a presigned CDN URL's whole query is its credential).
func ScrubURL(u *url.URL) string {
	c := *u
	c.User = nil
	if c.RawQuery != "" {
		if !strings.HasPrefix(c.Path, "/1.0/") && !strings.HasPrefix(c.Path, "/ap/") && !strings.HasPrefix(c.Path, "/auth/") {
			c.RawQuery = redacted
		} else {
			q := c.Query()
			for k := range q {
				if secretKeys[strings.ToLower(k)] {
					q[k] = []string{redacted}
				}
			}
			c.RawQuery = q.Encode()
		}
	}
	return c.String()
}

// scrubBody parses raw as JSON or a form and returns it scrubbed; anything else
// is its size only.
func scrubBody(raw []byte, contentType string) any {
	if len(raw) > maxRecordedBody {
		return fmt.Sprintf("<body over %d bytes, not recorded>", maxRecordedBody)
	}
	if strings.Contains(strings.ToLower(contentType), "x-www-form-urlencoded") {
		q, err := url.ParseQuery(string(raw))
		if err != nil {
			return fmt.Sprintf("<unparseable form, %d bytes>", len(raw))
		}
		out := map[string]any{}
		for k, vs := range q {
			if secretKeys[strings.ToLower(k)] {
				out[k] = redacted
			} else {
				out[k] = vs
			}
		}
		return out
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return fmt.Sprintf("<non-JSON body, %d bytes>", len(raw))
	}
	return scrubJSON(v, false)
}

// scrubJSON walks v, redacting secret keys' values, and every string under a
// secret subtree (all set).
func scrubJSON(v any, all bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			lk := strings.ToLower(k)
			switch {
			case secretKeys[lk]:
				t[k] = redactValue(val)
			default:
				t[k] = scrubJSON(val, all || secretSubtrees[lk])
			}
		}
		return t
	case []any:
		for i := range t {
			t[i] = scrubJSON(t[i], all)
		}
		return t
	case string:
		if all {
			return redacted
		}
		return t
	default:
		return t
	}
}

// redactValue replaces a secret value, keeping only its shape (an array stays
// an array of the same length).
func redactValue(v any) any {
	if arr, ok := v.([]any); ok {
		out := make([]any, len(arr))
		for i := range out {
			out[i] = redacted
		}
		return out
	}
	return redacted
}
