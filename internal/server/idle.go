package server

import (
	"errors"
	"io"
	"sync"
	"time"
)

// upstreamIdleTimeout defines how long an upstream response can go without new data before it is considered a dead connection.
//
// Why it is required: the account slot (inflight) for a streaming response is returned only when the Body is closed, while streaming
// may last several minutes. If the upstream hangs after sending the first frame, the old implementation relied only on r.Context()
// -if the client never disconnects, it waits forever; the slot and connection are occupied indefinitely, and no log can tell
// whether it is "stuck" or "still generating".
//
// Basis for choosing 120s: inference p50 is about 1.1s, long tail is on the order of 10s; for deep-thinking models the first frame may
// take more than ten seconds. If there is not a single byte within 120s, it can basically only be that the connection is dead or the upstream silently dropped packets.
const upstreamIdleTimeout = 120 * time.Second

// upstreamFirstByteTimeout is the budget for waiting for the first byte of a **non-streaming** response.
//
// 2026-11-13 two online 502s (125.7s / 131.1s, both /v1/responses non-streaming)
// were mistakenly killed by the 120s above: a non-streaming long think can naturally exceed two minutes before the first byte,
// while the request's own budget is request_timeout (default 600s). Separate "waiting for the first byte" from
// "inter-frame idle": wait at most 300s for the first byte, and after that still judge dead by 120s.
//
// Streaming remains unchanged at 120s: if SSE does not give even the first frame within 120s, the slot stays occupied, which is more expensive.
const upstreamFirstByteTimeout = 300 * time.Second

// errUpstreamIdle is the error reported to the upper layer after an idle timeout.
//
// Define a separate value: callers use it to record the request as upstream_error instead of "read EOF",
// so the console's last_err can show at a glance that the gateway actively ended it.
var errUpstreamIdle = errors.New("上游响应空闲超时（" + upstreamIdleTimeout.String() + " 无数据）")

// idleTimeoutBody closes the underlying Body when there is no successful read within d.
//
// Closing the underlying Body is currently the only reliable way to interrupt: net/http's response body does not expose
// SetReadDeadline, and internally transport uses exactly "cancel ctx → Close Body" to interrupt
// in-flight reads, so concurrent Close and Read is the pattern it supports.
type idleTimeoutBody struct {
	rc io.ReadCloser
	// idle is the inter-frame idle budget after receiving the first byte; first is the first-byte budget.
	idle  time.Duration
	first time.Duration
	timer *time.Timer

	mu       sync.Mutex
	closed   bool
	timedOut bool
}

// newIdleTimeoutBody wraps an idle timeout; when idle <= 0, return rc as-is.
//
// first is the first-byte budget (falls back to idle when <= 0): non-streaming requests use it to avoid "long thinking being treated as idle",
// for streaming, just pass idle. After receiving the first byte, switch back to idle uniformly.
func newIdleTimeoutBody(rc io.ReadCloser, idle, first time.Duration) io.ReadCloser {
	if rc == nil || idle <= 0 {
		return rc
	}
	if first <= 0 {
		first = idle
	}
	b := &idleTimeoutBody{rc: rc, idle: idle, first: first}
	b.timer = time.AfterFunc(first, b.onIdle)
	return b
}

// onIdle is triggered by the timer: mark the timeout and close the underlying connection so that a blocked Read returns immediately.
func (b *idleTimeoutBody) onIdle() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.timedOut = true
	b.mu.Unlock()
	_ = b.rc.Close()
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		// After the first byte, switch from first back to idle; if the timeout has already fired, the connection has been closed and is not revived.
		b.mu.Lock()
		if !b.closed && !b.timedOut {
			b.timer.Reset(b.idle)
		}
		b.mu.Unlock()
	}
	if err != nil {
		b.mu.Lock()
		timedOut := b.timedOut
		b.mu.Unlock()
		if timedOut {
			return n, errUpstreamIdle
		}
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.timer.Stop()
	return b.rc.Close()
}

// TimedOut reports whether this read was terminated due to an idle timeout.
func (b *idleTimeoutBody) TimedOut() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.timedOut
}
