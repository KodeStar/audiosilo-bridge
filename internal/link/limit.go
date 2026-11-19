package link

import (
	"errors"
	"net/http"
	"time"
)

// Backoff bounds how a job waits out the server's 429s to one call: the
// answer's Retry-After when it sent one (an explicit zero asks again at once),
// else First, doubling with each 429 in a row; Tries asks in all, each wait
// at most Max.
type Backoff struct {
	Tries      int
	First, Max time.Duration
}

// Wait is the wait before asking again after the limited-th 429 (e) in a row.
// false gives the call up: Tries asks made, or a wait over Max.
func (b Backoff) Wait(e *APIError, limited int) (time.Duration, bool) {
	wait := e.RetryAfter
	if !e.RetryAfterSet {
		wait = b.First << (limited - 1)
	}
	if limited >= b.Tries || wait > b.Max {
		return 0, false
	}
	return wait, true
}

// RateLimited reports whether err is the server's 429 (for Backoff.Wait).
func RateLimited(err error) (*APIError, bool) {
	var e *APIError
	if errors.As(err, &e) && e.Status == http.StatusTooManyRequests {
		return e, true
	}
	return nil, false
}
