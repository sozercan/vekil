package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

func newChatInvalidRequest(param, message string) *chatExecutionError {
	return &chatExecutionError{
		StatusCode: http.StatusBadRequest,
		Type:       "invalid_request_error",
		Param:      param,
		Message:    message,
	}
}

type responsesChatRequestOptions struct {
	UpstreamModel       string
	ReplayStore         *responsesChatReplayStore
	ReplayRoute         responsesChatReplayRoute
	MinimumOutputTokens int
	DropSamplingParams  bool
}

type responsesChatRequestPlan struct {
	Body         []byte
	Stream       bool
	IncludeUsage bool
}

type responsesChatRequestEnvelope struct {
	Model       string                     `json:"model"`
	Input       []json.RawMessage          `json:"input"`
	Stream      bool                       `json:"stream"`
	MaxOutput   *int                       `json:"max_output_tokens,omitempty"`
	Temperature *float64                   `json:"temperature,omitempty"`
	TopP        *float64                   `json:"top_p,omitempty"`
	Tools       []json.RawMessage          `json:"tools,omitempty"`
	ToolChoice  json.RawMessage            `json:"tool_choice,omitempty"`
	Parallel    *bool                      `json:"parallel_tool_calls,omitempty"`
	Text        map[string]json.RawMessage `json:"text,omitempty"`
	Reasoning   map[string]json.RawMessage `json:"reasoning,omitempty"`
	Metadata    map[string]json.RawMessage `json:"metadata,omitempty"`
	Store       *bool                      `json:"store,omitempty"`
	User        *string                    `json:"user,omitempty"`
	PromptCache *string                    `json:"prompt_cache_key,omitempty"`
	SafetyID    *string                    `json:"safety_identifier,omitempty"`
	Include     []string                   `json:"include,omitempty"`
}

func translateChatRequestToResponses(chatBody []byte, options responsesChatRequestOptions) (responsesChatRequestPlan, error) {
	raw, err := decodeChatJSONObject(chatBody, "")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	if err := validateChatResponsesTopLevel(raw); err != nil {
		return responsesChatRequestPlan{}, err
	}

	model, err := requiredJSONString(raw, "model")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	upstreamModel := strings.TrimSpace(options.UpstreamModel)
	if upstreamModel == "" {
		upstreamModel = model
	}

	messagesRaw, ok := raw["messages"]
	if !ok {
		return responsesChatRequestPlan{}, newChatInvalidRequest("messages", "messages is required")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil || len(messages) == 0 {
		return responsesChatRequestPlan{}, newChatInvalidRequest("messages", "messages must be a non-empty array")
	}

	input, err := translateChatMessagesToResponses(messages, options)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}

	stream := false
	if value, ok := raw["stream"]; ok {
		if err := json.Unmarshal(value, &stream); err != nil {
			return responsesChatRequestPlan{}, newChatInvalidRequest("stream", "stream must be a boolean")
		}
	}

	maxOutput, err := chatMaxOutputTokens(raw)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	if options.MinimumOutputTokens > 0 && (maxOutput == nil || *maxOutput < options.MinimumOutputTokens) {
		minimum := options.MinimumOutputTokens
		maxOutput = &minimum
	}
	if options.MinimumOutputTokens == 0 && maxOutput != nil && *maxOutput < responsesChatMinimumOutputTokens {
		param := "max_tokens"
		if _, ok := raw["max_completion_tokens"]; ok {
			param = "max_completion_tokens"
		}
		return responsesChatRequestPlan{}, newChatInvalidRequest(param, "Responses-backed Chat requires an output token limit of at least 16")
	}

	var temperature, topP *float64
	if value, ok := raw["temperature"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var parsed float64
		if err := json.Unmarshal(value, &parsed); err != nil {
			return responsesChatRequestPlan{}, newChatInvalidRequest("temperature", "temperature must be a number")
		}
		temperature = &parsed
	}
	if value, ok := raw["top_p"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var parsed float64
		if err := json.Unmarshal(value, &parsed); err != nil {
			return responsesChatRequestPlan{}, newChatInvalidRequest("top_p", "top_p must be a number")
		}
		topP = &parsed
	}
	if options.DropSamplingParams {
		temperature = nil
		topP = nil
	}

	includeUsage, err := parseChatStreamOptions(raw)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	tools, toolNames, err := translateChatTools(raw["tools"])
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	toolChoice, err := translateChatToolChoice(raw["tool_choice"], toolNames)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	var parallel *bool
	if value, ok := raw["parallel_tool_calls"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var parsed bool
		if err := json.Unmarshal(value, &parsed); err != nil {
			return responsesChatRequestPlan{}, newChatInvalidRequest("parallel_tool_calls", "parallel_tool_calls must be a boolean")
		}
		parallel = &parsed
	}
	textConfig, err := translateChatTextConfiguration(raw)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	reasoningConfig, err := translateChatReasoningConfiguration(raw)
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	metadata, err := optionalJSONObject(raw, "metadata")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	store, err := optionalJSONBool(raw, "store")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	if store == nil {
		defaultStore := false
		store = &defaultStore
	}
	user, err := optionalJSONString(raw, "user")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	promptCache, err := optionalJSONString(raw, "prompt_cache_key")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}
	safetyID, err := optionalJSONString(raw, "safety_identifier")
	if err != nil {
		return responsesChatRequestPlan{}, err
	}

	envelope := responsesChatRequestEnvelope{
		Model:       upstreamModel,
		Input:       input,
		Stream:      stream,
		MaxOutput:   maxOutput,
		Temperature: temperature,
		TopP:        topP,
		Tools:       tools,
		ToolChoice:  toolChoice,
		Parallel:    parallel,
		Text:        textConfig,
		Reasoning:   reasoningConfig,
		Metadata:    metadata,
		Store:       store,
		User:        user,
		PromptCache: promptCache,
		SafetyID:    safetyID,
		Include:     []string{"reasoning.encrypted_content"},
	}
	body, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		return responsesChatRequestPlan{}, fmt.Errorf("marshal Responses request: %w", marshalErr)
	}
	return responsesChatRequestPlan{Body: body, Stream: stream, IncludeUsage: includeUsage}, nil
}

func requiredJSONString(raw map[string]json.RawMessage, field string) (string, error) {
	value, ok := raw[field]
	if !ok {
		return "", newChatInvalidRequest(field, field+" is required")
	}
	var parsed string
	if err := json.Unmarshal(value, &parsed); err != nil || strings.TrimSpace(parsed) == "" {
		return "", newChatInvalidRequest(field, field+" must be a non-empty string")
	}
	return strings.TrimSpace(parsed), nil
}

func chatMaxOutputTokens(raw map[string]json.RawMessage) (*int, error) {
	var maxTokens, maxCompletion *int
	for field, target := range map[string]**int{
		"max_tokens":            &maxTokens,
		"max_completion_tokens": &maxCompletion,
	} {
		value, ok := raw[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		var parsed int
		if err := json.Unmarshal(value, &parsed); err != nil || parsed < 0 {
			return nil, newChatInvalidRequest(field, field+" must be a non-negative integer")
		}
		*target = &parsed
	}
	if maxTokens != nil && maxCompletion != nil && *maxTokens != *maxCompletion {
		return nil, newChatInvalidRequest("max_completion_tokens", "max_tokens and max_completion_tokens must match")
	}
	if maxCompletion != nil {
		return maxCompletion, nil
	}
	return maxTokens, nil
}

func translateChatMessagesToResponses(messages []json.RawMessage, options responsesChatRequestOptions) ([]json.RawMessage, error) {
	resultIndices, err := chatToolResultIndices(messages)
	if err != nil {
		return nil, err
	}
	// The slice may grow when one Chat message expands to multiple Responses
	// items. Start with the bounded decoded message count and let append grow it;
	// avoid arithmetic on an untrusted length in the allocation size.
	input := make([]json.RawMessage, 0, len(messages))
	calls := make(map[string]string)
	results := make(map[string]struct{})
	restoredGroups := make(map[uint64]struct{})
	for index, raw := range messages {
		messageParam := fmt.Sprintf("messages[%d]", index)
		if _, err := validateChatRawObjectFields(raw, messageParam, "role", "content", "refusal", "name", "tool_calls", "tool_call_id"); err != nil {
			return nil, err
		}
		var message struct {
			Role       string            `json:"role"`
			Content    json.RawMessage   `json:"content"`
			Refusal    json.RawMessage   `json:"refusal"`
			Name       string            `json:"name"`
			ToolCalls  []json.RawMessage `json:"tool_calls"`
			ToolCallID string            `json:"tool_call_id"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "message must be an object")
		}
		role := strings.TrimSpace(message.Role)
		if strings.TrimSpace(message.Name) != "" {
			return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].name", index), "message name is not supported")
		}
		switch role {
		case "system", "developer", "user":
			if len(bytes.TrimSpace(message.Refusal)) > 0 && !bytes.Equal(bytes.TrimSpace(message.Refusal), []byte("null")) {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].refusal", index), "refusal is valid only for assistant messages")
			}
			if len(message.ToolCalls) > 0 || strings.TrimSpace(message.ToolCallID) != "" {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "tool fields are not valid for this message role")
			}
			content, err := translateChatMessageContent(message.Content, role, index)
			if err != nil {
				return nil, err
			}
			if len(content) == 0 {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].content", index), "message content must not be empty")
			}
			item, _ := json.Marshal(map[string]any{"type": "message", "role": role, "content": content})
			input = append(input, item)
		case "assistant":
			if strings.TrimSpace(message.ToolCallID) != "" {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "tool_call_id is not valid for assistant messages")
			}
			content, err := translateChatMessageContent(message.Content, role, index)
			if err != nil {
				return nil, err
			}
			refusal, err := translateChatAssistantRefusal(message.Refusal, index)
			if err != nil {
				return nil, err
			}
			if len(message.ToolCalls) == 0 {
				assistantText := assistantHistoryText(content) + refusal
				if assistantText == "" {
					return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "assistant message must contain content, refusal, or tool calls")
				}
				input = appendAssistantHistoryMessage(input, assistantText)
				continue
			}

			projected := make([]responsesChatReplayProjectedCall, len(message.ToolCalls))
			syntheticItems := make([]json.RawMessage, len(message.ToolCalls))
			replayCalls := 0
			for callIndex, callRaw := range message.ToolCalls {
				callID, item, err := translateSyntheticChatToolCall(callRaw, index, callIndex)
				if err != nil {
					return nil, err
				}
				var parsed struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				}
				_ = json.Unmarshal(callRaw, &parsed)
				projected[callIndex] = responsesChatReplayProjectedCall{ID: callID, Name: strings.TrimSpace(parsed.Function.Name), Arguments: parsed.Function.Arguments}
				syntheticItems[callIndex] = item
				if isResponsesChatReplayCallID(callID) {
					replayCalls++
				}
			}
			if replayCalls > 0 && refusal != "" {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].refusal", index), "refusal is not supported on Responses replay tool-call messages")
			}
			if replayCalls != 0 && replayCalls != len(projected) {
				return nil, replayChatExecutionError(responsesChatReplayMixedCode, responsesChatReplayMixedMessage)
			}
			if replayCalls == 0 {
				if assistantText := assistantHistoryText(content) + refusal; assistantText != "" {
					input = appendAssistantHistoryMessage(input, assistantText)
				}
				matchedResults := 0
				for _, projectedCall := range projected {
					if resultIndex, ok := resultIndices[projectedCall.ID]; ok && resultIndex > index {
						matchedResults++
					}
				}
				if matchedResults == 0 {
					return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "assistant tool calls require at least one subsequent tool result")
				}
				for callIndex, projectedCall := range projected {
					if matchedResults < len(projected) {
						if resultIndex, ok := resultIndices[projectedCall.ID]; !ok || resultIndex <= index {
							continue
						}
					}
					if _, duplicate := calls[projectedCall.ID]; duplicate {
						return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_calls[%d].id", index, callIndex), "duplicate tool call ID")
					}
					calls[projectedCall.ID] = projectedCall.ID
					input = append(input, syntheticItems[callIndex])
				}
				continue
			}
			if options.ReplayStore == nil {
				return nil, missingResponsesChatReplayError()
			}
			projectionContent, _ := json.Marshal(assistantHistoryText(content))
			resolution, err := resolveResponsesChatReplay(options.ReplayStore, options.ReplayRoute, responsesChatReplayAssistantProjection{Content: projectionContent, Calls: projected})
			if err != nil {
				return nil, mapResponsesChatReplayResolveError(err)
			}
			if _, duplicate := restoredGroups[resolution.GroupID]; duplicate {
				return nil, replayChatExecutionError(responsesChatReplayProjectionCode, "Responses replay group appears more than once in the request.")
			}
			restoredGroups[resolution.GroupID] = struct{}{}
			resolvedByProxy := make(map[string]responsesChatReplayResolvedCall, len(resolution.Calls))
			matchedResults := 0
			for _, call := range resolution.Calls {
				resolvedByProxy[call.ProxyCallID] = call
				if resultIndex, ok := resultIndices[call.ProxyCallID]; ok && resultIndex > index {
					matchedResults++
				}
			}
			if matchedResults == 0 {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d]", index), "Responses-backed assistant tool calls require at least one subsequent tool result")
			}
			if matchedResults == len(resolution.Calls) {
				input = append(input, cloneReplayRawMessages(resolution.OutputItems)...)
			} else {
				// Live gpt-5.6-sol rejects a complete parallel call group when only a
				// subset has outputs. Replay the visible assistant text plus only the
				// exact calls that have results; the store remains intact for retries.
				if assistantText := assistantHistoryText(content) + refusal; assistantText != "" {
					input = appendAssistantHistoryMessage(input, assistantText)
				}
				for _, projectedCall := range projected {
					resolved := resolvedByProxy[projectedCall.ID]
					if resultIndex, ok := resultIndices[projectedCall.ID]; ok && resultIndex > index {
						input = append(input, cloneReplayRawMessage(resolved.OutputItem))
					}
				}
			}
			for callIndex, projectedCall := range projected {
				if _, duplicate := calls[projectedCall.ID]; duplicate {
					return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_calls[%d].id", index, callIndex), "duplicate tool call ID")
				}
				calls[projectedCall.ID] = resolvedByProxy[projectedCall.ID].UpstreamCallID
			}
		case "tool":
			if len(bytes.TrimSpace(message.Refusal)) > 0 && !bytes.Equal(bytes.TrimSpace(message.Refusal), []byte("null")) {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].refusal", index), "refusal is not valid for tool messages")
			}
			if len(message.ToolCalls) > 0 {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_calls", index), "tool_calls is not valid for tool messages")
			}
			callID := strings.TrimSpace(message.ToolCallID)
			if callID == "" {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "tool_call_id is required")
			}
			upstreamCallID, ok := calls[callID]
			if !ok {
				if isResponsesChatReplayCallID(callID) {
					return nil, missingResponsesChatReplayError()
				}
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "tool result references no prior assistant tool call")
			}
			if _, duplicate := results[callID]; duplicate {
				return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "duplicate tool result")
			}
			results[callID] = struct{}{}
			output, err := compactChatToolOutput(message.Content, index)
			if err != nil {
				return nil, err
			}
			item, _ := json.Marshal(map[string]any{"type": "function_call_output", "call_id": upstreamCallID, "output": output})
			input = append(input, item)
		default:
			return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].role", index), "unsupported message role")
		}
	}
	return input, nil
}

func chatToolResultIndices(messages []json.RawMessage) (map[string]int, error) {
	indices := make(map[string]int)
	for index, raw := range messages {
		messageParam := fmt.Sprintf("messages[%d]", index)
		if _, err := validateChatRawObjectFields(raw, messageParam, "role", "content", "refusal", "name", "tool_calls", "tool_call_id"); err != nil {
			return nil, err
		}
		var message struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		}
		if json.Unmarshal(raw, &message) != nil || strings.TrimSpace(message.Role) != "tool" {
			continue
		}
		callID := strings.TrimSpace(message.ToolCallID)
		if callID == "" {
			continue
		}
		if _, duplicate := indices[callID]; duplicate {
			return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].tool_call_id", index), "duplicate tool result")
		}
		indices[callID] = index
	}
	return indices, nil
}

func resolveResponsesChatReplay(store *responsesChatReplayStore, route responsesChatReplayRoute, projection responsesChatReplayAssistantProjection) (responsesChatReplayResolution, error) {
	resolution, err := store.Resolve(route, projection)
	var mismatch *responsesChatReplayProjectionError
	if !errors.As(err, &mismatch) || !replayContentIsNullOrEmpty(projection.Content) {
		return resolution, err
	}
	alternate := json.RawMessage(`""`)
	if bytes.Equal(bytes.TrimSpace(projection.Content), []byte(`""`)) {
		alternate = json.RawMessage("null")
	}
	projection.Content = alternate
	return store.Resolve(route, projection)
}

func replayContentIsNullOrEmpty(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`))
}

func mapResponsesChatReplayResolveError(err error) error {
	var replayCode interface{ ReplayCode() string }
	if errors.As(err, &replayCode) {
		switch replayCode.ReplayCode() {
		case responsesChatReplayMissingCode:
			return missingResponsesChatReplayError()
		case responsesChatReplayMixedCode:
			return replayChatExecutionError(responsesChatReplayMixedCode, responsesChatReplayMixedMessage)
		case responsesChatReplayProjectionCode:
			return replayChatExecutionError(responsesChatReplayProjectionCode, responsesChatReplayProjectionMessage)
		case responsesChatReplayClosedCode:
			return &chatExecutionError{StatusCode: http.StatusServiceUnavailable, Type: "server_error", Code: responsesChatReplayClosedCode, Param: "messages", Message: responsesChatReplayClosedMessage}
		}
	}
	return replayChatExecutionError(responsesChatReplayProjectionCode, responsesChatReplayProjectionMessage)
}

func replayChatExecutionError(code, message string) *chatExecutionError {
	return &chatExecutionError{StatusCode: http.StatusBadRequest, Type: "invalid_request_error", Code: code, Param: "messages", Message: message}
}

func translateChatAssistantRefusal(raw json.RawMessage, messageIndex int) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var refusal string
	if err := json.Unmarshal(raw, &refusal); err != nil {
		return "", newChatInvalidRequest(fmt.Sprintf("messages[%d].refusal", messageIndex), "assistant refusal must be a string or null")
	}
	return refusal, nil
}

func assistantHistoryText(content []map[string]any) string {
	var text strings.Builder
	for _, part := range content {
		if value, ok := part["text"].(string); ok {
			text.WriteString(value)
		}
	}
	return text.String()
}

func appendAssistantHistoryMessage(input []json.RawMessage, text string) []json.RawMessage {
	item, _ := json.Marshal(map[string]any{"role": "assistant", "content": text})
	return append(input, item)
}

func isResponsesChatReplayCallID(id string) bool {
	id = strings.TrimSpace(id)
	if len(id) != responsesChatReplayIDLength || !strings.HasPrefix(id, responsesChatReplayCallIDPrefix) {
		return false
	}
	for _, char := range id[len(responsesChatReplayCallIDPrefix):] {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func translateSyntheticChatToolCall(raw json.RawMessage, messageIndex, callIndex int) (string, json.RawMessage, error) {
	param := fmt.Sprintf("messages[%d].tool_calls[%d]", messageIndex, callIndex)
	callObject, err := validateChatRawObjectFields(raw, param, "id", "type", "function")
	if err != nil {
		return "", nil, err
	}
	functionObject := map[string]json.RawMessage(nil)
	if functionRaw, ok := callObject["function"]; ok {
		var err error
		functionObject, err = validateChatRawObjectFields(functionRaw, param+".function", "name", "arguments")
		if err != nil {
			return "", nil, err
		}
	}
	if functionObject == nil {
		return "", nil, newChatInvalidRequest(param+".function", "function is required")
	}
	rawArguments, ok := functionObject["arguments"]
	if !ok {
		return "", nil, newChatInvalidRequest(param+".function.arguments", "function arguments string is required")
	}
	var decodedArguments string
	if bytes.Equal(bytes.TrimSpace(rawArguments), []byte("null")) {
		return "", nil, newChatInvalidRequest(param+".function.arguments", "function arguments must be a string")
	}
	if err := json.Unmarshal(rawArguments, &decodedArguments); err != nil {
		return "", nil, newChatInvalidRequest(param+".function.arguments", "function arguments must be a string")
	}
	var call struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return "", nil, newChatInvalidRequest(param, "tool call must be an object")
	}
	callID := strings.TrimSpace(call.ID)
	if callID == "" {
		return "", nil, newChatInvalidRequest(param+".id", "tool call ID is required")
	}
	if call.Type != "" && call.Type != "function" {
		return "", nil, newChatInvalidRequest(param+".type", "only function tool calls are supported")
	}
	name := strings.TrimSpace(call.Function.Name)
	if name == "" {
		return "", nil, newChatInvalidRequest(param+".function.name", "function name is required")
	}
	item, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": call.Function.Arguments})
	return callID, item, nil
}

func compactChatToolOutput(raw json.RawMessage, messageIndex int) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", newChatInvalidRequest(fmt.Sprintf("messages[%d].content", messageIndex), "tool output is required")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var parts []json.RawMessage
		if err := json.Unmarshal(raw, &parts); err == nil && len(parts) > 0 {
			looksLikeContentParts := true
			for _, partRaw := range parts {
				var probe struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(partRaw, &probe) != nil || probe.Type != "text" {
					looksLikeContentParts = false
					break
				}
			}
			if looksLikeContentParts {
				var output strings.Builder
				for partIndex, partRaw := range parts {
					param := fmt.Sprintf("messages[%d].content[%d]", messageIndex, partIndex)
					if _, err := validateChatRawObjectFields(partRaw, param, "type", "text"); err != nil {
						return "", err
					}
					var part struct {
						Type string  `json:"type"`
						Text *string `json:"text"`
					}
					if err := json.Unmarshal(partRaw, &part); err != nil || part.Type != "text" || part.Text == nil {
						return "", newChatInvalidRequest(param, "tool message content supports text parts only")
					}
					output.WriteString(*part.Text)
				}
				return output.String(), nil
			}
		}
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", newChatInvalidRequest(fmt.Sprintf("messages[%d].content", messageIndex), "tool output must be valid JSON")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", newChatInvalidRequest(fmt.Sprintf("messages[%d].content", messageIndex), "tool output must be valid JSON")
	}
	return string(encoded), nil
}

func missingResponsesChatReplayError() *chatExecutionError {
	return &chatExecutionError{
		StatusCode: http.StatusBadRequest,
		Type:       "invalid_request_error",
		Code:       "responses_replay_state_missing",
		Param:      "messages",
		Message:    "Responses-backed tool state is no longer available; restart the assistant tool-call turn.",
	}
}

func translateChatMessageContent(raw json.RawMessage, role string, messageIndex int) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		return []map[string]any{{"type": contentType, "text": text}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, newChatInvalidRequest(fmt.Sprintf("messages[%d].content", messageIndex), "message content must be a string or array")
	}
	translated := make([]map[string]any, 0, len(parts))
	for partIndex, partRaw := range parts {
		var part struct {
			Type     string  `json:"type"`
			Text     *string `json:"text,omitempty"`
			ImageURL *struct {
				URL    string `json:"url"`
				Detail string `json:"detail,omitempty"`
			} `json:"image_url,omitempty"`
		}
		param := fmt.Sprintf("messages[%d].content[%d]", messageIndex, partIndex)
		partObject, validationErr := validateChatRawObjectFields(partRaw, param, "type", "text", "image_url")
		if validationErr != nil {
			return nil, validationErr
		}
		if rawImage, ok := partObject["image_url"]; ok && len(bytes.TrimSpace(rawImage)) > 0 && !bytes.Equal(bytes.TrimSpace(rawImage), []byte("null")) {
			if _, err := validateChatRawObjectFields(rawImage, param+".image_url", "url", "detail"); err != nil {
				return nil, err
			}
		}
		if err := json.Unmarshal(partRaw, &part); err != nil {
			return nil, newChatInvalidRequest(param, "content part must be an object")
		}
		switch strings.TrimSpace(part.Type) {
		case "text":
			if part.ImageURL != nil {
				return nil, newChatInvalidRequest(param+".image_url", "image_url is not valid for a text content part")
			}
			if part.Text == nil {
				return nil, newChatInvalidRequest(param+".text", "text is required")
			}
			contentType := "input_text"
			if role == "assistant" {
				contentType = "output_text"
			}
			translated = append(translated, map[string]any{"type": contentType, "text": *part.Text})
		case "image_url":
			if part.Text != nil {
				return nil, newChatInvalidRequest(param+".text", "text is not valid for an image content part")
			}
			if role != "user" {
				return nil, newChatInvalidRequest(param, "image content is supported only in user messages")
			}
			if part.ImageURL == nil || strings.TrimSpace(part.ImageURL.URL) == "" {
				return nil, newChatInvalidRequest(param+".image_url.url", "image URL is required")
			}
			imageURL := strings.TrimSpace(part.ImageURL.URL)
			if !validResponsesChatImageURL(imageURL) {
				return nil, newChatInvalidRequest(param+".image_url.url", "image URL must be HTTP(S) or a base64 image data URL")
			}
			image := map[string]any{"type": "input_image", "image_url": imageURL}
			if detail := strings.TrimSpace(part.ImageURL.Detail); detail != "" {
				switch detail {
				case "auto", "low", "high", "original":
					image["detail"] = detail
				default:
					return nil, newChatInvalidRequest(param+".image_url.detail", "unsupported image detail")
				}
			}
			translated = append(translated, image)
		default:
			return nil, newChatInvalidRequest(param+".type", "unsupported content part type")
		}
	}
	return translated, nil
}

func validResponsesChatImageURL(raw string) bool {
	if strings.HasPrefix(raw, "data:image/") {
		comma := strings.IndexByte(raw, ',')
		if comma <= len("data:image/") || !strings.HasSuffix(raw[:comma], ";base64") {
			return false
		}
		_, err := base64.StdEncoding.DecodeString(raw[comma+1:])
		return err == nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func parseChatStreamOptions(raw map[string]json.RawMessage) (bool, error) {
	value, ok := raw["stream_options"]
	if !ok || len(bytes.TrimSpace(value)) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return false, nil
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(value, &options); err != nil {
		return false, newChatInvalidRequest("stream_options", "stream_options must be an object")
	}
	for field := range options {
		if field != "include_usage" {
			return false, newChatInvalidRequest("stream_options."+field, "unsupported stream option")
		}
	}
	include := false
	if rawInclude, ok := options["include_usage"]; ok {
		if err := json.Unmarshal(rawInclude, &include); err != nil {
			return false, newChatInvalidRequest("stream_options.include_usage", "include_usage must be a boolean")
		}
	}
	return include, nil
}

func translateChatTools(raw json.RawMessage) ([]json.RawMessage, map[string]struct{}, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, nil, newChatInvalidRequest("tools", "tools must be an array")
	}
	translated := make([]json.RawMessage, 0, len(tools))
	names := make(map[string]struct{}, len(tools))
	for i, toolRaw := range tools {
		toolParam := fmt.Sprintf("tools[%d]", i)
		toolObject, err := validateChatRawObjectFields(toolRaw, toolParam, "type", "function")
		if err != nil {
			return nil, nil, err
		}
		var functionObject map[string]json.RawMessage
		if functionRaw, ok := toolObject["function"]; ok {
			functionObject, err = validateChatRawObjectFields(functionRaw, toolParam+".function", "name", "description", "parameters", "strict")
			if err != nil {
				return nil, nil, err
			}
		}
		strict := false
		if strictRaw, ok := functionObject["strict"]; ok && !bytes.Equal(bytes.TrimSpace(strictRaw), []byte("null")) {
			if err := json.Unmarshal(strictRaw, &strict); err != nil {
				return nil, nil, newChatInvalidRequest(toolParam+".function.strict", "strict must be a boolean")
			}
		}
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			} `json:"function"`
		}
		if err := json.Unmarshal(toolRaw, &tool); err != nil {
			return nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d]", i), "tool must be an object")
		}
		if strings.TrimSpace(tool.Type) != "function" {
			return nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d].type", i), "only function tools are supported")
		}
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			return nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d].function.name", i), "function name is required")
		}
		if _, duplicate := names[name]; duplicate {
			return nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d].function.name", i), "function names must be unique")
		}
		names[name] = struct{}{}
		parameters := tool.Function.Parameters
		if len(bytes.TrimSpace(parameters)) == 0 || bytes.Equal(bytes.TrimSpace(parameters), []byte("null")) {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		} else {
			var schema map[string]json.RawMessage
			if err := json.Unmarshal(parameters, &schema); err != nil || schema == nil {
				return nil, nil, newChatInvalidRequest(fmt.Sprintf("tools[%d].function.parameters", i), "function parameters must be a JSON object")
			}
		}
		flattened := map[string]any{
			"type":       "function",
			"name":       name,
			"parameters": parameters,
			"strict":     strict,
		}
		if tool.Function.Description != "" {
			flattened["description"] = tool.Function.Description
		}
		encoded, err := json.Marshal(flattened)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal function tool: %w", err)
		}
		translated = append(translated, encoded)
	}
	return translated, names, nil
}

func translateChatToolChoice(raw json.RawMessage, toolNames map[string]struct{}) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var choice string
	if err := json.Unmarshal(raw, &choice); err == nil {
		switch choice {
		case "none", "auto":
			return json.Marshal(choice)
		case "required":
			if len(toolNames) == 0 {
				return nil, newChatInvalidRequest("tool_choice", "required tool_choice needs declared tools")
			}
			return json.Marshal(choice)
		default:
			return nil, newChatInvalidRequest("tool_choice", "unsupported tool_choice")
		}
	}
	choiceObject, validationErr := validateChatRawObjectFields(raw, "tool_choice", "type", "function")
	if validationErr != nil {
		return nil, validationErr
	}
	if functionRaw, ok := choiceObject["function"]; ok {
		if _, err := validateChatRawObjectFields(functionRaw, "tool_choice.function", "name"); err != nil {
			return nil, err
		}
	}
	var object struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err != nil || object.Type != "function" || strings.TrimSpace(object.Function.Name) == "" {
		return nil, newChatInvalidRequest("tool_choice", "tool_choice must be none, auto, required, or a named function")
	}
	name := strings.TrimSpace(object.Function.Name)
	if _, ok := toolNames[name]; !ok {
		return nil, newChatInvalidRequest("tool_choice", "named tool_choice must reference a declared function")
	}
	return json.Marshal(map[string]string{"type": "function", "name": name})
}

func translateChatTextConfiguration(raw map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	text := make(map[string]json.RawMessage, 2)
	if formatRaw, ok := raw["response_format"]; ok && len(bytes.TrimSpace(formatRaw)) > 0 && !bytes.Equal(bytes.TrimSpace(formatRaw), []byte("null")) {
		format, err := translateChatResponseFormat(formatRaw)
		if err != nil {
			return nil, err
		}
		text["format"] = format
	}
	if verbosityRaw, ok := raw["verbosity"]; ok && !bytes.Equal(bytes.TrimSpace(verbosityRaw), []byte("null")) {
		var verbosity string
		if err := json.Unmarshal(verbosityRaw, &verbosity); err != nil {
			return nil, newChatInvalidRequest("verbosity", "verbosity must be a string")
		}
		switch verbosity {
		case "low", "medium", "high":
		default:
			return nil, newChatInvalidRequest("verbosity", "unsupported verbosity")
		}
		text["verbosity"] = json.RawMessage(strconvQuote(verbosity))
	}
	if len(text) == 0 {
		return nil, nil
	}
	return text, nil
}

func translateChatResponseFormat(raw json.RawMessage) (json.RawMessage, error) {
	formatObject, err := validateChatRawObjectFields(raw, "response_format", "type", "json_schema")
	if err != nil {
		return nil, err
	}
	var format struct {
		Type       string          `json:"type"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return nil, newChatInvalidRequest("response_format", "response_format must be an object")
	}
	switch format.Type {
	case "text", "json_object":
		return json.Marshal(map[string]string{"type": format.Type})
	case "json_schema":
		var schema struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Schema      json.RawMessage `json:"schema"`
			Strict      *bool           `json:"strict,omitempty"`
		}
		if _, err := validateChatRawObjectFields(formatObject["json_schema"], "response_format.json_schema", "name", "description", "schema", "strict"); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(format.JSONSchema, &schema); err != nil {
			return nil, newChatInvalidRequest("response_format.json_schema", "json_schema must be an object")
		}
		if strings.TrimSpace(schema.Name) == "" {
			return nil, newChatInvalidRequest("response_format.json_schema.name", "json schema name is required")
		}
		var schemaObject map[string]json.RawMessage
		if err := json.Unmarshal(schema.Schema, &schemaObject); err != nil || schemaObject == nil {
			return nil, newChatInvalidRequest("response_format.json_schema.schema", "json schema must be an object")
		}
		flattened := map[string]any{"type": "json_schema", "name": strings.TrimSpace(schema.Name), "schema": schema.Schema}
		if schema.Description != "" {
			flattened["description"] = schema.Description
		}
		if schema.Strict != nil {
			flattened["strict"] = *schema.Strict
		}
		return json.Marshal(flattened)
	default:
		return nil, newChatInvalidRequest("response_format.type", "unsupported response_format type")
	}
}

func translateChatReasoningConfiguration(raw map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	value, ok := raw["reasoning_effort"]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil
	}
	var effort string
	if err := json.Unmarshal(value, &effort); err != nil || strings.TrimSpace(effort) == "" {
		return nil, newChatInvalidRequest("reasoning_effort", "reasoning_effort must be a non-empty string")
	}
	return map[string]json.RawMessage{"effort": json.RawMessage(strconvQuote(strings.TrimSpace(effort)))}, nil
}

func optionalJSONObject(raw map[string]json.RawMessage, field string) (map[string]json.RawMessage, error) {
	value, ok := raw[field]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(value, &parsed); err != nil {
		return nil, newChatInvalidRequest(field, field+" must be an object")
	}
	return parsed, nil
}

func optionalJSONBool(raw map[string]json.RawMessage, field string) (*bool, error) {
	value, ok := raw[field]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil
	}
	var parsed bool
	if err := json.Unmarshal(value, &parsed); err != nil {
		return nil, newChatInvalidRequest(field, field+" must be a boolean")
	}
	return &parsed, nil
}

func optionalJSONString(raw map[string]json.RawMessage, field string) (*string, error) {
	value, ok := raw[field]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil, nil
	}
	var parsed string
	if err := json.Unmarshal(value, &parsed); err != nil {
		return nil, newChatInvalidRequest(field, field+" must be a string")
	}
	return &parsed, nil
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func validateChatResponsesTopLevel(raw map[string]json.RawMessage) error {
	allowed := map[string]struct{}{
		"model": {}, "messages": {}, "stream": {}, "stream_options": {},
		"temperature": {}, "top_p": {}, "max_tokens": {}, "max_completion_tokens": {},
		"tools": {}, "tool_choice": {}, "parallel_tool_calls": {},
		"response_format": {}, "reasoning_effort": {}, "verbosity": {},
		"metadata": {}, "store": {}, "user": {}, "prompt_cache_key": {}, "safety_identifier": {},
		"stop": {}, "n": {},
	}
	unknown := make([]string, 0)
	for field := range raw {
		if _, ok := allowed[field]; !ok {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		field := unknown[0]
		return newChatInvalidRequest(field, field+" is not supported for Responses-backed Chat completions")
	}
	if stop, ok := raw["stop"]; ok {
		empty, err := emptyChatStop(stop)
		if err != nil {
			return err
		}
		if !empty {
			return newChatInvalidRequest("stop", "stop is not supported for Responses-backed Chat completions")
		}
	}
	if value, ok := raw["n"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var n int
		if err := json.Unmarshal(value, &n); err != nil || n != 1 {
			return newChatInvalidRequest("n", "n must be 1 for Responses-backed Chat completions")
		}
	}
	return nil
}

func emptyChatStop(raw json.RawMessage) (bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == "", nil
	}
	var multiple []string
	if err := json.Unmarshal(raw, &multiple); err == nil {
		return len(multiple) == 0, nil
	}
	return false, newChatInvalidRequest("stop", "stop must be a string or array of strings")
}

func decodeChatJSONObject(body []byte, param string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return nil, newChatInvalidRequest(param, "invalid JSON in request body")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, newChatInvalidRequest(param, "request body must be a JSON object")
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, newChatInvalidRequest(param, "invalid JSON object")
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, newChatInvalidRequest(param, "invalid JSON object key")
		}
		if _, duplicate := object[key]; duplicate {
			fieldParam := key
			if param != "" {
				fieldParam = param + "." + key
			}
			return nil, newChatInvalidRequest(fieldParam, "duplicate JSON field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, newChatInvalidRequest(param, "invalid JSON object value")
		}
		object[key] = append(json.RawMessage(nil), value...)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, newChatInvalidRequest(param, "invalid JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, newChatInvalidRequest(param, "request body must contain one JSON object")
	}
	return object, nil
}

func validateChatRawObjectFields(raw json.RawMessage, param string, allowedFields ...string) (map[string]json.RawMessage, error) {
	object, err := decodeChatJSONObject(raw, param)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(allowedFields))
	for _, field := range allowedFields {
		allowed[field] = struct{}{}
	}
	unknown := make([]string, 0)
	for field := range object {
		if _, ok := allowed[field]; !ok {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		fieldParam := unknown[0]
		if param != "" {
			fieldParam = param + "." + unknown[0]
		}
		return nil, newChatInvalidRequest(fieldParam, "unsupported JSON field")
	}
	return object, nil
}
