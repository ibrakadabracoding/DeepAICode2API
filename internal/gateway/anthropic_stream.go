package gateway

import (
	"sort"
	"strings"
)

// --- Anthropic streaming content block manager ---

// anthropicToolBlock is a buffered tool call.
//
// OpenAI streaming tool_calls are distinguished by index, and argument fragments
// of multiple tool calls **may arrive interleaved** (especially for parallel tool
// calls). Anthropic content_block semantics are sequential: once a block is
// closed, nothing more can be appended to it. So arguments are accumulated by
// index here and emitted in index order in one go at Finish / before the next
// text block, avoiding writing tool#1's argument fragments into tool#0's block.
type anthropicToolBlock struct {
	toolIdx int
	id      string
	name    string
	args    strings.Builder
}

type AnthropicBlockWriter struct {
	send  func(event string, v any)
	kind  string // "" / thinking / text / tool
	index int
	open  bool
	tools map[int]*anthropicToolBlock
}

func NewAnthropicBlockWriter(send func(string, any)) *AnthropicBlockWriter {
	return &AnthropicBlockWriter{send: send, index: -1, tools: map[int]*anthropicToolBlock{}}
}

func (b *AnthropicBlockWriter) OpenBlock(kind string, content map[string]any) {
	b.Close()
	b.index++
	b.kind = kind
	b.open = true
	b.send("content_block_start", map[string]any{
		"type": "content_block_start", "index": b.index, "content_block": content,
	})
}

func (b *AnthropicBlockWriter) Close() {
	if !b.open {
		return
	}
	b.send("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": b.index,
	})
	b.open = false
	b.kind = ""
}

func (b *AnthropicBlockWriter) Thinking(text string) {
	if b.kind != "thinking" {
		b.OpenBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
	}
	b.send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

func (b *AnthropicBlockWriter) Text(text string) {
	if b.kind != "text" {
		b.OpenBlock("text", map[string]any{"type": "text", "text": ""})
	}
	b.send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

// ToolCall buffers a tool call fragment; the actual content blocks are emitted in index order at Finish.
func (b *AnthropicBlockWriter) ToolCall(toolIdx int, id, name, args string) {
	if b.tools == nil {
		b.tools = map[int]*anthropicToolBlock{}
	}
	t := b.tools[toolIdx]
	if t == nil {
		t = &anthropicToolBlock{toolIdx: toolIdx}
		b.tools[toolIdx] = t
	}
	if id != "" {
		t.id = id
	}
	if name != "" {
		t.name = name
	}
	t.args.WriteString(args)
}

// Finish ends the response: close the current block, emit the buffered tool calls in index order, then close the last one.
func (b *AnthropicBlockWriter) Finish() {
	b.Close()
	b.flushTools()
	b.Close()
}

// flushTools emits tool blocks in ascending tool index order.
//
// Each tool's arguments are emitted as a **single** input_json_delta: the upstream
// fragments are already accumulated, and splitting them into multiple deltas only
// adds client parsing overhead while failing to match OpenAI's fragment boundaries.
func (b *AnthropicBlockWriter) flushTools() {
	if len(b.tools) == 0 {
		return
	}
	idxs := make([]int, 0, len(b.tools))
	for idx := range b.tools {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		t := b.tools[idx]
		b.OpenBlock("tool", map[string]any{
			"type": "tool_use", "id": t.id, "name": t.name, "input": map[string]any{},
		})
		if t.args.Len() > 0 {
			b.send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": b.index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": t.args.String()},
			})
		}
	}
	b.tools = map[int]*anthropicToolBlock{}
}
