package netguard

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type StreamRequest struct {
	URL    string
	Offset int64
	ETag   string
}
type StreamResponse struct {
	Status            int
	Body              io.ReadCloser
	ContentLength     int64
	ContentRange      string
	Encoding          string
	ETag              string
	RetryAfterSeconds int
}

// Stream performs exactly one guarded audio GET. The caller owns the three-attempt
// budget, including resume fallbacks, and must close Body. There is no feed timeout.
func (c *Client) Stream(ctx context.Context, in StreamRequest) (*StreamResponse, error) {
	if !validURL(in.URL) {
		return nil, &Error{Code: "invalid_feed_url", Message: "The episode address is invalid."}
	}
	ctx, cancel := context.WithCancel(ctx)
	hc := &http.Client{Transport: c.transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return &Error{Code: "http_310", Message: "The episode redirected too many times."}
		}
		if !validURL(req.URL.String()) {
			return &Error{Code: "invalid_feed_url", Message: "The episode redirected to an invalid address."}
		}
		from := via[len(via)-1].URL
		if !strings.EqualFold(from.Scheme, req.URL.Scheme) || !strings.EqualFold(from.Host, req.URL.Host) {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			req.URL.User = nil
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
	if err != nil {
		cancel()
		return nil, &Error{Code: "invalid_feed_url", Message: "The episode address is invalid."}
	}
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept-Encoding", "identity")
	if in.Offset > 0 && StrongETag(in.ETag) {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(in.Offset, 10)+"-")
		req.Header.Set("If-Range", in.ETag)
	}
	resp, err := hc.Do(req) //nolint:bodyclose // ownership transfers to StreamResponse.Body; caller must close it
	if err != nil {
		cancel()
		return nil, classify(err)
	}
	timer := time.AfterFunc(c.stall, cancel)
	body := &streamBody{progressReader: progressReader{r: resp.Body, timer: timer, stall: c.stall}, body: resp.Body, cancel: cancel}
	return &StreamResponse{Status: resp.StatusCode, Body: body, ContentLength: resp.ContentLength, ContentRange: resp.Header.Get("Content-Range"), Encoding: strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))), ETag: resp.Header.Get("ETag"), RetryAfterSeconds: retryAfter(resp.Header.Get("Retry-After"), time.Now())}, nil
}

// streamBody resets the stall timer on progress (progressReader) and ends the
// request's context once on Close.
type streamBody struct {
	progressReader
	body   io.Closer
	cancel context.CancelFunc
	once   sync.Once
}

func (b *streamBody) Close() error {
	b.once.Do(func() { b.timer.Stop(); b.cancel() })
	return b.body.Close()
}

// StrongETag is deliberately stricter than a header presence or Last-Modified.
func StrongETag(s string) bool {
	if len(s) < 2 || len(s) > 8192 || s[0] != '"' || s[len(s)-1] != '"' {
		return false
	}
	for _, c := range []byte(s[1 : len(s)-1]) {
		if c < 0x21 || c == 0x22 || c == 0x7f {
			return false
		}
	}
	return true
}

// ResumeAction implements the byte-exact append contract. A 200 is consumed from
// zero; any unproven 206 is discarded and consumes an attempt before a fresh GET.
func ResumeAction(offset, total int64, etag string, r *StreamResponse) string {
	if r.Encoding != "" && r.Encoding != "identity" {
		return "fresh_get"
	}
	if r.Status == 200 {
		return "restart_with_200_body"
	}
	if r.Status != 206 || !StrongETag(etag) || r.ETag != etag || offset <= 0 || total <= offset {
		return "fresh_get"
	}
	expected := "bytes " + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(total-1, 10) + "/" + strconv.FormatInt(total, 10)
	if r.ContentRange != expected || r.ContentLength >= 0 && r.ContentLength != total-offset {
		return "fresh_get"
	}
	return "append"
}

func Retryable(err error) bool  { return retryable(classify(err)) }
func Classify(err error) *Error { return classify(err) }

// RetryDelay retains the existing jitter and honors a bounded Retry-After.
func RetryDelay(attempt int, retryAfter int) time.Duration {
	d := time.Second
	if attempt > 0 {
		d = 4 * time.Second
	}
	return max(jitter(d), time.Duration(retryAfter)*time.Second)
}
