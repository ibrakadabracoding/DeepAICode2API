package deepai

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestEnsureSystemMessageAddsOnlyWhenMissing pins down "the preset shape only adds system".
func TestEnsureSystemMessageAddsOnlyWhenMissing(t *testing.T) {
	body := []byte(`{"model":"m","temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`)
	out, inserted := EnsureSystemMessage(body, "")
	if !inserted {
		t.Fatal("缺少 system 应插入")
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	msgs := payload["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 长度 = %d，期望 2", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("第一条应为 system: %#v", msgs[0])
	}
	// Other fields must be preserved as-is (no trimming like ChatBodyFallback).
	if payload["temperature"].(float64) != 0.3 {
		t.Fatalf("temperature 被改写: %#v", payload["temperature"])
	}
	if !bytes.Contains(out, []byte(`"model":"m"`)) {
		t.Fatalf("model 丢失: %s", out)
	}

	// No change when system (or developer) is already present.
	for _, role := range []string{"system", "developer"} {
		with := []byte(`{"model":"m","messages":[{"role":"` + role + `","content":"s"},{"role":"user","content":"hi"}]}`)
		out2, inserted := EnsureSystemMessage(with, "")
		if inserted {
			t.Fatalf("已有 %s 时不应插入", role)
		}
		if !bytes.Equal(out2, with) {
			t.Fatalf("已有 %s 时不应改写请求体", role)
		}
	}

	// Invalid / empty messages: no insert, returns false.
	for _, bad := range []string{`not json`, `{}`, `{"messages":[]}`} {
		if _, ins := EnsureSystemMessage([]byte(bad), ""); ins {
			t.Fatalf("body=%q 不应插入", bad)
		}
	}
}

// TestQuotaExhaustedDetectionAndReset covers detection of 403 theos_exhausted and the reset time.
func TestQuotaExhaustedDetectionAndReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	// refresh_at must be **relative** to now: a hardcoded date turns this test case into a time bomb
	// (once 2026-11-14T00:05:52 passes, QuotaResetAt treats it as "expired", falls back to now+6h, and CI goes red).
	resetAt := now.Add(2 * time.Hour)
	stamp := resetAt.Format(time.RFC3339)
	body := `{"error":{"message":"You've used today's Theos. They refresh at ` + stamp + `.",` +
		`"type":"theos_exhausted","code":"theos_exhausted","refresh_at":"` + stamp + `"}}`
	e := mustErr(t, http.StatusForbidden, body)
	if !e.QuotaExhausted() {
		t.Fatal("应识别为 theos_exhausted")
	}
	if e.SocialLoginRequired() || e.EmailUnverified() || e.MachineRequired() {
		t.Fatal("不应误判为其它账号终态")
	}
	if got := e.QuotaResetAt(6*time.Hour, now); !got.Equal(resetAt) {
		t.Fatalf("恢复时间 = %v，期望 %v", got, resetAt)
	}
	// When refresh_at is already in the past (upstream gave a past time), use the fallback instead of retrying right away.
	past := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	stale := mustErr(t, http.StatusForbidden,
		`{"error":{"code":"theos_exhausted","refresh_at":"`+past+`"}}`)
	if got, want := stale.QuotaResetAt(6*time.Hour, now), now.Add(6*time.Hour); !got.Equal(want) {
		t.Fatalf("过期 refresh_at 应回退到 now+6h，实际 %v", got)
	}
	// Other error codes must not match.
	if mustErr(t, http.StatusForbidden, `{"error":{"code":"check_in_refused"}}`).QuotaExhausted() {
		t.Fatal("check_in_refused 不应识别为额度用完")
	}
}
