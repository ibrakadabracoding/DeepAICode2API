package deepai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain fixes the machine fingerprint so every test doesn't have to read SMBIOS (which starts powershell on Windows).
// MachineID uses a sync.Once and must be set before any Client is constructed.
func TestMain(m *testing.M) {
	os.Setenv("DEEPAI_MACHINE_ID", "11111111-2222-3333-4444-555555555555")
	os.Exit(m.Run())
}

func mustErr(t *testing.T, status int, body string) *HTTPError {
	t.Helper()
	return &HTTPError{Op: "上游推理", Status: status, Body: []byte(body)}
}

func TestHTTPErrorRetryable(t *testing.T) {
	cases := []struct {
		status int
		want   bool
		why    string
	}{
		{400, false, "请求本身的问题"},
		{404, false, "模型不存在，重试无意义"},
		{422, false, "参数非法"},
		{401, true, "鉴权失败可换号"},
		{403, true, ""},
		{409, true, "容量拒绝"},
		{429, true, "限流"},
		{500, true, ""},
		{502, true, ""},
		{503, true, ""},
		{504, true, ""},
		{418, false, "未知 4xx 不重试"},
	}
	for _, c := range cases {
		if got := mustErr(t, c.status, "{}").Retryable(); got != c.want {
			t.Errorf("status=%d Retryable()=%v，期望 %v（%s）", c.status, got, c.want, c.why)
		}
	}
}

func TestHTTPErrorAuthFailed(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{401, `{}`, true},
		{403, `{"error":{"code":"auth_required"}}`, true},
		{403, `{"error":{"code":"invalid_api_key"}}`, true},
		{403, `{"error":{"code":"email_unverified"}}`, true},
		{403, `{"error":{"code":"session_busy"}}`, false},
		{500, `{}`, false},
	}
	for _, c := range cases {
		if got := mustErr(t, c.status, c.body).AuthFailed(); got != c.want {
			t.Errorf("status=%d body=%s AuthFailed()=%v，期望 %v", c.status, c.body, got, c.want)
		}
	}
}

func TestHTTPErrorCapacityWaiting(t *testing.T) {
	busy := mustErr(t, http.StatusConflict, `{"error":{"code":"session_busy"}}`)
	if !busy.CapacityWaiting() {
		t.Fatal("409 + session_busy 应判定为容量等待")
	}
	if !busy.Retryable() {
		t.Fatal("容量等待必须可重试")
	}

	conflict := mustErr(t, http.StatusConflict, `{"error":{"code":"duplicate"}}`)
	if conflict.CapacityWaiting() {
		t.Fatal("非 session_busy 的 409 不应判定为容量等待")
	}
	if got := mustErr(t, 429, `{"error":{"code":"session_busy"}}`).CapacityWaiting(); got {
		t.Fatal("非 409 即使 code=session_busy 也不算容量等待")
	}
}

func TestHTTPErrorCodeAndMessage(t *testing.T) {
	e := mustErr(t, 409, `{"error":{"message":"模型正忙","code":"session_busy"}}`)
	if e.Code() != "session_busy" {
		t.Fatalf("Code() = %q", e.Code())
	}
	if got := e.Error(); !strings.Contains(got, "模型正忙") {
		t.Fatalf("Error() 应优先使用上游 message，得到 %q", got)
	}

	if got := mustErr(t, 500, "not json").Code(); got != "" {
		t.Fatalf("非 JSON body 的 Code() 应为空，得到 %q", got)
	}
}

func TestHTTPErrorTruncatesLongBody(t *testing.T) {
	// When the upstream errors out it returns HTML / stack traces; don't stuff the whole thing into the error string and write it to the log.
	long := strings.Repeat("A", 1000)
	got := mustErr(t, 502, long).Error()
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("长 body 应被截断并以省略号结尾: %q", got)
	}
	if strings.Count(got, "A") > 301 {
		t.Fatalf("截断长度不合理：%d 个 A", strings.Count(got, "A"))
	}

	if got := mustErr(t, 500, "   ").Error(); !strings.Contains(got, "HTTP 500") {
		t.Fatalf("空 body 应回退到状态码: %q", got)
	}
}

func TestRetryAfter(t *testing.T) {
	resp := func(v string) *http.Response {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return &http.Response{Header: h}
	}
	fallback := 7 * time.Second

	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", fallback},
		{"3", 3 * time.Second},
		{"1", time.Second},
		{"999", 30 * time.Second}, // Cap at 30s to avoid being dragged down by the upstream
		{"0", time.Second},        // Consistent with the desktop client: clamp to the 1s lower bound
		{"-5", time.Second},       // Negative numbers are likewise clamped to 1s
		{"abc", fallback},         // Fall back on non-numeric values
		{"Wed, 21 Oct", fallback}, // HTTP-date is not supported; fall back
	}
	for _, c := range cases {
		if got := RetryAfter(resp(c.header), fallback); got != c.want {
			t.Errorf("Retry-After=%q → %v，期望 %v", c.header, got, c.want)
		}
	}
}

// newTestClient builds a client pointed at a local fake upstream.
func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClientWithFingerprint("", "test-ua/1.0")
	c.Origin = srv.URL
	c.Machine = "machine-fingerprint"
	return c, srv
}

func TestClientSendsRequiredHeaders(t *testing.T) {
	var got http.Header
	var gotPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	acct := &Account{APIKey: "sk-upstream-key"}
	_, err := c.OpenModel(context.Background(), acct, PathChat, []byte(`{"model":"x"}`), "affinity-1")
	if err != nil {
		t.Fatalf("OpenModel: %v", err)
	}

	if gotPath != ModelBasePath+PathChat {
		t.Fatalf("请求路径 = %q，期望 %q", gotPath, ModelBasePath+PathChat)
	}
	if v := got.Get(HeaderKeyName); v != "sk-upstream-key" {
		t.Fatalf("%s = %q", HeaderKeyName, v)
	}
	if v := got.Get(HeaderLane); v != LaneVersion {
		t.Fatalf("%s = %q，期望 %q", HeaderLane, v, LaneVersion)
	}
	if LaneVersion != "6" {
		t.Fatalf("车道版本 = %q，0.3.37 客户端使用 6", LaneVersion)
	}
	if inst := got.Get(HeaderInstance); len(inst) != 32 || strings.Trim(inst, "0123456789abcdef") != "" {
		t.Fatalf("%s = %q，期望 32 位小写 hex", HeaderInstance, inst)
	}
	if v := got.Get(HeaderMachine); v != "machine-fingerprint" {
		t.Fatalf("%s = %q", HeaderMachine, v)
	}
	if v := got.Get(HeaderAffinity); v != "affinity-1" {
		t.Fatalf("%s = %q", HeaderAffinity, v)
	}
	if v := got.Get("User-Agent"); v != "test-ua/1.0" {
		t.Fatalf("User-Agent = %q", v)
	}
	if v := got.Get("Content-Type"); v != "application/json" {
		t.Fatalf("Content-Type = %q", v)
	}
}

func TestClientEmptyAffinityOmitsHeader(t *testing.T) {
	var got http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{}`)
	})
	if _, err := c.OpenModel(context.Background(), &Account{APIKey: "k"}, PathChat, []byte(`{}`), ""); err != nil {
		t.Fatalf("OpenModel: %v", err)
	}
	if v, ok := got[HeaderAffinity]; ok {
		t.Fatalf("空 affinity 不应发送 %s，实际 %v", HeaderAffinity, v)
	}
}

// The upstream 302s to the login page for an invalid Key: we neither follow it (which would disguise an authentication failure as 200),
// nor pass through the bare 3xx (an API client can't handle a 302 with no Location),
// but normalize it to 401 auth_required - only then can the caller mark the account as failed and switch accounts.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var finalHits int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			finalHits++
			_, _ = io.WriteString(w, "login page")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})

	_, err := c.OpenModel(context.Background(), &Account{APIKey: "bad"}, PathChat, []byte(`{}`), "")
	if err == nil {
		t.Fatal("302 应归一成鉴权错误")
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("错误类型 = %T，期望 *HTTPError", err)
	}
	if he.Status != http.StatusUnauthorized || !he.AuthFailed() || he.Code() != CodeAuthRequired {
		t.Fatalf("应归一成 401 auth_required，实际 %+v", he)
	}
	if finalHits != 0 {
		t.Fatal("不应跟随重定向到登录页")
	}
}

func TestFetchModels(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ModelBasePath+PathModels {
			t.Errorf("路径 = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[{"id":"m1","name":"M1"},{"id":"m2","name":"M2"}],"default":"m2"}`)
	})

	models, def, err := c.FetchModels(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "m1" {
		t.Fatalf("模型目录解析错误: %#v", models)
	}
	if def != "m2" {
		t.Fatalf("default = %q，期望 m2", def)
	}
	if len(models[0].Input) != 1 || models[0].Input[0] != "text" {
		t.Fatalf("缺失 input 应补为 [text]，实际 %#v", models[0].Input)
	}
}

// Credential contract for /models and lane probing (reversed as of 2026-11-14).
//
// The old contract was "deliberately send no credentials at all", on the grounds that the upstream catalog doesn't vary by account and that with or without a Key it
// returns the same thing. **Empirically broken as of 2026-11-14**: without an Api-Key it's always 401 auth_required.
// Now: if an account is given, its Key must be sent (otherwise catalog / lane probing is all 401),
// but headers for "inference sessions" like check-in or beacon still shouldn't be sent - they're unrelated to fetching the catalog,
// sending them only makes the upstream treat it as a suspicious check-in call.
func TestModelsAndLaneProbeCarryAccountKeyOnly(t *testing.T) {
	var mu sync.Mutex
	var headers []http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		_, _ = io.WriteString(w, `{"models":[{"id":"m1","name":"M1"}],"default":"m1"}`)
	})

	acct := &Account{APIKey: "sk-catalog", MachineID: "11111111-1111-1111-1111-111111111111"}
	if _, _, err := c.FetchModels(context.Background(), acct); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if _, err := c.DetectLane(context.Background(), acct); err != nil {
		t.Fatalf("DetectLane: %v", err)
	}
	// Anonymous calls are still allowed (for warm-up / connectivity probing): just don't send an Api-Key.
	if _, _, err := c.FetchModels(context.Background(), nil); err != nil {
		t.Fatalf("FetchModels(nil): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(headers) < 3 {
		t.Fatalf("请求数 = %d，期望至少 3", len(headers))
	}
	last := headers[len(headers)-1]
	if got := last.Get(HeaderKeyName); got != "" {
		t.Fatalf("acct=nil 时不应发送 Api-Key，实际 %q", got)
	}
	for _, h := range headers[:len(headers)-1] {
		if got := h.Get(HeaderKeyName); got != "sk-catalog" {
			t.Fatalf("带账号时 Api-Key = %q，期望 sk-catalog", got)
		}
	}
	for _, h := range headers {
		for _, name := range []string{HeaderCheckIn, HeaderBeacon} {
			if v := h.Get(name); v != "" {
				t.Fatalf("%s 不应出现在目录 / 车道探测请求上，实际 %q", name, v)
			}
		}
	}
}

func TestFetchModelsNormalizes(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// No default, duplicate IDs, empty IDs.
		_, _ = io.WriteString(w, `{"models":[{"id":"a"},{"id":"a"},{"id":""},{"id":"b","input":["text","image"]}]}`)
	})
	models, def, err := c.FetchModels(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("去重后应为 2 个模型，实际 %d: %#v", len(models), models)
	}
	if def != "a" {
		t.Fatalf("无 default 时应回退到首个模型，实际 %q", def)
	}
	if len(models[1].Input) != 2 {
		t.Fatalf("已声明的 input 不应被覆盖: %#v", models[1].Input)
	}
}

func TestFetchModelsEmptyIsError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[]}`)
	})
	if _, _, err := c.FetchModels(context.Background(), nil); err == nil {
		t.Fatal("空模型目录应返回错误")
	}
}

func TestTheosParsesAllowance(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"1.50000","granted":"0.00000","lane_version":2}`)
	})
	al, err := c.Theos(context.Background(), &Account{APIKey: "k"})
	if err != nil {
		t.Fatalf("Theos: %v", err)
	}
	if !al.Enabled || al.Balance != "1.50000" || al.Granted != "0.00000" {
		t.Fatalf("配额解析错误: %#v", al)
	}
	if al.CheckedAt == 0 {
		t.Fatal("CheckedAt 应被填充")
	}
}

func TestTheosReturnsHTTPError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key"}}`)
	})
	_, err := c.Theos(context.Background(), &Account{APIKey: "bad"})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	var httpErr *HTTPError
	if !asHTTPError(err, &httpErr) {
		t.Fatalf("错误类型 = %T，期望 *HTTPError", err)
	}
	if !httpErr.AuthFailed() {
		t.Fatal("应判定为鉴权失败")
	}
}

// asHTTPError is a thin wrapper around errors.As, so test files don't repeatedly import errors and confuse the semantics.
func asHTTPError(err error, target **HTTPError) bool {
	e, ok := err.(*HTTPError)
	if ok {
		*target = e
	}
	return ok
}

func TestJSONBodyIsVerbatim(t *testing.T) {
	var got []byte
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{}`)
	})
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"你好"}]}`)
	if _, err := c.OpenModel(context.Background(), &Account{APIKey: "k"}, PathChat, body, ""); err != nil {
		t.Fatalf("OpenModel: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("请求体被改写:\n got=%s\nwant=%s", got, body)
	}
	var probe map[string]any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
}

// IsTransient decides whether the critical path should back off and retry; a misjudgment costs one wasted retry
// or wrongly mark an account that should have been recovered as dead.
func TestIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"400 参数错误", mustErr(t, 400, `{"error":{"code":"invalid"}}`), false},
		{"401 鉴权失败", mustErr(t, 401, `{}`), false},
		{"403", mustErr(t, 403, `{}`), false},
		{"404", mustErr(t, 404, `{}`), false},
		{"429 限流", mustErr(t, 429, `{}`), true},
		{"408", mustErr(t, 408, `{}`), true},
		{"500", mustErr(t, 500, `{}`), true},
		{"503", mustErr(t, 503, `{}`), true},
		{"unexpected eof", errors.New(`Get "https://x": unexpected EOF`), true},
		{"no exit node", errors.New(`Get "https://x": No exit node`), true},
		{"tls handshake", errors.New("net/http: TLS handshake timeout"), true},
		{"connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"调用方取消", context.Canceled, false},
		{"超时", context.DeadlineExceeded, true},
		{"未知错误", errors.New("something else"), false},
	}
	for _, c := range cases {
		if got := IsTransient(c.err); got != c.want {
			t.Errorf("%s: IsTransient=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// Lane 4 check-in handshake: /theos issues check_in, and the next request returns the nonce and heartbeat.
func TestTheosCheckInHandshake(t *testing.T) {
	var (
		mu     sync.Mutex
		header []http.Header
	)
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		header = append(header, r.Header.Clone())
		n := len(header)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{"enabled":true,"balance":"1","granted":"0","lane_version":4,`+
				`"check_in":{"nonce":"0123456789abcdef0123456789abcdef","seconds":600}}`)
			return
		}
		_, _ = io.WriteString(w, `{"enabled":true,"lane_version":4}`)
	})

	acct := &Account{APIKey: "k-handshake"}
	if _, err := c.Theos(context.Background(), acct); err != nil {
		t.Fatalf("Theos: %v", err)
	}
	ci := c.CheckIn(acct)
	if ci == nil || ci.Nonce != "0123456789abcdef0123456789abcdef" || ci.Seconds != 600 {
		t.Fatalf("签到挑战解析错误: %#v", ci)
	}
	if got := header[0].Get(HeaderCheckIn); got != "" {
		t.Fatalf("首次请求不应带签到 nonce，实际 %q", got)
	}
	if b := header[0].Get(HeaderBeacon); !strings.HasPrefix(b, "v="+AppVersion+";up=") || !strings.Contains(b, "pi=") {
		t.Fatalf("心跳格式不对: %q", b)
	}

	if _, err := c.Theos(context.Background(), acct); err != nil {
		t.Fatalf("第二次 Theos: %v", err)
	}
	if got := header[1].Get(HeaderCheckIn); got != ci.Nonce {
		t.Fatalf("第二次请求应回传 nonce，实际 %q", got)
	}
}

// 403 rejections also carry check_in, which must be consumed as well, otherwise check-in will never succeed.
func TestTheosCapturesCheckInFromRefusal(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"social","code":"social_login_required"},`+
			`"check_in":{"nonce":"ffffffffffffffffffffffffffffffff","seconds":600}}`)
	})
	acct := &Account{APIKey: "k-blocked"}
	_, err := c.Theos(context.Background(), acct)
	var he *HTTPError
	if !asHTTPError(err, &he) || !he.SocialLoginRequired() {
		t.Fatalf("应返回 social_login_required，实际 %v", err)
	}
	if got := c.CheckIn(acct); got == nil || got.Seconds != 600 {
		t.Fatalf("拒绝响应里的 check_in 未被保存: %#v", got)
	}
}

// Policy errors are terminal states: they cannot be retried with another account, nor treated as retryable 403.
func TestHTTPErrorPolicyClassification(t *testing.T) {
	social := mustErr(t, http.StatusForbidden, `{"error":{"code":"social_login_required"}}`)
	if !social.SocialLoginRequired() || social.Retryable() {
		t.Fatalf("social_login_required 应识别为策略拦截且不可重试: %+v", social)
	}
	machine := mustErr(t, http.StatusBadRequest, `{"error":{"code":"machine_required"}}`)
	if !machine.MachineRequired() || machine.Retryable() {
		t.Fatalf("machine_required 应不可重试: %+v", machine)
	}
	lane := mustErr(t, http.StatusUpgradeRequired, `{"error":{"code":"lane_version_unsupported"}}`)
	if !lane.LaneUnsupported() || lane.Retryable() {
		t.Fatalf("lane_version_unsupported 应识别为车道错误且不可重试: %+v", lane)
	}
	// Ordinary 403s can still switch accounts.
	if !mustErr(t, http.StatusForbidden, `{}`).Retryable() {
		t.Fatal("普通 403 应保持可重试")
	}
}

// 400 shape rejection -> desktop field whitelist + append system message.
func TestChatBodyFallback(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"user":"u1","messages":[{"role":"user","content":"hi"}],"tools":[]}`)
	rejected := mustErr(t, http.StatusBadRequest,
		`{"error":{"message":"This request is not one DeepAI Code sends.","code":"invalid_request_error"}}`)
	if !rejected.BodyRejected() {
		t.Fatal("应识别为请求体形状拒绝")
	}

	out, ok := ChatBodyFallback(body, "")
	if !ok {
		t.Fatal("可解析的 messages 应能兜底")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("兜底结果不是合法 JSON: %v", err)
	}
	if _, exists := got["user"]; exists {
		t.Fatal("白名单外的字段应被剔除")
	}
	if got["stream"] != true || got["model"] != "m" {
		t.Fatalf("白名单字段应保留: %#v", got)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应插入一条 system 消息，实际 %d 条", len(msgs))
	}
	if role, _ := msgs[0].(map[string]any)["role"].(string); role != "system" {
		t.Fatalf("第一条应为 system，实际 %q", role)
	}

	if _, ok := ChatBodyFallback([]byte(`{"model":"m"}`), ""); ok {
		t.Fatal("没有 messages 时不应兜底")
	}
}

// When the wording changes again, 400 + code=invalid_request_error must still be handled as a shape rejection;
// Other 400s (context overflow, authentication, etc.) must not be misjudged, otherwise a modified body will be retried in vain once.
func TestBodyRejectedFallsBackToInvalidRequestCode(t *testing.T) {
	unknown := mustErr(t, http.StatusBadRequest,
		`{"error":{"message":"Something new entirely.","type":"invalid_request_error",`+
			`"code":"invalid_request_error"}}`)
	if !unknown.BodyRejected() {
		t.Fatal("400 + invalid_request_error 应按请求体形状拒绝处理")
	}

	context := mustErr(t, http.StatusBadRequest,
		`{"error":{"message":"maximum context length exceeded","code":"context_length_exceeded"}}`)
	if context.BodyRejected() {
		t.Fatal("上下文超限不应触发形状重试")
	}

	invalidKey := mustErr(t, http.StatusBadRequest, `{"error":{"code":"invalid_api_key"}}`)
	if invalidKey.BodyRejected() {
		t.Fatal("非形状错误不应触发形状重试")
	}

	if mustErr(t, http.StatusUnprocessableEntity, `{"error":{"code":"invalid_request_error"}}`).BodyRejected() {
		t.Fatal("非 400 不应按形状拒绝处理")
	}
}

// Requests that already have a system message should not have another inserted.
func TestChatBodyFallbackKeepsExistingSystem(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)
	out, ok := ChatBodyFallback(body, "")
	if !ok {
		t.Fatal("应能兜底")
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if msgs, _ := got["messages"].([]any); len(msgs) != 2 {
		t.Fatalf("不应插入额外 system，实际 %d 条", len(msgs))
	}
}

// The inference client must match the desktop: Node fetch does not send User-Agent / sec-ch-ua /
// Accept-Language; adding them at the gateway is equivalent to declaring "I am not the client" on the inference endpoint.
func TestInferenceClientOmitsBrowserFingerprint(t *testing.T) {
	var got http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	c.BrowserFingerprint = false

	acct := &Account{
		APIKey:         "k1",
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/156.0.0.0",
		AcceptLanguage: "en-US,en;q=0.9",
		SecCHUA:        `"Chromium";v="156"`,
		Platform:       `"Windows"`,
	}
	if _, err := c.OpenModel(context.Background(), acct, PathChat, []byte(`{"model":"x"}`), ""); err != nil {
		t.Fatalf("OpenModel: %v", err)
	}
	// The desktop uses Node fetch: the UA is node, with no sec-ch-ua / Accept-Language fingerprint.
	if v := got.Get("User-Agent"); v != "node" {
		t.Errorf("推理请求的 UA 应为 node，实际 %q", v)
	}
	if v := got.Get("Accept-Language"); v != "*" {
		t.Errorf("Accept-Language 应为 *，实际 %q", v)
	}
	for _, h := range []string{"sec-ch-ua", "sec-ch-ua-platform", "sec-ch-ua-mobile"} {
		if v := got.Get(h); v != "" {
			t.Errorf("推理请求不应携带 %s: %q", h, v)
		}
	}
}

// The registration path continues to carry the browser fingerprint (consistent with the characteristics of web registration).
func TestRegisterClientKeepsBrowserFingerprint(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClientWithFingerprint("", "agent-ua/1.0")
	c.Origin = srv.URL
	c.Machine = "machine-fingerprint"

	acct := &Account{APIKey: "k2", SecCHUA: `"Chromium";v="156"`, Platform: `"Windows"`, AcceptLanguage: "en-US"}
	if _, err := c.OpenModel(context.Background(), acct, PathChat, []byte(`{"model":"x"}`), ""); err != nil {
		t.Fatalf("OpenModel: %v", err)
	}
	if v := got.Get("User-Agent"); v != "agent-ua/1.0" {
		t.Fatalf("注册客户端应带 UA，实际 %q", v)
	}
	if got.Get("sec-ch-ua") == "" || got.Get("sec-ch-ua-platform") == "" || got.Get("Accept-Language") == "" {
		t.Fatalf("注册客户端应带齐指纹头: %#v", got)
	}
}

// Shape fallback must not incidentally alter sampling conditions: seed / service_tier / stream_options
// would change sampling and usage reporting, and fingerprint-based detection may even switch to a different candidate model because of it.
// store, however, is a field the desktop sends on every request and must likewise be preserved.
func TestChatBodyFallbackKeepsSamplingFields(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"seed":7,"service_tier":"flex","stream":true,"store":false,` +
		`"stream_options":{"include_usage":true},"presence_penalty":0.5,"user":"u1"}`)
	out, ok := ChatBodyFallback(body, "")
	if !ok {
		t.Fatal("应能兜底")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("兜底结果不是 JSON: %v", err)
	}
	for _, k := range []string{"seed", "service_tier", "stream_options", "store"} {
		if _, exists := got[k]; !exists {
			t.Errorf("白名单丢了 %s，会改变采样条件或用量上报", k)
		}
	}
	if v, exists := got["store"]; exists && v != false {
		t.Errorf("store 应原值保留，实际 %#v", v)
	}
	for _, k := range []string{"presence_penalty", "user"} {
		if _, exists := got[k]; exists {
			t.Errorf("非桌面字段 %s 仍应被剥离", k)
		}
	}
}

// GET requests must also match the desktop shape: the header literal of requestDeepAIJson, for bodyless
// /models, /theos, and /get_usage_summary, likewise carries Content-Type: application/json,
// and /get_usage_summary does not override the UA (only /gravity-display-proxy uses DeepAI-Code).
func TestGetRequestsMatchDesktopHeaders(t *testing.T) {
	type seen struct {
		path        string
		contentType string
		userAgent   string
	}
	var got []seen
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, seen{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("User-Agent")})
		if r.URL.Path == ModelBasePath+PathModels {
			_, _ = io.WriteString(w, `{"models":[{"id":"glm-5.3-flash","name":"GLM","reasoning":true,"input":["text"],"contextWindow":1000,"maxTokens":100}],"default":"glm-5.3-flash"}`)
			return
		}
		_, _ = io.WriteString(w, `{"enabled":true}`)
	})
	c.BrowserFingerprint = false // Inference shape: the UA should be node
	acct := &Account{APIKey: "k"}

	if _, _, err := c.FetchModels(context.Background(), nil); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if _, err := c.Theos(context.Background(), acct); err != nil {
		t.Fatalf("Theos: %v", err)
	}
	if _, err := c.UsageSummary(context.Background(), acct); err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}

	for _, s := range got {
		if s.contentType != "application/json" {
			t.Errorf("GET %s 的 Content-Type = %q，期望 application/json", s.path, s.contentType)
		}
		if s.userAgent != "node" {
			t.Errorf("GET %s 的 User-Agent = %q，期望 node", s.path, s.userAgent)
		}
	}
}

// After obtaining the nonce, it must be sent back as soon as possible: the upstream treats "completing check-in" as an admission condition for the real model branch,
// Pulling only without sending back is equivalent to not checking in (docs/recon.md §5.1).
//
// When there is no ackedNonce, EnsureCheckIn returns directly within seconds/2 (300s),
// In the first 5 minutes after a cold start, inference has not checked in yet.
func TestEnsureCheckInAcksPromptly(t *testing.T) {
	var (
		mu     sync.Mutex
		header []http.Header
	)
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		header = append(header, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"enabled":true,"balance":"1","granted":"0","lane_version":4,`+
			`"check_in":{"nonce":"0123456789abcdef0123456789abcdef","seconds":600}}`)
	})
	acct := &Account{APIKey: "k-ack"}
	ctx := context.Background()

	// Simulate startup probing: only the challenge is obtained, not yet sent back.
	if _, err := c.Theos(ctx, acct); err != nil {
		t.Fatalf("Theos: %v", err)
	}
	if got := header[0].Get(HeaderCheckIn); got != "" {
		t.Fatalf("首次请求不应带 nonce，实际 %q", got)
	}

	// Supplement check-in before inference: it must be sent back immediately, rather than waiting for the half-life.
	c.EnsureCheckIn(ctx, acct)
	if len(header) != 2 {
		t.Fatalf("应立刻补一次 /theos，实际请求数 %d", len(header))
	}
	if got := header[1].Get(HeaderCheckIn); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("补签到必须回传 nonce，实际 %q", got)
	}
	if b := header[1].Get(HeaderBeacon); !strings.Contains(b, "pi=") {
		t.Fatalf("补签到应带心跳，实际 %q", b)
	}

	// Already sent back and half-life not reached → do not disturb the upstream again.
	c.EnsureCheckIn(ctx, acct)
	if len(header) != 2 {
		t.Fatalf("已签到后不应重复请求，实际 %d", len(header))
	}
}

// Check-in being rate-limited (check_in_refused) does not mean the account is bad: inference still returns 200, it just falls on the substitute branch.
// It must be distinguishable from social_login_required; otherwise, a good account will be marked as error.
func TestCheckInRefusedDetection(t *testing.T) {
	e := mustErr(t, http.StatusForbidden, `{"error":{"type":"check_in_refused","code":"check_in_refused"}}`)
	if !IsCheckInRefused(e) {
		t.Fatal("应识别为 check_in_refused")
	}
	if e.SocialLoginRequired() {
		t.Fatal("不应被当成 social_login_required")
	}
	if e.AuthFailed() {
		t.Fatal("不应被当成鉴权失败")
	}
	// Other 403s should not be misjudged.
	if IsCheckInRefused(mustErr(t, http.StatusForbidden, `{"error":{"code":"social_login_required"}}`)) {
		t.Fatal("social_login_required 不应识别为 check_in_refused")
	}
}

// After check_in_refused, check-in must be silently locked: the 403 still issues a new check_in,
// If not locked, the needAck branch will make EnsureCheckIn, every 30s (checkInMinInterval),
// hit /theos again - creating a check-in storm on its own (docs/recon.md §5.1).
func TestEnsureCheckInHonorsRefusalLock(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"check_in_refused","code":"check_in_refused"},`+
			`"check_in":{"nonce":"0123456789abcdef0123456789abcdef","seconds":600}}`)
	})
	acct := &Account{APIKey: "k-refused"}
	ctx := context.Background()
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
	// Bypass the checkInMinInterval (30s) throttling: this test case only verifies the "lock" itself.
	resetAttempt := func() {
		st := c.stateFor(acct)
		c.checkIns.mu.Lock()
		st.attemptAt = time.Time{}
		c.checkIns.mu.Unlock()
	}

	c.EnsureCheckIn(ctx, acct)
	if got := count(); got != 1 {
		t.Fatalf("首次应触发一次 /theos，实际 %d", got)
	}
	until := c.CheckInRefusedUntil(acct)
	if until.IsZero() || time.Until(until) < 5*time.Hour {
		t.Fatalf("应锁约 6h，实际剩余 %v", time.Until(until))
	}

	resetAttempt()
	c.EnsureCheckIn(ctx, acct)
	if got := count(); got != 1 {
		t.Fatalf("锁期内不应再打 /theos，实际 %d 次", got)
	}

	// After the lock expires, allow one more retry (if still rejected, renew the lock).
	st := c.stateFor(acct)
	c.checkIns.mu.Lock()
	st.refusedUntil = time.Now().Add(-time.Second)
	c.checkIns.mu.Unlock()
	resetAttempt()
	c.EnsureCheckIn(ctx, acct)
	if got := count(); got != 2 {
		t.Fatalf("锁到期后应允许重试，实际 %d 次", got)
	}
}

// Lane detection: only a lane accepted by /models counts as a hit; after a hit, all requests automatically carry it.
func TestDetectLanePicksAcceptedLane(t *testing.T) {
	var mu sync.Mutex
	var lanes []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		lane := r.Header.Get(HeaderLane)
		mu.Lock()
		lanes = append(lanes, lane)
		mu.Unlock()
		if lane != LaneVersion {
			w.WriteHeader(http.StatusUpgradeRequired)
			_, _ = io.WriteString(w, `{"error":{"code":"lane_version_unsupported"}}`)
			return
		}
		if r.URL.Path == ModelBasePath+PathModels {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"id":"glm-5.3-flash","name":"GLM","reasoning":true,`+
				`"input":["text"],"contextWindow":1000,"maxTokens":100}],"default":"glm-5.3-flash"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"enabled":true}`)
	})

	lane, err := c.DetectLane(context.Background(), nil)
	if err != nil {
		t.Fatalf("DetectLane: %v", err)
	}
	if lane != LaneVersion || c.Lane() != LaneVersion {
		t.Fatalf("命中车道 = %q / Lane() = %q，期望 %q", lane, c.Lane(), LaneVersion)
	}

	// Subsequent requests directly carry the new lane.
	if _, err := c.Theos(context.Background(), &Account{APIKey: "k"}); err != nil {
		t.Fatalf("Theos: %v", err)
	}
	mu.Lock()
	last := lanes[len(lanes)-1]
	mu.Unlock()
	if last != LaneVersion {
		t.Fatalf("后续请求车道 = %q，期望 %q", last, LaneVersion)
	}
}

// Detection must start from the compile-time lane, and must not try the old lane first.
//
// `/models` is very tolerant of old lanes (in the 0.3.37 era, both 5 / 6 returned 200); if the candidate sequence starts with the old value,
// DetectLane hits the old lane on the first try, and the gateway will keep impersonating the old client.
func TestDetectLaneStartsAtCompiledLane(t *testing.T) {
	want := laneCandidates()
	if len(want) == 0 || want[0] != LaneVersion {
		t.Fatalf("候选序列 = %v，第一项应为编译期车道 %q", want, LaneVersion)
	}

	var mu sync.Mutex
	var probed []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		probed = append(probed, r.Header.Get(HeaderLane))
		mu.Unlock()
		// All lanes accept: the probe should stop at the compile-time lane on the first attempt.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
	})

	lane, err := c.DetectLane(context.Background(), nil)
	if err != nil {
		t.Fatalf("DetectLane: %v", err)
	}
	if lane != LaneVersion {
		t.Fatalf("命中车道 = %q，期望编译期车道 %q", lane, LaneVersion)
	}
	mu.Lock()
	got := append([]string(nil), probed...)
	mu.Unlock()
	if len(got) != 1 || got[0] != LaneVersion {
		t.Fatalf("探测序列 = %v，应只试编译期车道 %q", got, LaneVersion)
	}
}

// When all candidate lanes are rejected, return an error without changing the current lane.
func TestDetectLaneAllRejected(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = io.WriteString(w, `{"error":{"code":"lane_version_unsupported"}}`)
	})
	if _, err := c.DetectLane(context.Background(), nil); err == nil {
		t.Fatal("全部候选被拒应返回错误")
	}
	if c.Lane() != LaneVersion {
		t.Fatalf("失败不应改车道，实际 %q", c.Lane())
	}
}
