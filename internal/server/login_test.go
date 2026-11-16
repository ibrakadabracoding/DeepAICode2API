package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
)

// Browser login (including Google login) callback only yields an API Key, without a machine fingerprint.
// When adding to the pool, an independent UUID must be supplied: the upstream issues daily free quota per machine, and reusing the local machine fingerprint would cause
// multiple accounts to share the same quota (only the first one can claim it).
func TestLoginPollAssignsMachineID(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/theos") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"enabled":true,"balance":"200","granted":"200","lane_version":4}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	s, _ := newTestServer(t, u)

	sess := &loginSession{ID: "sid", State: "st", Created: time.Now()}
	sess.setResult(&loginResult{Creds: &deepai.Credentials{APIKey: "oauth-key", Username: "google-user"}})
	s.Login.put(sess)

	rec := do(t, s, http.MethodPost, "/admin/api/login/poll", "", `{"session_id":"sid"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login/poll 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("login/poll 应返回 status=ok: %s", rec.Body.String())
	}

	var acct *deepai.Account
	for _, a := range s.Store.Accounts() {
		if a.APIKey == "oauth-key" {
			acct = a
			break
		}
	}
	if acct == nil {
		t.Fatal("浏览器登录的账号没有入池")
	}
	if acct.Source != "oauth" {
		t.Errorf("来源应为 oauth，实际 %q", acct.Source)
	}
	if strings.TrimSpace(acct.MachineID) == "" {
		t.Fatal("浏览器登录入池未分配独立机器指纹")
	}
	// Adding to the pool starts a background health check: wait for it to persist to disk before returning, otherwise TempDir cleanup will race with the write.
	s.waitQuotaReprobe()
}

// When adding to the pool via browser login, if a proxy pool is configured (PROXY_POOL / PROXY_POOL_FILE), rotation must be used
// to allocate egress: the daily free quota is shared for the same direct-connect IP. Here, two consecutive
// pool additions on the same Server verify that the cursor keeps advancing within the process (rather than restarting from the first proxy each time).
func TestLoginPollAssignsProxyFromPool(t *testing.T) {
	t.Setenv("PROXY_POOL", "http://127.0.0.1:1,http://127.0.0.1:2")

	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/theos") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"enabled":true,"balance":"200","granted":"200","lane_version":4}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	s, _ := newTestServer(t, u)

	poll := func(id, key string) *deepai.Account {
		t.Helper()
		sess := &loginSession{ID: id, State: "st-" + id, Created: time.Now()}
		sess.setResult(&loginResult{Creds: &deepai.Credentials{APIKey: key, Username: "user-" + id}})
		s.Login.put(sess)
		rec := do(t, s, http.MethodPost, "/admin/api/login/poll", "", `{"session_id":"`+id+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("login/poll(%s) 应 200，实际 %d: %s", id, rec.Code, rec.Body.String())
		}
		for _, a := range s.Store.Accounts() {
			if a.APIKey == key {
				return a
			}
		}
		t.Fatalf("账号 %s 未入池", key)
		return nil
	}

	a1 := poll("sid-1", "oauth-key-1")
	a2 := poll("sid-2", "oauth-key-2")
	if a1.Proxy == "" || a2.Proxy == "" {
		t.Fatalf("oauth 入池应自动分配代理，实际 %q / %q", a1.Proxy, a2.Proxy)
	}
	if a1.Proxy == a2.Proxy {
		t.Fatalf("代理池应轮询到不同出口，实际都是 %q", a1.Proxy)
	}
	s.waitQuotaReprobe()
}

// Fake upstream: redeems the code, other paths respond on demand.
func loginUpstream(t *testing.T, apiKey, username string) *upstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/desktop_auth/exchange/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"api_key":"`+apiKey+`","username":"`+username+`","is_pro":false}`)
		case strings.HasSuffix(r.URL.Path, "/theos"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"enabled":true,"balance":"200","granted":"200","lane_version":4}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// Start a login session and return (session_id, state).
func startLogin(t *testing.T, s *Server) (string, string) {
	t.Helper()
	rec := do(t, s, http.MethodPost, "/admin/api/login/start", "", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login/start 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		SessionID string `json:"session_id"`
		Port      int    `json:"port"`
		Loopback  bool   `json:"loopback"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 login/start 响应: %v", err)
	}
	if out.SessionID == "" || out.Port <= 0 {
		t.Fatalf("login/start 响应缺少 session_id/port: %s", rec.Body.String())
	}
	if !out.Loopback {
		t.Fatalf("测试环境应能绑定本机回环，实际 loopback=false: %s", rec.Body.String())
	}
	sess := s.Login.get(out.SessionID)
	if sess == nil {
		t.Fatal("login/start 没有登记会话")
	}
	return out.SessionID, sess.State
}

// In Docker / remote deployments the browser callback cannot be opened, so the user pastes the address bar URL back into the console,
// and the gateway redeems the code and completes pool addition - equivalent to the local callback path.
func TestLoginCallbackPasteCompletesLogin(t *testing.T) {
	u := loginUpstream(t, "pasted-key", "pasted-user")
	s, _ := newTestServer(t, u)

	sid, state := startLogin(t, s)
	cb := "http://127.0.0.1:34105/auth-callback?code=bmQ6lztT61GCm9ED_bcN&state=" + state
	rec := do(t, s, http.MethodPost, "/admin/api/login/callback", "",
		`{"session_id":"`+sid+`","callback":"`+cb+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login/callback 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("应返回 status=ok: %s", rec.Body.String())
	}

	var acct *deepai.Account
	for _, a := range s.Store.Accounts() {
		if a.APIKey == "pasted-key" {
			acct = a
			break
		}
	}
	if acct == nil {
		t.Fatal("粘贴回调的账号没有入池")
	}
	if acct.Source != "oauth" {
		t.Errorf("来源应为 oauth，实际 %q", acct.Source)
	}
	if strings.TrimSpace(acct.MachineID) == "" {
		t.Error("粘贴回调入池未分配独立机器指纹")
	}
	if s.Login.get(sid) != nil {
		t.Error("登录完成后会话应被删除")
	}
	// The pool-addition health check goroutine calls Save() to persist to disk: wait for it to finish, otherwise TempDir cleanup will report
	// "directory not empty" (more likely to hit with -race on CI).
	s.waitQuotaReprobe()

	// The code is one-time: pasting the same code again must not add to the pool twice.
	again := do(t, s, http.MethodPost, "/admin/api/login/callback", "",
		`{"session_id":"`+sid+`","callback":"`+cb+`"}`)
	if again.Code == http.StatusOK {
		t.Fatalf("同一会话二次粘贴不应成功: %s", again.Body.String())
	}
}

// Only accept callbacks from "this login": a mismatched state must be rejected (to prevent pasting the wrong tab).
func TestLoginCallbackRejectsForeignState(t *testing.T) {
	u := loginUpstream(t, "k", "u")
	s, _ := newTestServer(t, u)

	sid, _ := startLogin(t, s)
	cb := "http://127.0.0.1:34105/auth-callback?code=abc&state=deadbeef"
	rec := do(t, s, http.MethodPost, "/admin/api/login/callback", "",
		`{"session_id":"`+sid+`","callback":"`+cb+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state 不匹配应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if len(s.Store.Accounts()) != 1 {
		t.Errorf("被拒的回调不应入池，账号数 = %d", len(s.Store.Accounts()))
	}
}

// Pasting only the code gives no state, so it cannot be confirmed as belonging to this login: reject directly with a hint.
func TestLoginCallbackRejectsBareCode(t *testing.T) {
	u := loginUpstream(t, "k", "u")
	s, _ := newTestServer(t, u)

	sid, _ := startLogin(t, s)
	rec := do(t, s, http.MethodPost, "/admin/api/login/callback", "",
		`{"session_id":"`+sid+`","callback":"bmQ6lztT61GCm9ED_bcN-AhOdT2GzgAdHC2iJvT-Bxc"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("只粘 code 应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "state") {
		t.Errorf("错误信息应提示缺少 state: %s", rec.Body.String())
	}
}

// Redemption failure (code expired / already used) must give a readable hint and end the session.
func TestLoginCallbackExchangeFailure(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/desktop_auth/exchange/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_code","message":"expired"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	s, _ := newTestServer(t, u)

	sid, state := startLogin(t, s)
	cb := "http://127.0.0.1:34105/auth-callback?code=abc&state=" + state
	rec := do(t, s, http.MethodPost, "/admin/api/login/callback", "",
		`{"session_id":"`+sid+`","callback":"`+cb+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("兑换失败应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "只能兑换一次") {
		t.Errorf("应提示 code 只能用一次: %s", rec.Body.String())
	}
	if s.Login.get(sid) != nil {
		t.Error("兑换失败后会话应被清理")
	}
}

// TestLoginSessionsPrunesExpiredOnGet covers session reclamation: an abandoned login must not hold the port forever.
//
// put only cleans up when "starting a new login"; if the user abandons a login and never starts another, the session and its loopback
// listener will stay forever. Every poll / open of the login page goes through get, so reclaim it there as a side effect.
func TestLoginSessionsPrunesExpiredOnGet(t *testing.T) {
	ls := newLoginSessions()
	old := &loginSession{ID: "old", Created: time.Now()}
	fresh := &loginSession{ID: "fresh", Created: time.Now()}
	ls.put(old)
	ls.put(fresh)

	// After put, age old further to simulate "left unattended for a while".
	ls.mu.Lock()
	old.Created = time.Now().Add(-2 * loginTTL)
	ls.mu.Unlock()

	// The requested session is returned as-is even if expired: callers rely on it to distinguish expired from 404.
	if got := ls.get("old"); got == nil {
		t.Fatal("被请求的过期会话仍应返回（由调用方判定 expired）")
	}
	ls.mu.Lock()
	_, keptOld := ls.m["old"]
	ls.mu.Unlock()
	if !keptOld {
		t.Fatal("被请求的会话不应在 get 里被删掉")
	}

	// Request with a different id: old is now a "sibling session" and should be reclaimed; fresh is unaffected.
	if got := ls.get("fresh"); got == nil {
		t.Fatal("未过期会话不应被回收")
	}
	ls.mu.Lock()
	_, stillOld := ls.m["old"]
	_, keptFresh := ls.m["fresh"]
	ls.mu.Unlock()
	if stillOld {
		t.Fatal("过期兄弟会话应被 get 回收")
	}
	if !keptFresh {
		t.Fatal("fresh 不应被回收")
	}
}
