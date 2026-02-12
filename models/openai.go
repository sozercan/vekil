package models

import "encoding/json"

// OpenAIRequest represents an OpenAI Chat Completions API request.
type OpenAIRequest struct {
	Model              string          `json:"model"`
	Messages           []OpenAIMessage `json:"messages"`
	MaxTokens          *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int           `json:"max_completion_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	N                  *int            `json:"n,omitempty"`
	Stream             *bool           `json:"stream,omitempty"`
	StreamOptions      *StreamOptions  `json:"stream_options,omitempty"`
	Stop               json.RawMessage `json:"stop,omitempty"`
	PresencePenalty    *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty   *float64        `json:"frequency_penalty,omitempty"`
	LogitBias          json.RawMessage `json:"logit_bias,omitempty"`
	User               string          `json:"user,omitempty"`
	Tools              []OpenAITool    `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat     json.RawMessage `json:"response_format,omitempty"`
	Seed               *int            `json:"seed,omitempty"`
}

// StreamOptions controls streaming behavior, such as including usage stats.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// OpenAIMessage is a single message in an OpenAI conversation.
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// OpenAITool defines a tool (function) available for the model to call.
type OpenAITool struct {
	Type     string       `json:"type"`
	Function OpenAIFunction `json:"function"`
}

// OpenAIFunction describes a function the model may call.
type OpenAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// OpenAIToolCall represents a tool call made by the model.
type OpenAIToolCall struct {
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function OpenAIFunctionCall `json:"function"`
	Index    *int             `json:"index,omitempty"`
}

// OpenAIFunctionCall contains the function name and arguments for a tool call.
type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// OpenAIResponse is the non-streaming response from the OpenAI Chat Completions API.
type OpenAIResponse struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	Model             string         `json:"model"`
	Choices           []OpenAIChoice `json:"choices"`
	Usage             *OpenAIUsage   `json:"usage,omitempty"`
	SystemFingerprint string         `json:"system_fingerprint,omitempty"`
}

// OpenAIChoice is a single completion choice in an OpenAI response.
type OpenAIChoice struct {
	Index        int          `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason *string      `json:"finish_reason,omitempty"`
}

// OpenAIUsage contains token usage statistics.
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIStreamChunk is a single SSE chunk in an OpenAI streaming response.
type OpenAIStreamChunk struct {
	ID                string               `json:"id"`
	Object            string               `json:"object"`
	Created           int64                `json:"created"`
	Model             string               `json:"model"`
	Choices           []OpenAIStreamChoice `json:"choices"`
	Usage             *OpenAIUsage         `json:"usage,omitempty"`
	SystemFingerprint string               `json:"system_fingerprint,omitempty"`
}

// OpenAIStreamChoice is a single choice within a streaming chunk.
type OpenAIStreamChoice struct {
	Index        int          `json:"index"`
	Delta        OpenAIMessage `json:"delta"`
	FinishReason *string      `json:"finish_reason,omitempty"`
}
