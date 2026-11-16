package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestParseSemver covers parsing edge cases: v prefix, prerelease suffix, non-canonical values.
func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"v0.1.0", [3]int{0, 1, 0}, true},
		{"1.2.3", [3]int{1, 2, 3}, true},
		{"v1.2", [3]int{1, 2, 0}, true},
		{"v1.2.3-rc1", [3]int{1, 2, 3}, true},
		{"v1.2.3+build.7", [3]int{1, 2, 3}, true},
		{"dev", [3]int{}, false},
		{"docker", [3]int{}, false},
		// git describe form (v0.1.0-3-gabc123) is parsed by base tag:
		// A build 3 commits later still belongs to v0.1.0; comparing it against release is reasonable.
		{"v0.1.0-3-gabc123", [3]int{0, 1, 0}, true},
		{"v1.2.3.4", [3]int{}, false},
		{"", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := parseSemver(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseSemver(%q) = %v,%v，期望 %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestVersionNewer guards against false upgrade reports: never return true when no number can be parsed.
func TestVersionNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.0", "v0.1.0", true},
		{"v0.1.1", "v0.1.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"v1.0.0", "dev", false},
		{"dev", "v0.1.0", false},
		// The rc suffix on same-day builds does not affect the decision.
		{"v0.2.0-rc1", "v0.1.0", true},
	}
	for _, c := range cases {
		if got := versionNewer(c.latest, c.current); got != c.want {
			t.Errorf("versionNewer(%q,%q) = %v，期望 %v", c.latest, c.current, got, c.want)
		}
	}
}

// withUpdateFeed points the update source at a test server and returns a request counter.
func withUpdateFeed(t *testing.T, handler http.HandlerFunc) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	old := updateFeedURL
	updateFeedURL = srv.URL
	t.Cleanup(func() { updateFeedURL = old })
	return &hits
}

// TestAdminVersionEndpoint verifies the full flow and caching for 'new version available'.
func TestAdminVersionEndpoint(t *testing.T) {
	hits := withUpdateFeed(t, func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua == "" {
			t.Error("请求应带 User-Agent（GitHub API 强制要求）")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v0.2.0",
			"html_url":     "https://github.com/2951461586/DeepAICode2API/releases/tag/v0.2.0",
			"published_at": "2026-11-12T00:00:00Z",
		})
	})

	s, _ := newTestServer(t, nil)
	s.SetVersion("v0.1.0")

	rec := do(t, s, http.MethodGet, "/admin/api/version", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["current"] != "v0.1.0" {
		t.Fatalf("current = %v", out["current"])
	}
	if out["update_available"] != true {
		t.Fatalf("应判定有新版本：%v", out)
	}
	if out["latest"] != "v0.2.0" {
		t.Fatalf("latest = %v", out["latest"])
	}
	if out["release_url"] == "" || out["release_url"] == nil {
		t.Fatalf("应带 release_url：%v", out)
	}

	// The second call must hit the cache: unauthenticated GitHub requests are rate-limited.
	_ = do(t, s, http.MethodGet, "/admin/api/version", "", "")
	if n := hits.Load(); n != 1 {
		t.Fatalf("应只查一次上游（缓存 6h），实际 %d 次", n)
	}
}

// TestAdminVersionDevBuild verifies dev builds skip comparison (otherwise it would always report 'new version available').
func TestAdminVersionDevBuild(t *testing.T) {
	hits := withUpdateFeed(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("dev 构建不应请求更新源")
	})
	s, _ := newTestServer(t, nil) // SetVersion not called → dev
	rec := do(t, s, http.MethodGet, "/admin/api/version", "", "")
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["update_available"] != false {
		t.Fatalf("dev 构建不应提示更新：%v", out)
	}
	if out["error"] == nil || out["error"] == "" {
		t.Fatalf("dev 构建应说明原因：%v", out)
	}
	if hits.Load() != 0 {
		t.Fatalf("dev 构建不应打上游，实际 %d 次", hits.Load())
	}
}

// TestAdminVersionUpstreamFailure verifies that upstream failure still returns 200 + current version.
func TestAdminVersionUpstreamFailure(t *testing.T) {
	withUpdateFeed(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	s, _ := newTestServer(t, nil)
	s.SetVersion("v0.1.0")

	rec := do(t, s, http.MethodGet, "/admin/api/version", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("上游失败不应让接口失败，实际 %d：%s", rec.Code, rec.Body.String())
	}
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["current"] != "v0.1.0" || out["update_available"] != false {
		t.Fatalf("应返回当前版本且不提示更新：%v", out)
	}
	if out["error"] == nil {
		t.Fatalf("应带 error 字段说明查询失败：%v", out)
	}

	// 405 when method is wrong.
	if rec := do(t, s, http.MethodPost, "/admin/api/version", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应回 405，实际 %d", rec.Code)
	}
}

// TestHealthIncludesVersion verifies the console can get the version number from a public endpoint.
func TestHealthIncludesVersion(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.SetVersion("v0.1.0")
	rec := do(t, s, http.MethodGet, "/health", "", "")
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["version"] != "v0.1.0" {
		t.Fatalf("health.version = %v", out["version"])
	}
}

// TestVersionStatusSingleFlight covers 'merging concurrent queries when the cache is cold'.
//
// The console may refresh in multiple tabs at once; without merging, every request hits GitHub,
// and the unauthenticated GitHub API is rate-limited. Here the upstream is allowed to be hit only once.
func TestVersionStatusSingleFlight(t *testing.T) {
	var hits atomic.Int64
	gate := make(chan struct{}) // Gate for 8 calls to truly overlap: release only after all have been initiated
	withUpdateFeed(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-gate
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.2.0"}); err != nil {
			t.Errorf("写测试响应失败: %v", err)
		}
	})

	s, _ := newTestServer(t, nil)
	s.SetVersion("v0.1.0")

	const n = 8
	// Results go through a channel instead of results[i]: writing different indices of the same slice concurrently is safe,
	// but the static rule (go-shared-map-write-goroutine) cannot see that this is 'each writing its own',
	// so every scan requires manual confirmation. A channel encodes 'who produces, who consumes' in the type.
	results := make(chan versionInfo, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			results <- s.versionStatus(t.Context())
		}()
	}
	close(gate) // Release: at this point all 8 goroutines are already in the singleflight queue
	wg.Wait()
	close(results)

	if got := hits.Load(); got != 1 {
		t.Fatalf("上游应只被查询一次，实际 %d 次", got)
	}
	seen := 0
	for got := range results {
		seen++
		if got.Current != "v0.1.0" || got.Latest != "v0.2.0" || !got.UpdateAvailable {
			t.Fatalf("第 %d 个结果不对：%+v", seen, got)
		}
	}
	if seen != n {
		t.Fatalf("结果条数 = %d，期望 %d", seen, n)
	}
}

// TestVersionStatusSurvivesCallerCancel guards against cache pollution when the first caller disconnects.
//
// Previously the singleflight leader used the caller's ctx directly: the console is opened and the tab is closed right away → the in-flight
// GitHub query gets aborted → the failure result is written to the cache for 6 hours → afterwards everyone opening the console sees
// "Unable to check for updates", even though GitHub is actually fully reachable.
func TestVersionStatusSurvivesCallerCancel(t *testing.T) {
	// arrived is closed when the upstream actually receives the request; release is closed by the test after cancel.
	// Use channels instead of sleep: no random waiting, and unaffected by machine speed.
	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hits := withUpdateFeed(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"tag_name": "v9.9.9"}); err != nil {
			t.Errorf("写测试响应失败: %v", err)
		}
	})

	s, _ := newTestServer(t, nil)
	s.SetVersion("v0.1.0")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan versionInfo, 1)
	go func() { done <- s.versionStatus(ctx) }()

	select {
	case <-arrived: // the leader's query is already on its way to the upstream
	case <-time.After(5 * time.Second):
		t.Fatal("上游始终没收到请求")
	}
	cancel()       // at this moment the caller disconnects (browser closes the tab)
	close(release) // let the upstream return
	leader := <-done

	// Key assertion: the cancel happens mid-query, and the leader should still get the result rather than caching the cancellation as a failure.
	if leader.Error != "" {
		t.Fatalf("调用方取消不应污染结果：%+v", leader)
	}
	got := s.versionStatus(t.Context())
	if got.Error != "" {
		t.Fatalf("缓存被污染：%+v", got)
	}
	if got.Latest != "v9.9.9" || !got.UpdateAvailable {
		t.Fatalf("缓存结果不对：%+v", got)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("上游应只被查一次，实际 %d 次（取消可能触发了重查）", n)
	}
}
