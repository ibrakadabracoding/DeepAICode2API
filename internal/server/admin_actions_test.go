package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"deepaicode2api/internal/deepai"
)

// This set of tests covers 5 actions that had **zero coverage** before the refactor: import / manual / remove / toggle /
// clear-errors. After the table-driven refactor (admin_actions.go), the input struct for each action
// changed, so these tests pin down their behavior.

// theosUpstream returns a fake upstream that has quota for /theos.
func theosUpstream(t *testing.T) *upstream {
	t.Helper()
	return newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, deepai.PathTheos) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"enabled":true,"balance":"200","granted":"200"}`))
			return
		}
		http.NotFound(w, r)
	})
}

func TestAdminActionManualThenToggleClearRemove(t *testing.T) {
	s, _ := newTestServer(t, theosUpstream(t))

	// manual: create a new account.
	rec := do(t, s, http.MethodPost, "/admin/api/accounts", "",
		`{"action":"manual","api_key":"sk-manual-1","name":"手工号","group":"g9"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual = %d：%s", rec.Code, rec.Body.String())
	}
	type manualResp struct {
		OK  bool   `json:"ok"`
		New bool   `json:"new"`
		ID  string `json:"id"`
	}
	created := decodeJSON[manualResp](t, rec.Body.Bytes())
	if !created.OK || !created.New || created.ID == "" {
		t.Fatalf("manual 响应异常: %+v", created)
	}
	acct := s.Store.Account(created.ID)
	if acct == nil {
		t.Fatal("manual 之后账号不存在")
	}
	if acct.Name != "手工号" || acct.Group != "g9" || acct.Source != "manual" {
		t.Fatalf("账号字段未按请求写入: %+v", acct)
	}
	if !acct.Enabled {
		t.Fatal("新账号默认应启用")
	}
	s.waitQuotaReprobe() // Wait for the async liveness probe to finish to avoid racing with TempDir cleanup

	// toggle: flip the enabled state.
	if rec := do(t, s, http.MethodPost, "/admin/api/accounts", "",
		`{"action":"toggle","id":"`+created.ID+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("toggle = %d", rec.Code)
	}
	if s.Store.Account(created.ID).Enabled {
		t.Fatal("toggle 未停用账号")
	}
	do(t, s, http.MethodPost, "/admin/api/accounts", "", `{"action":"toggle","id":"`+created.ID+`"}`)
	if !s.Store.Account(created.ID).Enabled {
		t.Fatal("toggle 第二次未恢复启用")
	}

	// clear-errors: clear the failure count and error state.
	s.Store.UpdateAccount(created.ID, func(a *deepai.Account) {
		a.Failures = 7
		a.Status = "error"
		a.LastErr = "boom"
	})
	if rec := do(t, s, http.MethodPost, "/admin/api/accounts", "",
		`{"action":"clear-errors","id":"`+created.ID+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("clear-errors = %d", rec.Code)
	}
	cleared := s.Store.Account(created.ID)
	if cleared.Failures != 0 || cleared.Status != "unknown" || cleared.LastErr != "" {
		t.Fatalf("clear-errors 未清干净: %+v", cleared)
	}

	// remove: deleting an existing account returns ok=true, a non-existent one returns ok=false.
	type okResp struct {
		OK bool `json:"ok"`
	}
	rec = do(t, s, http.MethodPost, "/admin/api/accounts", "", `{"action":"remove","id":"`+created.ID+`"}`)
	removed := decodeJSON[okResp](t, rec.Body.Bytes())
	if !removed.OK {
		t.Fatalf("remove 已存在的账号应 ok=true: %s", rec.Body.String())
	}
	if s.Store.Account(created.ID) != nil {
		t.Fatal("remove 之后账号仍然存在")
	}

	rec = do(t, s, http.MethodPost, "/admin/api/accounts", "", `{"action":"remove","id":"no-such-id"}`)
	removed = decodeJSON[okResp](t, rec.Body.Bytes())
	if removed.OK {
		t.Fatalf("remove 不存在的账号应 ok=false: %s", rec.Body.String())
	}
}

func TestAdminActionImportFromDir(t *testing.T) {
	s, _ := newTestServer(t, nil)

	base := t.TempDir()
	dir := filepath.Join(base, "userdata", "secrets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cred := `{"apiKey":"sk-from-desktop","username":"desk@example.com"}`
	if err := os.WriteFile(filepath.Join(dir, deepai.CredentialFile), []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}

	type importResp struct {
		OK       bool `json:"ok"`
		Count    int  `json:"count"`
		Imported []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
			New  bool   `json:"new"`
		} `json:"imported"`
	}

	body := mustMarshal(t, map[string]string{"action": "import", "dir": base}) // Windows paths contain backslashes, so they must go through JSON encoding
	rec := do(t, s, http.MethodPost, "/admin/api/accounts", "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d：%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON[importResp](t, rec.Body.Bytes())
	if !got.OK || got.Count != 1 || len(got.Imported) != 1 {
		t.Fatalf("import 结果异常: %+v", got)
	}
	if !got.Imported[0].New {
		t.Fatal("首次导入应为新建")
	}
	if !strings.HasSuffix(got.Imported[0].Path, deepai.CredentialFile) {
		t.Fatalf("导入路径 = %q", got.Imported[0].Path)
	}
	if acct := s.Store.Account(got.Imported[0].ID); acct == nil || acct.Source != "desktop" {
		t.Fatalf("导入账号未落库或来源错误: %+v", acct)
	}

	// Import again: the same Key goes through Upsert, not a new create.
	rec = do(t, s, http.MethodPost, "/admin/api/accounts", "", body)
	again := decodeJSON[importResp](t, rec.Body.Bytes())
	if len(again.Imported) != 1 || again.Imported[0].New {
		t.Fatalf("重复导入应复用已有账号: %+v", again)
	}
}

func TestAdminActionRejectsBadInput(t *testing.T) {
	s, _ := newTestServer(t, nil)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"未知 action", `{"action":"nope"}`, http.StatusBadRequest},
		{"缺 action", `{}`, http.StatusBadRequest},
		{"JSON 非法", `{`, http.StatusBadRequest},
		{"manual 缺 key", `{"action":"manual"}`, http.StatusBadRequest},
		{"import 目录不存在", `{"action":"import","dir":"/no/such/dir"}`, http.StatusBadRequest},
		{"set-group 缺 group", `{"action":"set-group","id":"x"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		if rec := do(t, s, http.MethodPost, "/admin/api/accounts", "", c.body); rec.Code != c.want {
			t.Errorf("%s: 状态码 = %d，期望 %d：%s", c.name, rec.Code, c.want, rec.Body.String())
		}
	}

	// The error message for an unknown action must include the action name to make console troubleshooting easier.
	rec := do(t, s, http.MethodPost, "/admin/api/accounts", "", `{"action":"nope"}`)
	if !strings.Contains(rec.Body.String(), "未知操作: nope") {
		t.Fatalf("错误文案未带 action: %s", rec.Body.String())
	}

	// Verbs other than read-only methods should return 405.
	if rec := do(t, s, http.MethodPut, "/admin/api/accounts", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT = %d，期望 405", rec.Code)
	}
}
