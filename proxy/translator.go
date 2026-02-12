package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sozercan/copilot-proxy/models"
)

// dateModelRegex strips dated suffixes like -20251001 from model names.
var dateModelRegex = regexp.MustCompile(`-\d{8}$`)

// modelAliases maps Anthropic model names to Copilot-compatible names.
var modelAliases = map[string]string{
	"claude-haiku-4-5":   "claude-haiku-4.5",
	"claude-sonnet-4-5":  "claude-sonnet-4.5",
	"claude-opus-4-5":    "claude-opus-4.5",
	"claude-sonnet-4-6":  "claude-sonnet-4.6",
	"claude-opus-4-6":    "claude-opus-4.6",
}

// NormalizeModelName converts Anthropic model names to Copilot-compatible names.
func NormalizeModelName(model string) string {
	// Strip dated suffix (e.g., claude-sonnet-4-20250514 → claude-sonnet-4)
	normalized := dateModelRegex.ReplaceAllString(model, "")
	// Check aliases (e.g., claude-haiku-4-5 → claude-haiku-4.5)
	if alias, ok := modelAliases[normalized]; ok {
		return alias
	}
	// Replace remaining hyphens-as-dots pattern for version numbers
	// e.g., claude-sonnet-4-5 → claude-sonnet-4.5 (already handled above)
	_ = strings.Count(normalized, "-") // keep strings imported
	return normalized
}

// TranslateAnthropicToOpenAI converts an Anthropic Messages API request to OpenAI Chat Completions format.
func TranslateAnthropicToOpenAI(req *models.AnthropicRequest) (*models.OpenAIRequest, error) {
	oaiReq := &models.OpenAIRequest{
		Model:       NormalizeModelName(req.Model),
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	// System message
	if len(req.System) > 0 {
		sysMsg, err := parseSystemMessage(req.System)
		if err != nil {
			return nil, fmt.Errorf("parsing system message: %w", err)
		}
		if sysMsg != nil {
			oaiReq.Messages = append(oaiReq.Messages, *sysMsg)
		}
	}

	// Messages
	for _, msg := range req.Messages {
		translated, err := translateMessage(msg)
		if err != nil {
			return nil, fmt.Errorf("translating message: %w", err)
		}
		oaiReq.Messages = append(oaiReq.Messages, translated...)
	}

	// Tools
	for _, t := range req.Tools {
		oaiReq.Tools = append(oaiReq.Tools, models.OpenAITool{
			Type: "function",
			Function: models.OpenAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	// Tool choice
	if req.ToolChoice != nil {
		tc, err := translateToolChoice(req.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("translating tool choice: %w", err)
		}
		oaiReq.ToolChoice = tc
	}

	// MaxTokens
	oaiReq.MaxTokens = req.MaxTokens

	// Thinking / extended thinking
	if req.Thinking != nil && req.Thinking.Type == "enabled" {
		tokens := req.Thinking.BudgetTokens
		oaiReq.MaxCompletionTokens = &tokens
		oaiReq.MaxTokens = nil
	}

	// Stream
	if req.Stream {
		b := true
		oaiReq.Stream = &b
		oaiReq.StreamOptions = &models.StreamOptions{IncludeUsage: true}
	}

	// Stop sequences
	if len(req.StopSequences) > 0 {
		stop, err := json.Marshal(req.StopSequences)
		if err != nil {
			return nil, fmt.Errorf("marshaling stop sequences: %w", err)
		}
		oaiReq.Stop = stop
	}

	return oaiReq, nil
}

func parseSystemMessage(raw json.RawMessage) (*models.OpenAIMessage, error) {
	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		content, _ := json.Marshal(s)
		return &models.OpenAIMessage{Role: "system", Content: content}, nil
	}

	// Try array of content blocks
	var blocks []models.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("system is neither string nor []ContentBlock: %w", err)
	}

	var text string
	for _, b := range blocks {
		if b.Type == "text" {
			text += b.Text
		}
	}
	if text == "" {
		return nil, nil
	}
	content, _ := json.Marshal(text)
	return &models.OpenAIMessage{Role: "system", Content: content}, nil
}

func translateMessage(msg models.AnthropicMessage) ([]models.OpenAIMessage, error) {
	// Try string content first
	var s string
	if err := json.Unmarshal(msg.Content, &s); err == nil {
		content, _ := json.Marshal(s)
		return []models.OpenAIMessage{{Role: msg.Role, Content: content}}, nil
	}

	// Parse as content blocks
	var blocks []models.ContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, fmt.Errorf("content is neither string nor []ContentBlock: %w", err)
	}

	var result []models.OpenAIMessage
	var textParts string
	var toolCalls []models.OpenAIToolCall

	for _, block := range blocks {
		switch block.Type {
		case "text":
			textParts += block.Text

		case "tool_use":
			toolCalls = append(toolCalls, models.OpenAIToolCall{
				ID:   block.ID,
				Type: "function",
				Function: models.OpenAIFunctionCall{
					Name:      block.Name,
					Arguments: string(block.Input),
				},
			})

		case "tool_result":
			toolContent, err := extractToolResultContent(block.Content)
			if err != nil {
				return nil, fmt.Errorf("extracting tool_result content: %w", err)
			}
			contentJSON, _ := json.Marshal(toolContent)
			result = append(result, models.OpenAIMessage{
				Role:       "tool",
				ToolCallID: block.ToolUseID,
				Content:    contentJSON,
			})

		case "thinking":
			// skip thinking blocks
		}
	}

	// Build the primary message for text/tool_use blocks
	if textParts != "" || len(toolCalls) > 0 {
		m := models.OpenAIMessage{Role: msg.Role}
		if textParts != "" {
			content, _ := json.Marshal(textParts)
			m.Content = content
		}
		if len(toolCalls) > 0 {
			m.ToolCalls = toolCalls
		}
		// Prepend before any tool_result messages
		result = append([]models.OpenAIMessage{m}, result...)
	}

	return result, nil
}

func extractToolResultContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}

	// Try string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	// Try []ContentBlock
	var blocks []models.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("tool_result content is neither string nor []ContentBlock: %w", err)
	}

	var text string
	for _, b := range blocks {
		if b.Type == "text" {
			text += b.Text
		}
	}
	return text, nil
}

func translateToolChoice(tc *models.AnthropicToolChoice) (json.RawMessage, error) {
	switch tc.Type {
	case "auto":
		return json.Marshal("auto")
	case "any":
		return json.Marshal("required")
	case "tool":
		return json.Marshal(map[string]interface{}{
			"type": "function",
			"function": map[string]string{
				"name": tc.Name,
			},
		})
	default:
		return json.Marshal(tc.Type)
	}
}

// MapStopReason maps an OpenAI finish reason to an Anthropic stop reason.
func MapStopReason(finishReason *string) string {
	if finishReason == nil || *finishReason == "" {
		return "end_turn"
	}
	switch *finishReason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// TranslateOpenAIToAnthropic translates an OpenAI Chat Completions response to Anthropic Messages format.
func TranslateOpenAIToAnthropic(resp *models.OpenAIResponse, model string) *models.AnthropicResponse {
	id := resp.ID
	if id == "" {
		id = fmt.Sprintf("msg_%d", resp.Created)
	}

	var content []models.ContentBlock
	var stopReason string

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		msg := choice.Message

		// Try to extract text content
		if len(msg.Content) > 0 {
			var text string
			if err := json.Unmarshal(msg.Content, &text); err == nil && text != "" {
				content = append(content, models.ContentBlock{
					Type: "text",
					Text: text,
				})
			}
		}

		// Add tool_use blocks
		for _, tc := range msg.ToolCalls {
			content = append(content, models.ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(tc.Function.Arguments),
			})
		}

		stopReason = MapStopReason(choice.FinishReason)
	} else {
		stopReason = "end_turn"
	}

	if content == nil {
		content = []models.ContentBlock{}
	}

	result := &models.AnthropicResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      model,
		StopReason: stopReason,
	}

	if resp.Usage != nil {
		result.Usage = models.AnthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}

	return result
}
