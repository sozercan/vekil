// Package models defines request and response types for both Anthropic and
// OpenAI APIs. These are data-only structs with no business logic.
package models

import "encoding/json"

// AnthropicRequest represents an incoming Anthropic Messages API request.
type AnthropicRequest struct {
	Model         string                 `json:"model"`
	Messages      []AnthropicMessage     `json:"messages"`
	MaxTokens     *int                   `json:"max_tokens,omitempty"`
	System        json.RawMessage        `json:"system,omitempty"`
	Stream        bool                   `json:"stream,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	TopK          *int                   `json:"top_k,omitempty"`
	Tools         []AnthropicTool        `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice   `json:"tool_choice,omitempty"`
	Metadata      json.RawMessage        `json:"metadata,omitempty"`
	Thinking      *AnthropicThinking     `json:"thinking,omitempty"`
	ServiceTier   string                 `json:"service_tier,omitempty"`
	InferenceGeo  string                 `json:"inference_geo,omitempty"`
	OutputConfig  *AnthropicOutputConfig `json:"output_config,omitempty"`
}

// AnthropicMessage is a single message in an Anthropic conversation.
// Content is json.RawMessage because it can be a string or []ContentBlock.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ContentBlock represents a typed content block in Anthropic messages.
// The Type field determines which other fields are populated
// (text, image, tool_use, tool_result, thinking).
type ContentBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	Source    *AnthropicImageSource `json:"source,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     json.RawMessage       `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   json.RawMessage       `json:"content,omitempty"`
	Thinking  string                `json:"thinking,omitempty"`
	Signature string                `json:"signature,omitempty"`
}

// AnthropicImageSource describes an image content-block source.
type AnthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthropicTool defines a tool available for the model to call.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicThinking configures extended thinking. Type can be "enabled"
// (with BudgetTokens), "disabled", or "adaptive".
type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens *int   `json:"budget_tokens,omitempty"`
}

// AnthropicOutputConfig controls output format and effort level.
type AnthropicOutputConfig struct {
	Effort string          `json:"effort,omitempty"`
	Format json.RawMessage `json:"format,omitempty"`
}

// AnthropicResponse is the non-streaming response from the Anthropic Messages API.
type AnthropicResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        AnthropicUsage `json:"usage"`
}

// AnthropicUsage contains token usage statistics.
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// AnthropicStreamEvent is a single SSE event in an Anthropic streaming response.
type AnthropicStreamEvent struct {
	Type         string             `json:"type"`
	Message      *AnthropicResponse `json:"message,omitempty"`
	Index        *int               `json:"index,omitempty"`
	ContentBlock *ContentBlock      `json:"content_block,omitempty"`
	Delta        *AnthropicDelta    `json:"delta,omitempty"`
	Usage        *AnthropicUsage    `json:"usage,omitempty"`
}

// AnthropicDelta carries incremental updates within a streaming event.
type AnthropicDelta struct {
	Type         string `json:"type,omitempty"`
	Text         string `json:"text,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
	Thinking     string `json:"thinking,omitempty"`
	Signature    string `json:"signature,omitempty"`
}

// AnthropicToolChoice controls how the model selects tools.
// Type can be "auto", "any", or "tool" (with Name specifying which tool).
type AnthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

// AnthropicError is the standard error response format for the Anthropic API.
type AnthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
