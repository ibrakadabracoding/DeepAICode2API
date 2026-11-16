package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// /v1/theos uses the cache within 10 minutes: it is also the check-in channel, and high-frequency polling will trigger the upstream's daily rate limit
// Restart check-in count exceeds the limit (403 check_in_refused).
func TestTheosEndpointCachesForTenMinutes(t *testing.T) {
	hits := 0
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"200","granted":"200"}`)
	})
	s, key := newTestServer(t, u)

	rec := do(t, s, http.MethodGet, "/v1/theos", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("首次查询失败: %d %s", rec.Code, rec.Body.String())
	}
	if hits != 1 {
		t.Fatalf("首次应真打上游一次，实际 %d", hits)
	}
	if strings.Contains(rec.Body.String(), `"cached":true`) {
		t.Fatalf("首次不应返回缓存: %s", rec.Body.String())
	}

	rec = do(t, s, http.MethodGet, "/v1/theos", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("缓存查询失败: %d", rec.Code)
	}
	if hits != 1 {
		t.Fatalf("10 分钟内应命中缓存，实际多打了 %d 次", hits-1)
	}
	if !strings.Contains(rec.Body.String(), `"cached":true`) {
		t.Fatalf("应标记 cached: %s", rec.Body.String())
	}

	// force=1 bypasses the cache.
	rec = do(t, s, http.MethodGet, "/v1/theos?force=1", key, "")
	if rec.Code != http.StatusOK || hits != 2 {
		t.Fatalf("force=1 应真打上游，hits=%d code=%d", hits, rec.Code)
	}
}
