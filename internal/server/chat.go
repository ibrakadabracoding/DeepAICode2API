package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
)

// handleChatCompletions is the main entry point for /v1/chat/completions.
//
// The upstream itself is an OpenAI-compatible interface, so this gateway acts as a "transparent proxy with account pool and quota governance":
// it rewrites the model name, injects the DeepAI authentication header, normalizes the reasoning chain fields, and records usage.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.WriteErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	start := time.Now()

	payload, ok := httpx.DecodeJSONRequest[map[string]any](w, r, func(code int, msg string) {
		httpx.WriteErr(w, code, msg, "invalid_request_error")
	})
	if !ok {
		return
	}
	if msgs, ok := payload["messages"].([]any); !ok || len(msgs) == 0 {
		httpx.WriteErr(w, http.StatusBadRequest, "messages 不能为空", "invalid_request_error")
		return
	}

	model, modelOK := s.resolveModelOK(requestModelName(payload))
	if !modelOK {
		httpx.WriteErr(w, http.StatusNotFound, s.unknownModelMessage(requestModelName(payload)), "invalid_request_error")
		return
	}
	payload["model"] = model.ID
	stream, _ := payload["stream"].(bool)
	// stream=false must be sent explicitly: when this field is absent, the upstream defaults to returning SSE,
	// whereas in the OpenAI spec the default is equivalent to false; this mismatch causes non-streaming requests to receive
	// `data: {...}` that cannot be parsed.
	payload["stream"] = stream
	affinity := affinityFor(r, payload)
	group := s.requestGroup(r)

	body, err := json.Marshal(payload)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, "请求序列化失败", "invalid_request_error")
		return
	}
	key := extractKey(r)
	promptLen := estimatePromptLen(payload)

	ctx := r.Context()
	if !stream {
		// Add an overall budget for non-streaming requests: MaxRetries × Retry-After can theoretically hang for over 90s.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, nonStreamBudget)
		defer cancel()
	}
	resp, acct, err := s.openModel(ctx, deepai.PathChat, body, affinity, group, key, stream)
	rl := s.newReqLog(key, acct, model.ID, "chat", stream, start, promptLen)
	if err != nil {
		rl.done(0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	if stream {
		s.streamChat(w, resp.Body, rl)
		return
	}

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		rl.done(0, Usage{}, err)
		httpx.WriteErr(w, http.StatusBadGateway, "读取上游响应失败: "+err.Error(), "upstream_error")
		return
	}
	var out map[string]any
	if err := json.Unmarshal(rawResp, &out); err != nil {
		// The upstream occasionally returns non-JSON (HTML gateway pages, plain-text rate-limit notices, SSE frames, etc.).
		// Log the response type and the leading fragment; otherwise all you have is "not valid JSON" with no way to investigate.
		log.Printf("上游响应解析失败：account=%s status=%d ct=%q len=%d head=%q",
			acctLabel(acct), resp.StatusCode, resp.Header.Get("Content-Type"), len(rawResp), headOf(rawResp, 160))
		rl.done(0, Usage{}, err)
		httpx.WriteErr(w, http.StatusBadGateway, "上游响应不是合法 JSON", "upstream_error")
		return
	}
	transformChatResponse(out, model.ID)
	usage := parseUsage(out["usage"])
	outLen := chatTextLen(out)

	encoded, err := json.Marshal(out)
	if err != nil {
		httpx.WriteErr(w, http.StatusInternalServerError, "响应序列化失败", "server_error")
		return
	}
	rl.done(outLen, usage, nil)
	httpx.WriteRawJSON(w, http.StatusOK, encoded)
}

// transformChatResponse normalizes non-streaming responses.
//
// The upstream puts the reasoning chain in `reasoning`; here we add a copy in `reasoning_content`, which is common in the OpenAI ecosystem
// (existing values are not overwritten), and change model back to the external model name.
func transformChatResponse(m map[string]any, publicModel string) {
	if publicModel != "" {
		m["model"] = publicModel
	}
	choices, _ := m["choices"].([]any)
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if msg, ok := cm["message"].(map[string]any); ok {
			addReasoningContent(msg)
		}
	}
}

// transformChatChunk normalizes a single streaming chunk.
func transformChatChunk(m map[string]any, publicModel string) {
	if publicModel != "" {
		m["model"] = publicModel
	}
	choices, _ := m["choices"].([]any)
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if d, ok := cm["delta"].(map[string]any); ok {
			addReasoningContent(d)
		}
	}
}

// addReasoningContent adds the field when reasoning is non-empty and reasoning_content is missing.
func addReasoningContent(obj map[string]any) {
	if _, exists := obj["reasoning_content"]; exists {
		return
	}
	if v, ok := obj["reasoning"].(string); ok && v != "" {
		obj["reasoning_content"] = v
	}
}

// streamChat forwards streaming in OpenAI SSE format.
func (s *Server) streamChat(w http.ResponseWriter, body io.Reader, rl reqLog) {
	sw := newSSEWriter(w)

	var (
		usage    Usage
		outLen   int
		doneSent bool
	)

	streamErr := forEachSSEFrame(body, func(f sseFrame) bool {
		if f.Comment != "" {
			// Upstream heartbeat, passed through as-is to keep the client connection alive.
			_, _ = w.Write([]byte(f.Comment + "\n\n"))
			sw.flush()
			return true
		}
		if f.Data == "" {
			return true // event: frames are meaningless in the chat scenario
		}
		if f.Data == sseDone {
			sw.done()
			doneSent = true
			return false
		}
		if f.Object == nil {
			// When parsing fails, pass through the original text to avoid breaking upstream extension events.
			_, _ = w.Write([]byte("data: " + f.Data + "\n\n"))
			sw.flush()
			return true
		}
		transformChatChunk(f.Object, rl.model)
		if u := parseUsage(f.Object["usage"]); u.TotalTokens > 0 || u.Cost > 0 {
			usage = u
		}
		outLen += chatTextLen(f.Object)
		if raw := jsonEvent(f.Object); raw != nil {
			sw.data(raw)
		}
		return true
	})

	if streamErr != nil {
		errPayload := jsonEvent(map[string]any{
			"error": map[string]any{"message": streamErr.Error(), "type": "upstream_error"},
		})
		if errPayload != nil {
			sw.data(errPayload)
		}
	}
	if !doneSent {
		// Do not append another [DONE] when the upstream already sent one: duplicate terminal frames cause some clients to raise alarms.
		sw.done()
	}
	rl.done(outLen, usage, streamErr)
}

// acctLabel returns the account identifier for logging (excluding the key).
func acctLabel(a *deepai.Account) string {
	if a == nil {
		return "-"
	}
	return a.Name
}

// headOf truncates the beginning of the response for troubleshooting.
func headOf(raw []byte, n int) string {
	if len(raw) <= n {
		return string(raw)
	}
	return string(raw[:n]) + "…"
}
