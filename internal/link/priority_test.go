package link

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestNegotiatedClaimPriority(t *testing.T) {
	raw, err := os.ReadFile("../podcast/testdata/phase2c2/wire-2b.json")
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Accepts  string `json:"accepts"`
		Priority int32  `json:"priority_interactive"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Accepts != AcceptClaimMinPriority {
		t.Fatalf("accepts mirror = %q, fixture = %q", AcceptClaimMinPriority, wire.Accepts)
	}
	f, c := newFake(t, 204, "")
	ctx := context.Background()
	if _, err := c.ClaimMinPriority(ctx, []string{"podcast.fetch"}, time.Second, wire.Priority); err == nil || f.last != nil {
		t.Fatal("unnegotiated filter reached server", err)
	}
	for _, accepts := range [][]string{nil, {"future"}, {"future", wire.Accepts}, nil} {
		body, _ := json.Marshal(map[string]any{"accepts": accepts, "future": true})
		f.status, f.body = 200, string(body)
		reply, err := c.Heartbeat(ctx, Status{})
		if err != nil {
			t.Fatal(err)
		}
		accepted := reply.Accepting(wire.Accepts)
		if c.SupportsClaimMinPriority() != accepted {
			t.Fatal("negotiation was not updated", accepts)
		}
		f.status, f.body = 204, ""
		if _, err := c.Claim(ctx, []string{"podcast.fetch"}, time.Hour); err != nil || f.last.URL.Query().Has("min_priority") {
			t.Fatal("ordinary claim changed", f.last.URL, err)
		}
		before := f.last
		_, err = c.ClaimMinPriority(ctx, []string{"podcast.fetch"}, time.Hour, wire.Priority)
		if accepted {
			q := f.last.URL.Query()
			if err != nil || q.Get("min_priority") != strconv.FormatInt(int64(wire.Priority), 10) || q.Get("wait") != "25" || q.Get("kind") != "podcast.fetch" {
				t.Fatal("filtered claim", q, err)
			}
			for _, minimum := range []int32{-2147483648, -10, 0, 2147483647} {
				if _, err := c.ClaimMinPriority(ctx, []string{"podcast.fetch"}, time.Second, minimum); err != nil || f.last.URL.Query().Get("min_priority") != strconv.FormatInt(int64(minimum), 10) {
					t.Fatal("signed int32 filter", minimum, f.last.URL, err)
				}
			}
		} else if err == nil || f.last != before {
			t.Fatal("filter sent without agreement", err)
		}
	}
	f.status, f.body = 200, `{ "libraries": [], "future": {} }`
	if reply, err := c.Heartbeat(ctx, Status{}); err != nil || reply.Accepting(wire.Accepts) || c.SupportsClaimMinPriority() {
		t.Fatal("missing optional accepts", reply, err)
	}
	other, _ := New("http://example.test", "new-pair", "test")
	if other.SupportsClaimMinPriority() {
		t.Fatal("a new pairing inherited negotiation")
	}
}
