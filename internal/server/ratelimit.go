package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
)

// keyIDOf maps a key to the rate-limit bucket key.
//
// Use fingerprint instead of plaintext: the rate limiter is a long-lived in-memory map, so there is no need to keep the full key in it.
func keyIDOf(key string) string { return deepai.KeyFingerprint(key) }

// --- Data plane: per-key rate limiting ---
//
// Background: keys issued externally by the gateway previously had only two states: valid / invalid. Any single key could
// exhaust the entire account pool - accounts in the pool are limited, upstream quota is issued daily, and a single runaway script is enough to
// make all callers hit 429 together. Two gates are added here, both disabled by default (0 = unlimited),
// upgrading will not change existing behavior:
//
// - Token bucket rate limiting (requests per second + burst capacity);
// - Per-key concurrency limit (in-flight requests).
//
// Both are isolated by the "key" dimension: rate limiting one group / one key will not affect other keys.

// keyRateLimiter is an in-process per-key rate limiter.
//
// Not persisted: the rate-limit window is on the order of seconds, resetting on restart is meaningless, and it avoids writing runtime state into the state file.
type keyRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	inflight map[string]int
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

const (
	// keyRateLimiterMaxBuckets is the upper limit on bucket table size: once exceeded, idle buckets are reclaimed before creating new ones.
	//
	// Buckets are indexed by key fingerprint; after key rotation / deletion, old buckets will not be automatically referenced; when size is unlimited,
	// a long-running gateway will keep the buckets of these "historical keys" in memory indefinitely.
	keyRateLimiterMaxBuckets = 512
	// keyRateLimiterIdleTTL is the idle reclaim duration for buckets.
	//
	// Much longer than the time required to fill a token bucket: a bucket idle for 30 minutes has long since refilled, so dropping it is equivalent to
	// "creating a new full bucket", and will not let extra requests through for callers.
	keyRateLimiterIdleTTL = 30 * time.Minute
)

func newKeyRateLimiter() *keyRateLimiter {
	return &keyRateLimiter{
		buckets:  map[string]*tokenBucket{},
		inflight: map[string]int{},
	}
}

// allow performs token bucket checks based on rps/burst; rps <= 0 means no rate limiting.
//
// Returns retryAfter for use in the 429 Retry-After header (rounded up to seconds, at least 1s).
func (l *keyRateLimiter) allow(id string, rps float64, burst int) (bool, time.Duration) {
	if rps <= 0 {
		return true, 0
	}
	if burst <= 0 {
		burst = int(rps)
		if burst < 2 {
			burst = 2
		}
	}

	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[id]
	if b == nil {
		// When the table is full, reclaim idle buckets first (holding the lock, see sweepLocked) to prevent key rotation from inflating memory.
		if len(l.buckets) >= keyRateLimiterMaxBuckets {
			l.sweepLocked(now)
		}
		b = &tokenBucket{tokens: float64(burst), last: now}
		l.buckets[id] = b
	}
	// Refill tokens based on elapsed time, capped at the bucket capacity.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rps
		if b.tokens > float64(burst) {
			b.tokens = float64(burst)
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// How many seconds remain until enough tokens accumulate for one token.
	wait := time.Duration((1 - b.tokens) / rps * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// sweepLocked reclaims buckets unused for a long time (caller holds the lock).
//
// Buckets with in-flight requests are not reclaimed: their tokens/last still participate in concurrency rate-limit decisions.
func (l *keyRateLimiter) sweepLocked(now time.Time) {
	for id, b := range l.buckets {
		if now.Sub(b.last) <= keyRateLimiterIdleTTL {
			continue
		}
		if _, busy := l.inflight[id]; busy {
			continue
		}
		delete(l.buckets, id)
	}
}

// acquire attempts to take a concurrency slot; limit <= 0 means unlimited.
func (l *keyRateLimiter) acquire(id string, limit int) bool {
	if limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[id] >= limit {
		return false
	}
	l.inflight[id]++
	return true
}

// release returns the concurrency slot.
func (l *keyRateLimiter) release(id string, limit int) {
	if limit <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[id] <= 1 {
		delete(l.inflight, id)
		return
	}
	l.inflight[id]--
}

// rateLimitGate performs rate-limit checks on new requests; once allowed, the concurrency slot is released when returning.
//
// Place it in withAuth (after key validation) rather than global middleware: rate limiting is isolated per key,
// Requests without a key (local bootstrap with no issued key) should not count against anyone's quota.
func (s *Server) rateLimitGate(keyID string) (release func(), ok bool, retryAfter time.Duration) {
	settings := s.Store.Settings()

	if allowed, wait := s.limiter.allow(keyID, settings.RateLimitRPS, settings.RateLimitBurst); !allowed {
		return nil, false, wait
	}
	if !s.limiter.acquire(keyID, settings.KeyConcurrency) {
		return nil, false, time.Second
	}
	limit := settings.KeyConcurrency
	return func() { s.limiter.release(keyID, limit) }, true, 0
}

// writeRateLimited writes 429 (OpenAI-compatible rate_limit_error).
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(retryAfter.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	httpx.WriteErr(w, http.StatusTooManyRequests,
		"请求过于频繁（该 API 密钥已触发限流），请在 "+strconv.Itoa(secs)+" 秒后重试", "rate_limit_error")
}
