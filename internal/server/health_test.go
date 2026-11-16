package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
)

// /health's ready criteria directly determine the capsule at the top of the console ("accounts N/M ready").
//
// Regression context: the old implementation used `enabled && status != "error"`, so
// - a single probabilistic EOF / 429 permanently marked the account as unhealthy (badge stuck at 1/16 for a long time);
// - conversely, blocked (an account-level block that will not self-heal) was counted as healthy.
//
// This set of tests pins down all three criteria: stale errors count as ready, blocked does not, and fresh errors go to degraded.
func TestHealthReadySemantics(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)

	stale := time.Now().Add(-deepai.ErrorStaleAfter - time.Minute).Unix()
	fresh := time.Now().Unix()

	// acc-1 (ok) already exists. Add: stale error, fresh error, blocked, disabled.
	s.Store.UpsertAccount(&deepai.Account{ID: "stale-err", Name: "stale", APIKey: "k-stale",
		Enabled: true, Status: deepai.StatusError, LastErrAt: stale})
	s.Store.UpsertAccount(&deepai.Account{ID: "fresh-err", Name: "fresh", APIKey: "k-fresh",
		Enabled: true, Status: deepai.StatusError, LastErrAt: fresh})
	s.Store.UpsertAccount(&deepai.Account{ID: "blocked", Name: "blocked", APIKey: "k-blocked",
		Enabled: true, Status: deepai.StatusBlocked})
	s.Store.UpsertAccount(&deepai.Account{ID: "off", Name: "off", APIKey: "k-off",
		Enabled: false, Status: "ok"})

	rec := do(t, s, http.MethodGet, "/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 /health: %v", err)
	}

	got := map[string]int{
		"accounts": int(body["accounts"].(float64)),
		"ready":    int(body["ready"].(float64)),
		"degraded": int(body["degraded"].(float64)),
		"blocked":  int(body["blocked"].(float64)),
	}
	// acc-1(ok) + stale-err (stale = ready) = 2; fresh-err/blocked/off do not count as ready.
	want := map[string]int{"accounts": 5, "ready": 2, "degraded": 1, "blocked": 1}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("%s = %d，期望 %d（全量 %#v）", k, got[k], w, got)
		}
	}
}
