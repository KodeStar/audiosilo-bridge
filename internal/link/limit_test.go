package link

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestJobRetryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		ctx := WithRetryDeadline(t.Context(), func() time.Time {
			return time.Now().Add(4 * time.Second) // a later renewal cannot extend this call
		})
		calls := 0
		var last *APIError
		err := Retry(WithLastError(ctx), func(context.Context) error {
			calls++
			last = &APIError{Status: http.StatusTooManyRequests, RetryAfter: time.Second, RetryAfterSet: true}
			return last
		})
		if err != last || calls != 4 || time.Since(start) != 4*time.Second {
			t.Fatalf("last refusal %v, returned %v after %d calls in %v", last, err, calls, time.Since(start))
		}
	})
}

func TestJobRetryWithoutHeaderAfterBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		ctx := WithRetryDeadline(t.Context(), func() time.Time { return start.Add(110 * time.Second) })
		calls := 0
		err := Retry(ctx, func(context.Context) error {
			calls++
			switch {
			case calls <= 20:
				return &APIError{Status: http.StatusTooManyRequests, RetryAfterSet: true}
			case calls == 21:
				return &APIError{Status: http.StatusTooManyRequests} // fall back to capped exponential backoff
			default:
				return nil
			}
		})
		if elapsed := time.Since(start); err != nil || calls != 22 || elapsed < 30*time.Second || elapsed > time.Minute {
			t.Fatalf("after the missing header: %v, %d calls in %v", err, calls, elapsed)
		}
	})
}

func TestJobRetryDeadlineDuringRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, parentEndsFirst := range []bool{false, true} {
			start := time.Now()
			ctx := WithRetryDeadline(t.Context(), func() time.Time { return start.Add(90 * time.Second) })
			parentBudget := 2 * time.Minute
			if parentEndsFirst {
				parentBudget = 80 * time.Second
			}
			parent, cancel := context.WithTimeout(ctx, parentBudget)
			calls := 0
			busy := &APIError{Status: http.StatusTooManyRequests, RetryAfter: 70 * time.Second, RetryAfterSet: true}
			err := Retry(parent, func(ctx context.Context) error {
				calls++
				if calls == 1 {
					return busy
				}
				if elapsed := time.Since(start); elapsed != busy.RetryAfter {
					t.Fatalf("retried after %v, want %v", elapsed, busy.RetryAfter)
				}
				<-ctx.Done()
				return ctx.Err()
			})
			cancel()
			wantErr, elapsed := error(busy), 90*time.Second
			if parentEndsFirst {
				wantErr, elapsed = context.DeadlineExceeded, 80*time.Second
			}
			if calls != 2 || !errors.Is(err, wantErr) || time.Since(start) != elapsed {
				t.Fatalf("retried %d times, ended with %v after %v", calls, err, time.Since(start))
			}
		}
	})
}

// Backoff.Wait: the server's Retry-After when it sent one (a zero asks again
// at once), else First doubling; Tries asks in all, each wait at most Max.
func TestBackoffWait(t *testing.T) {
	b := Backoff{Tries: 5, First: 2 * time.Second, Max: time.Minute}
	after := func(d time.Duration) *APIError { return &APIError{RetryAfter: d, RetryAfterSet: true} }
	for _, tc := range []struct {
		name    string
		b       Backoff
		e       *APIError
		limited int
		want    time.Duration
		ok      bool
	}{
		{"the first wait", b, &APIError{}, 1, 2 * time.Second, true},
		{"doubling", b, &APIError{}, 4, 16 * time.Second, true},
		{"the last ask", b, &APIError{}, 5, 0, false},
		{"its Retry-After", b, after(3 * time.Second), 2, 3 * time.Second, true},
		{"a zero Retry-After", b, after(0), 1, 0, true},
		{"a Retry-After at Max", b, after(time.Minute), 1, time.Minute, true},
		{"a Retry-After over Max", b, after(time.Minute + time.Second), 1, 0, false},
		{"a doubling over Max", Backoff{Tries: 9, First: 2 * time.Second, Max: 30 * time.Second}, &APIError{}, 5, 0, false},
		// A shift would wrap to a zero or negative wait, asked again at once.
		{"many 429s in a row", Backoff{Tries: 1000, First: 2 * time.Second, Max: time.Minute}, &APIError{}, 70, 0, false},
		{"many 429s and no Max to speak of", Backoff{Tries: 1000, First: 2 * time.Second, Max: math.MaxInt64}, &APIError{}, 70, 0, false},
		// A shift by a negative count panics.
		{"a count below one", b, &APIError{}, 0, 2 * time.Second, true},
		{"a negative count", b, &APIError{}, -3, 2 * time.Second, true},
		{"no first wait", Backoff{Tries: 5, Max: time.Minute}, &APIError{}, 3, 0, true},
		{"no tries", Backoff{}, &APIError{}, 1, 0, false},
	} {
		if got, ok := tc.b.Wait(tc.e, tc.limited); got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %v %v, want %v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestAPIErrorTransient(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusBadRequest:          false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
		http.StatusConflict:            false,
		http.StatusRequestTimeout:      true,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
	} {
		if got := (&APIError{Status: status}).Transient(); got != want {
			t.Errorf("%d: Transient = %v, want %v", status, got, want)
		}
	}
}

func TestRateLimited(t *testing.T) {
	busy := &APIError{Status: http.StatusTooManyRequests}
	if e, ok := RateLimited(fmt.Errorf("wrapped: %w", busy)); !ok || e != busy {
		t.Fatal("a wrapped 429 was not picked out", e, ok)
	}
	for _, err := range []error{nil, errors.New("other"), &APIError{Status: http.StatusServiceUnavailable}, ErrNotIndexable} {
		if _, ok := RateLimited(err); ok {
			t.Fatal("not a 429", err)
		}
	}
}
