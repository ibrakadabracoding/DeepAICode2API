package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/httpx"
)

// handleResponses is the entry point for /v1/responses.
//
// Two paths:
// - Models with api=openai-responses in the catalog: pass through to the upstream /responses as-is;
// - All other models: translate the Responses request into Chat Completions, then
// synthesize the result into a Responses object / event stream.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
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

	model, modelOK := s.resolveModelOK(requestModelName(payload))
	if !modelOK {
		httpx.WriteErr(w, http.StatusNotFound, s.unknownModelMessage(requestModelName(payload)), "invalid_request_error")
		return
	}
	payload["model"] = model.ID
	stream, _ := payload["stream"].(bool)
	affinity := affinityFor(r, payload)
	key := extractKey(r)
	promptLen := estimatePromptLen(payload)

	rls := s.newReqLog(key, nil, model.ID, "responses", stream, start, promptLen)
	if model.IsResponses() {
		s.passthroughResponses(w, r, payload, affinity, rls)
		return
	}

	chatPayload, err := responsesToChatPayload(payload, model.ID)
	if err != nil {
		httpx.WriteErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if stream {
		s.streamResponsesAsChat(w, r, chatPayload, affinity, rls)
		return
	}
	s.blockingResponsesAsChat(w, r, chatPayload, affinity, rls)
}

// passthroughResponses passes through to the upstream /responses.
func (s *Server) passthroughResponses(w http.ResponseWriter, r *http.Request, payload map[string]any, affinity string, rl reqLog) {
	ctx, cancel := httpx.ContextWithTimeout(r, s.requestTimeout())
	defer cancel()

	// Same as above: when the upstream omits stream, it returns SSE, so it must be sent explicitly.
	payload["stream"] = rl.stream
	body, err := json.Marshal(payload)
	if err != nil {
		rl.doneStatus(http.StatusBadRequest, 0, Usage{}, err)
		httpx.WriteErr(w, http.StatusBadRequest, "请求序列化失败", "invalid_request_error")
		return
	}
	resp, acct, err := s.openModel(ctx, deepai.PathResponses, body, affinity, s.requestGroup(r), rl.apiKey(), rl.stream)
	rl.acct = acct
	if err != nil {
		rl.done(0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	if !rl.stream {
		rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			rl.done(0, Usage{}, err)
			httpx.WriteErr(w, http.StatusBadGateway, "读取上游响应失败: "+err.Error(), "upstream_error")
			return
		}
		var out map[string]any
		if err := json.Unmarshal(rawResp, &out); err != nil {
			// The upstream returned something that is not JSON (HTML login page / SSE frames / plain-text rate-limit notice).
			// Pass through as-is + 200 (the client receives exactly what the upstream returned), but **logging is mandatory**:
			// The old implementation returned directly here, so the request vanished entirely from console logs and statistics.
			httpx.WriteRawJSON(w, http.StatusOK, rawResp)
			rl.doneStatus(http.StatusOK, 0, Usage{},
				fmt.Errorf("上游响应不是合法 JSON（已原样透传）: %w", err))
			return
		}
		rewriteResponsesModel(out, rl.model)
		usage := parseUsage(out["usage"])
		outLen := responsesTextLen(out)
		encoded, _ := json.Marshal(out)
		rl.done(outLen, usage, nil)
		httpx.WriteRawJSON(w, http.StatusOK, encoded)
		return
	}

	sw := newSSEWriter(w)
	var (
		usage  Usage
		outLen int
	)
	streamErr := forEachSSEFrame(resp.Body, func(f sseFrame) bool {
		if f.Event != "" {
			_, _ = w.Write([]byte("event: " + f.Event + "\n"))
			return true
		}
		if f.Data == "" {
			return true
		}
		if f.Data == sseDone {
			sw.done()
			return false
		}
		obj := f.Object
		if obj == nil {
			_, _ = w.Write([]byte("data: " + f.Data + "\n\n"))
			sw.flush()
			return true
		}
		rewriteResponsesModel(obj, rl.model)
		if respObj, ok := obj["response"].(map[string]any); ok {
			if u := parseUsage(respObj["usage"]); u.TotalTokens > 0 || u.Cost > 0 {
				usage = u
			}
		}
		outLen += responsesEventTextLen(obj)
		// The rewritten object must be written out: writing the raw data directly would lose the model rewrite.
		if raw := jsonEvent(obj); raw != nil {
			sw.data(raw)
			return true
		}
		_, _ = w.Write([]byte("data: " + f.Data + "\n\n"))
		sw.flush()
		return true
	})
	if streamErr != nil {
		payload := jsonEvent(map[string]any{
			"type": "error", "message": streamErr.Error(),
		})
		if payload != nil {
			sw.event("error", payload)
		}
	}
	rl.done(outLen, usage, streamErr)
}

// blockingResponsesAsChat synthesizes a Responses response using Chat Completions.
func (s *Server) blockingResponsesAsChat(w http.ResponseWriter, r *http.Request, chatPayload map[string]any, affinity string, rl reqLog) {
	ctx, cancel := httpx.ContextWithTimeout(r, s.requestTimeout())
	defer cancel()

	out, acct, err := s.callJSON(ctx, deepai.PathChat, chatPayload, affinity, s.requestGroup(r), rl.apiKey())
	rl.acct = acct
	if err != nil {
		rl.done(0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	respObj := chatToResponsesObject(out, rl.model)
	usage := parseUsage(out["usage"])
	outLen := responsesTextLen(respObj)

	rl.done(outLen, usage, nil)
	httpx.WriteJSON(w, http.StatusOK, respObj)
}

// streamResponsesAsChat synthesizes a Responses event stream using a Chat Completions stream.
func (s *Server) streamResponsesAsChat(w http.ResponseWriter, r *http.Request, chatPayload map[string]any, affinity string, rl reqLog) {
	ctx, cancel := httpx.ContextWithTimeout(r, s.requestTimeout())
	defer cancel()

	body, err := json.Marshal(chatPayload)
	if err != nil {
		rl.doneStatus(http.StatusBadRequest, 0, Usage{}, err)
		httpx.WriteErr(w, http.StatusBadRequest, "请求序列化失败", "invalid_request_error")
		return
	}
	resp, acct, err := s.openModel(ctx, deepai.PathChat, body, affinity, s.requestGroup(r), rl.apiKey(), true)
	rl.acct = acct
	if err != nil {
		rl.done(0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	sw := newSSEWriter(w)
	gen := newResponsesStreamGen(sw, rl.model)
	gen.created()

	var (
		usage  Usage
		outLen int
	)
	streamErr := forEachChatChunk(resp.Body, func(chunk map[string]any) bool {
		if u := parseUsage(chunk["usage"]); u.TotalTokens > 0 || u.Cost > 0 {
			usage = u
		}
		d, finish := chatChunkDelta(chunk)
		if d.Reasoning != "" {
			gen.reasoning(d.Reasoning)
			outLen += len([]rune(d.Reasoning))
		}
		if d.Content != "" {
			gen.textDelta(d.Content)
			outLen += len([]rune(d.Content))
		}
		for _, tc := range d.ToolCalls {
			gen.toolCall(tc.Index, tc.ID, tc.Name, tc.Args)
		}
		if finish != "" {
			gen.finishReason = finish
		}
		return true
	})

	if streamErr != nil {
		gen.fail(streamErr)
	}
	gen.complete(usage)
	rl.done(outLen, usage, streamErr)
}

// --- Helper functions ---

// rewriteResponsesModel changes the model in a Responses object / event to the external model name.
func rewriteResponsesModel(obj map[string]any, publicModel string) {
	if publicModel == "" {
		return
	}
	if _, ok := obj["model"]; ok {
		obj["model"] = publicModel
	}
	if resp, ok := obj["response"].(map[string]any); ok {
		rewriteResponsesModel(resp, publicModel)
	}
}

// responsesTextLen counts the length of the output text in a Responses object.
func responsesTextLen(obj map[string]any) int {
	total := 0
	if s, ok := obj["output_text"].(string); ok {
		total += len([]rune(s))
	}
	output, _ := obj["output"].([]any)
	for _, itemv := range output {
		item, ok := itemv.(map[string]any)
		if !ok {
			continue
		}
		switch strOf(item["type"]) {
		case "message":
			content, _ := item["content"].([]any)
			for _, cv := range content {
				c, ok := cv.(map[string]any)
				if ok {
					total += lenOfString(c["text"])
				}
			}
		case "reasoning":
			summary, _ := item["summary"].([]any)
			for _, sv := range summary {
				sm, ok := sv.(map[string]any)
				if ok {
					total += lenOfString(sm["text"])
				}
			}
		}
	}
	return total
}

// responsesEventTextLen counts the length of the text delta in a single stream event.
func responsesEventTextLen(obj map[string]any) int {
	if strOf(obj["type"]) == "response.output_text.delta" {
		return len([]rune(strOf(obj["delta"])))
	}
	if strOf(obj["type"]) == "response.reasoning_summary_text.delta" {
		return len([]rune(strOf(obj["delta"])))
	}
	return 0
}
