package logring

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRingKeepsRedactsAndPages(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	ring := New(3)
	log := slog.New(NewHandler(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}), ring))
	log.Debug("not kept")
	log.Info("paired", "server", "http://audiosilo:8080", "token", "secret-value", "pair_code", "ABCD")
	log.With("job", 7).WithGroup("g").Warn("slow", "n", 1)
	log.Error("boom")
	log.Info("four")

	all := ring.Since(0, 0, slog.LevelInfo)
	if len(all) != 3 || all[0].Message != "slow" || all[2].Message != "four" {
		t.Fatalf("ring = %+v", all)
	}
	if all[0].Attrs["job"] != "7" || all[0].Attrs["g.n"] != "1" {
		t.Fatalf("attrs = %+v", all[0].Attrs)
	}
	if got := ring.Since(all[0].ID, 1, slog.LevelInfo); len(got) != 1 || got[0].Message != "boom" {
		t.Fatalf("after = %+v", got)
	}
	if got := ring.Since(0, 0, slog.LevelError); len(got) != 1 || got[0].Message != "boom" {
		t.Fatalf("errors = %+v", got)
	}
	// The wrapped handler still gets what it asked for, and nothing secret is kept.
	if strings.Contains(out.String(), "paired") || !strings.Contains(out.String(), "boom") {
		t.Fatalf("wrapped output = %s", out.String())
	}
	ring2 := New(10)
	slog.New(NewHandler(slog.NewTextHandler(&out, nil), ring2)).Info("paired", "token", "secret-value", "pair_code", "ABCD", "authorization", "Bearer x")
	e := ring2.Since(0, 0, slog.LevelInfo)[0]
	for _, k := range []string{"token", "pair_code", "authorization"} {
		if e.Attrs[k] != "[redacted]" {
			t.Errorf("%s = %q, want redacted", k, e.Attrs[k])
		}
	}
}
