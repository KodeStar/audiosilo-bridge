package link

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A refused POST can be replayed, even if the server consumed the body before
// sending its 429. A later ambiguous failure must end the call.
func TestRateLimitedRescanReplaysPOST(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/api/v1/bridge/rescan" ||
					r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get(ClientHeader) != clientValue("dev") ||
					r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("rescan request %s %s, headers %v", r.Method, r.URL, r.Header)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != `{"library_id":7,"path":"A/B"}` {
					t.Errorf("rescan body = %s, err = %v", body, err)
				}
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"path":"A/B"}`)
			}))
			defer srv.Close()
			c, _ := New(srv.URL, "tok", "dev")
			start := time.Now()
			path, err := c.Rescan(t.Context(), 7, "A/B")
			if status == http.StatusOK && (err != nil || path != "A/B") {
				t.Fatalf("rescan = %q, %v", path, err)
			}
			var ae *APIError
			if status != http.StatusOK && (!errors.As(err, &ae) || ae.Status != status) {
				t.Fatalf("rescan = %v, want %d", err, status)
			}
			if took := time.Since(start); took < time.Second || calls.Load() != 2 {
				t.Fatalf("rescan took %v, calls %d", took, calls.Load())
			}
		})
	}
}

// A truncated response body cannot undo the status that preceded it. A 429
// refused the POST. A truncated 200 may have committed it.
func TestRescanReplayWithTruncatedAnswer(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusTooManyRequests, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Length", "100")
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
					return
				}
				_, _ = io.WriteString(w, `{"path":"A/B"}`)
			}))
			defer srv.Close()
			cl, _ := New(srv.URL, "", "dev")
			path, err := cl.Rescan(t.Context(), 7, "A/B")
			if status == http.StatusOK {
				if !errors.Is(err, io.ErrUnexpectedEOF) || calls.Load() != 1 {
					t.Fatalf("ambiguous POST replay: error %v, calls %d", err, calls.Load())
				}
			} else if err != nil || path != "A/B" || calls.Load() != 2 {
				t.Fatalf("refused POST: path %q, error %v, calls %d", path, err, calls.Load())
			}
		})
	}
}

func TestRateLimitedRequestWaits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, header string
		min, max     time.Duration
	}{
		{"date", "", 20 * time.Second, 30 * time.Second},
		{"missing", "", time.Second / 2, time.Second},
		{"junk", "1.5", time.Second / 2, time.Second},
		{"capped seconds", "86400", time.Minute, time.Minute},
		{"capped date", "Mon, 01 Jan 2091 00:00:00 GMT", time.Minute, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Query().Get("wait") != "25" ||
					!slices.Equal(r.URL.Query()["kind"], []string{"podcast.fetch", "podcast.poll"}) {
					t.Errorf("claim = %s %s", r.Method, r.URL)
				}
				if calls.Add(1) == 1 {
					header := tc.header
					if tc.name == "date" {
						header = time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
					}
					if header != "" {
						w.Header().Set("Retry-After", header)
					}
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			c, _ := New(srv.URL, "", "dev")
			var waits []time.Duration
			c.wait = func(ctx context.Context, d time.Duration) error {
				waits = append(waits, d)
				if _, deadline := ctx.Deadline(); deadline {
					t.Error("the long poll's HTTP timeout also covers the retry wait")
				}
				return nil
			}
			job, err := c.Claim(context.Background(), []string{"podcast.fetch", "podcast.poll"}, MaxWait)
			if err != nil || job != nil || calls.Load() != 2 || len(waits) != 1 || waits[0] < tc.min || waits[0] > tc.max {
				t.Fatalf("claim = %v, %v; calls %d, waits %v", job, err, calls.Load(), waits)
			}
		})
	}
}

func TestRateLimitedHeartbeatCancelledDuringWait(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "dev")
	waiting := make(chan struct{})
	c.wait = func(ctx context.Context, d time.Duration) error {
		close(waiting)
		return waitContext(ctx, d)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Heartbeat(ctx, Status{})
		done <- err
	}()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("heartbeat ended before waiting: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not reach the wait")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("heartbeat after cancel = %v, calls %d", err, calls.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat continued waiting after cancellation")
	}
}

func TestRateLimitedRequestStopsAtBound(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "dev")
	_, err := c.Book(t.Context(), 1, "A/B")
	busy, limited := RateLimited(err)
	if !limited || !busy.RetryAfterSet || busy.RetryAfter != 0 ||
		!strings.Contains(err.Error(), "attempt 3") || calls.Load() != 3 {
		t.Fatalf("book = %v, calls %d", err, calls.Load())
	}
}
