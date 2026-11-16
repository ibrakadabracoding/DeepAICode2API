package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"deepaicode2api/internal/store"
)

// Manage the passphrase login page (/login) and its verification endpoint (/admin/api/auth).
//
// Regression context: the console top bar used to show a passphrase input box directly, so the passphrase was on screen and on the long-lived page
// and always visible. After switching to a standalone login page, this chain must not break: the page must open, verification must take effect,
// and when no passphrase is set, the local machine must not be blocked.

func TestLoginPageServed(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)

	rec := do(t, s, http.MethodGet, "/login", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/login HTTP %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("/login Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="loginForm"`, `id="loginPw"`, `id="loginErr"`, `/js/login.js`} {
		if !strings.Contains(body, want) {
			t.Errorf("/login 缺少 %s", want)
		}
	}
	// The login page should not bring out the console's forms (the two pages have different responsibilities; mixing them makes people think
	// they can modify data directly without logging in).
	if strings.Contains(body, `id="acctTable"`) {
		t.Error("/login 不应包含控制台账号表")
	}
}

func TestLoginScriptServed(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)

	rec := do(t, s, http.MethodGet, "/js/login.js", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("/js/login.js HTTP %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/admin/api/auth") {
		t.Error("login.js 应调用 /admin/api/auth 校验口令")
	}
}

func TestAdminAuthEndpoint(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)

	// No admin passphrase set: local requests are allowed through directly (otherwise the console could never be accessed).
	if rec := do(t, s, http.MethodPost, "/admin/api/auth", "", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("未设口令时应 200，实际 %d", rec.Code)
	}

	s.Store.UpdateSettings(func(st *store.Settings) { st.AdminPassword = "s3cret" })
	if rec := do(t, s, http.MethodPost, "/admin/api/auth", "", "{}"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺口令应 401，实际 %d", rec.Code)
	}
	// do() puts the passphrase into Authorization: Bearer.
	if rec := do(t, s, http.MethodPost, "/admin/api/auth", "wrong", "{}"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错口令应 401，实际 %d", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, "/admin/api/auth", "s3cret", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("对口令应 200，实际 %d", rec.Code)
	}
	// The response body must not contain the passphrase itself.
	var body map[string]any
	rec := do(t, s, http.MethodPost, "/admin/api/auth", "s3cret", "{}")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(body) != 1 || body["ok"] != true {
		t.Fatalf("返回体只应有 {ok:true}，实际 %#v", body)
	}
}

func TestHealthExposesAdminRequired(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)

	read := func() bool {
		rec := do(t, s, http.MethodGet, "/health", "", "")
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析 /health: %v", err)
		}
		v, ok := body["admin_required"].(bool)
		if !ok {
			t.Fatalf("/health 缺少 admin_required 字段：%#v", body)
		}
		return v
	}

	if read() {
		t.Fatal("未设口令时 admin_required 应为 false")
	}
	s.Store.UpdateSettings(func(st *store.Settings) { st.AdminPassword = "s3cret" })
	if !read() {
		t.Fatal("设了口令后 admin_required 应为 true")
	}
}
