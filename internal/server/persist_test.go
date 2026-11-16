package server

import (
	"net/http"
	"os"
	"testing"
)

// breakStatePath makes the next Store.Save fail necessarily: replace the state file with a directory of the same name.
//
// rename(temp file → target) fails on both Linux and Windows, which is more reliable than changing permissions
// (on Windows the read-only bit on a directory basically does not block writes).
func breakStatePath(t *testing.T, s *Server) {
	t.Helper()
	path := s.Store.Path()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("移除状态文件: %v", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("创建同名目录: %v", err)
	}
}

// TestAdminMutationReportsSaveFailure locks down "user-initiated changes must report persist-to-disk failures".
//
// Historical behavior: `_ = s.Store.Save()` swallowed the error, so when the disk was full or the file was in use the API returned 200,
// and the change only lived in memory, lost on restart with no log trace whatsoever.
func TestAdminMutationReportsSaveFailure(t *testing.T) {
	s, _ := newTestServer(t, nil)
	before := len(s.Store.Keys())
	breakStatePath(t, s)

	rec := do(t, s, http.MethodPost, "/admin/api/keys", "", `{"name":"落盘失败"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（保存失败必须上报）：%s", rec.Code, rec.Body.String())
	}
	if got := len(s.Store.Keys()); got != before+1 {
		t.Fatalf("密钥数 = %d，期望 %d（内存改动已发生，只是没落盘）", got, before+1)
	}
}

// TestAdminKeysRejectsMalformedJSON covers "malformed request bodies being silently treated as empty requests".
//
// Historical behavior: decode errors were ignored → sending the wrong fields to POST /admin/api/keys would create a
// key with an empty name and return 200.
func TestAdminKeysRejectsMalformedJSON(t *testing.T) {
	s, _ := newTestServer(t, nil)
	before := len(s.Store.Keys())

	rec := do(t, s, http.MethodPost, "/admin/api/keys", "", `{"name":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400：%s", rec.Code, rec.Body.String())
	}
	if got := len(s.Store.Keys()); got != before {
		t.Fatalf("畸形请求不应创建密钥：%d → %d", before, got)
	}

	// An empty body is still valid: equivalent to "creating a new key with default parameters".
	rec = do(t, s, http.MethodPost, "/admin/api/keys", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("空体状态码 = %d，期望 200：%s", rec.Code, rec.Body.String())
	}
	if got := len(s.Store.Keys()); got != before+1 {
		t.Fatalf("空体应创建一把密钥：%d → %d", before, got)
	}
}
