package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sozercan/vekil/models"
)

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

func sniffResponsesUsageBody(body []byte) responsesUsage {
	if usage, ok := sniffResponsesUsageBodyFast(body); ok {
		return usage
	}
	var envelope struct {
		Usage responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return responsesUsage{}
	}
	return envelope.Usage
}

func sniffResponsesUsageBodyFast(body []byte) (responsesUsage, bool) {
	object, ok := newStrictRawJSONObjectScanner(body)
	if !ok {
		return responsesUsage{}, false
	}
	var usageRaw []byte
	usageSeen := false
	for {
		key, start, end, done, scanOK := object.next()
		if !scanOK {
			return responsesUsage{}, false
		}
		if done {
			break
		}
		if rawJSONKeyEqual(key, "usage") {
			if usageSeen {
				return responsesUsage{}, false
			}
			usageSeen = true
			usageRaw = body[start:end]
		} else if rawJSONKeyEqualFold(key, "usage") {
			return responsesUsage{}, false
		}
	}
	if !usageSeen {
		return responsesUsage{}, true
	}
	return parseResponsesUsageFast(usageRaw)
}

func parseResponsesUsageFast(raw []byte) (responsesUsage, bool) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		return responsesUsage{}, true
	}
	object, ok := newRawJSONObjectScanner(raw)
	if !ok {
		return responsesUsage{}, false
	}
	var usage responsesUsage
	var inputSeen, outputSeen, totalSeen, inputDetailsSeen, outputDetailsSeen bool
	for {
		key, start, end, done, scanOK := object.next()
		if !scanOK {
			return responsesUsage{}, false
		}
		if done {
			break
		}
		value := raw[start:end]
		switch {
		case rawJSONKeyEqual(key, "input_tokens"):
			if inputSeen {
				return responsesUsage{}, false
			}
			inputSeen = true
			usage.InputTokens, ok = rawJSONInt(value)
		case rawJSONKeyEqual(key, "output_tokens"):
			if outputSeen {
				return responsesUsage{}, false
			}
			outputSeen = true
			usage.OutputTokens, ok = rawJSONInt(value)
		case rawJSONKeyEqual(key, "total_tokens"):
			if totalSeen {
				return responsesUsage{}, false
			}
			totalSeen = true
			usage.TotalTokens, ok = rawJSONInt(value)
		case rawJSONKeyEqual(key, "input_tokens_details"):
			if inputDetailsSeen {
				return responsesUsage{}, false
			}
			inputDetailsSeen = true
			usage.InputTokensDetails.CachedTokens, ok = parseResponsesUsageDetailFast(value, "cached_tokens")
		case rawJSONKeyEqual(key, "output_tokens_details"):
			if outputDetailsSeen {
				return responsesUsage{}, false
			}
			outputDetailsSeen = true
			usage.OutputTokensDetails.ReasoningTokens, ok = parseResponsesUsageDetailFast(value, "reasoning_tokens")
		default:
			if rawJSONKeyEqualFold(key, "input_tokens") || rawJSONKeyEqualFold(key, "output_tokens") ||
				rawJSONKeyEqualFold(key, "total_tokens") || rawJSONKeyEqualFold(key, "input_tokens_details") ||
				rawJSONKeyEqualFold(key, "output_tokens_details") {
				return responsesUsage{}, false
			}
			ok = true
		}
		if !ok {
			return responsesUsage{}, false
		}
	}
	return usage, true
}

func parseResponsesUsageDetailFast(raw []byte, field string) (int, bool) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		return 0, true
	}
	object, ok := newRawJSONObjectScanner(raw)
	if !ok {
		return 0, false
	}
	value := 0
	seen := false
	for {
		key, start, end, done, scanOK := object.next()
		if !scanOK {
			return 0, false
		}
		if done {
			break
		}
		if rawJSONKeyEqual(key, field) {
			if seen {
				return 0, false
			}
			seen = true
			value, ok = rawJSONInt(raw[start:end])
			if !ok {
				return 0, false
			}
		} else if rawJSONKeyEqualFold(key, field) {
			return 0, false
		}
	}
	return value, true
}

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
		search = search[:idx]
	}
	return found, ok
}

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

func observeResponsesUsage(ctx context.Context, usage responsesUsage) {
	if usage.isZero() {
		return
	}
	observeOpenAIUsage(ctx, usage.toOpenAIUsage())
}

func observeAnthropicUsageBody(ctx context.Context, body []byte) {
	var parsed struct {
		Usage        models.AnthropicUsage `json:"usage"`
		CopilotUsage json.RawMessage       `json:"copilot_usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return
	}
	observeAnthropicUsage(ctx, parsed.Usage)
	observeCopilotUsage(ctx, parsed.CopilotUsage)
}

func observeAnthropicUsage(ctx context.Context, u models.AnthropicUsage) {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
		return
	}
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

type anthropicStreamUsageAccumulator struct {
	input         int
	output        int
	cacheRead     int
	cacheCreation int
	haveInput     bool
	haveOutput    bool
}

func (a *anthropicStreamUsageAccumulator) observe(data []byte) json.RawMessage {
	var event struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *models.AnthropicUsage `json:"usage"`
		} `json:"message"`
		Usage        *models.AnthropicUsage `json:"usage"`
		CopilotUsage json.RawMessage        `json:"copilot_usage"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return event.CopilotUsage
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
	return event.CopilotUsage
}

func anthropicStreamErrorStatus(data []byte) (int, bool) {
	var event struct {
		Type  string `json:"type"`
		Error *struct {
			Type string `json:"type"`
			Code string `json:"code"`
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
		switch strings.ToLower(strings.TrimSpace(event.Error.Code)) {
		case "user_model_rate_limited", "user_global_rate_limited", "user_weekly_rate_limited", "integration_rate_limited":
			return http.StatusTooManyRequests, true
		}
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
