package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-bridge/internal/workerproto"
)

// The client of the Bridge's worker routes.

// ErrProtocol: the Bridge speaks another protocol version. The Bridge's other
// refusals come back as workerproto's errors (ErrUnauthorized, ErrNotHeld,
// ErrUnknownTask, ErrInvalidResult); any other error is a transport or server
// error, to retry.
var ErrProtocol = errors.New("the Bridge speaks another worker protocol")

type client struct {
	base    *url.URL
	token   string
	agent   string
	http    *http.Client
	bridgeV int // the protocol the Bridge answered on a mismatch
}

func newClient(bridgeURL, token, version string, hc *http.Client) (*client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(bridgeURL), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("--bridge must be the Bridge page's http(s) address, e.g. http://192.168.1.20:8091")
	}
	if hc == nil {
		// No overall timeout: a long poll and a big FLAC each bound themselves.
		hc = &http.Client{}
	}
	return &client{base: u, token: token, agent: "AudioSilo Bridge worker/" + version, http: hc}, nil
}

// route fills a workerproto route's {id}.
func (c *client) route(route, taskID string, q url.Values) string {
	u := *c.base
	// Appended to the address's path as written, so a proxy's prefix reaches
	// it byte for byte (a "(" or "!" stays one rather than becoming %28, %21).
	raw := c.base.EscapedPath() + strings.ReplaceAll(route, "{id}", url.PathEscape(taskID))
	u.Path, _ = url.PathUnescape(raw) // valid escapes by construction (EscapedPath, PathEscape)
	u.RawPath = raw
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func (c *client) do(ctx context.Context, method, target string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(workerproto.HeaderProtocol, strconv.Itoa(workerproto.Version))
	req.Header.Set("User-Agent", c.agent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var eb workerproto.ErrorBody
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&eb)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, workerproto.ErrUnauthorized
	case eb.Code == workerproto.CodeProtocolMismatch:
		c.bridgeV = eb.Protocol
		return nil, ErrProtocol
	case eb.Code == workerproto.CodeNotHeld:
		return nil, workerproto.ErrNotHeld
	case eb.Code == workerproto.CodeUnknownTask:
		return nil, workerproto.ErrUnknownTask
	case eb.Code == workerproto.CodeInvalidResult:
		return nil, workerproto.ErrInvalidResult
	}
	return nil, fmt.Errorf("%s %s: %s %s", method, req.URL.Path, resp.Status, eb.Error)
}

func (c *client) postJSON(ctx context.Context, route, taskID string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, c.route(route, taskID, nil), bytes.NewReader(b), "application/json")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

func (c *client) hello(ctx context.Context, h workerproto.Hello) (workerproto.HelloReply, error) {
	var reply workerproto.HelloReply
	err := c.postJSON(ctx, workerproto.RouteHello, "", h, &reply)
	if err == nil && reply.Protocol != workerproto.Version {
		c.bridgeV = reply.Protocol
		return reply, ErrProtocol
	}
	return reply, err
}

// next long-polls for a task (nil when none came).
func (c *client) next(ctx context.Context) (*workerproto.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, (workerproto.MaxWaitSeconds+30)*time.Second)
	defer cancel()
	q := url.Values{"wait": {strconv.Itoa(workerproto.MaxWaitSeconds)}}
	resp, err := c.do(ctx, http.MethodGet, c.route(workerproto.RouteNext, "", q), nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var t workerproto.Task
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&t); err != nil {
		return nil, err
	}
	if t.ID == "" || strings.ContainsAny(t.ID, `/\.`) {
		return nil, fmt.Errorf("the Bridge sent a task with a bad id %q", t.ID)
	}
	return &t, nil
}

// download saves a task's FLAC to dest, checking its size and sha256.
func (c *client) download(ctx context.Context, t workerproto.Task, dest string) error {
	q := url.Values{"attempt": {strconv.Itoa(t.Attempt)}}
	resp, err := c.do(ctx, http.MethodGet, c.route(workerproto.RouteAudio, t.ID, q), nil, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // the worker's own spool
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, t.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && (n != t.Size || hex.EncodeToString(h.Sum(nil)) != t.SHA256) {
		err = fmt.Errorf("the segment's audio arrived damaged (%d of %d bytes, or a different sha256)", n, t.Size)
	}
	if err != nil {
		_ = os.Remove(part)
		return err
	}
	return os.Rename(part, dest)
}

// heartbeat renews the lease and answers the attempt now held.
func (c *client) heartbeat(ctx context.Context, taskID string, attempt int) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var reply workerproto.HeartbeatReply
	if err := c.postJSON(ctx, workerproto.RouteHeartbeat, taskID, workerproto.Heartbeat{Attempt: attempt}, &reply); err != nil {
		return 0, err
	}
	return reply.Attempt, nil
}

// result uploads a raw transcript.
func (c *client) result(ctx context.Context, taskID string, meta workerproto.ResultMeta, raw []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	resp, err := c.do(ctx, http.MethodPost, c.route(workerproto.RouteResult, taskID, meta.Query()), bytes.NewReader(raw), "application/json")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// fail reports a decode that failed.
func (c *client) fail(ctx context.Context, taskID string, f workerproto.Fail) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.postJSON(ctx, workerproto.RouteFail, taskID, f, nil)
}
