package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/link"
)

// boundedServer answers the heartbeat as a server with a body cap and a library
// count cap (an older server: 64 KiB and 512), listing libs. It refuses as the
// server does, a 400 with no code: "invalid status" for a body over the cap (a
// body it couldn't read), "invalid: too many libraries" for a list over it.
// refuseSetup: it lists setup in accepts, then refuses a beat carrying it,
// with setupRefusal ("" is "invalid status": a server that no longer knows
// the field).
type boundedServer struct {
	maxBytes, maxLibs int
	refuseSetup       bool
	setupRefusal      string
	libs              []link.Library
	mu                sync.Mutex
	sizes             []int // libraries per beat
	codes             []int
}

func (f *boundedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/bridge/pair" {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"tok","server_id":"srv-1","server_name":"Home"}`)
		return
	}
	if r.URL.Path != "/api/v1/bridge/status" {
		http.NotFound(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var st link.Status
	_ = json.Unmarshal(raw, &st)
	refusal := ""
	switch {
	case len(raw) > f.maxBytes:
		refusal = "invalid status"
	case f.refuseSetup && st.Setup != nil:
		refusal = cmp.Or(f.setupRefusal, "invalid status")
	case len(st.Libraries) > f.maxLibs:
		refusal = "invalid: too many libraries"
	}
	code := http.StatusOK
	if refusal != "" {
		code = http.StatusBadRequest
	}
	f.mu.Lock()
	f.sizes = append(f.sizes, len(st.Libraries))
	f.codes = append(f.codes, code)
	f.mu.Unlock()
	if refusal != "" {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": refusal})
		return
	}
	reply := link.StatusReply{ServerTime: "now", Libraries: f.libs}
	if f.refuseSetup {
		reply.Accepts = []string{link.AcceptSetup}
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *boundedServer) beats() (sizes, codes []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.sizes...), append([]int(nil), f.codes...)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// boundedBridge pairs a Bridge with f and gives it a checked mapping for each
// of f's libraries (none found here, so each carries an error).
func boundedBridge(t *testing.T, f *boundedServer, n int) (*Bridge, context.Context, *lockedBuffer) {
	t.Helper()
	for i := 1; i <= n; i++ {
		f.libs = append(f.libs, link.Library{ID: int64(i), Name: fmt.Sprintf("Library %d", i),
			Root: fmt.Sprintf("/nonexistent-audiosilo-test/%04d-%s", i, strings.Repeat("x", 60)), Kind: "books"})
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	logs := &lockedBuffer{}
	b, _, err := New(context.Background(), Options{DataDir: t.TempDir(), Version: "0.1.0", Listen: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.db.Close() })
	// Cancelled before the database closes: a mapping check a beat starts
	// stops with it.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := b.Pair(ctx, srv.URL, "CODE"); err != nil {
		t.Fatal(err)
	}
	drainKick(b)
	b.libs.SetLibraries(f.libs)
	b.libs.Check(ctx, nil, b.mount, time.Now)
	if got := len(b.libs.Statuses()); got != n {
		t.Fatalf("%d mappings checked, want %d", got, n)
	}
	return b, ctx, logs
}

const fitLog = "refused the heartbeat's library list for its size"

// server#445: a list an older server refuses for its size goes again at once
// cut to fit, so the Bridge comes online and learns the server's libraries;
// later beats send it cut, the whole list is tried again after an hour, and
// the cut is logged once.
func TestHeartbeatRefusedForSizeSendsAListThatFits(t *testing.T) {
	f := &boundedServer{maxBytes: link.LegacyStatusBytes, maxLibs: link.LegacyStatusLibraries}
	b, ctx, logs := boundedBridge(t, f, 600)
	f.libs = append(f.libs, link.Library{ID: 601, Name: "New", Root: "/nonexistent-audiosilo-test/new", Kind: "books"})

	b.beat(ctx)
	sizes, codes := f.beats()
	if len(codes) != 2 || codes[0] != http.StatusBadRequest || codes[1] != http.StatusOK {
		t.Fatalf("codes = %v, want a refusal then the cut list taken", codes)
	}
	if sizes[0] != 600 || sizes[1] == 0 || sizes[1] >= 600 {
		t.Fatalf("libraries sent = %v", sizes)
	}
	b.mu.Lock()
	online, beatErr := !b.lastBeat.IsZero(), b.beatErr
	b.mu.Unlock()
	if !online || beatErr != "" {
		t.Fatalf("the Bridge is not online after the cut list went through (err %q)", beatErr)
	}
	if _, ok := b.libs.Library(601); !ok {
		t.Fatal("the reply's libraries were not recorded")
	}
	// The new library starts a mapping check; it is done before the next beat.
	waitFor(t, "the new library's check", func() bool { _, ok := b.libs.Status(601); return ok })
	waitFor(t, "the check's beat", func() bool { return drainKick(b) })

	b.beat(ctx) // within the hour: the cut list at once
	sizes, codes = f.beats()
	if len(codes) != 3 || codes[2] != http.StatusOK || sizes[2] >= 601 {
		t.Fatalf("the next beat: sizes %v, codes %v, want the cut list once", sizes, codes)
	}

	b.mu.Lock()
	b.libsFitAt = time.Now().Add(-2 * setupRefusedFor)
	b.mu.Unlock()
	b.beat(ctx) // an hour on: the whole list again, refused, then cut
	_, codes = f.beats()
	if len(codes) != 5 || codes[3] != http.StatusBadRequest || codes[4] != http.StatusOK {
		t.Fatalf("after the hour: codes %v, want the whole list tried again", codes)
	}
	if n := logs.count(fitLog); n != 1 {
		t.Fatalf("the cut was logged %d times, want once", n)
	}
}

// A newer server takes the whole list: nothing is cut or sent twice.
func TestHeartbeatWholeListToAServerThatTakesIt(t *testing.T) {
	f := &boundedServer{maxBytes: 2 << 20, maxLibs: 4096}
	b, ctx, logs := boundedBridge(t, f, 600)
	b.beat(ctx)
	sizes, codes := f.beats()
	if len(codes) != 1 || codes[0] != http.StatusOK || sizes[0] != 600 {
		t.Fatalf("sizes %v, codes %v, want the whole list once", sizes, codes)
	}
	if logs.count(fitLog) != 0 {
		t.Fatal("logged a cut that never happened")
	}
}

// A 400 on a list that already fits isn't about its size: the beat isn't sent
// again cut, and the Bridge stays offline with the server's answer.
func TestHeartbeatRefusedNotForSizeIsNotResent(t *testing.T) {
	f := &boundedServer{maxBytes: link.LegacyStatusBytes, maxLibs: 0}
	b, ctx, _ := boundedBridge(t, f, 3)
	b.beat(ctx)
	_, codes := f.beats()
	if len(codes) != 1 || codes[0] != http.StatusBadRequest {
		t.Fatalf("codes %v, want one refused beat", codes)
	}
	b.mu.Lock()
	beatErr, fitAt := b.beatErr, b.libsFitAt
	b.mu.Unlock()
	if beatErr == "" || !fitAt.IsZero() {
		t.Fatalf("beatErr %q, libsFitAt %v", beatErr, fitAt)
	}
}

// A refusal that lasts until the setup block goes isn't known to be the
// size's, though the list went cut by then: the cut isn't kept or logged, and
// the next beat sends the whole list, which this server takes.
func TestHeartbeatRefusedForItsSetupDoesNotKeepTheCut(t *testing.T) {
	f := &boundedServer{maxBytes: 2 << 20, maxLibs: 4096, refuseSetup: true}
	b, ctx, logs := boundedBridge(t, f, 600)
	b.beat(ctx) // the whole list, no setup yet: accepts listed
	drainKick(b)
	b.beat(ctx) // with setup: refused whole, refused cut, taken cut without it
	sizes, codes := f.beats()
	if len(codes) != 4 || codes[1] != http.StatusBadRequest || codes[2] != http.StatusBadRequest || codes[3] != http.StatusOK ||
		sizes[1] != 600 || sizes[2] >= 600 {
		t.Fatalf("sizes %v, codes %v, want the setup beat refused whole and cut, then taken", sizes, codes)
	}
	b.mu.Lock()
	fitAt := b.libsFitAt
	b.mu.Unlock()
	if !fitAt.IsZero() || logs.count(fitLog) != 0 {
		t.Fatalf("the cut was kept (libsFitAt %v) or logged (%d)", fitAt, logs.count(fitLog))
	}
	b.beat(ctx)
	sizes, codes = f.beats()
	if len(codes) != 5 || codes[4] != http.StatusOK || sizes[4] != 600 {
		t.Fatalf("the next beat: sizes %v, codes %v, want the whole list taken", sizes, codes)
	}
}

// A 400 that names something other than the size takes the setup path though
// the list is over an older server's bounds: the whole list goes again
// without the setup block, nothing is cut, and the warning names the setup
// block (a server that takes 2 MiB and 4096 libraries).
func TestHeartbeatRefusedForABadSetupBlockKeepsTheWholeList(t *testing.T) {
	f := &boundedServer{maxBytes: 2 << 20, maxLibs: 4096, refuseSetup: true, setupRefusal: "invalid: setup"}
	b, ctx, logs := boundedBridge(t, f, 600)
	b.beat(ctx) // the whole list, no setup yet: accepts listed
	drainKick(b)
	b.beat(ctx) // with setup: refused, then taken whole without it
	sizes, codes := f.beats()
	if len(codes) != 3 || codes[1] != http.StatusBadRequest || codes[2] != http.StatusOK || sizes[1] != 600 || sizes[2] != 600 {
		t.Fatalf("sizes %v, codes %v, want the whole list refused with setup, then taken without", sizes, codes)
	}
	b.mu.Lock()
	fitAt, refusedAt := b.libsFitAt, b.setupRefusedAt
	b.mu.Unlock()
	if !fitAt.IsZero() || refusedAt.IsZero() || logs.count(fitLog) != 0 || logs.count("refused the heartbeat's setup block") != 1 {
		t.Fatalf("libsFitAt %v, setupRefusedAt %v, cut logged %d times", fitAt, refusedAt, logs.count(fitLog))
	}
}

// An older server's answer to a list over its count ("too many libraries",
// its body read) is the size's too: the list goes again cut to fit.
func TestHeartbeatRefusedForTooManyLibraries(t *testing.T) {
	f := &boundedServer{maxBytes: 2 << 20, maxLibs: link.LegacyStatusLibraries}
	b, ctx, logs := boundedBridge(t, f, 600)
	b.beat(ctx)
	sizes, codes := f.beats()
	if len(codes) != 2 || codes[0] != http.StatusBadRequest || codes[1] != http.StatusOK ||
		sizes[1] == 0 || sizes[1] > link.LegacyStatusLibraries {
		t.Fatalf("sizes %v, codes %v, want the list refused, then cut and taken", sizes, codes)
	}
	b.mu.Lock()
	fitAt := b.libsFitAt
	b.mu.Unlock()
	if fitAt.IsZero() || logs.count(fitLog) != 1 {
		t.Fatalf("libsFitAt %v, cut logged %d times", fitAt, logs.count(fitLog))
	}
}
