package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sozercan/vekil/models"
)

// dateModelRegex strips dated suffixes like -20251001 from model names.
var dateModelRegex = regexp.MustCompile(`-\d{8}$`)

// claudeNumericVersionRegex matches a trailing Claude numeric version pair while
// leaving all preceding model-family hyphens untouched.
var claudeNumericVersionRegex = regexp.MustCompile(`^(claude(?:-[^-]+)*)-(\d+)-(\d+)$`)

// modelAliases maps Anthropic model names to Copilot-compatible names.
var modelAliases = map[string]string{
	"claude-haiku-4-5":  "claude-haiku-4.5",
	"claude-sonnet-4-5": "claude-sonnet-4.5",
	"claude-opus-4-5":   "claude-opus-4.5",
	"claude-sonnet-4-6": "claude-sonnet-4.6",
	"claude-opus-4-6":   "claude-opus-4.6",
}

// NormalizeModelName converts Anthropic model names to Copilot-compatible names.
func NormalizeModelName(model string) string {
	// Strip dated suffix (e.g., claude-sonnet-4-20250514 → claude-sonnet-4)
	normalized := dateModelRegex.ReplaceAllString(model, "")
	// Check aliases (e.g., claude-haiku-4-5 → claude-haiku-4.5)
	if alias, ok := modelAliases[normalized]; ok {
		return alias
	}
	// Convert any remaining trailing Claude numeric version pair while preserving
	// non-version hyphens (e.g., claude-super-long-opus-4-8 →
	// claude-super-long-opus-4.8). Existing dotted versions do not match.
	return claudeNumericVersionRegex.ReplaceAllString(normalized, `${1}-${2}.${3}`)
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
	oaiReq.Messages = mergeSplitAnthropicReplayAssistantTurns(oaiReq.Messages)

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

	// Parallel tool calls
	if len(oaiReq.Tools) > 0 && (req.ToolChoice == nil || !strings.EqualFold(strings.TrimSpace(req.ToolChoice.Type), "none")) {
		parallelToolCalls := true
		if req.ToolChoice != nil && req.ToolChoice.DisableParallelToolUse != nil && *req.ToolChoice.DisableParallelToolUse {
			parallelToolCalls = false
		}
		oaiReq.ParallelToolCalls = &parallelToolCalls
	}

	// Claude's nonblank output effort is the Anthropic equivalent of Chat's
	// reasoning_effort. Preserve it on canonical Chat for direct-route validation;
	// a mapped policy may later replace it with the selected tier effort.
	if req.OutputConfig != nil {
		oaiReq.ReasoningEffort = strings.TrimSpace(req.OutputConfig.Effort)
	}

	// MaxTokens
	oaiReq.MaxTokens = req.MaxTokens

	// Anthropic max_tokens is the hard per-response limit, including current-turn
	// thinking. An interleaved thinking budget may exceed it because that budget
	// is cumulative across thinking blocks, not because one response may exceed
	// the caller's max_tokens ceiling.
	if req.Thinking != nil && req.Thinking.Type == "enabled" && req.MaxTokens != nil {
		tokens := *req.MaxTokens
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

func validateAnthropicOutputConfigEffort(body []byte) error {
	if err, ok := validateAnthropicOutputConfigEffortFast(body); ok {
		return err
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil // The handler reports malformed request JSON before this validation.
	}
	rawOutputConfig, ok := root["output_config"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawOutputConfig), []byte("null")) {
		return nil
	}
	var outputConfig map[string]json.RawMessage
	if err := json.Unmarshal(rawOutputConfig, &outputConfig); err != nil {
		return nil // The typed Anthropic decode reports invalid output_config shapes.
	}
	rawEffort, ok := outputConfig["effort"]
	if !ok {
		return nil
	}
	var effort string
	if err := json.Unmarshal(rawEffort, &effort); err != nil || strings.TrimSpace(effort) == "" {
		return fmt.Errorf("output_config.effort must be a non-empty string")
	}
	return nil
}

func validateAnthropicOutputConfigEffortFast(body []byte) (error, bool) {
	root, ok := newStrictRawJSONObjectScanner(body)
	if !ok {
		return nil, false
	}

	var rawOutputConfig []byte
	outputConfigSeen := false
	for {
		key, start, end, done, scanOK := root.next()
		if !scanOK {
			return nil, false
		}
		if done {
			break
		}
		if !rawJSONKeyEqual(key, "output_config") {
			continue
		}
		if outputConfigSeen {
			return nil, false
		}
		outputConfigSeen = true
		rawOutputConfig = bytes.TrimSpace(body[start:end])
	}
	if !outputConfigSeen || bytes.Equal(rawOutputConfig, []byte("null")) {
		return nil, true
	}
	if len(rawOutputConfig) == 0 || rawOutputConfig[0] != '{' {
		return nil, true
	}

	outputConfig, ok := newStrictRawJSONObjectScanner(rawOutputConfig)
	if !ok {
		return nil, false
	}
	var rawEffort []byte
	effortSeen := false
	for {
		key, start, end, done, scanOK := outputConfig.next()
		if !scanOK {
			return nil, false
		}
		if done {
			break
		}
		if !rawJSONKeyEqual(key, "effort") {
			continue
		}
		if effortSeen {
			return nil, false
		}
		effortSeen = true
		rawEffort = bytes.TrimSpace(rawOutputConfig[start:end])
	}
	if !effortSeen {
		return nil, true
	}

	contentStart, contentEnd, valueEnd, escaped, stringOK := scanStrictRawJSONString(rawEffort, 0)
	if !stringOK || valueEnd != len(rawEffort) {
		return fmt.Errorf("output_config.effort must be a non-empty string"), true
	}
	if escaped {
		return nil, false
	}
	if len(bytes.TrimSpace(rawEffort[contentStart:contentEnd])) == 0 {
		return fmt.Errorf("output_config.effort must be a non-empty string"), true
	}
	return nil, true
}

func mergeSplitAnthropicReplayAssistantTurns(messages []models.OpenAIMessage) []models.OpenAIMessage {
	if len(messages) < 2 {
		return messages
	}
	merged := make([]models.OpenAIMessage, 0, len(messages))
	for _, message := range messages {
		if len(merged) > 0 && shouldMergeSplitAnthropicReplayAssistantTurn(merged[len(merged)-1], message) {
			message.Content = append(json.RawMessage(nil), merged[len(merged)-1].Content...)
			merged[len(merged)-1] = message
			continue
		}
		merged = append(merged, message)
	}
	return merged
}

func shouldMergeSplitAnthropicReplayAssistantTurn(previous, current models.OpenAIMessage) bool {
	if previous.Role != "assistant" || current.Role != "assistant" || len(previous.ToolCalls) != 0 || len(current.ToolCalls) == 0 {
		return false
	}
	if len(previous.Content) == 0 || len(current.Content) != 0 || previous.ToolCallID != "" || current.ToolCallID != "" {
		return false
	}
	for _, call := range current.ToolCalls {
		if !isResponsesChatReplayCallID(call.ID) {
			return false
		}
	}
	return true
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

	// Flattening distinct blocks into Chat's one string welds a heading onto the
	// previous sentence unless a separator goes between them.
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text := derefString(b.Text)
			if text == "" {
				continue
			}
			if sb.Len() > 0 && !strings.HasSuffix(sb.String(), "\n") && !strings.HasSuffix(sb.String(), "\r") &&
				!strings.HasPrefix(text, "\n") && !strings.HasPrefix(text, "\r") {
				sb.WriteString("\n")
			}
			sb.WriteString(text)
		default:
			return nil, fmt.Errorf("unsupported system content block type %q", b.Type)
		}
	}
	if sb.Len() == 0 {
		return nil, nil
	}
	content, _ := json.Marshal(sb.String())
	return &models.OpenAIMessage{Role: "system", Content: content}, nil
}

func translateMessage(msg models.AnthropicMessage) ([]models.OpenAIMessage, error) {
	return translateMessageWithCacheControl(msg, false)
}

func translateMessageWithCacheControl(msg models.AnthropicMessage, preserveCacheControl bool) ([]models.OpenAIMessage, error) {
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
	var textParts strings.Builder
	var multimodalParts []models.OpenAIContentPart
	var toolCalls []models.OpenAIToolCall
	var cacheControl json.RawMessage

	for _, block := range blocks {
		switch block.Type {
		case "text":
			if preserveCacheControl {
				cacheControl = block.CacheControl
			}
			appendTextContentPart(&textParts, &multimodalParts, derefString(block.Text))

		case "image":
			part, err := translateAnthropicImageBlock(block)
			if err != nil {
				return nil, err
			}
			if preserveCacheControl {
				cacheControl = block.CacheControl
			}
			flushTextContentPart(&textParts, &multimodalParts)
			multimodalParts = append(multimodalParts, *part)

		case "tool_use":
			if preserveCacheControl {
				cacheControl = block.CacheControl
			}
			arguments := block.Input
			if !json.Valid(arguments) {
				arguments = json.RawMessage(`{}`)
			}
			toolCalls = append(toolCalls, models.OpenAIToolCall{
				ID:   block.ID,
				Type: "function",
				Function: models.OpenAIFunctionCall{
					Name:      block.Name,
					Arguments: string(arguments),
				},
			})

		case "tool_result":
			toolContent, err := extractToolResultContent(block.Content)
			if err != nil {
				return nil, fmt.Errorf("extracting tool_result content: %w", err)
			}
			contentJSON, _ := json.Marshal(toolContent)
			toolMessage := models.OpenAIMessage{
				Role:       "tool",
				ToolCallID: block.ToolUseID,
				Content:    contentJSON,
			}
			if preserveCacheControl {
				toolMessage.CopilotCacheControl = block.CacheControl
			}
			result = append(result, toolMessage)

		case "thinking", "redacted_thinking":
			// skip thinking blocks

		default:
			return nil, fmt.Errorf("unsupported content block type %q", block.Type)
		}
	}
	// Build the primary message for text/tool_use blocks.
	// When tool_calls are present, prepend before tool_result messages so
	// assistant→tool ordering is preserved.  When only text is present
	// (e.g. a user message carrying both text and tool_results), append
	// after the tool_result messages so tool responses stay adjacent to
	// the preceding assistant tool_calls.
	if textParts.Len() > 0 || len(multimodalParts) > 0 || len(toolCalls) > 0 {
		m := models.OpenAIMessage{Role: msg.Role, CopilotCacheControl: cacheControl}
		switch {
		case len(multimodalParts) > 0:
			content, _ := json.Marshal(multimodalParts)
			m.Content = content
		case textParts.Len() > 0:
			content, _ := json.Marshal(textParts.String())
			m.Content = content
		}
		if len(toolCalls) > 0 {
			m.ToolCalls = toolCalls
			result = append([]models.OpenAIMessage{m}, result...)
		} else {
			result = append(result, m)
		}
	}

	return result, nil
}

func appendTextContentPart(textParts *strings.Builder, multimodalParts *[]models.OpenAIContentPart, text string) {
	if len(*multimodalParts) == 0 {
		textParts.WriteString(text)
		return
	}
	partText := text
	*multimodalParts = append(*multimodalParts, models.OpenAIContentPart{
		Type: "text",
		Text: &partText,
	})
}

func flushTextContentPart(textParts *strings.Builder, multimodalParts *[]models.OpenAIContentPart) {
	if textParts.Len() == 0 {
		return
	}
	partText := textParts.String()
	*multimodalParts = append(*multimodalParts, models.OpenAIContentPart{
		Type: "text",
		Text: &partText,
	})
	textParts.Reset()
}

func translateAnthropicImageBlock(block models.ContentBlock) (*models.OpenAIContentPart, error) {
	if block.Source == nil {
		return nil, fmt.Errorf("image content block is missing source")
	}

	switch block.Source.Type {
	case "base64":
		if block.Source.MediaType == "" || block.Source.Data == "" {
			return nil, fmt.Errorf("base64 image source requires media_type and data")
		}
		if !strings.HasPrefix(strings.ToLower(block.Source.MediaType), "image/") {
			return nil, fmt.Errorf("unsupported image media_type %q", block.Source.MediaType)
		}
		return &models.OpenAIContentPart{
			Type: "image_url",
			ImageURL: &models.OpenAIImageURL{
				URL: fmt.Sprintf("data:%s;base64,%s", block.Source.MediaType, block.Source.Data),
			},
		}, nil
	case "url":
		if block.Source.URL == "" {
			return nil, fmt.Errorf("url image source requires url")
		}
		return &models.OpenAIContentPart{
			Type: "image_url",
			ImageURL: &models.OpenAIImageURL{
				URL: block.Source.URL,
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported image source type %q", block.Source.Type)
	}
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

	var sb strings.Builder
	for _, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("unsupported tool_result content block type %q on Chat translation", b.Type)
		}
		sb.WriteString(derefString(b.Text))
	}
	return sb.String(), nil
}

type anthropicChatCacheContextKey struct{}

func withAnthropicChatCacheControl(ctx context.Context, req *models.AnthropicRequest) context.Context {
	if req == nil {
		return ctx
	}
	hasCache := hasAnthropicBlockCacheControl(req.System)
	for _, message := range req.Messages {
		if hasCache {
			break
		}
		hasCache = hasAnthropicBlockCacheControl(message.Content)
	}
	for _, tool := range req.Tools {
		hasCache = hasCache || !rawJSONIsNullOrEmpty(tool.CacheControl)
	}
	if !hasCache {
		return ctx
	}
	return context.WithValue(ctx, anthropicChatCacheContextKey{}, req)
}

func hasAnthropicBlockCacheControl(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return false
	}
	var blocks []struct {
		Type         string          `json:"type"`
		CacheControl json.RawMessage `json:"cache_control"`
		Content      json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		if !rawJSONIsNullOrEmpty(block.CacheControl) {
			return true
		}
		if block.Type == "tool_result" {
			var parts []struct {
				CacheControl json.RawMessage `json:"cache_control"`
			}
			if json.Unmarshal(block.Content, &parts) == nil {
				for _, part := range parts {
					if !rawJSONIsNullOrEmpty(part.CacheControl) {
						return true
					}
				}
			}
		}
	}
	return false
}

type anthropicChatCacheEdit struct {
	index       int
	original    models.OpenAIMessage
	replacement []models.OpenAIMessage
}

// Cache hints are mapped only after native Chat is selected. Keeping them out of
// canonical Chat preserves strict Responses validation and provider portability.
func applyAnthropicChatCacheControl(ctx context.Context, provider *providerRuntime, path string, body []byte) ([]byte, error) {
	if provider == nil || provider.kind != providerTypeCopilot || path != providerEndpointChatCompletions {
		return body, nil
	}
	req, _ := ctx.Value(anthropicChatCacheContextKey{}).(*models.AnthropicRequest)
	if req == nil {
		return body, nil
	}
	var edits []anthropicChatCacheEdit
	messageCount := 0
	appendEdits := func(original, replacement []models.OpenAIMessage) error {
		if len(original) != len(replacement) {
			if len(original) != 1 {
				return fmt.Errorf("cache_control cannot split a message containing tool calls or results")
			}
			edits = append(edits, anthropicChatCacheEdit{index: messageCount, original: original[0], replacement: replacement})
		} else {
			for i := range original {
				if !rawJSONIsNullOrEmpty(replacement[i].CopilotCacheControl) {
					edits = append(edits, anthropicChatCacheEdit{index: messageCount + i, original: original[i], replacement: replacement[i : i+1]})
				}
			}
		}
		messageCount += len(original)
		return nil
	}
	if len(req.System) > 0 {
		original, err := parseSystemMessage(req.System)
		if err != nil {
			return nil, err
		}
		replacement, err := nativeChatSystemCacheMessages(req.System)
		if err != nil {
			return nil, err
		}
		if original != nil {
			if err := appendEdits([]models.OpenAIMessage{*original}, replacement); err != nil {
				return nil, err
			}
		}
	}
	for _, message := range req.Messages {
		original, err := translateMessage(message)
		if err != nil {
			return nil, err
		}
		replacement, err := nativeChatCacheMessages(message)
		if err != nil {
			return nil, err
		}
		if err := appendEdits(original, replacement); err != nil {
			return nil, err
		}
	}
	hasToolCache := false
	for _, tool := range req.Tools {
		if !rawJSONIsNullOrEmpty(tool.CacheControl) {
			hasToolCache = true
			if err := validateNativeChatCacheControl(tool.CacheControl); err != nil {
				return nil, err
			}
		}
	}
	if len(edits) == 0 && !hasToolCache {
		return body, nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, fmt.Errorf("invalid Chat request while mapping cache_control")
	}
	if len(edits) > 0 {
		var messages []json.RawMessage
		if json.Unmarshal(payload["messages"], &messages) != nil || len(messages) != messageCount {
			return nil, fmt.Errorf("cache_control cannot be mapped after message history changes")
		}
		out := make([]json.RawMessage, 0, len(messages))
		next := 0
		for _, edit := range edits {
			out = append(out, messages[next:edit.index]...)
			var current map[string]json.RawMessage
			if json.Unmarshal(messages[edit.index], &current) != nil || jsonRawString(current["role"]) != edit.original.Role || jsonRawString(current["tool_call_id"]) != edit.original.ToolCallID {
				return nil, fmt.Errorf("cache_control cannot be mapped after message identity changes")
			}
			if len(edit.replacement) == 1 {
				current["copilot_cache_control"] = edit.replacement[0].CopilotCacheControl
				encoded, _ := json.Marshal(current)
				out = append(out, encoded)
			} else {
				if !bytes.Equal(bytes.TrimSpace(current["content"]), bytes.TrimSpace(edit.original.Content)) || !rawJSONIsNullOrEmpty(current["tool_calls"]) {
					return nil, fmt.Errorf("cache_control cannot split modified content or tool-call history")
				}
				for _, replacement := range edit.replacement {
					encoded, _ := json.Marshal(replacement)
					out = append(out, encoded)
				}
			}
			next = edit.index + 1
		}
		out = append(out, messages[next:]...)
		payload["messages"], _ = json.Marshal(out)
	}
	if hasToolCache {
		var tools []map[string]json.RawMessage
		if json.Unmarshal(payload["tools"], &tools) != nil || len(tools) != len(req.Tools) {
			return nil, fmt.Errorf("cache_control cannot be mapped after tool definitions change")
		}
		for i, tool := range req.Tools {
			if !rawJSONIsNullOrEmpty(tool.CacheControl) {
				var function struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(tools[i]["function"], &function) != nil || function.Name != tool.Name {
					return nil, fmt.Errorf("cache_control cannot be mapped after tool identity changes")
				}
				tools[i]["copilot_cache_control"] = tool.CacheControl
			}
		}
		payload["tools"], _ = json.Marshal(tools)
	}
	return json.Marshal(payload)
}

func validateNativeChatCacheControl(raw json.RawMessage) error {
	if rawJSONIsNullOrEmpty(raw) {
		return nil
	}
	value, err := decodeChatJSONObject(raw, "cache_control")
	if err != nil || value == nil || jsonRawString(value["type"]) != "ephemeral" {
		return fmt.Errorf("cache_control must use type ephemeral on native Chat")
	}
	for key := range value {
		if key != "type" && key != "ttl" {
			return fmt.Errorf("unsupported cache_control field on native Chat")
		}
	}
	if ttl, present := value["ttl"]; present && jsonRawString(ttl) != "5m" && jsonRawString(ttl) != "1h" {
		return fmt.Errorf("cache_control.ttl must be 5m or 1h on native Chat")
	}
	return nil
}

func nativeChatSystemCacheMessages(raw json.RawMessage) ([]models.OpenAIMessage, error) {
	var blocks []models.ContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		message, err := parseSystemMessage(raw)
		if err != nil || message == nil {
			return nil, err
		}
		return []models.OpenAIMessage{*message}, nil
	}
	var out []models.OpenAIMessage
	var text strings.Builder
	previousText := ""
	for _, block := range blocks {
		if block.Type != "text" {
			return nil, fmt.Errorf("unsupported system block while mapping cache_control")
		}
		part := derefString(block.Text)
		if part != "" && previousText != "" && !strings.HasSuffix(previousText, "\n") && !strings.HasSuffix(previousText, "\r") && !strings.HasPrefix(part, "\n") && !strings.HasPrefix(part, "\r") {
			part = "\n" + part
		}
		text.WriteString(part)
		if part != "" {
			previousText = part
		}
		if !rawJSONIsNullOrEmpty(block.CacheControl) {
			if err := validateNativeChatCacheControl(block.CacheControl); err != nil {
				return nil, err
			}
			if text.Len() == 0 {
				return nil, fmt.Errorf("cache_control requires non-empty system content")
			}
			content, _ := json.Marshal(text.String())
			out = append(out, models.OpenAIMessage{Role: "system", Content: content, CopilotCacheControl: block.CacheControl})
			text.Reset()
		}
	}
	if text.Len() > 0 {
		content, _ := json.Marshal(text.String())
		out = append(out, models.OpenAIMessage{Role: "system", Content: content})
	}
	return out, nil
}

func nativeChatCacheMessages(message models.AnthropicMessage) ([]models.OpenAIMessage, error) {
	var blocks []models.ContentBlock
	if json.Unmarshal(message.Content, &blocks) != nil {
		return translateMessage(message)
	}
	canSplit := true
	firstPrimary := -1
	lastPrimary := -1
	firstResult := -1
	lastResult := -1
	hasToolCalls := false
	for i, block := range blocks {
		if block.Type != "text" && block.Type != "image" {
			canSplit = false
		}
		if block.Type == "text" || block.Type == "image" || block.Type == "tool_use" {
			if firstPrimary == -1 {
				firstPrimary = i
			}
			lastPrimary = i
		}
		if block.Type == "tool_use" {
			hasToolCalls = true
		}
		if block.Type == "tool_result" {
			if firstResult == -1 {
				firstResult = i
			}
			lastResult = i
		}
	}
	var out []models.OpenAIMessage
	start := 0
	for i := range blocks {
		block := &blocks[i]
		if block.Type == "tool_result" {
			var nested []models.ContentBlock
			if json.Unmarshal(block.Content, &nested) == nil {
				for j, part := range nested {
					if rawJSONIsNullOrEmpty(part.CacheControl) {
						continue
					}
					if j != len(nested)-1 || !rawJSONIsNullOrEmpty(block.CacheControl) && !bytes.Equal(block.CacheControl, part.CacheControl) {
						return nil, fmt.Errorf("cache_control cannot split a tool result on native Chat")
					}
					block.CacheControl = part.CacheControl
				}
			}
		}
		if rawJSONIsNullOrEmpty(block.CacheControl) {
			continue
		}
		if err := validateNativeChatCacheControl(block.CacheControl); err != nil {
			return nil, err
		}
		if !canSplit {
			if block.Type != "tool_result" && i != lastPrimary {
				return nil, fmt.Errorf("cache_control cannot split a message containing tool calls or results")
			}
			// Chat groups tool calls before their results, and tool results
			// before any trailing user text. A boundary cannot cross content
			// moved by that ordering.
			reordered := false
			if block.Type == "tool_result" {
				reordered = hasToolCalls && lastPrimary > i || !hasToolCalls && firstPrimary >= 0 && firstPrimary < i
			} else {
				reordered = hasToolCalls && firstResult >= 0 && firstResult < i || !hasToolCalls && lastResult > i
			}
			if reordered {
				return nil, fmt.Errorf("cache_control cannot cross reordered tool-call or result content")
			}
			continue
		}
		content, _ := json.Marshal(blocks[start : i+1])
		part, err := translateMessageWithCacheControl(models.AnthropicMessage{Role: message.Role, Content: content}, true)
		if err != nil {
			return nil, err
		}
		if len(part) != 1 {
			return nil, fmt.Errorf("cache_control requires non-empty message content")
		}
		out = append(out, part...)
		start = i + 1
	}
	if !canSplit {
		content, _ := json.Marshal(blocks)
		return translateMessageWithCacheControl(models.AnthropicMessage{Role: message.Role, Content: content}, true)
	}
	if start < len(blocks) {
		content, _ := json.Marshal(blocks[start:])
		part, err := translateMessageWithCacheControl(models.AnthropicMessage{Role: message.Role, Content: content}, true)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func translateToolChoice(tc *models.AnthropicToolChoice) (json.RawMessage, error) {
	switch tc.Type {
	case "auto":
		return json.Marshal("auto")
	case "any":
		return json.Marshal("required")
	case "none":
		return json.Marshal("none")
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
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func openAIUsageToAnthropicUsage(usage *models.OpenAIUsage) models.AnthropicUsage {
	if usage == nil {
		return models.AnthropicUsage{}
	}
	cachedTokens := 0
	if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens > 0 {
		cachedTokens = usage.PromptTokensDetails.CachedTokens
	}
	inputTokens := usage.PromptTokens - cachedTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	return models.AnthropicUsage{
		InputTokens:          inputTokens,
		CacheReadInputTokens: cachedTokens,
		OutputTokens:         usage.CompletionTokens,
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

		// Try to extract text content (skip empty/whitespace — Anthropic
		// rejects text blocks that contain no non-whitespace characters).
		if len(msg.Content) > 0 {
			var text string
			if err := json.Unmarshal(msg.Content, &text); err == nil && strings.TrimSpace(text) != "" {
				content = append(content, models.ContentBlock{
					Type: "text",
					Text: stringPtr(text),
				})
			}
		}
		if len(msg.Refusal) > 0 {
			var refusal string
			if err := json.Unmarshal(msg.Refusal, &refusal); err == nil && strings.TrimSpace(refusal) != "" {
				content = append(content, models.ContentBlock{Type: "text", Text: stringPtr(refusal)})
			}
		}

		// Add tool_use blocks
		for _, tc := range msg.ToolCalls {
			input := json.RawMessage(tc.Function.Arguments)
			if !json.Valid(input) {
				input = json.RawMessage(`{}`)
			}
			content = append(content, models.ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
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
		StopReason: &stopReason,
	}

	if resp.Usage != nil {
		result.Usage = openAIUsageToAnthropicUsage(resp.Usage)
	}

	return result
}
