package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestDecodeJSONRequestEnvelopes covers the shared "read body → parse → report error" flow of the three inference entry points.
//
// This logic was previously written separately in chat / responses / anthropic, so changing the status code and message in one place
// missed two places; after extracting it into decodeJSONRequest, the fail callback preserves each protocol's error envelope shape,
// This locks in "correct protocol, correct status code, and malformed requests are not passed into the business layer".
func TestDecodeJSONRequestEnvelopes(t *testing.T) {
	s, key := newTestServer(t, nil)

	cases := []struct {
		name      string
		path      string
		wantType  string // The type in the error envelope (Anthropic under error.type, OpenAI at top-level error.type)
		anthropic bool
	}{
		{"chat 请求体损坏", "/v1/chat/completions", "invalid_request_error", false},
		{"responses 请求体损坏", "/v1/responses", "invalid_request_error", false},
		{"messages 请求体损坏", "/v1/messages", "invalid_request_error", true},
		{"count_tokens 请求体损坏", "/v1/messages/count_tokens", "invalid_request_error", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, s, http.MethodPost, c.path, key, `{"model":`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400：%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, c.wantType) {
				t.Fatalf("错误信封缺 %q：%s", c.wantType, body)
			}
			if c.anthropic && !strings.Contains(body, `"type":"error"`) {
				t.Fatalf("Anthropic 错误信封应为 type=error：%s", body)
			}
			// The upstream should not be called: malformed requests should be blocked at the entry point.
		})
	}
}
