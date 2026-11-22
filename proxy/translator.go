package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/sozercan/vekil/models"
)

var dateModelRegex = regexp.MustCompile(`-\d{8}$`)

var claudeNumericVersionRegex = regexp.MustCompile(`^(claude(?:-[^-]+)*)-(\d+)-(\d+)$`)

var modelAliases = map[string]string{
	"claude-haiku-4-5":  "claude-haiku-4.5",
	"claude-sonnet-4-5": "claude-sonnet-4.5",
	"claude-opus-4-5":   "claude-opus-4.5",
	"claude-sonnet-4-6": "claude-sonnet-4.6",
	"claude-opus-4-6":   "claude-opus-4.6",
}

func NormalizeModelName(model string) string {
	normalized := dateModelRegex.ReplaceAllString(model, "")
	if alias, ok := modelAliases[normalized]; ok {
		return alias
	}
	return claudeNumericVersionRegex.ReplaceAllString(normalized, `${1}-${2}.${3}`)
}

func TranslateAnthropicToOpenAI(req *models.AnthropicRequest) (*models.OpenAIRequest, error) {
	oaiReq := &models.OpenAIRequest{
		Model:       NormalizeModelName(req.Model),
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}

	if len(req.System) > 0 {
		sysMsg, err := parseSystemMessage(req.System)
		if err != nil {
			return nil, fmt.Errorf("parsing system message: %w", err)
		}
		if sysMsg != nil {
			oaiReq.Messages = append(oaiReq.Messages, *sysMsg)
		}
	}

	projection, err := projectAnthropicChatMessages(req.Messages, false)
	if err != nil {
		return nil, fmt.Errorf("translating message: %w", err)
	}
	for _, message := range projection {
		oaiReq.Messages = append(oaiReq.Messages, message.value)
	}

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

	if req.ToolChoice != nil {
		tc, err := translateToolChoice(req.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("translating tool choice: %w", err)
		}
		oaiReq.ToolChoice = tc
	}

	if len(oaiReq.Tools) > 0 && (req.ToolChoice == nil || !strings.EqualFold(strings.TrimSpace(req.ToolChoice.Type), "none")) {
		parallelToolCalls := true
		if req.ToolChoice != nil && req.ToolChoice.DisableParallelToolUse != nil && *req.ToolChoice.DisableParallelToolUse {
			parallelToolCalls = false
		}
		oaiReq.ParallelToolCalls = &parallelToolCalls
	}

	if req.OutputConfig != nil {
		oaiReq.ReasoningEffort = strings.TrimSpace(req.OutputConfig.Effort)
	}

	oaiReq.MaxTokens = req.MaxTokens

	if req.Thinking != nil && req.Thinking.Type == "enabled" && req.MaxTokens != nil {
		tokens := *req.MaxTokens
		oaiReq.MaxCompletionTokens = &tokens
		oaiReq.MaxTokens = nil
	}

	if req.Stream {
		b := true
		oaiReq.Stream = &b
		oaiReq.StreamOptions = &models.StreamOptions{IncludeUsage: true}
	}

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
		return nil
	}
	rawOutputConfig, ok := root["output_config"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawOutputConfig), []byte("null")) {
		return nil
	}
	var outputConfig map[string]json.RawMessage
	if err := json.Unmarshal(rawOutputConfig, &outputConfig); err != nil {
		return nil
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

func mergeSplitAnthropicReplayAssistantTurns(messages []anthropicChatMessage) []anthropicChatMessage {
	if len(messages) < 2 {
		return messages
	}
	merged := make([]anthropicChatMessage, 0, len(messages))
	for _, message := range messages {
		if len(merged) > 0 && shouldMergeSplitAnthropicReplayAssistantTurn(merged[len(merged)-1].value, message.value) {
			previous := merged[len(merged)-1]
			message.value.Content = append(json.RawMessage(nil), previous.value.Content...)
			message.origins = append(append([]anthropicChatOrigin(nil), previous.origins...), message.origins...)
			message.first = min(previous.first, message.first)
			message.last = max(previous.last, message.last)
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
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		content, _ := json.Marshal(s)
		return &models.OpenAIMessage{Role: "system", Content: content}, nil
	}

	var blocks []models.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("system is neither string nor []ContentBlock: %w", err)
	}

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

type anthropicChatOrigin struct {
	message int
	block   int
	kind    uint8
}

const (
	anthropicPrimaryOrigin uint8 = iota
	anthropicToolTextOrigin
	anthropicToolImagesOrigin
)

type anthropicChatMessage struct {
	value   models.OpenAIMessage
	origins []anthropicChatOrigin
	first   int
	last    int
	images  *anthropicChatMessage
}

type anthropicCacheBoundary struct {
	after int
	value json.RawMessage
}

func projectAnthropicChatMessages(messages []models.AnthropicMessage, preserveCacheControl bool) ([]anthropicChatMessage, error) {
	var out, pending []anthropicChatMessage
	var boundaries []anthropicCacheBoundary
	position := 0
	resultIndex := 0
	flush := func() {
		out = append(out, pending...)
		pending = nil
	}
	appendParts := func(parts []anthropicChatMessage) {
		for _, part := range parts {
			if part.value.Role != "tool" {
				flush()
			}
			if part.images != nil {
				pending = append(pending, *part.images)
				part.images = nil
			}
			out = append(out, part)
		}
	}
	for index, message := range messages {
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			position++
			content, _ := json.Marshal(text)
			appendParts([]anthropicChatMessage{{
				value:   models.OpenAIMessage{Role: message.Role, Content: content},
				origins: []anthropicChatOrigin{{message: index, block: -1, kind: anthropicPrimaryOrigin}},
				first:   position, last: position,
			}})
			continue
		}
		var blocks []models.ContentBlock
		if err := json.Unmarshal(message.Content, &blocks); err != nil {
			return nil, fmt.Errorf("content is neither string nor []ContentBlock: %w", err)
		}
		base := position
		position += len(blocks)
		canSplit := false
		if preserveCacheControl {
			var err error
			canSplit, err = validateNativeChatCacheBlocks(blocks)
			if err != nil {
				return nil, err
			}
			for i, block := range blocks {
				if !rawJSONIsNullOrEmpty(block.CacheControl) {
					boundaries = append(boundaries, anthropicCacheBoundary{after: base + i + 1, value: block.CacheControl})
				}
			}
		}
		start := 0
		if canSplit {
			for i, block := range blocks {
				if rawJSONIsNullOrEmpty(block.CacheControl) {
					continue
				}
				parts, err := projectAnthropicChatBlocks(message.Role, blocks[start:i+1], index, start, base, &resultIndex)
				if err != nil {
					return nil, err
				}
				if len(parts) != 1 {
					return nil, fmt.Errorf("cache_control requires non-empty message content")
				}
				appendParts(parts)
				start = i + 1
			}
		}
		if start < len(blocks) {
			parts, err := projectAnthropicChatBlocks(message.Role, blocks[start:], index, start, base, &resultIndex)
			if err != nil {
				return nil, err
			}
			appendParts(parts)
		}
	}
	flush()
	out = mergeSplitAnthropicReplayAssistantTurns(out)
	if err := applyAnthropicCacheBoundaries(out, boundaries); err != nil {
		return nil, err
	}
	return out, nil
}

func applyAnthropicCacheBoundaries(messages []anthropicChatMessage, boundaries []anthropicCacheBoundary) error {
	if len(boundaries) == 0 {
		return nil
	}
	prefixMax := make([]int, len(messages))
	suffixMin := make([]int, len(messages))
	for index, message := range messages {
		prefixMax[index] = message.last
		if index > 0 {
			prefixMax[index] = max(prefixMax[index], prefixMax[index-1])
		}
	}
	for index := len(messages) - 1; index >= 0; index-- {
		suffixMin[index] = messages[index].first
		if index+1 < len(messages) {
			suffixMin[index] = min(suffixMin[index], suffixMin[index+1])
		}
	}
	end := 0
	for _, boundary := range boundaries {
		for end < len(messages) && prefixMax[end] <= boundary.after {
			end++
		}
		if end < len(messages) && suffixMin[end] <= boundary.after {
			return fmt.Errorf("cache_control cannot cross reordered tool-call or result content")
		}
		if end == 0 {
			return fmt.Errorf("cache_control requires non-empty message content")
		}
		current := messages[end-1].value.CopilotCacheControl
		if !rawJSONIsNullOrEmpty(current) && !sameCompactJSON(current, boundary.value) {
			return fmt.Errorf("cache_control has conflicting source boundaries")
		}
		messages[end-1].value.CopilotCacheControl = boundary.value
	}
	return nil
}

func sameCompactJSON(left, right json.RawMessage) bool {
	var compactLeft, compactRight bytes.Buffer
	if json.Compact(&compactLeft, left) != nil || json.Compact(&compactRight, right) != nil {
		return bytes.Equal(left, right)
	}
	return bytes.Equal(compactLeft.Bytes(), compactRight.Bytes())
}

func projectAnthropicChatBlocks(role string, blocks []models.ContentBlock, source, offset, base int, resultIndex *int) ([]anthropicChatMessage, error) {
	var results []anthropicChatMessage
	var text strings.Builder
	var parts []models.OpenAIContentPart
	var calls []models.OpenAIToolCall
	first, last := 0, 0
	for i, block := range blocks {
		position := base + offset + i + 1
		if block.Type == "text" || block.Type == "image" || block.Type == "tool_use" {
			if first == 0 {
				first = position
			}
			last = position
		}
		switch block.Type {
		case "text":
			appendTextContentPart(&text, &parts, derefString(block.Text))
		case "image":
			part, err := translateAnthropicImageBlock(block)
			if err != nil {
				return nil, err
			}
			flushTextContentPart(&text, &parts)
			parts = append(parts, *part)
		case "tool_use":
			arguments := block.Input
			if !json.Valid(arguments) {
				arguments = json.RawMessage(`{}`)
			}
			calls = append(calls, models.OpenAIToolCall{
				ID: block.ID, Type: "function",
				Function: models.OpenAIFunctionCall{Name: block.Name, Arguments: string(arguments)},
			})
		case "tool_result":
			*resultIndex++
			content, images, err := extractToolResultContent(block.Content, *resultIndex)
			if err != nil {
				return nil, fmt.Errorf("extracting tool_result content: %w", err)
			}
			if len(images) > 0 && role != "user" {
				return nil, fmt.Errorf("image-bearing tool_result requires a user message")
			}
			contentJSON, _ := json.Marshal(content)
			result := anthropicChatMessage{
				value:   models.OpenAIMessage{Role: "tool", ToolCallID: block.ToolUseID, Content: contentJSON},
				origins: []anthropicChatOrigin{{message: source, block: offset + i, kind: anthropicToolTextOrigin}},
				first:   position, last: position,
			}
			if len(images) > 0 {
				imageJSON, _ := json.Marshal(images)
				result.images = &anthropicChatMessage{
					value:   models.OpenAIMessage{Role: "user", Content: imageJSON},
					origins: []anthropicChatOrigin{{message: source, block: offset + i, kind: anthropicToolImagesOrigin}},
					first:   position, last: position,
				}
			}
			results = append(results, result)
		case "thinking", "redacted_thinking":
		default:
			return nil, fmt.Errorf("unsupported content block type %q", block.Type)
		}
	}
	if text.Len() == 0 && len(parts) == 0 && len(calls) == 0 {
		return results, nil
	}
	primary := anthropicChatMessage{
		value:   models.OpenAIMessage{Role: role},
		origins: []anthropicChatOrigin{{message: source, block: -1, kind: anthropicPrimaryOrigin}},
		first:   first, last: last,
	}
	if len(parts) > 0 {
		primary.value.Content, _ = json.Marshal(parts)
	} else if text.Len() > 0 {
		primary.value.Content, _ = json.Marshal(text.String())
	}
	if len(calls) > 0 {
		primary.value.ToolCalls = calls
		return append([]anthropicChatMessage{primary}, results...), nil
	}
	return append(results, primary), nil
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

func extractToolResultContent(raw json.RawMessage, result int) (string, []models.OpenAIContentPart, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var blocks []models.ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, fmt.Errorf("tool_result content is neither string nor []ContentBlock: %w", err)
	}
	var output strings.Builder
	var images []models.OpenAIContentPart
	imageIndex := 0
	for _, block := range blocks {
		switch block.Type {
		case "text":
			output.WriteString(derefString(block.Text))
		case "image":
			if block.Source != nil && block.Source.Type == "base64" {
				if strings.TrimSpace(block.Source.Data) == "" {
					return "", nil, fmt.Errorf("base64 image source requires non-empty data")
				}
				source := *block.Source
				source.MediaType = strings.ToLower(source.MediaType)
				if strings.HasPrefix(source.MediaType, "image/") && !validToolResultImageMediaType(source.MediaType) {
					return "", nil, fmt.Errorf("tool_result base64 image source requires an image/<subtype> media_type")
				}
				block.Source = &source
			}
			if block.Source != nil && block.Source.Type == "url" {
				parsed, err := url.Parse(block.Source.URL)
				if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
					return "", nil, fmt.Errorf("tool_result url image source requires an HTTP(S) URL")
				}
			}
			part, err := translateAnthropicImageBlock(block)
			if err != nil {
				return "", nil, err
			}
			if block.Source.Type == "base64" && !validBase64ImageData(block.Source.Data) {
				return "", nil, fmt.Errorf("tool_result image requires an HTTP(S) URL or valid base64 image data")
			}
			imageIndex++
			label := fmt.Sprintf("Tool result %d image %d", result, imageIndex)
			fmt.Fprintf(&output, "[%s]", label)
			preamble := label + " (tool results numbered in conversation order; attachment from tool output):"
			images = append(images, models.OpenAIContentPart{Type: "text", Text: &preamble}, *part)
		default:
			return "", nil, fmt.Errorf("unsupported tool_result content block type %q on Chat translation", block.Type)
		}
	}
	return output.String(), images, nil
}

func validBase64ImageData(data string) bool {
	if padding := strings.IndexByte(data, '='); padding >= 0 && strings.Trim(data[padding:], "=\r\n") != "" {
		return false
	}
	_, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
	return err == nil
}

func validToolResultImageMediaType(mediaType string) bool {
	subtype, ok := strings.CutPrefix(mediaType, "image/")
	if !ok || subtype == "" {
		return false
	}
	for _, r := range subtype {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("!#$&-^_.+", r) {
			return false
		}
	}
	return true
}

type anthropicChatCacheControlContextKey struct{}

func withAnthropicChatCacheControl(ctx context.Context, req *models.AnthropicRequest) context.Context {
	if req == nil {
		return ctx
	}
	hasCacheControl := hasAnthropicChatCacheControl(req.System)
	for _, message := range req.Messages {
		if hasCacheControl {
			break
		}
		hasCacheControl = hasAnthropicChatCacheControl(message.Content)
	}
	for _, tool := range req.Tools {
		hasCacheControl = hasCacheControl || !rawJSONIsNullOrEmpty(tool.CacheControl)
	}
	if !hasCacheControl {
		return ctx
	}
	return context.WithValue(ctx, anthropicChatCacheControlContextKey{}, &anthropicChatCacheRequest{request: req})
}

func hasAnthropicChatCacheControl(raw json.RawMessage) bool {
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

type anthropicChatCachePlan struct {
	canonical []models.OpenAIMessage
	edits     []anthropicChatCacheEdit
	toolCache bool
}

type anthropicChatCacheRequest struct {
	request *models.AnthropicRequest
	once    sync.Once
	plan    anthropicChatCachePlan
	err     error
}

func (r *anthropicChatCacheRequest) load() (anthropicChatCachePlan, error) {
	r.once.Do(func() { r.plan, r.err = buildAnthropicChatCachePlan(r.request) })
	return r.plan, r.err
}

func buildAnthropicChatCachePlan(req *models.AnthropicRequest) (anthropicChatCachePlan, error) {
	var plan anthropicChatCachePlan
	appendEdit := func(original models.OpenAIMessage, replacement []models.OpenAIMessage) {
		index := len(plan.canonical)
		plan.canonical = append(plan.canonical, original)
		if len(replacement) != 1 || !rawJSONIsNullOrEmpty(replacement[0].CopilotCacheControl) {
			plan.edits = append(plan.edits, anthropicChatCacheEdit{index: index, original: original, replacement: replacement})
		}
	}
	if len(req.System) > 0 {
		original, err := parseSystemMessage(req.System)
		if err != nil {
			return anthropicChatCachePlan{}, err
		}
		replacement, err := nativeChatSystemCacheMessages(req.System)
		if err != nil {
			return anthropicChatCachePlan{}, err
		}
		if original != nil {
			appendEdit(*original, replacement)
		}
	}
	original, err := projectAnthropicChatMessages(req.Messages, false)
	if err != nil {
		return anthropicChatCachePlan{}, err
	}
	replacement, err := projectAnthropicChatMessages(req.Messages, true)
	if err != nil {
		return anthropicChatCachePlan{}, err
	}
	groups, err := groupAnthropicCacheProjection(original, replacement)
	if err != nil {
		return anthropicChatCachePlan{}, err
	}
	for index, message := range original {
		appendEdit(message.value, groups[index])
	}
	for _, tool := range req.Tools {
		if !rawJSONIsNullOrEmpty(tool.CacheControl) {
			plan.toolCache = true
			if err := validateNativeChatCacheControl(tool.CacheControl); err != nil {
				return anthropicChatCachePlan{}, err
			}
		}
	}
	return plan, nil
}

func applyAnthropicChatCacheControl(ctx context.Context, provider *providerRuntime, path string, body []byte) ([]byte, error) {
	if provider == nil || provider.kind != providerTypeCopilot || path != providerEndpointChatCompletions {
		return body, nil
	}
	cached, _ := ctx.Value(anthropicChatCacheControlContextKey{}).(*anthropicChatCacheRequest)
	if cached == nil || cached.request == nil {
		return body, nil
	}
	req := cached.request
	plan, err := cached.load()
	if err != nil {
		return nil, err
	}
	edits, canonical, hasToolCache := plan.edits, plan.canonical, plan.toolCache
	if len(edits) == 0 && !hasToolCache {
		return body, nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return nil, fmt.Errorf("invalid Chat request while mapping native metadata")
	}
	if len(edits) > 0 {
		var messages []json.RawMessage
		if json.Unmarshal(payload["messages"], &messages) != nil || len(messages) != len(canonical) {
			return nil, fmt.Errorf("native Chat metadata cannot be mapped after message history changes")
		}
		if err := validateAnthropicCacheProjectionBody(messages, canonical); err != nil {
			return nil, err
		}
		out := make([]json.RawMessage, 0, len(messages))
		next := 0
		for _, edit := range edits {
			out = append(out, messages[next:edit.index]...)
			var current map[string]json.RawMessage
			if json.Unmarshal(messages[edit.index], &current) != nil {
				return nil, fmt.Errorf("native Chat metadata cannot be mapped after message identity changes")
			}
			if len(edit.replacement) == 1 {
				replacement := edit.replacement[0]
				if !rawJSONIsNullOrEmpty(replacement.CopilotCacheControl) {
					current["copilot_cache_control"] = replacement.CopilotCacheControl
				}
				encoded, _ := json.Marshal(current)
				out = append(out, encoded)
			} else {
				if !rawJSONIsNullOrEmpty(current["tool_calls"]) {
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

func validateAnthropicCacheProjectionBody(messages []json.RawMessage, canonical []models.OpenAIMessage) error {
	for index, raw := range messages {
		var current map[string]json.RawMessage
		if json.Unmarshal(raw, &current) != nil || jsonRawString(current["role"]) != strings.TrimSpace(canonical[index].Role) || jsonRawString(current["tool_call_id"]) != strings.TrimSpace(canonical[index].ToolCallID) {
			return fmt.Errorf("native Chat metadata cannot be mapped after message identity changes")
		}
		if canonical[index].Role != "tool" && !bytes.Equal(bytes.TrimSpace(current["content"]), bytes.TrimSpace(canonical[index].Content)) {
			return fmt.Errorf("native Chat metadata cannot be mapped after non-tool content changes")
		}
	}
	return nil
}

func groupAnthropicCacheProjection(original, replacement []anthropicChatMessage) ([][]models.OpenAIMessage, error) {
	owners := make(map[anthropicChatOrigin]int)
	for index, message := range original {
		for _, origin := range message.origins {
			if previous, exists := owners[origin]; exists && previous != index {
				return nil, fmt.Errorf("cache_control cannot map ambiguous message origins")
			}
			owners[origin] = index
		}
	}
	groups := make([][]models.OpenAIMessage, len(original))
	previous := 0
	for _, message := range replacement {
		owner := -1
		for _, origin := range message.origins {
			index, exists := owners[origin]
			if !exists || owner >= 0 && owner != index {
				return nil, fmt.Errorf("cache_control cannot cross message projection boundaries")
			}
			owner = index
		}
		if owner < previous || owner < 0 {
			return nil, fmt.Errorf("cache_control cannot reorder message projection boundaries")
		}
		groups[owner] = append(groups[owner], message.value)
		previous = owner
	}
	for _, group := range groups {
		if len(group) == 0 {
			return nil, fmt.Errorf("cache_control cannot omit message projection boundaries")
		}
	}
	return groups, nil
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

func validateNativeChatCacheBlocks(blocks []models.ContentBlock) (bool, error) {
	canSplit := true
	firstPrimary, lastPrimary, firstResult, lastResult := -1, -1, -1, -1
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
	for i := range blocks {
		block := &blocks[i]
		if block.Type == "tool_result" {
			var nested []models.ContentBlock
			if json.Unmarshal(block.Content, &nested) == nil {
				for j, part := range nested {
					if rawJSONIsNullOrEmpty(part.CacheControl) {
						continue
					}
					if j != len(nested)-1 || !rawJSONIsNullOrEmpty(block.CacheControl) && !sameCompactJSON(block.CacheControl, part.CacheControl) {
						return false, fmt.Errorf("cache_control cannot split a tool result on native Chat")
					}
					block.CacheControl = part.CacheControl
				}
			}
		}
		if rawJSONIsNullOrEmpty(block.CacheControl) {
			continue
		}
		if err := validateNativeChatCacheControl(block.CacheControl); err != nil {
			return false, err
		}
		if !canSplit {
			if block.Type != "tool_result" && i != lastPrimary {
				return false, fmt.Errorf("cache_control cannot split a message containing tool calls or results")
			}
			reordered := false
			if block.Type == "tool_result" {
				reordered = hasToolCalls && lastPrimary > i || !hasToolCalls && firstPrimary >= 0 && firstPrimary < i
			} else {
				reordered = hasToolCalls && firstResult >= 0 && firstResult < i || !hasToolCalls && lastResult > i
			}
			if reordered {
				return false, fmt.Errorf("cache_control cannot cross reordered tool-call or result content")
			}
		}
	}
	return canSplit, nil
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
