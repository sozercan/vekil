package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sozercan/vekil/models"
)

const geminiFunctionCallIDPrefix = "gemini-fc"

var geminiModelAliases = map[string]string{
	"chat-compression-3-pro":             "gemini-3-pro-preview",
	"chat-compression-3-flash":           "gemini-3-flash-preview",
	"chat-compression-2.5-pro":           "gemini-2.5-pro",
	"chat-compression-2.5-flash":         "gemini-3-flash-preview",
	"chat-compression-2.5-flash-lite":    "gemini-3-flash-preview",
	"chat-compression-default":           "gemini-3-pro-preview",
	"gemini-3.1-pro-preview-customtools": "gemini-3.1-pro-preview",
	"gemini-2.5-flash":                   "gemini-3-flash-preview",
	"gemini-2.5-flash-lite":              "gemini-3-flash-preview",
}

type openAIContentBuilder struct {
	parts      []models.OpenAIContentPart
	hasNonText bool
}

type geminiPendingToolCall struct {
	toolCallID string
	explicitID string
	batch      int
}

type geminiExplicitToolCall struct {
	name       string
	toolCallID string
	batch      int
}

type geminiPendingToolCalls struct {
	byName       map[string][]geminiPendingToolCall
	byExplicitID map[string]geminiExplicitToolCall
	batch        int
	answered     bool
}

func (b *openAIContentBuilder) appendText(text string) {
	b.parts = append(b.parts, models.OpenAIContentPart{
		Type: "text",
		Text: stringPtr(text),
	})
}

func (b *openAIContentBuilder) append(part models.OpenAIContentPart) {
	if part.Type != "text" {
		b.hasNonText = true
	}
	b.parts = append(b.parts, part)
}

func (b *openAIContentBuilder) empty() bool {
	return len(b.parts) == 0
}

func (b *openAIContentBuilder) marshal() (json.RawMessage, error) {
	if len(b.parts) == 0 {
		return nil, nil
	}

	if !b.hasNonText {
		var text strings.Builder
		for _, part := range b.parts {
			text.WriteString(derefString(part.Text))
		}
		return json.Marshal(text.String())
	}

	return json.Marshal(b.parts)
}

func newGeminiPendingToolCalls() *geminiPendingToolCalls {
	return &geminiPendingToolCalls{
		byName:       make(map[string][]geminiPendingToolCall),
		byExplicitID: make(map[string]geminiExplicitToolCall),
	}
}

func (p *geminiPendingToolCalls) beginBatch() {
	if p.batch == 0 || p.answered {
		p.batch++
		p.answered = false
	}
}

func (p *geminiPendingToolCalls) add(name, toolCallID, explicitID string) error {
	p.byName[name] = append(p.byName[name], geminiPendingToolCall{
		toolCallID: toolCallID,
		explicitID: explicitID,
		batch:      p.batch,
	})

	if explicitID == "" {
		return nil
	}

	if existing, ok := p.byExplicitID[explicitID]; ok {
		return invalidGeminiArgument("functionCall.id %q is duplicated for %q and %q", explicitID, existing.name, name)
	}

	p.byExplicitID[explicitID] = geminiExplicitToolCall{
		name:       name,
		toolCallID: toolCallID,
		batch:      p.batch,
	}

	return nil
}

func (p *geminiPendingToolCalls) consumeByExplicitID(name, explicitID string) (string, bool, error) {
	if explicitID == "" {
		return "", false, nil
	}

	explicit, ok := p.byExplicitID[explicitID]
	if !ok {
		if p.hasExplicitIDs(name) {
			return "", false, invalidGeminiArgument("functionResponse.id %q does not match any prior functionCall for %q", explicitID, name)
		}
		return "", false, nil
	}

	if explicit.name != name {
		return "", false, invalidGeminiArgument("functionResponse.id %q belongs to functionCall %q, not %q", explicitID, explicit.name, name)
	}
	if err := p.markBatchAnswered(name, explicit.batch); err != nil {
		return "", false, err
	}

	if err := p.removeByName(name, explicitID); err != nil {
		return "", false, err
	}
	delete(p.byExplicitID, explicitID)

	return explicit.toolCallID, true, nil
}

func (p *geminiPendingToolCalls) consumeNextByName(name string) (string, error) {
	pending := p.byName[name]
	if len(pending) == 0 {
		return "", invalidGeminiArgument("functionResponse for %q does not match any prior functionCall", name)
	}

	next := pending[0]
	if err := p.markBatchAnswered(name, next.batch); err != nil {
		return "", err
	}
	if len(pending) == 1 {
		delete(p.byName, name)
	} else {
		p.byName[name] = pending[1:]
	}

	if next.explicitID != "" {
		delete(p.byExplicitID, next.explicitID)
	}

	return next.toolCallID, nil
}

func (p *geminiPendingToolCalls) markBatchAnswered(name string, batch int) error {
	if batch == p.batch {
		p.answered = true
		return nil
	}
	return invalidGeminiArgument("functionResponse for %q answers a functionCall issued before a later functionCall; respond to every functionCall before issuing new ones", name)
}

func (p *geminiPendingToolCalls) hasExplicitIDs(name string) bool {
	for _, pending := range p.byName[name] {
		if pending.explicitID != "" {
			return true
		}
	}

	return false
}

func (p *geminiPendingToolCalls) removeByName(name, explicitID string) error {
	pending := p.byName[name]
	for idx, candidate := range pending {
		if candidate.explicitID != explicitID {
			continue
		}

		if len(pending) == 1 {
			delete(p.byName, name)
			return nil
		}

		p.byName[name] = append(pending[:idx], pending[idx+1:]...)
		return nil
	}

	return invalidGeminiArgument("functionResponse.id %q does not match any prior functionCall for %q", explicitID, name)
}

type geminiProtocolError struct {
	statusCode int
	status     string
	message    string
	cause      error
}

func (e *geminiProtocolError) Error() string {
	return e.message
}

func (e *geminiProtocolError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func invalidGeminiArgument(format string, args ...interface{}) error {
	return &geminiProtocolError{
		statusCode: http.StatusBadRequest,
		status:     "INVALID_ARGUMENT",
		message:    fmt.Sprintf(format, args...),
	}
}

func unsupportedGemini(format string, args ...interface{}) error {
	return &geminiProtocolError{
		statusCode: http.StatusNotImplemented,
		status:     "UNIMPLEMENTED",
		message:    fmt.Sprintf(format, args...),
	}
}

func annotateGeminiProtocolError(err error, format string, args ...interface{}) error {
	prefix := fmt.Sprintf(format, args...)

	var geminiErr *geminiProtocolError
	if errors.As(err, &geminiErr) {
		return &geminiProtocolError{
			statusCode: geminiErr.statusCode,
			status:     geminiErr.status,
			message:    prefix + ": " + geminiErr.message,
		}
	}

	return fmt.Errorf("%s: %w", prefix, err)
}

func normalizeGeminiModelName(model string) string {
	model = strings.TrimSpace(model)
	model = strings.Trim(model, "/")
	for strings.HasPrefix(model, "models/") {
		model = strings.TrimPrefix(model, "models/")
	}
	if alias, ok := geminiModelAliases[model]; ok {
		return alias
	}
	return model
}

func hasRawJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && string(trimmed) != "null"
}

func canonicalizeJSON(raw json.RawMessage) (json.RawMessage, error) {
	if !hasRawJSON(raw) {
		return nil, nil
	}

	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}

	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	return out, nil
}

func canonicalizeJSONDocument(v interface{}) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	var doc interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}

	return json.Marshal(doc)
}

func geminiPartContainsThought(part models.GeminiPart) bool {
	return part.Thought != nil && *part.Thought
}

func geminiPartHasOnlyThoughtMetadata(part models.GeminiPart) bool {
	return strings.TrimSpace(part.ThoughtSignature) != "" &&
		part.Text == nil &&
		part.FunctionCall == nil &&
		part.FunctionResponse == nil &&
		!hasRawJSON(part.InlineData) &&
		!hasRawJSON(part.FileData) &&
		!hasRawJSON(part.ExecutableCode) &&
		!hasRawJSON(part.CodeExecutionResult)
}

func shouldSkipGeminiPart(part models.GeminiPart) bool {
	return geminiPartContainsThought(part) || geminiPartHasOnlyThoughtMetadata(part)
}

func hashOpenAIRequest(req *models.OpenAIRequest) (string, error) {
	doc, err := canonicalizeJSONDocument(req)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:]), nil
}

func parseGeminiPath(path string) (string, string, error) {
	prefixes := []string{"/v1beta/models/", "/v1/models/", "/models/"}
	for _, prefix := range prefixes {
		if !strings.HasPrefix(path, prefix) {
			continue
		}

		remainder := strings.TrimPrefix(path, prefix)
		if remainder == "" {
			return "", "", invalidGeminiArgument("missing Gemini model and action in path")
		}

		idx := strings.LastIndex(remainder, ":")
		if idx <= 0 || idx == len(remainder)-1 {
			return "", "", invalidGeminiArgument("Gemini routes must use /models/{model}:action")
		}

		return remainder[:idx], remainder[idx+1:], nil
	}

	return "", "", invalidGeminiArgument("unsupported Gemini route")
}

type geminiTranslationOptions struct {
	unwrapCLITextOutput bool
	imageDetail         string
}

func TranslateGeminiToOpenAI(req *models.GeminiGenerateContentRequest, pathModel string, stream bool) (*models.OpenAIRequest, error) {
	return translateGeminiToOpenAIWithOptions(req, pathModel, stream, geminiTranslationOptions{})
}

func translateGeminiToOpenAIWithOptions(req *models.GeminiGenerateContentRequest, pathModel string, stream bool, options geminiTranslationOptions) (*models.OpenAIRequest, error) {
	if req == nil {
		return nil, invalidGeminiArgument("request body is required")
	}

	model := normalizeGeminiModelName(pathModel)
	if model == "" {
		return nil, invalidGeminiArgument("Gemini model path is required")
	}

	if req.Model != "" {
		bodyModel := normalizeGeminiModelName(req.Model)
		if bodyModel != model {
			return nil, invalidGeminiArgument("body model %q does not match path model %q", req.Model, pathModel)
		}
	}

	if req.CachedContent != "" {
		return nil, unsupportedGemini("cachedContent is not supported")
	}
	genConfig, err := validateGeminiGenerationConfig(req.GenerationConfig)
	if err != nil {
		return nil, err
	}
	options.imageDetail = genConfig.imageDetail

	oaiReq := &models.OpenAIRequest{Model: model}

	if req.SystemInstruction != nil {
		systemText, err := translateGeminiSystemInstruction(req.SystemInstruction)
		if err != nil {
			return nil, err
		}
		if systemText != "" {
			content, _ := json.Marshal(systemText)
			oaiReq.Messages = append(oaiReq.Messages, models.OpenAIMessage{
				Role:    "system",
				Content: content,
			})
		}
	}

	translatedMessages, err := translateGeminiContents(req.Contents, options)
	if err != nil {
		return nil, err
	}
	oaiReq.Messages = append(oaiReq.Messages, translatedMessages...)
	if len(oaiReq.Messages) == 0 {
		return nil, invalidGeminiArgument("request must include systemInstruction or contents")
	}

	tools, declaredNames, err := translateGeminiTools(req.Tools)
	if err != nil {
		return nil, err
	}
	oaiReq.Tools = tools

	if req.ToolConfig != nil && req.ToolConfig.FunctionCallingConfig != nil {
		if len(oaiReq.Tools) == 0 {
			return nil, invalidGeminiArgument("toolConfig.functionCallingConfig requires tools.functionDeclarations")
		}

		toolChoice, err := translateGeminiToolChoice(req.ToolConfig.FunctionCallingConfig, declaredNames)
		if err != nil {
			return nil, err
		}
		oaiReq.ToolChoice = toolChoice
		oaiReq.Tools = filterGeminiAllowedTools(oaiReq.Tools, req.ToolConfig.FunctionCallingConfig)
	}

	if len(oaiReq.Tools) > 0 && !openAIToolChoiceIsNone(oaiReq.ToolChoice) {
		parallelToolCalls := true
		oaiReq.ParallelToolCalls = &parallelToolCalls
	}

	if req.GenerationConfig != nil {
		oaiReq.Temperature = req.GenerationConfig.Temperature
		oaiReq.TopP = req.GenerationConfig.TopP
		oaiReq.MaxTokens = req.GenerationConfig.MaxOutputTokens
		oaiReq.PresencePenalty = req.GenerationConfig.PresencePenalty
		oaiReq.FrequencyPenalty = req.GenerationConfig.FrequencyPenalty
		oaiReq.Seed = req.GenerationConfig.Seed
		oaiReq.ReasoningEffort = genConfig.reasoningEffort

		if len(req.GenerationConfig.StopSequences) > 0 {
			stop, err := json.Marshal(req.GenerationConfig.StopSequences)
			if err != nil {
				return nil, invalidGeminiArgument("invalid generationConfig.stopSequences")
			}
			oaiReq.Stop = stop
		}

		responseFormat, err := translateGeminiResponseFormat(req.GenerationConfig)
		if err != nil {
			return nil, err
		}
		oaiReq.ResponseFormat = responseFormat
	}

	if stream {
		streamFlag := true
		oaiReq.Stream = &streamFlag
		oaiReq.StreamOptions = &models.StreamOptions{IncludeUsage: true}
	}

	return oaiReq, nil
}

func TranslateGeminiCountTokens(req *models.GeminiCountTokensRequest, pathModel string) (*models.OpenAIRequest, error) {
	return TranslateGeminiToOpenAI(req, pathModel, false)
}

func translateGeminiSystemInstruction(systemInstruction *models.GeminiContent) (string, error) {
	if systemInstruction == nil {
		return "", nil
	}

	var textParts []string
	for partIdx, part := range systemInstruction.Parts {
		if shouldSkipGeminiPart(part) {
			continue
		}

		kind, err := geminiPartKind(part)
		if err != nil {
			return "", annotateGeminiProtocolError(err, "systemInstruction.parts[%d]", partIdx)
		}
		if kind != "text" {
			return "", invalidGeminiArgument("systemInstruction.parts[%d] must be text", partIdx)
		}
		textParts = append(textParts, derefString(part.Text))
	}

	return strings.Join(textParts, "\n"), nil
}

func translateGeminiContents(contents []models.GeminiContent, options geminiTranslationOptions) ([]models.OpenAIMessage, error) {
	pendingToolCalls := newGeminiPendingToolCalls()
	var messages []models.OpenAIMessage
	var pendingToolImages []models.OpenAIMessage
	toolResultIndex := 0
	flushPendingToolImages := func() {
		if len(pendingToolImages) == 0 {
			return
		}
		messages = append(messages, pendingToolImages...)
		pendingToolImages = nil
	}

	for contentIdx, content := range contents {
		if len(content.Parts) == 0 {
			return nil, invalidGeminiArgument("contents[%d].parts must not be empty", contentIdx)
		}

		role := content.Role
		if role == "" {
			role = "user"
		}

		switch role {
		case "user":
			var contentBuilder openAIContentBuilder
			flushUserContent := func() error {
				if contentBuilder.empty() {
					return nil
				}

				contentJSON, err := contentBuilder.marshal()
				if err != nil {
					return err
				}

				flushPendingToolImages()
				messages = append(messages, models.OpenAIMessage{
					Role:    "user",
					Content: contentJSON,
				})
				contentBuilder = openAIContentBuilder{}
				return nil
			}

			for partIdx, part := range content.Parts {
				if shouldSkipGeminiPart(part) {
					continue
				}

				kind, err := geminiPartKind(part)
				if err != nil {
					return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d]", contentIdx, partIdx)
				}

				switch kind {
				case "text":
					contentBuilder.appendText(derefString(part.Text))
				case "inlineData":
					inlineData, err := parseGeminiInlineDataHeader(part.InlineData)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].inlineData", contentIdx, partIdx)
					}
					if !isGeminiImageMimeType(inlineData.MimeType) {
						contentBuilder.appendText(formatOmittedGeminiInlineData(inlineData, false))
						continue
					}
					imagePart, err := buildGeminiImagePart(inlineData, options.imageDetail)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].inlineData", contentIdx, partIdx)
					}
					contentBuilder.append(imagePart)
				case "fileData":
					fileData, err := parseGeminiFileData(part.FileData)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].fileData", contentIdx, partIdx)
					}
					if imagePart, ok := buildGeminiFileImagePart(fileData, options.imageDetail); ok {
						contentBuilder.append(imagePart)
					} else {
						contentBuilder.appendText(formatOmittedGeminiFileData(fileData, false))
					}
				case "executableCode":
					formatted, err := formatGeminiExecutableCode(part.ExecutableCode)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].executableCode", contentIdx, partIdx)
					}
					contentBuilder.appendText(formatted)
				case "codeExecutionResult":
					formatted, err := formatGeminiCodeExecutionResult(part.CodeExecutionResult)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].codeExecutionResult", contentIdx, partIdx)
					}
					contentBuilder.appendText(formatted)
				case "functionResponse":
					if err := flushUserContent(); err != nil {
						return nil, err
					}

					toolResultIndex++
					message, imageMessage, err := translateGeminiFunctionResponse(part.FunctionResponse, pendingToolCalls, options, toolResultIndex)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].functionResponse", contentIdx, partIdx)
					}
					messages = append(messages, message)
					if imageMessage != nil {
						pendingToolImages = append(pendingToolImages, *imageMessage)
					}
				case "functionCall":
					return nil, invalidGeminiArgument("contents[%d].parts[%d].functionCall requires role \"model\"", contentIdx, partIdx)
				}
			}

			if err := flushUserContent(); err != nil {
				return nil, err
			}

		case "model":
			var textParts []string
			var toolCalls []models.OpenAIToolCall
			flushAssistantContent := func() error {
				if len(textParts) == 0 && len(toolCalls) == 0 {
					return nil
				}

				message := models.OpenAIMessage{Role: "assistant"}
				if len(textParts) > 0 {
					contentJSON, err := json.Marshal(strings.Join(textParts, ""))
					if err != nil {
						return err
					}
					message.Content = contentJSON
				}
				if len(toolCalls) > 0 {
					message.ToolCalls = toolCalls
				}
				flushPendingToolImages()
				messages = append(messages, message)
				textParts = nil
				toolCalls = nil
				return nil
			}

			for partIdx, part := range content.Parts {
				if shouldSkipGeminiPart(part) {
					continue
				}

				kind, err := geminiPartKind(part)
				if err != nil {
					return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d]", contentIdx, partIdx)
				}

				switch kind {
				case "text":
					textParts = append(textParts, derefString(part.Text))
				case "functionCall":
					toolCall, explicitID, err := translateGeminiFunctionCall(part.FunctionCall, contentIdx, partIdx)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].functionCall", contentIdx, partIdx)
					}
					if len(toolCalls) == 0 {
						pendingToolCalls.beginBatch()
					}
					toolCalls = append(toolCalls, toolCall)
					if err := pendingToolCalls.add(toolCall.Function.Name, toolCall.ID, explicitID); err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].functionCall", contentIdx, partIdx)
					}
				case "functionResponse":
					if err := flushAssistantContent(); err != nil {
						return nil, err
					}

					toolResultIndex++
					message, imageMessage, err := translateGeminiFunctionResponse(part.FunctionResponse, pendingToolCalls, options, toolResultIndex)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].functionResponse", contentIdx, partIdx)
					}
					messages = append(messages, message)
					if imageMessage != nil {
						pendingToolImages = append(pendingToolImages, *imageMessage)
					}
				case "inlineData":
					return nil, invalidGeminiArgument("contents[%d].parts[%d].inlineData requires role \"user\"", contentIdx, partIdx)
				case "fileData":
					return nil, invalidGeminiArgument("contents[%d].parts[%d].fileData requires role \"user\"", contentIdx, partIdx)
				case "executableCode":
					formatted, err := formatGeminiExecutableCode(part.ExecutableCode)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].executableCode", contentIdx, partIdx)
					}
					textParts = append(textParts, formatted)
				case "codeExecutionResult":
					formatted, err := formatGeminiCodeExecutionResult(part.CodeExecutionResult)
					if err != nil {
						return nil, annotateGeminiProtocolError(err, "contents[%d].parts[%d].codeExecutionResult", contentIdx, partIdx)
					}
					textParts = append(textParts, formatted)
				}
			}

			if err := flushAssistantContent(); err != nil {
				return nil, err
			}

		default:
			return nil, invalidGeminiArgument("contents[%d].role must be \"user\" or \"model\"", contentIdx)
		}
	}

	flushPendingToolImages()
	return groupGeminiToolResults(messages), nil
}

func groupGeminiToolResults(messages []models.OpenAIMessage) []models.OpenAIMessage {
	grouped := make([]models.OpenAIMessage, 0, len(messages))
	moved := make([]bool, len(messages))

	for i, message := range messages {
		if moved[i] {
			continue
		}
		grouped = append(grouped, message)

		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			continue
		}

		outstanding := make(map[string]struct{}, len(message.ToolCalls))
		for _, toolCall := range message.ToolCalls {
			outstanding[toolCall.ID] = struct{}{}
		}

		for j := i + 1; j < len(messages) && len(outstanding) > 0; j++ {
			candidate := messages[j]
			if candidate.Role == "assistant" && len(candidate.ToolCalls) > 0 {
				break
			}
			if candidate.Role != "tool" {
				continue
			}
			if _, ok := outstanding[candidate.ToolCallID]; !ok {
				break
			}
			grouped = append(grouped, candidate)
			moved[j] = true
			delete(outstanding, candidate.ToolCallID)
		}
	}

	return grouped
}

func translateGeminiFunctionCall(functionCall *models.GeminiFunctionCall, contentIdx, partIdx int) (models.OpenAIToolCall, string, error) {
	if functionCall == nil || strings.TrimSpace(functionCall.Name) == "" {
		return models.OpenAIToolCall{}, "", invalidGeminiArgument("functionCall.name is required")
	}

	args, err := normalizeGeminiArguments(functionCall.Args)
	if err != nil {
		return models.OpenAIToolCall{}, "", invalidGeminiArgument("functionCall.args must be valid JSON")
	}

	explicitID := strings.TrimSpace(functionCall.ID)
	toolCallID := explicitID
	if toolCallID == "" {
		toolCallID = fmt.Sprintf("%s-%d-%d", geminiFunctionCallIDPrefix, contentIdx, partIdx)
	}

	return models.OpenAIToolCall{
		ID:   toolCallID,
		Type: "function",
		Function: models.OpenAIFunctionCall{
			Name:      functionCall.Name,
			Arguments: args,
		},
	}, explicitID, nil
}

func translateGeminiFunctionResponse(functionResponse *models.GeminiFunctionResponse, pendingToolCalls *geminiPendingToolCalls, options geminiTranslationOptions, resultIndex int) (models.OpenAIMessage, *models.OpenAIMessage, error) {
	if functionResponse == nil || strings.TrimSpace(functionResponse.Name) == "" {
		return models.OpenAIMessage{}, nil, invalidGeminiArgument("functionResponse.name is required")
	}

	responseID := strings.TrimSpace(functionResponse.ID)
	toolCallID, matched, err := pendingToolCalls.consumeByExplicitID(functionResponse.Name, responseID)
	if err != nil {
		return models.OpenAIMessage{}, nil, err
	}
	if !matched {
		toolCallID, err = pendingToolCalls.consumeNextByName(functionResponse.Name)
		if err != nil {
			return models.OpenAIMessage{}, nil, err
		}
	}

	partTexts, imageParts, err := translateGeminiFunctionResponseParts(functionResponse.Parts, resultIndex, options.imageDetail)
	if err != nil {
		return models.OpenAIMessage{}, nil, err
	}

	var segments []string
	if hasRawJSON(functionResponse.Response) || len(partTexts) == 0 {
		responseText, err := normalizeGeminiFunctionResponse(functionResponse.Response, options.unwrapCLITextOutput)
		if err != nil {
			return models.OpenAIMessage{}, nil, invalidGeminiArgument("functionResponse.response must be valid JSON")
		}
		segments = append(segments, responseText)
	}
	segments = append(segments, partTexts...)

	contentJSON, err := json.Marshal(strings.Join(segments, "\n"))
	if err != nil {
		return models.OpenAIMessage{}, nil, err
	}

	var imageMessage *models.OpenAIMessage
	if len(imageParts) > 0 {
		imageJSON, err := json.Marshal(imageParts)
		if err != nil {
			return models.OpenAIMessage{}, nil, err
		}
		imageMessage = &models.OpenAIMessage{
			Role:    "user",
			Content: imageJSON,
		}
	}

	return models.OpenAIMessage{
		Role:       "tool",
		ToolCallID: toolCallID,
		Content:    contentJSON,
	}, imageMessage, nil
}

func translateGeminiFunctionResponseParts(parts []models.GeminiFunctionResponsePart, resultIndex int, imageDetail string) ([]string, []models.OpenAIContentPart, error) {
	if len(parts) == 0 {
		return nil, nil, nil
	}

	var textSegments []string
	var images []models.OpenAIContentPart
	imageIndex := 0

	appendToolImage := func(imagePart models.OpenAIContentPart) {
		imageIndex++
		label := fmt.Sprintf("Tool result %d image %d", resultIndex, imageIndex)
		textSegments = append(textSegments, fmt.Sprintf("[%s]", label))
		preamble := label + " (tool results numbered in conversation order; attachment from tool output):"
		images = append(images, models.OpenAIContentPart{Type: "text", Text: &preamble}, imagePart)
	}

	for partIdx, part := range parts {
		hasFileData := hasRawJSON(part.FileData)
		hasInlineData := hasRawJSON(part.InlineData)

		count := 0
		if part.Text != nil {
			count++
		}
		if hasInlineData {
			count++
		}
		if hasFileData {
			count++
		}

		switch count {
		case 0:
			return nil, nil, invalidGeminiArgument("parts[%d] must contain text, inlineData, or fileData", partIdx)
		case 1:
		default:
			return nil, nil, invalidGeminiArgument("parts[%d] must contain exactly one supported field", partIdx)
		}

		if hasFileData {
			fileData, err := parseGeminiFileData(part.FileData)
			if err != nil {
				return nil, nil, annotateGeminiProtocolError(err, "parts[%d].fileData", partIdx)
			}
			if imagePart, ok := buildGeminiFileImagePart(fileData, imageDetail); ok {
				appendToolImage(imagePart)
			} else {
				textSegments = append(textSegments, formatOmittedGeminiFileData(fileData, true))
			}
			continue
		}

		if part.Text != nil {
			textSegments = append(textSegments, *part.Text)
			continue
		}

		inlineData, err := parseGeminiInlineDataHeader(part.InlineData)
		if err != nil {
			return nil, nil, annotateGeminiProtocolError(err, "parts[%d].inlineData", partIdx)
		}
		if !isGeminiImageMimeType(inlineData.MimeType) {
			textSegments = append(textSegments, formatOmittedGeminiInlineData(inlineData, true))
			continue
		}
		imagePart, err := buildGeminiImagePart(inlineData, imageDetail)
		if err != nil {
			return nil, nil, annotateGeminiProtocolError(err, "parts[%d].inlineData", partIdx)
		}
		appendToolImage(imagePart)
	}

	return textSegments, images, nil
}

func translateGeminiTools(tools []models.GeminiTool) ([]models.OpenAITool, map[string]struct{}, error) {
	var translated []models.OpenAITool
	declaredNames := make(map[string]struct{})

	for toolIdx, tool := range tools {
		if hasRawJSON(tool.GoogleSearch) {
			return nil, nil, unsupportedGemini("tools[%d].googleSearch is not supported; Gemini native web tools cannot be translated to Copilot function calls", toolIdx)
		}
		if hasRawJSON(tool.GoogleSearchRetrieval) {
			return nil, nil, unsupportedGemini("tools[%d].googleSearchRetrieval is not supported; Gemini native web tools cannot be translated to Copilot function calls", toolIdx)
		}
		if hasRawJSON(tool.URLContext) {
			return nil, nil, unsupportedGemini("tools[%d].urlContext is not supported; Gemini native web tools cannot be translated to Copilot function calls", toolIdx)
		}
		if hasRawJSON(tool.CodeExecution) {
			return nil, nil, unsupportedGemini("tools[%d].codeExecution is not supported; Gemini native execution tools cannot be translated to Copilot function calls", toolIdx)
		}
		if hasRawJSON(tool.GoogleMaps) {
			return nil, nil, unsupportedGemini("tools[%d].googleMaps is not supported; Gemini native map tools cannot be translated to Copilot function calls", toolIdx)
		}
		if hasRawJSON(tool.ComputerUse) {
			return nil, nil, unsupportedGemini("tools[%d].computerUse is not supported; Gemini native computer-use tools cannot be translated to Copilot function calls", toolIdx)
		}
		if hasRawJSON(tool.EnterpriseWebSearch) {
			return nil, nil, unsupportedGemini("tools[%d].enterpriseWebSearch is not supported; Gemini native enterprise web search tools cannot be translated to Copilot function calls", toolIdx)
		}

		for declIdx, decl := range tool.FunctionDeclarations {
			if strings.TrimSpace(decl.Name) == "" {
				return nil, nil, invalidGeminiArgument("tools[%d].functionDeclarations[%d].name is required", toolIdx, declIdx)
			}

			schemaField := "parameters"
			schema := decl.Parameters
			if !hasRawJSON(schema) && hasRawJSON(decl.ParametersJSONSchema) {
				schemaField = "parametersJsonSchema"
				schema = decl.ParametersJSONSchema
			}

			parameters, err := canonicalizeJSON(schema)
			if err == nil && schemaField == "parameters" {
				parameters, err = geminiSchemaToJSONSchema(parameters)
			}
			if err != nil {
				return nil, nil, invalidGeminiArgument("tools[%d].functionDeclarations[%d].%s must be valid JSON schema", toolIdx, declIdx, schemaField)
			}

			translated = append(translated, models.OpenAITool{
				Type: "function",
				Function: models.OpenAIFunction{
					Name:        decl.Name,
					Description: decl.Description,
					Parameters:  parameters,
				},
			})
			declaredNames[decl.Name] = struct{}{}
		}
	}

	return translated, declaredNames, nil
}

var geminiSchemaTypes = map[string]string{
	"STRING":  "string",
	"NUMBER":  "number",
	"INTEGER": "integer",
	"BOOLEAN": "boolean",
	"ARRAY":   "array",
	"OBJECT":  "object",
	"NULL":    "null",
}

var geminiSchemaInt64Keywords = []string{"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties"}

func geminiSchemaToJSONSchema(raw json.RawMessage) (json.RawMessage, error) {
	if !hasRawJSON(raw) {
		return raw, nil
	}
	var schema interface{}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	if !convertGeminiSchemaNode(schema) {
		return raw, nil
	}
	return json.Marshal(schema)
}

func convertGeminiSchemaNode(node interface{}) bool {
	schema, ok := node.(map[string]interface{})
	if !ok {
		return false
	}
	changed := false
	switch typ := schema["type"].(type) {
	case string:
		if lowered, ok := geminiSchemaTypes[typ]; ok {
			schema["type"] = lowered
			changed = true
		}
	case []interface{}:
		for i, entry := range typ {
			if name, ok := entry.(string); ok {
				if lowered, ok := geminiSchemaTypes[name]; ok {
					typ[i] = lowered
					changed = true
				}
			}
		}
	}
	if nullable, ok := schema["nullable"].(bool); ok {
		delete(schema, "nullable")
		changed = true
		if nullable {
			applyGeminiSchemaNullable(schema)
		}
	}
	for _, key := range geminiSchemaInt64Keywords {
		if text, ok := schema[key].(string); ok {
			if value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
				schema[key] = value
				changed = true
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties", "not"} {
		if child, ok := schema[key]; ok {
			changed = convertGeminiSchemaNode(child) || changed
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if children, ok := schema[key].([]interface{}); ok {
			for _, child := range children {
				changed = convertGeminiSchemaNode(child) || changed
			}
		}
	}
	for _, key := range []string{"properties", "$defs", "definitions"} {
		if children, ok := schema[key].(map[string]interface{}); ok {
			for _, child := range children {
				changed = convertGeminiSchemaNode(child) || changed
			}
		}
	}
	return changed
}

func applyGeminiSchemaNullable(schema map[string]interface{}) {
	switch typ := schema["type"].(type) {
	case string:
		if typ != "null" {
			schema["type"] = []interface{}{typ, "null"}
		}
	case []interface{}:
		if !slices.Contains(typ, interface{}("null")) {
			schema["type"] = append(typ, "null")
		}
	default:
		if anyOf, ok := schema["anyOf"].([]interface{}); ok {
			schema["anyOf"] = append(anyOf, map[string]interface{}{"type": "null"})
		}
	}
	if enum, ok := schema["enum"].([]interface{}); ok && !slices.Contains(enum, nil) {
		schema["enum"] = append(enum, nil)
	}
}

func filterGeminiAllowedTools(tools []models.OpenAITool, config *models.GeminiFunctionCallingConfig) []models.OpenAITool {
	if config == nil || len(config.AllowedFunctionNames) == 0 {
		return tools
	}

	mode := strings.ToUpper(strings.TrimSpace(config.Mode))
	if mode == "NONE" {
		return tools
	}

	allowed := make(map[string]struct{}, len(config.AllowedFunctionNames))
	for _, name := range config.AllowedFunctionNames {
		allowed[name] = struct{}{}
	}

	filtered := make([]models.OpenAITool, 0, len(config.AllowedFunctionNames))
	for _, tool := range tools {
		if tool.Type != "function" {
			filtered = append(filtered, tool)
			continue
		}
		if _, ok := allowed[tool.Function.Name]; ok {
			filtered = append(filtered, tool)
		}
	}

	return filtered
}

func translateGeminiToolChoice(config *models.GeminiFunctionCallingConfig, declaredNames map[string]struct{}) (json.RawMessage, error) {
	if config == nil {
		return nil, nil
	}

	for idx, name := range config.AllowedFunctionNames {
		if _, ok := declaredNames[name]; !ok {
			return nil, invalidGeminiArgument("toolConfig.functionCallingConfig.allowedFunctionNames[%d]=%q is not declared in tools.functionDeclarations", idx, name)
		}
	}

	mode := strings.ToUpper(strings.TrimSpace(config.Mode))
	if len(config.AllowedFunctionNames) == 1 && mode != "NONE" && mode != "VALIDATED" {
		return json.Marshal(map[string]interface{}{
			"type": "function",
			"function": map[string]string{
				"name": config.AllowedFunctionNames[0],
			},
		})
	}

	switch mode {
	case "", "AUTO", "VALIDATED":
		return json.Marshal("auto")
	case "NONE":
		return json.Marshal("none")
	case "ANY":
		return json.Marshal("required")
	default:
		return nil, invalidGeminiArgument("toolConfig.functionCallingConfig.mode %q is invalid", config.Mode)
	}
}

func translateGeminiResponseFormat(config *models.GeminiGenerationConfig) (json.RawMessage, error) {
	if config == nil {
		return nil, nil
	}

	responseSchema, err := resolveGeminiResponseSchema(config)
	if err != nil {
		return nil, err
	}

	responseMimeType := strings.TrimSpace(config.ResponseMimeType)
	switch {
	case hasRawJSON(responseSchema):
		if responseMimeType != "" && responseMimeType != "application/json" {
			return nil, unsupportedGemini("generationConfig.responseMimeType %q is not supported", config.ResponseMimeType)
		}

		schema, err := canonicalizeJSON(responseSchema)
		if err != nil {
			return nil, invalidGeminiArgument("generationConfig.responseSchema must be valid JSON")
		}

		return json.Marshal(map[string]interface{}{
			"type": "json_schema",
			"json_schema": map[string]interface{}{
				"name":   "response",
				"schema": schema,
			},
		})

	case responseMimeType == "", responseMimeType == "text/plain":
		return nil, nil
	case responseMimeType == "application/json":
		return json.Marshal(map[string]string{"type": "json_object"})
	default:
		return nil, unsupportedGemini("generationConfig.responseMimeType %q is not supported", config.ResponseMimeType)
	}
}

func resolveGeminiResponseSchema(config *models.GeminiGenerationConfig) (json.RawMessage, error) {
	if config == nil {
		return nil, nil
	}

	switch {
	case !hasRawJSON(config.ResponseSchema):
		return config.ResponseJSONSchema, nil
	case !hasRawJSON(config.ResponseJSONSchema):
		return config.ResponseSchema, nil
	}

	left, err := canonicalizeJSON(config.ResponseSchema)
	if err != nil {
		return nil, invalidGeminiArgument("generationConfig.responseSchema must be valid JSON")
	}
	right, err := canonicalizeJSON(config.ResponseJSONSchema)
	if err != nil {
		return nil, invalidGeminiArgument("generationConfig.responseJsonSchema must be valid JSON")
	}
	if !bytes.Equal(left, right) {
		return nil, invalidGeminiArgument("generationConfig.responseSchema and generationConfig.responseJsonSchema cannot differ")
	}

	return left, nil
}

type validatedGeminiGenerationConfig struct {
	imageDetail     string
	reasoningEffort string
}

func validateGeminiGenerationConfig(config *models.GeminiGenerationConfig) (validatedGeminiGenerationConfig, error) {
	if config == nil {
		return validatedGeminiGenerationConfig{}, nil
	}

	if config.CandidateCount != nil && *config.CandidateCount != 1 {
		return validatedGeminiGenerationConfig{}, unsupportedGemini("generationConfig.candidateCount=%d is not supported", *config.CandidateCount)
	}
	reasoningEffort, err := translateGeminiThinkingConfig(config.ThinkingConfig)
	if err != nil {
		return validatedGeminiGenerationConfig{}, err
	}
	if err := validateGeminiResponseModalities(config.ResponseModalities); err != nil {
		return validatedGeminiGenerationConfig{}, err
	}
	if hasRawJSON(config.SpeechConfig) {
		return validatedGeminiGenerationConfig{}, unsupportedGemini("generationConfig.speechConfig is not supported; Gemini speech configuration cannot be translated today")
	}
	if hasRawJSON(config.ImageConfig) {
		return validatedGeminiGenerationConfig{}, unsupportedGemini("generationConfig.imageConfig is not supported; Gemini image generation configuration cannot be translated today")
	}
	imageDetail, err := translateGeminiMediaResolution(config.MediaResolution)
	if err != nil {
		return validatedGeminiGenerationConfig{}, err
	}
	if err := validateGeminiResponseLogprobs(config.ResponseLogprobs); err != nil {
		return validatedGeminiGenerationConfig{}, err
	}
	if hasRawJSON(config.Logprobs) {
		return validatedGeminiGenerationConfig{}, unsupportedGemini("generationConfig.logprobs is not supported")
	}

	return validatedGeminiGenerationConfig{
		imageDetail:     imageDetail,
		reasoningEffort: reasoningEffort,
	}, nil
}

func validateGeminiResponseModalities(raw json.RawMessage) error {
	if !hasRawJSON(raw) {
		return nil
	}
	var modalities []string
	if err := json.Unmarshal(raw, &modalities); err != nil {
		return invalidGeminiArgument("generationConfig.responseModalities must be an array of strings")
	}
	for _, modality := range modalities {
		switch strings.ToUpper(strings.TrimSpace(modality)) {
		case "TEXT", "MODALITY_UNSPECIFIED":
		default:
			return unsupportedGemini("generationConfig.responseModalities is not supported; Gemini audio/image response modes cannot be translated today")
		}
	}
	return nil
}

func validateGeminiResponseLogprobs(raw json.RawMessage) error {
	if !hasRawJSON(raw) {
		return nil
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return invalidGeminiArgument("generationConfig.responseLogprobs must be a boolean")
	}
	if enabled {
		return unsupportedGemini("generationConfig.responseLogprobs is not supported")
	}
	return nil
}

func translateGeminiMediaResolution(raw json.RawMessage) (string, error) {
	if !hasRawJSON(raw) {
		return "", nil
	}
	var resolution string
	if err := json.Unmarshal(raw, &resolution); err != nil {
		return "", invalidGeminiArgument("generationConfig.mediaResolution must be a string")
	}
	switch strings.ToUpper(strings.TrimSpace(resolution)) {
	case "", "MEDIA_RESOLUTION_UNSPECIFIED":
		return "", nil
	case "MEDIA_RESOLUTION_LOW":
		return "low", nil
	case "MEDIA_RESOLUTION_MEDIUM":
		return "auto", nil
	case "MEDIA_RESOLUTION_HIGH":
		return "high", nil
	default:
		return "", invalidGeminiArgument("generationConfig.mediaResolution %q is invalid", resolution)
	}
}

type geminiThinkingConfig struct {
	IncludeThoughts *bool   `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int    `json:"thinkingBudget,omitempty"`
	ThinkingLevel   *string `json:"thinkingLevel,omitempty"`
}

func translateGeminiThinkingConfig(raw json.RawMessage) (string, error) {
	if !hasRawJSON(raw) {
		return "", nil
	}

	normalized, err := normalizeGeminiJSON(raw, geminiThinkingConfigNode, "generationConfig.thinkingConfig", true)
	if err != nil {
		return "", err
	}

	var config geminiThinkingConfig
	if err := json.Unmarshal(normalized, &config); err != nil {
		return "", invalidGeminiArgument("generationConfig.thinkingConfig has invalid field types")
	}

	if config.ThinkingLevel != nil {
		level := strings.ToLower(strings.TrimSpace(*config.ThinkingLevel))
		switch level {
		case "", "thinking_level_unspecified":
			return "", nil
		case "minimal":
			return "low", nil
		case "low", "medium", "high":
			return level, nil
		default:
			return "", invalidGeminiArgument("generationConfig.thinkingConfig.thinkingLevel %q is invalid", *config.ThinkingLevel)
		}
	}

	return "", nil
}

func geminiPartKind(part models.GeminiPart) (string, error) {
	count := 0
	kind := ""
	if part.Text != nil {
		count++
		kind = "text"
	}
	if part.FunctionCall != nil {
		count++
		kind = "functionCall"
	}
	if part.FunctionResponse != nil {
		count++
		kind = "functionResponse"
	}
	if hasRawJSON(part.InlineData) {
		count++
		kind = "inlineData"
	}
	if hasRawJSON(part.FileData) {
		count++
		kind = "fileData"
	}
	if hasRawJSON(part.ExecutableCode) {
		count++
		kind = "executableCode"
	}
	if hasRawJSON(part.CodeExecutionResult) {
		count++
		kind = "codeExecutionResult"
	}

	switch count {
	case 1:
		return kind, nil
	case 0:
		return "", invalidGeminiArgument("content part must contain text, functionCall, functionResponse, inlineData, fileData, executableCode, or codeExecutionResult")
	default:
		return "", invalidGeminiArgument("content part must contain exactly one supported field")
	}
}

func buildGeminiImagePart(inlineData *models.GeminiInlineData, detail string) (models.OpenAIContentPart, error) {
	normalized, err := normalizeGeminiInlineDataBase64(inlineData.Data)
	if err != nil {
		return models.OpenAIContentPart{}, invalidGeminiArgument("inlineData.data must be valid base64")
	}

	return models.OpenAIContentPart{
		Type: "image_url",
		ImageURL: &models.OpenAIImageURL{
			URL:    fmt.Sprintf("data:%s;base64,%s", inlineData.MimeType, normalized),
			Detail: detail,
		},
	}, nil
}

func parseGeminiInlineDataHeader(raw json.RawMessage) (*models.GeminiInlineData, error) {
	if !hasRawJSON(raw) {
		return nil, invalidGeminiArgument("inlineData must be an object with mimeType and data")
	}

	var inlineData models.GeminiInlineData
	if err := json.Unmarshal(raw, &inlineData); err != nil {
		return nil, invalidGeminiArgument("inlineData must be an object with mimeType and data")
	}

	inlineData.MimeType = strings.TrimSpace(inlineData.MimeType)
	inlineData.Data = strings.TrimSpace(inlineData.Data)
	inlineData.DisplayName = strings.TrimSpace(inlineData.DisplayName)

	if inlineData.MimeType == "" {
		return nil, invalidGeminiArgument("inlineData.mimeType is required")
	}
	if inlineData.Data == "" {
		return nil, invalidGeminiArgument("inlineData.data is required")
	}

	return &inlineData, nil
}

func parseGeminiFileData(raw json.RawMessage) (*models.GeminiFileData, error) {
	if !hasRawJSON(raw) {
		return nil, invalidGeminiArgument("fileData must be an object with fileUri")
	}

	var fileData models.GeminiFileData
	if err := json.Unmarshal(raw, &fileData); err != nil {
		return nil, invalidGeminiArgument("fileData must be an object with fileUri")
	}

	fileData.MimeType = strings.TrimSpace(fileData.MimeType)
	fileData.FileURI = strings.TrimSpace(fileData.FileURI)
	fileData.DisplayName = strings.TrimSpace(fileData.DisplayName)
	if fileData.FileURI == "" {
		return nil, invalidGeminiArgument("fileData.fileUri is required")
	}

	return &fileData, nil
}

func buildGeminiFileImagePart(fileData *models.GeminiFileData, detail string) (models.OpenAIContentPart, bool) {
	if !isGeminiImageMimeType(fileData.MimeType) || !isHTTPURL(fileData.FileURI) {
		return models.OpenAIContentPart{}, false
	}
	return models.OpenAIContentPart{
		Type: "image_url",
		ImageURL: &models.OpenAIImageURL{
			URL:    fileData.FileURI,
			Detail: detail,
		},
	}, true
}

func isHTTPURL(raw string) bool {
	lower := strings.ToLower(raw)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

func isGeminiImageMimeType(mimeType string) bool {
	return strings.HasPrefix(strings.ToLower(mimeType), "image/")
}

func formatOmittedGeminiInlineData(inlineData *models.GeminiInlineData, fromTool bool) string {
	suffix := ""
	if fromTool {
		suffix = " from tool output"
	}
	if inlineData.DisplayName != "" {
		return fmt.Sprintf("[omitted unsupported %s attachment %q%s]", inlineData.MimeType, inlineData.DisplayName, suffix)
	}
	return fmt.Sprintf("[omitted unsupported %s attachment%s]", inlineData.MimeType, suffix)
}

func formatOmittedGeminiFileData(fileData *models.GeminiFileData, fromTool bool) string {
	suffix := ""
	if fromTool {
		suffix = " from tool output"
	}
	if fileData.MimeType != "" {
		return fmt.Sprintf("[omitted unsupported %s file reference %q%s]", fileData.MimeType, fileData.FileURI, suffix)
	}
	return fmt.Sprintf("[omitted unsupported file reference %q%s]", fileData.FileURI, suffix)
}

func formatGeminiExecutableCode(raw json.RawMessage) (string, error) {
	if !hasRawJSON(raw) {
		return "", invalidGeminiArgument("executableCode must be an object")
	}
	var execCode models.GeminiExecutableCode
	if err := json.Unmarshal(raw, &execCode); err != nil {
		return "", invalidGeminiArgument("executableCode must be an object")
	}
	lang := strings.ToLower(strings.TrimSpace(execCode.Language))
	if lang == "language_unspecified" {
		lang = ""
	}
	return fmt.Sprintf("\n```%s\n%s\n```\n", lang, execCode.Code), nil
}

func formatGeminiCodeExecutionResult(raw json.RawMessage) (string, error) {
	if !hasRawJSON(raw) {
		return "", invalidGeminiArgument("codeExecutionResult must be an object")
	}
	var result models.GeminiCodeExecutionResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", invalidGeminiArgument("codeExecutionResult must be an object")
	}
	outcome := strings.TrimSpace(result.Outcome)
	output := strings.TrimSuffix(result.Output, "\n")
	if outcome != "" && output != "" {
		return fmt.Sprintf("\n[code execution result (%s)]\n%s\n", outcome, output), nil
	}
	if outcome != "" {
		return fmt.Sprintf("\n[code execution result (%s)]\n", outcome), nil
	}
	if output != "" {
		return fmt.Sprintf("\n[code execution result]\n%s\n", output), nil
	}
	return "\n[code execution result]\n", nil
}

func normalizeGeminiInlineDataBase64(data string) (string, error) {
	normalized := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, data)

	decoded, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(normalized)
		if err != nil {
			return "", err
		}
	}

	return base64.StdEncoding.EncodeToString(decoded), nil
}

func normalizeGeminiArguments(raw json.RawMessage) (string, error) {
	if !hasRawJSON(raw) {
		return `{}`, nil
	}

	args, err := canonicalizeJSON(raw)
	if err != nil {
		return "", err
	}
	return string(args), nil
}

func normalizeGeminiFunctionResponse(raw json.RawMessage, unwrapCLITextOutput bool) (string, error) {
	if !hasRawJSON(raw) {
		return `{}`, nil
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}

	var object map[string]json.RawMessage
	if unwrapCLITextOutput {
		if err := json.Unmarshal(raw, &object); err == nil && len(object) == 1 {
			if outputRaw, ok := object["output"]; ok {
				var output string
				if err := json.Unmarshal(outputRaw, &output); err == nil {
					return output, nil
				}
			}
		}
	}

	response, err := canonicalizeJSON(raw)
	if err != nil {
		return "", err
	}
	return string(response), nil
}

func TranslateOpenAIToGemini(resp *models.OpenAIResponse) *models.GeminiGenerateContentResponse {
	result := &models.GeminiGenerateContentResponse{}
	if resp == nil {
		return result
	}

	if resp.Usage != nil {
		result.UsageMetadata = &models.GeminiUsageMetadata{
			PromptTokenCount:     resp.Usage.PromptTokens,
			CandidatesTokenCount: resp.Usage.CompletionTokens,
			TotalTokenCount:      resp.Usage.TotalTokens,
		}
	}

	if len(resp.Choices) == 0 {
		return result
	}

	choice := resp.Choices[0]
	candidateIndex := 0
	content := &models.GeminiContent{
		Role:  "model",
		Parts: []models.GeminiPart{},
	}

	if len(choice.Message.Content) > 0 {
		var text string
		if err := json.Unmarshal(choice.Message.Content, &text); err == nil {
			content.Parts = append(content.Parts, models.GeminiPart{Text: stringPtr(text)})
		}
	}
	if len(choice.Message.Refusal) > 0 {
		var refusal string
		if err := json.Unmarshal(choice.Message.Refusal, &refusal); err == nil && refusal != "" {
			content.Parts = append(content.Parts, models.GeminiPart{Text: stringPtr(refusal)})
		}
	}

	for _, toolCall := range choice.Message.ToolCalls {
		args, err := canonicalizeJSON(json.RawMessage(toolCall.Function.Arguments))
		if err != nil || !hasRawJSON(args) {
			args = json.RawMessage(`{}`)
		}
		content.Parts = append(content.Parts, models.GeminiPart{
			FunctionCall: &models.GeminiFunctionCall{
				ID:   toolCall.ID,
				Name: toolCall.Function.Name,
				Args: args,
			},
		})
	}

	finishReason := mapOpenAIFinishReasonToGemini(choice.FinishReason)
	result.Candidates = []models.GeminiCandidate{{
		Content:      content,
		FinishReason: finishReason,
		Index:        &candidateIndex,
	}}

	return result
}

func mapOpenAIFinishReasonToGemini(reason *string) string {
	if reason == nil || *reason == "" {
		return ""
	}

	switch *reason {
	case "stop", "tool_calls":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		return strings.ToUpper(*reason)
	}
}

func stringPtr(s string) *string {
	return &s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
