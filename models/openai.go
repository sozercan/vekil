package models

import "encoding/json"

type OpenAIRequest struct {
	Model               string          `json:"model"`
	Messages            []OpenAIMessage `json:"messages"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	N                   *int            `json:"n,omitempty"`
	Stream              *bool           `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	LogitBias           json.RawMessage `json:"logit_bias,omitempty"`
	User                string          `json:"user,omitempty"`
	Tools               []OpenAITool    `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
	Seed                *int            `json:"seed,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAIMessage struct {
	Role                string           `json:"role,omitempty"`
	Content             json.RawMessage  `json:"content,omitempty"`
	Refusal             json.RawMessage  `json:"refusal,omitempty"`
	Name                string           `json:"name,omitempty"`
	ToolCalls           []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID          string           `json:"tool_call_id,omitempty"`
	CopilotCacheControl json.RawMessage  `json:"copilot_cache_control,omitempty"`
}

type OpenAIContentPart struct {
	Type     string          `json:"type"`
	Text     *string         `json:"text,omitempty"`
	ImageURL *OpenAIImageURL `json:"image_url,omitempty"`
}

type OpenAIImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type OpenAITool struct {
	Type                string          `json:"type"`
	Function            OpenAIFunction  `json:"function"`
	CopilotCacheControl json.RawMessage `json:"copilot_cache_control,omitempty"`
}

type OpenAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type OpenAIToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function OpenAIFunctionCall `json:"function"`
	Index    *int               `json:"index,omitempty"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type OpenAIResponse struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	Choices           []OpenAIChoice  `json:"choices"`
	Usage             *OpenAIUsage    `json:"usage,omitempty"`
	SystemFingerprint string          `json:"system_fingerprint,omitempty"`
	CopilotUsage      json.RawMessage `json:"copilot_usage,omitempty"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason *string       `json:"finish_reason,omitempty"`
}

type OpenAIUsage struct {
	PromptTokens            int                            `json:"prompt_tokens"`
	CompletionTokens        int                            `json:"completion_tokens"`
	TotalTokens             int                            `json:"total_tokens"`
	ReasoningTokens         int                            `json:"reasoning_tokens,omitempty"`
	PromptTokensDetails     *OpenAIPromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *OpenAICompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type OpenAIPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type OpenAICompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type OpenAIStreamChunk struct {
	ID                string               `json:"id"`
	Object            string               `json:"object"`
	Created           int64                `json:"created"`
	Model             string               `json:"model"`
	Choices           []OpenAIStreamChoice `json:"choices"`
	Usage             *OpenAIUsage         `json:"usage,omitempty"`
	SystemFingerprint string               `json:"system_fingerprint,omitempty"`
	CopilotUsage      json.RawMessage      `json:"copilot_usage,omitempty"`
}

type OpenAIStreamChoice struct {
	Index        int           `json:"index"`
	Delta        OpenAIMessage `json:"delta"`
	FinishReason *string       `json:"finish_reason,omitempty"`
}
