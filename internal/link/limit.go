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
// false gives the call up: Tries asks made, or a wait over Max. The doubling
// stops at the first wait over Max, so no count of 429s overflows it (a
// shift would, into a zero or negative wait asked again at once), and a
// limited below 1 counts as the first.
func (b Backoff) Wait(e *APIError, limited int) (time.Duration, bool) {
	if limited >= b.Tries {
		return 0, false
	}
	wait := e.RetryAfter
	if !e.RetryAfterSet {
		wait = b.First
		for n := 1; n < limited && wait > 0; n++ {
			if wait > b.Max/2 {
				return 0, false // the next doubling is over Max
			}
			wait *= 2
		}
	}
	if wait > b.Max {
		return 0, false
	}
	return wait, true
}

// Transient reports whether the same call may be answered differently if made
// again soon: a 5xx, a 408 or a 429. Any other status is the server's answer
// however soon it is asked again.
func (e *APIError) Transient() bool {
	return e.Status >= http.StatusInternalServerError ||
		e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests
}

// RateLimited reports whether err is the server's 429 (for Backoff.Wait).
func RateLimited(err error) (*APIError, bool) {
	var e *APIError
	if errors.As(err, &e) && e.Status == http.StatusTooManyRequests {
		return e, true
	}
	return nil, false
}
