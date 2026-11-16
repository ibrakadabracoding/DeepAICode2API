package server

import (
	"io"
	"net/http"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
)

// The cooldown after a failed probe must actually block the background guardian: otherwise every
// 5 minutes it re-probes the whole batch of failed accounts (failures often come in batches), turning
// into a sustained /theos storm.
func TestQuotaNeedsRefreshRespectsProbeCooldown(t *testing.T) {
	now := time.Now()

	cooling := &deepai.Account{ID: "a1", Enabled: true, ProbeAfter: now.Add(time.Hour).Unix()}
	if quotaNeedsRefresh(cooling, now, 6*time.Hour) {
		t.Fatal("冷却期内不应再探测")
	}
	if !quotaNeedsRefresh(cooling, now.Add(2*time.Hour), 6*time.Hour) {
		t.Fatal("冷却到期且从未探到额度，应重新探测")
	}

	fresh := &deepai.Account{
		ID: "a2", Enabled: true,
		Theos: &deepai.Allowance{Enabled: true, CheckedAt: now.Unix()},
	}
	if quotaNeedsRefresh(fresh, now, 6*time.Hour) {
		t.Fatal("额度快照新鲜时不应重探")
	}
}

// check_in_refused must set the 6h probe lock and must not mark the account as error
// (inference still works; it just falls to the substitute branch for the day).
func TestProbeCheckInRefusedLocksProbe(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"check_in_refused","code":"check_in_refused"},`+
			`"check_in":{"nonce":"0123456789abcdef0123456789abcdef","seconds":600}}`)
	})
	s, _ := newTestServer(t, u)

	s.probeAccounts(s.Store.Accounts())

	a := s.Store.Account("acc-1")
	if a == nil {
		t.Fatal("账号不应消失")
	}
	if a.Status == "error" {
		t.Fatalf("签到限流不是账号坏，不应标 error: %q", a.LastErr)
	}
	if a.ProbeAfter <= time.Now().Unix() {
		t.Fatal("应落探测锁")
	}
	if left := time.Until(time.Unix(a.ProbeAfter, 0)); left < 5*time.Hour {
		t.Fatalf("探测锁应约 6h，实际剩余 %v", left)
	}
}

// A failed on-demand re-probe on the inference hot path must leave backoff behind: in production
// (docs/changes.md §61) the same account was re-probed 323 times in 1 hour because this path only
// logged and never wrote ProbeAfter.
func TestProbeAsyncFailureRecordsBackoff(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"models temporarily unavailable"}}`)
	})
	s, _ := newTestServer(t, u)

	stale := time.Now().Add(-time.Hour).Unix()
	s.Store.UpsertAccount(&deepai.Account{
		ID: "stale-1", Name: "stale", APIKey: "sk-stale", Enabled: true, Status: "ok",
		Theos: &deepai.Allowance{Enabled: true, CheckedAt: stale},
	})
	a := s.Store.Account("stale-1")
	if a == nil {
		t.Fatal("账号不存在")
	}
	if !quotaNeedsRefresh(a, time.Now(), quotaStaleAfter) {
		t.Fatal("前置条件：额度快照过旧，应需要复探")
	}

	if !s.probeAsync(a) {
		t.Fatal("应发起一次探测")
	}
	s.quotaWG.Wait()

	got := s.Store.Account("stale-1")
	if got.ProbeFailures != 1 {
		t.Fatalf("探测失败应记一次退避，实际 ProbeFailures = %d", got.ProbeFailures)
	}
	if got.ProbeAfter <= time.Now().Unix() {
		t.Fatalf("应落探测冷却，实际 ProbeAfter = %d", got.ProbeAfter)
	}
	if quotaNeedsRefresh(got, time.Now(), quotaStaleAfter) {
		t.Fatal("冷却期内不应再被判为需要复探（否则每个请求都会重探一次）")
	}
	// Hot-path failures must not pollute scheduling: neither status nor failure count changes.
	if got.Status != "ok" || got.Failures != 0 {
		t.Fatalf("不应改调度状态: status=%q failures=%d", got.Status, got.Failures)
	}
	if got.Theos == nil || got.Theos.CheckedAt != stale {
		t.Fatal("失败不应改写额度快照")
	}
}
