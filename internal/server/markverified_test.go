package server

import (
	"net/http"
	"testing"

	"deepaicode2api/internal/deepai"
)

// After background verification completes, the registration flow backfills email_verified.
func TestMarkVerifiedByID(t *testing.T) {
	s := newEmptyServer(t)
	s.Store.UpsertAccount(&deepai.Account{ID: "acc-1", Name: "n", APIKey: "sk-1", Email: "a@b.com"})

	rec := postAdmin(t, s, "/admin/api/accounts", `{"action":"mark-verified","id":"acc-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	if a := s.Store.Account("acc-1"); a == nil || !a.EmailVerified {
		t.Fatalf("未回填 email_verified: %#v", a)
	}

	// Idempotent: repeated calls do not error.
	if rec := postAdmin(t, s, "/admin/api/accounts", `{"action":"mark-verified","id":"acc-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("重复调用应幂等，实际 %d", rec.Code)
	}
}

// When adding to the pool fails but verification succeeds, only the email is available, so fall back to
// looking up by email.
func TestMarkVerifiedByEmailFallback(t *testing.T) {
	s := newEmptyServer(t)
	s.Store.UpsertAccount(&deepai.Account{ID: "acc-9", Name: "n", APIKey: "sk-9", Email: "fallback@b.com"})

	rec := postAdmin(t, s, "/admin/api/accounts",
		`{"action":"mark-verified","id":"stale-id","email":"FALLBACK@b.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	if a := s.Store.Account("acc-9"); a == nil || !a.EmailVerified {
		t.Fatalf("按邮箱回填失败: %#v", a)
	}
}

func TestMarkVerifiedMissingAccount(t *testing.T) {
	s := newEmptyServer(t)
	rec := postAdmin(t, s, "/admin/api/accounts", `{"action":"mark-verified","id":"nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的账号应 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
}
