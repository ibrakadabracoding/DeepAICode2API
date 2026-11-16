package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/store"
)

// overview should contain only metadata that does not grow with the number of
// accounts, never the account list.
func TestOverviewEndpointShape(t *testing.T) {
	s, key := newTestServer(t, nil)

	rec := do(t, s, http.MethodGet, "/admin/api/overview", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("解析: %v", err)
	}
	for _, k := range []string{"accounts", "ready", "keys", "models", "settings", "stats", "default_model", "machine_id"} {
		if _, ok := d[k]; !ok {
			t.Fatalf("overview 缺少字段 %q", k)
		}
	}
	// The account list must go through the paginated endpoint; overview should not send it.
	if _, ok := d["items"]; ok {
		t.Fatal("overview 不应包含 items")
	}
	if got, _ := d["accounts"].(float64); got < 1 {
		t.Fatalf("accounts 计数异常: %v", d["accounts"])
	}
}

// The paginated endpoint should honor offset/limit and return the filtered total.
func TestAccountsPageEndpoint(t *testing.T) {
	s, key := newTestServer(t, nil)
	for _, id := range []string{"b1", "b2"} {
		s.Store.UpsertAccount(&deepai.Account{ID: id, APIKey: "sk-" + id, Group: "g2", Enabled: true})
	}

	rec := do(t, s, http.MethodGet, "/admin/api/accounts?offset=0&limit=2", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	var page struct {
		Items  []map[string]any `json:"items"`
		Total  int              `json:"total"`
		Offset int              `json:"offset"`
		Limit  int              `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("total = %d，期望 3", page.Total)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items = %d，期望 2", len(page.Items))
	}
	// The payload is a projection: it must include a masked key and must not contain the password.
	if _, ok := page.Items[0]["key"]; !ok {
		t.Fatal("缺少 key 投影")
	}
	if _, ok := page.Items[0]["password"]; ok {
		t.Fatal("不应下发密码")
	}

	// Group filtering.
	rec = do(t, s, http.MethodGet, "/admin/api/accounts?group=g2", key, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("分组过滤 total = %d，期望 2", page.Total)
	}
}

// An idle refresh should hit the ETag and return 304.
func TestOverviewETag(t *testing.T) {
	s, key := newTestServer(t, nil)

	rec := do(t, s, http.MethodGet, "/admin/api/overview", key, "")
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("overview 缺少 ETag")
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/overview", nil)
	req.RemoteAddr = "127.0.0.1:34567"
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req)

	if rec2.Code != http.StatusNotModified {
		t.Fatalf("命中 ETag 应返回 304，实际 %d", rec2.Code)
	}
}

// The compatibility endpoint supports ?accounts=0 to fetch only metadata.
func TestStateEndpointCanOmitAccounts(t *testing.T) {
	s, key := newTestServer(t, nil)

	rec := do(t, s, http.MethodGet, "/admin/api/state?accounts=0", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if v, ok := d["accounts"]; ok && v != nil {
		t.Fatalf("accounts 应为空，实际 %#v", v)
	}
	if _, ok := d["stats"]; !ok {
		t.Fatal("元信息仍应存在")
	}
}

// The logs endpoint should accept filter parameters.
func TestLogsEndpointFilters(t *testing.T) {
	s, key := newTestServer(t, nil)
	s.Store.Record(store.LogEntry{Time: 1, Endpoint: "chat", Status: 200}, 0)
	s.Store.Record(store.LogEntry{Time: 2, Endpoint: "audio", Status: 500}, 0)

	rec := do(t, s, http.MethodGet, "/admin/api/logs?endpoint=chat", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var d struct {
		Logs []map[string]any `json:"logs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(d.Logs) != 1 || d.Logs[0]["endpoint"] != "chat" {
		t.Fatalf("过滤未生效: %#v", d.Logs)
	}
}

// TestLogsEndpointRejectsUnsupportedMethod guards the allowed method set for
// /admin/api/logs.
//
// Previously only DELETE was rejected: POST / PUT fell through to the "query" branch and
// echoed the logs back unchanged. Callers seeing 200 easily assumed the write had
// succeeded when nothing had happened.
func TestLogsEndpointRejectsUnsupportedMethod(t *testing.T) {
	s, key := newTestServer(t, nil)
	s.Store.Record(store.LogEntry{Time: 1, Endpoint: "chat", Status: 200}, 0)

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		rec := do(t, s, m, "/admin/api/logs", key, "{}")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /admin/api/logs = %d，期望 405", m, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `"logs"`) {
			t.Errorf("%s /admin/api/logs 仍回了日志体：%s", m, rec.Body.String())
		}
	}
	// GET / DELETE must keep working as before.
	if rec := do(t, s, http.MethodGet, "/admin/api/logs", key, ""); rec.Code != http.StatusOK {
		t.Errorf("GET = %d，期望 200", rec.Code)
	}
	if rec := do(t, s, http.MethodDelete, "/admin/api/logs", key, ""); rec.Code != http.StatusOK {
		t.Errorf("DELETE = %d，期望 200", rec.Code)
	}
}
