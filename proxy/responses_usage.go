package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

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

func (u responsesUsage) totalTokens() int {
	if u.TotalTokens != 0 {
		return u.TotalTokens
	}
	return u.InputTokens + u.OutputTokens
}

// add accumulates another Responses usage observation. It normalizes omitted
// totals before summing so internal compaction usage can be folded into a
// terminal client turn without losing cached/reasoning details from that turn.
func (u *responsesUsage) add(other responsesUsage) {
	if u == nil || other.isZero() {
		return
	}
	total := u.totalTokens() + other.totalTokens()
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.TotalTokens = total
	u.InputTokensDetails.CachedTokens += other.InputTokensDetails.CachedTokens
	u.OutputTokensDetails.ReasoningTokens += other.OutputTokensDetails.ReasoningTokens
}

// toOpenAIUsage maps the Responses usage shape onto the chat-shaped
// models.OpenAIUsage so it can flow through the existing observeOpenAIUsage /
// setOpenAIUsage path: input→prompt, output→completion, cached and reasoning
// carried in the detail structs.
func (u responsesUsage) toOpenAIUsage() *models.OpenAIUsage {
	total := u.totalTokens()
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

// extractResponsesUsageObject finds the last "usage":{...} object in a raw byte
// buffer and parses it as a responsesUsage. It is used to recover usage from an
// oversized streamed response.completed event whose full JSON exceeds the
// failure tap's buffer cap (so it cannot be parsed as a whole). A streamed
// response.completed embeds usage after its (large) output, so the last
// balanced object following a "usage" key is the response usage. Returns ok=false
// when no parseable, non-zero usage object is found.
func extractResponsesUsageObject(buf []byte) (responsesUsage, bool) {
	key := []byte(`"usage"`)
	search := buf
	var found responsesUsage
	ok := false
	for {
		idx := bytes.LastIndex(search, key)
		if idx < 0 {
			break
		}
		// Find the opening brace after the key (skipping whitespace and the colon).
		j := idx + len(key)
		for j < len(search) && (search[j] == ' ' || search[j] == '\t' || search[j] == ':' || search[j] == '\r' || search[j] == '\n') {
			j++
		}
		if j < len(search) && search[j] == '{' {
			if obj, end := balancedJSONObject(search[j:]); end > 0 {
				var u responsesUsage
				if err := json.Unmarshal(obj, &u); err == nil && !u.isZero() {
					return u, true
				}
			}
		}
		// Not a usable usage object here; keep searching earlier in the buffer.
		search = search[:idx]
	}
	return found, ok
}

// balancedJSONObject returns the leading balanced { ... } object of buf (which
// must start with '{') and the index just past its closing brace, or (nil, 0) if
// the object is not closed within buf. It respects JSON string quoting/escaping
// so braces inside strings do not affect the depth count.
func balancedJSONObject(buf []byte) ([]byte, int) {
	if len(buf) == 0 || buf[0] != '{' {
		return nil, 0
	}
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(buf); i++ {
		c := buf[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return buf[:i+1], i + 1
			}
		}
	}
	return nil, 0
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
	observeAnthropicUsage(ctx, parsed.Usage)
}

func observeAnthropicUsage(ctx context.Context, u models.AnthropicUsage) {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
		return
	}
	// Anthropic reports input_tokens as only the non-cached tokens after the last
	// cache breakpoint; cache reads and writes are counted separately. Total input
	// = input_tokens + cache_read_input_tokens + cache_creation_input_tokens. Fold
	// the cache tokens into the prompt/total so cached-prompt volume is not
	// undercounted (and the cached-prompt percentage cannot exceed 100%).
	promptTokens := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	usage := &models.OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      promptTokens + u.OutputTokens,
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
// reports input tokens plus cache read/write on the message_start event and the
// running output-token total on each message_delta event; the last value seen
// wins. Call observe(frameData) for each SSE data payload, then flush(ctx) once
// at stream end.
type anthropicStreamUsageAccumulator struct {
	input         int
	output        int
	cacheRead     int
	cacheCreation int
	haveInput     bool
	haveOutput    bool
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
			a.cacheCreation = event.Message.Usage.CacheCreationInputTokens
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

// anthropicStreamErrorStatus inspects an Anthropic SSE data payload and, if it
// is an error event ({"type":"error","error":{"type":...}}), returns the mapped
// HTTP status and ok=true. Anthropic streams an error frame for post-commit
// failures (e.g. overloaded_error, rate_limit_error). Returns ok=false for any
// non-error frame.
func anthropicStreamErrorStatus(data []byte) (int, bool) {
	var event struct {
		Type  string `json:"type"`
		Error *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return 0, false
	}
	if event.Type != "error" && event.Error == nil {
		return 0, false
	}
	errType := ""
	if event.Error != nil {
		errType = strings.ToLower(strings.TrimSpace(event.Error.Type))
	}
	switch errType {
	case "rate_limit_error", "rate_limit_exceeded":
		return http.StatusTooManyRequests, true
	case "overloaded_error":
		return http.StatusServiceUnavailable, true
	case "api_error", "internal_server_error":
		return http.StatusBadGateway, true
	case "invalid_request_error":
		return http.StatusBadRequest, true
	case "authentication_error":
		return http.StatusUnauthorized, true
	case "permission_error":
		return http.StatusForbidden, true
	case "not_found_error":
		return http.StatusNotFound, true
	}
	return http.StatusBadGateway, true
}

func (a *anthropicStreamUsageAccumulator) flush(ctx context.Context) {
	if !a.haveInput && !a.haveOutput {
		return
	}
	// input_tokens counts only the non-cached tokens; fold cache read/write into
	// the prompt total so cached-prompt volume is not undercounted (matching the
	// non-streaming observeAnthropicUsageBody path).
	promptTokens := a.input + a.cacheRead + a.cacheCreation
	usage := &models.OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: a.output,
		TotalTokens:      promptTokens + a.output,
	}
	if a.cacheRead > 0 {
		usage.PromptTokensDetails = &models.OpenAIPromptTokensDetails{CachedTokens: a.cacheRead}
	}
	observeOpenAIUsage(ctx, usage)
}
