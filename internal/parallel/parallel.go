// Package parallel provides bounded-concurrency batch execution.
//
// The gateway has 6 places that "run a batch of tasks with fixed concurrency":
// bulk registration, ledger catch-up, quota probing, console health checks, proxy
// connectivity checks, and email re-verification. Each hand-rolled a jobs channel +
// WaitGroup, and their cancellation semantics and result collection differed. After
// unifying on For, the concurrency limit, ctx cancellation, and writing results back
// by index are implemented in exactly one place.
package parallel

import (
	"context"
	"sync"
)

// For runs fn(0)...fn(count-1) with at most workers concurrent executions.
//
// Semantics:
//   - workers<=0 is treated as 1; workers>count is clamped to count;
//   - after ctx is canceled no new tasks are dispatched, but started tasks run to completion;
//   - when For returns, all dispatched tasks have finished, so the caller may safely read result slices;
//   - fn's writes must target their own index (or use separate synchronization) to avoid result races.
func For(ctx context.Context, workers, count int, fn func(ctx context.Context, i int)) {
	if count <= 0 || fn == nil {
		return
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > count {
		workers = count
	}
	// Dispatch nothing when already canceled.
	//
	// Relying only on the select in the dispatch loop is insufficient: ctx.Done()
	// and jobs<-i may both be ready, and Go picks one at random, so a canceled ctx
	// would still run some tasks.
	if ctx.Err() != nil {
		return
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				fn(ctx, i)
			}
		}()
	}

	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
}
