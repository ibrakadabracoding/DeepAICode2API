package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// --- Upstream SSE frame reading ---

// sseFrame holds the business-relevant content of one upstream SSE frame.
//
// The gateway has two consumption scenarios at the same time:
// - Passthrough (chat / responses passthrough): needs event name, comment frames, raw data,
// so that upstream extension events and heartbeats can be forwarded as-is;
// - Protocol conversion (anthropic / responses-as-chat): only cares about the parsed JSON.
//
// Use one struct to cover both, avoiding each handler writing its own frame-reading loop.
type sseFrame struct {
	// A non-empty Comment indicates a comment / heartbeat frame (starting with ":"), used to keep the client connection alive during passthrough.
	Comment string
	// Event is the value of the "event:" field (required by Anthropic / Responses event streams).
	Event string
	// Data is the raw content after "data:" (leading and trailing whitespace trimmed).
	Data string
	// Object is the JSON parse result of Data; nil if parsing fails or it is not an object.
	Object map[string]any
}

// sseDone is the explicit end marker of an SSE stream.
const sseDone = "[DONE]"

// forEachSSEFrame parses upstream SSE frame by frame and passes each frame to fn; stops when fn returns false.
//
// Unifies the frame-reading logic of the four streaming handlers: data prefix, [DONE] termination, JSON parse result,
// and passthrough information for event: / comment frames. Returning false from fn or reading EOF both count as normal termination.
func forEachSSEFrame(body io.Reader, fn func(f sseFrame) bool) error {
	return readSSELines(body, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			return true
		case strings.HasPrefix(trimmed, ":"):
			return fn(sseFrame{Comment: trimmed})
		}
		if rest, ok := strings.CutPrefix(trimmed, "event:"); ok {
			return fn(sseFrame{Event: strings.TrimSpace(rest)})
		}
		if !strings.HasPrefix(trimmed, "data:") {
			return true
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" {
			return true
		}
		if data == sseDone {
			fn(sseFrame{Data: sseDone})
			return false
		}
		var obj map[string]any
		_ = json.Unmarshal([]byte(data), &obj) // Object remains nil when parsing fails
		return fn(sseFrame{Data: data, Object: obj})
	})
}

// forEachChatChunk is a thin wrapper around forEachSSEFrame under chat/completions semantics:
// it only delivers parsed JSON chunks and automatically skips heartbeats / event: / [DONE] / invalid frames.
func forEachChatChunk(body io.Reader, fn func(chunk map[string]any) bool) error {
	return forEachSSEFrame(body, func(f sseFrame) bool {
		if f.Object == nil {
			return true
		}
		return fn(f.Object)
	})
}

// readSSELines reads upstream SSE line by line; stops when the callback returns false.
//
// Deliberately does not reuse deepai.ReadSSE: that implementation merges multiple data lines per the SSE spec before dispatching,
// Will lose the original form of comment frames and event: lines, while the gateway's passthrough path needs them.
func readSSELines(body io.Reader, fn func(line string) bool) error {
	br := bufio.NewReaderSize(body, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if !fn(line) {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// --- chat completion delta extraction ---

// chatDelta is the delta content related to protocol adaptation in a chat completion chunk.
//
// The Anthropic and Responses adaptation paths previously each parsed it once
// choices[0] → delta → reasoning/content/tool_calls, with identical logic word for word;
// After unifying here, the adapter is only responsible for rendering the delta into its own event format.
type chatDelta struct {
	Reasoning string
	Content   string
	ToolCalls []chatToolDelta
}

// chatToolDelta is a delta fragment of a tool call.
type chatToolDelta struct {
	Index int
	ID    string
	Name  string
	Args  string
}

// chatChunkDelta extracts the delta content and finish_reason of the first choice in the chunk.
//
// An empty finish means this frame has no finish reason; the delta field may be entirely empty (e.g., a trailing frame with only usage).
func chatChunkDelta(chunk map[string]any) (chatDelta, string) {
	var d chatDelta
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return d, ""
	}
	cm, _ := choices[0].(map[string]any)
	if cm == nil {
		return d, ""
	}

	finish, _ := cm["finish_reason"].(string)
	delta, _ := cm["delta"].(map[string]any)
	if delta == nil {
		return d, finish
	}

	d.Reasoning, _ = delta["reasoning"].(string)
	d.Content, _ = delta["content"].(string)
	tcs, ok := delta["tool_calls"].([]any)
	if !ok {
		return d, finish
	}
	for _, tcv := range tcs {
		tc, ok := tcv.(map[string]any)
		if !ok {
			continue
		}
		td := chatToolDelta{Index: intOf(tc["index"])}
		td.ID, _ = tc["id"].(string)
		if fn, ok := tc["function"].(map[string]any); ok {
			td.Name, _ = fn["name"].(string)
			td.Args, _ = fn["arguments"].(string)
		}
		d.ToolCalls = append(d.ToolCalls, td)
	}
	return d, finish
}
