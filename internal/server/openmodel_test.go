package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"deepaicode2api/internal/deepai"
)

// inflightCount reads the current number of in-flight account slots (test only).
func inflightCount(s *Server) int {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return len(s.inflight)
}

// TestShapeFallbackRetryKeepsIdleTimeoutGuard covers the "request body shape fallback" 2xx return path.
//
// Both 2xx return paths (normal response / fallback retry) must wrap the same two layers: idle timeout + release
// account slot. Historically, the fallback path only wrapped releaseBody; when the upstream hangs after sending the first frame, the slot and connection will
// stay hung until the client disconnects-exactly the pitfall described in idle.go, just a missing branch.
func TestShapeFallbackRetryKeepsIdleTimeoutGuard(t *testing.T) {
	var chatCalls atomic.Int32
	rejected := `{"error":{"message":"This request is not one DeepAI Code sends.","code":"invalid_request_error"}}`
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// ModelURL appends the /deepai_code prefix; here we compare the full path.
		if r.URL.Path != "/deepai_code"+deepai.PathChat {
			http.NotFound(w, r)
			return
		}
		if chatCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, rejected)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	s, _ := newTestServer(t, u)

	body, err := json.Marshal(map[string]any{
		"model":    "glm-5.3-flash",
		"stream":   false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("序列化请求: %v", err)
	}

	resp, acct, err := s.openModel(context.Background(), deepai.PathChat, body, "", "", "", false)
	if err != nil {
		t.Fatalf("openModel: %v", err)
	}
	if acct == nil {
		t.Fatal("成功路径应返回账号")
	}
	if got := chatCalls.Load(); got != 2 {
		t.Fatalf("形状回退应重试一次，实际 chat 调用 %d 次", got)
	}
	if _, ok := resp.Body.(*idleTimeoutBody); !ok {
		t.Fatalf("回退路径的响应体未包空闲超时（实际 %T）", resp.Body)
	}
	if n := inflightCount(s); n != 1 {
		t.Fatalf("请求进行中在途槽位 = %d，期望 1", n)
	}

	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("关闭响应体: %v", err)
	}
	if n := inflightCount(s); n != 0 {
		t.Fatalf("关闭后在途槽位 = %d，期望 0（releaseBody 未生效）", n)
	}
}
