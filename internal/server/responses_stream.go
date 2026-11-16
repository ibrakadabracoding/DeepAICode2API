package server

import (
	"sort"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// --- Responses stream event generator ---

// This file stays in the server package (not gateway): the generator writes directly to *sseWriter,
// and sseWriter belongs to the HTTP transport layer. The field mapping part has been extracted to internal/gateway.

type responsesStreamGen struct {
	sw            *sseWriter
	model         string
	respID        string
	createdAt     int64
	outputIndex   int
	textItemID    string
	textBuf       strings.Builder
	textOpen      bool
	reasoningOpen bool
	reasoningID   string
	reasoningBuf  strings.Builder
	toolItems     map[int]*responsesToolState
	finishReason  string
	// output records final items by output_index: the close order (text → reasoning → tool) and
	// output_index are not necessarily in the same order (upstream may send tool before text), so before emitting, must sort by
	// index, otherwise the order of response.output contradicts the output_index reported in the stream.
	output []responsesOutputItem
}

// responsesOutputItem is one item of the final response.output (with a sort key).
type responsesOutputItem struct {
	index int
	item  map[string]any
}

type responsesToolState struct {
	itemID   string
	callID   string
	name     string
	args     strings.Builder
	index    int
	finished bool
}

func newResponsesStreamGen(sw *sseWriter, model string) *responsesStreamGen {
	return &responsesStreamGen{
		sw:        sw,
		model:     model,
		respID:    "resp_" + deepai.RandomHex(12),
		createdAt: time.Now().Unix(),
		toolItems: map[int]*responsesToolState{},
		output:    []responsesOutputItem{},
	}
}

func (g *responsesStreamGen) emit(event string, v map[string]any) {
	v["type"] = event
	if raw := jsonEvent(v); raw != nil {
		g.sw.event(event, raw)
	}
}

func (g *responsesStreamGen) response(status string, usage Usage) map[string]any {
	resp := map[string]any{
		"id": g.respID, "object": "response", "created_at": g.createdAt,
		"status": status, "model": g.model,
		"output":              g.sortedOutput(),
		"output_text":         g.textBuf.String(),
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
	}
	if usage.TotalTokens > 0 {
		resp["usage"] = map[string]any{
			"input_tokens":          usage.PromptTokens,
			"output_tokens":         usage.CompletionTokens,
			"total_tokens":          usage.TotalTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens},
		}
	}
	return resp
}

// sortedOutput returns the final output items by output_index (without modifying the internal slice).
func (g *responsesStreamGen) sortedOutput() []any {
	items := make([]responsesOutputItem, len(g.output))
	copy(items, g.output)
	sort.SliceStable(items, func(i, j int) bool { return items[i].index < items[j].index })
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = it.item
	}
	return out
}

func (g *responsesStreamGen) created() {
	g.emit("response.created", map[string]any{
		"response": g.response("in_progress", Usage{}),
	})
}

func (g *responsesStreamGen) reasoning(delta string) {
	if !g.reasoningOpen {
		g.closeText()
		g.reasoningOpen = true
		g.reasoningID = "rs_" + deepai.RandomHex(12)
		g.emit("response.output_item.added", map[string]any{
			"output_index": g.outputIndex,
			"item": map[string]any{
				"id": g.reasoningID, "type": "reasoning", "status": "in_progress",
				"summary": []any{},
			},
		})
		g.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": g.reasoningID, "output_index": g.outputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	g.reasoningBuf.WriteString(delta)
	g.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": g.reasoningID, "output_index": g.outputIndex, "summary_index": 0,
		"delta": delta,
	})
}

func (g *responsesStreamGen) textDelta(delta string) {
	if !g.textOpen {
		g.closeReasoning()
		g.textOpen = true
		g.textItemID = "msg_" + deepai.RandomHex(12)
		g.emit("response.output_item.added", map[string]any{
			"output_index": g.outputIndex,
			"item": map[string]any{
				"id": g.textItemID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		})
		g.emit("response.content_part.added", map[string]any{
			"item_id": g.textItemID, "output_index": g.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	g.textBuf.WriteString(delta)
	g.emit("response.output_text.delta", map[string]any{
		"item_id": g.textItemID, "output_index": g.outputIndex, "content_index": 0,
		"delta": delta,
	})
}

func (g *responsesStreamGen) toolCall(idx int, id, name, args string) {
	g.closeText()
	state, ok := g.toolItems[idx]
	if !ok {
		state = &responsesToolState{
			itemID: "fc_" + deepai.RandomHex(12),
			callID: firstString(id, "call_"+deepai.RandomHex(8)),
			name:   name,
			index:  g.outputIndex,
		}
		g.toolItems[idx] = state
		g.outputIndex++
		g.emit("response.output_item.added", map[string]any{
			"output_index": state.index,
			"item": map[string]any{
				"id": state.itemID, "type": "function_call", "status": "in_progress",
				"call_id": state.callID, "name": state.name, "arguments": "",
			},
		})
	}
	if name != "" && state.name == "" {
		state.name = name
	}
	if args != "" {
		state.args.WriteString(args)
		g.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": state.itemID, "output_index": state.index, "delta": args,
		})
	}
}

func (g *responsesStreamGen) closeReasoning() {
	if !g.reasoningOpen {
		return
	}
	g.reasoningOpen = false
	text := g.reasoningBuf.String()
	item := map[string]any{
		"id": g.reasoningID, "type": "reasoning", "status": "completed",
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
	g.output = append(g.output, responsesOutputItem{index: g.outputIndex, item: item})
	g.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id": g.reasoningID, "output_index": g.outputIndex, "summary_index": 0, "text": text,
	})
	g.emit("response.output_item.done", map[string]any{
		"output_index": g.outputIndex, "item": item,
	})
	g.outputIndex++
}

func (g *responsesStreamGen) closeText() {
	if !g.textOpen {
		g.closeReasoning()
		return
	}
	g.textOpen = false
	text := g.textBuf.String()
	item := map[string]any{
		"id": g.textItemID, "type": "message", "status": "completed",
		"role":    "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	g.output = append(g.output, responsesOutputItem{index: g.outputIndex, item: item})
	g.emit("response.output_text.done", map[string]any{
		"item_id": g.textItemID, "output_index": g.outputIndex, "content_index": 0, "text": text,
	})
	g.emit("response.content_part.done", map[string]any{
		"item_id": g.textItemID, "output_index": g.outputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	g.emit("response.output_item.done", map[string]any{
		"output_index": g.outputIndex, "item": item,
	})
	g.outputIndex++
}

func (g *responsesStreamGen) closeTools() {
	// map iteration order is random; the final response.output must be ordered by output_index,
	// otherwise the output order of parallel tool calls will vary randomly.
	idxs := make([]int, 0, len(g.toolItems))
	for idx := range g.toolItems {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	for _, idx := range idxs {
		state := g.toolItems[idx]
		if state.finished {
			continue
		}
		state.finished = true
		args := state.args.String()
		item := map[string]any{
			"id": state.itemID, "type": "function_call", "status": "completed",
			"call_id": state.callID, "name": state.name, "arguments": args,
		}
		g.output = append(g.output, responsesOutputItem{index: state.index, item: item})
		g.emit("response.function_call_arguments.done", map[string]any{
			"item_id": state.itemID, "output_index": state.index, "arguments": args,
		})
		g.emit("response.output_item.done", map[string]any{
			"output_index": state.index, "item": item,
		})
	}
}

func (g *responsesStreamGen) fail(err error) {
	g.emit("error", map[string]any{"message": err.Error()})
}

func (g *responsesStreamGen) complete(usage Usage) {
	g.closeText()
	g.closeReasoning()
	g.closeTools()
	g.emit("response.completed", map[string]any{
		"response": g.response("completed", usage),
	})
}
