package link

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	useLease   bool // use the handler's remaining lease when known
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
				if b.clamp {
					return b.Max, true
				}
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

// Outside a known job lease the default allows three attempts. A handler
// that can defer work earlier supplies its own Backoff.
var requestBackoff = Backoff{Tries: 3, First: time.Second, Max: time.Minute, clamp: true, useLease: true}

// promptBackoff bounds a call a person waits for on the Bridge's page (Pair,
// Client.Prompt), whose answer must come inside the page's 60 s write timeout:
// a Retry-After over 10 s (a pairing lockout's runs to minutes) is answered
// at once rather than clamped and waited.
var promptBackoff = Backoff{Tries: 3, First: time.Second, Max: 10 * time.Second}

type retryingKey struct{}
type lastErrorKey struct{}
type retryDeadlineKey struct{}

// WithRetryDeadline supplies the end of a running job's 429 budget. Each
// refused request reads it once, so renewals benefit later requests without
// keeping one permanently limited request alive. A zero time means no known
// lease. A nil deadline selects the ordinary request budget for work that can
// be deferred. Calls with their own Backoff keep their own policy.
func WithRetryDeadline(ctx context.Context, deadline func() time.Time) context.Context {
	return context.WithValue(ctx, retryDeadlineKey{}, deadline)
}

// WithLastError keeps the last refusal's original message after retries.
// Server-queued handlers use it to report their usual failure to the server.
func WithLastError(ctx context.Context) context.Context {
	return context.WithValue(ctx, lastErrorKey{}, true)
}

// SingleAttempt leaves retries to the caller. The job reporter uses it while
// holding its send lock so a 429 wait cannot delay newer progress or a finish.
func SingleAttempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryingKey{}, true)
}

// Retry applies the link's usual 429 budget to a call that must prepare each
// attempt itself, such as a refresh that takes a fresh progress snapshot.
func Retry(ctx context.Context, call func(context.Context) error) error {
	return requestBackoff.Do(ctx, Wait, call)
}

// Do retries a rate-limited request within b. A job can own the wait (for
// example, to slow subsequent enrichment PUTs). Its call must use the supplied
// context so the link shares that budget. Only missing or invalid headers get
// jitter, between half and all of the exponential delay.
func (b Backoff) Do(ctx context.Context, wait func(context.Context, time.Duration) error, call func(context.Context) error) error {
	if ctx.Value(retryingKey{}) != nil {
		return call(ctx)
	}
	ctx = SingleAttempt(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	err := call(ctx)
	busy, limited := RateLimited(err)
	if !limited {
		return err
	}

	retryCtx := ctx
	var until time.Time
	if deadline, _ := ctx.Value(retryDeadlineKey{}).(func() time.Time); b.useLease && deadline != nil {
		until = deadline()
		if !until.IsZero() {
			var cancel context.CancelFunc
			retryCtx, cancel = context.WithDeadline(ctx, until)
			defer cancel()
			b.Tries = math.MaxInt
		}
	}
	attempt := 1
	for ; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		delay, retry := b.Wait(busy, attempt)
		if !retry {
			break
		}
		if !until.IsZero() && busy.RetryAfterSet {
			delay = busy.RetryAfter // the lease bounds the wait, even for a Retry-After over Max
		}
		if !busy.RetryAfterSet && delay > 0 {
			delay = delay/2 + rand.N(delay-delay/2) //nolint:gosec // jitter only spreads retries
		}
		waitErr := wait(retryCtx, delay)
		if err := ctx.Err(); err != nil {
			return err
		}
		if !until.IsZero() && !time.Now().Before(until) {
			break
		}
		if waitErr != nil {
			return waitErr
		}
		next := call(retryCtx)
		if errors.Is(next, context.DeadlineExceeded) && retryCtx.Err() != nil && ctx.Err() == nil {
			break // the lease budget ended, return the last refusal
		}
		err = next
		if busy, limited = RateLimited(err); !limited {
			return err
		}
	}
	if ctx.Value(lastErrorKey{}) != nil {
		return err
	}
	return fmt.Errorf("the server is still rate limiting the Bridge (attempt %d): %w", attempt, err)
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
