package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
	"deepaicode2api/internal/store"
)

// TestReqLogDoneIsIdempotent locks down that "the same request is logged only once".
//
// Each handler passes reqLog by value, so done may be called once in multiple branches (early return +
// defer fallback). Without an idempotency flag, one request gets logged twice, doubling both throughput and error rate.
func TestReqLogDoneIsIdempotent(t *testing.T) {
	s, _ := newTestServer(t, nil)
	rl := s.newReqLog("sk-test", nil, "glm-5.3-flash", "chat", false, time.Now(), 12)

	rl.done(3, Usage{PromptTokens: 5, CompletionTokens: 2}, nil)
	rl.done(3, Usage{PromptTokens: 5, CompletionTokens: 2}, nil) // The second call should be ignored
	copied := rl
	copied.done(3, Usage{}, nil) // The copy shares the same flag and is likewise ignored

	logs := s.Store.Logs(10)
	if len(logs) != 1 {
		t.Fatalf("日志条数 = %d，期望 1（done 必须幂等）", len(logs))
	}
	if logs[0].Status != http.StatusOK || logs[0].PromptLen != 12 {
		t.Fatalf("日志字段不对: %+v", logs[0])
	}
}

// TestStatusOfUsesUpstreamStatusCode ensures the status code in the log matches what the client receives.
//
// The old implementation logged all errors as 502, while upstream 400 / 426 / 429 are passed through by writeUpstreamErr
// verbatim to the client: the console's "status ≥" filter and error attribution were therefore distorted for a long time.
func TestStatusOfUsesUpstreamStatusCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"无错误", nil, http.StatusOK},
		{"上游 400", &deepai.HTTPError{Op: "聊天", Status: 400, Body: []byte(`{"error":"bad"}`)}, 400},
		{"上游 426", &deepai.HTTPError{Op: "聊天", Status: 426}, 426},
		{"上游 429", &deepai.HTTPError{Op: "聊天", Status: 429}, 429},
		{"包装后的上游 403", wrap(&deepai.HTTPError{Op: "聊天", Status: 403}), 403},
		{"普通错误回 502", errors.New("boom"), http.StatusBadGateway},
		// Client disconnects / our own timeouts must align with writeUpstreamErr's actual response:
		// the former writes nothing (the connection is already gone), the latter writes 504. Previously everything was logged as 502, which made
		// "upstream 5xx", "client left", and "we timed out" all blur together in the logs.
		{"上下文取消记 499", context.Canceled, store.StatusClientGone},
		{"包装后的上下文取消", wrap(context.Canceled), store.StatusClientGone},
		{"上下文超时记 504", context.DeadlineExceeded, http.StatusGatewayTimeout},
		{"异常状态码回 502", &deepai.HTTPError{Op: "x", Status: 0}, http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := httpx.StatusOf(c.err); got != c.want {
				t.Fatalf("statusOf = %d，期望 %d", got, c.want)
			}
		})
	}
}

// wrap adds a %w layer to the error; verify that statusOf uses errors.As rather than a type assertion.
func wrap(err error) error { return errors.Join(errors.New("外层"), err) }

// TestPassthroughNonJSONIsLogged covers the branch where "the upstream response is neither JSON nor a stream".
//
// This branch used to return directly: the client got the raw 200, but there was **not a single log entry**,
// so "upstream occasionally returns an HTML login page" was completely invisible in the console.
func TestPassthroughNonJSONIsLogged(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><body>login required</body></html>")
	})
	s, key := newTestServer(t, u)
	s.Store.SetModels([]deepai.Model{{ID: "resp-model", API: "openai-responses"}}, "resp-model")

	rec := do(t, s, http.MethodPost, "/v1/responses", key,
		`{"model":"resp-model","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（应保持原样透传 200）：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "login required") {
		t.Fatalf("原文未透传: %s", rec.Body.String())
	}

	logs := s.Store.Logs(10)
	if len(logs) != 1 {
		t.Fatalf("日志条数 = %d，期望 1（该分支不能漏记）", len(logs))
	}
	got := logs[0]
	if got.Status != http.StatusOK {
		t.Fatalf("日志状态 = %d，期望 200（客户端实际收到 200）", got.Status)
	}
	if !strings.Contains(got.Err, "不是合法 JSON") {
		t.Fatalf("日志未记录失败原因: %+v", got)
	}
	if stats := s.Store.Stats(); stats.FailedRequests != 1 {
		t.Fatalf("FailedRequests = %d，期望 1（Err 非空即计入失败）", stats.FailedRequests)
	}
}
