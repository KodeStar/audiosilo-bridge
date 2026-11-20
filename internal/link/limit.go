package link

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
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
	clamp      bool // wait Max for a longer Retry-After instead of giving up
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
		if b.clamp {
			return b.Max, true
		}
		return 0, false
	}
	return wait, true
}

// The default bounds one call to three attempts and two minutes of waiting.
// Jobs with an earlier point at which to defer work supply their own Backoff.
var requestBackoff = Backoff{Tries: 3, First: time.Second, Max: time.Minute, clamp: true}

type retryingKey struct{}

// Do retries a rate-limited request within b. A job can own the wait (for
// example, to slow subsequent enrichment PUTs). Its call must use the supplied
// context so the link shares that budget. Only missing or invalid headers get
// jitter, between half and all of the exponential delay.
func (b Backoff) Do(ctx context.Context, wait func(context.Context, time.Duration) error, call func(context.Context) error) error {
	if ctx.Value(retryingKey{}) != nil {
		return call(ctx)
	}
	ctx = context.WithValue(ctx, retryingKey{}, true)
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := call(ctx)
		busy, limited := RateLimited(err)
		if !limited {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		delay, retry := b.Wait(busy, attempt)
		if !retry {
			return fmt.Errorf("the server is still rate limiting the Bridge (attempt %d): %w", attempt, err)
		}
		if !busy.RetryAfterSet && delay > 0 {
			delay = delay/2 + rand.N(delay-delay/2) //nolint:gosec // jitter only spreads retries
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}

// Wait waits d, or until ctx ends. An ended ctx always wins, so a zero wait
// (an explicit Retry-After: 0) never lets a cancelled job go on.
func Wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil || d <= 0 {
		return err
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
