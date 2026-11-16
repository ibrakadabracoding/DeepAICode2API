package gateway

import (
	"encoding/json"
	"fmt"
	"strings"

	"deepaicode2api/internal/deepai"
)

// --- Anthropic → OpenAI ---

// AnthropicToChatPayload converts an Anthropic request into OpenAI Chat Completions.
func AnthropicToChatPayload(req *AnthropicRequest, modelID string) (map[string]any, error) {
	payload := map[string]any{
		"model":  modelID,
		"stream": req.Stream,
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}
	if effort := AnthropicEffort(req); effort != "" {
		payload["reasoning_effort"] = effort
	}

	messages := make([]any, 0, len(req.Messages)+1)
	if sys := AnthropicSystemText(req.System); sys != "" {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}

	for _, m := range req.Messages {
		if err := AppendAnthropicMessage(&messages, m); err != nil {
			return nil, err
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("消息内容为空")
	}
	payload["messages"] = messages

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				var schema any
				if json.Unmarshal(t.InputSchema, &schema) == nil {
					fn["parameters"] = schema
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		payload["tools"] = tools
		if tc := AnthropicToolChoice(req.ToolChoice); tc != nil {
			payload["tool_choice"] = tc
		}
	}
	return payload, nil
}

// AnthropicEffort maps Anthropic's thinking / output_config.effort to the
// reasoning_effort of the upstream chat endpoint.
//
// The upstream only understands the OpenAI shape; previously, thinking was parsed
// and then dropped, which silently downgraded the reasoning effort explicitly
// requested by Anthropic clients (including model fingerprint checks) to the
// upstream default - so the measured "model fingerprint" naturally disagreed with
// the client.
//
// Priority: output_config.effort > thinking.type > thinking.budget_tokens.
// output_config.effort is passed through as-is (including `xhigh` / `max`);
// budget_tokens has no standard conversion, so it is roughly bucketed by
// magnitude: below 4k low, below 16k medium, below 32k high, below 64k xhigh,
// anything higher max. Measured on 2026-11-15: the upstream accepts all six
// levels (none/low/medium/high/xhigh/max) for these three free models, so mapping
// to a new level does not cause a 400.
func AnthropicEffort(req *AnthropicRequest) string {
	if req == nil {
		return ""
	}
	if req.OutputConfig != nil {
		if e := strings.ToLower(strings.TrimSpace(req.OutputConfig.Effort)); e != "" {
			return e
		}
	}
	if len(req.Thinking) == 0 {
		return ""
	}
	var th struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	if json.Unmarshal(req.Thinking, &th) != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(th.Type)) {
	case "disabled":
		return "none"
	case "enabled":
		switch {
		case th.BudgetTokens <= 0:
			return "" // Only "enabled" was given, no budget: leave it to the upstream default
		case th.BudgetTokens < 4096:
			return "low"
		case th.BudgetTokens < 16384:
			return "medium"
		case th.BudgetTokens < 32768:
			return "high"
		case th.BudgetTokens < 65536:
			return "xhigh"
		default:
			return "max"
		}
	}
	return ""
}

// AppendAnthropicMessage expands one Anthropic message into one or more OpenAI messages.
func AppendAnthropicMessage(messages *[]any, m AnthropicMessage) error {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	if role == "" {
		role = "user"
	}

	// Pass through directly when content is a string.
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		if role == "assistant" {
			*messages = append(*messages, map[string]any{"role": "assistant", "content": text})
		} else {
			*messages = append(*messages, map[string]any{"role": role, "content": text})
		}
		return nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return fmt.Errorf("content 字段格式不支持")
	}

	if role == "assistant" {
		var textParts []string
		var toolCalls []any
		for _, b := range blocks {
			switch b["type"] {
			case "text":
				if t, ok := b["text"].(string); ok && t != "" {
					textParts = append(textParts, t)
				}
			case "tool_use":
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				args, _ := json.Marshal(b["input"])
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": string(args),
					},
				})
			}
		}
		msg := map[string]any{"role": "assistant"}
		if len(textParts) > 0 {
			msg["content"] = strings.Join(textParts, "\n")
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		if msg["content"] != nil || len(toolCalls) > 0 {
			*messages = append(*messages, msg)
		}
		return nil
	}

	// user: tool_result becomes a separate role=tool message; the remaining text / images
	// are merged into one message.
	//
	// **tool_result must be sent first**: OpenAI requires role=tool to immediately follow
	// an assistant with tool_calls, while Anthropic often puts tool_result and follow-up
	// explanations in the same user message. Sending text first in the original order
	// would insert it between the assistant and tool messages, and the upstream would
	// reject it outright or treat the tool result as a new user turn.
	for _, b := range blocks {
		if b["type"] != "tool_result" {
			continue
		}
		id, _ := b["tool_use_id"].(string)
		content := AnthropicToolResultText(b)
		if content == "" {
			content = "(no output)"
		}
		*messages = append(*messages, map[string]any{
			"role": "tool", "tool_call_id": id, "content": content,
		})
	}

	var parts []any
	for _, b := range blocks {
		switch b["type"] {
		case "text":
			if t, ok := b["text"].(string); ok && t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			if u := AnthropicImageURL(b); u != "" {
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": u},
				})
			}
		case "document":
			if u := AnthropicDocumentDataURL(b); u != "" {
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": u},
				})
			} else {
				name := FirstString(b["title"], b["name"])
				if name == "" {
					name = "document"
				}
				parts = append(parts, map[string]any{"type": "text", "text": "[document] " + name})
			}
		}
	}
	if len(parts) > 0 {
		*messages = append(*messages, map[string]any{"role": role, "content": parts})
	}
	return nil
}

// AnthropicSystemText extracts the system field (a string or an array of text blocks).
func AnthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b["type"] == "text" {
			if t, ok := b["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func AnthropicImageURL(block map[string]any) string {
	src, ok := block["source"].(map[string]any)
	if !ok {
		return ""
	}
	switch src["type"] {
	case "base64":
		media, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if data == "" {
			return ""
		}
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + data
	case "url":
		u, _ := src["url"].(string)
		return u
	}
	return ""
}

func AnthropicDocumentDataURL(block map[string]any) string {
	src, ok := block["source"].(map[string]any)
	if !ok || src["type"] != "base64" {
		return ""
	}
	media, _ := src["media_type"].(string)
	if !strings.HasPrefix(media, "image/") {
		return ""
	}
	data, _ := src["data"].(string)
	if data == "" {
		return ""
	}
	return "data:" + media + ";base64," + data
}

func AnthropicToolResultText(block map[string]any) string {
	switch c := block["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			case "image":
				parts = append(parts, "[image omitted]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func AnthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var body struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	switch body.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if body.Name == "" {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": body.Name}}
	}
	return nil
}

// --- OpenAI → Anthropic ---

// ChatToAnthropicMessage converts an OpenAI chat response into an Anthropic message.
func ChatToAnthropicMessage(out map[string]any, publicModel string) map[string]any {
	content := make([]any, 0, 3)
	stopReason := "end_turn"

	choices, _ := out["choices"].([]any)
	if len(choices) > 0 {
		if cm, ok := choices[0].(map[string]any); ok {
			if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
				stopReason = AnthropicStopReason(fr)
			}
			if msg, ok := cm["message"].(map[string]any); ok {
				if reasoning, ok := msg["reasoning"].(string); ok && reasoning != "" {
					content = append(content, map[string]any{
						"type": "thinking", "thinking": reasoning, "signature": "",
					})
				}
				if text, ok := msg["content"].(string); ok && text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tcv := range tcs {
						tc, ok := tcv.(map[string]any)
						if !ok {
							continue
						}
						id, _ := tc["id"].(string)
						name := ""
						input := map[string]any{}
						if fn, ok := tc["function"].(map[string]any); ok {
							name, _ = fn["name"].(string)
							if args, ok := fn["arguments"].(string); ok && args != "" {
								var parsed map[string]any
								if json.Unmarshal([]byte(args), &parsed) == nil {
									input = parsed
								} else {
									input = map[string]any{"_raw": args}
								}
							}
						}
						content = append(content, map[string]any{
							"type": "tool_use", "id": id, "name": name, "input": input,
						})
					}
				}
			}
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	usage := ParseUsage(out["usage"])
	return map[string]any{
		"id":            "msg_" + deepai.RandomHex(12),
		"type":          "message",
		"role":          "assistant",
		"model":         publicModel,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": usage.CompletionTokens,
		},
	}
}

// AnthropicStopReason maps OpenAI finish_reason → Anthropic stop_reason.
func AnthropicStopReason(fr string) string {
	switch fr {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func AnthropicTextLen(msg map[string]any) int {
	total := 0
	content, _ := msg["content"].([]any)
	for _, c := range content {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch cm["type"] {
		case "text":
			total += LenOfString(cm["text"])
		case "thinking":
			total += LenOfString(cm["thinking"])
		}
	}
	return total
}
