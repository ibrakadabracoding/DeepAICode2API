package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"deepaicode2api/internal/deepai"
)

// fixtureModels is a catalog covering various shapes: free / paid / per-second billing / responses endpoint.
func fixtureModels() []deepai.Model {
	return []deepai.Model{
		{
			ID: "free-chat", Name: "Free Chat", Reasoning: true,
			Input: []string{"text", "image"}, ContextWindow: 1048576, MaxTokens: 131072,
			TheosPerRequest: "0",
		},
		{
			ID: "paid-chat", Name: "Paid Chat", Premier: true, Reasoning: true,
			Input: []string{"text"}, ContextWindow: 1000000, MaxTokens: 64000,
			TheosPerRequest: "15", TheosPerSecond: map[string]string{"standard": "0.5"},
		},
		{
			ID: "paid-responses", Name: "Paid Responses", Premier: true,
			Input: []string{"text", "image"}, API: "openai-responses",
			TheosPerRequest: "50",
		},
	}
}

// TestPublicModelsExposeDeepAIMeta pins the deepai metadata fields of /v1/models.
//
// Clients (including scripts outside the console) rely on it to determine context window and billing;
// a missing field doesn't error, it just silently becomes undefined.
func TestPublicModelsExposeDeepAIMeta(t *testing.T) {
	s, key := newTestServer(t, nil)
	s.Store.SetModels(fixtureModels(), "free-chat")

	rec := do(t, s, http.MethodGet, "/v1/models", key, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d：%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string         `json:"id"`
			Object string         `json:"object"`
			Owned  string         `json:"owned_by"`
			Deep   map[string]any `json:"deepai"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if payload.Object != "list" || len(payload.Data) != 3 {
		t.Fatalf("列表结构异常: object=%q count=%d", payload.Object, len(payload.Data))
	}

	for _, m := range payload.Data {
		if m.Object != "model" || m.Owned != "deepai" {
			t.Errorf("%s: object/owned_by 错误", m.ID)
		}
		for _, field := range []string{
			"name", "reasoning", "input", "context_window", "max_tokens",
			"premier", "free", "api", "theos_per_request", "theos_per_second",
		} {
			if _, ok := m.Deep[field]; !ok {
				t.Errorf("%s: deepai 元信息缺少 %q", m.ID, field)
			}
		}
	}

	// free must share its source with Model.Free(): a paid model must not be marked free even if it is cheap.
	byID := map[string]map[string]any{}
	for _, m := range payload.Data {
		byID[m.ID] = m.Deep
	}
	if byID["free-chat"]["free"] != true {
		t.Error("free-chat 应标记 free=true")
	}
	if byID["paid-chat"]["free"] != false {
		t.Error("paid-chat 应标记 free=false")
	}
	sec, ok := byID["paid-chat"]["theos_per_second"].(map[string]any)
	if !ok || sec["standard"] != "0.5" {
		t.Fatalf("theos_per_second 未透出: %#v", byID["paid-chat"]["theos_per_second"])
	}
}

// TestAdminModelsExposeFreeAndPerSecond pins the fields of the console model list.
//
// The console's "billing" column reads the backend-provided free directly (it no longer computes the
// criteria itself), and the "per-second billing" column reads theosPerSecond; missing either leaves
// the column silently blank.
func TestAdminModelsExposeFreeAndPerSecond(t *testing.T) {
	s, _ := newTestServer(t, nil)
	s.Store.SetModels(fixtureModels(), "free-chat")

	rec := do(t, s, http.MethodGet, "/admin/api/models", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/models = %d", rec.Code)
	}

	var got struct {
		Models []map[string]any `json:"models"`
		Def    string           `json:"default_model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got.Def != "free-chat" {
		t.Fatalf("default_model = %q", got.Def)
	}
	if len(got.Models) != 3 {
		t.Fatalf("模型数 = %d，期望 3", len(got.Models))
	}

	for _, m := range got.Models {
		if _, ok := m["free"]; !ok {
			t.Errorf("%v: 缺少 free（控制台计费列依赖它）", m["id"])
		}
		if _, ok := m["theosPerSecond"]; !ok {
			t.Errorf("%v: 缺少 theosPerSecond（控制台每秒计费列依赖它）", m["id"])
		}
		if _, ok := m["contextWindow"]; !ok {
			t.Errorf("%v: 缺少 contextWindow", m["id"])
		}
	}

	freeCount := 0
	for _, m := range got.Models {
		if m["free"] == true {
			freeCount++
		}
	}
	if freeCount != 1 {
		t.Fatalf("免费模型数 = %d，期望 1（只有 free-chat）", freeCount)
	}
}

// TestAdminModelsFallbackWhenCatalogEmpty confirms that on a fresh install with an unreachable
// upstream, both the console and /v1/models fall back to the same fallback model (there was once a
// case where one was empty while the other was not).
func TestAdminModelsFallbackWhenCatalogEmpty(t *testing.T) {
	s, key := newTestServer(t, nil)

	var admin struct {
		Models  []map[string]any `json:"models"`
		Default string           `json:"default_model"`
	}
	if err := json.Unmarshal(do(t, s, http.MethodGet, "/admin/api/models", "", "").Body.Bytes(), &admin); err != nil {
		t.Fatalf("管理端响应解析失败: %v", err)
	}
	var pub struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(do(t, s, http.MethodGet, "/v1/models", key, "").Body.Bytes(), &pub); err != nil {
		t.Fatalf("公开端响应解析失败: %v", err)
	}

	if len(admin.Models) != 1 || admin.Models[0]["id"] != deepai.DefaultModel {
		t.Fatalf("管理端兜底目录异常: %#v", admin.Models)
	}
	if admin.Default != deepai.DefaultModel {
		t.Fatalf("管理端 default_model = %q", admin.Default)
	}
	if len(pub.Data) != 1 || pub.Data[0].ID != deepai.DefaultModel {
		t.Fatalf("公开端兜底目录异常: %#v", pub.Data)
	}
}

// Since 2026-11-14 the upstream /models requires an Api-Key: catalog refresh must borrow an account's
// credentials, and when the account pool is empty it must give an actionable error instead of passing
// the upstream 401 straight to the console.
func TestRefreshModelsNeedsAccount(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)
	s.Store.UpdateAccounts(func(a *deepai.Account) bool {
		a.Enabled = false
		return true
	})

	_, err := s.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("没有可用账号时应报错")
	}
	if !strings.Contains(err.Error(), "Api-Key") {
		t.Fatalf("错误应说明需要 Api-Key，实际 %v", err)
	}
	if got := countPath(u, deepai.ModelBasePath+deepai.PathModels); got != 0 {
		t.Fatalf("没有凭据时不应发请求，实际 %d 次", got)
	}
}

// With an account available, its key must be included (otherwise the catalog fetch is guaranteed to 401).
func TestRefreshModelsSendsAccountKey(t *testing.T) {
	u := newWarmupUpstream(t)
	s, _ := newTestServer(t, u)

	models, err := s.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("目录不应为空")
	}
	if key := u.last(t).Header.Get(deepai.HeaderKeyName); key != "upstream-key" {
		t.Fatalf("拉目录的 Api-Key = %q，期望 upstream-key", key)
	}
}

// probeAccount prefers an "ok + has machine fingerprint" account; otherwise it falls back to any
// enabled account with a key.
func TestProbeAccountPrefersReadyWithMachine(t *testing.T) {
	s, _ := newTestServer(t, nil) // acc-1: ok, but no machine fingerprint
	s.Store.UpsertAccount(&deepai.Account{
		ID: "with-machine", Name: "wm", APIKey: "k-wm", Enabled: true, Status: "ok",
		MachineID: "11111111-1111-1111-1111-111111111111",
	})
	s.Store.UpsertAccount(&deepai.Account{
		ID: "disabled", Name: "off", APIKey: "k-off", Enabled: false,
	})

	got := s.probeAccount()
	if got == nil || got.ID != "with-machine" {
		t.Fatalf("应优先选带机器指纹的就绪号，实际 %+v", got)
	}

	s.Store.UpdateAccounts(func(a *deepai.Account) bool {
		if a.ID == "with-machine" || a.ID == "disabled" {
			a.Enabled = false
		}
		return true
	})
	if got := s.probeAccount(); got == nil || got.ID != "acc-1" {
		t.Fatalf("应回落到仍启用的 acc-1，实际 %+v", got)
	}
}
