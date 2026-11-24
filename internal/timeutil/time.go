// Package timeutil provides cancellable sleeps and parses HTTP retry delays.
package timeutil

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SleepCtx waits for d or until ctx ends. A ctx that has already ended always
// wins, even over a zero or negative d, so a retry loop never goes on after its
// cancel (an expired timer would otherwise race the ended ctx in the select).
func SleepCtx(ctx context.Context, d time.Duration) error {
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

// RetryAfterSource selects the header syntax a client accepts.
type RetryAfterSource int

const (
	// Server accepts unsigned decimal digits, including values larger than an int.
	Server RetryAfterSource = iota
	// Audible accepts nonnegative values that fit in an int, including a leading +.
	Audible
	// Feed accepts seconds like Audible. It reads dates as received, in whole seconds.
	Feed
)

// RetryAfter reads delay seconds or an HTTP date, capped at one day.
// An explicit zero and a past date are both valid requests to retry immediately.
func RetryAfter(value string, now time.Time, source RetryAfterSource) (time.Duration, bool) {
	const limit = 24 * time.Hour
	trimmed := strings.TrimSpace(value)
	if source == Server {
		if trimmed != "" && strings.Trim(trimmed, "0123456789") == "" {
			// ParseUint saturates on overflow. Clamp before multiplying.
			n, _ := strconv.ParseUint(trimmed, 10, 64)
			return time.Duration(min(n, uint64(limit/time.Second))) * time.Second, true
		}
	} else if n, err := strconv.Atoi(trimmed); err == nil && n >= 0 {
		return time.Duration(min(n, int(limit/time.Second))) * time.Second, true
	}
	if source != Feed {
		value = trimmed
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := date.Sub(now)
	if source == Feed {
		seconds := min(max(int(delay.Seconds()), 0), int(limit/time.Second))
		return time.Duration(seconds) * time.Second, true
	}
	return min(max(delay, 0), limit), true
}
