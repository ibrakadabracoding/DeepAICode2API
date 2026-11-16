// Package retry provides backoff retry based on "transient error" classification.
//
// The gateway has several "retry on failure" paths (/theos liveness checks, quota
// probing on pool entry, proxy connectivity checks). They share the same predicate
// (deepai.IsTransient) but each hand-rolled a backoff loop, and a change could easily
// miss one of them. After unifying here, "how many retries, how long each wait, and
// which errors are not retried" is defined in exactly one place.
package retry

import (
	"context"
	"time"
)

// Classifier reports whether an error is worth retrying.
type Classifier func(error) bool

// Do runs fn; when it fails and retryable says it is retryable, back off according to
// delays and retry.
//
// Semantics:
//   - empty delays means a single attempt;
//   - if the first failure is classified as non-retryable, return that error immediately;
//   - ctx is explicitly passed to fn so each attempt can set its own timeout;
//   - if ctx expires during backoff, return the last error (no new attempt is made).
//
// A nil return means some attempt succeeded.
func Do(ctx context.Context, delays []time.Duration, retryable Classifier, fn func(context.Context) error) error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 && !sleep(ctx, delays[attempt-1]) {
			return lastErr
		}
		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if retryable != nil && !retryable(err) {
			return err
		}
		if attempt >= len(delays) {
			return lastErr
		}
	}
}

// sleep waits for d and returns false when ctx expires.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
