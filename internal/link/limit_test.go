package link

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

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
