package server

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// slowBody is a ReadCloser that can feed bytes at the test's pace.
//
// Use a channel instead of time.Sleep: the test must prove "timeout if no data within X milliseconds",
// let Read block on the channel itself, and let idleTimeoutBody handle the timing.
type slowBody struct {
	ch     chan []byte
	once   sync.Once
	closed chan struct{}
}

func newSlowBody() *slowBody {
	return &slowBody{ch: make(chan []byte, 4), closed: make(chan struct{})}
}

func (b *slowBody) Read(p []byte) (int, error) {
	select {
	case data, ok := <-b.ch:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, data), nil
	case <-b.closed:
		return 0, errors.New("closed")
	}
}

func (b *slowBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// readAsync reads once in the background and sends the result back over a channel (Read blocks until timeout).
func readAsync(body io.Reader) <-chan error {
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		_, err := body.Read(buf)
		done <- err
	}()
	return done
}

// TestIdleTimeoutAllowsSlowFirstByte pins down "non-streaming long thinking is not treated as idle":
// when the first byte exceeds the inter-frame idle budget but is still within the first-byte budget, it must be allowed through.
func TestIdleTimeoutAllowsSlowFirstByte(t *testing.T) {
	src := newSlowBody()
	body := newIdleTimeoutBody(src, 20*time.Millisecond, 300*time.Millisecond)
	defer body.Close()

	// 80ms > idle(20ms), but < first(300ms): this used to be judged as dead here.
	time.Sleep(80 * time.Millisecond)
	src.ch <- []byte("a")

	select {
	case err := <-readAsync(body):
		if err != nil {
			t.Fatalf("首字节在首字节预算内，不应超时: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("首字节读取卡住")
	}
}

// TestIdleTimeoutAfterFirstByte pins down "switch back to the inter-frame idle budget after the first byte is received":
// silence exceeding idle after the first byte must be finalized.
func TestIdleTimeoutAfterFirstByte(t *testing.T) {
	src := newSlowBody()
	body := newIdleTimeoutBody(src, 20*time.Millisecond, 500*time.Millisecond)
	defer body.Close()

	src.ch <- []byte("a")
	if err := <-readAsync(body); err != nil {
		t.Fatalf("首字节不应超时: %v", err)
	}

	select {
	case err := <-readAsync(body):
		if !errors.Is(err, errUpstreamIdle) {
			t.Fatalf("帧间静默应报告 errUpstreamIdle，实际 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("帧间空闲没有触发超时")
	}
}

// TestIdleTimeoutFirstByteBudget pins down "first-byte budget exhausted": the upstream does not even provide the first byte.
func TestIdleTimeoutFirstByteBudget(t *testing.T) {
	src := newSlowBody()
	body := newIdleTimeoutBody(src, 20*time.Millisecond, 30*time.Millisecond)
	defer body.Close()

	select {
	case err := <-readAsync(body):
		if !errors.Is(err, errUpstreamIdle) {
			t.Fatalf("首字节超预算应报告 errUpstreamIdle，实际 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("首字节预算没有触发超时")
	}
}

// TestHoldResponseBodyBudgets pins down "streaming and non-streaming use different first-byte budgets".
func TestHoldResponseBodyBudgets(t *testing.T) {
	s := &Server{inflight: map[string]int{}}

	streamBody := s.holdResponseBody(newSlowBody(), "acct", true)
	defer streamBody.Close()
	if b, ok := streamBody.(*idleTimeoutBody); !ok {
		t.Fatalf("应返回 idleTimeoutBody，实际 %T", streamBody)
	} else if b.first != upstreamIdleTimeout {
		t.Fatalf("流式首字节预算 = %v，期望 %v", b.first, upstreamIdleTimeout)
	}

	blockBody := s.holdResponseBody(newSlowBody(), "acct", false)
	defer blockBody.Close()
	if b, ok := blockBody.(*idleTimeoutBody); !ok {
		t.Fatalf("应返回 idleTimeoutBody，实际 %T", blockBody)
	} else if b.first != upstreamFirstByteTimeout {
		t.Fatalf("非流式首字节预算 = %v，期望 %v", b.first, upstreamFirstByteTimeout)
	} else if b.idle != upstreamIdleTimeout {
		t.Fatalf("非流式帧间预算 = %v，期望 %v", b.idle, upstreamIdleTimeout)
	}
}
