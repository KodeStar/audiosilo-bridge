// Package logring keeps the Bridge's recent log lines for its own page: an
// slog.Handler that passes every record on and copies Info and up into a
// bounded ring, with the values of secret-sounding attributes redacted.
package logring

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Entry is one log line. Level encodes as its name ("INFO", "WARN", ...).
type Entry struct {
	ID      uint64            `json:"id"`
	Time    time.Time         `json:"time"`
	Level   slog.Level        `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Ring holds the newest entries in a fixed circular buffer.
type Ring struct {
	mu   sync.Mutex
	buf  []Entry
	head int // where the next entry goes
	n    int // how many entries are held
	next uint64
}

// New returns a ring of size entries.
func New(size int) *Ring { return &Ring{buf: make([]Entry, size)} }

func (r *Ring) add(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	e.ID = r.next
	r.buf[r.head] = e
	r.head = (r.head + 1) % len(r.buf)
	r.n = min(r.n+1, len(r.buf))
}

// Since returns up to limit entries after the id after (0: the newest limit),
// at or above minLevel, oldest first.
func (r *Ring) Since(after uint64, limit int, minLevel slog.Level) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Entry{}
	for i := range r.n {
		e := r.buf[(r.head-r.n+i+len(r.buf))%len(r.buf)]
		if e.ID > after && e.Level >= minLevel {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		if after == 0 {
			out = out[len(out)-limit:]
		} else {
			out = out[:limit]
		}
	}
	return out
}

// secretWords mark an attribute whose value is never kept.
var secretWords = []string{"token", "password", "code", "key", "secret", "cookie", "authorization"}

func redact(key string) bool {
	k := strings.ToLower(key)
	for _, w := range secretWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// Handler tees records into a Ring.
type Handler struct {
	next   slog.Handler
	ring   *Ring
	attrs  []slog.Attr
	groups []string
}

// NewHandler wraps next, copying Info-and-up records into ring.
func NewHandler(next slog.Handler, ring *Ring) *Handler { return &Handler{next: next, ring: ring} }

// Enabled reports whether either the wrapped handler or the ring wants level.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || h.next.Enabled(ctx, l)
}

// Handle passes the record on and keeps a copy.
func (h *Handler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelInfo {
		e := Entry{Time: rec.Time, Level: rec.Level, Message: rec.Message}
		add := func(key string, a slog.Attr) {
			if e.Attrs == nil {
				e.Attrs = map[string]string{}
			}
			if redact(a.Key) {
				e.Attrs[key] = "[redacted]"
				return
			}
			v := fmt.Sprint(a.Value.Resolve().Any())
			if len(v) > 500 {
				v = v[:500]
			}
			e.Attrs[key] = v
		}
		for _, a := range h.attrs {
			add(a.Key, a)
		}
		prefix := strings.Join(h.groups, ".")
		rec.Attrs(func(a slog.Attr) bool {
			key := a.Key
			if prefix != "" {
				key = prefix + "." + key
			}
			add(key, a)
			return true
		})
		h.ring.add(e)
	}
	if h.next.Enabled(ctx, rec.Level) {
		return h.next.Handle(ctx, rec)
	}
	return nil
}

// WithAttrs returns a handler carrying attrs (their keys under the groups open
// now).
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept := append([]slog.Attr(nil), h.attrs...)
	prefix := strings.Join(h.groups, ".")
	for _, a := range attrs {
		if prefix != "" {
			a.Key = prefix + "." + a.Key
		}
		kept = append(kept, a)
	}
	return &Handler{next: h.next.WithAttrs(attrs), ring: h.ring, attrs: kept, groups: h.groups}
}

// WithGroup returns a handler in group name.
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), ring: h.ring, attrs: h.attrs, groups: append(append([]string(nil), h.groups...), name)}
}
