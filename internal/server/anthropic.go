package server

import (
	"encoding/json"
	"net/http"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
)

// --- Anthropic request model ---

// handleAnthropicMessages is the entry point for POST /v1/messages.
//
// Convert the Anthropic Messages request to OpenAI Chat Completions, call the upstream, then
// map it back to the Anthropic message / SSE event model (including tool_use and thinking).
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	start := time.Now()

	req, ok := httpx.DecodeJSONRequest[anthropicRequest](w, r, func(code int, msg string) {
		writeAnthropicErr(w, code, "invalid_request_error", msg)
	})
	if !ok {
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}

	model, modelOK := s.resolveModelOK(req.Model)
	if !modelOK {
		writeAnthropicErr(w, http.StatusNotFound, "invalid_request_error", s.unknownModelMessage(req.Model))
		return
	}
	payload, err := anthropicToChatPayload(&req, model.ID)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	affinity := affinityFor(r, nil)
	key := extractKey(r)
	promptLen := estimatePromptLen(payload)

	rl := s.newReqLog(key, nil, model.ID, "messages", req.Stream, start, promptLen)
	if req.Stream {
		s.streamAnthropic(w, r, payload, affinity, rl)
		return
	}
	s.blockingAnthropic(w, r, payload, affinity, rl)
}

// blockingAnthropic collects the full reply and returns it in the Anthropic message structure.
func (s *Server) blockingAnthropic(w http.ResponseWriter, r *http.Request, payload map[string]any, affinity string, rl reqLog) {
	ctx, cancel := httpx.ContextWithTimeout(r, s.requestTimeout())
	defer cancel()

	out, acct, err := s.callJSON(ctx, deepai.PathChat, payload, affinity, s.requestGroup(r), rl.apiKey())
	rl.acct = acct
	if err != nil {
		rl.done(0, Usage{}, err)
		writeAnthropicUpstreamErr(w, err)
		return
	}
	usage := parseUsage(out["usage"])
	msg := chatToAnthropicMessage(out, rl.model)
	outLen := anthropicTextLen(msg)

	rl.done(outLen, usage, nil)
	httpx.WriteJSON(w, http.StatusOK, msg)
}

// streamAnthropic streams the response using the Anthropic SSE event model.
func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, payload map[string]any, affinity string, rl reqLog) {
	ctx, cancel := httpx.ContextWithTimeout(r, s.requestTimeout())
	defer cancel()

	body, err := json.Marshal(payload)
	if err != nil {
		rl.doneStatus(http.StatusBadRequest, 0, Usage{}, err)
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求序列化失败")
		return
	}
	resp, acct, err := s.openModel(ctx, deepai.PathChat, body, affinity, s.requestGroup(r), rl.apiKey(), true)
	rl.acct = acct
	if err != nil {
		rl.done(0, Usage{}, err)
		writeAnthropicUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	sw := newSSEWriter(w)

	msgID := "msg_" + deepai.RandomHex(12)
	send := func(event string, v any) {
		if raw := jsonEvent(v); raw != nil {
			sw.event(event, raw)
		}
	}

	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": rl.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})

	block := newAnthropicBlockWriter(send)
	var (
		usage      Usage
		outLen     int
		stopReason = "end_turn"
		streamErr  error
	)

	streamErr = forEachChatChunk(resp.Body, func(chunk map[string]any) bool {
		if u := parseUsage(chunk["usage"]); u.TotalTokens > 0 || u.Cost > 0 {
			usage = u
		}
		d, finish := chatChunkDelta(chunk)
		if finish != "" {
			stopReason = anthropicStopReason(finish)
		}
		if d.Reasoning != "" {
			block.Thinking(d.Reasoning)
			outLen += len([]rune(d.Reasoning))
		}
		if d.Content != "" {
			block.Text(d.Content)
			outLen += len([]rune(d.Content))
		}
		for _, tc := range d.ToolCalls {
			block.ToolCall(tc.Index, tc.ID, tc.Name, tc.Args)
		}
		return true
	})

	if streamErr != nil {
		// On error, only close the currently open block; do not flush the tool buffer:
		// the argument JSON may be only half received, and sending it out as a complete tool_use would make client parsing fail.
		block.Close()
		send("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": streamErr.Error()},
		})
	} else {
		block.Finish()
	}
	outputTokens := usage.CompletionTokens
	if outputTokens == 0 {
		outputTokens = estTokens(outLen)
	}
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": outputTokens,
		},
	})
	send("message_stop", map[string]any{"type": "message_stop"})

	rl.done(outLen, usage, streamErr)
}

// handleAnthropicCountTokens returns a rough input token estimate.
//
// Anthropic clients (such as Claude Code) call it before sending messages for budget control;
// the upstream does not provide this capability, so a character-count approximation is used here; it only needs to be monotonic and reasonable.
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	req, ok := httpx.DecodeJSONRequest[anthropicRequest](w, r, func(code int, msg string) {
		writeAnthropicErr(w, code, "invalid_request_error", msg)
	})
	if !ok {
		return
	}
	payload, err := anthropicToChatPayload(&req, s.resolveModel(req.Model).ID)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	rawPayload, _ := json.Marshal(payload)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"input_tokens": estTokens(len(rawPayload)),
	})
}

// --- Helper functions ---

// estTokens roughly estimates tokens by character count (1 token ≈ 2 characters).
func estTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return chars/2 + 1
}

// writeAnthropicErr returns an error in the Anthropic error structure.
func writeAnthropicErr(w http.ResponseWriter, code int, typ, msg string) {
	httpx.WriteJSON(w, code, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// requestTimeout returns the request timeout from settings.
func (s *Server) requestTimeout() time.Duration {
	sec := s.Store.Settings().RequestTimeout
	if sec <= 0 {
		sec = 600
	}
	return time.Duration(sec) * time.Second
}
