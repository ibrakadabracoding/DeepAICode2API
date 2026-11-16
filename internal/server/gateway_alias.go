package server

// The protocol conversion layer (Anthropic / Responses field mapping, streaming block generation, usage parsing) has been extracted to
// internal/gateway. That layer depends only on the standard library and the deepai protocol constants, and does not touch HTTP, store
// or the account pool, so it can be tested and modified independently of the gateway.
//
// Package-local short names (type aliases + function variables) are kept here so that existing handlers and tests need no changes at all:
// The migration is a pure move, not a behavior change.
//
// Write new conversion logic directly in internal/gateway; do not add implementations to this file.

import "deepaicode2api/internal/gateway"

type (
	// Usage is the usage returned by the upstream (token / cost).
	Usage = gateway.Usage

	// Anthropic request model.
	anthropicRequest     = gateway.AnthropicRequest
	anthropicMessage     = gateway.AnthropicMessage
	anthropicTool        = gateway.AnthropicTool
	anthropicBlockWriter = gateway.AnthropicBlockWriter
)

var (
	// Usage parsing.
	parseUsage  = gateway.ParseUsage
	intOf       = gateway.IntOf
	floatOf     = gateway.FloatOf
	lenOfString = gateway.LenOfString
	strOf       = gateway.StrOf
	boolOf      = gateway.BoolOf

	// errNoMessages indicates that there are no usable messages after conversion (mapped to 400).
	errNoMessages = gateway.ErrNoMessages

	// Anthropic ⇄ Chat mapping.
	firstString              = gateway.FirstString
	anthropicToChatPayload   = gateway.AnthropicToChatPayload
	anthropicEffort          = gateway.AnthropicEffort
	appendAnthropicMessage   = gateway.AppendAnthropicMessage
	anthropicSystemText      = gateway.AnthropicSystemText
	anthropicImageURL        = gateway.AnthropicImageURL
	anthropicDocumentDataURL = gateway.AnthropicDocumentDataURL
	anthropicToolResultText  = gateway.AnthropicToolResultText
	anthropicToolChoice      = gateway.AnthropicToolChoice
	chatToAnthropicMessage   = gateway.ChatToAnthropicMessage
	anthropicStopReason      = gateway.AnthropicStopReason
	anthropicTextLen         = gateway.AnthropicTextLen
	newAnthropicBlockWriter  = gateway.NewAnthropicBlockWriter

	// Responses ⇄ Chat mapping.
	responsesReasoningEffort   = gateway.ResponsesReasoningEffort
	responsesToChatPayload     = gateway.ResponsesToChatPayload
	responsesInputContent      = gateway.ResponsesInputContent
	responsesToolOutput        = gateway.ResponsesToolOutput
	convertResponsesToolChoice = gateway.ConvertResponsesToolChoice
	chatToResponsesObject      = gateway.ChatToResponsesObject
)
