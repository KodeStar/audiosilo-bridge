package timeutil

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

func TestSleepCtx(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if err := SleepCtx(t.Context(), time.Minute); err != nil || time.Since(start) != time.Minute {
			t.Fatalf("sleep ended after %v with %v", time.Since(start), err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- SleepCtx(ctx, time.Hour) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || time.Since(start) != time.Minute {
			t.Fatalf("cancelled sleep ended after %v with %v", time.Since(start), err)
		}

		// An ended ctx wins over a wait that is already due, every time.
		for _, d := range []time.Duration{-time.Second, 0, time.Nanosecond} {
			for range 100 {
				if err := SleepCtx(ctx, d); !errors.Is(err, context.Canceled) {
					t.Fatalf("SleepCtx(ended, %v) = %v, want the cancel", d, err)
				}
			}
		}
		if err := SleepCtx(t.Context(), -time.Second); err != nil || time.Since(start) != time.Minute {
			t.Fatalf("a negative wait waited until %v with %v", time.Since(start), err)
		}
	})
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	const day = 24 * time.Hour
	for _, tc := range []struct {
		header string
		wait   time.Duration
		ok     bool
	}{
		{"0", 0, true}, {" 30 ", 30 * time.Second, true},
		{now.Add(time.Minute).Format(http.TimeFormat), time.Minute, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"86401", day, true}, {now.Add(2 * day).Format(http.TimeFormat), day, true},
		{"", 0, false}, {"soon", 0, false}, {"-5", 0, false},
	} {
		for _, source := range []RetryAfterSource{Server, Audible, Feed} {
			if wait, ok := RetryAfter(tc.header, now, source); wait != tc.wait || ok != tc.ok {
				t.Errorf("source %d, %q = (%v, %t), want (%v, %t)", source, tc.header, wait, ok, tc.wait, tc.ok)
			}
		}
	}
}

func TestRetryAfterSourceSyntax(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		header                string
		wait                  time.Duration
		server, audible, feed bool
	}{
		{"+3", 3 * time.Second, false, true, true},
		{"-0", 0, false, true, true},
		{"9999999999", 24 * time.Hour, true, strconv.IntSize == 64, strconv.IntSize == 64},
		{"18446744074", 24 * time.Hour, true, strconv.IntSize == 64, strconv.IntSize == 64},
		{"9223372036854775807", 24 * time.Hour, true, strconv.IntSize == 64, strconv.IntSize == 64},
		{"  " + now.Add(time.Minute).Format(http.TimeFormat) + "  ", time.Minute, true, true, false},
		{"9999999999999999999999999999", 24 * time.Hour, true, false, false},
	} {
		for source, accepted := range map[RetryAfterSource]bool{Server: tc.server, Audible: tc.audible, Feed: tc.feed} {
			want := time.Duration(0)
			if accepted {
				want = tc.wait
			}
			if wait, ok := RetryAfter(tc.header, now, source); wait != want || ok != accepted {
				t.Errorf("source %d, %q = (%v, %t), want (%v, %t)", source, tc.header, wait, ok, want, accepted)
			}
		}
	}
}

func TestRetryAfterDatePrecision(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 250_000_000, time.UTC)
	header := now.Add(2 * time.Second).Format(http.TimeFormat)
	for source, want := range map[RetryAfterSource]time.Duration{
		Server: 1750 * time.Millisecond, Audible: 1750 * time.Millisecond, Feed: time.Second,
	} {
		if wait, ok := RetryAfter(header, now, source); wait != want || !ok {
			t.Errorf("source %d, %q = (%v, %t), want (%v, true)", source, header, wait, ok, want)
		}
	}
}
