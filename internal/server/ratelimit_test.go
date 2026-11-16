package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"deepaicode2api/internal/store"
)

// chatUpstreamOK returns a minimal chat response that always succeeds.
func chatUpstreamOK(t *testing.T) *upstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"gen-1","choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
}

const chatBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`

// TestRateLimitDisabledByDefault guards "upgrading does not change existing behavior":
// When rate limiting is not configured, consecutive requests must all succeed.
func TestRateLimitDisabledByDefault(t *testing.T) {
	s, key := newTestServer(t, chatUpstreamOK(t))
	for i := 0; i < 10; i++ {
		rec := do(t, s, http.MethodPost, "/v1/chat/completions", key, chatBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求 = %d：%s", i, rec.Code, rec.Body.String())
		}
	}
}

// TestRateLimitPerKey verifies the token bucket applies per key, and 429 includes Retry-After.
func TestRateLimitPerKey(t *testing.T) {
	s, key := newTestServer(t, chatUpstreamOK(t))
	s.Store.UpdateSettings(func(st *store.Settings) {
		st.RateLimitRPS = 1
		st.RateLimitBurst = 1
	})

	first := do(t, s, http.MethodPost, "/v1/chat/completions", key, chatBody)
	if first.Code != http.StatusOK {
		t.Fatalf("第一次请求应放行，实际 %d：%s", first.Code, first.Body.String())
	}
	second := do(t, s, http.MethodPost, "/v1/chat/completions", key, chatBody)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("第二次请求应被限流，实际 %d：%s", second.Code, second.Body.String())
	}
	if ra := second.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("429 必须带正的 Retry-After，实际 %q", ra)
	}
	if body := second.Body.String(); !strings.Contains(body, "rate_limit_error") {
		t.Fatalf("429 响应体应为 rate_limit_error：%s", body)
	}

	// Another key is unaffected: rate limiting is per-key, not global.
	other := s.Store.AddKey("other", "").Key
	if rec := do(t, s, http.MethodPost, "/v1/chat/completions", other, chatBody); rec.Code != http.StatusOK {
		t.Fatalf("另一把密钥应不受影响，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestKeyConcurrencyLimit verifies the per-key concurrency gate and slot release.
func TestKeyConcurrencyLimit(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.Store.UpdateSettings(func(st *store.Settings) { st.KeyConcurrency = 1 })

	release, ok, _ := s.rateLimitGate("fingerprint-a")
	if !ok {
		t.Fatal("第一个请求应拿到并发名额")
	}
	if _, ok, _ := s.rateLimitGate("fingerprint-a"); ok {
		t.Fatal("同一密钥的第二个并发请求应被拒绝")
	}
	// Different keys do not affect each other.
	releaseB, okB, _ := s.rateLimitGate("fingerprint-b")
	if !okB {
		t.Fatal("不同密钥不应共享并发名额")
	}
	release()
	releaseB()
	if _, ok, _ := s.rateLimitGate("fingerprint-a"); !ok {
		t.Fatal("名额归还后应能再次获取")
	}
}

// TestTokenBucketRefill verifies tokens refill over time (rather than being permanently blocked).
func TestTokenBucketRefill(t *testing.T) {
	l := newKeyRateLimiter()
	if ok, _ := l.allow("k", 20, 1); !ok {
		t.Fatal("桶初始应有一个令牌")
	}
	if ok, _ := l.allow("k", 20, 1); ok {
		t.Fatal("令牌用尽后应立即拒绝")
	}
	// At 20 rps, one token is refilled about every 50ms; leave enough margin.
	time.Sleep(120 * time.Millisecond)
	if ok, _ := l.allow("k", 20, 1); !ok {
		t.Fatal("等待后应按速率补充令牌")
	}
}

// TestRateLimitSettingsRoundTrip verifies the console can save and read back rate limit parameters (including explicit 0).
func TestRateLimitSettingsRoundTrip(t *testing.T) {
	s, _ := newTestServer(t, nil)

	rec := do(t, s, http.MethodPost, "/admin/api/settings", "", `{"rate_limit_rps":2.5,"rate_limit_burst":5,"key_concurrency":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存设置失败：%d %s", rec.Code, rec.Body.String())
	}
	st := s.Store.Settings()
	if st.RateLimitRPS != 2.5 || st.RateLimitBurst != 5 || st.KeyConcurrency != 3 {
		t.Fatalf("设置未生效：%+v", st)
	}

	// An explicit 0 means rate limiting is disabled and must not be swallowed by the old "<= 0 means ignore" logic.
	do(t, s, http.MethodPost, "/admin/api/settings", "", `{"rate_limit_rps":0,"key_concurrency":0}`)
	st = s.Store.Settings()
	if st.RateLimitRPS != 0 || st.KeyConcurrency != 0 {
		t.Fatalf("显式 0 应关闭限流：%+v", st)
	}
}

// TestTokenBucketSweepsIdleEntries covers bucket table sweeping: old buckets must not remain forever after key rotation.
//
// Buckets are indexed by key fingerprint; key deletion / rotation does not proactively notify the rate limiter. Without sweeping, a long-running
// running gateway's memory grows linearly with key history (a few hundred bytes each; small in scale but only increasing).
func TestTokenBucketSweepsIdleEntries(t *testing.T) {
	l := newKeyRateLimiter()

	// Fill a table with idle buckets: directly move last beyond the TTL to avoid waiting 30 minutes in the test.
	for i := 0; i < keyRateLimiterMaxBuckets; i++ {
		id := "old-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		l.allow(id, 1, 1)
	}
	l.mu.Lock()
	for _, b := range l.buckets {
		b.last = time.Now().Add(-2 * keyRateLimiterIdleTTL)
	}
	l.mu.Unlock()

	// Creating another new bucket triggers sweeping.
	if ok, _ := l.allow("fresh", 1, 1); !ok {
		t.Fatal("新密钥应能正常限流")
	}
	l.mu.Lock()
	n := len(l.buckets)
	_, kept := l.buckets["fresh"]
	l.mu.Unlock()

	if n > keyRateLimiterMaxBuckets {
		t.Fatalf("桶表未回收：%d 个（上限 %d）", n, keyRateLimiterMaxBuckets)
	}
	if !kept {
		t.Fatal("新桶不应被回收")
	}

	// Buckets with in-flight requests are not swept: their tokens/last are still involved in concurrency decisions.
	l.mu.Lock()
	l.buckets["busy"] = &tokenBucket{tokens: 1, last: time.Now().Add(-2 * keyRateLimiterIdleTTL)}
	l.mu.Unlock()
	l.acquire("busy", 1)
	for i := 0; i < keyRateLimiterMaxBuckets; i++ {
		l.allow("fill-"+string(rune('a'+i%26))+string(rune('a'+i/26)), 1, 1)
	}
	l.mu.Lock()
	_, stillThere := l.buckets["busy"]
	l.mu.Unlock()
	l.release("busy", 1)
	if !stillThere {
		t.Fatal("有在途请求的桶不应被回收")
	}
}
