// Package workerproto is the wire protocol between the Bridge and its ASR
// workers (BRIDGE-PLAN.md Phase 1b): the routes, the version both sides must
// share, and the JSON they exchange. It is a leaf, shared by the Bridge's
// worker routes (internal/api), its dispatcher (internal/workers) and the
// worker itself (internal/worker), so the two sides can't drift.
//
// The worker always connects out to the Bridge, never the reverse. Every call
// carries the worker's token (Authorization: Bearer) and the protocol version
// (HeaderProtocol); the Bridge refuses any call whose version isn't Version with
// 409 CodeProtocolMismatch, and a worker refuses a Bridge that answers another
// version.
//
// The unit of work is a SEGMENT, not a chapter: a 16 kHz mono FLAC, its start
// in the book and an opaque id. A chapter is one kind of segment; Phase 1d's
// duration windows for chapterless books are another, through the same worker
// with no version bump.
package workerproto

import (
	"errors"
	"net/url"
	"strconv"
	"time"
)

// Version is the protocol version. Bump it only for a change an installed
// worker can't follow: every installed worker is refused until it updates.
// Adding a field is not such a change (both sides ignore what they don't know).
const Version = 1

// HeaderProtocol carries the protocol version on every worker call.
const HeaderProtocol = "X-AudioSilo-Worker-Protocol"

// Prefix is where the worker routes live on the Bridge's page server.
const Prefix = "/worker/v1"

// The routes, relative to the Bridge's address. {id} is a task id.
const (
	RouteHello     = Prefix + "/hello"                // POST Hello -> HelloReply
	RouteNext      = Prefix + "/tasks/next"           // GET ?wait=<seconds> -> Task | 204
	RouteAudio     = Prefix + "/tasks/{id}/audio"     // GET ?attempt= -> the FLAC
	RouteHeartbeat = Prefix + "/tasks/{id}/heartbeat" // POST Heartbeat -> HeartbeatReply
	RouteResult    = Prefix + "/tasks/{id}/result"    // POST the raw transcript, ResultMeta in the query -> 204
	RouteFail      = Prefix + "/tasks/{id}/fail"      // POST Fail -> 204
)

// Error codes the Bridge answers with ({"error": "...", "code": "..."}).
const (
	// CodeProtocolMismatch (409): the worker speaks another Version. The reply
	// carries the Bridge's version in "protocol".
	CodeProtocolMismatch = "protocol_mismatch"
	// CodeNotHeld (409): the task isn't this worker's at that attempt any more
	// (its lease ran out and it went to another worker, it was cancelled, or
	// it is done). Stop working on it.
	CodeNotHeld = "not_held"
	// CodeUnknownTask (404): the Bridge has no such task now. It may only be
	// restarting: a result for it is kept and offered again for a while.
	CodeUnknownTask = "unknown_task"
	// CodeInvalidResult (422): the transcript isn't a complete one.
	CodeInvalidResult = "invalid_result"
)

// The Bridge's answers as errors: the dispatcher returns them and the routes
// map them (ErrUnauthorized 401, ErrUnknownTask 404 CodeUnknownTask, ErrNotHeld
// 409 CodeNotHeld, ErrInvalidResult 422 CodeInvalidResult).
var (
	ErrUnauthorized  = errors.New("unknown worker token")
	ErrUnknownTask   = errors.New("no such task")
	ErrNotHeld       = errors.New("the task isn't this worker's")
	ErrInvalidResult = errors.New("not a complete transcript")
)

// MaxWaitSeconds bounds a long poll for a task.
const MaxWaitSeconds = 25

// LeaseDuration is how long a claim holds a task without a heartbeat; a
// worker beats every HeartbeatEvery, well inside it. A lease that runs out puts
// the task back in the queue for the next worker (or the Bridge's own ASR).
const (
	LeaseDuration  = 60 * time.Second
	HeartbeatEvery = 15 * time.Second
)

// MaxResultBytes caps a transcript upload (a long chapter's word-timestamped
// JSON is a few MB).
const MaxResultBytes = 64 << 20

// MaxTextBytes caps the short free-text fields (names, versions, errors).
const MaxTextBytes = 512

// Hello is what a worker says when it starts, and again whenever the Bridge
// stops knowing it (a restart): who it is and what it runs.
type Hello struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	// Platform is GOOS/GOARCH, e.g. darwin/arm64.
	Platform string `json:"platform"`
	// Backend, Device and Model are the speech recognition it runs
	// (mlx-whisper on metal, whisper-cpp on cpu or cuda, and the model).
	Backend string `json:"backend"`
	Device  string `json:"device"`
	Model   string `json:"model"`
}

// HelloReply is the Bridge's answer: its own protocol and version, and the
// name the worker was given when its token was made.
type HelloReply struct {
	Protocol      int    `json:"protocol"`
	BridgeVersion string `json:"bridge_version"`
	WorkerID      int64  `json:"worker_id"`
	Name          string `json:"name"`
}

// Task is one segment to transcribe.
type Task struct {
	// ID is opaque. The same segment with the same options has the same id
	// (a Bridge that restarts asks for it again under it), so a result a worker
	// finished while the Bridge was down is still taken.
	ID      string `json:"id"`
	Attempt int    `json:"attempt"`
	// Start is the segment's start in the book, in seconds (informational: the
	// transcript's times stay relative to the segment), Duration its length.
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
	// Size and SHA256 describe the FLAC at RouteAudio.
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Language, NoContext and InitialPrompt are the decode options
	// (asr.Job's).
	Language      string `json:"language"`
	NoContext     bool   `json:"no_context"`
	InitialPrompt string `json:"initial_prompt,omitempty"`
}

// Heartbeat renews a task's lease while it is decoded.
type Heartbeat struct {
	Attempt int `json:"attempt"`
}

// HeartbeatReply is the attempt the worker holds the task under now: the one
// it sent, or a new one when the Bridge gave it the task again (a Bridge that
// restarted asks for the same segment, and the worker decoding it keeps it).
// A task the worker no longer holds is a 409 CodeNotHeld instead: stop.
type HeartbeatReply struct {
	Attempt int `json:"attempt"`
}

// ResultMeta describes a finished transcript. The transcript itself is the
// request body of RouteResult, the backend's raw output byte for byte (it may
// hold NaN, which is not JSON, so it never rides inside a JSON envelope); this
// travels in the query (Query and ParseResultMeta).
type ResultMeta struct {
	Attempt int
	Backend string
	Device  string
	Model   string
	// DecodeSeconds is how long the decode took on the worker.
	DecodeSeconds float64
}

// Query encodes m for RouteResult's query string.
func (m ResultMeta) Query() url.Values {
	return url.Values{
		"attempt": {strconv.Itoa(m.Attempt)}, "backend": {m.Backend}, "device": {m.Device}, "model": {m.Model},
		"decode_seconds": {strconv.FormatFloat(m.DecodeSeconds, 'f', 1, 64)},
	}
}

// maxDecodeSeconds bounds a reported decode time (a week): it is summed into
// the worker's row, and an infinite or overflowing sum would make the workers
// list unencodable as JSON.
const maxDecodeSeconds = 7 * 24 * 60 * 60

// ParseResultMeta reads RouteResult's query string. A missing or malformed
// number reads as 0 (an attempt of 0 matches no claim), and so does a decode
// time that is negative, not finite or beyond maxDecodeSeconds.
func ParseResultMeta(q url.Values) ResultMeta {
	attempt, _ := strconv.Atoi(q.Get("attempt"))
	secs, _ := strconv.ParseFloat(q.Get("decode_seconds"), 64)
	if !(secs >= 0 && secs <= maxDecodeSeconds) { // NaN fails both
		secs = 0
	}
	return ResultMeta{Attempt: attempt, Backend: q.Get("backend"), Device: q.Get("device"), Model: q.Get("model"), DecodeSeconds: secs}
}

// Fail reports a decode that failed: the segment fails on the Bridge with
// Error, as a local decode error would (Retry on the Bridge's page tries the
// book again).
type Fail struct {
	Attempt int    `json:"attempt"`
	Error   string `json:"error"`
}

// ErrorBody is the shape of every error answer.
type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
	// Protocol is the Bridge's version, on CodeProtocolMismatch.
	Protocol int `json:"protocol,omitempty"`
}
