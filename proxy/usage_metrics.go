package proxy

import (
	"encoding/json"

	"github.com/sozercan/vekil/models"
)

func observeOpenAIUsage(scope *requestMetricsScope, usage *models.OpenAIUsage) {
	if scope == nil || usage == nil {
		return
	}
	scope.observeTokens("input", usage.PromptTokens)
	scope.observeTokens("output", usage.CompletionTokens)
}

func observeAnthropicUsage(scope *requestMetricsScope, usage models.AnthropicUsage) {
	if scope == nil {
		return
	}
	scope.observeTokens("input", usage.InputTokens)
	scope.observeTokens("output", usage.OutputTokens)
}

func extractResponsesUsage(body []byte) (inputTokens, outputTokens int, ok bool) {
	var payload struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0, false
	}
	return payload.Usage.InputTokens, payload.Usage.OutputTokens, true
}
