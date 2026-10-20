package logring

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// The ring keeps exactly the newest entries in order as it wraps, and a level
// encodes as its name.
func TestRingWraps(t *testing.T) {
	t.Parallel()
	ring := New(3)
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), ring))
	for i := range 7 {
		log.Info(fmt.Sprint(i))
	}
	got := ring.Since(0, 0, slog.LevelInfo)
	if len(got) != 3 || got[0].Message != "4" || got[2].Message != "6" || got[2].ID-got[0].ID != 2 {
		t.Fatalf("ring = %+v", got)
	}
	raw, _ := json.Marshal(got[0])
	if !strings.Contains(string(raw), `"level":"INFO"`) {
		t.Fatalf("entry JSON = %s", raw)
	}
}

// A page keeps its cursor across a Bridge restart (its session is in the
// database): the new process's ids are all above the old one's, so its lines
// still come back (allowed), while a cursor of this process hides what it has
// already shown (denied).
func TestIDsGrowAcrossRestarts(t *testing.T) {
	t.Parallel()
	before := New(10)
	logTo(before, "old", 5)
	shown := before.Since(0, 0, slog.LevelInfo)
	cursor := shown[len(shown)-1].ID
	time.Sleep(2 * time.Millisecond) // a restart takes far longer than the lines logged meanwhile
	after := New(10)
	logTo(after, "new", 3)
	if got := after.Since(cursor, 0, slog.LevelInfo); len(got) != 3 || got[0].Message != "new" {
		t.Fatalf("after a restart, the old cursor %d gives %+v", cursor, got)
	}
	if got := after.Since(after.Since(0, 0, slog.LevelInfo)[2].ID, 0, slog.LevelInfo); len(got) != 0 {
		t.Fatalf("this process's own cursor repeats lines: %+v", got)
	}
}

func logTo(r *Ring, msg string, n int) {
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), r))
	for range n {
		log.Info(msg)
	}
}

func TestFeedURLsRedactedInBothSinks(t *testing.T) {
	t.Parallel()
	var captured bytes.Buffer
	ring := New(10)
	log := slog.New(NewHandler(slog.NewTextHandler(&captured, nil), ring))
	url := "https://user:password@example.com/private/feed?auth=SECRET123"
	log.With("feed_url", url).WithGroup("podcast").Warn("failed "+url, "err", fmt.Errorf("fetch %s: refused", url), "feed_url", url, "episodes", 12)
	log.Info("nested", slog.Group("podcast", "feed_url", url, "title", "Allowed title"))
	raw, _ := json.Marshal(ring.Since(0, 0, slog.LevelInfo))
	for _, out := range []string{captured.String(), string(raw)} {
		if strings.Contains(out, "SECRET123") || strings.Contains(out, "user:password") {
			t.Fatalf("credential leaked: %s", out)
		}
		if !strings.Contains(out, "https://example.com/...") || !strings.Contains(out, "Allowed title") {
			t.Fatalf("allowed fields lost: %s", out)
		}
	}
}

// The key redaction is the page's log view's: the process log keeps the values
// of keys that only sound secret (key_source, decode_seconds, exit_code), as it
// did before, while URLs lose their credentials in both sinks.
func TestProcessLogKeepsWhatOnlyTheRingRedacts(t *testing.T) {
	t.Parallel()
	var captured bytes.Buffer
	ring := New(10)
	log := slog.New(NewHandler(slog.NewTextHandler(&captured, nil), ring))
	log.WithGroup("asr").Info("started", "key_source", "env", "decode_seconds", 12, "exit_code", 3, "err", fmt.Errorf("fetch https://u:p@example.com/feed?auth=SECRET123: refused"), "token", "s3cret")
	out := captured.String()
	for _, want := range []string{"asr.key_source=env", "asr.decode_seconds=12", "asr.exit_code=3", "https://example.com/..."} {
		if !strings.Contains(out, want) {
			t.Errorf("process log lost %q: %s", want, out)
		}
	}
	if strings.Contains(out, "SECRET123") {
		t.Fatalf("credential leaked: %s", out)
	}
	e := ring.Since(0, 0, slog.LevelInfo)[0]
	raw, _ := json.Marshal(e)
	if e.Attrs["asr.token"] != "[redacted]" || strings.Contains(string(raw), "SECRET123") || strings.Contains(string(raw), "s3cret") {
		t.Fatalf("ring = %s", raw)
	}
}

type secretValue string

func (s secretValue) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token", string(s)), slog.String("server", "http://audiosilo:8080"))
}

// Security: a secret inside a group (slog.Group, a LogValuer that resolves to
// one, a record attr under a group named for a secret) is redacted like a
// top-level one (denied), while its harmless siblings are kept (allowed).
func TestRedactsInsideGroups(t *testing.T) {
	t.Parallel()
	ring := New(10)
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), ring))
	log.Info("group", slog.Group("auth", "token", "s3cret-1", "user", "admin"))
	log.Info("valuer", "link", secretValue("s3cret-2"))
	log.WithGroup("token").Info("named group", "value", "s3cret-3")
	log.With(slog.Group("pairing", "code", "s3cret-4")).Info("with group")
	for _, e := range ring.Since(0, 0, slog.LevelInfo) {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "s3cret") {
			t.Errorf("a secret was kept: %s", raw)
		}
	}
	got := ring.Since(0, 0, slog.LevelInfo)
	if got[0].Attrs["auth.token"] != "[redacted]" || got[0].Attrs["auth.user"] != "admin" {
		t.Errorf("group attrs = %+v", got[0].Attrs)
	}
	if got[1].Attrs["link.token"] != "[redacted]" || got[1].Attrs["link.server"] != "http://audiosilo:8080" {
		t.Errorf("valuer attrs = %+v", got[1].Attrs)
	}
}

// An attribute is cut at a character's boundary, never inside one.
func TestAttrCutKeepsUTF8(t *testing.T) {
	t.Parallel()
	ring := New(2)
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), ring))
	log.Info("long", "path", strings.Repeat("a", maxAttrText-1)+"é")
	v := ring.Since(0, 0, slog.LevelInfo)[0].Attrs["path"]
	if !utf8.ValidString(v) || len(v) != maxAttrText-1 {
		t.Fatalf("cut to %d bytes, valid %v", len(v), utf8.ValidString(v))
	}
}
