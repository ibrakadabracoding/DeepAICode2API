package parallel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestForRunsEveryIndexOnce(t *testing.T) {
	const count = 50
	seen := make([]int32, count)
	For(t.Context(), 4, count, func(_ context.Context, i int) {
		atomic.AddInt32(&seen[i], 1)
	})
	for i, n := range seen {
		if n != 1 {
			t.Fatalf("下标 %d 执行 %d 次，期望 1", i, n)
		}
	}
}

// The concurrency limit must be honored: an atomic counter tracks the peak number of tasks
// running at once.
func TestForRespectsConcurrencyLimit(t *testing.T) {
	const limit, count = 3, 36
	var cur, peak int32
	For(t.Context(), limit, count, func(context.Context, int) {
		n := atomic.AddInt32(&cur, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		atomic.AddInt32(&cur, -1)
	})
	if peak > limit {
		t.Fatalf("并发峰值 %d 超过上限 %d", peak, limit)
	}
	if peak < 2 {
		t.Fatalf("并发峰值只有 %d，说明没有真正并行", peak)
	}
}

// No task is dispatched when ctx is already canceled.
func TestForSkipsEverythingWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var calls int32
	For(ctx, 4, 20, func(context.Context, int) {
		atomic.AddInt32(&calls, 1)
	})
	if calls != 0 {
		t.Fatalf("ctx 已取消，不应执行任何任务，实际 %d", calls)
	}
}

// Edge cases: count<=0 / workers<=0 must not panic.
func TestForEdgeCases(t *testing.T) {
	For(t.Context(), 4, 0, func(context.Context, int) { t.Fatal("不应执行") })
	For(t.Context(), 0, 3, func(context.Context, int) {})
}
