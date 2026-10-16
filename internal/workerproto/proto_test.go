package workerproto

import (
	"net/url"
	"testing"
)

// ResultMeta round-trips through the query; a decode time that would poison
// the worker's summed row (negative, not finite, absurd) reads as 0.
func TestParseResultMeta(t *testing.T) {
	t.Parallel()
	m := ResultMeta{Attempt: 3, Backend: "mlx-whisper", Device: "metal", Model: "turbo", DecodeSeconds: 41.5}
	if got := ParseResultMeta(m.Query()); got != m {
		t.Fatalf("round trip = %+v, want %+v", got, m)
	}
	for _, bad := range []string{"-1", "NaN", "Inf", "+Inf", "1e308", "x", ""} {
		if got := ParseResultMeta(url.Values{"decode_seconds": {bad}}); got.DecodeSeconds != 0 {
			t.Errorf("decode_seconds=%q = %v, want 0", bad, got.DecodeSeconds)
		}
	}
}
