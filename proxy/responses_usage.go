package proxy

import (
	"context"
	"encoding/json"

	"github.com/sozercan/vekil/models"
)

// responsesUsage is the token-usage object returned by the OpenAI Responses
// API. Its shape differs from Chat Completions: input_tokens / output_tokens
// (with cached/reasoning nested in detail objects) rather than prompt_tokens /
// completion_tokens. It is mapped onto the chat-shaped RequestSummary fields so
// the traffic dashboard records Responses (Codex) traffic the same way.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u responsesUsage) isZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0
}

// toOpenAIUsage maps the Responses usage shape onto the chat-shaped
// models.OpenAIUsage so it can flow through the existing observeOpenAIUsage /
// setOpenAIUsage path: input→prompt, output→completion, cached and reasoning
// carried in the detail structs.
func (u responsesUsage) toOpenAIUsage() *models.OpenAIUsage {
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	usage := &models.OpenAIUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      total,
	}
	if u.InputTokensDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &models.OpenAIPromptTokensDetails{CachedTokens: u.InputTokensDetails.CachedTokens}
	}
	if u.OutputTokensDetails.ReasoningTokens > 0 {
		usage.CompletionTokensDetails = &models.OpenAICompletionTokensDetails{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens}
	}
	return usage
}

// sniffResponsesUsageBody extracts the usage object from a non-streaming
// Responses JSON body (envelope { "usage": {...} }). Returns the zero value
// when absent or unparseable; callers treat a zero result as "no usage".
func sniffResponsesUsageBody(body []byte) responsesUsage {
	var envelope struct {
		Usage responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return responsesUsage{}
	}
	return envelope.Usage
}

// observeResponsesUsage records a Responses usage object into the per-request
// RequestSummary attached to ctx, if any. A zero usage is ignored so a request
// with no usage data does not overwrite a prior observation with zeros.
func observeResponsesUsage(ctx context.Context, usage responsesUsage) {
	if usage.isZero() {
		return
	}
	observeOpenAIUsage(ctx, usage.toOpenAIUsage())
}

// observeAnthropicUsageBody parses the usage block from a non-streaming
// Anthropic Messages response body and records it onto the per-request
// RequestSummary. Used by the direct anthropic-compatible passthrough, which
// otherwise never converts Anthropic's input_tokens/output_tokens into the
// chat-shaped token fields the dashboard reads. Cache-read tokens map to the
// cached-prompt detail; Anthropic does not report a separate reasoning count.
func observeAnthropicUsageBody(ctx context.Context, body []byte) {
	var parsed struct {
		Usage models.AnthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return
	}
	u := parsed.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return
	}
	usage := &models.OpenAIUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadInputTokens > 0 {
		usage.PromptTokensDetails = &models.OpenAIPromptTokensDetails{CachedTokens: u.CacheReadInputTokens}
	}
	observeOpenAIUsage(ctx, usage)
}

// anthropicStreamUsageAccumulator collects token usage from an Anthropic
// Messages SSE stream so the direct anthropic-compatible streaming passthrough
// can record it (that path only re-frames SSE and otherwise never converts
// Anthropic usage into the chat-shaped fields the dashboard reads). Anthropic
// reports input tokens (and cache-read) on the message_start event and the
// running output-token total on each message_delta event; the last value seen
// wins. Call observe(frameData) for each SSE data payload, then flush(ctx) once
// at stream end.
type anthropicStreamUsageAccumulator struct {
	input      int
	output     int
	cacheRead  int
	haveInput  bool
	haveOutput bool
}

func (a *anthropicStreamUsageAccumulator) observe(data []byte) {
	var event struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *models.AnthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *models.AnthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return
	}
	switch event.Type {
	case "message_start":
		if event.Message != nil && event.Message.Usage != nil {
			a.input = event.Message.Usage.InputTokens
			a.cacheRead = event.Message.Usage.CacheReadInputTokens
			a.haveInput = true
			if event.Message.Usage.OutputTokens > 0 {
				a.output = event.Message.Usage.OutputTokens
				a.haveOutput = true
			}
		}
	case "message_delta":
		if event.Usage != nil && event.Usage.OutputTokens > 0 {
			a.output = event.Usage.OutputTokens
			a.haveOutput = true
		}
	}
}

func (a *anthropicStreamUsageAccumulator) flush(ctx context.Context) {
	if !a.haveInput && !a.haveOutput {
		return
	}
	usage := &models.OpenAIUsage{
		PromptTokens:     a.input,
		CompletionTokens: a.output,
		TotalTokens:      a.input + a.output,
	}
	if a.cacheRead > 0 {
		usage.PromptTokensDetails = &models.OpenAIPromptTokensDetails{CachedTokens: a.cacheRead}
	}
	observeOpenAIUsage(ctx, usage)
}
