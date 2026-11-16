package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
)

// quotaAccount constructs an account with a quota snapshot.
func quotaAccount(id string, checkedAt int64, refreshAt string) *deepai.Account {
	a := &deepai.Account{ID: id, Name: id, APIKey: "sk-" + id, Enabled: true}
	if checkedAt > 0 {
		a.Theos = &deepai.Allowance{
			Enabled: true, Balance: "0.00000", RefreshAt: refreshAt, CheckedAt: checkedAt,
		}
	}
	return a
}

func TestQuotaNeedsRefresh(t *testing.T) {
	now := time.Now()
	future := now.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	past := now.Add(-time.Hour).UTC().Format(time.RFC3339)

	cases := []struct {
		name       string
		acct       *deepai.Account
		staleAfter time.Duration
		want       bool
	}{
		{"nil", nil, 0, false},
		{"disabled", &deepai.Account{Enabled: false}, 0, false},
		{"never probed", quotaAccount("a", 0, ""), 0, true},
		{"refresh passed", quotaAccount("b", now.Unix(), past), 0, true},
		{"refresh future strict", quotaAccount("c", now.Unix(), future), 0, false},
		{"refresh future but stale", quotaAccount("d", now.Add(-16*time.Minute).Unix(), future), 15 * time.Minute, true},
		{"fresh", quotaAccount("e", now.Add(-time.Minute).Unix(), future), 15 * time.Minute, false},
	}
	for _, tc := range cases {
		if got := quotaNeedsRefresh(tc.acct, now, tc.staleAfter); got != tc.want {
			t.Errorf("%s: quotaNeedsRefresh = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

// Batching: each request probes only limit accounts and reports total / probed / remaining.
func TestCheckinBatchesAndReportsRemaining(t *testing.T) {
	var hits atomic.Int64
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"1.00"}`)
	})
	s, _ := newTestServer(t, u) // acc-1
	for i := 0; i < 4; i++ {
		s.Store.UpsertAccount(&deepai.Account{
			ID: "batch-" + string(rune('a'+i)), APIKey: "sk-b" + string(rune('a'+i)), Enabled: true,
		})
	}

	rec := postAdmin(t, s, "/admin/api/checkin", `{"limit":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["total"] != float64(5) || out["probed"] != float64(3) || out["remaining"] != float64(2) {
		t.Fatalf("分批计数异常: %#v", out)
	}
	if results, _ := out["results"].([]any); len(results) != 3 {
		t.Fatalf("results 条数 = %d，期望 3", len(results))
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("第一批上游探测 %d 次，期望 3", got)
	}

	// The second batch should fill in the unprobed accounts, and remaining should drop to zero.
	rec2 := postAdmin(t, s, "/admin/api/checkin", `{"limit":3}`)
	out2 := decodeJSON[map[string]any](t, rec2.Body.Bytes())
	if out2["remaining"] != float64(0) {
		t.Fatalf("第二批 remaining = %v，期望 0", out2["remaining"])
	}
	for i := 0; i < 4; i++ {
		id := "batch-" + string(rune('a'+i))
		if a := s.Store.Account(id); a == nil || a.Theos == nil || a.Theos.CheckedAt == 0 {
			t.Fatalf("账号 %s 未被探测: %#v", id, a)
		}
	}
}

// group / stale_only filtering: should not hit unrelated accounts.
func TestCheckinFiltersByGroupAndStaleness(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Api-Key")]++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"1.00"}`)
	})
	s, _ := newTestServer(t, u)
	stale := time.Now().Add(-time.Hour).Unix()
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	// The accounts in g2 are stale; in g1, one is stale and one was just probed.
	s.Store.UpsertAccount(quotaAccount("g1-stale", stale, future))
	s.Store.UpdateAccount("g1-stale", func(a *deepai.Account) { a.Group = "g1" })
	s.Store.UpsertAccount(quotaAccount("g1-fresh", time.Now().Unix(), future))
	s.Store.UpdateAccount("g1-fresh", func(a *deepai.Account) { a.Group = "g1" })
	s.Store.UpsertAccount(quotaAccount("g2-stale", stale, future))
	s.Store.UpdateAccount("g2-stale", func(a *deepai.Account) { a.Group = "g2" })

	rec := postAdmin(t, s, "/admin/api/checkin", `{"group":"g2"}`)
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["total"] != float64(1) || out["probed"] != float64(1) {
		t.Fatalf("分组过滤异常: %#v", out)
	}
	mu.Lock()
	if seen["sk-g2-stale"] != 1 {
		mu.Unlock()
		t.Fatalf("g2 账号未被探测: %#v", seen)
	}
	mu.Unlock()

	// stale_only should skip g1-fresh, which was just probed (but acc-1 has never been probed and will be included).
	seen = map[string]int{}
	rec2 := postAdmin(t, s, "/admin/api/checkin", `{"stale_only":true}`)
	out2 := decodeJSON[map[string]any](t, rec2.Body.Bytes())
	mu.Lock()
	freshHits := seen["sk-g1-fresh"]
	mu.Unlock()
	if freshHits != 0 {
		t.Fatalf("stale_only 仍探测了刚探过的账号: %#v", seen)
	}
	if out2["probed"] == float64(0) {
		t.Fatalf("stale_only 应至少探到从未探测的账号: %#v", out2)
	}
}

// Only one probe is allowed per account at a time (manual + background + on-demand paths can collide).
func TestProbeBatchDedupesInFlight(t *testing.T) {
	var hits atomic.Int64
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"1.00"}`)
	})
	s, _ := newTestServer(t, u)
	a := s.Store.Account("acc-1")

	if !s.claimProbe(a.ID) {
		t.Fatal("首次占用探测槽位应成功")
	}
	out := s.probeBatch(t.Context(), []*deepai.Account{a}, time.Second, true)
	s.releaseProbe(a.ID)

	if len(out) != 1 || !out[0].Skipped {
		t.Fatalf("在途时应标记 Skipped: %#v", out)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("在途去重失效，上游被探测 %d 次", got)
	}
}

// The background daemon should only pick stale accounts; fresh accounts should not be probed repeatedly.
func TestReprobeStaleOnlyProbesDueAccounts(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Api-Key")]++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"1.00"}`)
	})
	s, _ := newTestServer(t, u) // acc-1: never probed → should be probed
	future := time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)
	s.Store.UpsertAccount(quotaAccount("fresh", time.Now().Unix(), future))
	s.Store.UpdateAccount("fresh", func(a *deepai.Account) { a.Group = "g" })

	if n := s.ReprobeStale(0); n != 1 {
		t.Fatalf("后台复探数 = %d，期望 1（只有从未探测的 acc-1）", n)
	}
	mu.Lock()
	freshHits := seen["sk-fresh"]
	accHits := seen["upstream-key"]
	mu.Unlock()
	if freshHits != 0 {
		t.Fatalf("新鲜的账号被后台探测 %d 次", freshHits)
	}
	if accHits != 1 {
		t.Fatalf("过旧账号应被探测 1 次，实际 %d", accHits)
	}
}

// When inference hits an account whose quota snapshot has expired, it should asynchronously re-probe once (without blocking the current request).
func TestInferenceRefreshesStaleQuota(t *testing.T) {
	var mu sync.Mutex
	theosHits := 0
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "theos") {
			mu.Lock()
			theosHits++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"enabled":true,"balance":"9.00"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"glm-5.3-flash",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	s, key := newTestServer(t, u)
	past := time.Now().Add(-time.Hour)
	s.Store.UpdateAccount("acc-1", func(a *deepai.Account) {
		a.Theos = &deepai.Allowance{
			Enabled: true, Balance: "0.00", CheckedAt: past.Unix(),
			RefreshAt: past.Add(48 * time.Hour).UTC().Format(time.RFC3339),
		}
	})

	rec := do(t, s, http.MethodPost, "/v1/chat/completions", key,
		`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}

	// Wait for the asynchronous probe to finish (also avoids goroutines writing to the temp directory after the test ends).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.probeMu.Lock()
		busy := len(s.probing) > 0
		s.probeMu.Unlock()
		mu.Lock()
		hits := theosHits
		mu.Unlock()
		if !busy && hits > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	hits := theosHits
	mu.Unlock()
	if hits == 0 {
		t.Fatal("推理命中过期额度账号时未触发异步复探")
	}
	if a := s.Store.Account("acc-1"); a == nil || a.Theos == nil || a.Theos.Balance != "9.00" {
		t.Fatalf("复探结果未写回: %#v", a.Theos)
	}
}

// A failed automatic re-probe should not mark an account serving inference as error / increment Failures.
func TestProbeAsyncFailureDoesNotMarkAccount(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	s, _ := newTestServer(t, u)
	a := s.Store.Account("acc-1")

	if !s.probeAsync(a) {
		t.Fatal("首次异步探测应被发起")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.probeMu.Lock()
		busy := len(s.probing) > 0
		s.probeMu.Unlock()
		if !busy {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := s.Store.Account("acc-1")
	if got.Status != "ok" || got.Failures != 0 {
		t.Fatalf("自动复探失败污染了账号状态: status=%q failures=%d", got.Status, got.Failures)
	}
}

// TestCheckinSkipsInFlightProbes guards the counting semantics when a probe is already in flight.
//
// Previously, probed was taken directly as len(batch), counting skipped ones as probed this time; and the skipped entry was then
// placed into settled, so remaining also considered it "converged". Result: the console reported "quota refresh
// complete: N accounts", some of which were never probed; and after remaining dropped to zero, no more retries.
func TestCheckinSkipsInFlightProbes(t *testing.T) {
	var hits atomic.Int64
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"1.00"}`)
	})
	s, _ := newTestServer(t, u)

	// Occupy acc-1's probe slot to simulate "another path is probing".
	if !s.claimProbe("acc-1") {
		t.Fatal("占用探测槽位失败")
	}
	defer s.releaseProbe("acc-1")

	rec := postAdmin(t, s, "/admin/api/checkin", `{"id":"acc-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["probed"] != float64(0) {
		t.Fatalf("跳过的探测被算成 probed: %#v", out)
	}
	if out["skipped"] != float64(1) {
		t.Fatalf("skipped = %v，期望 1: %#v", out["skipped"], out)
	}
	// Skipped ≠ probed: it is still in an unknown state and must remain in remaining for the next batch.
	if out["remaining"] != float64(1) {
		t.Fatalf("remaining = %v，期望 1（跳过不应收敛）", out["remaining"])
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("跳过时不应打上游，实际 %d 次", got)
	}
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results 条数 = %d，期望 1", len(results))
	}
	if item, _ := results[0].(map[string]any); item["skipped"] != true {
		t.Fatalf("results[0] 应标记 skipped: %#v", results[0])
	}
}
