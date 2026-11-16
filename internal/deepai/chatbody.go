package deepai

import "encoding/json"

// DefaultSystemPrompt is the fallback prompt used when filling in a system message.
//
// Upstream 0.3.19+ body validation only accepts "the shape DeepAI Code sends";
// the desktop client attaches a system prompt and tools to every request, and
// pure user-only requests are occasionally rejected with a 400
// invalid_request_error. This only adds a system message and never rewrites user
// content; if a system / developer message already exists it is kept as-is
// and never replaced.
const DefaultSystemPrompt = "You are a helpful AI assistant. Answer the user's request directly."

// desktopChatKeys is "the fields the desktop client sends ∪ the knobs that
// change sampling/usage".
//
// It is only used as a fallback retry after upstream rejects with
// "This request is not one DeepAI Code sends."; the normal path still passes
// through the client's original fields.
//
// Measured on the desktop client (0.3.20, re-verified with 0.3.37 on 2026-11-15;
// the pi runtime is also 1.3.0 and the request body shape is unchanged), every
// chat request sends:
//
//	model / messages / stream / stream_options / store=false /
//	max_completion_tokens / temperature / tools / tool_choice /
//	reasoning_effort ("none" when thinking is disabled on reasoning models)
//
// store therefore has to stay in the allowlist (it was missing early on,
// so the reordered body ended up one field short of the desktop client).
// As for seed / service_tier / top_p / stop / prompt_cache_key: the desktop
// client does not send them, but they are sampling/usage params the gateway
// caller set explicitly, and dropping them would silently change request
// semantics, so they are kept.
var desktopChatKeys = map[string]bool{
	"model":                 true,
	"messages":              true,
	"tools":                 true,
	"tool_choice":           true,
	"parallel_tool_calls":   true,
	"max_tokens":            true,
	"max_completion_tokens": true,
	"temperature":           true,
	"top_p":                 true,
	"stop":                  true,
	"stream":                true,
	"stream_options":        true,
	"store":                 true,
	"seed":                  true,
	"service_tier":          true,
	"reasoning_effort":      true,
	"prompt_cache_key":      true,
}

// ChatBodyFallback narrows a chat request body to the desktop client shape,
// for use as a retry after a 400 rejection.
//
// Rules:
//   - keep only allowlisted fields;
//   - ensure the first message is system (insert the default prompt if missing).
//
// ok=false means the body cannot be parsed / has no messages, so a retry is
// not worthwhile.
func ChatBodyFallback(body []byte, systemPrompt string) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return nil, false
	}
	if systemPrompt == "" {
		systemPrompt = DefaultSystemPrompt
	}

	strict := make(map[string]any, len(desktopChatKeys))
	for k, v := range payload {
		if desktopChatKeys[k] {
			strict[k] = v
		}
	}

	if !hasSystemMessage(msgs) {
		withSystem := make([]any, 0, len(msgs)+1)
		withSystem = append(withSystem, map[string]any{"role": "system", "content": systemPrompt})
		withSystem = append(withSystem, msgs...)
		strict["messages"] = withSystem
	}

	out, err := json.Marshal(strict)
	if err != nil {
		return nil, false
	}
	return out, true
}

func hasSystemMessage(msgs []any) bool {
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "system" || role == "developer" {
			return true
		}
	}
	return false
}

// EnsureSystemMessage adds a default prompt only when no system / developer
// message is present.
//
// Unlike ChatBodyFallback it does **not** touch any other field (no allowlist
// trimming), so it is safe to apply on the normal path without silently
// changing sampling params the caller set explicitly.
// inserted=true means the request body was rewritten.
func EnsureSystemMessage(body []byte, systemPrompt string) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false
	}
	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return nil, false
	}
	if hasSystemMessage(msgs) {
		return body, false
	}
	if systemPrompt == "" {
		systemPrompt = DefaultSystemPrompt
	}
	withSystem := make([]any, 0, len(msgs)+1)
	withSystem = append(withSystem, map[string]any{"role": "system", "content": systemPrompt})
	withSystem = append(withSystem, msgs...)
	payload["messages"] = withSystem
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return out, true
}
