package deepai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

// errWriter returns an error after the cumulative write budget is exhausted, used to cover the SSE write error branch.
type errWriter struct{ budget int }

func (w *errWriter) Write(p []byte) (int, error) {
	if w.budget <= 0 {
		return 0, errors.New("写入失败")
	}
	if len(p) > w.budget {
		n := w.budget
		w.budget = 0
		return n, errors.New("写入失败")
	}
	w.budget -= len(p)
	return len(p), nil
}

func TestWriteSSEFormatAndErrors(t *testing.T) {
	var buf strings.Builder
	if err := WriteSSE(&buf, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("WriteSSE: %v", err)
	}
	if got := buf.String(); got != "data: {\"a\":1}\n\n" {
		t.Fatalf("WriteSSE 输出 = %q", got)
	}

	// Three write segments ("data: " 6 bytes / payload 7 bytes / "\n\n" 2 bytes)
	// If any segment fails, the error must be propagated to the caller (otherwise the stream is silently truncated).
	for _, budget := range []int{0, 6, 13} {
		if err := WriteSSE(&errWriter{budget: budget}, []byte(`{"a":1}`)); err == nil {
			t.Fatalf("写入预算 %d 字节用尽时应报错", budget)
		}
	}
}

func TestWriteSSEEventFormatAndErrors(t *testing.T) {
	var buf strings.Builder
	if err := WriteSSEEvent(&buf, "content_block_delta", []byte(`{"x":1}`)); err != nil {
		t.Fatalf("WriteSSEEvent: %v", err)
	}
	want := "event: content_block_delta\ndata: {\"x\":1}\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("WriteSSEEvent 输出 = %q，期望 %q", got, want)
	}

	// An empty event name means no event line is written (the shape of OpenAI-compatible endpoints).
	buf.Reset()
	if err := WriteSSEEvent(&buf, "", []byte(`[DONE]`)); err != nil {
		t.Fatalf("WriteSSEEvent: %v", err)
	}
	if got := buf.String(); got != "data: [DONE]\n\n" {
		t.Fatalf("空 event 输出 = %q", got)
	}

	// "event: e\n" 9 + "data: " 6 + payload 7 + "\n\n" 2 = 24 bytes.
	for _, budget := range []int{0, 5, 9, 15, 22} {
		if err := WriteSSEEvent(&errWriter{budget: budget}, "e", []byte(`{"x":1}`)); err == nil {
			t.Fatalf("写入预算 %d 字节用尽时应报错", budget)
		}
	}
}

// TestBodyRejectedRequires400 pins down that only a 400 with a matching message counts as a request body shape rejection.
func TestBodyRejectedRequires400(t *testing.T) {
	msg := `{"error":{"message":"This request is not one DeepAI Code sends.","code":"invalid_request_error"}}`
	if !mustErr(t, http.StatusBadRequest, msg).BodyRejected() {
		t.Fatal("400 + 固定文案应判定为请求体拒绝")
	}
	if mustErr(t, http.StatusInternalServerError, msg).BodyRejected() {
		t.Fatal("非 400 不应判定为请求体拒绝")
	}
	if mustErr(t, http.StatusBadRequest, `{"error":{"message":"bad param"}}`).BodyRejected() {
		t.Fatal("文案不匹配不应判定为请求体拒绝")
	}
	if mustErr(t, http.StatusBadRequest, ``).BodyRejected() {
		t.Fatal("空 body 不应判定为请求体拒绝")
	}
}

// TestBodyRejectedAcceptsNewMessage covers the new message measured on 2026-11-13:
// The upstream changed the message for the same threshold to "This request cannot be accepted.",
// If it is missed, ChatBodyFallback will not retry and the caller gets a 400 directly.
func TestBodyRejectedAcceptsNewMessage(t *testing.T) {
	msg := `{"error":{"message":"This request cannot be accepted.","type":"invalid_request_error","code":"invalid_request_error"}}`
	if !mustErr(t, http.StatusBadRequest, msg).BodyRejected() {
		t.Fatal("400 + 新文案（cannot be accepted）应判定为请求体拒绝")
	}
	if mustErr(t, http.StatusForbidden, msg).BodyRejected() {
		t.Fatal("非 400 不应判定为请求体拒绝")
	}
}

// TestHomeDirFallback covers the fallback order when the home directory is unavailable.
func TestHomeDirFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		// On Windows, os.UserHomeDir reads USERPROFILE; it only fails when both are cleared.
		t.Setenv("USERPROFILE", "")
		t.Setenv("HOMEDRIVE", "")
		t.Setenv("HOMEPATH", "")
	} else {
		t.Setenv("HOME", "")
	}
	// Whether or not a real home directory is obtained, returning an empty string is not allowed.
	if got := HomeDir(); got == "" {
		t.Fatal("HomeDir 不应为空串")
	}
}

// TestDoTransportError covers connection failure: the error must be propagated as-is and classified as a transient fault.
func TestDoTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // The port is closed, so subsequent requests are refused.

	c := NewClient()
	c.Origin = url

	_, _, err := c.FetchModels(context.Background(), nil)
	if err == nil {
		t.Fatal("连接失败应返回错误")
	}
	if !IsTransient(err) {
		t.Fatalf("连接被拒应判定为瞬时故障，得到 %v", err)
	}

	if _, err := c.OpenModel(context.Background(), &Account{APIKey: "k"}, PathChat, []byte(`{}`), "aff"); err == nil {
		t.Fatal("OpenModel 连接失败应返回错误")
	}
	if _, err := c.Theos(context.Background(), &Account{APIKey: "k"}); err == nil {
		t.Fatal("Theos 连接失败应返回错误")
	}
	if _, err := c.postJSON(context.Background(), nil, PathWebSearch, []byte(`{}`), "联网搜索"); err == nil {
		t.Fatal("postJSON 连接失败应返回错误")
	}
	if _, err := c.Transcribe(context.Background(), nil, "", "", []byte("x")); err == nil {
		t.Fatal("Transcribe 连接失败应返回错误")
	}
	if _, err := c.ExchangeCode(context.Background(), "code"); err == nil {
		t.Fatal("ExchangeCode 连接失败应返回错误")
	}
	if _, err := c.DetectLane(context.Background(), nil); err == nil {
		t.Fatal("DetectLane 连接失败应返回错误")
	}
	if c.Lane() != LaneVersion {
		t.Fatalf("探测失败不应改车道，实际 %q", c.Lane())
	}
}

// TestDetectLaneServerErrorFallback covers the all-5xx case: a fallback error should be returned,
// because 5xx means upstream flakiness rather than a wrong lane (lastErr is empty in this case).
func TestDetectLaneServerErrorFallback(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := c.DetectLane(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "所有候选车道都被上游拒绝") {
		t.Fatalf("全 5xx 应给出兜底错误，得到 %v", err)
	}
}

// TestImportCredentialFileReadError covers the path exists but cannot be read case.
func TestImportCredentialFileReadError(t *testing.T) {
	dir := t.TempDir()
	if _, err := importCredentialFile(dir); err == nil ||
		!strings.Contains(err.Error(), "读取凭据失败") {
		t.Fatalf("目录当作凭据文件应报读取失败，得到 %v", err)
	}
	if _, err := importCredentialFile(dir + string(os.PathSeparator) + "missing.bin"); err == nil {
		t.Fatal("不存在的文件应报错")
	}
}

func TestAccountFromCredentialsPropagatesInvalidKey(t *testing.T) {
	_, err := AccountFromCredentials(&Credentials{APIKey: "bad key"}, "desktop")
	if err == nil || !strings.Contains(err.Error(), "格式非法") {
		t.Fatalf("非法 Key 应透传校验错误，得到 %v", err)
	}

	acct, err := AccountFromCredentials(&Credentials{APIKey: "sk-x", Username: "u", IsPro: true}, "")
	if err != nil {
		t.Fatalf("AccountFromCredentials: %v", err)
	}
	if acct.Source != "manual" {
		t.Fatalf("空 source 应回落 manual，得到 %q", acct.Source)
	}
	if acct.Name != "u" || !acct.IsPro {
		t.Fatalf("字段丢失: %+v", acct)
	}
}

// TestHasSystemMessageIgnoresMalformed covers dirty data in messages.
func TestHasSystemMessageIgnoresMalformed(t *testing.T) {
	cases := []struct {
		name string
		msgs []any
		want bool
	}{
		{"空", nil, false},
		{"只有 user", []any{map[string]any{"role": "user"}}, false},
		{"system", []any{map[string]any{"role": "system"}}, true},
		{"developer 也算", []any{map[string]any{"role": "developer"}}, true},
		{"非对象条目被跳过", []any{"oops", 42, nil, map[string]any{"role": "system"}}, true},
		{"role 非字符串", []any{map[string]any{"role": 7}}, false},
		{"大小写敏感", []any{map[string]any{"role": "System"}}, false},
	}
	for _, c := range cases {
		if got := hasSystemMessage(c.msgs); got != c.want {
			t.Errorf("%s: hasSystemMessage()=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestChatBodyFallbackDirtyMessages covers that fallback still works when non-object entries are mixed into messages.
func TestChatBodyFallbackDirtyMessages(t *testing.T) {
	body := []byte(`{"model":"m","messages":["oops",{"role":"user","content":"hi"}]}`)
	out, ok := ChatBodyFallback(body, "自定义提示词")
	if !ok {
		t.Fatal("messages 非空即应兜底")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("兜底结果不是合法 JSON: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("应插入一条 system，实际 %d 条", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "自定义提示词" {
		t.Fatalf("自定义提示词未生效: %#v", first)
	}

	if _, ok := ChatBodyFallback([]byte(`not json`), ""); ok {
		t.Fatal("非法 JSON 不应兜底")
	}
	if _, ok := ChatBodyFallback([]byte(`{"messages":{}}`), ""); ok {
		t.Fatal("messages 不是数组时不应兜底")
	}
	if _, ok := ChatBodyFallback([]byte(`{"messages":[]}`), ""); ok {
		t.Fatal("messages 为空时不应兜底")
	}
}

// TestDetectMachineRawNonWindows only runs on non-Windows: the Windows branch will spin up
// powershell (12s timeout), not suitable for unit tests.
func TestDetectMachineRawNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 分支依赖 powershell / reg，跳过")
	}
	got := detectMachineRaw()
	if got == "" {
		t.Log("未探测到机器标识（容器无 SMBIOS / 无 machine-id），回落空串符合预期")
		return
	}
	if got != strings.ToLower(got) {
		t.Fatalf("机器标识应统一小写，得到 %q", got)
	}
	if strings.ContainsAny(got, " \t\r\n") {
		t.Fatalf("机器标识不应含空白，得到 %q", got)
	}
}
